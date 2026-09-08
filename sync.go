package main

import (
	"context"
	"log"
	"sync"
	"time"
)

// Syncer orquesta el ciclo de vida del agent. Cada `interval` segundos:
//  1. Manda heartbeat al cloud.
//  2. Pregunta si hay whitelist nuevo (If-Modified-Since).
//  3. Si lo hay, lo empuja a la cámara local.
//
// V1.3+: el Syncer también detecta cambios de `vendor_family` reportados
// por el cloud y hot-swap el adapter en caliente — sin reiniciar el agent.
// Si el admin cambia el modelo de la cámara en el panel (ej. reemplaza
// una DS-TCG405-E por una iDS-TCM403-MA), el siguiente heartbeat trae el
// vendor_family nuevo y el agent se reconfigura automáticamente.
//
// Resiliencia ante caídas:
//   - Si el cloud falla: log + back-off exponencial. La cámara YA tiene el
//     último whitelist sincronizado, así que la portería sigue funcionando.
//   - Si la cámara falla en el push: log + retry en el próximo ciclo. El
//     cloud volverá a marcar el whitelist como modificado en cuanto se
//     pueda re-sincronizar.
//
// Si el agent se detiene (Windows Service stop): el último whitelist queda
// en la cámara. La portería opera offline indefinidamente.
// ColaConEstadisticas es lo único que el Syncer necesita de la queue local:
// cuántos eventos hay represados. Se toma como interfaz y no como *FileQueue
// para que el receiver siga siendo opcional — cuando está apagado, el Syncer
// simplemente no tiene queue y reporta cero.
type ColaConEstadisticas interface {
	Stats() (items int, bytes int64, oldestAge time.Duration)
}

type Syncer struct {
	cloud    *CloudClient
	cfg      *Config
	interval time.Duration
	queue    ColaConEstadisticas

	mu                sync.Mutex
	camera            CameraAdapter
	consecutiveErrors int

	// Resultado del último push a la cámara, que se reporta en el siguiente
	// heartbeat. Va aquí y no en una variable local porque el heartbeat de un
	// ciclo ocurre ANTES del push de ese mismo ciclo: lo que se reporta es
	// siempre el resultado del ciclo anterior, que es justo lo que interesa
	// —si el push lleva fallando, se sabe en el siguiente latido y no cuando
	// alguien vaya a mirar el log del PC de la portería—.
	lastPushOK      *bool
	lastPushError   string
	lastPlatesCount int
}

func NewSyncer(cloud *CloudClient, cfg *Config, camera CameraAdapter, intervalSeconds int) *Syncer {
	return &Syncer{
		cloud:    cloud,
		cfg:      cfg,
		camera:   camera,
		interval: time.Duration(intervalSeconds) * time.Second,
	}
}

// ConCola le da al Syncer la queue local para que pueda reportar cuánto hay
// represado. Sin ella el agent reporta cero, que es correcto: si el receiver
// está apagado, no hay nada encolándose.
func (s *Syncer) ConCola(q ColaConEstadisticas) *Syncer {
	s.queue = q
	return s
}

// reporte arma lo que se le cuenta al cloud en este latido.
func (s *Syncer) reporte() HeartbeatReport {
	s.mu.Lock()
	defer s.mu.Unlock()

	r := HeartbeatReport{
		CameraPushOK:    s.lastPushOK,
		CameraPushError: s.lastPushError,
		PlatesPushed:    s.lastPlatesCount,
	}
	if s.queue != nil {
		items, _, _ := s.queue.Stats()
		r.QueueSize = items
	}
	return r
}

// anotarPush guarda cómo le fue al push para el siguiente latido.
func (s *Syncer) anotarPush(err error, placas int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ok := err == nil
	s.lastPushOK = &ok
	if ok {
		s.lastPushError = ""
		s.lastPlatesCount = placas
		return
	}
	s.lastPushError = err.Error()
}

// Run ejecuta el loop hasta que ctx se cancele (señal del Windows Service).
// Bloquea hasta el cierre — debe llamarse en una goroutine.
func (s *Syncer) Run(ctx context.Context) {
	// Primer ciclo inmediato (no esperar `interval` al arranque).
	s.cycle(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[sync] contexto cancelado, deteniendo loop")
			return
		case <-ticker.C:
			s.cycle(ctx)
		}
	}
}

