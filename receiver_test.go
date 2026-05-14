package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

// Tests del Receiver. Cubren parsing multipart + dispatch por X-Agent-Source +
// validación de tamaño + integración con queue.

func newTestReceiver(t *testing.T) (*Receiver, *FileQueue, func()) {
	t.Helper()
	dir := t.TempDir()
	q, err := NewFileQueue(dir, 100, 100*1024*1024)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	cfg := &Config{}
	cfg.Receiver.Enabled = true
	cfg.Receiver.BindAddress = "127.0.0.1:0" // puerto libre
	cfg.Receiver.QueueDir = dir
	cfg.Receiver.MaxQueueItems = 100
	cfg.Receiver.MaxQueueBytes = 100 * 1024 * 1024

	r := NewReceiver(cfg, q)
	return r, q, func() { _ = q }
}

// buildGenericMultipart helper construye un body multipart con event_data + snapshot.
func buildGenericMultipart(t *testing.T, eventJSON string, snapshot []byte, snapshotMime string) (*bytes.Buffer, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)

	h1 := textproto.MIMEHeader{}
	h1.Set("Content-Disposition", `form-data; name="event_data"`)
	h1.Set("Content-Type", "application/json")
	p1, _ := mw.CreatePart(h1)
	_, _ = p1.Write([]byte(eventJSON))

	if len(snapshot) > 0 {
		h2 := textproto.MIMEHeader{}
		h2.Set("Content-Disposition", `form-data; name="snapshot"; filename="x.jpg"`)
		h2.Set("Content-Type", snapshotMime)
		p2, _ := mw.CreatePart(h2)
		_, _ = p2.Write(snapshot)
	}

	_ = mw.Close()
	return buf, mw.FormDataContentType()
}

