package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// terminalSimulado guarda personas y tarjetas como un DS-K1T, con un
// residente que el conjunto cargó a mano y que el agente no puede tocar.
type terminalSimulado struct {
	mu       sync.Mutex
	personas map[string]map[string]any
	tarjetas map[string]string // cardNo → employeeNo
	llamadas []string
}

func nuevoTerminal() *terminalSimulado {
	return &terminalSimulado{
		personas: map[string]map[string]any{
			"R-0042": {"employeeNo": "R-0042", "name": "Residente con rostro"},
		},
		tarjetas: map[string]string{},
	}
}

func (t *terminalSimulado) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.llamadas = append(t.llamadas, r.Method+" "+r.URL.Path)
	var cuerpo map[string]any
	_ = json.NewDecoder(r.Body).Decode(&cuerpo)
	switch r.URL.Path {
	case "/ISAPI/System/deviceInfo":
		_, _ = io.WriteString(w, `{"DeviceInfo":{"model":"DS-K1T680DF-E1"}}`)
	case "/ISAPI/AccessControl/UserInfo/Search":
		cond := cuerpo["UserInfoSearchCond"].(map[string]any)
		pos := int(cond["searchResultPosition"].(float64))
		todos := []map[string]any{}
		for _, p := range t.personas {
			todos = append(todos, p)
		}
		// Páginas de dos para obligar a recorrerlas.
		fin := pos + 2
		estado := "MORE"
		if fin >= len(todos) {
			fin, estado = len(todos), "OK"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"UserInfoSearch": map[string]any{
			"responseStatusStrg": estado, "numOfMatches": fin - pos, "UserInfo": todos[pos:fin],
		}})
	case "/ISAPI/AccessControl/UserInfo/Record", "/ISAPI/AccessControl/UserInfo/Modify":
		p := cuerpo["UserInfo"].(map[string]any)
		t.personas[p["employeeNo"].(string)] = p
	case "/ISAPI/AccessControl/CardInfo/Record":
		c := cuerpo["CardInfo"].(map[string]any)
		t.tarjetas[c["cardNo"].(string)] = c["employeeNo"].(string)
	case "/ISAPI/AccessControl/UserInfoDetail/Delete":
		lista := cuerpo["UserInfoDetail"].(map[string]any)["EmployeeNoList"].([]any)
		for _, e := range lista {
			no := e.(map[string]any)["employeeNo"].(string)
			delete(t.personas, no)
			for card, dueño := range t.tarjetas {
				if dueño == no {
					delete(t.tarjetas, card)
				}
			}
		}
	default:
		http.NotFound(w, r)
	}
}

func adaptadorContra(t *testing.T, term *terminalSimulado) *HikvisionDSK1TAdapter {
	t.Helper()
	srv := httptest.NewServer(term)
	t.Cleanup(srv.Close)
	host, puerto, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	p, _ := strconv.Atoi(puerto)
	return NewHikvisionDSK1TAdapter("hikvision_dsk1t_qr_camara", host, p, "admin", "clave")
}

func pase(id, codigo, desde, hasta string) Credential {
	return Credential{Type: "qr", Credential: codigo, Holder: "v:" + id, ValidFrom: desde, ValidUntil: hasta}
}

