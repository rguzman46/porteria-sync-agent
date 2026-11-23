package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// PanelLocal es la página que abre el técnico en el PC de la portería:
// http://localhost:8787. Dice en verde o en rojo si el agente habla con la
// plataforma y con el equipo, y deja cambiar la IP, el usuario y la
// contraseña del equipo sin tocar archivos ni comandos.
//
// **Solo le contesta a este mismo PC.** El puerto escucha en toda la red de
// la portería (por ahí llegan las lecturas), así que el panel exige las tres
// cosas: que la conexión venga de 127.0.0.1, que la dirección escrita sea
// `localhost` (contra una página de internet que se haga pasar por local) y,
// para guardar, que el formulario venga de esta misma página con su ficha.
// Nunca muestra la contraseña guardada.
type PanelLocal struct {
	cfg        *Config
	configPath string
	servicio   string
	estado     *Estado
	ficha      string
	recargar   func()
}

// NuevoPanelLocal arma el panel. `recargar` aplica el config nuevo: reinicia
// el agente por dentro, sin reiniciar el servicio.
func NuevoPanelLocal(cfg *Config, configPath, servicio string, estado *Estado, recargar func()) *PanelLocal {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err) // sin azar del sistema no hay nada seguro que hacer
	}
	return &PanelLocal{
		cfg:        cfg,
		configPath: configPath,
		servicio:   servicio,
		estado:     estado,
		ficha:      hex.EncodeToString(b),
		recargar:   recargar,
	}
}

func (p *PanelLocal) soloEstePC(w http.ResponseWriter, req *http.Request) bool {
	ip := net.ParseIP(clientIP(req))
	host := req.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	if ip == nil || !ip.IsLoopback() || (host != "localhost" && host != "127.0.0.1" && host != "::1") {
		http.NotFound(w, req)
		return false
	}
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	h.Set("X-Frame-Options", "DENY")
	// `same-origin` y no `no-referrer`: con `no-referrer` Chrome y Edge mandan
	// el formulario con `Origin: null` y guardar fallaba siempre con «el
	// formulario no viene de esta página» (2026-11-20). Hacia afuera no se
	// filtra nada igual: la página no enlaza a ningún otro sitio.
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Cache-Control", "no-store")
	return true
}

// vieneDeEstaPagina: el formulario lo mandó esta misma página, no otra que
// sepa que el panel escucha en localhost. Vale el `Origin` exacto, o —cuando
// el navegador lo manda como `null`, que pasa con algunas políticas de
// referer y extensiones— `Sec-Fetch-Site: same-origin`, que pone el propio
// navegador y ninguna página puede escribir. La ficha del formulario se
// exige además, aparte.
func vieneDeEstaPagina(req *http.Request) bool {
	origen := req.Header.Get("Origin")
	if origen == "http://"+req.Host {
		return true
	}
	return (origen == "" || origen == "null") && req.Header.Get("Sec-Fetch-Site") == "same-origin"
}

func (p *PanelLocal) handleInicio(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/" || req.Method != http.MethodGet {
		http.NotFound(w, req)
		return
	}
	if !p.soloEstePC(w, req) {
		return
	}
	p.pintar(w, http.StatusOK, "", "")
}

func (p *PanelLocal) handleGuardar(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.NotFound(w, req)
		return
	}
	if !p.soloEstePC(w, req) {
		return
	}
	if !vieneDeEstaPagina(req) {
		http.Error(w, "el formulario no viene de esta página", http.StatusForbidden)
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, 16*1024)
	if err := req.ParseForm(); err != nil {
		http.Error(w, "formulario inválido", http.StatusBadRequest)
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.PostForm.Get("ficha")), []byte(p.ficha)) != 1 {
		http.Error(w, "la página venció: recárgala", http.StatusForbidden)
		return
	}
	puerto, _ := strconv.Atoi(strings.TrimSpace(req.PostForm.Get("puerto")))
	cambios := CambiosDelEquipo{
		Host:     req.PostForm.Get("ip"),
		Port:     puerto,
		User:     req.PostForm.Get("usuario"),
		Password: req.PostForm.Get("clave"),
	}
	if err := actualizarEquipo(p.configPath, cambios); err != nil {
		p.pintar(w, http.StatusUnprocessableEntity, "", err.Error())
		return
	}
	aviso := "Guardado. El agente se reinicia y en unos segundos esta página muestra el estado nuevo."
	if nuevaIP := strings.TrimSpace(cambios.Host); nuevaIP != p.cfg.Camera.Host {
		if err := abrirFirewallPara(p.servicio, nuevaIP); err != nil {
			aviso += " Ojo: no se pudo actualizar el firewall de Windows (" + err.Error() +
				"). Hasta que alguien lo haga, el equipo no puede mandar sus lecturas."
		}
	}
	log.Printf("[panel] el técnico cambió el equipo desde el panel local: ip=%s puerto=%d usuario=%s clave_cambiada=%v",
		cambios.Host, cambios.Port, cambios.User, cambios.Password != "")
	p.pintar(w, http.StatusOK, aviso, "")
	// Después de contestar: la recarga apaga este mismo servidor.
	go func() {
		time.Sleep(500 * time.Millisecond)
		p.recargar()
	}()
}

