package main

// Prueba manual contra un servidor real, no contra un servidor falso.
//
// Es la única que demuestra que el agent y la plataforma **hablan el mismo
// idioma**: que los nombres de los campos coinciden de verdad. Los otros tests
// usan un servidor de mentira que acepta lo que sea, así que un error de
// tipeo en `camera_push_ok` pasaría desapercibido por los dos lados.
//
// No corre en CI: necesita el backend levantado y credenciales reales. Se
// invoca a mano:
//
//	PP_URL=http://demo.localhost:8000 PP_KEY=ppk_… PP_DEVICE=ppd_… \
//	  go test -run TestContraPlataformaReal -v
//
// Sin esas variables se salta sola.

import (
	"context"
	"os"
	"testing"
)

func TestContraPlataformaReal(t *testing.T) {
	url, key, device := os.Getenv("PP_URL"), os.Getenv("PP_KEY"), os.Getenv("PP_DEVICE")
	if url == "" || key == "" || device == "" {
		t.Skip("faltan PP_URL, PP_KEY y PP_DEVICE — es una prueba manual")
	}

	cliente := NewCloudClient(url, key)
	cliente.deviceToken = device

	fallo := false
	casos := []struct {
		nombre  string
		reporte HeartbeatReport
	}{
		{"recién instalado", HeartbeatReport{}},
		{"la cámara rechazó", HeartbeatReport{
			CameraPushOK:    &fallo,
			CameraPushError: "401 Unauthorized — Digest rechazado",
			QueueSize:       12,
		}},
		{"todo bien", func() HeartbeatReport {
			ok := true
			return HeartbeatReport{CameraPushOK: &ok, PlatesPushed: 412}
		}()},
	}

	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			res, err := cliente.Heartbeat(context.Background(), c.reporte)
			if err != nil {
				t.Fatalf("la plataforma rechazó el latido: %v", err)
			}
			if !res.OK {
				t.Errorf("la plataforma respondió ok=false")
			}
			// El agent usa esto para enterarse de un cambio sin esperar al
			// siguiente sondeo de la lista completa.
			if res.WhitelistVersion == "" {
				t.Errorf("la plataforma no devolvió whitelist_version")
			}
		})
	}
}
