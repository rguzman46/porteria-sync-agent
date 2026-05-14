package main

import (
	"context"
	"log"
	"strconv"
	"time"
)

// ReplayWorker drena la queue del receiver subiendo eventos al cloud.
// Corre como goroutine separada de Syncer (que maneja whitelist/heartbeat).
// Ambos comparten el mismo CloudClient pero usan endpoints distintos.
//
// Estrategia:
//
//  1. Cada `tickInterval` lee los N más antiguos pendientes (default 10).
//  2. Para cada uno, intenta `cloud.PostEventMultipart(ev, snapshot)`.
//  3. Si éxito: borrar del queue.
//  4. Si permanent (4xx): borrar del queue + log warning (config inválida,
//     no tiene sentido reintentar).
//  5. Si transient (5xx, network): MarkAttempt (incrementa retry_count) +
//     dejar para el próximo tick. Si retry_count > MaxRetries, descartar
//     y log warning (probablemente algo más serio — auth roto, etc.).
//
// Back-off por evento: aplicamos un delay basado en `retry_count` para
// no martillar al cloud con eventos que están fallando consistentemente.
// Solo procesamos eventos cuyo `last_attempt_at` sea más antiguo que el
// delay correspondiente.
type ReplayWorker struct {
	cloud        *CloudClient
	queue        *FileQueue
	tickInterval time.Duration

	// MaxRetries antes de descartar. 5 es generoso pero finito —
	// outage de 6h con 30s tick = 720 tries, pero con back-off escalado
	// alcanza ~3-4h efectivos antes de descarte. Suficiente para outages
	// realistas.
	maxRetries int

	// batchSize: eventos a procesar por tick. 10 evita ráfagas que sobrecarguen
	// el cloud cuando vuelve internet tras outage largo.
	batchSize int
}

func NewReplayWorker(cloud *CloudClient, queue *FileQueue, tickSeconds int) *ReplayWorker {
	return &ReplayWorker{
		cloud:        cloud,
		queue:        queue,
		tickInterval: time.Duration(tickSeconds) * time.Second,
		maxRetries:   5,
		batchSize:    10,
	}
}

// Run bloquea procesando eventos hasta ctx.Done(). Debe llamarse en goroutine.
func (w *ReplayWorker) Run(ctx context.Context) {
	log.Printf("[replay] iniciando worker (tick=%s, batch=%d, max_retries=%d)",
		w.tickInterval, w.batchSize, w.maxRetries)

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

	now := time.Now()
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

	status, result, err := w.cloud.PostEventMultipart(ctx, ev, snapshot)

	switch status {
	case PostEventSuccess:
		visitaID := "-"
		if result != nil && result.VisitaID != nil {
			visitaID = "visita=" + strconv.FormatInt(*result.VisitaID, 10)
		}
		log.Printf("[replay] ✓ enviado evento %s placa=%s (%s)", ev.ID, ev.Plate, visitaID)
		_ = w.queue.Delete(ev.ID)

	case PostEventPermanent:
		// 4xx — config inválida, capture deshabilitada, payload malo, etc.
		// Reintentar no va a cambiar el resultado. Descartar + warning.
		log.Printf("[replay] ✗ rechazo permanente evento %s placa=%s: %v (descartando)",
			ev.ID, ev.Plate, err)
		_ = w.queue.Delete(ev.ID)

	case PostEventTransient:
		// Red caída / cloud temporal / 5xx. Reintentar más tarde con back-off.
		_ = w.queue.MarkAttempt(ev)
		if ev.RetryCount >= w.maxRetries {
			log.Printf("[replay] ✗ evento %s placa=%s descartado tras %d reintentos: %v",
				ev.ID, ev.Plate, ev.RetryCount, err)
			_ = w.queue.Delete(ev.ID)
		} else {
			log.Printf("[replay] ↻ retry %d/%d evento %s placa=%s: %v",
				ev.RetryCount, w.maxRetries, ev.ID, ev.Plate, err)
		}
	}
}

// eligibleForRetry implementa el back-off por evento. Tabla de delays:
//
//	retry_count=0 → siempre (primer intento)
//	retry_count=1 → 1 min desde last_attempt_at
//	retry_count=2 → 5 min
//	retry_count=3 → 15 min
//	retry_count=4 → 1 hora
//	retry_count>=5 → no aplica (ya descartado por tryOne)
//
// Esto evita martillar al cloud con eventos que están fallando consistentemente
// — el primer evento de cada placa intenta inmediato (caso happy path) y solo
// los que rebotan se ralentizan.
func (w *ReplayWorker) eligibleForRetry(ev *QueuedEvent, now time.Time) bool {
	if ev.RetryCount == 0 {
		return true
	}
	delays := []time.Duration{
		1 * time.Minute,
		5 * time.Minute,
		15 * time.Minute,
		60 * time.Minute,
	}
	idx := ev.RetryCount - 1
	if idx >= len(delays) {
		idx = len(delays) - 1
	}
	return now.Sub(ev.LastAttemptAt) >= delays[idx]
}

