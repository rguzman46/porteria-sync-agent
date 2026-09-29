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
// cámaras Hikvision (Smart Event → ANPR → HTTP Listening). Solo extraemos
// los campos que nos interesan.
//
// Fuente: guía «How to integrate with Hikvision LPR function via ISAPI»
// v1.0.0 (download.viakom.cz/HIKVISION/SDK/) y el hilo
// ipcamtalk.com/threads/get-passed-license-plate-numbers-using-isapi.79032/.
//
// Estructura típica (simplificada):
//
//	<EventNotificationAlert>
//	  <dateTime>2026-05-13T14:32:15-05:00</dateTime>
//	  <eventType>ANPR</eventType>
//	  <ANPR>
//	    <licensePlate>ABC123</licensePlate>
//	    <country>0</country>
//	    <plateType>...</plateType>
//	    <plateColor>...</plateColor>
//	    <laneNo>1</laneNo>
//	    <direction>forward</direction>      <!-- forward=entry, reverse=exit, unknown -->
//	    <confidenceLevel>98</confidenceLevel><!-- 0–100 -->
//	    <matchingResult>...</matchingResult>
//	    <picName>...</picName>
//	    <pictureInfoList>
//	      <pictureInfo>
//	        <fileName>detectionPicture.jpg</fileName>
//	        <type>detectionPicture</type>
//	        <plateRect><X>…</X><Y>…</Y><width>…</width><height>…</height></plateRect>
//	      </pictureInfo>
//	    </pictureInfoList>
//	  </ANPR>
//	</EventNotificationAlert>
//
// El multipart trae `anpr.xml` + `licensePlatePicture.jpg` (el RECORTE de
// la placa) + `detectionPicture.jpg` (la ESCENA). El orden de las partes no
// es contrato: se identifica cada una por nombre.
type HikvisionAlert struct {
	XMLName   xml.Name `xml:"EventNotificationAlert"`
	DateTime  string   `xml:"dateTime"`
	EventType string   `xml:"eventType"`
	ANPR      struct {
		LicensePlate    string           `xml:"licensePlate"`
		Country         string           `xml:"country"`
		VehicleType     string           `xml:"vehicleType"`
		ColorOfVehicle  string           `xml:"colorOfVehicle"`
		PlateType       string           `xml:"plateType"`
		PlateColor      string           `xml:"plateColor"`
		LaneNo          string           `xml:"laneNo"`
		Direction       string           `xml:"direction"`
		ConfidenceLevel string           `xml:"confidenceLevel"`
		MatchingResult  string           `xml:"matchingResult"`
		PicName         string           `xml:"picName"`
		PicType         string           `xml:"picType"`
		PictureInfos    []hikPictureInfo `xml:"pictureInfoList>pictureInfo"`
	} `xml:"ANPR"`
}

// hikPictureInfo describe cada imagen adjunta y, en la escena, dónde está
// la placa. `plateRect` viene de fuente comunitaria: se mapea solo si viene.
type hikPictureInfo struct {
	FileName  string   `xml:"fileName"`
	Type      string   `xml:"type"`
	PlateRect *hikRect `xml:"plateRect"`
}

// hikRect acepta las dos grafías vistas en campo (X/Y/width/height y
// x/y/w/h): la que venga vacía queda en cero.
type hikRect struct {
	X      int `xml:"X"`
	Y      int `xml:"Y"`
	Width  int `xml:"width"`
	Height int `xml:"height"`
	Xm     int `xml:"x"`
	Ym     int `xml:"y"`
	Wm     int `xml:"w"`
	Hm     int `xml:"h"`
}

func (r *hikRect) plateBox() *PlateBox {
	if r == nil {
		return nil
	}
	x, y, w, h := r.X, r.Y, r.Width, r.Height
	if w == 0 && h == 0 {
		x, y, w, h = r.Xm, r.Ym, r.Wm, r.Hm
	}
	if w <= 0 || h <= 0 {
		return nil
	}
	return &PlateBox{XMin: x, YMin: y, XMax: x + w, YMax: y + h}
}

// tipoDeImagenHikvision clasifica una parte del multipart por su nombre.
const (
	imgEscena = iota
	imgRecorte
	imgRostro
	imgDesconocida
)

