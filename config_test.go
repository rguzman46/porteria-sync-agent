package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Pruebas de carga y validación de config.yaml.

func escribirConfig(t *testing.T, cuerpo string) string {
	t.Helper()
	ruta := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(ruta, []byte(cuerpo), 0o600); err != nil {
		t.Fatalf("escribiendo config: %v", err)
	}
	return ruta
}

const configMinima = `
cloud:
  base_url: https://demo.porteriaplus.com
  token: %s
camera:
  type: hikvision
  host: 192.168.1.50
  password: x
`

func TestLaLlaveDelConjuntoEmpiezaConPpk(t *testing.T) {
	// La plataforma emite `ppk_`; exigir `pa_` dejaba a toda instalación
	// nueva sin arrancar con «formato inválido».
	ruta := escribirConfig(t, strings.Replace(configMinima, "%s", "ppk_abc123", 1))
	if _, _, err := loadConfig(ruta); err != nil {
		t.Fatalf("una llave ppk_ debe pasar: %v", err)
	}
}

func TestLaLlaveViejaPaSigueAceptada(t *testing.T) {
	ruta := escribirConfig(t, strings.Replace(configMinima, "%s", "pa_abc123", 1))
	if _, _, err := loadConfig(ruta); err != nil {
		t.Fatalf("una llave pa_ de una portería vieja debe seguir pasando: %v", err)
	}
}

func TestUnaLlaveSinPrefijoConocidoSeRechaza(t *testing.T) {
	ruta := escribirConfig(t, strings.Replace(configMinima, "%s", "abc123", 1))
	_, _, err := loadConfig(ruta)
	if err == nil || !strings.Contains(err.Error(), "ppk_") {
		t.Fatalf("esperaba rechazo mencionando ppk_, recibí %v", err)
	}
}

func TestElReceiverEstaEncendidoSiNoLoApagan(t *testing.T) {
	// Sin sección `receiver:` la instalación tiene que recibir fotos. Antes el
	// default real era «apagado» y nadie lo notaba.
	ruta := escribirConfig(t, strings.Replace(configMinima, "%s", "ppk_abc123", 1))
	cfg, _, err := loadConfig(ruta)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ReceiverEnabled() {
		t.Fatal("receiver debería estar encendido por defecto")
	}

	ruta = escribirConfig(t, strings.Replace(configMinima, "%s", "ppk_abc123", 1)+"receiver:\n  enabled: false\n")
	cfg, _, err = loadConfig(ruta)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReceiverEnabled() {
		t.Fatal("enabled: false tiene que apagarlo")
	}
}

func TestAllowFromValidaIPsYRedes(t *testing.T) {
	base := strings.Replace(configMinima, "%s", "ppk_abc123", 1)
	ruta := escribirConfig(t, base+"receiver:\n  allow_from: ['192.168.1.0/24', '10.0.0.7']\n")
	if _, _, err := loadConfig(ruta); err != nil {
		t.Fatalf("IP y CIDR válidos deben pasar: %v", err)
	}
	ruta = escribirConfig(t, base+"receiver:\n  allow_from: ['la-camara']\n")
	if _, _, err := loadConfig(ruta); err == nil {
		t.Fatal("un origen que no es IP ni CIDR debe rechazarse al cargar, no al primer POST")
	}
}

func TestDefaultsDelLog(t *testing.T) {
	ruta := escribirConfig(t, strings.Replace(configMinima, "%s", "ppk_abc123", 1))
	cfg, _, err := loadConfig(ruta)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.File != "agent.log" || cfg.Log.MaxSizeMB != 10 || cfg.Log.Keep != 3 {
		t.Errorf("defaults del log: %+v", cfg.Log)
	}
}

func TestLaFamiliaAuditEsValida(t *testing.T) {
	ruta := escribirConfig(t, `
cloud:
  base_url: https://demo.porteriaplus.com
  token: ppk_abc123
camera:
  type: hikvision
  family: hikvision_anpr_audit
  host: 192.168.1.50
`)
	cfg, _, err := loadConfig(ruta)
	if err != nil {
		t.Fatal(err)
	}
	if resolveVendorFamily(cfg) != "hikvision_anpr_audit" {
		t.Errorf("familia resuelta: %q", resolveVendorFamily(cfg))
	}
}
