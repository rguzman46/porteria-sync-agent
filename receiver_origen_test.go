package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
)

// Quién puede postear al receptor, y qué se le responde a cada marca.

func TestUnPostQueNoVieneDeLaCamaraRecibe403(t *testing.T) {
	r, q, _ := newTestReceiver(t)
	body, ct := buildGenericMultipart(t, `{"plate":"ABC123"}`, []byte("x"), "image/jpeg")
	req := httptest.NewRequest(http.MethodPost, "/lpr-event", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Agent-Source", "generic")
	req.RemoteAddr = "192.168.1.77:4444" // otro PC de la LAN
	w := httptest.NewRecorder()

	r.server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d, esperaba 403", w.Code)
	}
	if items, _, _ := q.Stats(); items != 0 {
		t.Error("una lectura de un origen no permitido no puede encolarse")
	}
}

func TestLaCamaraYAllowFromSiPasan(t *testing.T) {
	r, _, _ := newTestReceiver(t)
	r.cfg.Receiver.AllowFrom = []string{"10.0.0.0/8"}
	r.origenes = origenesPermitidos(r.cfg)

	for _, ip := range []string{"192.0.2.1:1", "10.4.5.6:2"} {
		body, ct := buildGenericMultipart(t, `{"plate":"ABC123"}`, []byte("x"), "image/jpeg")
		req := httptest.NewRequest(http.MethodPost, "/lpr-event", body)
		req.Header.Set("Content-Type", ct)
		req.Header.Set("X-Agent-Source", "generic")
		req.RemoteAddr = ip
		w := httptest.NewRecorder()
		r.server.Handler.ServeHTTP(w, req)
		if w.Code != http.StatusAccepted {
			t.Errorf("desde %s: status=%d body=%s, esperaba 202", ip, w.Code, w.Body.String())
		}
	}
}

func TestAllowAnyApagaElFiltro(t *testing.T) {
	r, _, _ := newTestReceiver(t)
	r.allowAny = true
	body, ct := buildGenericMultipart(t, `{"plate":"ABC123"}`, []byte("x"), "image/jpeg")
	req := httptest.NewRequest(http.MethodPost, "/lpr-event", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Agent-Source", "generic")
	req.RemoteAddr = "192.168.1.77:4444"
	w := httptest.NewRecorder()
	r.server.Handler.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d, esperaba 202 con allow_any", w.Code)
	}
}

func TestHealthQuedaAbierto(t *testing.T) {
	r, _, _ := newTestReceiver(t)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.RemoteAddr = "192.168.1.77:4444"
	w := httptest.NewRecorder()
	r.server.Handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, /health no escribe nada y debe seguir abierto", w.Code)
	}
}

// --------------------------------------------------------------------------
// Hikvision: partes con nombre, plateRect y respuesta 200 vacía
// --------------------------------------------------------------------------

