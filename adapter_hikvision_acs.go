package main

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
)

// Eventos de acceso de los terminales Hikvision DS-K1T (ADR-130 de la
// plataforma): el visitante mostró el QR de su pase, el terminal lo comparó
// con sus tarjetas y abrió —o no—. Llegan por «HTTP Listening» del equipo
// (`/ISAPI/Event/notification/httpHosts`) a `/acs-event` de este receptor.
//
// El formato sale de la wiki ISAPI (`AccessControllerEvent`) y de lo que
// reporta un integrador con un DS-K1T341A: un multipart con una parte JSON y,
// si el equipo tiene encendido «Subir la foto capturada al autenticar», la
// imagen en otra parte. ⚠️ El nombre exacto de las partes y los `subEventType`
// de una lectura de QR se confirman con el equipo físico
// (`docs/investigacion-hikvision-terminales-qr.md` §9 de la plataforma): el
// parser se apoya en el tipo de contenido y no en los nombres.
//
// **Solo se reenvía lo que es de un pase.** Un residente que entra con su
// rostro o con su llavero produce el mismo evento, y esa foto y ese número
// no tienen por qué salir del conjunto. Pasa solo el evento de una persona
// que creó el agente (`employeeNoString` con el prefijo de pases) o, sin
// persona, un número con la forma de un pase: el intento con un código que
// no existe también le interesa a la administración.

// prefijoDePase marca las personas que el agente crea en el terminal: una
// por pase. Es lo que impide tocar a los residentes que el conjunto cargó
// con su rostro.
const prefijoDePase = "pp"

// acsEvento es la parte de `AccessControllerEvent` que se usa.
type acsEvento struct {
	DateTime              string `json:"dateTime"`
	EventType             string `json:"eventType"`
	AccessControllerEvent struct {
		MajorEventType    int    `json:"majorEventType"`
		SubEventType      int    `json:"subEventType"`
		CardNo            string `json:"cardNo"`
		EmployeeNoString  string `json:"employeeNoString"`
		SerialNo          int64  `json:"serialNo"`
		CurrentVerifyMode string `json:"currentVerifyMode"`
	} `json:"AccessControllerEvent"`
	IPAddress string `json:"ipAddress"`
}

// Los `subEventType` del evento mayor 5 (eventos de acceso) que dicen qué
// hizo el equipo con una tarjeta. Son los códigos MINOR_* del SDK de
// Hikvision; lo que no está aquí queda como «no lo dijo» (nil) en vez de
// adivinar.
var (
	acsAbrio = map[int]bool{
		1: true, // MINOR_LEGAL_CARD_PASS: tarjeta válida, abrió
	}
	acsNego = map[int]bool{
		6:  true, // MINOR_CARD_NO_RIGHT: la tarjeta no tiene permiso
		7:  true, // MINOR_CARD_INVALID_PERIOD: fuera de vigencia
		8:  true, // MINOR_CARD_OUT_OF_DATE: vencida
		9:  true, // MINOR_INVALID_CARD: no existe en el equipo
		10: true, // MINOR_OVER_TIME: fuera del horario
	}
)

// parseHikvisionAccessEvent lee el evento de acceso y su foto. Devuelve
// errEventoIgnorado para lo que no es de un pase.
func parseHikvisionAccessEvent(req *http.Request) (*QueuedEvent, []byte, error) {
	cuerpo, foto, err := partesDelEventoACS(req)
	if err != nil {
		return nil, nil, err
	}
	var ev acsEvento
	if err := json.Unmarshal(cuerpo, &ev); err != nil {
		return nil, nil, fmt.Errorf("JSON del evento de acceso inválido: %w", err)
	}
	if ev.EventType != "" && ev.EventType != "AccessControllerEvent" {
		// El latido del equipo y otros eventos llegan por el mismo canal.
		return nil, nil, errEventoIgnorado
	}
	ace := ev.AccessControllerEvent
	if ace.MajorEventType != 0 && ace.MajorEventType != 5 {
		return nil, nil, errEventoIgnorado
	}
	numero := strings.TrimSpace(ace.CardNo)
	persona := strings.TrimSpace(ace.EmployeeNoString)
	esDePase := strings.HasPrefix(persona, prefijoDePase) || (persona == "" && pareceUnPase(numero))
	if numero == "" || !esDePase {
		return nil, nil, errEventoIgnorado
	}

	evento := &QueuedEvent{
		CredentialType: "qr",
		Credential:     numero,
		Timestamp:      strings.TrimSpace(ev.DateTime),
	}
	// El terminal reintenta lo que no se le confirma: el mismo evento trae el
	// mismo `serialNo`, así que el id deriva de él y no de la llegada.
	if ace.SerialNo > 0 {
		evento.ClientEventID = "acs-" + strconv.FormatInt(ace.SerialNo, 10) + "-" + limpiarParaID(ev.DateTime)
	}
	switch {
	case acsAbrio[ace.SubEventType]:
		verdadero := true
		evento.Opened = &verdadero
	case acsNego[ace.SubEventType]:
		falso := false
		evento.Opened = &falso
	}
	evento.Metadata = map[string]any{"sub_event_type": ace.SubEventType}
	if len(foto) > 0 {
		evento.SnapshotMimeType = "image/jpeg"
	}
	return evento, foto, nil
}

// partesDelEventoACS saca el JSON y la primera imagen del cuerpo. Acepta
// multipart —lo normal— o el JSON solo, que es lo que manda el equipo cuando
// la subida de fotos está apagada.
func partesDelEventoACS(req *http.Request) ([]byte, []byte, error) {
	ct := req.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return nil, nil, fmt.Errorf("Content-Type inválido: %w", err)
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		cuerpo, err := readAllLimited(req.Body, MaxRequestSize)
		return cuerpo, nil, err
	}
	mr := multipart.NewReader(req.Body, params["boundary"])
	var cuerpo, foto []byte
	for {
		parte, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("leyendo el multipart: %w", err)
		}
		tipo := strings.ToLower(parte.Header.Get("Content-Type"))
		datos, err := readAllLimited(parte, MaxSnapshotSize)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case strings.HasPrefix(tipo, "image/") && foto == nil:
			foto = datos
		case cuerpo == nil && (strings.Contains(tipo, "json") || tipo == "" || strings.HasPrefix(tipo, "text/")):
			cuerpo = datos
		}
	}
	if cuerpo == nil {
		return nil, nil, fmt.Errorf("el evento de acceso no trae JSON")
	}
	return cuerpo, foto, nil
}

// pareceUnPase dice si el número tiene la forma del código de un pase: diez
// dígitos, sin cero adelante, hasta 4.294.967.295.
func pareceUnPase(numero string) bool {
	if len(numero) != 10 || numero[0] == '0' {
		return false
	}
	valor, err := strconv.ParseUint(numero, 10, 64)
	return err == nil && valor <= 4_294_967_295
}

func limpiarParaID(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
			b.WriteRune(r)
		}
	}
	return b.String()
}
