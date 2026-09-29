package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// parseAxisJSON parsea el push del ACAP «AXIS License Plate Verifier»
// (HTTP POST con JSON). Fuente:
// https://developer.axis.com/vapix/applications/license-plate-verifier-api/
//
// Campos:
//
//	plateASCII / plateUTF8     — la placa
//	plateConfidence            — confianza
//	carMoveDirection           — in | out | unknown
//	carID                      — identificador del vehículo en la sesión
//	carState                   — new | update | lost
//	plateCoordinates           — [left, top, width, height] en píxeles de la escena
//	imageType                  — plate | frame | vehicle
//	imageArray                 — base64 (o `imagesURI`, que no se sigue: exigiría
//	                             pedirla a la cámara con credenciales desde el receptor)
//	capture_timestamp          — instante de la lectura
//
// El ACAP manda varios eventos por carro (`new`, luego `update`s, al final
// `lost`). Solo `new` produce lectura; los demás se contestan como
// recibidos y se ignoran. Si el POST trae un lote (array), se agrupa por
// carID: el `new` de cada carro, o el último `lost` si en el lote no vino
// el `new`.
type axisEvento struct {
	PlateASCII       string `json:"plateASCII"`
	PlateUTF8        string `json:"plateUTF8"`
	PlateConfidence  any    `json:"plateConfidence"`
	CarMoveDirection string `json:"carMoveDirection"`
	CarID            any    `json:"carID"`
	CarState         string `json:"carState"`
	PlateCoordinates []any  `json:"plateCoordinates"`
	ImageType        string `json:"imageType"`
	ImageArray       any    `json:"imageArray"`
	CaptureTimestamp any    `json:"capture_timestamp"`
}

func parseAxisJSON(req *http.Request) (*QueuedEvent, []byte, []byte, error) {
	raw, err := readAllLimited(req.Body, MaxRequestSize)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("leyendo cuerpo Axis: %w", err)
	}
	eventos, err := decodificarAxis(raw)
	if err != nil {
		return nil, nil, nil, err
	}
	elegido := elegirEventoAxis(eventos)
	if elegido == nil {
		return nil, nil, nil, fmt.Errorf("solo carState update/lost: %w", errEventoIgnorado)
	}

	placa := normalizeAdapterPlate(elegido.PlateASCII)
	if placa == "" {
		placa = normalizeAdapterPlate(elegido.PlateUTF8)
	}
	if placa == "" {
		return nil, nil, nil, fmt.Errorf("evento Axis sin plateASCII/plateUTF8")
	}

	// Las imágenes del mismo carro pueden venir en varios eventos del lote
	// (uno por imageType).
	var escena, recorte []byte
	carID := fmt.Sprint(elegido.CarID)
	for i := range eventos {
		e := &eventos[i]
		if fmt.Sprint(e.CarID) != carID {
			continue
		}
		img, err := decodificarBase64(primeraCadena(e.ImageArray))
		if err != nil || img == nil {
			continue
		}
		switch strings.ToLower(e.ImageType) {
		case "plate":
			if recorte == nil {
				recorte = img
			}
		default: // frame, vehicle
			if escena == nil {
				escena = img
			}
		}
	}
	if escena == nil && recorte != nil {
		escena, recorte = recorte, nil
	}

	ts := normalizeAxisTimestamp(elegido.CaptureTimestamp)
	ev := &QueuedEvent{
		ClientEventID:    idEstableAxis(carID, ts),
		Plate:            placa,
		Direction:        normalizeAxisDirection(elegido.CarMoveDirection),
		Timestamp:        ts,
		SnapshotMimeType: "image/jpeg",
		PlateBox:         plateBoxAxis(elegido.PlateCoordinates),
		Metadata: map[string]any{
			"source":    "axis",
			"car_id":    carID,
			"car_state": elegido.CarState,
		},
	}
	if f, ok := aFloat(elegido.PlateConfidence); ok {
		ev.Confidence = &f
	}
	return ev, escena, recorte, nil
}

func decodificarAxis(raw []byte) ([]axisEvento, error) {
	cuerpo := strings.TrimSpace(string(raw))
	if strings.HasPrefix(cuerpo, "[") {
		var lote []axisEvento
		if err := json.Unmarshal(raw, &lote); err != nil {
			return nil, fmt.Errorf("parseando lote JSON Axis: %w", err)
		}
		return lote, nil
	}
	var uno axisEvento
	if err := json.Unmarshal(raw, &uno); err != nil {
		return nil, fmt.Errorf("parseando JSON Axis: %w", err)
	}
	return []axisEvento{uno}, nil
}

// elegirEventoAxis aplica la regla de carState. Devuelve nil si nada del
// lote produce lectura.
func elegirEventoAxis(eventos []axisEvento) *axisEvento {
	var ultimoLost *axisEvento
	for i := range eventos {
		e := &eventos[i]
		switch strings.ToLower(e.CarState) {
		case "new", "":
			return e
		case "lost":
			ultimoLost = e
		}
	}
	return ultimoLost
}

func primeraCadena(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		if len(x) > 0 {
			if s, ok := x[0].(string); ok {
				return s
			}
		}
	}
	return ""
}

// plateBoxAxis convierte [left, top, width, height] a xmin/ymin/xmax/ymax.
func plateBoxAxis(coords []any) *PlateBox {
	if len(coords) != 4 {
		return nil
	}
	var v [4]float64
	for i, c := range coords {
		f, ok := aFloat(c)
		if !ok {
			return nil
		}
		v[i] = f
	}
	if v[2] <= 0 || v[3] <= 0 {
		return nil
	}
	return &PlateBox{XMin: int(v[0]), YMin: int(v[1]), XMax: int(v[0] + v[2]), YMax: int(v[1] + v[3])}
}

func normalizeAxisDirection(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "in":
		return "entry"
	case "out":
		return "exit"
	default:
		return ""
	}
}

// normalizeAxisTimestamp acepta epoch (segundos o milisegundos) o texto ISO.
func normalizeAxisTimestamp(v any) string {
	switch x := v.(type) {
	case float64:
		if x <= 0 {
			return ""
		}
		if x > 1e12 {
			return time.UnixMilli(int64(x)).UTC().Format(time.RFC3339)
		}
		return time.Unix(int64(x), 0).UTC().Format(time.RFC3339)
	case string:
		if n, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
			return normalizeAxisTimestamp(n)
		}
		return normalizeHikvisionTimestamp(strings.TrimSpace(x))
	}
	return ""
}

// idEstableAxis: el carID se reinicia con la cámara, así que va con el día.
func idEstableAxis(carID, ts string) string {
	if carID == "" || carID == "<nil>" {
		return ""
	}
	dia := ""
	if len(ts) >= 10 {
		dia = "-" + strings.ReplaceAll(ts[:10], "-", "")
	}
	return truncar("axis-"+carID+dia, 64)
}
