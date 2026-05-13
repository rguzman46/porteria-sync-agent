package main

import (
	"context"
	"fmt"
)

// DahuaAdapter — STUB para v1. Implementación completa en próximo release.
//
// Dahua usa CGI API ubicada en:
//   /cgi-bin/configManager.cgi?action=setConfig&...  (config)
//   /cgi-bin/api/AccessControl/insertCard           (whitelist plates)
//
// Auth: Basic auth con realm específico de Dahua. Algunos firmwares
// requieren Digest.
//
// Si tu conjunto usa Dahua, contacta soporte@porteriaplus.com para que
// te incluyamos en el beta del adapter.
type DahuaAdapter struct {
	host     string
	port     int
	user     string
	password string
}

func NewDahuaAdapter(host string, port int, user, password string) *DahuaAdapter {
	return &DahuaAdapter{host: host, port: port, user: user, password: password}
}

func (d *DahuaAdapter) Name() string { return "dahua" }

func (d *DahuaAdapter) Ping(_ context.Context) error {
	return fmt.Errorf("dahua adapter aún no implementado (V1) — contacta soporte@porteriaplus.com")
}

func (d *DahuaAdapter) SyncWhitelist(_ context.Context, _ []Plate) error {
	return fmt.Errorf("dahua adapter aún no implementado (V1)")
}