func tipoDeImagenHikvision(nombre string) int {
	n := strings.ToLower(nombre)
	switch {
	case n == "":
		return imgDesconocida
	case strings.Contains(n, "face"), strings.Contains(n, "driver"), strings.Contains(n, "pilot"):
		// Habeas Data: rostros no se guardan nunca.
		return imgRostro
	case strings.Contains(n, "licenseplate"), strings.Contains(n, "plate"):
		return imgRecorte
	case strings.Contains(n, "detection"), strings.Contains(n, "scene"), strings.Contains(n, "vehicle"), strings.Contains(n, "background"):
		return imgEscena
	default:
		return imgDesconocida
	}
}

// parseHikvisionMultipart parsea el POST multipart de la cámara Hikvision:
//   - parte XML (Content-Type: application/xml o text/xml) — el alert
//   - 1+ parte(s) image/jpeg — escena, recorte de placa y, si el admin lo
//     dejó activo, rostro (que se descarta).
//
// Devuelve el evento, la escena y el recorte (nil si no se pudo identificar
// uno con certeza: no se adivina).
func parseHikvisionMultipart(req *http.Request) (*QueuedEvent, []byte, []byte, error) {
	ct := req.Header.Get("Content-Type")
	if ct == "" {
		return nil, nil, nil, fmt.Errorf("Content-Type vacío")
	}

	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("Content-Type inválido: %w", err)
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		return nil, nil, nil, fmt.Errorf("Content-Type debe ser multipart/* (recibí %s)", mediaType)
	}

	boundary := params["boundary"]
	if boundary == "" {
		return nil, nil, nil, fmt.Errorf("boundary vacío en Content-Type")
	}

	mr := multipart.NewReader(req.Body, boundary)

	var (
		alert   *HikvisionAlert
		escena  []byte
		recorte []byte
		// primeraDesconocida es la escena si ninguna parte se llama como
		// escena: una cámara que manda una sola imagen sin nombre manda la
		// escena, y con dos sin nombre no se adivina cuál es el recorte.
		primeraDesconocida []byte
	)

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, nil, fmt.Errorf("leyendo siguiente parte multipart: %w", err)
		}

		partCT := part.Header.Get("Content-Type")
		partCTType, _, _ := mime.ParseMediaType(partCT)

		switch {
		case strings.HasSuffix(partCTType, "/xml") || strings.HasSuffix(partCTType, "+xml"):
			data, err := readAllLimited(part, 1*1024*1024) // 1MB cap para XML
			_ = part.Close()
			if err != nil {
				return nil, nil, nil, fmt.Errorf("leyendo XML alert: %w", err)
			}
			a := &HikvisionAlert{}
			if err := xml.Unmarshal(data, a); err != nil {
				return nil, nil, nil, fmt.Errorf("parseando XML alert: %w", err)
			}
			alert = a

		case strings.HasPrefix(partCTType, "image/"):
			tipo := tipoDeImagenHikvision(part.FileName())
			if tipo == imgDesconocida {
				tipo = tipoDeImagenHikvision(part.FormName())
			}
			if tipo == imgDesconocida && alert != nil {
				// El XML puede decir qué es cada archivo aunque la parte no.
				tipo = tipoSegunPictureInfo(alert, part.FileName())
			}
			if tipo == imgRostro || (tipo == imgEscena && escena != nil) || (tipo == imgRecorte && recorte != nil) {
				_, _ = io.Copy(io.Discard, part)
				_ = part.Close()
				continue
			}
			data, err := readAllLimited(part, MaxSnapshotSize)
			_ = part.Close()
			if err != nil {
				return nil, nil, nil, fmt.Errorf("leyendo image part: %w", err)
			}
			switch tipo {
			case imgEscena:
				escena = data
			case imgRecorte:
				recorte = data
			default:
				if primeraDesconocida == nil {
					primeraDesconocida = data
				}
			}

		default:
			// Parte desconocida (text/plain, etc.) — drenar y descartar.
			_, _ = io.Copy(io.Discard, part)
			_ = part.Close()
		}
	}

	if alert == nil {
		return nil, nil, nil, fmt.Errorf("no se encontró parte XML EventNotificationAlert")
	}

	if alert.EventType != "" && !strings.EqualFold(alert.EventType, "ANPR") {
		// Hikvision puede mandar otros eventos al mismo listener (motion,
		// tampering, heartbeat). Solo procesamos ANPR; el resto se contesta
		// como recibido para que la cámara no insista.
		return nil, nil, nil, fmt.Errorf("eventType %q no es ANPR: %w", alert.EventType, errEventoIgnorado)
	}

	if escena == nil {
		escena = primeraDesconocida
	}

	ev := &QueuedEvent{
		Plate:            normalizeAdapterPlate(alert.ANPR.LicensePlate),
		Direction:        normalizeHikvisionDirection(alert.ANPR.Direction),
		Timestamp:        normalizeHikvisionTimestamp(alert.DateTime),
		SnapshotMimeType: "image/jpeg", // Hikvision siempre JPEG
		PlateBox:         plateBoxDeLaEscena(alert),
		Metadata: map[string]any{
			"source":           "hikvision",
			"country":          alert.ANPR.Country,
			"vehicle_type":     alert.ANPR.VehicleType,
			"color":            alert.ANPR.ColorOfVehicle,
			"plate_type":       alert.ANPR.PlateType,
			"plate_color":      alert.ANPR.PlateColor,
			"lane":             alert.ANPR.LaneNo,
			"matching_result":  alert.ANPR.MatchingResult,
			"confidence_level": alert.ANPR.ConfidenceLevel,
		},
	}
	if f, ok := aFloat(alert.ANPR.ConfidenceLevel); ok {
		ev.Confidence = &f
	}
	for k, v := range ev.Metadata {
		if s, ok := v.(string); ok && s == "" {
			delete(ev.Metadata, k)
		}
	}

	return ev, escena, recorte, nil
}

