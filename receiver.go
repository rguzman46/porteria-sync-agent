package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// Receiver es el HTTP server LAN que recibe las lecturas de la cámara LPR.
// Bind default a `0.0.0.0:8787` para que la cámara (típicamente en otra IP
// de la LAN del conjunto) pueda postearle.
//
// **Quién puede postear**: solo `camera.host` y lo que esté en
// `receiver.allow_from`. El puerto escucha en toda la LAN de la portería, y
// una lectura falsa posteada desde cualquier PC abre una visita en el panel
// con una foto inventada. Las cámaras no saben mandar Bearer, así que el
// filtro es por IP de origen; `receiver.allow_any` lo apaga para
// diagnóstico. `/health` queda abierto: no escribe nada.
//
// **Resiliencia offline**: el handler responde en cuanto encola el evento.
// El envío al cloud es asíncrono (replay worker en goroutine separada). La
// cámara NO bloquea esperando confirmación del cloud.
//
// Cada marca espera una respuesta distinta y hay que dársela:
//   - Hikvision: 200 con cuerpo vacío (Content-Length: 0).
//   - Dahua ITSAPI: `{"Result": true}`; con cualquier otra cosa reenvía el
//     mismo evento en bucle.
//   - Genérico / Axis: 202 con JSON.
type Receiver struct {
	cfg     *Config
	queue   *FileQueue
	server  *http.Server
	maxSize int64

	allowAny bool
	origenes []*net.IPNet
}

// MaxRequestSize: 24 MB. Dahua manda las dos imágenes en base64 dentro del
// JSON (un tercio más grandes) y una escena de 8 MP puede pasar de 6 MB.
// Defensa contra uploads enormes desde la LAN.
const MaxRequestSize = 24 * 1024 * 1024

// MaxSnapshotSize es lo más grande que se lee de una imagen antes de
// intentar recomprimirla (ver acotarFoto). Mayor que esto es ya basura.
const MaxSnapshotSize = 12 * 1024 * 1024

// errEventoIgnorado marca un POST válido que no produce lectura (Hikvision
// mandó otro tipo de evento, Axis un `update`). Se responde como éxito para
// que la cámara no reintente.
var errEventoIgnorado = errors.New("evento ignorado")

// NewReceiver construye el receiver listo para Start(). NO arranca el listener
// — debes llamar Start(ctx) en una goroutine.
func NewReceiver(cfg *Config, queue *FileQueue) *Receiver {
	mux := http.NewServeMux()
	r := &Receiver{
		cfg:      cfg,
		queue:    queue,
		maxSize:  MaxRequestSize,
		allowAny: cfg.Receiver.AllowAny,
		origenes: origenesPermitidos(cfg),
	}
	mux.HandleFunc("/lpr-event", r.soloDesdeLaCamara(r.handleLprEvent))
	mux.HandleFunc("/axis-event", r.soloDesdeLaCamara(r.handleAxisEvent))
	// Rutas nativas de Dahua ITSAPI: la cámara no deja cambiarlas.
	mux.HandleFunc("/NotificationInfo/TollgateInfo", r.soloDesdeLaCamara(r.handleDahuaTollgate))
	mux.HandleFunc("/NotificationInfo/KeepAlive", r.soloDesdeLaCamara(r.handleDahuaKeepAlive))
	mux.HandleFunc("/health", r.handleHealth)

	r.server = &http.Server{
		Addr:              cfg.Receiver.BindAddress,
		Handler:           mux,
		ReadTimeout:       60 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	return r
}

// origenesPermitidos arma la lista de IPs/redes que pueden postear:
// `camera.host` (resuelto si es nombre) más `receiver.allow_from`.
func origenesPermitidos(cfg *Config) []*net.IPNet {
	var out []*net.IPNet
	if host := strings.TrimSpace(cfg.Camera.Host); host != "" {
		if red, err := parseOrigen(host); err == nil {
			out = append(out, red)
		} else if ips, err := net.LookupIP(host); err == nil {
			for _, ip := range ips {
				out = append(out, redDeUnaIP(ip))
			}
		} else {
			log.Printf("[receiver] ⚠ camera.host=%q no es IP ni resuelve: la cámara no va a poder postear hasta corregirlo", host)
		}
	}
	for _, origen := range cfg.Receiver.AllowFrom {
		if red, err := parseOrigen(origen); err == nil {
			out = append(out, red)
		}
	}
	return out
}

// parseOrigen acepta una IP (`192.168.1.50`) o una red CIDR
// (`192.168.1.0/24`).
func parseOrigen(s string) (*net.IPNet, error) {
	s = strings.TrimSpace(s)
	if _, red, err := net.ParseCIDR(s); err == nil {
		return red, nil
	}
	if ip := net.ParseIP(s); ip != nil {
		return redDeUnaIP(ip), nil
	}
	return nil, fmt.Errorf("%q no es una IP ni una red CIDR", s)
}

func redDeUnaIP(ip net.IP) *net.IPNet {
	if v4 := ip.To4(); v4 != nil {
		return &net.IPNet{IP: v4, Mask: net.CIDRMask(32, 32)}
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)}
}

