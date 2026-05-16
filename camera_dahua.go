package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DahuaITCAdapter integra con la línea Intelligent Traffic Camera de Dahua.
//
// Modelos soportados (familia `dahua_itc`):
//   - ITC215-PW6M-IRLZF — 2MP entrada/salida con IR.
//   - ITC237-PU1B-IRZF — 2MP profesional.
//   - ITC413-PW4D-IZ   — 4MP con AI integrado.
//
// Dahua usa CGI API (no ISAPI como Hikvision). Endpoints relevantes:
//
//	POST /cgi-bin/recordUpdater.cgi?action=insert&name=AccessControlCardList
//	POST /cgi-bin/recordUpdater.cgi?action=remove&name=AccessControlCardList
//	GET  /cgi-bin/magicBox.cgi?action=getSystemInfo
//
// Para LPR / Vehicle Access Control específicamente:
//
//	POST /cgi-bin/recordUpdater.cgi?action=insert&name=TrafficSnapshot
//	      → body con plate, validFrom, validUntil
//
// Auth: HTTP Digest (Dahua moderno, firmware >= 2.700). Algunos firmwares
// más viejos requieren Basic — para esos hay que migrar firmware o cambiar
// auth en config web de la cámara.
//
// Diferencia crítica vs Hikvision: Dahua NO soporta replace-all atómico.
// Cada placa se inserta/actualiza individualmente. Estrategia:
//  1. GET la lista actual.
//  2. Diff vs la lista del cloud (set difference).
//  3. INSERT placas nuevas + REMOVE placas eliminadas.
//
// Para simplificar V1: hacemos "wipe + insert all" — más simple pero menos
// eficiente para conjuntos grandes (1000+ placas). Optimizar a diff cuando
// el primer piloto reporte tiempos de sync > 30s.
type DahuaITCAdapter struct {
	host     string
	port     int
	user     string
	password string
	digest   *digestClient
}

func NewDahuaITCAdapter(host string, port int, user, password string) *DahuaITCAdapter {
	httpClient := &http.Client{Timeout: 30 * time.Second}
	return &DahuaITCAdapter{
		host:     host,
		port:     port,
		user:     user,
		password: password,
		digest:   newDigestClient(user, password, httpClient),
	}
}

func (d *DahuaITCAdapter) Name() string { return "dahua_itc" }

func (d *DahuaITCAdapter) Ping(ctx context.Context) error {
	// /cgi-bin/magicBox.cgi?action=getSystemInfo es el endpoint canónico
	// para health-check en cámaras Dahua. Retorna texto plano con
	// `deviceType=...`, `serialNumber=...`, etc.
	u := fmt.Sprintf("http://%s:%d/cgi-bin/magicBox.cgi?action=getSystemInfo", d.host, d.port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := d.digest.Do(req)
	if err != nil {
		return fmt.Errorf("ping a %s: %w", d.host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ping status %d", resp.StatusCode)
	}
	return nil
}

// SyncWhitelist empuja la lista completa de placas. Estrategia V1: clear
// + insert all (más simple que diff). Para conjuntos grandes optimizar
// a diff en V2.
func (d *DahuaITCAdapter) SyncWhitelist(ctx context.Context, plates []Plate) error {
	// 1. Clear: borramos toda la lista actual.
	if err := d.clearAllPlates(ctx); err != nil {
		// Si el clear falla, log warning pero seguimos — el insert puede
		// generar duplicados que la cámara tolera (idempotente por plateNumber).
		// Si fuera una falla de red real, el siguiente insert también fallará.
		return fmt.Errorf("clear previo a sync: %w", err)
	}

	// 2. Insert: una request por placa. La cámara Dahua no soporta bulk
	// nativo — el batch es secuencial. Si una placa falla, log + continúa
	// (no abortamos toda la sync por una placa rota).
	failed := 0
	for _, p := range plates {
		if err := d.insertPlate(ctx, p); err != nil {
			failed++
			// Log pero continúa con la siguiente
			if failed > 10 {
				return fmt.Errorf("demasiados fallos al insertar placas (%d): última: %w", failed, err)
			}
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d placas no se pudieron insertar (de %d totales)", failed, len(plates))
	}
	return nil
}

func (d *DahuaITCAdapter) clearAllPlates(ctx context.Context) error {
	u := fmt.Sprintf("http://%s:%d/cgi-bin/recordUpdater.cgi?action=clear&name=AccessControlCardList", d.host, d.port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := d.digest.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	// Dahua responde "OK" en text/plain en success, o "Error" + descripción
	// en failure. 200 sin "OK" es poco común pero puede pasar con firmwares
	// no estándar — toleramos.
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("clear status %d: %s", resp.StatusCode, body)
	}
	return nil
}

func (d *DahuaITCAdapter) insertPlate(ctx context.Context, p Plate) error {
	// Dahua CGI con action=insert toma los campos del record en la query string
	// o como x-www-form-urlencoded. Formato POST es más robusto para placas
	// largas o caracteres no-ASCII.
	form := url.Values{}
	form.Set("action", "insert")
	form.Set("name", "AccessControlCardList")
	form.Set("CardName", truncate(p.Owner, 31))
	form.Set("CardNo", p.Plate) // Dahua usa CardNo para representar placas en Access Control
	form.Set("CardStatus", "0") // 0 = activo / allow
	form.Set("CardType", "0")   // 0 = general
	if p.ValidFrom != "" {
		form.Set("StartTime", normalizeDahuaTime(p.ValidFrom))
	}
	if p.ValidUntil != "" {
		form.Set("EndTime", normalizeDahuaTime(p.ValidUntil))
	}

	u := fmt.Sprintf("http://%s:%d/cgi-bin/recordUpdater.cgi", d.host, d.port)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := d.digest.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("insert %q status %d: %s", p.Plate, resp.StatusCode, body)
	}
	if strings.Contains(strings.ToLower(string(body)), "error") {
		return fmt.Errorf("insert %q rechazada: %s", p.Plate, body)
	}
	return nil
}

// normalizeDahuaTime convierte ISO 8601 a formato Dahua: `YYYY-MM-DD HH:MM:SS`.
// La cámara rechaza con error parsing si recibe T separator o Z suffix.
func normalizeDahuaTime(ts string) string {
	t := strings.TrimSpace(ts)
	t = strings.ReplaceAll(t, "T", " ")
	// Recortar Z y offsets de timezone.
	for _, suffix := range []string{"Z"} {
		t = strings.TrimSuffix(t, suffix)
	}
	if idx := strings.LastIndexAny(t, "+-"); idx > 10 {
		// "2026-05-15 10:00:00+00:00" → recortar offset
		t = t[:idx]
	}
	return strings.TrimSpace(t)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
