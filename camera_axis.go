package main

import (
	"context"
	"fmt"
)

// AxisAdapter — STUB para v1. Implementación completa en próximo release.
//
// Axis usa VAPIX (Video API for X) ubicada en:
//   /vapix/cgi-bin/...
//
// LPR en Axis es via AXIS License Plate Verifier — la integración requiere
// el ACAP App específico instalado en la cámara y autenticación Basic con
// usuario admin.
//
// Si tu conjunto usa Axis, contacta soporte@porteriaplus.com para que te
// incluyamos en el beta del adapter.
type AxisAdapter struct {
	host     string
	port     int
	user     string
	password string
}

func NewAxisAdapter(host string, port int, user, password string) *AxisAdapter {
	return &AxisAdapter{host: host, port: port, user: user, password: password}
}

func (a *AxisAdapter) Name() string { return "axis" }

func (a *AxisAdapter) Ping(_ context.Context) error {
	return fmt.Errorf("axis adapter aún no implementado (V1) — contacta soporte@porteriaplus.com")
}

func (a *AxisAdapter) SyncWhitelist(_ context.Context, _ []Plate) error {
	return fmt.Errorf("axis adapter aún no implementado (V1)")
}
