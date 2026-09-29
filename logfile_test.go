package main

import (
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
