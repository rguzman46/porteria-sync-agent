package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"time"
)

// HikvisionAdapter integra con la API ISAPI (Intelligent Security API)
// de cámaras Hikvision. Documentación oficial:
// https://www.hikvision.com/en/support/download/sdk/
//
// Endpoints relevantes para LPR:
//   POST /ISAPI/Traffic/channels/1/vehicleDetect/plateInfo  — añadir placa
//   PUT  /ISAPI/Traffic/channels/1/vehicleDetect/plateInfo  — reemplazar lista completa
//   GET  /ISAPI/Traffic/channels/1/vehicleDetect/plateInfo  — leer lista actual
//   DEL  /ISAPI/Traffic/channels/1/vehicleDetect/plateInfo/{id}  — quitar placa
//
// Auth: HTTP Digest (estándar Hikvision). Los Hikvision modernos también
// aceptan Basic, pero Digest es más seguro y es el default factory.
type HikvisionAdapter struct {
	host     string
	port     int
	user     string
	password string
	http     *http.Client
}

func NewHikvisionAdapter(host string, port int, user, password string) *HikvisionAdapter {
	return &HikvisionAdapter{
		host:     host,
		port:     port,
		user:     user,
		password: password,
		http: &http.Client{
			Timeout: 20 * time.Second,
		},
	}
}

func (h *HikvisionAdapter) Name() string {
	return "hikvision"
}

func (h *HikvisionAdapter) Ping(ctx context.Context) error {
	// /ISAPI/System/deviceInfo retorna info del device (modelo, firmware).
	// Es el endpoint estándar para health-check en Hikvision.
	url := fmt.Sprintf("http://%s:%d/ISAPI/System/deviceInfo", h.host, h.port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := h.doWithDigest(req)
	if err != nil {
		return fmt.Errorf("ping a %s: %w", h.host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ping status %d", resp.StatusCode)
	}
	return nil
}

// SyncWhitelist envía la lista completa de placas a la cámara via PUT.
// Hikvision ISAPI v2 acepta XML payload con todas las placas — la cámara
// reemplaza su whitelist local atomicamente. Operación idempotente:
// si las placas no cambiaron, no produce side-effects visibles al portero.
func (h *HikvisionAdapter) SyncWhitelist(ctx context.Context, plates []Plate) error {
	xml := buildHikvisionPlatesXML(plates)
	url := fmt.Sprintf("http://%s:%d/ISAPI/Traffic/channels/1/vehicleDetect/plateInfo", h.host, h.port)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, strings.NewReader(xml))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/xml")
	req.Header.Set("Accept", "application/xml")

	resp, err := h.doWithDigest(req)
	if err != nil {
		return fmt.Errorf("PUT whitelist: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("PUT whitelist status %d: %s", resp.StatusCode, snippet)
	}
	return nil
}

func buildHikvisionPlatesXML(plates []Plate) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	b.WriteString(`<PlateInfoList version="2.0">`)
	for i, p := range plates {
		fmt.Fprintf(&b, `<PlateInfo><id>%d</id><plateNumber>%s</plateNumber>`, i+1, xmlEscape(p.Plate))
		// `plateType=0` = whitelist (allowed). `1` = blacklist en Hikvision.
		b.WriteString(`<plateType>0</plateType>`)
		if p.ValidUntil != "" {
			fmt.Fprintf(&b, `<effectivePeriod><endTime>%s</endTime></effectivePeriod>`, xmlEscape(p.ValidUntil))
		}
		b.WriteString(`</PlateInfo>`)
	}
	b.WriteString(`</PlateInfoList>`)
	return b.String()
}

func xmlEscape(s string) string {
	// Hikvision rara vez recibe chars problemáticos en placas (alfanuméricas)
	// pero por defensa: escapamos los 5 chars XML estándar.
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	)
	return r.Replace(s)
}

