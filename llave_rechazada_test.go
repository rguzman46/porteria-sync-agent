package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Cuando la plataforma deja de reconocer la llave de un equipo (lo borraron
// en Cámaras de placas o le generaron un token nuevo), el agente lo dice tal
// cual y da el comando para quitar solo esa copia. Nació de un PC con dos
// copias: la vieja daba 401 y el técnico no sabía qué hacer con ella.

func nubeQueContesta(t *testing.T, status int) *CloudClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, `{"error":"no_autenticado","message":"API key inválida o revocada."}`)
	}))
	t.Cleanup(srv.Close)
	return NewCloudClient(srv.URL, "ppk_prueba")
}

func TestEl401EsLlaveRechazada(t *testing.T) {
	c := nubeQueContesta(t, http.StatusUnauthorized)
	if _, err := c.Heartbeat(context.Background(), HeartbeatReport{}); !errors.Is(err, ErrLlaveRechazada) {
		t.Fatalf("heartbeat con 401: %v", err)
	}
	if _, _, err := c.FetchWhitelist(context.Background()); !errors.Is(err, ErrLlaveRechazada) {
		t.Fatalf("lista con 401: %v", err)
	}
	// Un 500 es la plataforma caída, no la llave: no se le dice al técnico
	// que quite el agente.
	c = nubeQueContesta(t, http.StatusInternalServerError)
	if _, err := c.Heartbeat(context.Background(), HeartbeatReport{}); err == nil || errors.Is(err, ErrLlaveRechazada) {
		t.Fatalf("heartbeat con 500: %v", err)
	}
}

func TestElEstadoAnotaLaLlaveRechazadaYLaOlvida(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{}
	cfg.Receiver.BindAddress = "0.0.0.0:8788"
	e := NuevoEstado(cfg, "PorteriaSyncAgent-2", dir)

	e.Nube(fmt.Errorf("heartbeat: %w", ErrLlaveRechazada))
	var guardado DatosDelEquipo
	data, _ := os.ReadFile(filepath.Join(dir, "estado.json"))
	if json.Unmarshal(data, &guardado) != nil || !guardado.LlaveRechazada {
		t.Fatalf("estado.json debía decir llave_rechazada: %s", data)
	}
	// Con la llave buena otra vez (token nuevo puesto a mano), se olvida.
	e.Nube(nil)
	if e.Copia().LlaveRechazada {
		t.Fatal("con la nube bien no debía seguir marcada")
	}
	// Un corte de internet no es llave rechazada.
	e.Nube(errors.New("dial tcp: no such host"))
	if e.Copia().LlaveRechazada {
		t.Fatal("un error de red no es una llave rechazada")
	}
}

func TestElPanelDiceComoQuitarLaCopiaHuerfana(t *testing.T) {
	panel, r, path, _ := panelDePrueba(t)
	panel.estado.Nube(ErrLlaveRechazada)

	w := pedir(r, "GET", "/", "127.0.0.1:5000", "localhost:8787", nil, "")
	cuerpo := w.Body.String()
	esperado := `-File &#34;` + filepath.Join(filepath.Dir(path), "uninstall.ps1") + `&#34; -Servicio PorteriaSyncAgent-9`
	if !strings.Contains(cuerpo, "La plataforma no reconoce este equipo") || !strings.Contains(cuerpo, esperado) {
		t.Fatalf("el panel debía decir qué pasó y dar el comando %q:\n%s", esperado, cuerpo)
	}

	// Con la nube bien no aparece nada de esto.
	panel.estado.Nube(nil)
	if w := pedir(r, "GET", "/", "127.0.0.1:5000", "localhost:8787", nil, ""); strings.Contains(w.Body.String(), "-Servicio") {
		t.Fatal("con la nube bien no debía ofrecer quitar la copia")
	}
}

func TestElComandoSoloLlevaNombresDeServicioDelInstalador(t *testing.T) {
	raiz := `C:\PorteriaAgent`
	if got := comandoParaQuitar(raiz, "PorteriaSyncAgent-2"); got != `powershell -ExecutionPolicy Bypass -File "C:\PorteriaAgent\uninstall.ps1" -Servicio PorteriaSyncAgent-2` &&
		got != `powershell -ExecutionPolicy Bypass -File "C:\PorteriaAgent/uninstall.ps1" -Servicio PorteriaSyncAgent-2` {
		t.Fatalf("comando: %s", got)
	}
	if comandoParaQuitar(raiz, ServiceName) == "" {
		t.Fatal("la primera copia también se puede quitar")
	}
	// estado.json de otra copia lo puede escribir cualquiera con acceso a la
	// carpeta: nada que no sea un nombre del instalador llega a un comando
	// que alguien pega como administrador.
	for _, malo := range []string{"", "Otro", `PorteriaSyncAgent; Remove-Item C:\ -Recurse`, "PorteriaSyncAgent-2 -Force", "PorteriaSyncAgent-x"} {
		if got := comandoParaQuitar(raiz, malo); got != "" {
			t.Fatalf("%q no debía dar comando, dio %q", malo, got)
		}
	}
}

func TestLaOtraCopiaHuerfanaTambienSeVe(t *testing.T) {
	raiz := t.TempDir()
	segunda := filepath.Join(raiz, "equipo-2")
	if err := os.MkdirAll(segunda, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{}
	cfg.Receiver.BindAddress = "0.0.0.0:8787"
	NuevoEstado(cfg, ServiceName, raiz).Nube(ErrLlaveRechazada)

	otras := otrasCopias(segunda)
	if len(otras) != 1 {
		t.Fatalf("debía ver la copia de la raíz: %+v", otras)
	}
	v := vista(otras[0], false, raiz)
	if v.Quitar == "" || !strings.Contains(v.Quitar, "-Servicio "+ServiceName) {
		t.Fatalf("la copia huérfana de al lado debía traer su comando: %+v", v)
	}
}

// El desinstalador entiende -Servicio con la misma forma de nombre que el
// panel pone en el comando, y el instalador avisa de las copias huérfanas.
func TestLosGuionesHablanIgualQueElPanel(t *testing.T) {
	des, err := os.ReadFile(filepath.Join("scripts", "uninstall.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, debe := range []string{"[string]$Servicio", `'^PorteriaSyncAgent(-\d{1,3})?$'`} {
		if !strings.Contains(string(des), debe) {
			t.Fatalf("uninstall.ps1 debía tener %s", debe)
		}
	}
	ins, err := os.ReadFile(filepath.Join("scripts", "install.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ins), "llave_rechazada") || !strings.Contains(string(ins), "-Servicio") {
		t.Fatal("install.ps1 debía avisar de las copias con la llave rechazada")
	}
}
