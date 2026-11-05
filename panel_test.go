package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// Tests del panel local: solo le contesta a este PC, guarda sin perder el
// resto del config y nunca muestra la contraseña.

const configDePrueba = `# instalado por install.ps1
cloud:
  base_url: "https://demo.porteriaplus.com"
  token: "ppk_prueba"
  device_token: "ppd_prueba"
camera:
  type: "hikvision"
  family: "hikvision_dsk1t_qr_camara"
  host: "192.168.1.50"
  port: 80
  user: "admin"
  password: "secreta-vieja"
receiver:
  bind_address: "0.0.0.0:8787"
`

func panelDePrueba(t *testing.T) (*PanelLocal, *Receiver, string, *int32) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(configDePrueba), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := loadConfig(path)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	cfg.Receiver.QueueDir = filepath.Join(dir, "queue")
	q, err := NewFileQueue(cfg.Receiver.QueueDir, 10, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var recargas int32
	estado := NuevoEstado(cfg, "PorteriaSyncAgent-9", dir)
	panel := NuevoPanelLocal(cfg, path, "PorteriaSyncAgent-9", estado, func() { atomic.AddInt32(&recargas, 1) })
	r := NewReceiver(cfg, q).ConPanel(panel, estado)
	return panel, r, path, &recargas
}

func pedir(r *Receiver, metodo, ruta, remoto, host string, form url.Values, origen string) *httptest.ResponseRecorder {
	var req *http.Request
	if form != nil {
		req = httptest.NewRequest(metodo, ruta, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(metodo, ruta, nil)
	}
	req.RemoteAddr = remoto
	req.Host = host
	if origen != "" {
		req.Header.Set("Origin", origen)
	}
	w := httptest.NewRecorder()
	r.mux.ServeHTTP(w, req)
	return w
}

func TestElPanelSoloLeContestaAEstePC(t *testing.T) {
	_, r, _, _ := panelDePrueba(t)

	if w := pedir(r, "GET", "/", "127.0.0.1:5000", "localhost:8787", nil, ""); w.Code != 200 {
		t.Fatalf("desde este PC: %d", w.Code)
	}
	// Otro PC de la red de la portería.
	if w := pedir(r, "GET", "/", "192.168.1.20:5000", "192.168.1.10:8787", nil, ""); w.Code != 404 {
		t.Fatalf("desde la LAN debía ser 404, fue %d", w.Code)
	}
	// Una página de internet que apunta su dominio a 127.0.0.1.
	if w := pedir(r, "GET", "/", "127.0.0.1:5000", "malo.example:8787", nil, ""); w.Code != 404 {
		t.Fatalf("con otro Host debía ser 404, fue %d", w.Code)
	}
}

func TestElPanelNoMuestraLaContraseña(t *testing.T) {
	_, r, _, _ := panelDePrueba(t)
	w := pedir(r, "GET", "/", "127.0.0.1:5000", "localhost:8787", nil, "")
	cuerpo := w.Body.String()
	if strings.Contains(cuerpo, "secreta-vieja") {
		t.Fatal("la página muestra la contraseña guardada")
	}
	mustContain(t, cuerpo, "192.168.1.50")
	mustContain(t, cuerpo, "Terminal de la puerta (QR)")
	if w.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatal("falta X-Frame-Options")
	}
}

func TestGuardarExigeLaFichaYElOrigen(t *testing.T) {
	panel, r, path, recargas := panelDePrueba(t)
	form := url.Values{"ip": {"192.168.1.60"}, "puerto": {"80"}, "usuario": {"admin"}, "clave": {""}}

	form.Set("ficha", panel.ficha)
	if w := pedir(r, "POST", "/guardar", "127.0.0.1:5000", "localhost:8787", form, ""); w.Code != 403 {
		t.Fatalf("sin Origin debía ser 403, fue %d", w.Code)
	}
	if w := pedir(r, "POST", "/guardar", "127.0.0.1:5000", "localhost:8787", form, "http://malo.example"); w.Code != 403 {
		t.Fatalf("con Origin ajeno debía ser 403, fue %d", w.Code)
	}
	form.Set("ficha", "otra")
	if w := pedir(r, "POST", "/guardar", "127.0.0.1:5000", "localhost:8787", form, "http://localhost:8787"); w.Code != 403 {
		t.Fatalf("con ficha equivocada debía ser 403, fue %d", w.Code)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "192.168.1.50") {
		t.Fatal("un intento rechazado cambió el config")
	}
	if atomic.LoadInt32(recargas) != 0 {
		t.Fatal("un intento rechazado reinició el agente")
	}
}

