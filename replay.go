package main

import (
	"context"
	"log"
	"time"
)

// ReplayWorker drena la queue del receiver subiendo eventos al cloud.
// Corre como goroutine separada de Syncer (que maneja whitelist/heartbeat).
// Ambos comparten el mismo CloudClient pero usan endpoints distintos.
//
// Estrategia:
//
//  1. Cada `tickInterval` lee los N más antiguos pendientes (default 10).
//  2. Para cada uno, intenta `cloud.PostEventMultipart`.
//  3. Si éxito: borrar del queue.
//  4. Si permanente (4xx): borrar del queue + log warning (payload inválido,
//     llave revocada: reintentar no cambia nada).
//  5. Si transitorio (5xx, red, 429): MarkAttempt y esperar el back-off.
//     **Nunca se descarta por transitorio.** Antes se descartaba al quinto
//     intento —unos 81 minutos de back-off acumulado—, así que un corte de
//     internet de una tarde borraba las fotos de esa tarde: justo lo que la
//     cola existe para evitar. El único techo es el de la cola misma
//     (10.000 / 1 GB), que expulsa lo más viejo.
type ReplayWorker struct {
	cloud        *CloudClient
	queue        *FileQueue
	tickInterval time.Duration

	// batchSize: eventos a procesar por tick. 10 evita ráfagas que sobrecarguen
	// el cloud cuando vuelve internet tras outage largo.
	batchSize int

	// ahora se inyecta en pruebas para no dormir de verdad.
	ahora func() time.Time
}

func NewReplayWorker(cloud *CloudClient, queue *FileQueue, tickSeconds int) *ReplayWorker {
	return &ReplayWorker{
		cloud:        cloud,
		queue:        queue,
		tickInterval: time.Duration(tickSeconds) * time.Second,
		batchSize:    10,
		ahora:        time.Now,
	}
}

// Run bloquea procesando eventos hasta ctx.Done(). Debe llamarse en goroutine.
func (w *ReplayWorker) Run(ctx context.Context) {
	log.Printf("[replay] iniciando worker (tick=%s, batch=%d)", w.tickInterval, w.batchSize)

	// Primer ciclo inmediato — si el agent arranca con queue acumulada de
	// una sesión anterior, drenamos ya.
	w.processOnce(ctx)

	ticker := time.NewTicker(w.tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[replay] contexto cancelado, deteniendo worker")
			return
		case <-ticker.C:
			w.processOnce(ctx)
		}
	}
}

// processOnce hace una pasada por la queue: lee batch, intenta cada uno.
func (w *ReplayWorker) processOnce(ctx context.Context) {
	events, err := w.queue.PeekOldest(w.batchSize)
	if err != nil {
		log.Printf("[replay] peek queue falló: %v", err)
		return
	}
	if len(events) == 0 {
		return
	}

	now := w.ahora()
	for _, ev := range events {
		// Back-off: si este evento intentó hace poco y falló, no insistimos.
		if !w.eligibleForRetry(ev, now) {
			continue
		}

		// Timeout corto por evento — el upload de 1 imagen no debería tomar
		// más de 30s en conexiones típicas residenciales.
		evCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		w.tryOne(evCtx, ev)
		cancel()

		// Permitir cancelación rápida si el agent se detiene mid-batch.
		if ctx.Err() != nil {
			return
		}
	}
}

// tryOne procesa un evento. Maneja success / permanent / transient.
func (w *ReplayWorker) tryOne(ctx context.Context, ev *QueuedEvent) {
	snapshot, err := w.queue.Snapshot(ev.ID)
	if err != nil {
		// .bin desapareció (manual delete? corrupción del FS?) — borrar el
		// .json huérfano para no quedar atorados.
		log.Printf("[replay] snapshot %s no encontrado (.json huérfano), borrando: %v", ev.ID, err)
		_ = w.queue.Delete(ev.ID)
		return
	}
	recorte := w.queue.Recorte(ev.ID)

	status, result, err := w.cloud.PostEventMultipart(ctx, ev, snapshot, recorte)

	switch status {
	case PostEventSuccess:
		detalle := "-"
		if result != nil {
			detalle = result.Status
			if result.BlocklistHit != "" {
				detalle += " lista_negra=" + result.BlocklistHit
			}
		}
		log.Printf("[replay] ✓ enviado evento %s placa=%s (%s)", ev.ID, ev.Plate, detalle)
		_ = w.queue.Delete(ev.ID)

	case PostEventPermanent:
		// 4xx — llave revocada, payload malo, captura apagada. Reintentar no
		// va a cambiar el resultado. Descartar + warning.
		log.Printf("[replay] ✗ rechazo permanente evento %s placa=%s: %v (descartando)",
			ev.ID, ev.Plate, err)
		_ = w.queue.Delete(ev.ID)

	case PostEventTransient:
		// Red caída / cloud temporal / 5xx. Se anota el intento para el
		// back-off y se deja en la cola: volverá cuando vuelva internet.
		_ = w.queue.MarkAttempt(ev)
		log.Printf("[replay] ↻ intento %d evento %s placa=%s (siguiente en %s): %v",
			ev.RetryCount, ev.ID, ev.Plate, esperaDeReintento(ev.RetryCount), err)
	}
}

// esperaDeReintento es el back-off por evento, acotado a 5 minutos: con
// internet caído no hay nada que martillar, y cuando vuelve conviene que la
// cola drene en minutos, no en horas. Tabla:
//
//	1 fallo → 30 s, 2 → 1 min, 3 → 2 min, 4+ → 5 min.
func esperaDeReintento(intentos int) time.Duration {
	if intentos <= 0 {
		return 0
	}
	delays := []time.Duration{30 * time.Second, 1 * time.Minute, 2 * time.Minute, 5 * time.Minute}
	idx := intentos - 1
	if idx >= len(delays) {
		idx = len(delays) - 1
	}
	return delays[idx]
}

// eligibleForRetry aplica esperaDeReintento sobre last_attempt_at. El primer
// intento de cada evento es inmediato: el caso normal no espera nada.
func (w *ReplayWorker) eligibleForRetry(ev *QueuedEvent, now time.Time) bool {
	if ev.RetryCount == 0 {
		return true
	}
	return now.Sub(ev.LastAttemptAt) >= esperaDeReintento(ev.RetryCount)
}
