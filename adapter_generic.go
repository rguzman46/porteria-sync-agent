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
//   - parte "event_data" (application/json) con `{plate, direction, timestamp, metadata}`
//   - parte "snapshot" (image/jpeg | image/png | image/webp)
//
// Pensado para tests manuales (curl), integradores propios, o adapters
// futuros (Dahua/Axis pueden adaptarse a este shape vía script intermedio).
//
// Ejemplo con curl:
//
//	curl -X POST http://localhost:8787/lpr-event \
//	  -H "X-Agent-Source: generic" \
//	  -F 'event_data={"plate":"ABC123","direction":"entry"}' \
//	  -F 'snapshot=@carro.jpg;type=image/jpeg'
type GenericEventData struct {
	Plate     string         `json:"plate"`
	Direction string         `json:"direction"`
	Timestamp string         `json:"timestamp"`
	Metadata  map[string]any `json:"metadata"`
}

func parseGenericMultipart(req *http.Request) (*QueuedEvent, []byte, error) {
	ct := req.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return nil, nil, fmt.Errorf("Content-Type inválido: %w", err)
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		return nil, nil, fmt.Errorf("Content-Type debe ser multipart/* (recibí %s)", mediaType)
	}

	mr := multipart.NewReader(req.Body, params["boundary"])

	var (
		data       *GenericEventData
		snapshot   []byte
		snapshotCT string
	)

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("leyendo siguiente parte: %w", err)
		}

		switch part.FormName() {
		case "event_data":
			raw, err := readAllLimited(part, 1*1024*1024)
			_ = part.Close()
			if err != nil {
				return nil, nil, fmt.Errorf("leyendo event_data: %w", err)
			}
			d := &GenericEventData{}
			if err := json.Unmarshal(raw, d); err != nil {
				return nil, nil, fmt.Errorf("parseando event_data JSON: %w", err)
			}
			data = d

		case "snapshot":
			raw, err := readAllLimited(part, MaxSnapshotSize)
			snapshotCT = part.Header.Get("Content-Type")
			_ = part.Close()
			if err != nil {
				return nil, nil, fmt.Errorf("leyendo snapshot: %w", err)
			}
			snapshot = raw

		default:
			_, _ = io.Copy(io.Discard, part)
			_ = part.Close()
		}
	}

	if data == nil {
		return nil, nil, fmt.Errorf("falta parte 'event_data' (JSON)")
	}
	if snapshot == nil {
		return nil, nil, fmt.Errorf("falta parte 'snapshot' (image)")
	}

	if snapshotCT == "" {
		snapshotCT = "image/jpeg" // fallback razonable
	}

	ev := &QueuedEvent{
		Plate:            normalizeAdapterPlate(data.Plate),
		Direction:        strings.ToLower(strings.TrimSpace(data.Direction)),
		Timestamp:        data.Timestamp,
		Metadata:         data.Metadata,
		SnapshotMimeType: snapshotCT,
	}
	if ev.Metadata == nil {
		ev.Metadata = map[string]any{}
	}
	ev.Metadata["source"] = "generic"

	return ev, snapshot, nil
}
