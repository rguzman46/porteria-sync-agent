package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// El servicio corre como SYSTEM. Hasta la v1.7.8 la carpeta heredaba
// «Usuarios autenticados: Modificar» de C:\ y cualquier usuario del PC podía
// cambiar el .exe. El instalador la restringe antes de bajar nada y, si no
// puede, no instala. La prueba de verdad de los permisos se hace en Windows
// (ver README, «Permisos de la carpeta»); esta cuida que nadie la quite ni la
// mueva después de la descarga.
func TestElInstaladorRestringeLaCarpetaAntesDeBajarNada(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("scripts", "install.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	ins := string(data)

	llamada := strings.Index(ins, "Set-PermisosDeLaCarpeta $CarpetaRaiz")
	descarga := strings.Index(ins, "Invoke-WebRequest -Uri $BinaryUrl")
	desinstalador := strings.Index(ins, "Invoke-WebRequest -Uri $UninstallUrl")
	if llamada < 0 || descarga < 0 || desinstalador < 0 {
		t.Fatalf("faltan piezas: permisos=%d binario=%d desinstalador=%d", llamada, descarga, desinstalador)
	}
	if llamada > descarga || llamada > desinstalador {
		t.Fatal("la carpeta se restringe antes de bajar el binario y el desinstalador")
	}

	// Lo que la función tiene que hacer, dicho en su código.
	for _, debe := range []string{
		"SetAccessRuleProtection($true, $false)", // herencia cortada
		"'S-1-5-18', 'S-1-5-32-544'",             // solo SYSTEM y Administradores
		"SetOwner(",                              // dueño Administradores
		"/reset /T",                              // lo de adentro, reiniciado
		`-notmatch '^[A-Za-z]:\\'`,               // nunca una ruta incompleta
		"GetPathRoot(",                           // nunca la raíz de un disco
	} {
		if !strings.Contains(ins, debe) {
			t.Errorf("install.ps1 debía tener %s", debe)
		}
	}

	// Si no puede restringirla, se detiene: un agente en una carpeta abierta
	// es peor que ningún agente.
	bloque := ins[llamada:]
	if fin := strings.Index(bloque, "# 3."); fin > 0 {
		bloque = bloque[:fin]
	}
	if !strings.Contains(bloque, "catch") || !strings.Contains(bloque, "return") {
		t.Fatal("si los permisos fallan, el instalador debía detenerse")
	}
}
