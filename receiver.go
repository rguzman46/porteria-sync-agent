package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// Receiver es el HTTP server LAN que recibe multipart de la cámara LPR
// (Módulo LPR — Día 8). Bind default a `0.0.0.0:8787` para que la cámara
// (típicamente en otra IP de la LAN del conjunto) pueda postearle.
//
// **Sin auth**: la red local del conjunto es zona de confianza (firewall
// del router). Añadir Bearer aquí complicaría el setup de Hikvision sin
// ganancia de seguridad real — quien comprometa la LAN ya tiene acceso
// directo a la cámara/grabador.
//
// **Resiliencia offline**: el handler responde 202 Accepted en cuanto
// encola el evento. El envío al cloud es asíncrono (replay worker en
// goroutine separada). La cámara NO bloquea esperando confirmación del
// cloud — gana resiliencia natural ante outages de internet.
type Receiver struct {
	cfg     *Config
	queue   *FileQueue
	server  *http.Server
	maxSize int64
}

// MaxRequestSize: 10MB es generoso para 1 imagen + metadata. Defense
// contra clientes maliciosos que intenten DoS via uploads enormes.
const MaxRequestSize = 10 * 1024 * 1024

// MaxSnapshotSize: 5MB alineado con el endpoint cloud (`StoreAccessEventMultipartRequest`
// rule `max:5120`). Mayor que esto es ya basura — rechazamos local antes
// de encolar.
const MaxSnapshotSize = 5 * 1024 * 1024

// NewReceiver construye el receiver listo para Start(). NO arranca el listener
// — debes llamar Start(ctx) en una goroutine.
func NewReceiver(cfg *Config, queue *FileQueue) *Receiver {
	mux := http.NewServeMux()
	r := &Receiver{
		cfg:     cfg,
		queue:   queue,
		maxSize: MaxRequestSize,
	}
	mux.HandleFunc("/lpr-event", r.handleLprEvent)
	mux.HandleFunc("/health", r.handleHealth)

	r.server = &http.Server{
		Addr:              cfg.Receiver.BindAddress,
		Handler:           mux,
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	return r
}

// Start bloquea hasta que ctx se cancele, momento en el cual hace un
// graceful shutdown (drena requests en vuelo con timeout 5s).
func (r *Receiver) Start(ctx context.Context) error {
	log.Printf("[receiver] escuchando en %s (queue=%s)", r.cfg.Receiver.BindAddress, r.cfg.Receiver.QueueDir)

	// Arrancar listener en goroutine — el bloqueo lo hacemos esperando ctx.
	errCh := make(chan error, 1)
	go func() {
		err := r.server.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		log.Println("[receiver] contexto cancelado, shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return r.server.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// handleLprEvent procesa un multipart de la cámara. Dispatch por header
// `X-Agent-Source`. Si el header no viene, asume hikvision (vendor más
// común y único estable en V2).
func (r *Receiver) handleLprEvent(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Limit request size defensivo. Sin esto, una cámara malconfigurada
	// (o atacante en LAN) podría llenar la queue con uploads huge.
	req.Body = http.MaxBytesReader(w, req.Body, r.maxSize)

	source := strings.ToLower(req.Header.Get("X-Agent-Source"))
	if source == "" {
		source = "hikvision" // default — vendor más común
	}

	var (
		event    *QueuedEvent
		snapshot []byte
		err      error
	)

	switch source {
	case "hikvision":
		event, snapshot, err = parseHikvisionMultipart(req)
	case "generic", "json":
		event, snapshot, err = parseGenericMultipart(req)
	default:
		http.Error(w, fmt.Sprintf("X-Agent-Source desconocido: %q (soportados: hikvision, generic)", source), http.StatusBadRequest)
		return
	}

	if err != nil {
		log.Printf("[receiver] parse error (source=%s, ip=%s): %v", source, clientIP(req), err)
		http.Error(w, "parse error: "+err.Error(), http.StatusBadRequest)
		return
	}

	if event.Plate == "" {
		http.Error(w, "evento sin placa", http.StatusBadRequest)
		return
	}

	if len(snapshot) == 0 {
		// Sin snapshot no encolamos — este endpoint es específicamente para
		// captura visual. Si la cámara reporta solo texto, debe postear al
		// endpoint legacy /api/access/event directamente.
		http.Error(w, "evento sin snapshot (este endpoint requiere imagen — usa /api/access/event para texto-only)", http.StatusBadRequest)
		return
	}

	if len(snapshot) > MaxSnapshotSize {
		http.Error(w, fmt.Sprintf("snapshot excede %d bytes (recibido %d)", MaxSnapshotSize, len(snapshot)), http.StatusRequestEntityTooLarge)
		return
	}

	if err := r.queue.Enqueue(event, snapshot); err != nil {
		log.Printf("[receiver] enqueue falló: %v", err)
		http.Error(w, "no se pudo encolar el evento", http.StatusInternalServerError)
		return
	}

	log.Printf("[receiver] encolado evento placa=%s bytes=%d source=%s ip=%s",
		event.Plate, len(snapshot), source, clientIP(req))

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":       true,
		"event_id": event.ID,
		"queued":   true,
	})
}

// handleHealth retorna telemetría útil para el admin diagnosticando el agent.
// Sin auth — la LAN del conjunto es zona de confianza.
func (r *Receiver) handleHealth(w http.ResponseWriter, req *http.Request) {
	items, bytes, oldest := r.queue.Stats()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":              true,
		"agent_version":   AgentVersion,
		"queue_items":     items,
		"queue_bytes":     bytes,
		"queue_oldest_s":  int(oldest.Seconds()),
		"max_items":       r.cfg.Receiver.MaxQueueItems,
		"max_bytes":       r.cfg.Receiver.MaxQueueBytes,
		"server_time":     time.Now().Format(time.RFC3339),
	})
}

// clientIP best-effort para logs. No es 100% confiable (puede haber proxy
// transparente en la LAN), pero suficiente para diagnóstico.
func clientIP(req *http.Request) string {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr
	}
	return host
}

// readAllLimited es una utility wrapper que lee hasta `max` bytes con error
// explícito si excede (vs truncation silenciosa de io.LimitReader).
func readAllLimited(r io.Reader, max int64) ([]byte, error) {
	lr := io.LimitReader(r, max+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("payload excede %d bytes", max)
	}
	return data, nil
}