func multipartHikvision(t *testing.T, xml string, imagenes map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="anpr.xml"; filename="anpr.xml"`)
	h.Set("Content-Type", "application/xml")
	p, _ := mw.CreatePart(h)
	_, _ = io.Copy(p, strings.NewReader(xml))
	for nombre, contenido := range imagenes {
		hi := textproto.MIMEHeader{}
		hi.Set("Content-Disposition", `form-data; name="`+nombre+`"; filename="`+nombre+`"`)
		hi.Set("Content-Type", "image/jpeg")
		pi, _ := mw.CreatePart(hi)
		_, _ = pi.Write([]byte(contenido))
	}
	_ = mw.Close()
	return buf, mw.FormDataContentType()
}

const xmlHikvisionCompleto = `<EventNotificationAlert>
  <dateTime>2026-05-13T14:32:15-05:00</dateTime>
  <eventType>ANPR</eventType>
  <ANPR>
    <licensePlate>abc123</licensePlate>
    <direction>reverse</direction>
    <confidenceLevel>93</confidenceLevel>
    <laneNo>2</laneNo>
    <plateColor>yellow</plateColor>
    <pictureInfoList>
      <pictureInfo>
        <fileName>detectionPicture.jpg</fileName>
        <type>detectionPicture</type>
        <plateRect><X>100</X><Y>200</Y><width>300</width><height>80</height></plateRect>
      </pictureInfo>
      <pictureInfo>
        <fileName>licensePlatePicture.jpg</fileName>
        <type>licensePlatePicture</type>
      </pictureInfo>
    </pictureInfoList>
  </ANPR>
</EventNotificationAlert>`

func TestHikvisionIdentificaEscenaYRecortePorNombreNoPorOrden(t *testing.T) {
	// El recorte va PRIMERO a propósito: el orden de las partes no es contrato.
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	for _, parte := range []struct{ nombre, contenido, ct string }{
		{"licensePlatePicture.jpg", "recorte-bytes", "image/jpeg"},
		{"anpr.xml", xmlHikvisionCompleto, "application/xml"},
		{"detectionPicture.jpg", "escena-bytes", "image/jpeg"},
	} {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="`+parte.nombre+`"; filename="`+parte.nombre+`"`)
		h.Set("Content-Type", parte.ct)
		p, _ := mw.CreatePart(h)
		_, _ = p.Write([]byte(parte.contenido))
	}
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/lpr-event", buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	ev, escena, recorte, err := parseHikvisionMultipart(req)
	if err != nil {
		t.Fatal(err)
	}
	if string(escena) != "escena-bytes" || string(recorte) != "recorte-bytes" {
		t.Errorf("escena=%q recorte=%q", escena, recorte)
	}
	if ev.Plate != "ABC123" || ev.Direction != "exit" {
		t.Errorf("placa=%q sentido=%q", ev.Plate, ev.Direction)
	}
	if ev.Confidence == nil || *ev.Confidence != 93 {
		t.Errorf("confianza=%v", ev.Confidence)
	}
	if ev.PlateBox == nil || *ev.PlateBox != (PlateBox{XMin: 100, YMin: 200, XMax: 400, YMax: 280}) {
		t.Errorf("plate_box=%+v, esperaba el plateRect de la escena en píxeles", ev.PlateBox)
	}
	if ev.Metadata["lane"] != "2" || ev.Metadata["plate_color"] != "yellow" {
		t.Errorf("metadata: %v", ev.Metadata)
	}
}

