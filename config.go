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
		Type     string `yaml:"type"`     // hikvision | dahua | axis
		Host     string `yaml:"host"`     // 192.168.1.50
		Port     int    `yaml:"port"`     // 80 (default si 0)
		User     string `yaml:"user"`     // admin
		Password string `yaml:"password"` // ...
	} `yaml:"camera"`

	Poll struct {
		IntervalSeconds int `yaml:"interval_seconds"` // 60 por default
	} `yaml:"poll"`

	Log struct {
		File  string `yaml:"file"`  // logs/agent.log (relativo al binario)
		Level string `yaml:"level"` // info | debug | warn | error
	} `yaml:"log"`
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
// PORTERIA_CAMERA_HOST, PORTERIA_CAMERA_USER, PORTERIA_CAMERA_PASSWORD.
func (c *Config) applyEnvOverrides() {
	if v := os.Getenv("PORTERIA_CLOUD_URL"); v != "" {
		c.Cloud.BaseURL = v
	}
	if v := os.Getenv("PORTERIA_CLOUD_TOKEN"); v != "" {
		c.Cloud.Token = v
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
	switch c.Camera.Type {
	case "hikvision", "dahua", "axis":
		// OK
	default:
		return fmt.Errorf("camera.type debe ser uno de: hikvision, dahua, axis (recibí %q)", c.Camera.Type)
	}
	if c.Poll.IntervalSeconds < 30 {
		return fmt.Errorf("poll.interval_seconds debe ser >= 30 (recibí %d)", c.Poll.IntervalSeconds)
	}
	return nil
}
