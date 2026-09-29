package main

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// DahuaITCAdapter integra con la línea Intelligent Traffic Camera de Dahua.
//
// Modelos (familia `dahua_itc`):
//   - ITC215-PW6M-IRLZF — 2MP entrada/salida con IR.
//   - ITC237-PU1B-IRZF — 2MP profesional.
//   - ITC413-PW4D-IZ   — 4MP con AI integrado.
//
// Lo que SÍ funciona con esta familia es la recepción de lecturas: la cámara
// hace `POST /NotificationInfo/TollgateInfo` al receptor del agente (ver
// `adapter_dahua_json.go`).
//
// Lo que NO está confirmado es la **escritura de la lista**. La versión
// anterior escribía en `recordUpdater.cgi?name=AccessControlCardList`, que es
// la tabla de **tarjetas** del control de acceso, no la de placas: la cámara
// la aceptaba y no la usaba para abrir. Se quitó. El candidato documentado
// en la «Dahua Intelligent Traffic HTTP API» es
// `recordUpdater.cgi?action=insert&name=TrafficRedList` (permitidas) /
// `TrafficBlackList` (negadas), pero sin una cámara al lado para confirmar
// nombres de campos no se escribe: por eso `Capacidades()` reporta ambas en
// `false`, el agente no intenta el push y el panel lo muestra.
//
// Auth: HTTP Digest (Dahua moderno, firmware >= 2.700).
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

// Capacidades: ver el comentario del tipo. Ninguna lista se escribe hasta
// confirmar el endpoint con una cámara física.
func (d *DahuaITCAdapter) Capacidades() Capacidades {
	return Capacidades{Whitelist: false, Blocklist: false}
}

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

// SyncWhitelist no escribe nada: el endpoint de lista de placas no está
// confirmado. El Syncer no lo llama cuando Capacidades().Whitelist es
// false; si alguien lo llama igual, el error lo dice.
func (d *DahuaITCAdapter) SyncWhitelist(ctx context.Context, plates []Plate, blocked []Plate) error {
	return fmt.Errorf("dahua_itc: la escritura de la lista de placas no está confirmada en esta versión (ver camera_dahua.go)")
}
