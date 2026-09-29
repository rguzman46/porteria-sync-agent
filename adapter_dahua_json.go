package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// parseDahuaTollgate parsea el push ITSAPI de una cámara Dahua ITC:
// `POST /NotificationInfo/TollgateInfo` con JSON.
//
// Fuentes: gist.github.com/paindefender/cf74164ade12f8a2954f271f3d5972e7 e
// ipcamtalk.com/threads/dahua-itc415-itsapi-sending-the-same-http-push-repeatedly.66375/.
//
// Campos que se usan:
//
//	Picture.Plate.PlateNumber      — la placa
//	Picture.Plate.Confidence       — confianza (el cloud normaliza)
//	Picture.Plate.BoundingBox      — rectángulo; ver abajo por qué no va como plate_box
//	Picture.CutoutPic.Content      — recorte de la placa, base64
//	Picture.NormalPic.Content      — escena, base64
//	Picture.SnapInfo.AccurateTime  — instante de la lectura
//	Picture.SnapInfo.Direction     — sentido (best-effort, ver normalizeDahuaDirection)
//	PicName (o Picture.*.PicName)  — nombre del archivo en la cámara
//
// La cámara reenvía el mismo evento hasta recibir `{"Result": true}`, y a
// veces incluso después. Por eso `client_event_id` no es el momento de
// llegada sino un hash de `PicName + AccurateTime`: el cloud deduplica y un
// reenvío no crea una segunda visita.
type dahuaTollgate struct {
	PicName string `json:"PicName"`
	Picture struct {
		Plate struct {
			PlateNumber string `json:"PlateNumber"`
			Confidence  any    `json:"Confidence"`
			BoundingBox []any  `json:"BoundingBox"`
		} `json:"Plate"`
		CutoutPic struct {
			Content string `json:"Content"`
			PicName string `json:"PicName"`
		} `json:"CutoutPic"`
		NormalPic struct {
			Content string `json:"Content"`
			PicName string `json:"PicName"`
		} `json:"NormalPic"`
		SnapInfo map[string]any `json:"SnapInfo"`
	} `json:"Picture"`
}

func parseDahuaTollgate(req *http.Request) (*QueuedEvent, []byte, []byte, error) {
	raw, err := readAllLimited(req.Body, MaxRequestSize)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("leyendo cuerpo Dahua: %w", err)
	}
	var d dahuaTollgate
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, nil, nil, fmt.Errorf("parseando JSON ITSAPI: %w", err)
	}

	placa := normalizeAdapterPlate(d.Picture.Plate.PlateNumber)
	if placa == "" {
		return nil, nil, nil, fmt.Errorf("TollgateInfo sin Picture.Plate.PlateNumber")
	}

	escena, err := decodificarBase64(d.Picture.NormalPic.Content)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("NormalPic.Content: %w", err)
	}
	recorte, err := decodificarBase64(d.Picture.CutoutPic.Content)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("CutoutPic.Content: %w", err)
	}
	if escena == nil && recorte != nil {
		// Solo vino el recorte: es la única foto que hay, y va como tal.
		escena, recorte = recorte, nil
	}

	picName := d.PicName
	if picName == "" {
		picName = d.Picture.NormalPic.PicName
	}
	if picName == "" {
		picName = d.Picture.CutoutPic.PicName
	}
	accurate, _ := d.Picture.SnapInfo["AccurateTime"].(string)

	ev := &QueuedEvent{
		ClientEventID:    idEstableDahua(picName, accurate, placa),
		Plate:            placa,
		Direction:        normalizeDahuaDirection(d.Picture.SnapInfo["Direction"]),
		Timestamp:        normalizeDahuaTimestamp(accurate),
		SnapshotMimeType: "image/jpeg",
		Metadata: map[string]any{
			"source":   "dahua",
			"pic_name": picName,
		},
	}
	if f, ok := aFloat(d.Picture.Plate.Confidence); ok {
		ev.Confidence = &f
	}
	if len(d.Picture.Plate.BoundingBox) == 4 {
		// Va en metadata y NO como plate_box: en las APIs inteligentes de
		// Dahua las coordenadas suelen ser relativas a 0–8192 y no píxeles,
		// y eso no está confirmado para este push. Un rectángulo en la
		// unidad equivocada pinta la placa en cualquier parte de la foto.
		ev.Metadata["dahua_bounding_box"] = d.Picture.Plate.BoundingBox
	}
	if lane, ok := d.Picture.SnapInfo["Lane"]; ok {
		ev.Metadata["lane"] = lane
	}
	return ev, escena, recorte, nil
}

func decodificarBase64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	// Algunos firmwares mandan el prefijo data-URI.
	if i := strings.Index(s, "base64,"); i >= 0 {
		s = s[i+len("base64,"):]
	}
	data, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(s)
	}
	if err != nil {
		return nil, err
	}
	if len(data) > MaxSnapshotSize {
		return nil, fmt.Errorf("imagen de %d bytes supera el tope de %d", len(data), MaxSnapshotSize)
	}
	return data, nil
}

// idEstableDahua es el mismo para cada reenvío del mismo evento. Se hashea
// porque el cloud acepta hasta 64 caracteres y PicName ya suele pasar de
// ahí. Si la cámara no manda ni PicName ni AccurateTime, no hay con qué
// deduplicar y se deja vacío (la cola pone el suyo).
func idEstableDahua(picName, accurate, placa string) string {
	if picName == "" && accurate == "" {
		return ""
	}
	h := sha1.Sum([]byte(picName + "|" + accurate + "|" + placa))
	return "dahua-" + hex.EncodeToString(h[:])[:24]
}

// normalizeDahuaDirection es best-effort: el nombre del valor cambia entre
// firmwares. Lo que no se reconoce queda vacío y el cloud usa el sentido del
// dispositivo, que es lo correcto en una cámara de un solo carril.
func normalizeDahuaDirection(v any) string {
	s, _ := v.(string)
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "approach", "in", "entry", "enter", "forward", "obverse":
		return "entry"
	case "leave", "out", "exit", "reverse", "away":
		return "exit"
	default:
		return ""
	}
}

// normalizeDahuaTimestamp acepta `2026-05-13 14:32:15` (formato Dahua) o
// ISO 8601 y devuelve ISO 8601. Sin zona: el cloud lo interpreta en hora
// de Bogotá, que es la que tiene la cámara.
func normalizeDahuaTimestamp(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if t, err := time.Parse("2006-01-02 15:04:05", raw); err == nil {
		return t.Format("2006-01-02T15:04:05")
	}
	return normalizeHikvisionTimestamp(raw)
}
