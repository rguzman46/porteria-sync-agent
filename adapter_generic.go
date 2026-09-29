package main

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
)

// parseGenericMultipart parsea un multipart genérico con shape simple:
//   - parte "event_data" (application/json) con
//     `{plate, direction, timestamp, confidence, plate_box, client_event_id, metadata}`
//   - parte "snapshot" (image/jpeg | image/png | image/webp)
//   - parte "plate_crop" opcional (el recorte de la placa)
//
// Pensado para tests manuales (curl), integradores propios, o cámaras que
// se puedan configurar para emitir este shape.
//
// Ejemplo con curl:
//
//	curl -X POST http://localhost:8787/lpr-event \
//	  -H "X-Agent-Source: generic" \
//	  -F 'event_data={"plate":"ABC123","direction":"entry","confidence":0.97}' \
//	  -F 'snapshot=@carro.jpg;type=image/jpeg'
type GenericEventData struct {
	Plate         string         `json:"plate"`
	Direction     string         `json:"direction"`
	Timestamp     string         `json:"timestamp"`
	ClientEventID string         `json:"client_event_id"`
	Confidence    *float64       `json:"confidence"`
	PlateBox      *PlateBox      `json:"plate_box"`
	Metadata      map[string]any `json:"metadata"`
}

func parseGenericMultipart(req *http.Request) (*QueuedEvent, []byte, []byte, error) {
	ct := req.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("Content-Type inválido: %w", err)
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		return nil, nil, nil, fmt.Errorf("Content-Type debe ser multipart/* (recibí %s)", mediaType)
	}

	mr := multipart.NewReader(req.Body, params["boundary"])

	var (
		data       *GenericEventData
		snapshot   []byte
		recorte    []byte
		snapshotCT string
	)

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, nil, fmt.Errorf("leyendo siguiente parte: %w", err)
		}

		switch part.FormName() {
		case "event_data":
			raw, err := readAllLimited(part, 1*1024*1024)
			_ = part.Close()
			if err != nil {
				return nil, nil, nil, fmt.Errorf("leyendo event_data: %w", err)
			}
			d := &GenericEventData{}
			if err := json.Unmarshal(raw, d); err != nil {
				return nil, nil, nil, fmt.Errorf("parseando event_data JSON: %w", err)
			}
			data = d

		case "snapshot":
			raw, err := readAllLimited(part, MaxSnapshotSize)
			snapshotCT = part.Header.Get("Content-Type")
			_ = part.Close()
			if err != nil {
				return nil, nil, nil, fmt.Errorf("leyendo snapshot: %w", err)
			}
			snapshot = raw

		case "plate_crop":
			raw, err := readAllLimited(part, MaxSnapshotSize)
			_ = part.Close()
			if err != nil {
				return nil, nil, nil, fmt.Errorf("leyendo plate_crop: %w", err)
			}
			recorte = raw

		default:
			_, _ = io.Copy(io.Discard, part)
			_ = part.Close()
		}
	}

	if data == nil {
		return nil, nil, nil, fmt.Errorf("falta parte 'event_data' (JSON)")
	}
	if snapshot == nil {
		return nil, nil, nil, fmt.Errorf("falta parte 'snapshot' (image)")
	}

	if snapshotCT == "" {
		snapshotCT = "image/jpeg" // fallback razonable
	}

	ev := &QueuedEvent{
		ClientEventID:    strings.TrimSpace(data.ClientEventID),
		Plate:            normalizeAdapterPlate(data.Plate),
		Direction:        strings.ToLower(strings.TrimSpace(data.Direction)),
		Timestamp:        strings.TrimSpace(data.Timestamp),
		Confidence:       data.Confidence,
		PlateBox:         data.PlateBox,
		Metadata:         data.Metadata,
		SnapshotMimeType: snapshotCT,
	}
	if ev.Metadata == nil {
		ev.Metadata = map[string]any{}
	}
	ev.Metadata["source"] = "generic"

	return ev, snapshot, recorte, nil
}
