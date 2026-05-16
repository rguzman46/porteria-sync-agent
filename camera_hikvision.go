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
//	POST /ISAPI/Traffic/channels/1/vehicleDetect/plateInfo       — añadir placa
//	PUT  /ISAPI/Traffic/channels/1/vehicleDetect/plateInfo       — reemplazar lista completa
//	GET  /ISAPI/Traffic/channels/1/vehicleDetect/plateInfo       — leer lista actual
//	DEL  /ISAPI/Traffic/channels/1/vehicleDetect/plateInfo/{id}  — quitar placa
//
// Para la línea ITC Entrance (DS-TCG*) ver `camera_hikvision_itc.go` — usa
// un endpoint distinto (`/ISAPI/ITC/Entrance/VCL`) y formato XML levemente
// diferente.
//
// Auth: HTTP Digest (estándar Hikvision). Los Hikvision modernos también
// aceptan Basic, pero Digest es más seguro y es el default factory.
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

func (h *HikvisionTrafficAdapter) Name() string {
	return "hikvision_traffic"
}

func (h *HikvisionTrafficAdapter) Ping(ctx context.Context) error {
	// /ISAPI/System/deviceInfo retorna info del device (modelo, firmware).
	// Es el endpoint estándar para health-check en Hikvision.
	url := fmt.Sprintf("http://%s:%d/ISAPI/System/deviceInfo", h.host, h.port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := h.digest.Do(req)
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
func (h *HikvisionTrafficAdapter) SyncWhitelist(ctx context.Context, plates []Plate) error {
	xml := buildHikvisionTrafficPlatesXML(plates)
	url := fmt.Sprintf("http://%s:%d/ISAPI/Traffic/channels/1/vehicleDetect/plateInfo", h.host, h.port)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, strings.NewReader(xml))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/xml")
	req.Header.Set("Accept", "application/xml")

	resp, err := h.digest.Do(req)
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

// buildHikvisionTrafficPlatesXML construye el XML para la línea Traffic.
// Estructura raíz `<PlateInfoList version="2.0">` con elementos `<PlateInfo>`.
func buildHikvisionTrafficPlatesXML(plates []Plate) string {
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

// xmlEscape escapa los 5 chars XML estándar. Compartido por todos los
// adapters XML (Hikvision Traffic, Hikvision ITC).
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
