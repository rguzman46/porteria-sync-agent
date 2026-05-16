package main

import (
	"context"
	"testing"
)

// Tests del hot-swap automático del adapter cuando el cloud reporta una
// vendor_family distinta (V1.3 plug-and-play).
//
// Caso de uso: admin tiene una cámara Hikvision Traffic instalada. Compra
// una nueva DS-TCG405-E (línea ITC) y la cambia en el panel. El cloud
// reporta `device.vendor_family = "hikvision_itc"` en el siguiente heartbeat.
// El agent debe reconfigurar su adapter sin reiniciar el servicio.

func TestMaybeReconfigureAdapterSwapsOnFamilyChange(t *testing.T) {
	cfg := newCfg("hikvision", "hikvision_traffic")
	current, _ := NewCameraAdapter(cfg)

	s := &Syncer{
		cfg:    cfg,
		camera: current,
	}

	// Cloud reporta el cambio de familia.
	s.maybeReconfigureAdapter(&DeviceMetadata{
		ID:           1,
		DeviceType:   "hikvision",
		VendorFamily: "hikvision_itc",
		DeviceModel:  "DS-TCG405-E",
	})

	if s.currentCamera().Name() != "hikvision_itc" {
		t.Errorf("adapter no se reconfiguró: got %q, esperado hikvision_itc", s.currentCamera().Name())
	}
	if s.cfg.Camera.Family != "hikvision_itc" {
		t.Errorf("cfg.Camera.Family no se actualizó: %q", s.cfg.Camera.Family)
	}
}

func TestMaybeReconfigureAdapterNoSwapIfSameFamily(t *testing.T) {
	cfg := newCfg("hikvision", "hikvision_traffic")
	current, _ := NewCameraAdapter(cfg)

	s := &Syncer{
		cfg:    cfg,
		camera: current,
	}

	originalCamera := s.currentCamera()

	// Cloud reporta MISMA familia → no debe hacer swap.
	s.maybeReconfigureAdapter(&DeviceMetadata{
		VendorFamily: "hikvision_traffic",
	})

	if s.currentCamera() != originalCamera {
		t.Error("adapter se reconfiguró aunque la familia no cambió (debería ser no-op)")
	}
}

func TestMaybeReconfigureAdapterRespectsAutoConfigFalse(t *testing.T) {
	cfg := newCfg("hikvision", "hikvision_traffic")
	disabled := false
	cfg.Camera.AutoConfig = &disabled
	current, _ := NewCameraAdapter(cfg)

	s := &Syncer{
		cfg:    cfg,
		camera: current,
	}

	originalCamera := s.currentCamera()

	// Cloud reporta cambio, pero auto_config=false → debe ignorar.
	s.maybeReconfigureAdapter(&DeviceMetadata{
		VendorFamily: "hikvision_itc",
	})

	if s.currentCamera() != originalCamera {
		t.Error("adapter se reconfiguró con auto_config=false (debería preservar)")
	}
}

func TestMaybeReconfigureAdapterIgnoresEmptyFamily(t *testing.T) {
	cfg := newCfg("hikvision", "hikvision_traffic")
	current, _ := NewCameraAdapter(cfg)

	s := &Syncer{
		cfg:    cfg,
		camera: current,
	}

	originalCamera := s.currentCamera()

	// Cloud responde sin vendor_family (caso config legacy o devices sin modelo).
	s.maybeReconfigureAdapter(&DeviceMetadata{VendorFamily: ""})

	if s.currentCamera() != originalCamera {
		t.Error("adapter se reconfiguró con family vacío")
	}
}

func TestMaybeReconfigureAdapterIgnoresNilMetadata(t *testing.T) {
	cfg := newCfg("hikvision", "hikvision_traffic")
	current, _ := NewCameraAdapter(cfg)
	s := &Syncer{cfg: cfg, camera: current}

	// nil metadata (caso defensivo) — no debe panic ni hacer cambios.
	s.maybeReconfigureAdapter(nil)

	if s.currentCamera() != current {
		t.Error("adapter se reconfiguró con metadata nil")
	}
}

func TestMaybeReconfigureAdapterIgnoresUnknownFamily(t *testing.T) {
	cfg := newCfg("hikvision", "hikvision_traffic")
	current, _ := NewCameraAdapter(cfg)
	s := &Syncer{cfg: cfg, camera: current}

	originalCamera := s.currentCamera()

	// Cloud reporta una familia que no conocemos (ej. release nuevo del
	// cloud con un vendor que este agent aún no soporta). Debe mantener el
	// adapter actual y NO crashear.
	s.maybeReconfigureAdapter(&DeviceMetadata{
		VendorFamily: "vendor_del_futuro",
	})

	if s.currentCamera() != originalCamera {
		t.Error("adapter cambió a uno desconocido — debería preservar el actual")
	}
}

// Compile-check: el Syncer struct debe implementar correctamente
// el flujo nuevo de Heartbeat (verificación estática, no test runtime).
var _ = func() bool {
	var s *Syncer
	if s != nil {
		_ = s.currentCamera
		_ = s.maybeReconfigureAdapter
	}
	_ = context.Background()
	return true
}()
