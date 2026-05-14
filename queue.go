package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// QueuedEvent es la unidad atómica de la queue local. Representa un evento
// LPR recibido de la cámara que aún no fue subido exitosamente al cloud.
//
// Persistencia: cada evento se serializa en dos archivos en `queueDir/`:
//   - {ts}_{rand}.json — metadata (este struct serializado)
//   - {ts}_{rand}.bin  — payload binario de la imagen (separado para no
//     parsear N MB de JSON solo para chequear retry_count)
//
// Atomicidad: write a tmpfile + os.Rename (POSIX atómico). Si el agent muere
// mid-write, el .json incompleto queda como `.tmp` y el glob no lo recoge.
type QueuedEvent struct {
	// ID interno (filename prefix). Formato: `{unix_nanoseconds}_{rand6}`.
	// Ordenable lexicográficamente = ordenable cronológicamente.
	ID string `json:"id"`

	// Plate normalizada (uppercase + strip), tal como vino del adapter.
	Plate string `json:"plate"`

	// Direction opcional ('entry' | 'exit' | ''). Si vacío, el cloud usa
	// el config del device.
	Direction string `json:"direction,omitempty"`

	// Timestamp del evento (ISO 8601). Si vacío, el cloud usa server time.
	// Crítico para outage recovery: la cámara puede mandar eventos rezagados
	// con el timestamp ORIGINAL (cuando ocurrió, no cuando subimos).
	Timestamp string `json:"timestamp,omitempty"`

	// Metadata adicional (confidence, vendor-specific fields, etc.).
	Metadata map[string]any `json:"metadata,omitempty"`

	// SnapshotMimeType del .bin file. Generalmente "image/jpeg" desde Hikvision
	// directo. El cloud convierte a WebP server-side via ImageHelper.
	SnapshotMimeType string `json:"snapshot_mime_type,omitempty"`

	// SnapshotBytes es el tamaño del .bin (informativo + cap defensive).
	SnapshotBytes int `json:"snapshot_bytes,omitempty"`

	// RetryCount cuenta los intentos fallidos. Sobre 5 → descarta.
	RetryCount int `json:"retry_count"`

	// LastAttemptAt timestamp del último intento (para back-off).
	LastAttemptAt time.Time `json:"last_attempt_at"`

	// CreatedAt cuando recibimos el evento de la cámara.
	CreatedAt time.Time `json:"created_at"`
}

// FileQueue es una cola persistente file-based, optimizada para
// resiliencia offline. NO necesita SQL — el orden lexicográfico de los
// filenames (timestamp prefix) garantiza FIFO. Atomicidad vía rename.
//
// **Por qué no SQLite**: añadir modernc.org/sqlite (pure Go) infla el
// binario ~10MB y agrega complejidad de schema/migraciones. Para ~800
// eventos/día con caps absolutos de 10k items/1GB, file system es
// suficientemente rápido y mucho más simple de inspeccionar/debug.
type FileQueue struct {
	dir            string
	maxItems       int
	maxBytes       int64
	mu             sync.Mutex // serializa Enqueue para evitar race en counts
}

// NewFileQueue crea (o reusa) la queue en `dir`. Crea el directorio si no
// existe. Returns error solo si el filesystem no permite mkdir.
func NewFileQueue(dir string, maxItems int, maxBytes int64) (*FileQueue, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creando queue dir %s: %w", dir, err)
	}
	return &FileQueue{
		dir:      dir,
		maxItems: maxItems,
		maxBytes: maxBytes,
	}, nil
}

// Enqueue persiste un nuevo evento + su binario. Si la queue está al cap,
// descarta los MÁS ANTIGUOS primero (FIFO eviction) y loggea warning.
//
// Atomicidad: el .bin y el .json se escriben primero como `.tmp` y luego
// se renombran (rename atómico en POSIX). El glob de Drain ignora `.tmp`,
// así que un evento parcialmente escrito nunca se procesa.
func (q *FileQueue) Enqueue(ev *QueuedEvent, snapshot []byte) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Cap check antes de escribir. Si pasamos el límite, eviction primero.
	if err := q.enforceCapsLocked(); err != nil {
		log.Printf("[queue] enforceCaps falló: %v", err)
	}

	if ev.ID == "" {
		rand6 := make([]byte, 3)
		_, _ = rand.Read(rand6)
		ev.ID = fmt.Sprintf("%020d_%s", time.Now().UnixNano(), hex.EncodeToString(rand6))
	}
	ev.SnapshotBytes = len(snapshot)
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now()
	}

	// 1) Escribir el .bin primero (atomic via rename). Si falla, no creamos
	// el .json — el evento queda como si no hubiera sido encolado.
	binPath := filepath.Join(q.dir, ev.ID+".bin")
	if err := writeFileAtomic(binPath, snapshot); err != nil {
		return fmt.Errorf("escribiendo snapshot bin: %w", err)
	}

	// 2) Escribir el .json. Si falla, intentar limpiar el .bin.
	jsonPath := filepath.Join(q.dir, ev.ID+".json")
	data, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		_ = os.Remove(binPath)
		return fmt.Errorf("serializando event json: %w", err)
	}
	if err := writeFileAtomic(jsonPath, data); err != nil {
		_ = os.Remove(binPath)
		return fmt.Errorf("escribiendo event json: %w", err)
	}

	return nil
}

