package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const hashFalso = "60b3c7f8152e1cbe736713242d79e7bbc062c4b34fb996e2ccbf71c95ed6ef30"

// publicarLosDeVerdad arma los guiones del repositorio, no unos de juguete:
// lo que se prueba es lo que va a salir en la release.
func publicarLosDeVerdad(t *testing.T) (instalar, desinstalar string) {
	t.Helper()
	out := t.TempDir()
	if err := publicar(filepath.Join("..", "..", "scripts"), out, "9.8.7", hashFalso); err != nil {
		t.Fatal(err)
	}
	leer := func(n string) string {
		b, err := os.ReadFile(filepath.Join(out, n))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	return leer("install.ps1"), leer("uninstall.ps1")
}

func TestLosGuionesPublicadosSonASCII(t *testing.T) {
	instalar, desinstalar := publicarLosDeVerdad(t)
	for nombre, texto := range map[string]string{"install.ps1": instalar, "uninstall.ps1": desinstalar} {
		for i, c := range []byte(texto) {
			if c > 0x7F {
				t.Fatalf("%s: byte 0x%02X en la posición %d; Windows PowerShell 5.1 lo leería mal", nombre, c, i)
			}
		}
	}
}

func TestLasPreguntasConservanSusTildes(t *testing.T) {
	instalar, _ := publicarLosDeVerdad(t)
	// «Contraseña admin de la cámara»: ñ = 0x00F1, á = 0x00E1.
	if !strings.Contains(instalar, `Contrase$([char]0x00F1)a admin de la c$([char]0x00E1)mara`) {
		t.Fatal("la pregunta de la contraseña no conserva la ñ ni la á")
	}
}

func TestNoQuedanMarcadores(t *testing.T) {
	instalar, desinstalar := publicarLosDeVerdad(t)
	if m := regexp.MustCompile(`__[A-Z_]+__`).FindString(instalar + desinstalar); m != "" {
		t.Fatalf("quedó %s sin reemplazar", m)
	}
	if !strings.Contains(instalar, "agent-v9.8.7/porteria-agent.exe") || !strings.Contains(instalar, hashFalso) {
		t.Fatal("el instalador no lleva la versión o el hash de la release")
	}
}

// La v1.7.0 comparaba contra el marcador; al reemplazarlo, la comprobación
// quedó `-like '*1.7.0*'` y el instalador se negaba a correr siempre.
func TestLaReleaseNoSeReconoceComoFuente(t *testing.T) {
	instalar, _ := publicarLosDeVerdad(t)
	url := regexp.MustCompile(`\$BinaryUrl = '([^']+)'`).FindStringSubmatch(instalar)
	if url == nil {
		t.Fatal("no se encontró el BinaryUrl por defecto")
	}
	if strings.Contains(url[1], "/agent-v_") {
		t.Fatalf("la URL publicada %q dispara la comprobación de «no viene de una release»", url[1])
	}
	if !strings.Contains(instalar, `$BinaryUrl -like '*/agent-v_*'`) {
		t.Fatal("la comprobación de la release cambió: revisar que no compare contra su propia versión")
	}
	if strings.Contains(instalar, "-ne '"+hashFalso+"'") {
		t.Fatal("la verificación del hash compara contra el hash inyectado y se salta siempre")
	}
}

// El bloque del panel corre el guion dentro de la ventana de quien instala:
// un `exit` la cierra y el mensaje de error no se alcanza a leer.
func TestNingunGuionCierraLaVentana(t *testing.T) {
	instalar, desinstalar := publicarLosDeVerdad(t)
	reExit := regexp.MustCompile(`(?mi)^[^#\r\n]*\bexit\b`)
	for nombre, texto := range map[string]string{"install.ps1": instalar, "uninstall.ps1": desinstalar} {
		if m := reExit.FindString(texto); m != "" {
			t.Errorf("%s usa exit (%q): con el bloque del panel cierra la ventana; usar return", nombre, strings.TrimSpace(m))
		}
	}
}

func TestElInstaladorLlevaElHashDelDesinstaladorPublicado(t *testing.T) {
	instalar, desinstalar := publicarLosDeVerdad(t)
	suma := sha256.Sum256([]byte(desinstalar))
	if !strings.Contains(instalar, "'"+hex.EncodeToString(suma[:])+"'") {
		t.Fatal("el hash del desinstalador no corresponde con el archivo publicado")
	}
}

func TestASCII(t *testing.T) {
	casos := []struct{ entrada, salida string }{
		{`Write-Host "año"`, `Write-Host "a$([char]0x00F1)o"`},
		{"# canción\n", "# cancion\r\n"},
		{"<# “cita” — ok #>", `<# "cita" - ok #>`},
		{"$x = @\"\nmañana\n\"@", "$x = @\"\r\nma$([char]0x00F1)ana\r\n\"@"},
		{`Write-Host "a ""ñ"" b"`, `Write-Host "a ""$([char]0x00F1)"" b"`},
		{"Write-Host \"x\" # ñ", "Write-Host \"x\" # n"},
		{`Write-Host "it's ñ"`, `Write-Host "it's $([char]0x00F1)"`},
	}
	for _, c := range casos {
		got, err := aASCII(c.entrada)
		if err != nil {
			t.Fatalf("%q: %v", c.entrada, err)
		}
		if got != c.salida {
			t.Errorf("%q\n  dio    %q\n  quería %q", c.entrada, got, c.salida)
		}
	}
}

// En comillas simples o en código no hay forma de escribirlo en ASCII sin
// cambiar lo que hace: se niega en vez de publicar algo distinto.
func TestSeNiegaDondeNoHayFormaSegura(t *testing.T) {
	for _, entrada := range []string{`$a = 'ñ'`, "$x = @'\nñ\n'@", `$cañon = 1`} {
		if _, err := aASCII(entrada); err == nil {
			t.Errorf("%q debió fallar", entrada)
		}
	}
}
