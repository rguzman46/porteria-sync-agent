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
		BaseURL string `yaml:"base_url"` // https://miconjunto.porteriaplus.com

		// Token es la **llave de API del conjunto** (`ppk_…`), la que se crea
		// en «Integraciones y API keys». Autentica al conjunto, no a la
		// cámara: puede ser la misma para todos los agents del conjunto.
		Token string `yaml:"token"`

		// DeviceToken es el token de **esta cámara** (`ppd_…`), el que el
		// panel muestra una sola vez al registrarla.
		//
		// Sin él, el cloud cae al primer dispositivo activo del conjunto, y en
		// una portería con cámara de entrada Y de salida eso significa que
		// **las salidas se registran como entradas**: el sentido y el modo son
		// del dispositivo, no de la llave. La visita no se cierra al salir y el
		// conteo de quién está adentro solo crece.
		//
		// Es opcional para no romper las instalaciones que ya están en campo
		// sin él —siguen funcionando como antes, atribuidas al primer
		// dispositivo—, pero cualquier instalación nueva debe ponerlo.
		DeviceToken string `yaml:"device_token"`
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
		//   hikvision_traffic    — línea Traffic (iDS-*, DS-2CD7*), `vehicleDetect/plateInfo`.
		//   hikvision_itc        — línea ITC Entrance (DS-TCG*), `ITC/Entrance/VCL`.
		//   hikvision_anpr_audit — firmwares recientes (7A26), `licensePlateAuditData`.
		//   dahua_itc            — Dahua Intelligent Traffic Camera.
		//   axis_vapix           — Axis con ACAP License Plate Verifier.
		// Vacío → derivar del Type.
		Family string `yaml:"family"`

		Host     string `yaml:"host"`     // 192.168.1.50
		Port     int    `yaml:"port"`     // 80 (default si 0)
		User     string `yaml:"user"`     // admin
		Password string `yaml:"password"` // ...

		// AutoConfig: si true (default), el agent acepta auto-actualizar la
		// familia cuando el cloud reporta una distinta en la respuesta del
		// whitelist o del latido. Útil para que el admin pueda cambiar el
		// modelo de cámara en el panel y el agent se reconfigure sin
		// reinstalación. Set a false si quieres pin manual.
		AutoConfig *bool `yaml:"auto_config"`
	} `yaml:"camera"`

	Poll struct {
		IntervalSeconds int `yaml:"interval_seconds"` // 60 por default
	} `yaml:"poll"`

	Log struct {
		// File es el archivo de log. Relativo → junto al config.yaml (que es
		// donde vive el binario en una instalación normal). Vacío → solo
		// stderr (lo que captura el gestor de servicios).
		File  string `yaml:"file"`
		Level string `yaml:"level"` // info | debug | warn | error
		// MaxSizeMB: al superarlo el archivo se rota a `.1`, `.2`… y se
		// conservan `Keep` copias. Sin esto el log de un PC de portería crecía
		// sin techo durante años hasta llenar el disco.
		MaxSizeMB int `yaml:"max_size_mb"` // default 10
		Keep      int `yaml:"keep"`        // default 3
	} `yaml:"log"`

	// Receiver — HTTP server local que recibe las lecturas de la cámara LPR
	// (foto + placa). La cámara postea aquí; el agent encola y reenvía al
	// cloud. Si Enabled=false, el agent corre solo como puller (whitelist
	// sync + heartbeat).
	Receiver struct {
		// Enabled es puntero para distinguir «no lo pusieron» de «lo apagaron»:
		// con un bool a secas la ausencia de la sección dejaba el receiver
		// apagado y la instalación quedaba sin fotos sin que nadie lo notara.
		Enabled     *bool  `yaml:"enabled"`      // default true
		BindAddress string `yaml:"bind_address"` // default 0.0.0.0:8787
		// AllowFrom son las IPs (o redes CIDR) que pueden postear lecturas
		// además de `camera.host`. AllowAny apaga el filtro: solo para
		// diagnóstico, porque el puerto escucha en toda la LAN y una lectura
		// falsa abre una visita en el panel.
		AllowFrom     []string `yaml:"allow_from"`
		AllowAny      bool     `yaml:"allow_any"`
		QueueDir      string   `yaml:"queue_dir"`       // default <dir del binario>/queue
		MaxQueueItems int      `yaml:"max_queue_items"` // default 10000
		MaxQueueBytes int64    `yaml:"max_queue_bytes"` // default 1GB
		ReplayTickSec int      `yaml:"replay_tick_sec"` // default 30
	} `yaml:"receiver"`
}