func TestGuardarCambiaElEquipoYDejaLoDemas(t *testing.T) {
	panel, r, path, _ := panelDePrueba(t)
	form := url.Values{
		"ficha": {panel.ficha}, "ip": {"192.168.1.60"}, "puerto": {"8000"},
		"usuario": {"tecnico"}, "clave": {""},
	}
	w := pedir(r, "POST", "/guardar", "127.0.0.1:5000", "localhost:8787", form, "http://localhost:8787")
	if w.Code != 200 {
		t.Fatalf("guardar: %d %s", w.Code, w.Body.String())
	}
	cfg, _, err := loadConfig(path)
	if err != nil {
		t.Fatalf("el config guardado no carga: %v", err)
	}
	if cfg.Camera.Host != "192.168.1.60" || cfg.Camera.Port != 8000 || cfg.Camera.User != "tecnico" {
		t.Fatalf("no se guardó: %+v", cfg.Camera)
	}
	if cfg.Camera.Password != "secreta-vieja" {
		t.Fatal("la contraseña vacía debía dejar la que estaba")
	}
	if cfg.Cloud.DeviceToken != "ppd_prueba" || cfg.Camera.Family != "hikvision_dsk1t_qr_camara" {
		t.Fatal("se perdió lo que el panel no toca")
	}
	data, _ := os.ReadFile(path)
	mustContain(t, string(data), "# instalado por install.ps1")
	if _, err := os.Stat(path + ".bak-panel"); err != nil {
		t.Fatal("no dejó copia del config anterior")
	}
}

func TestGuardarRechazaDatosMalos(t *testing.T) {
	panel, r, path, recargas := panelDePrueba(t)
	for _, malo := range []url.Values{
		{"ip": {"192.168.1.60; rm"}, "puerto": {"80"}, "usuario": {"admin"}},
		{"ip": {"192.168.1.60"}, "puerto": {"0"}, "usuario": {"admin"}},
		{"ip": {"192.168.1.60"}, "puerto": {"80"}, "usuario": {""}},
		{"ip": {"192.168.1.60"}, "puerto": {"80"}, "usuario": {"admin"}, "clave": {"a\nb"}},
	} {
		malo.Set("ficha", panel.ficha)
		w := pedir(r, "POST", "/guardar", "127.0.0.1:5000", "localhost:8787", malo, "http://localhost:8787")
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%v: debía ser 422, fue %d", malo, w.Code)
		}
	}
	data, _ := os.ReadFile(path)
	if string(data) != configDePrueba || atomic.LoadInt32(recargas) != 0 {
		t.Fatal("datos malos tocaron el config o reiniciaron el agente")
	}
}

func TestElPanelMuestraLasOtrasCopias(t *testing.T) {
	raiz := t.TempDir()
	segunda := filepath.Join(raiz, "equipo-2")
	if err := os.MkdirAll(segunda, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{}
	cfg.Camera.Type = "hikvision"
	cfg.Camera.Family = "hikvision_itc"
	cfg.Camera.Host = "192.168.1.40"
	cfg.Receiver.BindAddress = "0.0.0.0:8787"
	NuevoEstado(cfg, ServiceName, raiz).Nube(nil)

	otras := otrasCopias(segunda)
	if len(otras) != 1 || otras[0].IP != "192.168.1.40" || otras[0].Puerto != "8787" {
		t.Fatalf("desde equipo-2 debía ver la de la raíz: %+v", otras)
	}
	if len(otrasCopias(raiz)) != 0 {
		t.Fatal("la raíz no debía verse a sí misma")
	}
}

func TestLaReglaDelFirewallCoincideConElInstalador(t *testing.T) {
	if reglaDeFirewall(ServiceName) != "Porteria Sync Agent - receptor LPR" {
		t.Fatal("la primera copia cambió de regla: las instalaciones existentes la perderían")
	}
	if reglaDeFirewall("PorteriaSyncAgent-2") != "Porteria Sync Agent - receptor LPR (PorteriaSyncAgent-2)" {
		t.Fatal("la regla de la segunda copia no coincide con install.ps1")
	}
}

func TestGuardarConservaLaSangria(t *testing.T) {
	_, _, path, _ := panelDePrueba(t)
	if err := actualizarEquipo(path, CambiosDelEquipo{Host: "192.168.1.70", Port: 80, User: "admin"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	mustContain(t, string(data), "\n  host: \"192.168.1.70\"")
}
