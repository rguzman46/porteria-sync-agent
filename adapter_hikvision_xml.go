package main

import (
	"encoding/xml"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// HikvisionAlert es el shape del XML EventNotificationAlert que envían las
// cámaras Hikvision ITC/DS-TCG (Smart Event → ANPR → HTTP Listening). Solo
// extraemos los campos que nos interesan — Hikvision añade docenas de
// subcampos que ignoramos por ser irrelevantes (regionID, AID, etc.).
//
// Estructura típica (simplificada):
//
//	<EventNotificationAlert>
//	  <ipAddress>192.168.1.50</ipAddress>
//	  <portNo>80</portNo>
//	  <protocol>HTTP</protocol>
//	  <macAddress>...</macAddress>
//	  <channelID>1</channelID>
//	  <dateTime>2026-05-13T14:32:15-05:00</dateTime>
//	  <activePostCount>1</activePostCount>
//	  <eventType>ANPR</eventType>
//	  <ANPR>
//	    <licensePlate>ABC123</licensePlate>
//	    <licenseBright>true</licenseBright>
//	    <country>0</country>
//	    <vehicleType>1</vehicleType>
//	    <colorOfVehicle>blue</colorOfVehicle>
//	    <direction>forward</direction>      <!-- forward=entry, reverse=exit -->
//	    <confidenceLevel>98</confidenceLevel>
//	    ...
//	    <picName>licensePicture.jpg</picName>   <!-- nombre del attachment binario -->
//	  </ANPR>
//	</EventNotificationAlert>
type HikvisionAlert struct {
	XMLName  xml.Name `xml:"EventNotificationAlert"`
	DateTime string   `xml:"dateTime"`
	EventType string  `xml:"eventType"`
	ANPR     struct {
		LicensePlate    string `xml:"licensePlate"`
		Country         string `xml:"country"`
		VehicleType     string `xml:"vehicleType"`
		ColorOfVehicle  string `xml:"colorOfVehicle"`
		Direction       string `xml:"direction"`
		ConfidenceLevel string `xml:"confidenceLevel"`
		PicName         string `xml:"picName"`
	} `xml:"ANPR"`
}

// parseHikvisionMultipart parsea el POST multipart de la cámara Hikvision:
//   - parte XML (Content-Type: application/xml o text/xml) — el alert
//   - 1+ parte(s) image/jpeg — los snapshots referenciados en <picName>
//
// Solo tomamos el PRIMER snapshot image/jpeg (la "escena completa"). Las
// cámaras Hikvision a veces mandan también un crop de la placa y/o cara
// del conductor — los ignoramos (decisión de diseño Habeas Data: NO
// almacenamos rostros).
//
// Returns:
//   - QueuedEvent con plate, direction, timestamp, metadata.
//   - snapshot binario (raw JPEG, no convertido — el cloud convierte a WebP).
//   - error si el XML no parsea o no hay placa.
func parseHikvisionMultipart(req *http.Request) (*QueuedEvent, []byte, error) {
	ct := req.Header.Get("Content-Type")
	if ct == "" {
		return nil, nil, fmt.Errorf("Content-Type vacío")
	}

	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return nil, nil, fmt.Errorf("Content-Type inválido: %w", err)
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		return nil, nil, fmt.Errorf("Content-Type debe ser multipart/* (recibí %s)", mediaType)
	}

	boundary := params["boundary"]
	if boundary == "" {
		return nil, nil, fmt.Errorf("boundary vacío en Content-Type")
	}

	mr := multipart.NewReader(req.Body, boundary)

	var (
		alert     *HikvisionAlert
		snapshot  []byte
	)

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("leyendo siguiente parte multipart: %w", err)
		}

		partCT := part.Header.Get("Content-Type")
		partCTType, _, _ := mime.ParseMediaType(partCT)

		switch {
		case strings.HasSuffix(partCTType, "/xml") || strings.HasSuffix(partCTType, "+xml"):
			data, err := readAllLimited(part, 1*1024*1024) // 1MB cap para XML
			_ = part.Close()
			if err != nil {
				return nil, nil, fmt.Errorf("leyendo XML alert: %w", err)
			}
			a := &HikvisionAlert{}
			if err := xml.Unmarshal(data, a); err != nil {
				return nil, nil, fmt.Errorf("parseando XML alert: %w", err)
			}
			alert = a

		case strings.HasPrefix(partCTType, "image/"):
			// Tomamos el PRIMER image. Las siguientes (plate crop, face) se
			// descartan — solo nos interesa la escena completa.
			if snapshot == nil {
				data, err := readAllLimited(part, MaxSnapshotSize)
				if err != nil {
					_ = part.Close()
					return nil, nil, fmt.Errorf("leyendo image part: %w", err)
				}
				snapshot = data
			}
			_ = part.Close()

		default:
			// Parte desconocida (text/plain, etc.) — drenar y descartar.
			_, _ = io.Copy(io.Discard, part)
			_ = part.Close()
		}
	}

	if alert == nil {
		return nil, nil, fmt.Errorf("no se encontró parte XML EventNotificationAlert")
	}

	if alert.EventType != "" && !strings.EqualFold(alert.EventType, "ANPR") {
		// Hikvision puede mandar otros eventos al mismo listener (motion,
		// tampering, etc.). Solo procesamos ANPR.
		return nil, nil, fmt.Errorf("eventType %q no es ANPR — ignorado", alert.EventType)
	}

	ev := &QueuedEvent{
		Plate:            normalizeAdapterPlate(alert.ANPR.LicensePlate),
		Direction:        normalizeHikvisionDirection(alert.ANPR.Direction),
		Timestamp:        normalizeHikvisionTimestamp(alert.DateTime),
		SnapshotMimeType: "image/jpeg", // Hikvision siempre JPEG
		Metadata: map[string]any{
			"source":           "hikvision",
			"country":          alert.ANPR.Country,
			"vehicle_type":     alert.ANPR.VehicleType,
			"color":            alert.ANPR.ColorOfVehicle,
			"confidence_level": alert.ANPR.ConfidenceLevel,
		},
	}

	return ev, snapshot, nil
}

// normalizeAdapterPlate strip whitespace + uppercase. La normalización
// completa (uppercase + strip espacios/guiones) la hace el cloud en
// AccessEventService::normalizePlate. Acá solo nos aseguramos de no
// mandar control chars.
func normalizeAdapterPlate(raw string) string {
	return strings.ToUpper(strings.TrimSpace(raw))
}

// normalizeHikvisionDirection mapea forward → entry, reverse → exit.
// Hikvision usa estos términos en su payload ANPR para reportar el
// sentido detectado por la cámara LPR (configurable por carril).
func normalizeHikvisionDirection(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "forward":
		return "entry"
	case "reverse":
		return "exit"
	default:
		return "" // vacío = cloud usa config del device
	}
}

// normalizeHikvisionTimestamp valida que la fecha sea parseable como RFC3339
// (ISO 8601). Si no lo es, retorna vacío y el cloud usará server time.
// Hikvision suele enviar en formato `2026-05-13T14:32:15-05:00` que es
// válido RFC3339.
func normalizeHikvisionTimestamp(raw string) string {
	if raw == "" {
		return ""
	}
	if _, err := time.Parse(time.RFC3339, raw); err != nil {
		// Intentar formato Hikvision sin timezone (algunas firmware viejas).
		if _, err2 := time.Parse("2006-01-02T15:04:05", raw); err2 != nil {
			return ""
		}
	}
	return raw
}
