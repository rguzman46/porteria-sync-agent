package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Que el panel del conjunto se entere de cómo le está yendo al agent.
//
// Estar vivo y tener la cámara al día NO son lo mismo. El agent puede estar
// corriendo, con internet, y bajando el whitelist sin un solo error, y aun así
// no poder escribirlo en la cámara: le cambiaron la clave, está apagada, el
// firmware no responde. En ese estado el panel se veía completamente verde
// mientras la lista de la cámara llevaba semanas congelada — los residentes
// nuevos no entran, los pases revocados siguen abriendo — y nadie se enteraba
// hasta que alguien reclamaba en la portería.
//
// Estos tests fijan que ese estado sí viaja.

// cloudFalso captura el último cuerpo de heartbeat que recibió.
type cloudFalso struct {
	srv     *httptest.Server
	ultimo  map[string]any
	llamado bool
}

func nuevoCloudFalso(t *testing.T) *cloudFalso {
	t.Helper()
	c := &cloudFalso{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/heartbeat") {
			http.NotFound(w, r)
			return
		}
		c.llamado = true
		c.ultimo = map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&c.ultimo); err != nil {
			t.Errorf("cuerpo de heartbeat ilegible: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"server_time":"2026-09-08T12:00:00Z","whitelist_version":"v1"}`))
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func TestHeartbeatNoReportaPushAntesDeIntentarlo(t *testing.T) {
	// Un agent recién instalado no ha fallado, pero tampoco ha tenido éxito.
	// Mandar `false` haría sonar la alarma de «la cámara no recibe las placas»
	// por el solo hecho de acabar de arrancar.
	c := nuevoCloudFalso(t)
	cliente := NewCloudClient(c.srv.URL, "ppk_prueba")

	if _, err := cliente.Heartbeat(context.Background(), HeartbeatReport{}); err != nil {
		t.Fatalf("heartbeat falló: %v", err)
	}
	if _, hay := c.ultimo["camera_push_ok"]; hay {
		t.Errorf("reportó el push sin haberlo intentado: %v", c.ultimo)
	}
	// La versión y el sistema sí van desde el primer latido: es lo primero que
	// se pregunta cuando una portería falla.
	if c.ultimo["agent_version"] == nil || c.ultimo["system_info"] == nil {
		t.Errorf("falta el diagnóstico básico: %v", c.ultimo)
	}
}

func TestHeartbeatReportaQueLaCamaraRechazo(t *testing.T) {
	c := nuevoCloudFalso(t)
	cliente := NewCloudClient(c.srv.URL, "ppk_prueba")

	fallo := false
	reporte := HeartbeatReport{
		CameraPushOK:    &fallo,
		CameraPushError: "401 Unauthorized — Digest rechazado",
	}
	if _, err := cliente.Heartbeat(context.Background(), reporte); err != nil {
		t.Fatalf("heartbeat falló: %v", err)
	}
	if c.ultimo["camera_push_ok"] != false {
		t.Errorf("no reportó el fallo: %v", c.ultimo)
	}
	if !strings.Contains(c.ultimo["camera_push_error"].(string), "401") {
		t.Errorf("el motivo no llegó: %v", c.ultimo)
	}
}

func TestHeartbeatReportaLaColaRepresada(t *testing.T) {
	c := nuevoCloudFalso(t)
	cliente := NewCloudClient(c.srv.URL, "ppk_prueba")

	if _, err := cliente.Heartbeat(context.Background(), HeartbeatReport{QueueSize: 137}); err != nil {
		t.Fatalf("heartbeat falló: %v", err)
	}
	if c.ultimo["queue_size"] != float64(137) {
		t.Errorf("la cola no llegó: %v", c.ultimo)
	}
}

func TestElMotivoDelFalloSeAcota(t *testing.T) {
	// El cloud lo guarda en una columna de 500. Un error de driver puede traer
	// un volcado entero, y mandarlo completo es que el cloud lo rechace o lo
	// trunque a la brava en medio de un carácter.
	c := nuevoCloudFalso(t)
	cliente := NewCloudClient(c.srv.URL, "ppk_prueba")

	fallo := false
	largo := strings.Repeat("é", 900) // acentos: cortar por bytes puede partirlos
	reporte := HeartbeatReport{CameraPushOK: &fallo, CameraPushError: largo}
	if _, err := cliente.Heartbeat(context.Background(), reporte); err != nil {
		t.Fatalf("heartbeat falló: %v", err)
	}
	recortado, _ := c.ultimo["camera_push_error"].(string)
	if len(recortado) > 500 {
		t.Errorf("no se acotó: %d bytes", len(recortado))
	}
	if !strings.HasSuffix(recortado, "é") {
		t.Errorf("se partió un carácter a la mitad: %q", recortado[len(recortado)-3:])
	}
}

// --------------------------------------------------------------------------
// El Syncer: que lo que pasó en el ciclo llegue al siguiente latido
// --------------------------------------------------------------------------

type colaDePrueba struct{ items int }

func (c colaDePrueba) Stats() (int, int64, time.Duration) {
	return c.items, 0, 0
}

func TestElSyncerRecuerdaComoLeFueElPush(t *testing.T) {
	s := &Syncer{}

	// Falló: se anota el motivo.
	s.anotarPush(errors.New("connection refused"), 0)
	r := s.reporte()
	if r.CameraPushOK == nil || *r.CameraPushOK {
		t.Fatalf("no anotó el fallo: %+v", r)
	}
	if r.CameraPushError != "connection refused" {
		t.Errorf("perdió el motivo: %q", r.CameraPushError)
	}

	// Se arregló: la alarma tiene que bajar sola, no quedarse encendida.
	s.anotarPush(nil, 412)
	r = s.reporte()
	if r.CameraPushOK == nil || !*r.CameraPushOK {
		t.Fatalf("no anotó el éxito: %+v", r)
	}
	if r.CameraPushError != "" {
		t.Errorf("se quedó con el motivo viejo: %q", r.CameraPushError)
	}
	if r.PlatesPushed != 412 {
		t.Errorf("no contó las placas: %d", r.PlatesPushed)
	}
}

func TestElSyncerReportaCeroSinReceiver(t *testing.T) {
	// El receiver es opcional. Con él apagado no hay nada encolándose, y cero
	// es la respuesta correcta — no «no sé».
	s := &Syncer{}
	if got := s.reporte().QueueSize; got != 0 {
		t.Errorf("sin cola debería reportar 0, reportó %d", got)
	}

	s = (&Syncer{}).ConCola(colaDePrueba{items: 42})
	if got := s.reporte().QueueSize; got != 42 {
		t.Errorf("con cola debería reportar 42, reportó %d", got)
	}
}
