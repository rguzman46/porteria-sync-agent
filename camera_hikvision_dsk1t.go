package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	_ "time/tzdata" // Windows no trae la base de husos: sin esto no hay America/Bogota.
)

// HikvisionDSK1TAdapter carga los pases en los terminales faciales Hikvision
// que leen QR (familias `hikvision_dsk1t_qr_camara` y `hikvision_dsk1t_qr_modulo`
// del catálogo de la plataforma; ADR-130).
//
// El terminal guarda el QR como **número de tarjeta** de una persona y decide
// solo: el visitante muestra el QR, el terminal busca ese número y abre por
// su relé. Por eso cada pase es una persona en el equipo, con su vigencia
// (`Valid`) y una tarjeta con el código del pase.
//
// **Solo toca lo suyo.** Las personas que crea llevan el prefijo de pases
// (`pp`) en `employeeNo`, y solo esas se modifican o se borran: los
// residentes que el conjunto cargó con su rostro no se tocan nunca.
//
// ISAPI con Digest, por la red del conjunto:
//
//	POST /ISAPI/AccessControl/UserInfo/Search?format=json   — las personas del agente
//	POST /ISAPI/AccessControl/UserInfo/Record?format=json   — alta del pase
//	PUT  /ISAPI/AccessControl/UserInfo/Modify?format=json   — cambio de vigencia
//	POST /ISAPI/AccessControl/CardInfo/Record?format=json   — el código como tarjeta
//	PUT  /ISAPI/AccessControl/UserInfoDetail/Delete?format=json — el pase que ya no va
//
// ⚠️ Las llamadas salen de la wiki ISAPI. Lo que solo confirma el equipo
// —que la vigencia se lea en hora local, que un QR nuestro abra como la
// tarjeta— está en `docs/investigacion-hikvision-terminales-qr.md` §9 de la
// plataforma. Hasta el piloto la familia queda en `beta`.
type HikvisionDSK1TAdapter struct {
	familia string
	base    string
	digest  *digestClient
	zona    *time.Location
}

func NewHikvisionDSK1TAdapter(familia, host string, port int, user, password string) *HikvisionDSK1TAdapter {
	zona, err := time.LoadLocation("America/Bogota")
	if err != nil {
		// Con `time/tzdata` embebido no pasa; si pasara, Bogotá no tiene
		// horario de verano y UTC-5 fijo es la misma hora.
		zona = time.FixedZone("COT", -5*60*60)
	}
	return &HikvisionDSK1TAdapter{
		familia: familia,
		base:    fmt.Sprintf("http://%s:%d", host, port),
		digest:  newDigestClient(user, password, &http.Client{Timeout: 20 * time.Second}),
		zona:    zona,
	}
}

func (h *HikvisionDSK1TAdapter) Name() string { return h.familia }

// Capacidades: no es una cámara de placas. Escribe pases.
func (h *HikvisionDSK1TAdapter) Capacidades() Capacidades {
	return Capacidades{Credenciales: true}
}

func (h *HikvisionDSK1TAdapter) Ping(ctx context.Context) error {
	_, err := h.pedir(ctx, http.MethodGet, "/ISAPI/System/deviceInfo", nil)
	return err
}

// SyncWhitelist no hace nada: un terminal de puerta peatonal no lee placas.
func (h *HikvisionDSK1TAdapter) SyncWhitelist(context.Context, []Plate, []Plate) error {
	return nil
}

// pasoDelEquipo es una persona del agente tal como está en el terminal.
type pasoDelEquipo struct {
	EmployeeNo string `json:"employeeNo"`
	Valid      struct {
		BeginTime string `json:"beginTime"`
		EndTime   string `json:"endTime"`
	} `json:"Valid"`
}

// SyncCredentials deja en el terminal exactamente los pases de la lista:
// agrega los nuevos, corrige la vigencia de los que cambiaron y borra los
// que ya no van. Idempotente: con la misma lista no escribe nada.
func (h *HikvisionDSK1TAdapter) SyncCredentials(ctx context.Context, pases []Credential) error {
	enEquipo, err := h.pasesDelEquipo(ctx)
	if err != nil {
		return fmt.Errorf("leyendo los pases del terminal: %w", err)
	}
	quedan := map[string]bool{}
	for _, pase := range pases {
		if pase.Type != "qr" || pase.Credential == "" {
			continue
		}
		persona := personaDelPase(pase.Holder)
		if persona == "" {
			continue
		}
		quedan[persona] = true
		desde, hasta := h.vigencia(pase)
		actual, existe := enEquipo[persona]
		switch {
		case !existe:
			if err := h.alta(ctx, persona, pase, desde, hasta); err != nil {
				return err
			}
		case actual.Valid.BeginTime != desde || actual.Valid.EndTime != hasta:
			if err := h.enviar(ctx, http.MethodPut, "/ISAPI/AccessControl/UserInfo/Modify?format=json",
				map[string]any{"UserInfo": h.persona(persona, pase, desde, hasta)}); err != nil {
				return fmt.Errorf("cambiando la vigencia de %s: %w", persona, err)
			}
		}
	}
	var sobran []map[string]string
	for persona := range enEquipo {
		if !quedan[persona] {
			sobran = append(sobran, map[string]string{"employeeNo": persona})
		}
	}
	if len(sobran) > 0 {
		cuerpo := map[string]any{"UserInfoDetail": map[string]any{"mode": "byEmployeeNo", "EmployeeNoList": sobran}}
		if err := h.enviar(ctx, http.MethodPut, "/ISAPI/AccessControl/UserInfoDetail/Delete?format=json", cuerpo); err != nil {
			return fmt.Errorf("borrando %d pases que ya no van: %w", len(sobran), err)
		}
	}
	return nil
}

