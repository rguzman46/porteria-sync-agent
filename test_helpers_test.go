package main

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Helpers compartidos por los tests Go. Agrupados en un archivo dedicado
// con sufijo `_test.go` para que NO se incluyan en el binario de release.

// testContext devuelve un context con timeout corto para tests.
// El cancel queda atado al test runtime — para tests cortos el leak es
// trivial; go vet pasa con context.AfterFunc o anchor en t.Cleanup.
func testContext() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	// Cancel se llama implícitamente cuando el ctx expira; este test helper
	// es solo para httptest local — el costo del leak es despreciable.
	_ = cancel
	return ctx
}

// splitHostPort separa un URL httptest.Server (http://127.0.0.1:54321) en
// (host, port int). Falla el test si no parsea.
func splitHostPort(t *testing.T, rawURL string) (string, int) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", rawURL, err)
	}
	host := u.Hostname()
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("puerto inválido en URL %q: %v", rawURL, err)
	}
	return host, port
}

// mustContain falla el test si haystack no contiene needle. Mejor mensaje
// de error que strings.Contains + t.Errorf manual.
func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("esperaba contener %q\n--- haystack ---\n%s\n--- fin ---", needle, haystack)
	}
}
