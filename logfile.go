package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// archivoRotativo es un io.Writer que escribe en un archivo y, cuando pasa
// de `maxBytes`, lo rota a `.1`, `.2`… conservando `keep` copias.
//
// Sin dependencias: lumberjack pesa poco pero es una librería más para
// auditar y este agente entra en un PC que nadie actualiza. La rotación
// por tamaño es la única que hace falta —el disco es lo que se llena—; por
// fecha no aporta nada en un servicio que corre meses seguidos.
type archivoRotativo struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int
	f        *os.File
	size     int64
}

func abrirLogRotativo(path string, maxBytes int64, keep int) (*archivoRotativo, error) {
	if keep < 1 {
		keep = 1
	}
	r := &archivoRotativo{path: path, maxBytes: maxBytes, keep: keep}
	if err := r.abrir(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *archivoRotativo) abrir() error {
	// 0600: el log lleva placas y nombres de residentes.
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	r.f = f
	r.size = info.Size()
	return nil
}

func (r *archivoRotativo) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.size+int64(len(p)) > r.maxBytes && r.size > 0 {
		if err := r.rotar(); err != nil {
			// Se sigue escribiendo en el archivo actual antes que perder el
			// log: un rename que falla (antivirus con el archivo abierto) no
			// justifica quedarse ciego.
			fmt.Fprintf(os.Stderr, "[log] no se pudo rotar %s: %v\n", r.path, err)
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// rotar desplaza agent.log.2 → agent.log.3, agent.log.1 → agent.log.2,
// agent.log → agent.log.1 y abre uno nuevo. La copia más vieja se pierde.
func (r *archivoRotativo) rotar() error {
	_ = r.f.Close()
	for i := r.keep - 1; i >= 1; i-- {
		de := fmt.Sprintf("%s.%d", r.path, i)
		a := fmt.Sprintf("%s.%d", r.path, i+1)
		if _, err := os.Stat(de); err == nil {
			_ = os.Rename(de, a)
		}
	}
	if err := os.Rename(r.path, r.path+".1"); err != nil {
		// Se reabre el mismo para no dejar r.f cerrado.
		_ = r.abrir()
		return err
	}
	return r.abrir()
}

func (r *archivoRotativo) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}

// configurarLog manda la salida del log al archivo configurado además de
// stderr (que es lo que captura el gestor de servicios). Relativo → junto al
// config.yaml. Devuelve lo que hay que cerrar al salir.
func configurarLog(cfg *Config, configPath string) (io.Closer, error) {
	if cfg.Log.File == "" {
		return nil, nil
	}
	ruta := cfg.Log.File
	if !filepath.IsAbs(ruta) {
		ruta = filepath.Join(filepath.Dir(configPath), ruta)
	}
	rot, err := abrirLogRotativo(ruta, int64(cfg.Log.MaxSizeMB)*1024*1024, cfg.Log.Keep)
	if err != nil {
		return nil, fmt.Errorf("abriendo log %s: %w", ruta, err)
	}
	log.SetOutput(io.MultiWriter(os.Stderr, rot))
	return rot, nil
}
