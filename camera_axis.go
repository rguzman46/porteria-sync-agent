package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// AxisVapixAdapter integra con cámaras Axis vía VAPIX + el ACAP «AXIS License
// Plate Verifier», que debe estar instalado en la cámara con licencia activa.
//
// Modelos soportados (familia `axis_vapix`):
//   - P3265-LVE — bullet 2MP outdoor con LPR ACAP.
//   - Q1700-LE — box camera profesional para vías y parqueaderos.
//
// IMPORTANTE: Axis requiere comprar la licencia ACAP por separado. Sin ACAP,
// la cámara NO hace OCR de placas — solo es CCTV regular.
//
// API de listas del ACAP (fuente:
// https://developer.axis.com/vapix/applications/license-plate-verifier-api/):
//
//	GET  /axis-cgi/basicdeviceinfo.cgi                                   — info del device (health)
//	GET  /local/fflprapp/api.cgi?api=addplate&plate=<texto>,<fecha>,<desc>&list=<lista>
//	GET  /local/fflprapp/api.cgi?api=delplate&plate=<texto>&list=<lista>
//	GET  /local/fflprapp/api.cgi?api=export<lista>
//	POST /local/fflprapp/<lista>.cgi                                     — importación masiva
//
// Las listas son `allowlist` y `blocklist`. El endpoint `/local/lpv/.api`
// que usaba la versión anterior no aparece en esa documentación y se
// reemplazó.
//
// Estrategia de sincronización: `addplate` por cada placa que debe estar y
// `delplate` por cada una que estaba en el push anterior y ya no. Es
// idempotente por placa (la cámara acepta re-agregar) y usa solo llamadas
// documentadas. La conciliación completa contra lo que la cámara tiene de
// verdad necesita `export<lista>`, cuyo formato de salida hay que confirmar
// con la cámara al lado; hasta entonces, el primer push tras un reinicio del
// agente no borra lo que otro haya metido a mano.
//
// Auth: Digest por default. Mismo helper digestClient que Hikvision.
type AxisVapixAdapter struct {
	host     string
	port     int
	user     string
	password string
	digest   *digestClient

	mu       sync.Mutex
	anterior map[string]map[string]bool // lista → placas del último push exitoso
}

func NewAxisVapixAdapter(host string, port int, user, password string) *AxisVapixAdapter {
	httpClient := &http.Client{Timeout: 30 * time.Second}
	return &AxisVapixAdapter{
		host:     host,
		port:     port,
		user:     user,
		password: password,
		digest:   newDigestClient(user, password, httpClient),
		anterior: map[string]map[string]bool{},
	}
}

func (a *AxisVapixAdapter) Name() string { return "axis_vapix" }

// Capacidades: el ACAP tiene `allowlist` y `blocklist` con la misma API.
func (a *AxisVapixAdapter) Capacidades() Capacidades {
	return Capacidades{Whitelist: true, Blocklist: true}
}

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

// SyncWhitelist sincroniza `allowlist` y `blocklist` del ACAP.
func (a *AxisVapixAdapter) SyncWhitelist(ctx context.Context, plates []Plate, blocked []Plate) error {
	if err := a.sincronizarLista(ctx, "allowlist", plates); err != nil {
		return err
	}
	return a.sincronizarLista(ctx, "blocklist", blocked)
}

func (a *AxisVapixAdapter) sincronizarLista(ctx context.Context, lista string, deseadas []Plate) error {
	a.mu.Lock()
	previas := a.anterior[lista]
	a.mu.Unlock()

	actuales := make(map[string]bool, len(deseadas))
	fallos := 0
	var ultimo error
	for _, p := range deseadas {
		actuales[p.Plate] = true
		if err := a.addplate(ctx, lista, p); err != nil {
			fallos++
			ultimo = err
			if fallos > 10 {
				return fmt.Errorf("demasiados fallos en addplate (%d) en %s: última: %w", fallos, lista, err)
			}
		}
	}
	for placa := range previas {
		if actuales[placa] {
			continue
		}
		if err := a.delplate(ctx, lista, placa); err != nil {
			fallos++
			ultimo = err
		}
	}
	if fallos > 0 {
		return fmt.Errorf("%d placas no se pudieron sincronizar en %s de Axis (de %d): %w", fallos, lista, len(deseadas), ultimo)
	}
	a.mu.Lock()
	a.anterior[lista] = actuales
	a.mu.Unlock()
	return nil
}

// addplate agrega una placa. El campo `<fecha>` del API es la vigencia; se
// deja vacío a propósito: su formato no está confirmado y el cloud ya
// excluye lo vencido, así que la placa sale de la cámara en el siguiente
// push por `delplate`.
func (a *AxisVapixAdapter) addplate(ctx context.Context, lista string, p Plate) error {
	desc := truncateStr(strings.ReplaceAll(p.Owner, ",", " "), 64)
	q := url.Values{}
	q.Set("api", "addplate")
	q.Set("plate", p.Plate+",,"+desc)
	q.Set("list", lista)
	return a.llamarAPI(ctx, "addplate", q)
}

func (a *AxisVapixAdapter) delplate(ctx context.Context, lista, placa string) error {
	q := url.Values{}
	q.Set("api", "delplate")
	q.Set("plate", placa)
	q.Set("list", lista)
	return a.llamarAPI(ctx, "delplate", q)
}

func (a *AxisVapixAdapter) llamarAPI(ctx context.Context, que string, q url.Values) error {
	u := fmt.Sprintf("http://%s:%d/local/fflprapp/api.cgi?%s", a.host, a.port, q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := a.digest.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("ACAP License Plate Verifier no instalado en la cámara (404 en /local/fflprapp/). Instálalo desde Apps en la web admin antes de usar este adapter")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s status %d: %s", que, resp.StatusCode, respBody)
	}
	// El ACAP responde 200 también cuando rechaza; el cuerpo lo dice.
	if strings.Contains(strings.ToLower(string(respBody)), "error") {
		return fmt.Errorf("%s rechazado por el ACAP: %s", que, strings.TrimSpace(string(respBody)))
	}
	return nil
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