func (h *HikvisionDSK1TAdapter) alta(ctx context.Context, persona string, pase Credential, desde, hasta string) error {
	if err := h.enviar(ctx, http.MethodPost, "/ISAPI/AccessControl/UserInfo/Record?format=json",
		map[string]any{"UserInfo": h.persona(persona, pase, desde, hasta)}); err != nil {
		return fmt.Errorf("creando el pase %s: %w", persona, err)
	}
	tarjeta := map[string]any{"CardInfo": map[string]any{
		"employeeNo": persona,
		"cardNo":     pase.Credential,
		"cardType":   "normalCard",
	}}
	if err := h.enviar(ctx, http.MethodPost, "/ISAPI/AccessControl/CardInfo/Record?format=json", tarjeta); err != nil {
		return fmt.Errorf("cargando el código del pase %s: %w", persona, err)
	}
	return nil
}

// persona arma el UserInfo de un pase. Sin nombre ni documento del visitante
// (Ley 1581): el terminal solo necesita el número para abrir, y en su
// pantalla sale «Visitante» con los últimos cuatro dígitos.
func (h *HikvisionDSK1TAdapter) persona(persona string, pase Credential, desde, hasta string) map[string]any {
	sufijo := pase.Credential
	if len(sufijo) > 4 {
		sufijo = sufijo[len(sufijo)-4:]
	}
	return map[string]any{
		"employeeNo": persona,
		"name":       "Visitante " + sufijo,
		"userType":   "normal",
		"Valid": map[string]any{
			"enable":    true,
			"beginTime": desde,
			"endTime":   hasta,
			"timeType":  "local",
		},
	}
}

// vigencia convierte la del pase —UTC sin zona, como la manda la nube— a la
// hora de pared de Bogotá, que es la que lee el terminal. Sin fecha de inicio
// el pase vale desde ya; sin fin, hasta el tope del equipo.
func (h *HikvisionDSK1TAdapter) vigencia(pase Credential) (string, string) {
	const formato = "2006-01-02T15:04:05"
	local := func(utc string, siFalta string) string {
		t, err := time.Parse(formato, recortarAlSegundo(utc))
		if err != nil {
			return siFalta
		}
		return t.In(h.zona).Format(formato)
	}
	return local(pase.ValidFrom, "2000-01-01T00:00:00"), local(pase.ValidUntil, "2037-12-31T23:59:59")
}

// recortarAlSegundo deja `YYYY-MM-DDTHH:MM:SS` de un ISO 8601 que puede traer
// microsegundos o zona.
func recortarAlSegundo(ts string) string {
	ts = strings.TrimSpace(ts)
	if len(ts) >= 19 {
		return ts[:19]
	}
	return ts
}

// personaDelPase es el `employeeNo` del agente para un titular `v:<id>`.
func personaDelPase(holder string) string {
	id := strings.TrimPrefix(strings.TrimSpace(holder), "v:")
	if id == "" || id == holder {
		return ""
	}
	return prefijoDePase + "v" + id
}

// pasesDelEquipo lee, página por página, las personas que creó el agente.
func (h *HikvisionDSK1TAdapter) pasesDelEquipo(ctx context.Context) (map[string]pasoDelEquipo, error) {
	encontrados := map[string]pasoDelEquipo{}
	for posicion := 0; ; {
		cond := map[string]any{"UserInfoSearchCond": map[string]any{
			"searchID":             "porteria-plus",
			"searchResultPosition": posicion,
			"maxResults":           30,
		}}
		cuerpo, err := h.pedir(ctx, http.MethodPost, "/ISAPI/AccessControl/UserInfo/Search?format=json", cond)
		if err != nil {
			return nil, err
		}
		var resp struct {
			UserInfoSearch struct {
				ResponseStatusStrg string          `json:"responseStatusStrg"`
				NumOfMatches       int             `json:"numOfMatches"`
				UserInfo           []pasoDelEquipo `json:"UserInfo"`
			} `json:"UserInfoSearch"`
		}
		if err := json.Unmarshal(cuerpo, &resp); err != nil {
			return nil, fmt.Errorf("respuesta de búsqueda inválida: %w", err)
		}
		for _, p := range resp.UserInfoSearch.UserInfo {
			if strings.HasPrefix(p.EmployeeNo, prefijoDePase) {
				encontrados[p.EmployeeNo] = p
			}
		}
		posicion += resp.UserInfoSearch.NumOfMatches
		if resp.UserInfoSearch.ResponseStatusStrg != "MORE" || resp.UserInfoSearch.NumOfMatches == 0 {
			return encontrados, nil
		}
	}
}

func (h *HikvisionDSK1TAdapter) enviar(ctx context.Context, metodo, ruta string, cuerpo any) error {
	_, err := h.pedir(ctx, metodo, ruta, cuerpo)
	return err
}

// pedir hace la llamada ISAPI y devuelve el cuerpo si salió bien. Un 4xx con
// `statusCode` de error del equipo se reporta con su texto, que es lo que hay
// que leer para entender qué no le gustó.
func (h *HikvisionDSK1TAdapter) pedir(ctx context.Context, metodo, ruta string, cuerpo any) ([]byte, error) {
	var lector io.Reader
	if cuerpo != nil {
		datos, err := json.Marshal(cuerpo)
		if err != nil {
			return nil, err
		}
		lector = bytes.NewReader(datos)
	}
	req, err := http.NewRequestWithContext(ctx, metodo, h.base+ruta, lector)
	if err != nil {
		return nil, err
	}
	if cuerpo != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.digest.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respuesta, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: status %d: %s", metodo, ruta, resp.StatusCode, snippet(respuesta, 256))
	}
	return respuesta, nil
}
