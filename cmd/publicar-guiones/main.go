// Command publicar-guiones arma los install.ps1 y uninstall.ps1 que van en
// una release: les pone la versión y los hashes, y los deja en ASCII puro.
//
//	go run ./cmd/publicar-guiones -version 1.7.1 -hash <sha256 del .exe> -out bin
//
// # Por qué ASCII
//
// Windows PowerShell 5.1 —el que trae Windows 10 y 11— adivina la
// codificación de un guion: un archivo UTF-8 sin BOM lo lee como Windows-1252
// y un `irm` sin charset lo lee como Latin-1. Con tildes, rayas y símbolos
// («—», «…», «✓») la primera lectura convierte algunos bytes en comillas y el
// guion ni siquiera arranca («The string is missing the terminator»); la
// segunda arranca pero pregunta «ContraseÃ±a». Le pasó a la v1.7.0 con el
// archivo bajado de GitHub (2026-11-20).
//
// Un archivo ASCII se lee igual con cualquier codificación. El fuente sigue
// escrito en español con sus tildes; lo que cambia es lo que se publica:
//
//   - dentro de un texto entre comillas dobles (lo que ve quien instala), cada
//     carácter se escribe `$([char]0x00F1)`, que PowerShell convierte de
//     vuelta en «ñ» al mostrarlo;
//   - en un comentario se quita la tilde: nadie lo ve al instalar;
//   - en cualquier otro sitio (comillas simples, código) no hay forma segura
//     de escribirlo, y la herramienta se niega en vez de publicar algo roto.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Marcadores que CI reemplaza. Un guion no debe compararse contra ellos: el
// reemplazo también los alcanza (la v1.7.0 quedó con `-like '*1.7.0*'`, que
// siempre es verdad, y el instalador se negaba a correr).
const (
	marcaVersion    = "__VERSION__"
	marcaHash       = "__EXPECTED_HASH__"
	marcaHashDesins = "__UNINSTALL_HASH__"
)

// bom es la marca de orden de bytes UTF-8, por si alguien guarda el fuente
// con ella: no debe quedar en el publicado.
var bom = string([]byte{0xEF, 0xBB, 0xBF})

var reVersion =regexp.MustCompile(`^\d+\.\d+\.\d+$`)
var reHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

func main() {
	version := flag.String("version", "", "versión de la release, p. ej. 1.7.1")
	hash := flag.String("hash", "", "SHA-256 de porteria-agent.exe")
	src := flag.String("src", "scripts", "carpeta con los guiones fuente")
	out := flag.String("out", "bin", "carpeta de salida")
	flag.Parse()

	if err := publicar(*src, *out, *version, *hash); err != nil {
		fmt.Fprintln(os.Stderr, "publicar-guiones:", err)
		os.Exit(1)
	}
}

func publicar(src, out, version, hash string) error {
	if !reVersion.MatchString(version) {
		return fmt.Errorf("versión inválida %q (se espera X.Y.Z)", version)
	}
	if !reHash.MatchString(hash) {
		return fmt.Errorf("hash inválido %q (se esperan 64 hex en minúscula)", hash)
	}

	desinstalar, err := preparar(filepath.Join(src, "uninstall.ps1"), map[string]string{
		marcaVersion: version,
	})
	if err != nil {
		return err
	}
	sumaDesins := sha256.Sum256([]byte(desinstalar))

	instalar, err := preparar(filepath.Join(src, "install.ps1"), map[string]string{
		marcaVersion:    version,
		marcaHash:       hash,
		marcaHashDesins: hex.EncodeToString(sumaDesins[:]),
	})
	if err != nil {
		return err
	}

	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "uninstall.ps1"), []byte(desinstalar), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "install.ps1"), []byte(instalar), 0o644)
}

// preparar lee un guion, reemplaza sus marcadores y lo deja en ASCII.
func preparar(ruta string, valores map[string]string) (string, error) {
	crudo, err := os.ReadFile(ruta)
	if err != nil {
		return "", err
	}
	texto := strings.TrimPrefix(string(crudo), bom)
	for marca, valor := range valores {
		texto = strings.ReplaceAll(texto, marca, valor)
	}
	if resto := regexp.MustCompile(`__[A-Z_]+__`).FindString(texto); resto != "" {
		return "", fmt.Errorf("%s: queda el marcador %s sin reemplazar", ruta, resto)
	}
	ascii, err := aASCII(texto)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ruta, err)
	}
	return ascii, nil
}

