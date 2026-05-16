package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// AxisVapixAdapter integra con cámaras Axis vía VAPIX (Video API for X)
// + el ACAP "AXIS License Plate Verifier" que debe estar pre-instalado en
// la cámara con licencia activa.
//
// Modelos soportados (familia `axis_vapix`):
//   - P3265-LVE — bullet 2MP outdoor con LPR ACAP.
//   - Q1700-LE — box camera profesional para vías y parqueaderos.
//
// IMPORTANTE: Axis requiere comprar la licencia ACAP por separado
// (~$80-200 USD por cámara). Sin ACAP, la cámara NO hace OCR de placas
// — solo es CCTV regular. El admin debe verificar instalación antes
// de configurar este adapter.
//
// Endpoints VAPIX relevantes:
//
//	GET  /axis-cgi/basicdeviceinfo.cgi              — info del device (health)
//	POST /local/lpv/.api?json=1                     — API del ACAP LPV
//	     {"apiVersion":"1.0","method":"addAllowlist","params":{"plate":"ABC123"}}
//	     {"apiVersion":"1.0","method":"clearAllowlist"}
//
// Auth: Digest por default (algunos firmwares aceptan Basic via flag config).
// Mismo helper digestClient que Hikvision/Dahua.
type AxisVapixAdapter struct {
	host     string
	port     int
	user     string
	password string
	digest   *digestClient
}

func NewAxisVapixAdapter(host string, port int, user, password string) *AxisVapixAdapter {
	httpClient := &http.Client{Timeout: 30 * time.Second}
	return &AxisVapixAdapter{
		host:     host,
		port:     port,
		user:     user,
		password: password,
		digest:   newDigestClient(user, password, httpClient),
	}
}

func (a *AxisVapixAdapter) Name() string { return "axis_vapix" }

func (a *AxisVapixAdapter) Ping(ctx context.Context) error {
	// basicdeviceinfo.cgi NO requiere ACAP — funciona en cualquier cámara
	// Axis. Si esto retorna 200, la cámara responde aunque no tenga LPV.
	u := fmt.Sprintf("http://%s:%d/axis-cgi/basicdeviceinfo.cgi", a.host, a.port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := a.digest.Do(req)
	if err != nil {
		return fmt.Errorf("ping a %s: %w", a.host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ping status %d", resp.StatusCode)
	}
	return nil
}

// SyncWhitelist empuja la lista a la cámara vía API del ACAP LPV.
// Estrategia: clearAllowlist + addAllowlist por placa (LPV no soporta
// bulk upload). Mismo trade-off que Dahua — optimizar a diff en V2 si
// conjuntos grandes lo necesitan.
func (a *AxisVapixAdapter) SyncWhitelist(ctx context.Context, plates []Plate) error {
	// 1. Clear: vaciar la allowlist actual.
	if err := a.callLPV(ctx, lpvRequest{
		APIVersion: "1.0",
		Method:     "clearAllowlist",
	}); err != nil {
		return fmt.Errorf("clearAllowlist: %w", err)
	}

	// 2. Insert: una request por placa.
	failed := 0
	for _, p := range plates {
		params := map[string]any{
			"plate": p.Plate,
		}
		if p.Owner != "" {
			params["description"] = truncateStr(p.Owner, 64)
		}
		if p.ValidUntil != "" {
			params["expiry"] = p.ValidUntil // ISO 8601 — LPV lo acepta nativo
		}

		err := a.callLPV(ctx, lpvRequest{
			APIVersion: "1.0",
			Method:     "addAllowlist",
			Params:     params,
		})
		if err != nil {
			failed++
			if failed > 10 {
				return fmt.Errorf("demasiados fallos en addAllowlist (%d): última: %w", failed, err)
			}
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d placas no se pudieron añadir a Axis LPV (de %d totales)", failed, len(plates))
	}
	return nil
}

// lpvRequest es la estructura JSON-RPC que espera la API del ACAP
// AXIS License Plate Verifier.
type lpvRequest struct {
	APIVersion string         `json:"apiVersion"`
	Method     string         `json:"method"`
	Params     map[string]any `json:"params,omitempty"`
}

type lpvResponse struct {
	APIVersion string `json:"apiVersion"`
	Method     string `json:"method"`
	Error      *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (a *AxisVapixAdapter) callLPV(ctx context.Context, payload lpvRequest) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	u := fmt.Sprintf("http://%s:%d/local/lpv/.api?json=1", a.host, a.port)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := a.digest.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("ACAP LPV no instalado en la cámara (404 en /local/lpv/.api). Instala 'AXIS License Plate Verifier' desde Apps en la web admin antes de usar este adapter")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s status %d: %s", payload.Method, resp.StatusCode, respBody)
	}

	// Validar formato JSON-RPC del ACAP. Si el body no parsea como JSON
	// (p.ej. firmware viejo que devuelve text/plain), pasamos por OK siempre
	// que el status sea 2xx — confiamos en el código HTTP.
	var parsed lpvResponse
	if err := json.Unmarshal(respBody, &parsed); err == nil && parsed.Error != nil {
		return fmt.Errorf("%s rechazado por LPV (code=%d): %s", payload.Method, parsed.Error.Code, parsed.Error.Message)
	}
	return nil
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
