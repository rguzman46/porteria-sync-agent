package main

import (
	"context"
	"fmt"
	"strings"
)

// CameraAdapter es la abstracción común sobre cualquier vendor de cámara LPR.
// Cada adapter sabe cómo hablar con su hardware específico para sincronizar
// la lista de placas permitidas.
//
// La interfaz es minimalista por diseño: V1 sólo necesita PUSH del whitelist
// completo. Los eventos (placa detectada → apertura) la cámara los maneja
// localmente con la whitelist sincronizada. Si el cloud está caído, la
// portería sigue operando — esa es la razón fundamental del módulo.
type CameraAdapter interface {
	// Name identifica el adapter para logs (ej. "hikvision_traffic",
	// "hikvision_itc", "dahua_itc", "axis_vapix").
	Name() string

	// Ping verifica conectividad básica con la cámara. Se llama al boot
	// del agent para fallar rápido si la cámara no es alcanzable.
	Ping(ctx context.Context) error

	// SyncWhitelist empuja la lista completa de placas autorizadas a la
	// cámara. La implementación debe ser idempotente: si las mismas placas
	// ya están en la cámara, no debe causar churn ni reset de estado.
	//
	// Estrategia recomendada por implementación: GET current list → diff →
	// PUSH solo cambios. Si el vendor no soporta diff, replace completo
	// (caso Hikvision ISAPI v2).
	SyncWhitelist(ctx context.Context, plates []Plate) error
}

// resolveVendorFamily determina qué adapter exacto cargar.
//
// V1.3 introduce el concepto de "familia" (sub-vendor) además de la marca.
// Hikvision tiene dos líneas con endpoints ISAPI distintos:
//   - hikvision_traffic — línea profesional (iDS-*, DS-2CD7*).
//   - hikvision_itc     — línea entrada/salida residencial (DS-TCG*).
//
// Resolución (en orden de prioridad):
//
//  1. Si `cfg.Camera.Family` está seteado, ganar (override explícito).
//  2. Si el cloud envió `vendor_family` en heartbeat/whitelist, usarlo
//     (auto-config dinámico desde el panel admin — sin reinstalar).
//  3. Si `cfg.Camera.Type` ya tiene formato vendor_family (contiene `_`),
//     usarlo tal cual (compat retrocompat con instalaciones nuevas).
//  4. Si `cfg.Camera.Type` es solo marca (hikvision, dahua, axis), usar
//     la familia "default" del vendor:
//     - hikvision → hikvision_traffic (más común en producción hoy).
//     - dahua     → dahua_itc.
//     - axis      → axis_vapix.
//
// Esto preserva retrocompat con configs viejas (`type: hikvision`) y a la
// vez habilita el nuevo modo plug-and-play donde el admin elige el modelo
// exacto en el UI y el sistema deriva la familia automáticamente.
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
// `camera_<family>.go` correspondiente. Mantener orden alfabético por
// marca para que el diff sea limpio cuando se agrega un vendor.
func NewCameraAdapter(cfg *Config) (CameraAdapter, error) {
	family := resolveVendorFamily(cfg)
	switch family {
	// ── Hikvision ────────────────────────────────────────────────
	case "hikvision_traffic":
		return NewHikvisionTrafficAdapter(cfg.Camera.Host, cfg.Camera.Port, cfg.Camera.User, cfg.Camera.Password), nil
	case "hikvision_itc":
		return NewHikvisionITCAdapter(cfg.Camera.Host, cfg.Camera.Port, cfg.Camera.User, cfg.Camera.Password), nil
	// ── Dahua ────────────────────────────────────────────────────
	case "dahua_itc":
		return NewDahuaITCAdapter(cfg.Camera.Host, cfg.Camera.Port, cfg.Camera.User, cfg.Camera.Password), nil
	// ── Axis ─────────────────────────────────────────────────────
	case "axis_vapix":
		return NewAxisVapixAdapter(cfg.Camera.Host, cfg.Camera.Port, cfg.Camera.User, cfg.Camera.Password), nil
	default:
		return nil, fmt.Errorf("familia de cámara no soportada: %q (tipo=%q). Familias válidas: hikvision_traffic, hikvision_itc, dahua_itc, axis_vapix", family, cfg.Camera.Type)
	}
}
