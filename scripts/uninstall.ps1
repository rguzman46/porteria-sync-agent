<#
.SYNOPSIS
    Desinstala Porteria Sync Agent del PC del portero.

.DESCRIPTION
    Detiene el servicio, lo desregistra del SCM, quita las exclusiones de
    Windows Defender y opcionalmente borra el directorio de instalación
    (incluyendo logs y config con secretos).

    Diseñado para ser invocado one-shot desde el panel admin:

        iex (irm 'https://catamaran.porteriaplus.com/integrations/uninstall-script.ps1')

.NOTES
    Versión: __VERSION__
#>

[CmdletBinding()]
param(
    [string]$InstallDir = 'C:\PorteriaAgent',
    [switch]$KeepLogs,
    [switch]$KeepConfig
)

$ErrorActionPreference = 'Stop'

# ─────────────────────────────────────────────────────────────────────────
# 0. Validar elevación
# ─────────────────────────────────────────────────────────────────────────

$principal = New-Object System.Security.Principal.WindowsPrincipal(
    [System.Security.Principal.WindowsIdentity]::GetCurrent()
)
if (-not $principal.IsInRole([System.Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Host ""
    Write-Host "✗ Este script requiere PowerShell ejecutado como Administrador." -ForegroundColor Red
    exit 1
}

Write-Host ""
Write-Host "══════════════════════════════════════════════════════════════" -ForegroundColor Cyan
Write-Host "  Porteria Sync Agent — Desinstalación __VERSION__" -ForegroundColor Cyan
Write-Host "══════════════════════════════════════════════════════════════" -ForegroundColor Cyan
Write-Host ""

$exePath = Join-Path $InstallDir 'porteria-agent.exe'

# ─────────────────────────────────────────────────────────────────────────
# 1. Detener servicio (si está corriendo)
# ─────────────────────────────────────────────────────────────────────────

$svc = Get-Service -Name 'PorteriaSyncAgent' -ErrorAction SilentlyContinue
if ($svc) {
    if ($svc.Status -eq 'Running') {
        Write-Host "▸ Deteniendo servicio..."
        Stop-Service -Name 'PorteriaSyncAgent' -Force -ErrorAction SilentlyContinue
        Start-Sleep -Seconds 2
    }

    Write-Host "▸ Desregistrando servicio..."
    if (Test-Path $exePath) {
        & $exePath -uninstall 2>&1 | Out-Null
    } else {
        # El binario ya no está pero el servicio sigue registrado.
        # Lo borramos con sc.exe como fallback.
        sc.exe delete 'PorteriaSyncAgent' | Out-Null
    }
    Write-Host "  ✓ Servicio eliminado"
} else {
    Write-Host "▸ El servicio 'PorteriaSyncAgent' no estaba instalado — saltando"
}

# ─────────────────────────────────────────────────────────────────────────
# 2. Quitar exclusiones de Windows Defender
# ─────────────────────────────────────────────────────────────────────────

Write-Host "▸ Quitando exclusiones de Windows Defender..."
try {
    Remove-MpPreference -ExclusionPath $InstallDir -ErrorAction SilentlyContinue
    Remove-MpPreference -ExclusionProcess "$InstallDir\porteria-agent.exe" -ErrorAction SilentlyContinue
    Write-Host "  ✓ Exclusiones removidas"
} catch {
    Write-Host "  ⚠ No se pudieron quitar exclusiones: $($_.Exception.Message)" -ForegroundColor Yellow
}

# ─────────────────────────────────────────────────────────────────────────
# 3. Borrar archivos
# ─────────────────────────────────────────────────────────────────────────

if (Test-Path $InstallDir) {
    # Por defecto borramos TODO. Flags -KeepLogs / -KeepConfig preservan
    # selectivamente para que el admin pueda auditar después.
    if ($KeepLogs -or $KeepConfig) {
        Write-Host "▸ Borrando archivos (preservando flags solicitados)..."
        Get-ChildItem -Path $InstallDir -File | ForEach-Object {
            $skip = $false
            if ($KeepLogs -and $_.Extension -eq '.log') { $skip = $true }
            if ($KeepConfig -and $_.Name -eq 'config.yaml') { $skip = $true }
            if (-not $skip) {
                Remove-Item $_.FullName -Force
            }
        }
        # Si no queda nada (no flags), borra el dir entero.
        if (-not (Get-ChildItem $InstallDir -ErrorAction SilentlyContinue)) {
            Remove-Item $InstallDir -Recurse -Force
        }
    } else {
        Write-Host "▸ Borrando directorio $InstallDir ..."
        Remove-Item $InstallDir -Recurse -Force
    }
    Write-Host "  ✓ Archivos eliminados"
} else {
    Write-Host "▸ Directorio $InstallDir no existe — nada que borrar"
}

# ─────────────────────────────────────────────────────────────────────────
# 4. Resumen
# ─────────────────────────────────────────────────────────────────────────

Write-Host ""
Write-Host "══════════════════════════════════════════════════════════════" -ForegroundColor Green
Write-Host "  ✓ DESINSTALACIÓN COMPLETA" -ForegroundColor Green
Write-Host "══════════════════════════════════════════════════════════════" -ForegroundColor Green
Write-Host ""

if ($KeepConfig -or $KeepLogs) {
    Write-Host "  Archivos preservados en: $InstallDir"
    if ($KeepConfig) {
        Write-Host "    ⚠ config.yaml contiene token sensible — bórralo si ya no lo necesitas." -ForegroundColor Yellow
    }
}

Write-Host "  Recuerda revocar el device desde el panel admin si ya no lo usas:"
Write-Host "    https://<tu-conjunto>.porteriaplus.com/integrations"
Write-Host ""
