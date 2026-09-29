package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HikvisionTrafficAdapter integra con la línea Traffic de cámaras Hikvision
// vía API ISAPI (Intelligent Security API). Documentación oficial:
// https://www.hikvision.com/en/support/download/sdk/
//
// Modelos soportados (familia `hikvision_traffic`):
//   - iDS-2CD7A26G0/P-IZHS — bullet 4MP con LPR built-in.
//   - iDS-TCM403-MA / iDS-TCM203-A — cámaras profesionales de tráfico.
//   - DS-2CD7A85G0-LPR — 8MP profesional para parqueaderos comerciales.
//
// Endpoints relevantes para LPR Traffic:
//
//	PUT  /ISAPI/Traffic/channels/1/vehicleDetect/plateInfo       — reemplazar lista completa
//	GET  /ISAPI/Traffic/channels/1/vehicleDetect/plateInfo       — leer lista actual
//
// Los firmwares recientes (p. ej. 7A26) exponen además
// `licensePlateAuditData` con permitidas y negadas en un solo archivo: ver
// `camera_hikvision_audit.go` (familia `hikvision_anpr_audit`). Para la
// línea ITC Entrance (DS-TCG*) ver `camera_hikvision_itc.go`. Cuál de los
// tres acepta una cámara concreta se confirma con la cámara al lado
// (README, «Hikvision: tres endpoints»).
//
// Auth: HTTP Digest (estándar Hikvision).
type HikvisionTrafficAdapter struct {
	host     string
	port     int
	user     string
	password string
	digest   *digestClient
}

func NewHikvisionTrafficAdapter(host string, port int, user, password string) *HikvisionTrafficAdapter {
	httpClient := &http.Client{Timeout: 20 * time.Second}
	return &HikvisionTrafficAdapter{
		host:     host,
		port:     port,
		user:     user,
		password: password,
		digest:   newDigestClient(user, password, httpClient),
	}
}

func (h *HikvisionTrafficAdapter) Name() string { return "hikvision_traffic" }

// Capacidades: `plateType` distingue permitida (0) de negada (1) en la misma
// lista, así que las dos viajan en un solo PUT.
func (h *HikvisionTrafficAdapter) Capacidades() Capacidades {
	return Capacidades{Whitelist: true, Blocklist: true}
}

func (h *HikvisionTrafficAdapter) Ping(ctx context.Context) error {
	return pingHikvision(ctx, h.digest, h.host, h.port)
}

// pingHikvision usa /ISAPI/System/deviceInfo, presente en TODAS las cámaras
// Hikvision: el health-check estándar. Compartido por las tres familias.
func pingHikvision(ctx context.Context, digest *digestClient, host string, port int) error {
	url := fmt.Sprintf("http://%s:%d/ISAPI/System/deviceInfo", host, port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := digest.Do(req)
	if err != nil {
		return fmt.Errorf("ping a %s: %w", host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ping status %d", resp.StatusCode)
	}
	return nil
}

// SyncWhitelist envía la lista completa de placas a la cámara via PUT.
// Hikvision ISAPI v2 acepta XML payload con todas las placas — la cámara
// reemplaza su lista local atómicamente. Operación idempotente.
func (h *HikvisionTrafficAdapter) SyncWhitelist(ctx context.Context, plates []Plate, blocked []Plate) error {
	xml := buildHikvisionTrafficPlatesXML(plates, blocked)
	url := fmt.Sprintf("http://%s:%d/ISAPI/Traffic/channels/1/vehicleDetect/plateInfo", h.host, h.port)
	return putXMLHikvision(ctx, h.digest, url, xml, "PUT whitelist")
}

// putXMLHikvision hace el PUT y traduce un status distinto de 200 a error
// con el principio del cuerpo, que es donde la cámara dice por qué.
func putXMLHikvision(ctx context.Context, digest *digestClient, url, xml, que string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, strings.NewReader(xml))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/xml")
	req.Header.Set("Accept", "application/xml")

	resp, err := digest.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", que, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s status %d: %s", que, resp.StatusCode, snippet)
	}
	return nil
}

// buildHikvisionTrafficPlatesXML construye el XML para la línea Traffic.
// Estructura raíz `<PlateInfoList version="2.0">` con elementos `<PlateInfo>`.
// `plateType` 0 = permitida (whitelist), 1 = negada (blacklist).
func buildHikvisionTrafficPlatesXML(plates []Plate, blocked []Plate) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	b.WriteString(`<PlateInfoList version="2.0">`)
	id := 0
	escribir := func(p Plate, plateType int) {
		id++
		fmt.Fprintf(&b, `<PlateInfo><id>%d</id><plateNumber>%s</plateNumber><plateType>%d</plateType>`, id, xmlEscape(p.Plate), plateType)
		if p.ValidUntil != "" {
			fmt.Fprintf(&b, `<effectivePeriod><endTime>%s</endTime></effectivePeriod>`, xmlEscape(p.ValidUntil))
		}
		b.WriteString(`</PlateInfo>`)
	}
	for _, p := range plates {
		escribir(p, 0)
	}
	for _, p := range blocked {
		escribir(p, 1)
	}
	b.WriteString(`</PlateInfoList>`)
	return b.String()
}

// xmlEscape escapa los 5 chars XML estándar. Compartido por todos los
// adapters XML.
func xmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	)
	return r.Replace(s)
}