func TestHikvisionConDosImagenesSinNombreNoAdivinaElRecorte(t *testing.T) {
	xml := `<EventNotificationAlert><eventType>ANPR</eventType><ANPR><licensePlate>XYZ789</licensePlate></ANPR></EventNotificationAlert>`
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="alert"`)
	h.Set("Content-Type", "application/xml")
	p, _ := mw.CreatePart(h)
	_, _ = io.Copy(p, strings.NewReader(xml))
	for _, contenido := range []string{"primera", "segunda"} {
		hi := textproto.MIMEHeader{}
		hi.Set("Content-Disposition", `form-data; name="img"`)
		hi.Set("Content-Type", "image/jpeg")
		pi, _ := mw.CreatePart(hi)
		_, _ = pi.Write([]byte(contenido))
	}
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/lpr-event", buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	ev, escena, recorte, err := parseHikvisionMultipart(req)
	if err != nil {
		t.Fatal(err)
	}
	if string(escena) != "primera" {
		t.Errorf("escena=%q, esperaba la primera", escena)
	}
	if recorte != nil {
		t.Errorf("recorte=%q: sin nombres no se adivina", recorte)
	}
	if ev.PlateBox != nil {
		t.Errorf("sin plateRect no hay plate_box: %+v", ev.PlateBox)
	}
}

func TestHikvisionRecibe200ConCuerpoVacio(t *testing.T) {
	r, q, _ := newTestReceiver(t)
	body, ct := multipartHikvision(t, xmlHikvisionCompleto, map[string]string{
		"detectionPicture.jpg":    "escena",
		"licensePlatePicture.jpg": "recorte",
	})
	req := httptest.NewRequest(http.MethodPost, "/lpr-event", body)
	req.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	r.server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK || w.Body.Len() != 0 || w.Header().Get("Content-Length") != "0" {
		t.Fatalf("Hikvision espera 200 vacío; recibió %d %q (Content-Length=%q)", w.Code, w.Body.String(), w.Header().Get("Content-Length"))
	}
	got, _ := q.PeekOldest(1)
	if len(got) != 1 || got[0].RecorteBytes != len("recorte") {
		t.Errorf("el recorte no quedó en la cola: %+v", got)
	}
	if string(q.Recorte(got[0].ID)) != "recorte" {
		t.Errorf("recorte en disco: %q", q.Recorte(got[0].ID))
	}
}

func TestHikvisionUnEventoQueNoEsANPRSeContestaConExito(t *testing.T) {
	r, q, _ := newTestReceiver(t)
	body, ct := multipartHikvision(t, `<EventNotificationAlert><eventType>VMD</eventType></EventNotificationAlert>`, nil)
	req := httptest.NewRequest(http.MethodPost, "/lpr-event", body)
	req.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	r.server.Handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d: un evento que no es ANPR se acepta para que la cámara no insista", w.Code)
	}
	if items, _, _ := q.Stats(); items != 0 {
		t.Error("no debía encolar nada")
	}
}

// --------------------------------------------------------------------------
// Dahua ITSAPI
// --------------------------------------------------------------------------

func cuerpoDahua(placa, picName, hora string) []byte {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	cuerpo := map[string]any{
		"PicName": picName,
		"Picture": map[string]any{
			"Plate": map[string]any{
				"PlateNumber": placa,
				"Confidence":  87,
				"BoundingBox": []int{1000, 2000, 3000, 2500},
			},
			"CutoutPic": map[string]any{"Content": b64("recorte-dahua"), "PicName": picName + "_cut"},
			"NormalPic": map[string]any{"Content": b64("escena-dahua"), "PicName": picName},
			"SnapInfo":  map[string]any{"AccurateTime": hora, "Direction": "Approach", "Lane": 1},
		},
	}
	raw, _ := json.Marshal(cuerpo)
	return raw
}

func TestDahuaRutaNativaRespondeResultTrueYDeduplica(t *testing.T) {
	r, q, _ := newTestReceiver(t)

	var ids []string
	for i := 0; i < 2; i++ { // la cámara reenvía el mismo evento
		req := httptest.NewRequest(http.MethodPost, "/NotificationInfo/TollgateInfo", bytes.NewReader(cuerpoDahua("abc123", "20260513143215_1.jpg", "2026-05-13 14:32:15")))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.server.Handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"Result": true}` {
			t.Fatalf("Dahua espera {\"Result\": true}; recibió %d %q", w.Code, w.Body.String())
		}
		got, _ := q.PeekOldest(10)
		ids = append(ids, got[len(got)-1].ClientEventID)
	}
	if ids[0] != ids[1] || !strings.HasPrefix(ids[0], "dahua-") {
		t.Errorf("el reenvío debe llevar el mismo client_event_id: %v", ids)
	}

	got, _ := q.PeekOldest(1)
	ev := got[0]
	if ev.Plate != "ABC123" || ev.Direction != "entry" || ev.Timestamp != "2026-05-13T14:32:15" {
		t.Errorf("evento: %+v", ev)
	}
	if ev.Confidence == nil || *ev.Confidence != 87 {
		t.Errorf("confianza=%v", ev.Confidence)
	}
	if ev.PlateBox != nil {
		t.Errorf("BoundingBox de Dahua no va como plate_box (unidades sin confirmar): %+v", ev.PlateBox)
	}
	if _, hay := ev.Metadata["dahua_bounding_box"]; !hay {
		t.Errorf("el BoundingBox crudo debe quedar en metadata: %v", ev.Metadata)
	}
	escena, _ := q.Snapshot(ev.ID)
	if string(escena) != "escena-dahua" || string(q.Recorte(ev.ID)) != "recorte-dahua" {
		t.Errorf("escena=%q recorte=%q", escena, q.Recorte(ev.ID))
	}
}