func TestElTerminalQuedaConLosPasesYSinTocarAlResidente(t *testing.T) {
	term := nuevoTerminal()
	a := adaptadorContra(t, term)
	ctx := context.Background()

	pases := []Credential{
		pase("1", "1000000001", "2026-11-03T13:00:00", "2026-11-03T22:00:00"),
		pase("2", "1000000002", "", ""),
		pase("3", "1000000003", "", ""),
	}
	if err := a.SyncCredentials(ctx, pases); err != nil {
		t.Fatal(err)
	}
	if len(term.personas) != 4 || term.tarjetas["1000000001"] != "ppv1" {
		t.Fatalf("personas=%v tarjetas=%v", term.personas, term.tarjetas)
	}

	// Se revoca el 2 y se acorta el 1: el residente sigue ahí.
	pases = []Credential{pases[0], pases[2]}
	pases[0].ValidUntil = "2026-11-03T18:00:00"
	if err := a.SyncCredentials(ctx, pases); err != nil {
		t.Fatal(err)
	}
	if _, sigue := term.personas["R-0042"]; !sigue {
		t.Fatal("el agente borró a un residente que no es suyo")
	}
	if _, sigue := term.personas["ppv2"]; sigue {
		t.Fatal("el pase revocado sigue en el terminal")
	}
	if _, sigue := term.tarjetas["1000000002"]; sigue {
		t.Fatal("el código del pase revocado sigue abriendo")
	}
	fin := term.personas["ppv1"]["Valid"].(map[string]any)["endTime"]
	if fin != "2026-11-03T13:00:00" {
		t.Fatalf("la vigencia no se corrigió o no está en hora de Bogotá: %v", fin)
	}
}

func TestLaVigenciaVaEnHoraDeBogota(t *testing.T) {
	a := NewHikvisionDSK1TAdapter("hikvision_dsk1t_qr_camara", "127.0.0.1", 80, "", "")
	// 13:00 UTC son las 8:00 en Bogotá; la nube manda UTC sin zona.
	desde, hasta := a.vigencia(pase("9", "1000000009", "2026-11-03T13:00:00.123456", "2026-11-04T04:59:59"))
	if desde != "2026-11-03T08:00:00" || hasta != "2026-11-03T23:59:59" {
		t.Fatalf("desde=%s hasta=%s", desde, hasta)
	}
	desde, hasta = a.vigencia(pase("9", "1000000009", "", ""))
	if desde != "2000-01-01T00:00:00" || hasta != "2037-12-31T23:59:59" {
		t.Fatalf("sin vigencia: desde=%s hasta=%s", desde, hasta)
	}
}

func TestConLaMismaListaNoEscribeNada(t *testing.T) {
	term := nuevoTerminal()
	a := adaptadorContra(t, term)
	pases := []Credential{pase("1", "1000000001", "2026-11-03T13:00:00", "2026-11-03T22:00:00")}
	if err := a.SyncCredentials(context.Background(), pases); err != nil {
		t.Fatal(err)
	}
	antes := len(term.llamadas)
	if err := a.SyncCredentials(context.Background(), pases); err != nil {
		t.Fatal(err)
	}
	for _, l := range term.llamadas[antes:] {
		if !strings.Contains(l, "/Search") {
			t.Fatalf("la segunda sincronización escribió: %s", l)
		}
	}
}

func TestElPaseNoLlevaElNombreDelVisitante(t *testing.T) {
	a := NewHikvisionDSK1TAdapter("hikvision_dsk1t_qr_camara", "127.0.0.1", 80, "", "")
	p := a.persona("ppv7", pase("7", "1234567890", "", ""), "x", "y")
	if p["name"] != "Visitante 7890" {
		t.Fatalf("name=%v", p["name"])
	}
}

// eventoACS arma el multipart que manda el terminal al autenticar.
func eventoACS(t *testing.T, ace map[string]any, foto []byte) *http.Request {
	t.Helper()
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	cuerpo, _ := json.Marshal(map[string]any{
		"dateTime": "2026-11-03T08:15:00-05:00", "eventType": "AccessControllerEvent",
		"AccessControllerEvent": ace,
	})
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="event_log"`)
	h.Set("Content-Type", "application/json")
	parte, _ := mw.CreatePart(h)
	_, _ = parte.Write(cuerpo)
	if foto != nil {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="Picture"; filename="Picture.jpg"`)
		h.Set("Content-Type", "image/jpeg")
		parte, _ := mw.CreatePart(h)
		_, _ = parte.Write(foto)
	}
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/acs-event", buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func TestElEventoDeUnPaseSeConvierteEnLecturaDeCredencial(t *testing.T) {
	ev, foto, err := parseHikvisionAccessEvent(eventoACS(t, map[string]any{
		"majorEventType": 5, "subEventType": 1, "cardNo": "1234567890",
		"employeeNoString": "ppv7", "serialNo": 88,
	}, []byte{0xFF, 0xD8, 0xFF}))
	if err != nil {
		t.Fatal(err)
	}
	if ev.CredentialType != "qr" || ev.Credential != "1234567890" || ev.Plate != "" {
		t.Fatalf("evento=%+v", ev)
	}
	if ev.Opened == nil || !*ev.Opened {
		t.Fatal("una tarjeta válida (sub 1) es que el equipo abrió")
	}
	if len(foto) != 3 || ev.ClientEventID == "" {
		t.Fatalf("foto=%d id=%q", len(foto), ev.ClientEventID)
	}
}