// origenPermitido dice si la IP remota puede postear lecturas.
func (r *Receiver) origenPermitido(req *http.Request) bool {
	if r.allowAny {
		return true
	}
	ip := net.ParseIP(clientIP(req))
	if ip == nil {
		return false
	}
	for _, red := range r.origenes {
		if red.Contains(ip) {
			return true
		}
	}
	return false
}

// soloDesdeLaCamara envuelve un handler con el filtro de origen.
func (r *Receiver) soloDesdeLaCamara(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if !r.origenPermitido(req) {
			log.Printf("[receiver] rechazado POST %s desde %s: no es la cámara (camera.host / receiver.allow_from)", req.URL.Path, clientIP(req))
			http.Error(w, "origen no permitido", http.StatusForbidden)
			return
		}
		next(w, req)
	}
}

// Start bloquea hasta que ctx se cancele, momento en el cual hace un
// graceful shutdown (drena requests en vuelo con timeout 5s).
func (r *Receiver) Start(ctx context.Context) error {
	log.Printf("[receiver] escuchando en %s (queue=%s, orígenes=%d, allow_any=%v)",
		r.cfg.Receiver.BindAddress, r.cfg.Receiver.QueueDir, len(r.origenes), r.allowAny)

	// Arrancar listener en goroutine — el bloqueo lo hacemos esperando ctx.
	errCh := make(chan error, 1)
	go func() {
		err := r.server.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		log.Println("[receiver] contexto cancelado, shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return r.server.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// handleLprEvent procesa un POST de la cámara. Dispatch por header
// `X-Agent-Source`. Si el header no viene, asume hikvision (vendor más
// común; su HTTP Listening no permite cabeceras propias).
func (r *Receiver) handleLprEvent(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, r.maxSize)

	source := strings.ToLower(req.Header.Get("X-Agent-Source"))
	if source == "" {
		source = "hikvision"
	}

	var (
		event           *QueuedEvent
		escena, recorte []byte
		err             error
	)

	switch source {
	// Familia Hikvision — todas las líneas emiten el MISMO XML
	// EventNotificationAlert con estructura ANPR. El parser es uno solo.
	case "hikvision", "hikvision_traffic", "hikvision_itc", "hikvision_anpr_audit":
		event, escena, recorte, err = parseHikvisionMultipart(req)
	// Dahua tiene su ruta nativa (/NotificationInfo/TollgateInfo). Acá se
	// acepta el JSON ITSAPI si alguien apunta la cámara a /lpr-event, o el
	// genérico si es multipart.
	case "dahua", "dahua_itc":
		if esJSON(req) {
			event, escena, recorte, err = parseDahuaTollgate(req)
		} else {
			event, escena, recorte, err = parseGenericMultipart(req)
		}
	case "axis", "axis_vapix":
		if esJSON(req) {
			event, escena, recorte, err = parseAxisJSON(req)
		} else {
			event, escena, recorte, err = parseGenericMultipart(req)
		}
	case "generic", "json":
		event, escena, recorte, err = parseGenericMultipart(req)
	default:
		http.Error(w, fmt.Sprintf("X-Agent-Source desconocido: %q (soportados: hikvision, hikvision_traffic, hikvision_itc, hikvision_anpr_audit, dahua, dahua_itc, axis, axis_vapix, generic)", source), http.StatusBadRequest)
		return
	}

	r.encolarYResponder(w, req, source, event, escena, recorte, err)
}

// handleAxisEvent es la ruta para el ACAP License Plate Verifier, que manda
// JSON y no pone cabeceras propias.
func (r *Receiver) handleAxisEvent(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, r.maxSize)
	event, escena, recorte, err := parseAxisJSON(req)
	r.encolarYResponder(w, req, "axis", event, escena, recorte, err)
}

// handleDahuaTollgate es la ruta nativa del push ITSAPI de Dahua.
func (r *Receiver) handleDahuaTollgate(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, r.maxSize)
	event, escena, recorte, err := parseDahuaTollgate(req)
	r.encolarYResponder(w, req, "dahua", event, escena, recorte, err)
}

