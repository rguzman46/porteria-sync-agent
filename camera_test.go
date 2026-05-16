package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Tests del factory de adapters + resolución de vendor_family.
// Asegura retrocompat con configs legacy (`type: hikvision`) y a la vez
// habilita el nuevo formato V1.3 con familia explícita.

func newCfg(camType, camFamily string) *Config {
	cfg := &Config{}
	cfg.Camera.Type = camType
	cfg.Camera.Family = camFamily
	cfg.Camera.Host = "192.168.1.50"
	cfg.Camera.Port = 80
	cfg.Camera.User = "admin"
	cfg.Camera.Password = "secret"
	return cfg
}

func TestResolveVendorFamilyExplicit(t *testing.T) {
	cfg := newCfg("hikvision", "hikvision_itc")
	got := resolveVendorFamily(cfg)
	if got != "hikvision_itc" {
		t.Errorf("expected hikvision_itc (explicit family wins), got %q", got)
	}
}

func TestResolveVendorFamilyTypeWithUnderscore(t *testing.T) {
	cfg := newCfg("hikvision_itc", "")
	got := resolveVendorFamily(cfg)
	if got != "hikvision_itc" {
		t.Errorf("expected hikvision_itc (type already has family form), got %q", got)
	}
}

func TestResolveVendorFamilyHikvisionDefault(t *testing.T) {
	cfg := newCfg("hikvision", "")
	got := resolveVendorFamily(cfg)
	if got != "hikvision_traffic" {
		t.Errorf("expected hikvision_traffic (default for hikvision), got %q", got)
	}
}

func TestResolveVendorFamilyDahuaDefault(t *testing.T) {
	cfg := newCfg("dahua", "")
	got := resolveVendorFamily(cfg)
	if got != "dahua_itc" {
		t.Errorf("expected dahua_itc, got %q", got)
	}
}

func TestResolveVendorFamilyAxisDefault(t *testing.T) {
	cfg := newCfg("axis", "")
	got := resolveVendorFamily(cfg)
	if got != "axis_vapix" {
		t.Errorf("expected axis_vapix, got %q", got)
	}
}

func TestFactoryAcceptsAllSupportedFamilies(t *testing.T) {
	families := []string{"hikvision_traffic", "hikvision_itc", "dahua_itc", "axis_vapix"}
	for _, f := range families {
		t.Run(f, func(t *testing.T) {
			cfg := newCfg("hikvision", f)
			adapter, err := NewCameraAdapter(cfg)
			if err != nil {
				t.Fatalf("NewCameraAdapter(%s) falló: %v", f, err)
			}
			if adapter.Name() != f {
				t.Errorf("adapter.Name() = %q, esperado %q", adapter.Name(), f)
			}
		})
	}
}

func TestFactoryRejectsUnknownFamily(t *testing.T) {
	cfg := newCfg("hikvision", "vendor_inexistente")
	_, err := NewCameraAdapter(cfg)
	if err == nil {
		t.Fatal("esperaba error para vendor_family desconocido, got nil")
	}
}

// Tests de los XML builders — verifican el formato esperado por cada cámara.

func TestBuildHikvisionTrafficPlatesXML(t *testing.T) {
	plates := []Plate{
		{Plate: "ABC123", ValidUntil: "2026-12-31T23:59:59Z"},
		{Plate: "DEF456"},
	}
	xml := buildHikvisionTrafficPlatesXML(plates)

	mustContain(t, xml, `<PlateInfoList version="2.0">`)
	mustContain(t, xml, `<plateNumber>ABC123</plateNumber>`)
	mustContain(t, xml, `<plateNumber>DEF456</plateNumber>`)
	mustContain(t, xml, `<plateType>0</plateType>`)
	mustContain(t, xml, `<effectivePeriod><endTime>2026-12-31T23:59:59Z</endTime></effectivePeriod>`)
}

func TestBuildHikvisionITCVehicleListXML(t *testing.T) {
	plates := []Plate{
		{Plate: "DSE001", Owner: "Juan", ValidFrom: "2026-05-15T07:00:00", ValidUntil: "2026-05-15T17:00:00"},
		{Plate: "EMP999"},
	}
	xml := buildHikvisionITCVehicleListXML(plates)

	mustContain(t, xml, `<VehicleControlList version="2.0">`)
	mustContain(t, xml, `<plateNumber>DSE001</plateNumber>`)
	mustContain(t, xml, `<plateNumber>EMP999</plateNumber>`)
	mustContain(t, xml, `<plateType>0</plateType>`)
	mustContain(t, xml, `<enabled>true</enabled>`)
	mustContain(t, xml, `<beginTime>2026-05-15T07:00:00</beginTime>`)
	mustContain(t, xml, `<endTime>2026-05-15T17:00:00</endTime>`)
	// Sin vigencia explícita → defaults amplios
	mustContain(t, xml, `<beginTime>1970-01-01T00:00:00</beginTime>`)
	mustContain(t, xml, `<endTime>2099-12-31T23:59:59</endTime>`)
}

