package main

import (
	"context"
	"fmt"
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
// la línea ITC (y los firmwares viejos en general, según la guía «How to
// integrate with Hikvision LPR function via ISAPI» v1.0.0) tiene su propio
// módulo "Entrance":
//
//	PUT /ISAPI/ITC/Entrance/VCL  — reemplazar Vehicle Control List
//	GET /ISAPI/ITC/Entrance/VCL  — leer lista actual
//
// VCL = Vehicle Control List. En la web admin de la cámara está bajo
// "Configuration → Vehicle Recognition → Plate Management". La cámara
// dispara su relé (Alarm Out) cuando detecta una placa permitida,
// abriendo la talanquera sin pasar por el cloud.
//
// Estructura XML:
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

func (h *HikvisionITCAdapter) Name() string { return "hikvision_itc" }

// Capacidades: la VCL lleva `plateType` 0 = Allow, 1 = Deny, así que la
// lista negra viaja en el mismo PUT.
func (h *HikvisionITCAdapter) Capacidades() Capacidades {
	return Capacidades{Whitelist: true, Blocklist: true}
}

func (h *HikvisionITCAdapter) Ping(ctx context.Context) error {
	return pingHikvision(ctx, h.digest, h.host, h.port)
}

// SyncWhitelist empuja la VCL completa a la cámara ITC. La cámara la
// almacena en flash y opera 100% offline después. Idempotente.
func (h *HikvisionITCAdapter) SyncWhitelist(ctx context.Context, plates []Plate, blocked []Plate) error {
	xml := buildHikvisionITCVehicleListXML(plates, blocked)
	url := fmt.Sprintf("http://%s:%d/ISAPI/ITC/Entrance/VCL", h.host, h.port)
	return putXMLHikvision(ctx, h.digest, url, xml, "PUT VCL")
}

// buildHikvisionITCVehicleListXML construye el XML para la línea ITC.
// Diferencias vs Traffic:
//   - Raíz: <VehicleControlList> en lugar de <PlateInfoList>.
//   - Elementos: <Vehicle> en lugar de <PlateInfo>.
//   - Vigencia: <effectiveTime> con <enabled>, <beginTime>, <endTime>,
//     en lugar de <effectivePeriod><endTime>.
func buildHikvisionITCVehicleListXML(plates []Plate, blocked []Plate) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	b.WriteString(`<VehicleControlList version="2.0">`)
	id := 0
	escribir := func(p Plate, plateType int) {
		id++
		fmt.Fprintf(&b, `<Vehicle><id>%d</id><plateNumber>%s</plateNumber><plateType>%d</plateType>`, id, xmlEscape(p.Plate), plateType)
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
	for _, p := range plates {
		escribir(p, 0)
	}
	for _, p := range blocked {
		escribir(p, 1)
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