// familiasValidas son las que el factory de camera.go sabe construir.
var familiasValidas = map[string]bool{
	"hikvision_traffic":    true,
	"hikvision_itc":        true,
	"hikvision_anpr_audit": true,
	"dahua_itc":            true,
	"axis_vapix":           true,
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
// PORTERIA_DEVICE_TOKEN,
// PORTERIA_CAMERA_HOST, PORTERIA_CAMERA_USER, PORTERIA_CAMERA_PASSWORD,
// PORTERIA_CAMERA_TYPE, PORTERIA_CAMERA_FAMILY.
func (c *Config) applyEnvOverrides() {
	if v := os.Getenv("PORTERIA_CLOUD_URL"); v != "" {
		c.Cloud.BaseURL = v
	}
	if v := os.Getenv("PORTERIA_DEVICE_TOKEN"); v != "" {
		c.Cloud.DeviceToken = v
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
	if c.Log.MaxSizeMB == 0 {
		c.Log.MaxSizeMB = 10
	}
	if c.Log.Keep == 0 {
		c.Log.Keep = 3
	}
	c.Camera.Type = strings.ToLower(strings.TrimSpace(c.Camera.Type))
	c.Camera.Family = strings.ToLower(strings.TrimSpace(c.Camera.Family))

	// AutoConfig default true. El admin puede pasarlo explícito a false
	// si quiere pin manual de la familia.
	if c.Camera.AutoConfig == nil {
		t := true
		c.Camera.AutoConfig = &t
	}

	// Receiver encendido salvo que lo apaguen a propósito con
	// `receiver: { enabled: false }`. Antes el default real era «apagado» y
	// una instalación sin la sección quedaba sin fotos.
	if c.Receiver.Enabled == nil {
		t := true
		c.Receiver.Enabled = &t
	}
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
		return fmt.Errorf("cloud.token requerido (la llave del conjunto, `ppk_…`, de Integraciones y API keys)")
	}
	// `ppk_` es lo que emite la plataforma actual; `pa_` lo emitía la anterior
	// y sigue en algunas porterías. Exigir solo `pa_` dejaba a toda instalación
	// nueva sin arrancar.
	if !strings.HasPrefix(c.Cloud.Token, "ppk_") && !strings.HasPrefix(c.Cloud.Token, "pa_") {
		return fmt.Errorf("cloud.token tiene formato inválido (debe empezar con 'ppk_')")
	}
	if c.Camera.Host == "" {
		return fmt.Errorf("camera.host requerido (IP local de la cámara LPR)")
	}
	// Validamos contra marca top-level. La familia exacta se resuelve después
	// en `resolveVendorFamily` — aquí solo chequeamos que el vendor sea uno
	// conocido. Si se pasa `family` directo también lo validamos.
	switch {
	case c.Camera.Type == "hikvision", c.Camera.Type == "dahua", c.Camera.Type == "axis":
		// OK — la familia se resolverá en el factory.
	case familiasValidas[c.Camera.Type]:
		// Type ya tiene formato familia — válido directamente.
	default:
		return fmt.Errorf("camera.type debe ser uno de: hikvision, dahua, axis (o una familia específica: %s). Recibí %q", listaDeFamilias(), c.Camera.Type)
	}
	if c.Camera.Family != "" && !familiasValidas[c.Camera.Family] {
		return fmt.Errorf("camera.family debe ser una de: %s. Recibí %q", listaDeFamilias(), c.Camera.Family)
	}
	if c.Poll.IntervalSeconds < 30 {
		return fmt.Errorf("poll.interval_seconds debe ser >= 30 (recibí %d)", c.Poll.IntervalSeconds)
	}
	for _, origen := range c.Receiver.AllowFrom {
		if _, err := parseOrigen(origen); err != nil {
			return fmt.Errorf("receiver.allow_from: %w", err)
		}
	}
	return nil
}

// ReceiverEnabled dice si hay que levantar el receptor de fotos.
func (c *Config) ReceiverEnabled() bool {
	return c.Receiver.Enabled == nil || *c.Receiver.Enabled
}

func listaDeFamilias() string {
	return "hikvision_traffic, hikvision_itc, hikvision_anpr_audit, dahua_itc, axis_vapix"
}
