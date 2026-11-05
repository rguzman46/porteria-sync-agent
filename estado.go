package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Estado es lo que el panel local muestra de este equipo: si la nube
// contesta, si el equipo acepta la lista y cuándo fue la última lectura. Lo
// escriben el syncer y el receiver; lo lee el panel.
//
// Se guarda además en `estado.json`, junto al config.yaml, para que el panel
// de **otra** copia del agente en el mismo PC lo pueda mostrar: el técnico
// abre una sola página y ve los dos equipos (la reja y el terminal).
type Estado struct {
	mu      sync.Mutex
	archivo string
	d       DatosDelEquipo
}

// DatosDelEquipo es lo que se pinta y lo que va en estado.json.
type DatosDelEquipo struct {
	Servicio string `json:"servicio"`
	Puerto   string `json:"puerto"`
	Equipo   string `json:"equipo"` // marca o familia, como está en el config
	Modelo   string `json:"modelo,omitempty"`
	IP       string `json:"ip"`
	Version  string `json:"version"`

	NubeOK    *bool     `json:"nube_ok,omitempty"`
	NubeError string    `json:"nube_error,omitempty"`
	NubeEn    time.Time `json:"nube_en,omitempty"`

	EquipoOK    *bool     `json:"equipo_ok,omitempty"`
	EquipoError string    `json:"equipo_error,omitempty"`
	EquipoEn    time.Time `json:"equipo_en,omitempty"`
	Cargados    int       `json:"cargados"`

	UltimaLectura time.Time `json:"ultima_lectura,omitempty"`
	Actualizado   time.Time `json:"actualizado"`
}

// NuevoEstado arma el estado de esta copia del agente. `dir` es la carpeta
// de su config.yaml; vacío, no se guarda en disco (pruebas).
func NuevoEstado(cfg *Config, servicio, dir string) *Estado {
	e := &Estado{d: DatosDelEquipo{
		Servicio: servicio,
		Puerto:   puertoDe(cfg.Receiver.BindAddress),
		Equipo:   equipoDe(cfg),
		IP:       cfg.Camera.Host,
		Version:  AgentVersion,
	}}
	if dir != "" {
		e.archivo = filepath.Join(dir, "estado.json")
	}
	e.guardar()
	return e
}

func equipoDe(cfg *Config) string {
	if cfg.Camera.Family != "" {
		return cfg.Camera.Family
	}
	return cfg.Camera.Type
}

func puertoDe(bind string) string {
	if i := strings.LastIndex(bind, ":"); i >= 0 {
		return bind[i+1:]
	}
	return bind
}

// Nube anota cómo salió la última conversación con la plataforma.
func (e *Estado) Nube(err error) {
	if e == nil {
		return
	}
	e.mu.Lock()
	ok := err == nil
	e.d.NubeOK, e.d.NubeEn, e.d.NubeError = &ok, time.Now(), ""
	if err != nil {
		e.d.NubeError = err.Error()
	}
	e.mu.Unlock()
	e.guardar()
}

// EquipoCargado anota cómo salió la última carga de la lista al equipo.
func (e *Estado) EquipoCargado(err error, cargados int) {
	if e == nil {
		return
	}
	e.mu.Lock()
	ok := err == nil
	e.d.EquipoOK, e.d.EquipoEn, e.d.EquipoError = &ok, time.Now(), ""
	if err != nil {
		e.d.EquipoError = err.Error()
	} else {
		e.d.Cargados = cargados
	}
	e.mu.Unlock()
	e.guardar()
}

// ConModelo anota el modelo que la plataforma dice que tiene este equipo.
func (e *Estado) ConModelo(modelo string) {
	if e == nil || modelo == "" {
		return
	}
	e.mu.Lock()
	cambio := e.d.Modelo != modelo
	e.d.Modelo = modelo
	e.mu.Unlock()
	if cambio {
		e.guardar()
	}
}

// Lectura anota que el equipo mandó una lectura.
func (e *Estado) Lectura() {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.d.UltimaLectura = time.Now()
	e.mu.Unlock()
	e.guardar()
}

// Copia devuelve una foto del estado para pintarla sin sostener el candado.
func (e *Estado) Copia() DatosDelEquipo {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.d
}

// guardar escribe estado.json. Si falla no pasa nada: es para que lo vea el
// panel de otra copia, no para operar.
func (e *Estado) guardar() {
	if e.archivo == "" {
		return
	}
	e.mu.Lock()
	e.d.Actualizado = time.Now()
	data, err := json.MarshalIndent(e.d, "", "  ")
	e.mu.Unlock()
	if err != nil {
		return
	}
	tmp := e.archivo + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, e.archivo)
	}
}

// carpetaRaiz es la carpeta donde viven todas las copias del agente en este
// PC: la primera en la raíz (`C:\PorteriaAgent`) y las demás en
// `equipo-N` adentro.
func carpetaRaiz(dir string) string {
	if strings.HasPrefix(strings.ToLower(filepath.Base(dir)), "equipo-") {
		return filepath.Dir(dir)
	}
	return dir
}

// otrasCopias lee el estado de las demás copias del agente en este PC.
func otrasCopias(dir string) []DatosDelEquipo {
	raiz := carpetaRaiz(dir)
	candidatos := []string{filepath.Join(raiz, "estado.json")}
	if extra, err := filepath.Glob(filepath.Join(raiz, "equipo-*", "estado.json")); err == nil {
		candidatos = append(candidatos, extra...)
	}
	propio := filepath.Clean(filepath.Join(dir, "estado.json"))
	var out []DatosDelEquipo
	for _, ruta := range candidatos {
		if filepath.Clean(ruta) == propio {
			continue
		}
		data, err := os.ReadFile(ruta)
		if err != nil {
			continue
		}
		var e DatosDelEquipo
		if json.Unmarshal(data, &e) == nil && e.Puerto != "" {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Puerto < out[j].Puerto })
	return out
}
