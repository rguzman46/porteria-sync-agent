package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Tests del FileQueue. Cubren atomicidad, FIFO, eviction por caps, retry tracking.

func TestEnqueueAndPeekFIFO(t *testing.T) {
	dir := t.TempDir()
	q, err := NewFileQueue(dir, 100, 100*1024*1024)
	if err != nil {
		t.Fatalf("NewFileQueue: %v", err)
	}

	for i, plate := range []string{"AAA111", "BBB222", "CCC333"} {
		ev := &QueuedEvent{Plate: plate}
		if err := q.Enqueue(ev, []byte("snapshot-"+plate)); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
		// micro-pausa para garantizar timestamps únicos (queue ordena por
		// nanoseconds en filename — sin esto puede haber collision en CI muy rápido)
		time.Sleep(2 * time.Millisecond)
	}

	got, err := q.PeekOldest(10)
	if err != nil {
		t.Fatalf("PeekOldest: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("PeekOldest devolvió %d eventos, esperaba 3", len(got))
	}
	expected := []string{"AAA111", "BBB222", "CCC333"}
	for i, ev := range got {
		if ev.Plate != expected[i] {
			t.Errorf("orden FIFO roto: posición %d = %q, esperaba %q", i, ev.Plate, expected[i])
		}
	}
}

func TestSnapshotReturnsBinary(t *testing.T) {
	dir := t.TempDir()
	q, _ := NewFileQueue(dir, 100, 100*1024*1024)

	original := []byte("imagen-binaria-de-prueba")
	ev := &QueuedEvent{Plate: "SNAP01"}
	if err := q.Enqueue(ev, original); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	got, err := q.Snapshot(ev.ID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if string(got) != string(original) {
		t.Errorf("Snapshot devolvió %q, esperaba %q", got, original)
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	q, _ := NewFileQueue(dir, 100, 100*1024*1024)

	ev := &QueuedEvent{Plate: "DEL01"}
	_ = q.Enqueue(ev, []byte("x"))

	if err := q.Delete(ev.ID); err != nil {
		t.Fatalf("Delete 1: %v", err)
	}
	// Segunda vez debe ser no-op (idempotente)
	if err := q.Delete(ev.ID); err != nil {
		t.Fatalf("Delete 2 (idempotente): %v", err)
	}

	if _, err := q.Snapshot(ev.ID); !os.IsNotExist(err) {
		t.Errorf("Snapshot tras Delete debería ser ErrNotExist, recibí: %v", err)
	}
}

func TestMarkAttemptIncrementsRetryCount(t *testing.T) {
	dir := t.TempDir()
	q, _ := NewFileQueue(dir, 100, 100*1024*1024)

	ev := &QueuedEvent{Plate: "RETRY01"}
	_ = q.Enqueue(ev, []byte("x"))

	if err := q.MarkAttempt(ev); err != nil {
		t.Fatalf("MarkAttempt: %v", err)
	}
	if err := q.MarkAttempt(ev); err != nil {
		t.Fatalf("MarkAttempt 2: %v", err)
	}

	// Re-leer y verificar persistencia
	got, _ := q.PeekOldest(1)
	if len(got) != 1 || got[0].RetryCount != 2 {
		t.Errorf("retry_count persistido = %d, esperaba 2", got[0].RetryCount)
	}
	if got[0].LastAttemptAt.IsZero() {
		t.Errorf("last_attempt_at no persistió")
	}
}

func TestEvictionWhenItemsCapReached(t *testing.T) {
	dir := t.TempDir()
	q, _ := NewFileQueue(dir, 3, 100*1024*1024) // cap a 3 items

	for i := 0; i < 5; i++ {
		ev := &QueuedEvent{Plate: "EVICT0" + string(rune('1'+i))}
		_ = q.Enqueue(ev, []byte("x"))
		time.Sleep(2 * time.Millisecond)
	}

	items, _, _ := q.Stats()
	if items > 3 {
		t.Errorf("eviction no funcionó: %d items, cap=3", items)
	}

	// Los más antiguos (EVICT01, EVICT02) deberían estar descartados.
	// Verificar que EVICT05 (el más nuevo) sigue.
	got, _ := q.PeekOldest(10)
	found := false
	for _, ev := range got {
		if ev.Plate == "EVICT05" {
			found = true
		}
	}
	if !found {
		t.Errorf("EVICT05 (el más nuevo) debería estar en queue, no está: %+v", got)
	}
}

func TestAtomicityViaRename(t *testing.T) {
	// Verifica que un .tmp huérfano (interrupción mid-write) NO se mezcle
	// en PeekOldest. El glob filtra por .json — el .tmp queda invisible.
	dir := t.TempDir()
	q, _ := NewFileQueue(dir, 100, 100*1024*1024)

	// Simular un .tmp huérfano (como si el agent hubiera muerto mid-write).
	tmpPath := filepath.Join(dir, "99999999_corrupt.json.tmp")
	if err := os.WriteFile(tmpPath, []byte("{partial json"), 0o644); err != nil {
		t.Fatalf("crear tmp huérfano: %v", err)
	}

	// Encolar un evento real.
	ev := &QueuedEvent{Plate: "REAL01"}
	_ = q.Enqueue(ev, []byte("x"))

	got, _ := q.PeekOldest(10)
	if len(got) != 1 {
		t.Errorf("PeekOldest debería ignorar .tmp huérfanos, devolvió %d", len(got))
	}
}

func TestStatsReturnsCounts(t *testing.T) {
	dir := t.TempDir()
	q, _ := NewFileQueue(dir, 100, 100*1024*1024)

	items, bytes, oldest := q.Stats()
	if items != 0 || bytes != 0 || oldest != 0 {
		t.Errorf("Stats vacío esperado, recibí items=%d bytes=%d oldest=%v", items, bytes, oldest)
	}

	for i := 0; i < 3; i++ {
		_ = q.Enqueue(&QueuedEvent{Plate: "X"}, []byte("snapshot-data-aquí"))
		time.Sleep(2 * time.Millisecond)
	}

	items, bytes, oldest = q.Stats()
	if items != 3 {
		t.Errorf("Stats items=%d, esperaba 3", items)
	}
	if bytes == 0 {
		t.Errorf("Stats bytes=0, esperaba >0")
	}
	if oldest <= 0 {
		t.Errorf("Stats oldest=%v, esperaba >0", oldest)
	}
}
