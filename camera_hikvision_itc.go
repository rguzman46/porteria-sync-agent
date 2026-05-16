package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HikvisionITCAdapter integra con la línea ITC Entrance Control de Hikvision.
//
// Modelos soportados (familia `hikvision_itc`):
//   - DS-TCG405-E / DS-TCG405-E/H — 4MP entrada/salida residencial, PoE.
//   - DS-TCG411-E — variante con mayor alcance.
//   - DS-TCG615-EI — 6MP, AI integrado, mejor rendimiento nocturno.
//
// Diferencia clave vs línea Traffic: el endpoint ISAPI es distinto.
// Mientras Traffic usa `/ISAPI/Traffic/channels/1/vehicleDetect/plateInfo`,
// la línea ITC tiene su propio módulo "Entrance" con endpoint:
//
//	PUT /ISAPI/ITC/Entrance/VCL  — reemplazar Vehicle Control List
//	GET /ISAPI/ITC/Entrance/VCL  — leer lista actual
//
// VCL = Vehicle Control List. En la web admin de la cámara está bajo
// "Configuration → Vehicle Recognition → Plate Management" → modo "Allow List".
// La cámara dispara su relé (Alarm Out) cuando detecta una placa en VCL,
// permitiendo apertura local de talanquera sin pasar por el cloud.
//
// Estructura XML (validada con DS-TCG405-E firmware V5.7+):
//
//	<VehicleControlList version="2.0">
//	  <Vehicle>
//	    <id>1</id>
//	    <plateNumber>ABC123</plateNumber>
//	    <plateType>0</plateType>      <!-- 0=Allow, 1=Deny -->
//	    <effectiveTime>
//	      <enabled>true</enabled>
//	      <beginTime>2026-05-15T00:00:00</beginTime>
//	      <endTime>2026-12-31T23:59:59</endTime>
//	    </effectiveTime>
//	  </Vehicle>
//	  ...
//	</VehicleControlList>
//
// Auth: HTTP Digest (mismo que la línea Traffic — comparte digestClient).
type HikvisionITCAdapter struct {
	host     string
	port     int
	user     string
	password string
	digest   *digestClient
}

func NewHikvisionITCAdapter(host string, port int, user, password string) *HikvisionITCAdapter {
	httpClient := &http.Client{Timeout: 20 * time.Second}
	return &HikvisionITCAdapter{
		host:     host,
		port:     port,
		user:     user,
		password: password,
		digest:   newDigestClient(user, password, httpClient),
	}
}

func (h *HikvisionITCAdapter) Name() string {
	return "hikvision_itc"
}

func (h *HikvisionITCAdapter) Ping(ctx context.Context) error {
	// /ISAPI/System/deviceInfo está presente en TODAS las cámaras Hikvision —
	// usado como health-check estándar también en la línea ITC.
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

// SyncWhitelist empuja la VCL completa a la cámara ITC. La cámara la
// almacena en flash y opera 100% offline después (decide aperturas locales
// sin internet). Operación idempotente — si las placas no cambiaron, la
// cámara no resetea su estado.
func (h *HikvisionITCAdapter) SyncWhitelist(ctx context.Context, plates []Plate) error {
	xml := buildHikvisionITCVehicleListXML(plates)
	url := fmt.Sprintf("http://%s:%d/ISAPI/ITC/Entrance/VCL", h.host, h.port)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, strings.NewReader(xml))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/xml")
	req.Header.Set("Accept", "application/xml")

	resp, err := h.digest.Do(req)
	if err != nil {
		return fmt.Errorf("PUT VCL: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("PUT VCL status %d: %s", resp.StatusCode, snippet)
	}
	return nil
}

// buildHikvisionITCVehicleListXML construye el XML para la línea ITC.
// Diferencias vs Traffic:
//   - Raíz: <VehicleControlList> en lugar de <PlateInfoList>.
//   - Elementos: <Vehicle> en lugar de <PlateInfo>.
//   - Vigencia: <effectiveTime> con <enabled>, <beginTime>, <endTime>,
//     en lugar de <effectivePeriod><endTime>.
func buildHikvisionITCVehicleListXML(plates []Plate) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	b.WriteString(`<VehicleControlList version="2.0">`)
	for i, p := range plates {
		fmt.Fprintf(&b, `<Vehicle><id>%d</id><plateNumber>%s</plateNumber>`, i+1, xmlEscape(p.Plate))
		// `plateType=0` = Allow (whitelist). `1` = Deny (blacklist).
		b.WriteString(`<plateType>0</plateType>`)
		// La línea ITC requiere effectiveTime con enabled+begin+end juntos.
		// Si no hay vigencia explícita del cloud, usamos vigencia permanente
		// (1970..2099). Cámara igual la deja activa indefinidamente.
		begin := p.ValidFrom
		if begin == "" {
			begin = "1970-01-01T00:00:00"
		} else {
			begin = trimZuluForITC(begin)
		}
		end := p.ValidUntil
		if end == "" {
			end = "2099-12-31T23:59:59"
		} else {
			end = trimZuluForITC(end)
		}
		fmt.Fprintf(&b,
			`<effectiveTime><enabled>true</enabled><beginTime>%s</beginTime><endTime>%s</endTime></effectiveTime>`,
			xmlEscape(begin), xmlEscape(end),
		)
		b.WriteString(`</Vehicle>`)
	}
	b.WriteString(`</VehicleControlList>`)
	return b.String()
}

// trimZuluForITC normaliza ISO8601 con offset / Z al formato que aceptan
// las cámaras ITC: `YYYY-MM-DDTHH:MM:SS` sin sufijo Z ni offset (la cámara
// asume su zona horaria local configurada). Si la entrada ya está en ese
// formato, la devuelve sin cambios.
func trimZuluForITC(ts string) string {
	// "2026-05-15T10:00:00Z" o "2026-05-15T10:00:00+00:00" → recortar después del segundo
	t := strings.TrimSpace(ts)
	if idx := strings.Index(t, "T"); idx > 0 && len(t) > idx+9 {
		// "T" + "HH:MM:SS" = 9 chars; cortar ahí
		t = t[:idx+9]
	}
	return t
}
