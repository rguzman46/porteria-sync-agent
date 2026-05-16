package main

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
)

// digestClient hace HTTP requests con autenticación Digest (RFC 2617).
//
// Diseño compartido — los 3 vendors de cámara LPR principales usan Digest
// auth con realm específico (Hikvision Traffic, Hikvision ITC, Dahua y Axis
// para algunos endpoints). Mantener este helper aislado de cada adapter
// evita duplicar ~120 LoC de parsing del challenge + cálculo del MD5
// response.
//
// El flow es:
//  1. Primera request sin auth → cámara retorna 401 con WWW-Authenticate.
//  2. Parseamos el challenge (realm + nonce + algorithm + opaque + qop).
//  3. Reenviamos con header `Authorization: Digest ...` calculado.
//
// Atomic: no comparte state entre llamadas, seguro de usar concurrentemente.
type digestClient struct {
	user     string
	password string
	http     *http.Client
}

func newDigestClient(user, password string, httpClient *http.Client) *digestClient {
	return &digestClient{
		user:     user,
		password: password,
		http:     httpClient,
	}
}

// Do ejecuta `req` con Digest auth. Si el body no es nil, se lee y clona
// para poder re-enviarlo tras el 401. Si la cámara responde algo distinto
// a 401, se devuelve tal cual (incluye 200 sin auth required, raro pero
// posible en algunas líneas anteriores de firmware).
func (d *digestClient) Do(req *http.Request) (*http.Response, error) {
	// Clonamos el body para poder re-enviarlo tras el 401 challenge.
	var bodyBytes []byte
	if req.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		req.ContentLength = int64(len(bodyBytes))
	}

	// Primera tentativa sin auth.
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil // OK o error real distinto a 401
	}

	// Parseamos el challenge del header WWW-Authenticate.
	authHeader := resp.Header.Get("WWW-Authenticate")
	resp.Body.Close()
	if !strings.HasPrefix(authHeader, "Digest ") {
		return nil, fmt.Errorf("cámara no devolvió Digest challenge: %q", authHeader)
	}
	challenge := parseDigestChallenge(authHeader)

	// Segunda tentativa con Authorization calculado.
	req2, err := http.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req2.Header = req.Header.Clone()
	req2.Header.Set("Authorization", d.buildDigestHeader(challenge, req.Method, req.URL.RequestURI()))

	return d.http.Do(req2)
}

func parseDigestChallenge(header string) map[string]string {
	result := map[string]string{}
	body := strings.TrimPrefix(header, "Digest ")
	for _, part := range splitDigestParts(body) {
		eq := strings.IndexByte(part, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(part[:eq])
		val := strings.Trim(strings.TrimSpace(part[eq+1:]), `"`)
		result[key] = val
	}
	return result
}

// splitDigestParts hace split por comas respetando valores entre comillas.
// El parsing custom es necesario porque `strings.Split(s, ",")` rompería
// valores que contienen comas (ej. realm con espacios y comas).
func splitDigestParts(s string) []string {
	var parts []string
	var current strings.Builder
	inQuotes := false
	for _, r := range s {
		if r == '"' {
			inQuotes = !inQuotes
			current.WriteRune(r)
			continue
		}
		if r == ',' && !inQuotes {
			parts = append(parts, strings.TrimSpace(current.String()))
			current.Reset()
			continue
		}
		current.WriteRune(r)
	}
	if current.Len() > 0 {
		parts = append(parts, strings.TrimSpace(current.String()))
	}
	return parts
}

func (d *digestClient) buildDigestHeader(c map[string]string, method, uri string) string {
	realm := c["realm"]
	nonce := c["nonce"]
	qop := c["qop"]
	opaque := c["opaque"]
	algorithm := c["algorithm"]
	if algorithm == "" {
		algorithm = "MD5"
	}

	cnonce := randomHex(8)
	nc := "00000001"

	ha1 := md5hex(d.user + ":" + realm + ":" + d.password)
	ha2 := md5hex(method + ":" + uri)

	var response string
	if qop != "" {
		response = md5hex(ha1 + ":" + nonce + ":" + nc + ":" + cnonce + ":" + qop + ":" + ha2)
	} else {
		response = md5hex(ha1 + ":" + nonce + ":" + ha2)
	}

	var b strings.Builder
	fmt.Fprintf(&b, `Digest username="%s", realm="%s", nonce="%s", uri="%s", algorithm=%s, response="%s"`,
		d.user, realm, nonce, uri, algorithm, response)
	if qop != "" {
		fmt.Fprintf(&b, `, qop=%s, nc=%s, cnonce="%s"`, qop, nc, cnonce)
	}
	if opaque != "" {
		fmt.Fprintf(&b, `, opaque="%s"`, opaque)
	}
	return b.String()
}

func md5hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

func randomHex(bytes int) string {
	buf := make([]byte, bytes)
	rand.Read(buf) // Para cnonce no necesitamos CSPRNG — la cámara lo trata como opaque token.
	return hex.EncodeToString(buf)
}