func TestLoDelResidenteNoSaleDelConjunto(t *testing.T) {
	// Un residente con su rostro, y con su llavero de fábrica: ni la foto ni
	// el número tienen por qué subir.
	for _, ace := range []map[string]any{
		{"majorEventType": 5, "subEventType": 75, "employeeNoString": "R-0042"},
		{"majorEventType": 5, "subEventType": 1, "cardNo": "3051234", "employeeNoString": "R-0042"},
	} {
		_, _, err := parseHikvisionAccessEvent(eventoACS(t, ace, []byte{0xFF}))
		if err != errEventoIgnorado {
			t.Fatalf("se habría subido un evento de residente: %v (%v)", ace, err)
		}
	}
}

func TestUnCodigoQueNoExisteSeReportaComoNegado(t *testing.T) {
	ev, _, err := parseHikvisionAccessEvent(eventoACS(t, map[string]any{
		"majorEventType": 5, "subEventType": 9, "cardNo": "4000000000",
	}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Opened == nil || *ev.Opened {
		t.Fatal("tarjeta inexistente (sub 9): el equipo negó")
	}
}

func TestLaLecturaDeCredencialVaASuRutaSinPlaca(t *testing.T) {
	abrio := true
	ev := &QueuedEvent{ID: "1", CredentialType: "qr", Credential: "1234567890", Opened: &abrio}
	cuerpo, tipo, err := buildEventMultipartBody(ev, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = tipo
	crudo, _ := io.ReadAll(cuerpo)
	texto := string(crudo)
	if strings.Contains(texto, `"plate"`) || !strings.Contains(texto, `"credential":"1234567890"`) || !strings.Contains(texto, `"opened":true`) {
		t.Fatalf("cuerpo=%s", texto)
	}
	if strings.Contains(texto, `name="snapshot"`) {
		t.Fatal("sin foto no debe ir una parte snapshot vacía")
	}
}

// El ciclo completo: el cloud manda la lista con pases y el terminal queda con
// ellos. Un lector de credenciales no tiene placas que escribir y aun así su
// push cuenta como exitoso en el latido.
func TestElCicloLeCargaLosPasesAlLector(t *testing.T) {
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			_, _ = io.WriteString(w, `{"ok":true,"server_time":"2026-11-03T13:00:00","whitelist_version":"v1"}`)
		case strings.HasSuffix(r.URL.Path, "/whitelist"):
			w.Header().Set("Last-Modified", "Tue, 03 Nov 2026 13:00:00 GMT")
			_, _ = io.WriteString(w, `{"version":"v1","plates":[],"blocked_plates":[],
				"credentials":[{"type":"qr","credential":"1000000001","holder":"v:1",
				"valid_from":null,"valid_until":"2026-11-03T22:00:00"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer cloud.Close()

	term := nuevoTerminal()
	s := NewSyncer(NewCloudClient(cloud.URL, "ppk_prueba"), &Config{}, adaptadorContra(t, term), 60)
	s.cycle(context.Background())

	if term.tarjetas["1000000001"] != "ppv1" {
		t.Fatalf("el pase no llegó al terminal: %v", term.tarjetas)
	}
	if s.lastPushOK == nil || !*s.lastPushOK || s.lastPlatesCount != 1 {
		t.Fatalf("el latido no dirá que el lector está al día: ok=%v n=%d", s.lastPushOK, s.lastPlatesCount)
	}
}
