package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"
)

// Plate representa una entrada del whitelist devuelto por el cloud.
type Plate struct {
	Plate      string `json:"plate"`
	Type       string `json:"type"`   // "vehicle" | "invitation"
	Owner      string `json:"owner"`  // nullable
	ValidFrom  string `json:"valid_from"`
	ValidUntil string `json:"valid_until"`
}

// Whitelist es la respuesta completa del endpoint /api/access/whitelist.
type Whitelist struct {
	Version     string  `json:"version"`
	GeneratedAt string  `json:"generated_at"`
	Plates      []Plate `json:"plates"`
}

// CloudClient encapsula las llamadas HTTPS al cloud de Porteria Plus.
// Usa un único http.Client con timeouts agresivos — el polling loop debe
// fallar rápido para reintentar, no quedarse colgado bloqueando el ciclo.
type CloudClient struct {
	baseURL    string
	token      string
	http       *http.Client
	userAgent  string

	// lastModified es el header `Last-Modified` recibido en la última
	// respuesta 200. Se envía como `If-Modified-Since` en la siguiente
	// request — si nada cambió el cloud retorna 304 (sin body).
	lastModified string
}

// AgentVersion es la versión del binario, inyectada en build via -ldflags.
var AgentVersion = "dev"

func NewCloudClient(baseURL, token string) *CloudClient {
	return &CloudClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        4,
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     90 * time.Second,
				// El cloud usa TLS válido — sin tweaks de cert verification.
			},
		},
		userAgent: fmt.Sprintf("PorteriaSyncAgent/%s (%s/%s)", AgentVersion, runtime.GOOS, runtime.GOARCH),
	}
}

// FetchWhitelist obtiene el whitelist actual. Retorna (whitelist, true, nil)
// si hubo cambios desde la última llamada, (nil, false, nil) si retornó 304,
// o (nil, false, err) en cualquier otro caso.
//
// Persiste internamente el header `Last-Modified` recibido para usarlo como
// `If-Modified-Since` en la próxima llamada → smart polling escalable.
func (c *CloudClient) FetchWhitelist(ctx context.Context) (*Whitelist, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/access/whitelist", nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if c.lastModified != "" {
		req.Header.Set("If-Modified-Since", c.lastModified)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified:
		// Nada cambió — el agent NO empuja a la cámara, solo lo registra.
		return nil, false, nil

	case http.StatusOK:
		body, err := io.ReadAll(io.LimitReader(resp.Body, 50*1024*1024)) // 50MB cap defensivo
		if err != nil {
			return nil, false, fmt.Errorf("leyendo body: %w", err)
		}
		var wl Whitelist
		if err := json.Unmarshal(body, &wl); err != nil {
			return nil, false, fmt.Errorf("parseando whitelist json: %w", err)
		}
		if lm := resp.Header.Get("Last-Modified"); lm != "" {
			c.lastModified = lm
		}
		return &wl, true, nil

	case http.StatusUnauthorized:
		return nil, false, fmt.Errorf("token inválido (401) — regenera el token en el panel /integrations")

	default:
		// Cualquier otro código: leer un poco del body para mostrar al usuario.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, false, fmt.Errorf("status inesperado %d: %s", resp.StatusCode, snippet)
	}
}

// Heartbeat reporta al cloud que el agent está vivo. Incluye agent_version
// y system_info para que el panel del admin pueda diagnosticar a distancia.
// Retorna la versión actual del whitelist desde la respuesta del cloud
// (útil para detectar cambios incluso entre polls del whitelist).
func (c *CloudClient) Heartbeat(ctx context.Context) (whitelistVersion string, err error) {
	payload := map[string]string{
		"agent_version": AgentVersion,
		"system_info":   fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/access/heartbeat", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("heartbeat status %d: %s", resp.StatusCode, snippet)
	}

	var response struct {
		OK               bool   `json:"ok"`
		ServerTime       string `json:"server_time"`
		WhitelistVersion string `json:"whitelist_version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return "", fmt.Errorf("parseando respuesta heartbeat: %w", err)
	}
	return response.WhitelistVersion, nil
}