// tipoSegunPictureInfo busca el archivo en `pictureInfoList` del XML.
func tipoSegunPictureInfo(alert *HikvisionAlert, fileName string) int {
	for _, pi := range alert.ANPR.PictureInfos {
		if fileName != "" && strings.EqualFold(pi.FileName, fileName) {
			if t := tipoDeImagenHikvision(pi.Type); t != imgDesconocida {
				return t
			}
			return tipoDeImagenHikvision(pi.FileName)
		}
	}
	return imgDesconocida
}

// plateBoxDeLaEscena toma el `plateRect` del pictureInfo de la escena. El
// del recorte no sirve: sus coordenadas son de otra imagen.
func plateBoxDeLaEscena(alert *HikvisionAlert) *PlateBox {
	for _, pi := range alert.ANPR.PictureInfos {
		tipo := tipoDeImagenHikvision(pi.Type)
		if tipo == imgDesconocida {
			tipo = tipoDeImagenHikvision(pi.FileName)
		}
		if tipo == imgRecorte || tipo == imgRostro {
			continue
		}
		if box := pi.PlateRect.plateBox(); box != nil {
			return box
		}
	}
	return nil
}

// normalizeAdapterPlate strip whitespace + uppercase. La normalización
// completa la hace el cloud. Acá solo nos aseguramos de no mandar control
// chars.
func normalizeAdapterPlate(raw string) string {
	return strings.ToUpper(strings.TrimSpace(raw))
}

// normalizeHikvisionDirection mapea forward → entry, reverse → exit.
// `unknown` o vacío → vacío: el cloud usa el sentido del dispositivo.
func normalizeHikvisionDirection(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "forward":
		return "entry"
	case "reverse":
		return "exit"
	default:
		return ""
	}
}

// normalizeHikvisionTimestamp valida que la fecha sea parseable como RFC3339
// (ISO 8601). Si no lo es, retorna vacío y el cloud usará la hora de llegada.
// Hikvision suele enviar `2026-05-13T14:32:15-05:00`; algunos firmwares
// viejos sin zona, que el cloud interpreta en hora de Bogotá.
func normalizeHikvisionTimestamp(raw string) string {
	if raw == "" {
		return ""
	}
	if _, err := time.Parse(time.RFC3339, raw); err != nil {
		if _, err2 := time.Parse("2006-01-02T15:04:05", raw); err2 != nil {
			return ""
		}
	}
	return raw
}
