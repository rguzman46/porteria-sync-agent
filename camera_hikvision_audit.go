package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// HikvisionAuditAdapter escribe la lista en los firmwares Hikvision recientes
// (p. ej. iDS-2CD7A26G0) que ya no exponen `vehicleDetect/plateInfo` ni
// `ITC/Entrance/VCL`, sino un solo archivo con permitidas y negadas:
//
//	GET /ISAPI/Traffic/channels/1/licensePlateAuditData?fileType=xml
//	PUT /ISAPI/Traffic/channels/1/licensePlateAuditData?fileType=xml
//
// Fuentes: guía «How to integrate with Hikvision LPR function via ISAPI»
// v1.0.0 (download.viakom.cz/HIKVISION/SDK/) e hilo
// ipcamtalk.com/threads/get-passed-license-plate-numbers-using-isapi.79032/.
//
// **Por confirmar con la cámara al lado**: el endpoint está documentado; el
// cuerpo de abajo (`LPListAuditData` con `listType` whiteList/blackList y
// vigencia por fecha) sale de la misma guía, pero no se ha validado contra
// un firmware físico. Si la cámara responde 400, el primer sitio a mirar es
// `GET …?fileType=xml`: lo que devuelva es el formato exacto que acepta, y
// la única función que hay que ajustar es `buildHikvisionAuditXML`.
type HikvisionAuditAdapter struct {
	host     string
	port     int
	user     string
	password string
	digest   *digestClient
}

func NewHikvisionAuditAdapter(host string, port int, user, password string) *HikvisionAuditAdapter {
	httpClient := &http.Client{Timeout: 20 * time.Second}
	return &HikvisionAuditAdapter{
		host:     host,
		port:     port,
		user:     user,
		password: password,
		digest:   newDigestClient(user, password, httpClient),
	}
}

func (h *HikvisionAuditAdapter) Name() string { return "hikvision_anpr_audit" }

// Capacidades: el archivo lleva `listType`, así que negadas y permitidas
// van juntas.
func (h *HikvisionAuditAdapter) Capacidades() Capacidades {
	return Capacidades{Whitelist: true, Blocklist: true}
}

func (h *HikvisionAuditAdapter) Ping(ctx context.Context) error {
	return pingHikvision(ctx, h.digest, h.host, h.port)
}

func (h *HikvisionAuditAdapter) SyncWhitelist(ctx context.Context, plates []Plate, blocked []Plate) error {
	xml := buildHikvisionAuditXML(plates, blocked)
	url := fmt.Sprintf("http://%s:%d/ISAPI/Traffic/channels/1/licensePlateAuditData?fileType=xml", h.host, h.port)
	return putXMLHikvision(ctx, h.digest, url, xml, "PUT licensePlateAuditData")
}

// buildHikvisionAuditXML arma el archivo de auditoría de placas. La vigencia
// en este formato es por día (`effectiveStartDate` / `effectiveTime`, sin
// hora): la cámara no acepta más fino y el cloud ya excluye lo vencido.
func buildHikvisionAuditXML(plates []Plate, blocked []Plate) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	b.WriteString(`<LPListAuditData version="2.0">`)
	id := 0
	escribir := func(p Plate, listType string) {
		id++
		fmt.Fprintf(&b, `<LPListAuditData><id>%d</id><LicensePlate>%s</LicensePlate><listType>%s</listType>`,
			id, xmlEscape(p.Plate), listType)
		inicio := soloFecha(p.ValidFrom, "1970-01-01")
		fin := soloFecha(p.ValidUntil, "2099-12-31")
		fmt.Fprintf(&b, `<effectiveStartDate>%s</effectiveStartDate><effectiveTime>%s</effectiveTime></LPListAuditData>`,
			xmlEscape(inicio), xmlEscape(fin))
	}
	for _, p := range plates {
		escribir(p, "whiteList")
	}
	for _, p := range blocked {
		escribir(p, "blackList")
	}
	b.WriteString(`</LPListAuditData>`)
	return b.String()
}

// soloFecha recorta un ISO 8601 a `YYYY-MM-DD`; vacío → el default.
func soloFecha(ts, def string) string {
	t := strings.TrimSpace(ts)
	if len(t) < 10 {
		return def
	}
	return t[:10]
}
