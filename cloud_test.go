package main

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Lo que el agente le manda al cloud por cada lectura: el contrato del
// multipart, campo por campo.

// leerMultipart devuelve el JSON de event_data y las partes binarias por nombre.
func leerMultipart(t *testing.T, body io.Reader, contentType string) (map[string]any, map[string][]byte) {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("content-type: %v", err)
	}
	mr := multipart.NewReader(body, params["boundary"])
	datos := map[string]any{}
	partes := map[string][]byte{}
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("parte: %v", err)
		}
		raw, _ := io.ReadAll(p)
		if p.FormName() == "event_data" {
			if err := json.Unmarshal(raw, &datos); err != nil {
				t.Fatalf("event_data no es JSON: %v", err)
			}
			continue
		}
		partes[p.FormName()] = raw
	}
	return datos, partes
}

func TestElEventoLlevaClientEventIdYNoLlevaTimestampVacio(t *testing.T) {
	ev := &QueuedEvent{ID: "0001_abc", Plate: "ABC123"}
	body, ct, err := buildEventMultipartBody(ev, []byte("jpg"), nil)
	if err != nil {
		t.Fatal(err)
	}
	datos, partes := leerMultipart(t, body, ct)

	if datos["client_event_id"] != "0001_abc" {
		t.Errorf("client_event_id=%v, esperaba el ID de la cola", datos["client_event_id"])
	}
	if _, hay := datos["timestamp"]; hay {
		// `""` era un 422, y un 4xx es permanente: la foto se descartaba.
		t.Errorf("timestamp vacío no debe viajar: %v", datos)
	}
	if _, hay := datos["direction"]; hay {
		t.Errorf("direction vacía no debe viajar: %v", datos)
	}
	if string(partes["snapshot"]) != "jpg" {
		t.Errorf("snapshot no llegó: %q", partes["snapshot"])
	}
	if _, hay := partes["plate_crop"]; hay {
		t.Errorf("no había recorte y viajó uno")
	}
}

func TestElEventoLlevaConfianzaRecuadroYRecorte(t *testing.T) {
	conf := 97.0
	ev := &QueuedEvent{
		ID:            "0002_abc",
		ClientEventID: "dahua-estable",
		Plate:         "ABC123",
		Direction:     "entry",
		Timestamp:     "2026-05-13T14:32:15-05:00",
		Confidence:    &conf,
		PlateBox:      &PlateBox{XMin: 10, YMin: 20, XMax: 110, YMax: 60},
		Metadata:      map[string]any{"source": "hikvision"},
	}
	body, ct, err := buildEventMultipartBody(ev, []byte("escena"), []byte("recorte"))
	if err != nil {
		t.Fatal(err)
	}
	datos, partes := leerMultipart(t, body, ct)

	if datos["client_event_id"] != "dahua-estable" {
		t.Errorf("client_event_id=%v, esperaba el estable del adaptador", datos["client_event_id"])
	}
	if datos["confidence"] != 97.0 {
		t.Errorf("confidence=%v", datos["confidence"])
	}
	box, _ := datos["plate_box"].(map[string]any)
	if box["xmin"] != 10.0 || box["ymax"] != 60.0 {
		t.Errorf("plate_box=%v", datos["plate_box"])
	}
	if datos["timestamp"] != "2026-05-13T14:32:15-05:00" || datos["direction"] != "entry" {
		t.Errorf("timestamp/direction: %v", datos)
	}
	if string(partes["plate_crop"]) != "recorte" {
		t.Errorf("plate_crop=%q", partes["plate_crop"])
	}
}

func TestLaConfianzaSaleDeMetadataSiElAdaptadorLaDejoAhi(t *testing.T) {
	ev := &QueuedEvent{ID: "x", Plate: "ABC123", Metadata: map[string]any{"confidence_level": "98"}}
	body, ct, _ := buildEventMultipartBody(ev, []byte("jpg"), nil)
	datos, _ := leerMultipart(t, body, ct)
	if datos["confidence"] != 98.0 {
		t.Errorf("confidence=%v, esperaba 98 desde metadata.confidence_level", datos["confidence"])
	}
}