// reglaDeFirewall es el nombre que le da el instalador a la regla de esta
// copia del agente. Tiene que coincidir con `install.ps1`.
func reglaDeFirewall(servicio string) string {
	const base = "Porteria Sync Agent - receptor LPR"
	if servicio == ServiceName {
		return base
	}
	return base + " (" + servicio + ")"
}

// abrirFirewallPara cambia la IP a la que la regla de esta copia le deja
// entrar. Sin esto, cambiar la IP del equipo en el panel dejaba el puerto
// cerrado para el equipo nuevo y las lecturas se perdían en silencio.
func abrirFirewallPara(servicio, ip string) error {
	if runtime.GOOS != "windows" {
		return nil
	}
	if net.ParseIP(ip) == nil {
		return fmt.Errorf("la regla necesita una IP, no un nombre")
	}
	out, err := exec.Command("netsh", "advfirewall", "firewall", "set", "rule",
		"name="+reglaDeFirewall(servicio), "new", "remoteip="+ip).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

type vistaEquipo struct {
	Nombre, Puerto, IP, Version, Modelo string
	Nube, NubeDetalle                   string
	NubeOK                              bool
	Equipo, EquipoDetalle               string
	EquipoOK                            bool
	Lectura                             string
	Propio                              bool
	Enlace                              string
	// Quitar es el comando para desinstalar solo esta copia, cuando la
	// plataforma ya no reconoce su llave. Vacío en cualquier otro caso.
	Quitar string
}

// nombreDeServicio es la forma que les da el instalador a las copias del
// agente. El nombre sale de estado.json, que otra copia escribe: solo se
// pone en un comando para pegar como administrador si tiene esta forma.
var nombreDeServicio = regexp.MustCompile(`^PorteriaSyncAgent(-[0-9]{1,3})?$`)

// comandoParaQuitar es la línea que quita solo la copia `servicio`, con el
// desinstalador que el instalador deja en la carpeta raíz (`raiz`).
func comandoParaQuitar(raiz, servicio string) string {
	if !nombreDeServicio.MatchString(servicio) || raiz == "" {
		return ""
	}
	return fmt.Sprintf(`powershell -ExecutionPolicy Bypass -File "%s" -Servicio %s`,
		filepath.Join(raiz, "uninstall.ps1"), servicio)
}

// vista arma lo que se pinta de una copia. `raiz` es la carpeta donde el
// instalador dejó el desinstalador (C:\PorteriaAgent).
func vista(d DatosDelEquipo, propio bool, raiz string) vistaEquipo {
	v := vistaEquipo{
		Nombre: nombreDelEquipo(d.Equipo), Puerto: d.Puerto, IP: d.IP,
		Version: d.Version, Modelo: d.Modelo, Propio: propio,
		Enlace: "http://localhost:" + d.Puerto + "/",
	}
	switch {
	case d.NubeOK == nil:
		v.Nube = "Conectando…"
	case *d.NubeOK:
		v.Nube, v.NubeOK = "Conectado", true
		v.NubeDetalle = hace(d.NubeEn)
	case d.LlaveRechazada:
		// No es la red: la plataforma ya no reconoce este equipo. Reintentar
		// no lo arregla, así que se dice qué pasó y cómo se quita esta copia.
		v.Nube = "La plataforma no reconoce este equipo"
		v.NubeDetalle = "Lo borraron en Cámaras de placas o le generaron un token nuevo, así que su llave ya no sirve."
		v.Quitar = comandoParaQuitar(raiz, d.Servicio)
	default:
		v.Nube, v.NubeDetalle = "Sin conexión", d.NubeError
	}
	switch {
	case d.EquipoOK == nil:
		v.Equipo = "Esperando la primera carga"
	case *d.EquipoOK:
		v.Equipo, v.EquipoOK = fmt.Sprintf("Lista cargada (%d)", d.Cargados), true
		v.EquipoDetalle = hace(d.EquipoEn)
	default:
		v.Equipo, v.EquipoDetalle = "No acepta la lista", d.EquipoError
	}
	if d.UltimaLectura.IsZero() {
		v.Lectura = "Ninguna desde que arrancó"
	} else {
		v.Lectura = hace(d.UltimaLectura)
	}
	// Una copia que lleva rato sin anotar nada está apagada o colgada.
	if !propio && time.Since(d.Actualizado) > 5*time.Minute {
		v.Nube, v.NubeOK, v.NubeDetalle = "Sin señal", false,
			"No ha dado señales "+hace(d.Actualizado)+": revisa que el servicio esté corriendo."
	}
	return v
}

func nombreDelEquipo(familia string) string {
	switch {
	case strings.Contains(familia, "dsk1t"):
		return "Terminal de la puerta (QR)"
	case familia == "":
		return "Equipo"
	default:
		return "Cámara de placas"
	}
}

func hace(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "hace un momento"
	case d < time.Hour:
		return fmt.Sprintf("hace %d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("hace %d h", int(d.Hours()))
	default:
		return t.Format("02/01/2006 15:04")
	}
}

func (p *PanelLocal) pintar(w http.ResponseWriter, status int, aviso, falla string) {
	raiz := carpetaRaiz(filepath.Dir(p.configPath))
	datos := map[string]any{
		"Propio":  vista(p.estado.Copia(), true, raiz),
		"Otros":   []vistaEquipo{},
		"IP":      p.cfg.Camera.Host,
		"Puerto":  p.cfg.Camera.Port,
		"Usuario": p.cfg.Camera.User,
		"Ficha":   p.ficha,
		"Aviso":   aviso,
		"Falla":   falla,
		"Version": AgentVersion,
	}
	var otros []vistaEquipo
	for _, d := range otrasCopias(filepath.Dir(p.configPath)) {
		otros = append(otros, vista(d, false, raiz))
	}
	datos["Otros"] = otros
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := plantillaPanel.Execute(w, datos); err != nil {
		log.Printf("[panel] no se pudo pintar: %v", err)
	}
}

var plantillaPanel = template.Must(template.New("panel").Parse(`<!doctype html>
<html lang="es"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
{{if .Aviso}}<meta http-equiv="refresh" content="8;url=/">{{end}}
<title>Portería Plus · Agente</title>
<style>
:root{--fondo:#f8fafc;--tarjeta:#fff;--borde:#e2e8f0;--texto:#0f172a;--suave:#64748b;--ok:#15803d;--okf:#dcfce7;--mal:#b91c1c;--malf:#fee2e2;--marca:#1d4ed8}
*{box-sizing:border-box}body{margin:0;background:var(--fondo);color:var(--texto);font:16px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif}
main{max-width:640px;margin:0 auto;padding:24px 16px 48px}
h1{font-size:22px;margin:0 0 4px}h2{font-size:17px;margin:0 0 12px}
.sub{color:var(--suave);margin:0 0 24px;font-size:14px}
.tarjeta{background:var(--tarjeta);border:1px solid var(--borde);border-radius:14px;padding:18px;margin-bottom:16px}
.fila{display:flex;justify-content:space-between;gap:12px;padding:8px 0;border-top:1px solid var(--borde)}
.fila:first-of-type{border-top:0}.fila span:first-child{color:var(--suave)}
.fila span:last-child{text-align:right}
.punto{display:inline-block;width:10px;height:10px;border-radius:50%;margin-right:6px;vertical-align:middle}
.ok{background:var(--ok)}.mal{background:var(--mal)}
.detalle{display:block;font-size:13px;color:var(--suave);word-break:break-word}
label{display:block;font-weight:600;font-size:14px;margin:14px 0 4px}
input{width:100%;padding:10px 12px;border:1px solid var(--borde);border-radius:10px;font:inherit}
.ayuda{font-size:13px;color:var(--suave);margin:4px 0 0}
button{margin-top:18px;width:100%;padding:12px;border:0;border-radius:10px;background:var(--marca);color:#fff;font:600 16px system-ui,sans-serif;cursor:pointer}
.aviso{background:var(--okf);color:var(--ok);border-radius:10px;padding:12px;margin-bottom:16px}
.falla{background:var(--malf);color:var(--mal);border-radius:10px;padding:12px;margin-bottom:16px}
a{color:var(--marca)}
.quitar{background:var(--malf);border-radius:10px;padding:12px;margin-top:8px;font-size:14px}
.quitar p{margin:0 0 8px}.quitar code{display:block;background:#fff;border:1px solid var(--borde);border-radius:8px;padding:8px;font:13px ui-monospace,Consolas,monospace;word-break:break-all;user-select:all}
</style></head><body><main>
<h1>Agente de portería</h1>
<p class="sub">Portería Plus · versión {{.Version}} · esta página solo se abre desde este computador</p>
{{if .Aviso}}<div class="aviso">{{.Aviso}}</div>{{end}}
{{if .Falla}}<div class="falla">No se guardó: {{.Falla}}</div>{{end}}
{{with .Propio}}
<section class="tarjeta"><h2>{{.Nombre}}{{if .Modelo}} · {{.Modelo}}{{end}}</h2>
<div class="fila"><span>Plataforma</span><span><i class="punto {{if .NubeOK}}ok{{else}}mal{{end}}"></i>{{.Nube}}<span class="detalle">{{.NubeDetalle}}</span></span></div>
{{if .Quitar}}<div class="quitar"><p>Si este equipo ya no se usa, o lo reemplazaste por otro, quita esta copia del agente: abre PowerShell <b>como administrador</b> y pega esta línea. Los demás equipos de este computador siguen funcionando.</p><code>{{.Quitar}}</code><p class="ayuda">Si el equipo sigue en uso, después pega el bloque nuevo que da el panel en Cámaras de placas.</p></div>{{end}}
<div class="fila"><span>Equipo ({{.IP}})</span><span><i class="punto {{if .EquipoOK}}ok{{else}}mal{{end}}"></i>{{.Equipo}}<span class="detalle">{{.EquipoDetalle}}</span></span></div>
<div class="fila"><span>Último aviso del equipo</span><span>{{.Lectura}}</span></div>
<div class="fila"><span>Puerto de este agente</span><span>{{.Puerto}}</span></div>
</section>
{{end}}
<section class="tarjeta"><h2>Datos del equipo</h2>
<form method="post" action="/guardar" autocomplete="off">
<input type="hidden" name="ficha" value="{{.Ficha}}">
<label for="ip">IP del equipo</label>
<input id="ip" name="ip" value="{{.IP}}" required inputmode="decimal">
<p class="ayuda">La que tiene el equipo en la red de la portería, por ejemplo 192.168.1.50.</p>
<label for="puerto">Puerto del equipo</label>
<input id="puerto" name="puerto" value="{{.Puerto}}" required inputmode="numeric">
<label for="usuario">Usuario del equipo</label>
<input id="usuario" name="usuario" value="{{.Usuario}}" required>
<label for="clave">Contraseña del equipo</label>
<input id="clave" name="clave" type="password" placeholder="Déjala vacía para no cambiarla">
<button type="submit">Guardar y reiniciar el agente</button>
</form></section>
{{if .Otros}}<h2>Otros equipos en este computador</h2>
{{range .Otros}}<section class="tarjeta"><h2>{{.Nombre}}{{if .Modelo}} · {{.Modelo}}{{end}}</h2>
<div class="fila"><span>Plataforma</span><span><i class="punto {{if .NubeOK}}ok{{else}}mal{{end}}"></i>{{.Nube}}<span class="detalle">{{.NubeDetalle}}</span></span></div>
{{if .Quitar}}<div class="quitar"><p>Si este equipo ya no se usa, o lo reemplazaste por otro, quita esta copia del agente: abre PowerShell <b>como administrador</b> y pega esta línea. Los demás equipos de este computador siguen funcionando.</p><code>{{.Quitar}}</code><p class="ayuda">Si el equipo sigue en uso, después pega el bloque nuevo que da el panel en Cámaras de placas.</p></div>{{end}}
<div class="fila"><span>Equipo ({{.IP}})</span><span><i class="punto {{if .EquipoOK}}ok{{else}}mal{{end}}"></i>{{.Equipo}}<span class="detalle">{{.EquipoDetalle}}</span></span></div>
<div class="fila"><span>Último aviso del equipo</span><span>{{.Lectura}}</span></div>
<div class="fila"><span>Para cambiar sus datos</span><span><a href="{{.Enlace}}">{{.Enlace}}</a></span></div>
</section>{{end}}{{end}}
</main></body></html>`))
