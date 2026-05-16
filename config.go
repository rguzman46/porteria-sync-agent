package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config define todos los parámetros del sync agent. Se carga desde
// config.yaml (junto al binario) con override por variables de entorno
// `PORTERIA_*`. Las credenciales sensibles (camera.password) pueden vivir
// en el OS keychain en una versión futura — por ahora viven en el yaml
// con permisos restrictivos (chmod 600).
type Config struct {
	Cloud struct {
		BaseURL string `yaml:"base_url"` // https://catamaran.porteriaplus.com
		Token   string `yaml:"token"`    // pa_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
	} `yaml:"cloud"`

	Camera struct {
		// Type es la marca de la cámara (hikvision | dahua | axis). Si no se
		// pasa `family`, el agent elige la familia "default" del vendor (ver
		// `resolveVendorFamily` en camera.go). Si `family` está seteado, gana.
		// V1.3+: aceptamos también valores con sufijo de familia directamente
		// (ej. "hikvision_itc") para configs simples sin dos campos separados.
		Type string `yaml:"type"`

		// Family especifica la familia/línea dentro de la marca cuando hay
		// múltiples con endpoints distintos. Valores:
		//   hikvision_traffic — línea Traffic (iDS-*, DS-2CD7*).
		//   hikvision_itc     — línea ITC Entrance (DS-TCG*).
		//   dahua_itc         — Dahua Intelligent Traffic Camera.
		//   axis_vapix        — Axis con ACAP License Plate Verifier.
		// Vacío → derivar del Type.
		Family string `yaml:"family"`

		Host     string `yaml:"host"`     // 192.168.1.50
		Port     int    `yaml:"port"`     // 80 (default si 0)
		User     string `yaml:"user"`     // admin
		Password string `yaml:"password"` // ...

		// AutoConfig: si true (default), el agent acepta auto-actualizar la
		// familia cuando el cloud reporta una distinta en /api/access/whitelist
		// o /heartbeat. Útil para que el admin pueda cambiar el modelo de
		// cámara en el panel y el agent se reconfigure sin reinstalación.
		// Set a false si quieres pin manual (caso debug / cámara experimental).
		AutoConfig *bool `yaml:"auto_config"`
	} `yaml:"camera"`

	Poll struct {
		IntervalSeconds int `yaml:"interval_seconds"` // 60 por default
	} `yaml:"poll"`

	Log struct {
		File  string `yaml:"file"`  // logs/agent.log (relativo al binario)
		Level string `yaml:"level"` // info | debug | warn | error
	} `yaml:"log"`

	// Receiver — HTTP server local que recibe multipart de la cámara LPR
	// (Módulo LPR — Día 8, captura visual). La cámara postea aquí; el agent
	// encola y reenvía al cloud. Si Enabled=false, el agent corre solo como
	// puller (whitelist sync + heartbeat) — compat backward con v0.1.x.
	Receiver struct {
		Enabled       bool   `yaml:"enabled"`         // default true
		BindAddress   string `yaml:"bind_address"`    // default 0.0.0.0:8787
		QueueDir      string `yaml:"queue_dir"`       // default <appdata>/queue
		MaxQueueItems int    `yaml:"max_queue_items"` // default 10000
		MaxQueueBytes int64  `yaml:"max_queue_bytes"` // default 1GB
		ReplayTickSec int    `yaml:"replay_tick_sec"` // default 30
	} `yaml:"receiver"`
}

// loadConfig lee `configPath` (yaml), aplica overrides de env y devuelve
// la configuración validada + la ruta resuelta (necesaria para registrar
// como argumento del servicio Windows/launchd). Si configPath está vacío
// busca:
//
//	./config.yaml  (junto al binario)
//	%APPDATA%/PorteriaAgent/config.yaml  (Windows)
//	$HOME/.porteria-agent/config.yaml    (Unix)
func loadConfig(configPath string) (*Config, string, error) {
	if configPath == "" {
		configPath = findDefaultConfigPath()
	}
	absPath, _ := filepath.Abs(configPath)

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, absPath, fmt.Errorf("leyendo %s: %w", configPath, err)
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, absPath, fmt.Errorf("parseando yaml: %w", err)
	}

	cfg.applyEnvOverrides()
	cfg.applyDefaults()

	if err := cfg.validate(); err != nil {
		return nil, absPath, err
	}

	return cfg, absPath, nil
}

func findDefaultConfigPath() string {
	// 1. Junto al binario (deploy típico: copy + config en mismo dir)
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "config.yaml")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	// 2. AppData (Windows) o ~/.porteria-agent (Unix)
	if appdata := os.Getenv("APPDATA"); appdata != "" {
		candidate := filepath.Join(appdata, "PorteriaAgent", "config.yaml")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, ".porteria-agent", "config.yaml")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	return "config.yaml" // fallback al cwd
}