func TestTrimZuluForITC(t *testing.T) {
	cases := map[string]string{
		"2026-05-15T10:00:00Z":      "2026-05-15T10:00:00",
		"2026-05-15T10:00:00+00:00": "2026-05-15T10:00:00",
		"2026-05-15T10:00:00":      "2026-05-15T10:00:00",
		"":                         "",
	}
	for in, want := range cases {
		got := trimZuluForITC(in)
		if got != want {
			t.Errorf("trimZuluForITC(%q) = %q, esperado %q", in, got, want)
		}
	}
}

func TestNormalizeDahuaTime(t *testing.T) {
	cases := map[string]string{
		"2026-05-15T10:00:00Z":      "2026-05-15 10:00:00",
		"2026-05-15T10:00:00+00:00": "2026-05-15 10:00:00",
		"2026-05-15 10:00:00":       "2026-05-15 10:00:00",
		"":                         "",
	}
	for in, want := range cases {
		got := normalizeDahuaTime(in)
		if got != want {
			t.Errorf("normalizeDahuaTime(%q) = %q, esperado %q", in, got, want)
		}
	}
}

// Test integración Hikvision ITC contra httptest.Server simulando una cámara.

func TestHikvisionITCAdapterPingOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ISAPI/System/deviceInfo" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`<DeviceInfo><deviceName>DS-TCG405-E</deviceName></DeviceInfo>`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	host, port := splitHostPort(t, srv.URL)
	a := NewHikvisionITCAdapter(host, port, "admin", "x")
	if err := a.Ping(testContext()); err != nil {
		t.Fatalf("Ping inesperadamente falló: %v", err)
	}
}

func TestHikvisionITCAdapterSyncWhitelistEndpoint(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotBody   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		gotBody = string(b[:n])
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	host, port := splitHostPort(t, srv.URL)
	a := NewHikvisionITCAdapter(host, port, "admin", "x")
	plates := []Plate{{Plate: "TST123", ValidUntil: "2026-12-31T23:59:59Z"}}

	if err := a.SyncWhitelist(testContext(), plates); err != nil {
		t.Fatalf("SyncWhitelist falló: %v", err)
	}

	if gotMethod != http.MethodPut {
		t.Errorf("esperaba PUT, recibí %s", gotMethod)
	}
	if gotPath != "/ISAPI/ITC/Entrance/VCL" {
		t.Errorf("endpoint incorrecto: %q (esperaba /ISAPI/ITC/Entrance/VCL)", gotPath)
	}
	mustContain(t, gotBody, `<VehicleControlList`)
	mustContain(t, gotBody, `<plateNumber>TST123</plateNumber>`)
}

func TestDahuaAdapterPingEndpoint(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("deviceType=ITC215\nserialNumber=ABC"))
	}))
	defer srv.Close()

	host, port := splitHostPort(t, srv.URL)
	a := NewDahuaITCAdapter(host, port, "admin", "x")
	if err := a.Ping(testContext()); err != nil {
		t.Fatalf("Ping falló: %v", err)
	}
	if !strings.HasPrefix(gotPath, "/cgi-bin/magicBox.cgi") {
		t.Errorf("endpoint Dahua incorrecto: %q", gotPath)
	}
}

func TestAxisAdapterPingEndpoint(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	host, port := splitHostPort(t, srv.URL)
	a := NewAxisVapixAdapter(host, port, "admin", "x")
	if err := a.Ping(testContext()); err != nil {
		t.Fatalf("Ping falló: %v", err)
	}
	if gotPath != "/axis-cgi/basicdeviceinfo.cgi" {
		t.Errorf("endpoint Axis incorrecto: %q", gotPath)
	}
}

func TestAxisAdapterDetectsACAPNotInstalled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/local/lpv/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	host, port := splitHostPort(t, srv.URL)
	a := NewAxisVapixAdapter(host, port, "admin", "x")

	err := a.SyncWhitelist(testContext(), []Plate{{Plate: "X"}})
	if err == nil {
		t.Fatal("esperaba error porque ACAP LPV no responde")
	}
	if !strings.Contains(err.Error(), "ACAP LPV no instalado") {
		t.Errorf("error no indica falta de ACAP: %v", err)
	}
}
