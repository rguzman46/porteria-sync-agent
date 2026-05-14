package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
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

	// acknowledgedLastModified es el header `Last-Modified` que el syncer
	// confirmó haber empujado exitosamente a la cámara local. Se envía como
	// `If-Modified-Since` en la siguiente request — si nada cambió el cloud
	// retorna 304 (sin body).
	//
	// pendingLastModified es lo recibido en la última respuesta 200 PERO
	// aún no empujado a la cámara. Solo se promueve a acknowledgedLastModified
	// cuando el syncer llama AckPending() tras un push exitoso.
	//
	// Sin esta separación: si la cámara local falla en el push, el cloud
	// avanza lastModified igual, próximo poll retorna 304 y la whitelist
	// queda permanentemente desincronizada hasta que algo mute en BD.
	acknowledgedLastModified string
	pendingLastModified      string
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
	if c.acknowledgedLastModified != "" {
		req.Header.Set("If-Modified-Since", c.acknowledgedLastModified)
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
		// Guardamos el Last-Modified como PENDIENTE — solo se promueve a
		// acknowledged cuando el syncer confirme que la cámara recibió el
		// whitelist exitosamente vía AckPending().
		if lm := resp.Header.Get("Last-Modified"); lm != "" {
			c.pendingLastModified = lm
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

// PostEventMultipartResult resume el resultado del cloud al recibir el
// evento + snapshot. Solo expone lo útil para logs del agent.
type PostEventMultipartResult struct {
	OK              bool   `json:"ok"`
	EventID         int64  `json:"event_id"`
	Action          string `json:"action"`
	VisitaID        *int64 `json:"visita_id"`
	Deduplicated    bool   `json:"deduplicated"`
	SnapshotStored  bool   `json:"snapshot_stored"`
	SnapshotPath    string `json:"snapshot_path"`
}

// PostEventResultStatus categoriza el outcome para decidir si reintentamos.
type PostEventResultStatus int

const (
	// PostEventSuccess: el cloud aceptó (HTTP 2xx). Borrar de la queue.
	PostEventSuccess PostEventResultStatus = iota
	// PostEventPermanent: el cloud rechazó con 4xx (auth, payload inválido,
	// capture deshabilitada por toggle). Reintentar no va a ayudar — descartar.
	PostEventPermanent
	// PostEventTransient: error de red, 5xx, timeout. Reintentar con back-off.
	PostEventTransient
)

// PostEventMultipart envía un evento queued + su snapshot al cloud via
// POST /api/access/event/multipart. Construye el multipart in-memory y lo
// envía con Bearer auth + User-Agent.
//
// Retorna el status categorizado para que el replay worker decida si
// borrar de la queue (success/permanent) o reintentar (transient).
func (c *CloudClient) PostEventMultipart(ctx context.Context, ev *QueuedEvent, snapshot []byte) (PostEventResultStatus, *PostEventMultipartResult, error) {
	body, contentType, err := buildEventMultipartBody(ev, snapshot)
	if err != nil {
		return PostEventPermanent, nil, fmt.Errorf("construyendo multipart: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/access/event/multipart", body)
	if err != nil {
		return PostEventPermanent, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		// Errores de red — transient, reintentar más tarde.
		return PostEventTransient, nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var result PostEventMultipartResult
		if err := json.Unmarshal(respBody, &result); err != nil {
			// El cloud aceptó pero respondió shape raro — éxito desde el
			// punto de vista del agent.
			return PostEventSuccess, nil, nil
		}
		return PostEventSuccess, &result, nil
	}

	// 4xx → permanente (auth, payload, capture_snapshot=false)
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return PostEventPermanent, nil, fmt.Errorf("status %d: %s", resp.StatusCode, snippet(respBody, 256))
	}

	// 5xx / otros → transient
	return PostEventTransient, nil, fmt.Errorf("status %d: %s", resp.StatusCode, snippet(respBody, 256))
}

// buildEventMultipartBody construye un body multipart in-memory con dos
// partes: event_data (JSON) + snapshot (binary). El endpoint cloud lo
// espera con esos nombres exactos (ver StoreAccessEventMultipartRequest).
func buildEventMultipartBody(ev *QueuedEvent, snapshot []byte) (io.Reader, string, error) {
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)

	// 1) event_data como JSON.
	eventJSON, err := json.Marshal(map[string]any{
		"plate":     ev.Plate,
		"direction": ev.Direction,
		"timestamp": ev.Timestamp,
		"metadata":  ev.Metadata,
	})
	if err != nil {
		return nil, "", err
	}

	eventHeader := textproto.MIMEHeader{}
	eventHeader.Set("Content-Disposition", `form-data; name="event_data"`)
	eventHeader.Set("Content-Type", "application/json")
	eventPart, err := mw.CreatePart(eventHeader)
	if err != nil {
		return nil, "", err
	}
	if _, err := eventPart.Write(eventJSON); err != nil {
		return nil, "", err
	}

	// 2) snapshot como image/* — usamos el mime original del evento (JPEG
	// típicamente de Hikvision). El cloud convierte a WebP server-side.
	mimeType := ev.SnapshotMimeType
	if mimeType == "" {
		mimeType = "image/jpeg"
	}
	snapshotHeader := textproto.MIMEHeader{}
	snapshotHeader.Set("Content-Disposition", `form-data; name="snapshot"; filename="snapshot.jpg"`)
	snapshotHeader.Set("Content-Type", mimeType)
	snapshotPart, err := mw.CreatePart(snapshotHeader)
	if err != nil {
		return nil, "", err
	}
	if _, err := snapshotPart.Write(snapshot); err != nil {
		return nil, "", err
	}

	if err := mw.Close(); err != nil {
		return nil, "", err
	}

	return buf, mw.FormDataContentType(), nil
}

func snippet(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…"
}

// AckPending promueve el Last-Modified pendiente a confirmado. Debe
// llamarse SOLO después de que el syncer empujó exitosamente el whitelist
// a la cámara local. Si nunca se llama tras un fetch 200, el siguiente
// FetchWhitelist re-pedirá el mismo whitelist (el If-Modified-Since del
// próximo poll seguirá siendo el valor viejo).
//
// Si no hay pending (nunca hubo un fetch 200 desde el último ack), no hace
// nada — idempotente.
func (c *CloudClient) AckPending() {
	if c.pendingLastModified != "" {
		c.acknowledgedLastModified = c.pendingLastModified
		c.pendingLastModified = ""
	}
}
