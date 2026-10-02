package main

import (
	"context"
	"fmt"
	"strings"
)

// CameraAdapter es la abstracción común sobre cualquier vendor de cámara LPR.
// Cada adapter sabe cómo hablar con su hardware específico para sincronizar
// las listas de placas.
//
// La interfaz es minimalista por diseño: solo hace falta PUSH de las listas
// completas. Los eventos (placa detectada → apertura) la cámara los maneja
// localmente con la lista sincronizada. Si el cloud está caído, la portería
// sigue operando — esa es la razón fundamental del módulo.
type CameraAdapter interface {
	// Name identifica el adapter para logs (ej. "hikvision_traffic",
	// "hikvision_itc", "dahua_itc", "axis_vapix").
	Name() string

	// Ping verifica conectividad básica con la cámara. Se llama al boot
	// del agent para fallar rápido si la cámara no es alcanzable.
	Ping(ctx context.Context) error

	// Capacidades dice qué listas sabe escribir este adaptador de verdad.
	// Un adaptador cuyo endpoint no está confirmado contra una cámara
	// física reporta `false` y el agente no intenta el push: es preferible
	// que el panel diga «esta cámara no recibe la lista» a que diga que sí
	// y no sea cierto.
	Capacidades() Capacidades

	// SyncWhitelist empuja las placas permitidas y las negadas a la cámara.
	// La implementación debe ser idempotente: si las mismas placas ya están
	// en la cámara, no debe causar churn ni reset de estado.
	//
	// `blocked` se escribe como lista negra donde el vendor la tiene; si
	// `Capacidades().Blocklist` es false, se ignora.
	SyncWhitelist(ctx context.Context, plates []Plate, blocked []Plate) error
}

// resolveVendorFamily determina qué adapter exacto cargar.
//
// Hikvision tiene tres formas de recibir la lista según la línea y el
// firmware:
//   - hikvision_traffic    — `/ISAPI/Traffic/channels/1/vehicleDetect/plateInfo`.
//   - hikvision_itc        — `/ISAPI/ITC/Entrance/VCL` (DS-TCG*, firmwares viejos).
//   - hikvision_anpr_audit — `/ISAPI/Traffic/channels/1/licensePlateAuditData`
//     (firmwares recientes, p. ej. iDS-2CD7A26).
//
// Resolución (en orden de prioridad):
//
//  1. Si `cfg.Camera.Family` está seteado, ganar (override explícito).
//  2. Si el cloud envió `vendor_family` en la respuesta del whitelist,
//     usarlo (auto-config desde el panel admin — sin reinstalar).
//  3. Si `cfg.Camera.Type` ya tiene formato vendor_family (contiene `_`),
//     usarlo tal cual.
//  4. Si `cfg.Camera.Type` es solo marca (hikvision, dahua, axis), usar
//     la familia "default" del vendor:
//     - hikvision → hikvision_traffic (más común en producción hoy).
//     - dahua     → dahua_itc.
//     - axis      → axis_vapix.
func resolveVendorFamily(cfg *Config) string {
	if f := strings.TrimSpace(strings.ToLower(cfg.Camera.Family)); f != "" {
		return f
	}

	t := strings.TrimSpace(strings.ToLower(cfg.Camera.Type))
	if strings.Contains(t, "_") {
		return t
	}

	switch t {
	case "hikvision":
		return "hikvision_traffic"
	case "dahua":
		return "dahua_itc"
	case "axis":
		return "axis_vapix"
	default:
		return t
	}
}

// NewCameraAdapter construye el adapter correcto según la familia resuelta.
// Centraliza el factory para que el main.go no tenga que conocer cada vendor.
//
// Nuevas familias se agregan aquí con un case más, apuntando a su archivo
// `camera_<family>.go` correspondiente, y en `familiasValidas` de config.go.
func NewCameraAdapter(cfg *Config) (CameraAdapter, error) {
	family := resolveVendorFamily(cfg)
	c := cfg.Camera
	switch family {
	// ── Hikvision ────────────────────────────────────────────────
	case "hikvision_traffic":
		return NewHikvisionTrafficAdapter(c.Host, c.Port, c.User, c.Password), nil
	case "hikvision_itc":
		return NewHikvisionITCAdapter(c.Host, c.Port, c.User, c.Password), nil
	case "hikvision_anpr_audit":
		return NewHikvisionAuditAdapter(c.Host, c.Port, c.User, c.Password), nil
	// ── Dahua ────────────────────────────────────────────────────
	case "dahua_itc":
		return NewDahuaITCAdapter(c.Host, c.Port, c.User, c.Password), nil
	// ── Axis ─────────────────────────────────────────────────────
	case "axis_vapix":
		return NewAxisVapixAdapter(c.Host, c.Port, c.User, c.Password).conEstado(rutaDeEstadoAxis(c.Host, c.Port)), nil
	default:
		return nil, fmt.Errorf("familia de cámara no soportada: %q (tipo=%q). Familias válidas: %s", family, cfg.Camera.Type, listaDeFamilias())
	}
}