func TestReceiverAcceptsGenericValidEvent(t *testing.T) {
	r, q, _ := newTestReceiver(t)

	body, ct := buildGenericMultipart(t,
		`{"plate":"ABC123","direction":"entry","timestamp":"2026-05-13T14:30:00-05:00"}`,
		[]byte("fake-jpeg-bytes"),
		"image/jpeg",
	)

	req := httptest.NewRequest(http.MethodPost, "/lpr-event", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Agent-Source", "generic")
	w := httptest.NewRecorder()

	r.handleLprEvent(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s, esperaba 202", w.Code, w.Body.String())
	}

	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["queued"] != true {
		t.Errorf("response.queued=%v, esperaba true", resp["queued"])
	}

	items, _, _ := q.Stats()
	if items != 1 {
		t.Errorf("queue items=%d tras encolar, esperaba 1", items)
	}

	got, _ := q.PeekOldest(1)
	if got[0].Plate != "ABC123" {
		t.Errorf("plate persistida=%q, esperaba ABC123", got[0].Plate)
	}
	if got[0].Direction != "entry" {
		t.Errorf("direction persistida=%q, esperaba entry", got[0].Direction)
	}
	snapshot, _ := q.Snapshot(got[0].ID)
	if string(snapshot) != "fake-jpeg-bytes" {
		t.Errorf("snapshot persistida=%q, esperaba fake-jpeg-bytes", snapshot)
	}
}

func TestReceiverRejectsWrongMethod(t *testing.T) {
	r, _, _ := newTestReceiver(t)
	req := httptest.NewRequest(http.MethodGet, "/lpr-event", nil)
	w := httptest.NewRecorder()
	r.handleLprEvent(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status=%d, esperaba 405", w.Code)
	}
}

func TestReceiverRejectsUnknownSource(t *testing.T) {
	r, _, _ := newTestReceiver(t)
	body, ct := buildGenericMultipart(t, `{"plate":"ABC"}`, []byte("x"), "image/jpeg")
	req := httptest.NewRequest(http.MethodPost, "/lpr-event", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Agent-Source", "unknownvendor")
	w := httptest.NewRecorder()
	r.handleLprEvent(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d, esperaba 400", w.Code)
	}
}

func TestReceiverRejectsGenericWithoutSnapshot(t *testing.T) {
	r, _, _ := newTestReceiver(t)
	// Sin snapshot — el adapter generic debe rechazar.
	body, ct := buildGenericMultipart(t, `{"plate":"ABC123"}`, nil, "")
	req := httptest.NewRequest(http.MethodPost, "/lpr-event", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Agent-Source", "generic")
	w := httptest.NewRecorder()
	r.handleLprEvent(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d, esperaba 400 (snapshot faltante)", w.Code)
	}
}

func TestReceiverRejectsGenericWithoutPlate(t *testing.T) {
	r, _, _ := newTestReceiver(t)
	body, ct := buildGenericMultipart(t, `{"plate":""}`, []byte("x"), "image/jpeg")
	req := httptest.NewRequest(http.MethodPost, "/lpr-event", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Agent-Source", "generic")
	w := httptest.NewRecorder()
	r.handleLprEvent(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d, esperaba 400 (placa vacía)", w.Code)
	}
}

func TestReceiverHealthEndpoint(t *testing.T) {
	r, q, _ := newTestReceiver(t)

	// Encolar 2 eventos para verificar que health los reporta.
	_ = q.Enqueue(&QueuedEvent{Plate: "X"}, []byte("a"))
	time.Sleep(2 * time.Millisecond)
	_ = q.Enqueue(&QueuedEvent{Plate: "Y"}, []byte("b"))

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	r.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, esperaba 200", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["ok"] != true {
		t.Errorf("health ok=%v, esperaba true", resp["ok"])
	}
	if int(resp["queue_items"].(float64)) != 2 {
		t.Errorf("queue_items=%v, esperaba 2", resp["queue_items"])
	}
}

// Test del parser Hikvision XML — caso happy path con un alert + 1 imagen.
func TestHikvisionAdapterParsesValidAlert(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<EventNotificationAlert>
	<dateTime>2026-05-13T14:32:15-05:00</dateTime>
	<eventType>ANPR</eventType>
	<ANPR>
		<licensePlate>ABC123</licensePlate>
		<direction>forward</direction>
		<confidenceLevel>98</confidenceLevel>
		<colorOfVehicle>blue</colorOfVehicle>
	</ANPR>
</EventNotificationAlert>`

	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)

	h1 := textproto.MIMEHeader{}
	h1.Set("Content-Disposition", `form-data; name="alert"`)
	h1.Set("Content-Type", "application/xml")
	p1, _ := mw.CreatePart(h1)
	_, _ = io.Copy(p1, strings.NewReader(xml))

	h2 := textproto.MIMEHeader{}
	h2.Set("Content-Disposition", `form-data; name="image"; filename="scene.jpg"`)
	h2.Set("Content-Type", "image/jpeg")
	p2, _ := mw.CreatePart(h2)
	_, _ = p2.Write([]byte("escena-completa-bytes"))

	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/lpr-event", buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	ev, snapshot, err := parseHikvisionMultipart(req)
	if err != nil {
		t.Fatalf("parseHikvisionMultipart: %v", err)
	}
	if ev.Plate != "ABC123" {
		t.Errorf("plate=%q, esperaba ABC123", ev.Plate)
	}
	if ev.Direction != "entry" {
		t.Errorf("direction=%q, esperaba entry (forward→entry)", ev.Direction)
	}
	if ev.Timestamp != "2026-05-13T14:32:15-05:00" {
		t.Errorf("timestamp=%q, no preservado", ev.Timestamp)
	}
	if string(snapshot) != "escena-completa-bytes" {
		t.Errorf("snapshot binario perdido: %q", snapshot)
	}
	if ev.Metadata["color"] != "blue" {
		t.Errorf("metadata.color=%v, esperaba blue", ev.Metadata["color"])
	}
}

func TestHikvisionAdapterIgnoresMultipleImages(t *testing.T) {
	// Hikvision a veces manda escena + crop placa + face. Solo tomamos la primera
	// (decisión Habeas Data: nunca rostros).
	xml := `<EventNotificationAlert><eventType>ANPR</eventType><ANPR><licensePlate>XYZ789</licensePlate></ANPR></EventNotificationAlert>`

	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)

	hxml := textproto.MIMEHeader{}
	hxml.Set("Content-Disposition", `form-data; name="alert"`)
	hxml.Set("Content-Type", "application/xml")
	pxml, _ := mw.CreatePart(hxml)
	_, _ = io.Copy(pxml, strings.NewReader(xml))

	// 3 imágenes — solo la primera debe quedar.
	for i, name := range []string{"scene", "plate-crop", "face"} {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="img"; filename="`+name+`.jpg"`)
		h.Set("Content-Type", "image/jpeg")
		p, _ := mw.CreatePart(h)
		_, _ = p.Write([]byte("imagen-" + name))
		_ = i
	}
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/lpr-event", buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	_, snapshot, err := parseHikvisionMultipart(req)
	if err != nil {
		t.Fatalf("parseHikvisionMultipart: %v", err)
	}
	if string(snapshot) != "imagen-scene" {
		t.Errorf("snapshot=%q, esperaba la PRIMERA imagen (scene)", snapshot)
	}
}

func TestHikvisionAdapterRejectsNonANPR(t *testing.T) {
	xml := `<EventNotificationAlert><eventType>VMD</eventType></EventNotificationAlert>`
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="alert"`)
	h.Set("Content-Type", "application/xml")
	p, _ := mw.CreatePart(h)
	_, _ = io.Copy(p, strings.NewReader(xml))
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/lpr-event", buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	_, _, err := parseHikvisionMultipart(req)
	if err == nil {
		t.Errorf("esperaba error por eventType!=ANPR, no recibí ninguno")
	}
}

// Smoke test: levantar receiver real, postear via TCP, verificar end-to-end.
func TestReceiverEndToEndOverTCP(t *testing.T) {
	dir := t.TempDir()
	q, _ := NewFileQueue(dir, 100, 100*1024*1024)
	cfg := &Config{}
	cfg.Receiver.Enabled = true
	cfg.Receiver.QueueDir = dir
	cfg.Receiver.MaxQueueItems = 100
	cfg.Receiver.MaxQueueBytes = 100 * 1024 * 1024

	// Asignar puerto libre dinámicamente (httptest no expone server.Addr para
	// nuestro http.Server; usamos un listener TCP estándar)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cfg.Receiver.BindAddress = ln.Addr().String()
	_ = ln.Close()

	r := NewReceiver(cfg, q)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Start(ctx) }()

	// Esperar a que el listener esté listo.
	time.Sleep(100 * time.Millisecond)

	body, ct := buildGenericMultipart(t, `{"plate":"E2E001","direction":"entry"}`, []byte("img-e2e"), "image/jpeg")
	req, _ := http.NewRequest(http.MethodPost, "http://"+cfg.Receiver.BindAddress+"/lpr-event", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Agent-Source", "generic")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST e2e: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	items, _, _ := q.Stats()
	if items != 1 {
		t.Errorf("queue items=%d tras POST e2e, esperaba 1", items)
	}
}
