package main

import (
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestElLogRotaPorTamanoYConservaCopias(t *testing.T) {
	dir := t.TempDir()
	ruta := filepath.Join(dir, "agent.log")
	rot, err := abrirLogRotativo(ruta, 100, 2) // 100 bytes, 2 copias
	if err != nil {
		t.Fatal(err)
	}
	defer rot.Close()

	linea := []byte(strings.Repeat("a", 60) + "\n")
	for i := 0; i < 6; i++ { // 6×61 bytes → varias rotaciones
		if _, err := rot.Write(linea); err != nil {
			t.Fatal(err)
		}
	}

	for _, nombre := range []string{"agent.log", "agent.log.1", "agent.log.2"} {
		info, err := os.Stat(filepath.Join(dir, nombre))
		if err != nil {
			t.Errorf("falta %s: %v", nombre, err)
			continue
		}
		if info.Size() > 130 {
			t.Errorf("%s pesa %d, más que el tope + una línea", nombre, info.Size())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "agent.log.3")); err == nil {
		t.Error("agent.log.3 no debería existir con keep=2")
	}
}

func TestElLogRelativoVaJuntoAlConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{}
	cfg.Log.File = "agent.log"
	cfg.Log.MaxSizeMB = 1
	cfg.Log.Keep = 1
	cerrar, err := configurarLog(cfg, filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cerrar.Close()
		log.SetOutput(os.Stderr)
	}()
	if _, err := os.Stat(filepath.Join(dir, "agent.log")); err != nil {
		t.Errorf("el log no se creó junto al config: %v", err)
	}
}

// Como servicio de Windows no hay consola: escribir en stderr falla. El
// archivo tiene que recibir la línea igual (hasta la v1.7.7 quedaba vacío).
func TestElLogLlegaAlArchivoAunqueNoHayaConsola(t *testing.T) {
	ruta := filepath.Join(t.TempDir(), "agent.log")
	rot, err := abrirLogRotativo(ruta, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer rot.Close()

	// Así estaba hasta la v1.7.7: MultiWriter se detiene en la consola rota y
	// el archivo no recibe nada.
	_, _ = io.MultiWriter(consolaRota{}, rot).Write([]byte("antes\n"))
	if data, _ := os.ReadFile(ruta); len(data) != 0 {
		t.Fatalf("se esperaba reproducir el archivo vacío, tiene %q", data)
	}

	salida := salidaDelLog{archivo: rot, consola: consolaRota{}}
	if _, err := salida.Write([]byte("[cloud] heartbeat ok\n")); err != nil {
		t.Fatalf("sin consola no debía fallar: %v", err)
	}
	data, _ := os.ReadFile(ruta)
	if !strings.Contains(string(data), "heartbeat ok") {
		t.Fatalf("el archivo debía tener la línea, tiene %q", data)
	}
}

type consolaRota struct{}

func (consolaRota) Write([]byte) (int, error) { return 0, errors.New("The handle is invalid.") }
