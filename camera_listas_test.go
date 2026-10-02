package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// La lista negra y las capacidades de cada adaptador.

func TestHikvisionTrafficEscribeLaListaNegraConPlateType1(t *testing.T) {
	xml := buildHikvisionTrafficPlatesXML([]Plate{{Plate: "AAA111"}}, []Plate{{Plate: "ZZZ999"}})
	mustContain(t, xml, `<plateNumber>AAA111</plateNumber><plateType>0</plateType>`)
	mustContain(t, xml, `<plateNumber>ZZZ999</plateNumber><plateType>1</plateType>`)
}

func TestHikvisionITCEscribeLaListaNegraConPlateType1(t *testing.T) {
	xml := buildHikvisionITCVehicleListXML([]Plate{{Plate: "AAA111"}}, []Plate{{Plate: "ZZZ999"}})
	mustContain(t, xml, `<plateNumber>AAA111</plateNumber><plateType>0</plateType>`)
	mustContain(t, xml, `<plateNumber>ZZZ999</plateNumber><plateType>1</plateType>`)
}

func TestHikvisionAuditEscribeAmbasListasEnUnArchivo(t *testing.T) {
	xml := buildHikvisionAuditXML([]Plate{{Plate: "AAA111", ValidUntil: "2026-12-31T23:59:59Z"}}, []Plate{{Plate: "ZZZ999"}})
	mustContain(t, xml, `<LicensePlate>AAA111</LicensePlate><listType>whiteList</listType>`)
	mustContain(t, xml, `<effectiveTime>2026-12-31</effectiveTime>`)
	mustContain(t, xml, `<LicensePlate>ZZZ999</LicensePlate><listType>blackList</listType>`)
}

func TestHikvisionAuditApuntaAlEndpointDeAuditoria(t *testing.T) {
	var gotPath, gotQuery, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotMethod = r.URL.Path, r.URL.RawQuery, r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	host, port := splitHostPort(t, srv.URL)
	a := NewHikvisionAuditAdapter(host, port, "admin", "x")
	if err := a.SyncWhitelist(testContext(), []Plate{{Plate: "AAA111"}}, nil); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPut || gotPath != "/ISAPI/Traffic/channels/1/licensePlateAuditData" || gotQuery != "fileType=xml" {
		t.Errorf("%s %s?%s", gotMethod, gotPath, gotQuery)
	}
}

func TestDahuaNoPrometeListasQueNoEscribe(t *testing.T) {
	a := NewDahuaITCAdapter("1.2.3.4", 80, "admin", "x")
	caps := a.Capacidades()
	if caps.Whitelist || caps.Blocklist {
		t.Errorf("dahua_itc no tiene endpoint de lista confirmado: %+v", caps)
	}
}

func TestLasCapacidadesViajanEnElLatido(t *testing.T) {
	cfg := newCfg("dahua", "")
	cam, _ := NewCameraAdapter(cfg)
	s := &Syncer{cfg: cfg, camera: cam}
	r := s.reporte()
	if r.Capacidades == nil || r.Capacidades.Whitelist {
		t.Errorf("reporte: %+v", r.Capacidades)
	}
}

func TestAxisUsaElAPIDelACAPYRetiraLoQueYaNoEsta(t *testing.T) {
	var llamadas []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		llamadas = append(llamadas, r.URL.Path+"?"+r.URL.RawQuery)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	}))
	defer srv.Close()
	host, port := splitHostPort(t, srv.URL)
	a := NewAxisVapixAdapter(host, port, "admin", "x")

	if err := a.SyncWhitelist(testContext(), []Plate{{Plate: "AAA111", Owner: "Ana, Torre 2"}}, []Plate{{Plate: "ZZZ999"}}); err != nil {
		t.Fatal(err)
	}
	mustContain(t, strings.Join(llamadas, "\n"), "/local/fflprapp/api.cgi?api=addplate&list=allowlist&plate=AAA111%2C%2CAna++Torre+2")
	mustContain(t, strings.Join(llamadas, "\n"), "/local/fflprapp/api.cgi?api=addplate&list=blocklist&plate=ZZZ999%2C%2C")

	// Segunda sincronización sin AAA111: hay que retirarla.
	llamadas = nil
	if err := a.SyncWhitelist(testContext(), nil, []Plate{{Plate: "ZZZ999"}}); err != nil {
		t.Fatal(err)
	}
	mustContain(t, strings.Join(llamadas, "\n"), "api=delplate&list=allowlist&plate=AAA111")
}

func TestAxisRecuerdaLoQueEmpujoTrasUnReinicio(t *testing.T) {
	var llamadas []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		llamadas = append(llamadas, r.URL.RawQuery)
		_, _ = w.Write([]byte("OK"))
	}))
	defer srv.Close()
	host, port := splitHostPort(t, srv.URL)
	estado := filepath.Join(t.TempDir(), "estado", "axis.json")

	antes := NewAxisVapixAdapter(host, port, "admin", "x").conEstado(estado)
	if err := antes.SyncWhitelist(testContext(), []Plate{{Plate: "AAA111"}}, nil); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(estado); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("estado no guardado con 0600: %v", err)
	}

	// El agente se reinicia y la placa ya no está en la plataforma.
	llamadas = nil
	despues := NewAxisVapixAdapter(host, port, "admin", "x").conEstado(estado)
	if err := despues.SyncWhitelist(testContext(), nil, nil); err != nil {
		t.Fatal(err)
	}
	mustContain(t, strings.Join(llamadas, "\n"), "api=delplate&list=allowlist&plate=AAA111")
}

func TestLaColaGuardaConPermisosRestringidos(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "queue")
	q, err := NewFileQueue(dir, 10, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	ev := &QueuedEvent{Plate: "AAA111"}
	_ = q.EnqueueConRecorte(ev, []byte("x"), []byte("y"))

	info, _ := os.Stat(dir)
	if info.Mode().Perm() != 0o700 {
		t.Errorf("directorio con %o, esperaba 0700", info.Mode().Perm())
	}
	for _, ext := range []string{".json", ".bin", ".crop"} {
		info, err := os.Stat(filepath.Join(dir, ev.ID+ext))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s con %o, esperaba 0600", ext, info.Mode().Perm())
		}
	}
	_ = q.Delete(ev.ID)
	if _, err := os.Stat(filepath.Join(dir, ev.ID+".crop")); !os.IsNotExist(err) {
		t.Error("Delete debe borrar también el recorte")
	}
}