// applyEnvOverrides permite que credenciales sensibles se inyecten por env
// en lugar de yaml (útil en CI / docker / Windows Service con env vars).
// Variables soportadas: PORTERIA_CLOUD_URL, PORTERIA_CLOUD_TOKEN,
// PORTERIA_CAMERA_HOST, PORTERIA_CAMERA_USER, PORTERIA_CAMERA_PASSWORD,
// PORTERIA_CAMERA_TYPE, PORTERIA_CAMERA_FAMILY.
func (c *Config) applyEnvOverrides() {
	if v := os.Getenv("PORTERIA_CLOUD_URL"); v != "" {
		c.Cloud.BaseURL = v
	}
	if v := os.Getenv("PORTERIA_CLOUD_TOKEN"); v != "" {
		c.Cloud.Token = v
	}
	if v := os.Getenv("PORTERIA_CAMERA_TYPE"); v != "" {
		c.Camera.Type = v
	}
	if v := os.Getenv("PORTERIA_CAMERA_FAMILY"); v != "" {
		c.Camera.Family = v
	}
	if v := os.Getenv("PORTERIA_CAMERA_HOST"); v != "" {
		c.Camera.Host = v
	}
	if v := os.Getenv("PORTERIA_CAMERA_USER"); v != "" {
		c.Camera.User = v
	}
	if v := os.Getenv("PORTERIA_CAMERA_PASSWORD"); v != "" {
		c.Camera.Password = v
	}
}

func (c *Config) applyDefaults() {
	if c.Camera.Port == 0 {
		c.Camera.Port = 80
	}
	if c.Poll.IntervalSeconds == 0 {
		c.Poll.IntervalSeconds = 60
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Log.File == "" {
		c.Log.File = "agent.log"
	}
	c.Camera.Type = strings.ToLower(strings.TrimSpace(c.Camera.Type))
	c.Camera.Family = strings.ToLower(strings.TrimSpace(c.Camera.Family))

	// AutoConfig default true. El admin puede pasarlo explícito a false
	// si quiere pin manual de la familia.
	if c.Camera.AutoConfig == nil {
		t := true
		c.Camera.AutoConfig = &t
	}

	// Receiver defaults — habilitado por default en v0.2.0+. Para deshabilitar
	// explícitamente (modo legacy v0.1.x sin captura visual), poner
	// `receiver: { enabled: false }` en el yaml.
	if c.Receiver.BindAddress == "" {
		c.Receiver.BindAddress = "0.0.0.0:8787"
	}
	if c.Receiver.QueueDir == "" {
		// Default: <directorio del binario>/queue (mismo lugar que config.yaml).
		// Si no podemos resolver exe path, fallback a CWD.
		if exe, err := os.Executable(); err == nil {
			c.Receiver.QueueDir = filepath.Join(filepath.Dir(exe), "queue")
		} else {
			c.Receiver.QueueDir = "queue"
		}
	}
	if c.Receiver.MaxQueueItems == 0 {
		c.Receiver.MaxQueueItems = 10000
	}
	if c.Receiver.MaxQueueBytes == 0 {
		c.Receiver.MaxQueueBytes = 1 * 1024 * 1024 * 1024 // 1GB
	}
	if c.Receiver.ReplayTickSec == 0 {
		c.Receiver.ReplayTickSec = 30
	}
}

func (c *Config) validate() error {
	if c.Cloud.BaseURL == "" {
		return fmt.Errorf("cloud.base_url requerido (ej: https://catamaran.porteriaplus.com)")
	}
	if c.Cloud.Token == "" {
		return fmt.Errorf("cloud.token requerido (Bearer del device, generado en /integrations)")
	}
	if !strings.HasPrefix(c.Cloud.Token, "pa_") {
		return fmt.Errorf("cloud.token tiene formato inválido (debe empezar con 'pa_')")
	}
	if c.Camera.Host == "" {
		return fmt.Errorf("camera.host requerido (IP local de la cámara LPR)")
	}
	// Validamos contra marca top-level. La familia exacta (hikvision_traffic vs
	// hikvision_itc) se resuelve después en `resolveVendorFamily` — aquí solo
	// chequeamos que el vendor sea uno conocido. Si se pasa `family` directo
	// también lo validamos.
	switch c.Camera.Type {
	case "hikvision", "dahua", "axis":
		// OK — la familia se resolverá en el factory.
	case "hikvision_traffic", "hikvision_itc", "dahua_itc", "axis_vapix":
		// Type ya tiene formato familia — válido directamente.
	default:
		return fmt.Errorf("camera.type debe ser uno de: hikvision, dahua, axis (o una familia específica: hikvision_traffic/itc, dahua_itc, axis_vapix). Recibí %q", c.Camera.Type)
	}
	if c.Camera.Family != "" {
		switch c.Camera.Family {
		case "hikvision_traffic", "hikvision_itc", "dahua_itc", "axis_vapix":
			// OK
		default:
			return fmt.Errorf("camera.family debe ser una de: hikvision_traffic, hikvision_itc, dahua_itc, axis_vapix. Recibí %q", c.Camera.Family)
		}
	}
	if c.Poll.IntervalSeconds < 30 {
		return fmt.Errorf("poll.interval_seconds debe ser >= 30 (recibí %d)", c.Poll.IntervalSeconds)
	}
	return nil
}