// handleDahuaKeepAlive responde el latido que la cámara Dahua le manda al
// receptor. Sin `{"Result": true}` la cámara da el destino por caído.
func (r *Receiver) handleDahuaKeepAlive(w http.ResponseWriter, req *http.Request) {
	_, _ = io.Copy(io.Discard, io.LimitReader(req.Body, 64*1024))
	responderDahua(w)
}

// encolarYResponder es el tramo común: validar, acotar las fotos, encolar y
// contestar en el dialecto de la marca.
func (r *Receiver) encolarYResponder(w http.ResponseWriter, req *http.Request, source string, event *QueuedEvent, escena, recorte []byte, err error) {
	if errors.Is(err, errEventoIgnorado) {
		// Válido pero sin lectura: éxito para la cámara, para que no insista.
		responderExito(w, source, map[string]any{"ok": true, "ignored": true})
		return
	}
	if err != nil {
		log.Printf("[receiver] parse error (source=%s, ip=%s): %v", source, clientIP(req), err)
		http.Error(w, "parse error: "+err.Error(), http.StatusBadRequest)
		return
	}
	if event.Plate == "" {
		http.Error(w, "evento sin placa", http.StatusBadRequest)
		return
	}
	if len(escena) == 0 {
		// Sin foto no encolamos — este receptor es para la captura visual.
		http.Error(w, "evento sin imagen (este receptor requiere la foto — usa /api/v1/access/event para texto solo)", http.StatusBadRequest)
		return
	}

	escena, mime, err := acotarFoto(escena, event.SnapshotMimeType)
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	event.SnapshotMimeType = mime
	if len(recorte) > 0 {
		recorte, _, err = acotarFoto(recorte, "image/jpeg")
		if err != nil {
			// El recorte es accesorio: se pierde él, no la lectura.
			log.Printf("[receiver] recorte descartado (placa=%s): %v", event.Plate, err)
			recorte = nil
		}
	}

	if err := r.queue.EnqueueConRecorte(event, escena, recorte); err != nil {
		log.Printf("[receiver] enqueue falló: %v", err)
		http.Error(w, "no se pudo encolar el evento", http.StatusInternalServerError)
		return
	}

	log.Printf("[receiver] encolado evento placa=%s bytes=%d recorte=%d source=%s ip=%s",
		event.Plate, len(escena), len(recorte), source, clientIP(req))

	responderExito(w, source, map[string]any{
		"ok":       true,
		"event_id": event.ID,
		"queued":   true,
	})
}

// responderExito contesta como espera cada marca. Ver el comentario del tipo.
func responderExito(w http.ResponseWriter, source string, cuerpo map[string]any) {
	switch {
	case strings.HasPrefix(source, "hikvision"):
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
	case strings.HasPrefix(source, "dahua"):
		responderDahua(w)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(cuerpo)
	}
}

func responderDahua(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"Result": true}`))
}

func esJSON(req *http.Request) bool {
	return strings.HasPrefix(strings.ToLower(req.Header.Get("Content-Type")), "application/json")
}

// handleHealth retorna telemetría útil para el admin diagnosticando el agent.
// Sin filtro de origen — no escribe nada y no expone datos personales.
func (r *Receiver) handleHealth(w http.ResponseWriter, req *http.Request) {
	items, bytes, oldest := r.queue.Stats()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":             true,
		"agent_version":  AgentVersion,
		"queue_items":    items,
		"queue_bytes":    bytes,
		"queue_oldest_s": int(oldest.Seconds()),
		"max_items":      r.cfg.Receiver.MaxQueueItems,
		"max_bytes":      r.cfg.Receiver.MaxQueueBytes,
		"server_time":    time.Now().Format(time.RFC3339),
	})
}

// clientIP es la IP del socket. No se mira X-Forwarded-For a propósito: el
// filtro de origen se apoya en esto y una cabecera la pone cualquiera.
func clientIP(req *http.Request) string {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr
	}
	return host
}

// readAllLimited lee hasta `max` bytes con error explícito si excede (vs
// truncation silenciosa de io.LimitReader).
func readAllLimited(r io.Reader, max int64) ([]byte, error) {
	lr := io.LimitReader(r, max+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("payload excede %d bytes", max)
	}
	return data, nil
}