// doWithDigest ejecuta una request con HTTP Digest authentication.
// Hikvision soporta Basic Auth pero el factory default es Digest — más
// seguro y obligatorio en muchas instalaciones empresariales.
//
// El flow es:
//  1. Primera request sin auth → cámara retorna 401 con WWW-Authenticate header.
//  2. Parseamos el challenge (realm + nonce + algorithm).
//  3. Reenviamos con header `Authorization: Digest ...` calculado.
func (h *HikvisionAdapter) doWithDigest(req *http.Request) (*http.Response, error) {
	// Clonamos el body para poder re-enviarlo tras el 401 challenge.
	var bodyBytes []byte
	if req.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		req.ContentLength = int64(len(bodyBytes))
	}

	// Primera tentativa sin auth.
	resp, err := h.http.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil // OK o error real distinto a 401
	}

	// Parseamos el challenge del header WWW-Authenticate.
	authHeader := resp.Header.Get("WWW-Authenticate")
	resp.Body.Close()
	if !strings.HasPrefix(authHeader, "Digest ") {
		return nil, fmt.Errorf("cámara no devolvió Digest challenge: %q", authHeader)
	}
	challenge := parseDigestChallenge(authHeader)

	// Segunda tentativa con Authorization calculado.
	req2, err := http.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req2.Header = req.Header.Clone()
	req2.Header.Set("Authorization", h.buildDigestHeader(challenge, req.Method, req.URL.RequestURI()))

	return h.http.Do(req2)
}

func parseDigestChallenge(header string) map[string]string {
	result := map[string]string{}
	body := strings.TrimPrefix(header, "Digest ")
	for _, part := range splitDigestParts(body) {
		eq := strings.IndexByte(part, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(part[:eq])
		val := strings.Trim(strings.TrimSpace(part[eq+1:]), `"`)
		result[key] = val
	}
	return result
}

// splitDigestParts hace split por comas respetando valores entre comillas.
// El parsing custom es necesario porque `strings.Split(s, ",")` rompería
// valores que contienen comas (ej. realm con espacios y comas).
func splitDigestParts(s string) []string {
	var parts []string
	var current strings.Builder
	inQuotes := false
	for _, r := range s {
		if r == '"' {
			inQuotes = !inQuotes
			current.WriteRune(r)
			continue
		}
		if r == ',' && !inQuotes {
			parts = append(parts, strings.TrimSpace(current.String()))
			current.Reset()
			continue
		}
		current.WriteRune(r)
	}
	if current.Len() > 0 {
		parts = append(parts, strings.TrimSpace(current.String()))
	}
	return parts
}

func (h *HikvisionAdapter) buildDigestHeader(c map[string]string, method, uri string) string {
	realm := c["realm"]
	nonce := c["nonce"]
	qop := c["qop"]
	opaque := c["opaque"]
	algorithm := c["algorithm"]
	if algorithm == "" {
		algorithm = "MD5"
	}

	cnonce := randomHex(8)
	nc := "00000001"

	ha1 := md5hex(h.user + ":" + realm + ":" + h.password)
	ha2 := md5hex(method + ":" + uri)

	var response string
	if qop != "" {
		response = md5hex(ha1 + ":" + nonce + ":" + nc + ":" + cnonce + ":" + qop + ":" + ha2)
	} else {
		response = md5hex(ha1 + ":" + nonce + ":" + ha2)
	}

	var b strings.Builder
	fmt.Fprintf(&b, `Digest username="%s", realm="%s", nonce="%s", uri="%s", algorithm=%s, response="%s"`,
		h.user, realm, nonce, uri, algorithm, response)
	if qop != "" {
		fmt.Fprintf(&b, `, qop=%s, nc=%s, cnonce="%s"`, qop, nc, cnonce)
	}
	if opaque != "" {
		fmt.Fprintf(&b, `, opaque="%s"`, opaque)
	}
	return b.String()
}

func md5hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

func randomHex(bytes int) string {
	buf := make([]byte, bytes)
	rand.Read(buf) // Para cnonce no necesitamos CSPRNG — Hikvision lo trata como opaque token.
	return hex.EncodeToString(buf)
}