// PeekOldest lee los N eventos más antiguos pendientes. Ordenados por filename
// lexicográficamente == cronológicamente (timestamp prefix).
//
// NO marca los eventos como "en proceso" — el caller que invoque Delete()
// al terminar es responsable de no procesarlos dos veces (single replay
// goroutine garantiza esto en el agent).
func (q *FileQueue) PeekOldest(limit int) ([]*QueuedEvent, error) {
	entries, err := os.ReadDir(q.dir)
	if err != nil {
		return nil, err
	}

	// Filtra solo .json (orden lexicográfico por filename).
	jsonNames := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if filepath.Ext(name) == ".json" {
			jsonNames = append(jsonNames, name)
		}
	}
	sort.Strings(jsonNames)

	if limit > 0 && len(jsonNames) > limit {
		jsonNames = jsonNames[:limit]
	}

	out := make([]*QueuedEvent, 0, len(jsonNames))
	for _, name := range jsonNames {
		ev, err := q.readEvent(name)
		if err != nil {
			log.Printf("[queue] leyendo %s falló (se omite): %v", name, err)
			continue
		}
		out = append(out, ev)
	}
	return out, nil
}

// Snapshot devuelve el contenido binario asociado al evento. Vacío si el .bin
// fue borrado externamente (raro — sería un .json huérfano).
func (q *FileQueue) Snapshot(eventID string) ([]byte, error) {
	binPath := filepath.Join(q.dir, eventID+".bin")
	return os.ReadFile(binPath)
}

// Delete borra atómicamente .json + .bin del evento. Llamar tras éxito al
// reenviar al cloud o tras 5 retry fallidos (descarte definitivo).
//
// No es error si los archivos ya no existen (idempotente).
func (q *FileQueue) Delete(eventID string) error {
	binPath := filepath.Join(q.dir, eventID+".bin")
	jsonPath := filepath.Join(q.dir, eventID+".json")
	_ = os.Remove(binPath)
	if err := os.Remove(jsonPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// MarkAttempt actualiza retry_count y last_attempt_at del evento sin tocar
// su payload. Reescribe solo el .json (atomic via rename).
func (q *FileQueue) MarkAttempt(ev *QueuedEvent) error {
	ev.RetryCount++
	ev.LastAttemptAt = time.Now()
	data, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		return err
	}
	jsonPath := filepath.Join(q.dir, ev.ID+".json")
	return writeFileAtomic(jsonPath, data)
}

// Stats retorna telemetría útil para heartbeat / health-check.
func (q *FileQueue) Stats() (items int, bytes int64, oldestAge time.Duration) {
	entries, err := os.ReadDir(q.dir)
	if err != nil {
		return 0, 0, 0
	}
	var oldest time.Time
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		name := info.Name()
		switch filepath.Ext(name) {
		case ".json":
			items++
			if oldest.IsZero() || info.ModTime().Before(oldest) {
				oldest = info.ModTime()
			}
			bytes += info.Size()
		case ".bin":
			bytes += info.Size()
		}
	}
	if !oldest.IsZero() {
		oldestAge = time.Since(oldest)
	}
	return items, bytes, oldestAge
}

// enforceCapsLocked descarta eventos más antiguos hasta caer dentro de los
// caps configurados. DEBE invocarse con q.mu tomado.
//
// Esta es una válvula de seguridad — perder el evento más viejo es mejor
// que llenar el disco del PC del portero (lo que tumbaría el sync agent
// completo y dejaría la cámara sin sync de whitelist).
func (q *FileQueue) enforceCapsLocked() error {
	items, bytes, _ := q.Stats()
	if items < q.maxItems && bytes < q.maxBytes {
		return nil
	}

	log.Printf("[queue] ⚠ cap alcanzado (items=%d/%d bytes=%d/%d) — descartando más antiguos",
		items, q.maxItems, bytes, q.maxBytes)

	oldest, err := q.PeekOldest(10) // batch de eviction
	if err != nil {
		return err
	}
	for _, ev := range oldest {
		if items < q.maxItems && bytes < q.maxBytes {
			break
		}
		log.Printf("[queue] descartando evento %s (placa=%s, retry=%d, edad=%s)",
			ev.ID, ev.Plate, ev.RetryCount, time.Since(ev.CreatedAt).Round(time.Second))
		_ = q.Delete(ev.ID)
		items, bytes, _ = q.Stats()
	}
	return nil
}

func (q *FileQueue) readEvent(jsonFilename string) (*QueuedEvent, error) {
	data, err := os.ReadFile(filepath.Join(q.dir, jsonFilename))
	if err != nil {
		return nil, err
	}
	var ev QueuedEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil, err
	}
	return &ev, nil
}

// writeFileAtomic escribe `data` a `path` de forma atómica (write a .tmp + rename).
// En POSIX rename es atómico — readers nunca ven un archivo a medio escribir.
//
// En Windows rename es atómico solo si el destino no existe; para sobrescribir
// usa MoveFileEx con MOVEFILE_REPLACE_EXISTING (lo que hace os.Rename
// internamente con la API moderna desde Go 1.5+).
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