// cycle hace una pasada completa: heartbeat → fetch whitelist → push si cambió.
//
// Recoge el error pero NO retorna — un ciclo fallido no debe abortar el agent;
// el siguiente puede tener éxito (intermitencia de red es común en porterías
// con ADSL/4G de baja calidad).
func (s *Syncer) cycle(ctx context.Context) {
	cycleCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// 1) Heartbeat. Si falla, asumimos red caída — saltamos fetch.
	hb, err := s.cloud.Heartbeat(cycleCtx, s.reporte())
	if err != nil {
		s.consecutiveErrors++
		log.Printf("[sync] heartbeat falló (consecutivos=%d): %v", s.consecutiveErrors, err)
		s.backoff()
		return
	}

	// 1.b) Auto-config: si el cloud reporta un vendor_family distinto al
	// que tenemos cargado, hot-swap el adapter sin reiniciar.
	if hb.Device != nil {
		s.maybeReconfigureAdapter(hb.Device)
	}

	// 2) Fetch whitelist con If-Modified-Since.
	wl, changed, err := s.cloud.FetchWhitelist(cycleCtx)
	if err != nil {
		s.consecutiveErrors++
		log.Printf("[sync] fetch whitelist falló (consecutivos=%d): %v", s.consecutiveErrors, err)
		s.backoff()
		return
	}

	// Heartbeat + fetch OK → resetear contador de errores.
	s.consecutiveErrors = 0

	if !changed {
		log.Printf("[sync] sin cambios (304)")
		return
	}

	// El whitelist response también trae device metadata (V1.3+) — usamos
	// para auto-config también acá por si la marca cambia entre heartbeats.
	if wl.Device != nil {
		s.maybeReconfigureAdapter(wl.Device)
	}

	// 3) Push a la cámara.
	camera := s.currentCamera()
	log.Printf("[sync] whitelist actualizado: %d placas (versión %s, adapter=%s)", len(wl.Plates), wl.Version, camera.Name())
	if err := camera.SyncWhitelist(cycleCtx, wl.Plates); err != nil {
		// Se anota para el siguiente latido: es la única forma de que el
		// panel se entere. Sin esto, este error vivía y moría en el log de un
		// computador al que hay que ir a mirar.
		s.anotarPush(err, 0)
		log.Printf("[sync] push a cámara falló: %v", err)
		// NO incrementar consecutiveErrors aquí — la red al cloud anda bien.
		// NO hacemos AckPending — el próximo FetchWhitelist re-pedirá el
		// mismo whitelist (If-Modified-Since seguirá siendo el viejo
		// acknowledged) y reintentaremos el push. Sin esta separación, el
		// cloud responderia 304 y la cámara quedaría desincronizada hasta
		// que algo cambiase en BD.
		return
	}

	// Push exitoso → confirmar al cloud client que ya tenemos esta versión.
	// Las siguientes requests usarán este Last-Modified como If-Modified-Since.
	s.anotarPush(nil, len(wl.Plates))
	s.cloud.AckPending()
	log.Printf("[sync] cámara sincronizada exitosamente")
}

// currentCamera devuelve el adapter activo de forma thread-safe (puede
// estar siendo reconfigurado en otro goroutine vía maybeReconfigureAdapter).
func (s *Syncer) currentCamera() CameraAdapter {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.camera
}

// maybeReconfigureAdapter detecta si el cloud reporta un vendor_family
// distinto al adapter actual y, si auto_config está habilitado, hot-swap
// el adapter en caliente.
//
// Casos:
//   - Cloud reporta family vacío → no hace nada (config legacy / sin modelo).
//   - Cloud reporta family == adapter actual → no-op.
//   - Cloud reporta family distinto:
//     a) auto_config=true → construir nuevo adapter + swap atómico.
//     b) auto_config=false → log warning, mantener actual.
//
// Si el constructor del nuevo adapter falla (vendor_family desconocido),
// mantenemos el adapter actual y logueamos error — no aborta el sync.
func (s *Syncer) maybeReconfigureAdapter(meta *DeviceMetadata) {
	if meta == nil || meta.VendorFamily == "" {
		return
	}

	current := s.currentCamera()
	if current.Name() == meta.VendorFamily {
		return // no hay cambio
	}

	if s.cfg.Camera.AutoConfig != nil && !*s.cfg.Camera.AutoConfig {
		log.Printf("[sync] cloud reporta vendor_family=%s pero adapter actual=%s (auto_config=false — no reconfigurando)",
			meta.VendorFamily, current.Name())
		return
	}

	// Build new adapter con la family reportada por el cloud. Construimos
	// un cfg temporal con la family override para que el factory la use.
	tmpCfg := *s.cfg
	tmpCfg.Camera.Family = meta.VendorFamily

	newAdapter, err := NewCameraAdapter(&tmpCfg)
	if err != nil {
		log.Printf("[sync] auto-config falló: cloud reporta family=%s pero el adapter no se pudo construir: %v",
			meta.VendorFamily, err)
		return
	}

	s.mu.Lock()
	s.camera = newAdapter
	s.cfg.Camera.Family = meta.VendorFamily
	s.mu.Unlock()

	log.Printf("[sync] adapter reconfigurado: %s → %s (modelo=%s reportado por cloud)",
		current.Name(), newAdapter.Name(), meta.DeviceModel)
}

// backoff duerme proporcionalmente al número de errores consecutivos, con
// cap de 5min. Evita martillar al cloud / cámara cuando algo va mal.
//
// Curva: 1 error = 0s extra (sigue el interval normal),
//
//	2 errores = 30s extra, 3 = 60s, 4 = 2min, 5+ = 5min.
func (s *Syncer) backoff() {
	if s.consecutiveErrors <= 1 {
		return
	}
	delays := []time.Duration{30 * time.Second, 60 * time.Second, 2 * time.Minute, 5 * time.Minute}
	idx := s.consecutiveErrors - 2
	if idx >= len(delays) {
		idx = len(delays) - 1
	}
	time.Sleep(delays[idx])
}
