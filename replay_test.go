package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// El replay worker: qué se descarta y qué se queda esperando.
//
// Un corte de internet de una tarde no puede borrar las fotos de esa tarde.
// Antes, al quinto intento fallido (unos 81 minutos de back-off) el evento
// se descartaba: justo lo que la cola existe para evitar.

func nuevoReplayDePrueba(t *testing.T, handler http.HandlerFunc) (*ReplayWorker, *FileQueue) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	q, err := NewFileQueue(t.TempDir(), 100, 100*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	w := NewReplayWorker(NewCloudClient(srv.URL, "ppk_x"), q, 30)
	return w, q
}

func TestUn5xxNoDescartaNuncaPorMuchosIntentos(t *testing.T) {
	var llamadas int32
	w, q := nuevoReplayDePrueba(t, func(rw http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&llamadas, 1)
		rw.WriteHeader(http.StatusBadGateway)
	})
	ev := &QueuedEvent{Plate: "AAA111"}
	_ = q.Enqueue(ev, []byte("jpg"))

	// Veinte intentos, cada uno «después» del back-off: el reloj inyectado
	// avanza para no dormir.
	ahora := time.Now()
	w.ahora = func() time.Time { return ahora }
	for i := 0; i < 20; i++ {
		w.processOnce(context.Background())
		ahora = ahora.Add(10 * time.Minute)
	}
	items, _, _ := q.Stats()
	if items != 1 {
		t.Fatalf("el evento se descartó tras %d intentos con 5xx; debía seguir en cola", llamadas)
	}
	if atomic.LoadInt32(&llamadas) != 20 {
		t.Errorf("esperaba 20 intentos, hubo %d", llamadas)
	}
	got, _ := q.PeekOldest(1)
	if got[0].RetryCount != 20 {
		t.Errorf("retry_count=%d, esperaba 20", got[0].RetryCount)
	}
}

func TestUnErrorDeRedNoDescarta(t *testing.T) {
	q, _ := NewFileQueue(t.TempDir(), 100, 100*1024*1024)
	// Puerto cerrado: connection refused.
	w := NewReplayWorker(NewCloudClient("http://127.0.0.1:1", "ppk_x"), q, 30)
	_ = q.Enqueue(&QueuedEvent{Plate: "AAA111"}, []byte("jpg"))

	w.processOnce(context.Background())

	items, _, _ := q.Stats()
	if items != 1 {
		t.Fatal("sin internet el evento tiene que quedarse en la cola")
	}
}

func TestUn4xxDescartaDeUna(t *testing.T) {
	w, q := nuevoReplayDePrueba(t, func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(http.StatusUnprocessableEntity)
	})
	_ = q.Enqueue(&QueuedEvent{Plate: "AAA111"}, []byte("jpg"))

	w.processOnce(context.Background())

	items, _, _ := q.Stats()
	if items != 0 {
		t.Fatal("un 422 es permanente: reintentar no lo arregla y hay que soltar la cola")
	}
}

func TestUn2xxBorraDeLaCola(t *testing.T) {
	w, q := nuevoReplayDePrueba(t, func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"event_id":7,"status":"created"}`))
	})
	_ = q.Enqueue(&QueuedEvent{Plate: "AAA111"}, []byte("jpg"))

	w.processOnce(context.Background())

	items, _, _ := q.Stats()
	if items != 0 {
		t.Fatal("tras un 2xx el evento debe salir de la cola")
	}
}

func TestElBackoffSeAcotaACincoMinutos(t *testing.T) {
	casos := map[int]time.Duration{
		0:   0,
		1:   30 * time.Second,
		2:   1 * time.Minute,
		3:   2 * time.Minute,
		4:   5 * time.Minute,
		50:  5 * time.Minute,
		500: 5 * time.Minute,
	}
	for intentos, esperado := range casos {
		if got := esperaDeReintento(intentos); got != esperado {
			t.Errorf("esperaDeReintento(%d)=%s, esperaba %s", intentos, got, esperado)
		}
	}
}

func TestElBackoffRespetaLaEspera(t *testing.T) {
	var llamadas int32
	w, q := nuevoReplayDePrueba(t, func(rw http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&llamadas, 1)
		rw.WriteHeader(http.StatusInternalServerError)
	})
	_ = q.Enqueue(&QueuedEvent{Plate: "AAA111"}, []byte("jpg"))

	ahora := time.Now()
	w.ahora = func() time.Time { return ahora }
	w.processOnce(context.Background()) // intento 1
	w.processOnce(context.Background()) // hace 0 s: no toca
	if atomic.LoadInt32(&llamadas) != 1 {
		t.Fatalf("reintentó sin esperar el back-off: %d llamadas", llamadas)
	}
	ahora = ahora.Add(31 * time.Second)
	w.processOnce(context.Background()) // ya pasaron 30 s
	if atomic.LoadInt32(&llamadas) != 2 {
		t.Fatalf("no reintentó pasado el back-off: %d llamadas", llamadas)
	}
}

func TestElRecorteViajaConLaEscena(t *testing.T) {
	var partes map[string][]byte
	w, q := nuevoReplayDePrueba(t, func(rw http.ResponseWriter, r *http.Request) {
		_, partes = leerMultipart(t, r.Body, r.Header.Get("Content-Type"))
		_, _ = rw.Write([]byte(`{}`))
	})
	_ = q.EnqueueConRecorte(&QueuedEvent{Plate: "AAA111"}, []byte("escena"), []byte("recorte"))

	w.processOnce(context.Background())

	if string(partes["snapshot"]) != "escena" || string(partes["plate_crop"]) != "recorte" {
		t.Errorf("partes recibidas: %q", partes)
	}
}