func TestDahuaKeepAliveRespondeResultTrue(t *testing.T) {
	r, _, _ := newTestReceiver(t)
	req := httptest.NewRequest(http.MethodPost, "/NotificationInfo/KeepAlive", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	r.server.Handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"Result": true`) {
		t.Fatalf("KeepAlive: %d %q", w.Code, w.Body.String())
	}
}

// --------------------------------------------------------------------------
// Axis License Plate Verifier
// --------------------------------------------------------------------------

func TestAxisTomaSoloElNewYJuntaLasImagenesDelCarro(t *testing.T) {
	r, q, _ := newTestReceiver(t)
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	lote := []map[string]any{
		{"plateASCII": "abc123", "carID": 7, "carState": "new", "carMoveDirection": "in",
			"plateConfidence": 0.91, "plateCoordinates": []int{10, 20, 200, 60},
			"imageType": "frame", "imageArray": b64("escena-axis"), "capture_timestamp": 1778000000},
		{"plateASCII": "abc123", "carID": 7, "carState": "update", "imageType": "plate", "imageArray": b64("recorte-axis")},
	}
	raw, _ := json.Marshal(lote)
	req := httptest.NewRequest(http.MethodPost, "/axis-event", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.server.Handler.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	got, _ := q.PeekOldest(1)
	ev := got[0]
	if ev.Plate != "ABC123" || ev.Direction != "entry" {
		t.Errorf("evento: %+v", ev)
	}
	if ev.PlateBox == nil || *ev.PlateBox != (PlateBox{XMin: 10, YMin: 20, XMax: 210, YMax: 80}) {
		t.Errorf("plate_box=%+v", ev.PlateBox)
	}
	if !strings.HasPrefix(ev.ClientEventID, "axis-7-") {
		t.Errorf("client_event_id=%q", ev.ClientEventID)
	}
	if !strings.HasPrefix(ev.Timestamp, "2026-05-0") {
		t.Errorf("timestamp=%q", ev.Timestamp)
	}
	escena, _ := q.Snapshot(ev.ID)
	if string(escena) != "escena-axis" || string(q.Recorte(ev.ID)) != "recorte-axis" {
		t.Errorf("escena=%q recorte=%q", escena, q.Recorte(ev.ID))
	}
}

func TestAxisUnUpdateSoloSeIgnora(t *testing.T) {
	r, q, _ := newTestReceiver(t)
	raw, _ := json.Marshal(map[string]any{"plateASCII": "abc123", "carID": 7, "carState": "update"})
	req := httptest.NewRequest(http.MethodPost, "/axis-event", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.server.Handler.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d", w.Code)
	}
	if items, _, _ := q.Stats(); items != 0 {
		t.Error("un update no produce lectura")
	}
}

// --------------------------------------------------------------------------
// Genérico: recorte y campos nuevos pasan tal cual
// --------------------------------------------------------------------------

func TestGenericoAceptaRecorteConfianzaYRecuadro(t *testing.T) {
	r, q, _ := newTestReceiver(t)
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	_ = mw.WriteField("event_data", `{"plate":"abc123","client_event_id":"mio-1","confidence":0.5,"plate_box":{"xmin":1,"ymin":2,"xmax":3,"ymax":4}}`)
	p, _ := mw.CreateFormFile("snapshot", "s.jpg")
	_, _ = p.Write([]byte("escena"))
	p, _ = mw.CreateFormFile("plate_crop", "c.jpg")
	_, _ = p.Write([]byte("recorte"))
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/lpr-event", buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Agent-Source", "generic")
	w := httptest.NewRecorder()
	r.server.Handler.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	got, _ := q.PeekOldest(1)
	ev := got[0]
	if ev.ClientEventID != "mio-1" || ev.Confidence == nil || *ev.Confidence != 0.5 || ev.PlateBox == nil || ev.PlateBox.YMax != 4 {
		t.Errorf("evento: %+v", ev)
	}
	if string(q.Recorte(ev.ID)) != "recorte" {
		t.Errorf("recorte=%q", q.Recorte(ev.ID))
	}
}
