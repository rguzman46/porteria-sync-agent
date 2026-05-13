package main

import (
	"context"
	"fmt"
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
	// Name identifica el adapter para logs (ej. "hikvision").
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

// NewCameraAdapter construye el adapter correcto según config.camera.type.
// Centraliza el factory para que el main.go no tenga que conocer cada vendor.
func NewCameraAdapter(cfg *Config) (CameraAdapter, error) {
	switch cfg.Camera.Type {
	case "hikvision":
		return NewHikvisionAdapter(cfg.Camera.Host, cfg.Camera.Port, cfg.Camera.User, cfg.Camera.Password), nil
	case "dahua":
		return NewDahuaAdapter(cfg.Camera.Host, cfg.Camera.Port, cfg.Camera.User, cfg.Camera.Password), nil
	case "axis":
		return NewAxisAdapter(cfg.Camera.Host, cfg.Camera.Port, cfg.Camera.User, cfg.Camera.Password), nil
	default:
		return nil, fmt.Errorf("vendor de cámara no soportado: %q", cfg.Camera.Type)
	}
}