func TestLasRutasVanVersionadasYConCabeceras(t *testing.T) {
	var rutas []string
	var cabeceras []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rutas = append(rutas, r.URL.Path)
		cabeceras = append(cabeceras, r.Header.Clone())
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			_, _ = w.Write([]byte(`{"ok":true,"server_time":"x","whitelist_version":"v"}`))
		case strings.HasSuffix(r.URL.Path, "/whitelist"):
			_, _ = w.Write([]byte(`{"version":"v","plates":[{"plate":"AAA111"}],"blocked_plates":[{"plate":"ZZZ999"}]}`))
		default:
			_, _ = w.Write([]byte(`{"event_id":1,"status":"created"}`))
		}
	}))
	defer srv.Close()

	c := NewCloudClient(srv.URL, "ppk_x").ConDeviceToken("ppd_y")
	ctx := context.Background()
	if _, err := c.Heartbeat(ctx, HeartbeatReport{}); err != nil {
		t.Fatal(err)
	}
	wl, _, err := c.FetchWhitelist(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.PostEventMultipart(ctx, &QueuedEvent{ID: "e", Plate: "AAA111"}, []byte("jpg"), nil); err != nil {
		t.Fatal(err)
	}

	esperadas := []string{"/api/v1/access/heartbeat", "/api/v1/access/whitelist", "/api/v1/access/event/multipart"}
	for i, e := range esperadas {
		if rutas[i] != e {
			t.Errorf("ruta %d = %q, esperaba %q", i, rutas[i], e)
		}
		if cabeceras[i].Get("X-Device-Token") != "ppd_y" {
			t.Errorf("ruta %s sin X-Device-Token", e)
		}
		if cabeceras[i].Get("Authorization") != "Bearer ppk_x" {
			t.Errorf("ruta %s sin Bearer", e)
		}
		if !strings.HasPrefix(cabeceras[i].Get("User-Agent"), "PorteriaSyncAgent/") {
			t.Errorf("ruta %s sin User-Agent del agente", e)
		}
	}
	if len(wl.BlockedPlates) != 1 || wl.BlockedPlates[0].Plate != "ZZZ999" {
		t.Errorf("blocked_plates no se leyó: %+v", wl)
	}
}

func TestElLatidoReportaCapacidades(t *testing.T) {
	c := nuevoCloudFalso(t)
	cliente := NewCloudClient(c.srv.URL, "ppk_prueba")
	caps := Capacidades{Whitelist: true, Blocklist: false}
	if _, err := cliente.Heartbeat(context.Background(), HeartbeatReport{Capacidades: &caps}); err != nil {
		t.Fatal(err)
	}
	if c.ultimo["whitelist_supported"] != true || c.ultimo["blocklist_supported"] != false {
		t.Errorf("capacidades no llegaron: %v", c.ultimo)
	}
}

func TestEl429EsTransitorioYEl400Permanente(t *testing.T) {
	codigo := 429
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(codigo)
	}))
	defer srv.Close()
	c := NewCloudClient(srv.URL, "ppk_x")
	ev := &QueuedEvent{ID: "e", Plate: "AAA111"}

	st, _, _ := c.PostEventMultipart(context.Background(), ev, []byte("jpg"), nil)
	if st != PostEventTransient {
		t.Errorf("429 debe ser transitorio, fue %v", st)
	}
	codigo = 400
	st, _, _ = c.PostEventMultipart(context.Background(), ev, []byte("jpg"), nil)
	if st != PostEventPermanent {
		t.Errorf("400 debe ser permanente, fue %v", st)
	}
	codigo = 503
	st, _, _ = c.PostEventMultipart(context.Background(), ev, []byte("jpg"), nil)
	if st != PostEventTransient {
		t.Errorf("503 debe ser transitorio, fue %v", st)
	}
}