// Dónde está el cursor al recorrer el guion.
type zona int

const (
	codigo zona = iota
	comentarioDeLinea
	comentarioDeBloque // <# … #>
	comillasDobles     // "…": expande $(), así que acepta $([char]0x…)
	comillasSimples    // '…': literal
	hereDobles         // @" … "@
	hereSimples        // @' … '@
)

// aASCII reescribe cada carácter no ASCII según dónde aparece. Ver el
// comentario del paquete.
func aASCII(texto string) (string, error) {
	r := []rune(strings.ReplaceAll(texto, "\r\n", "\n"))
	var b strings.Builder
	z := codigo
	linea := 1
	inicioDeLinea := func(i int) bool { return i == 0 || r[i-1] == '\n' }

	for i := 0; i < len(r); i++ {
		c := r[i]
		sig := rune(0)
		if i+1 < len(r) {
			sig = r[i+1]
		}

		if c > 0x7F {
			switch z {
			case comillasDobles, hereDobles:
				fmt.Fprintf(&b, "$([char]0x%04X)", c)
			case comentarioDeLinea, comentarioDeBloque:
				b.WriteString(transliterar(c))
			default:
				return "", fmt.Errorf("línea %d: %q fuera de un texto entre comillas dobles o de un comentario; no hay forma segura de publicarlo en ASCII", linea, c)
			}
			continue
		}

		b.WriteRune(c)
		if c == '\n' {
			linea++
		}

		switch z {
		case codigo:
			switch {
			case c == '<' && sig == '#':
				z = comentarioDeBloque
				b.WriteRune(sig)
				i++
			case c == '#':
				z = comentarioDeLinea
			case c == '`':
				if sig != 0 {
					b.WriteRune(sig)
					i++
				}
			case c == '@' && sig == '"':
				z = hereDobles
				b.WriteRune(sig)
				i++
			case c == '@' && sig == '\'':
				z = hereSimples
				b.WriteRune(sig)
				i++
			case c == '"':
				z = comillasDobles
			case c == '\'':
				z = comillasSimples
			}
		case comentarioDeLinea:
			if c == '\n' {
				z = codigo
			}
		case comentarioDeBloque:
			if c == '#' && sig == '>' {
				b.WriteRune(sig)
				i++
				z = codigo
			}
		case comillasDobles:
			switch c {
			case '`':
				if sig != 0 && sig <= 0x7F {
					b.WriteRune(sig)
					i++
				}
			case '"':
				z = codigo // `""` vuelve a abrir en la siguiente vuelta
			}
		case comillasSimples:
			if c == '\'' {
				z = codigo
			}
		case hereDobles:
			if c == '"' && sig == '@' && inicioDeLinea(i) {
				b.WriteRune(sig)
				i++
				z = codigo
			}
		case hereSimples:
			if c == '\'' && sig == '@' && inicioDeLinea(i) {
				b.WriteRune(sig)
				i++
				z = codigo
			}
		}
	}
	if z != codigo && z != comentarioDeLinea {
		return "", fmt.Errorf("el guion termina dentro de un texto o comentario sin cerrar")
	}
	// Windows PowerShell lee igual LF que CRLF; CRLF es lo que espera quien
	// lo abra en el Bloc de notas.
	return strings.ReplaceAll(b.String(), "\n", "\r\n"), nil
}

var equivalentes = map[rune]string{
	'á': "a", 'é': "e", 'í': "i", 'ó': "o", 'ú': "u", 'ü': "u", 'ñ': "n",
	'Á': "A", 'É': "E", 'Í': "I", 'Ó': "O", 'Ú': "U", 'Ü': "U", 'Ñ': "N",
	'¿': "", '¡': "", '«': `"`, '»': `"`, '“': `"`, '”': `"`, '‘': "'", '’': "'",
	'—': "-", '–': "-", '─': "-", '═': "=", '…': "...", '→': "->", '←': "<-",
	'·': "-", '•': "-", '×': "x", '≥': ">=", '≤': "<=", '✓': "OK", '✗': "X",
	'⚠': "!", '▸': ">", '°': "o",
}

// transliterar da el equivalente ASCII de un carácter de comentario.
func transliterar(c rune) string {
	if s, ok := equivalentes[c]; ok {
		return s
	}
	return "?"
}
