package main

import (
	"context"
	"log"
	"time"
)

// Syncer orquesta el ciclo de vida del agent. Cada `interval` segundos:
//  1. Manda heartbeat al cloud.
//  2. Pregunta si hay whitelist nuevo (If-Modified-Since).
//  3. Si lo hay, lo empuja a la cámara local.
//
// Resiliencia ante caídas:
//  - Si el cloud falla: log + back-off exponencial. La cámara YA tiene el
//    último whitelist sincronizado, así que la portería sigue funcionando.
//  - Si la cámara falla en el push: log + retry en el próximo ciclo. El
//    cloud volverá a marcar el whitelist como modificado en cuanto se
//    pueda re-sincronizar.
//
// Si el agent se detiene (Windows Service stop): el último whitelist queda
// en la cámara. La portería opera offline indefinidamente.
type Syncer struct {
	cloud    *CloudClient
	camera   CameraAdapter
	interval time.Duration

	consecutiveErrors int
}

func NewSyncer(cloud *CloudClient, camera CameraAdapter, intervalSeconds int) *Syncer {
	return &Syncer{
		cloud:    cloud,
		camera:   camera,
		interval: time.Duration(intervalSeconds) * time.Second,
	}
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
	if _, err := s.cloud.Heartbeat(cycleCtx); err != nil {
		s.consecutiveErrors++
		log.Printf("[sync] heartbeat falló (consecutivos=%d): %v", s.consecutiveErrors, err)
		s.backoff()
		return
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

	// 3) Push a la cámara.
	log.Printf("[sync] whitelist actualizado: %d placas (versión %s)", len(wl.Plates), wl.Version)
	if err := s.camera.SyncWhitelist(cycleCtx, wl.Plates); err != nil {
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
	s.cloud.AckPending()
	log.Printf("[sync] cámara sincronizada exitosamente")
}

// backoff duerme proporcionalmente al número de errores consecutivos, con
// cap de 5min. Evita martillar al cloud / cámara cuando algo va mal.
//
// Curva: 1 error = 0s extra (sigue el interval normal),
//        2 errores = 30s extra, 3 = 60s, 4 = 2min, 5+ = 5min.
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
