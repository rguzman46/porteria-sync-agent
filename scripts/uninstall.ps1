<#
.SYNOPSIS
    Desinstala Porteria Sync Agent del PC del portero.

.DESCRIPTION
    Detiene el servicio, lo desregistra del SCM, quita las exclusiones de
    Windows Defender y opcionalmente borra el directorio de instalación
    (incluyendo logs y config con secretos).

    El instalador lo deja en C:\PorteriaAgent. En PowerShell como
    administrador:

        powershell -ExecutionPolicy Bypass -File C:\PorteriaAgent\uninstall.ps1

    Quita todos los equipos del PC (C:\PorteriaAgent y sus equipo-N).

    Para quitar uno solo (por ejemplo, el que se borró en la plataforma),
    con el nombre que muestra la página del agente:

        powershell -ExecutionPolicy Bypass -File C:\PorteriaAgent\uninstall.ps1 -Servicio PorteriaSyncAgent-2

.NOTES
    Versión: __VERSION__
#>

[CmdletBinding()]
param(
    [string]$InstallDir = 'C:\PorteriaAgent',
    # Quita solo esta copia (PorteriaSyncAgent o PorteriaSyncAgent-N); sin él, todas.
    [string]$Servicio = '',
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
    return
}

Write-Host ""
Write-Host "══════════════════════════════════════════════════════════════" -ForegroundColor Cyan
Write-Host "  Porteria Sync Agent — Desinstalación __VERSION__" -ForegroundColor Cyan
Write-Host "══════════════════════════════════════════════════════════════" -ForegroundColor Cyan
Write-Host ""


# ─────────────────────────────────────────────────────────────────────────
# 1. Qué copias se quitan
# ─────────────────────────────────────────────────────────────────────────
# Cada equipo de la portería es una copia del agente: el primero es el
# servicio PorteriaSyncAgent en $InstallDir; los siguientes,
# PorteriaSyncAgent-N en $InstallDir\equipo-N (así los crea install.ps1).
# Sin -Servicio se quitan todas; con -Servicio, solo esa, y las demás siguen
# funcionando.

$formaDelNombre = '^PorteriaSyncAgent(-\d{1,3})?$'

function Get-CarpetaDeCopia([string]$nombre) {
    if ($nombre -match '^PorteriaSyncAgent-(\d{1,3})$') {
        return (Join-Path $InstallDir "equipo-$($matches[1])")
    }
    return $InstallDir
}

function Get-ReglaDeCopia([string]$nombre) {
    # El mismo nombre que le pone install.ps1 (y `reglaDeFirewall` del agente).
    if ($nombre -eq 'PorteriaSyncAgent') { return 'Porteria Sync Agent - receptor LPR' }
    return "Porteria Sync Agent - receptor LPR ($nombre)"
}

function Remove-CopiaDelAgente([string]$nombre) {
    $svc = Get-Service -Name $nombre -ErrorAction SilentlyContinue
    if ($svc) {
        if ($svc.Status -ne 'Stopped') {
            Write-Host "▸ Deteniendo $nombre..."
            Stop-Service -Name $nombre -Force -ErrorAction SilentlyContinue
            Start-Sleep -Seconds 2
        }
        Write-Host "▸ Desregistrando $nombre..."
        sc.exe delete $nombre | Out-Null
        Write-Host "  ✓ Servicio eliminado"
    } else {
        Write-Host "▸ $nombre no estaba registrado como servicio"
    }
    Get-NetFirewallRule -DisplayName (Get-ReglaDeCopia $nombre) -ErrorAction SilentlyContinue |
        Remove-NetFirewallRule -ErrorAction SilentlyContinue
    $carpeta = Get-CarpetaDeCopia $nombre
    try {
        Remove-MpPreference -ExclusionPath $carpeta -ErrorAction SilentlyContinue
        Remove-MpPreference -ExclusionProcess (Join-Path $carpeta 'porteria-agent.exe') -ErrorAction SilentlyContinue
    } catch {
        Write-Host "  ⚠ No se pudieron quitar las exclusiones de Defender: $($_.Exception.Message)" -ForegroundColor Yellow
    }
}

function Get-CopiasInstaladas {
    # Por servicio y por carpeta: una copia a medio quitar puede tener solo
    # una de las dos.
    $nombres = @(Get-Service -Name 'PorteriaSyncAgent*' -ErrorAction SilentlyContinue |
        Where-Object { $_.Name -match $formaDelNombre } | ForEach-Object { $_.Name })
    if (Test-Path (Join-Path $InstallDir 'porteria-agent.exe')) { $nombres += 'PorteriaSyncAgent' }
    Get-ChildItem $InstallDir -Directory -Filter 'equipo-*' -ErrorAction SilentlyContinue | ForEach-Object {
        if ($_.Name -match '^equipo-(\d{1,3})$') { $nombres += "PorteriaSyncAgent-$($matches[1])" }
    }
    return @($nombres | Sort-Object -Unique)
}

function Remove-ArchivosDeCopia([string]$carpeta, [bool]$soloArchivos) {
    # $soloArchivos: la carpeta raíz cuando quedan otras copias en sus
    # equipo-N. Se borran sus archivos y se deja el desinstalador, que es
    # el que usan las demás.
    if (-not (Test-Path $carpeta)) {
        Write-Host "▸ $carpeta no existe — nada que borrar"
        return
    }
    if ($KeepLogs -or $KeepConfig -or $soloArchivos) {
        Write-Host "▸ Borrando los archivos de $carpeta ..."
        Get-ChildItem -Path $carpeta -File | ForEach-Object {
            $dejar = ($KeepLogs -and $_.Extension -eq '.log') -or
                     ($KeepConfig -and $_.Name -eq 'config.yaml') -or
                     ($soloArchivos -and $_.Name -eq 'uninstall.ps1')
            if (-not $dejar) { Remove-Item $_.FullName -Force }
        }
        if (-not (Get-ChildItem $carpeta -Force -ErrorAction SilentlyContinue)) {
            Remove-Item $carpeta -Recurse -Force
        }
    } else {
        Write-Host "▸ Borrando $carpeta ..."
        Remove-Item $carpeta -Recurse -Force
    }
    Write-Host "  ✓ Archivos eliminados"
}

$quedan = @()
if ($Servicio) {
    if ($Servicio -notmatch $formaDelNombre) {
        Write-Host "✗ -Servicio debe ser PorteriaSyncAgent o PorteriaSyncAgent-N (N = 2, 3…)." -ForegroundColor Red
        Write-Host "  El nombre exacto lo dice la página del agente (http://localhost:8787)."
        return
    }
    $instaladas = Get-CopiasInstaladas
    if ($instaladas -notcontains $Servicio) {
        Write-Host "✗ En este computador no hay ninguna copia del agente llamada $Servicio." -ForegroundColor Red
        Write-Host "  Instaladas: $($instaladas -join ', ')"
        return
    }
    Remove-CopiaDelAgente $Servicio
    $carpeta = Get-CarpetaDeCopia $Servicio
    $quedan = @($instaladas | Where-Object { $_ -ne $Servicio })
    # La raíz guarda las carpetas equipo-N: con otras copias vivas solo se
    # borran sus archivos.
    Remove-ArchivosDeCopia $carpeta (($carpeta -eq $InstallDir) -and ($quedan.Count -gt 0))
} else {
    $copias = Get-CopiasInstaladas
    if ($copias.Count -eq 0) {
        Write-Host "▸ No había ninguna copia del agente instalada — saltando"
    }
    foreach ($nombre in $copias) { Remove-CopiaDelAgente $nombre }
    # Por si quedó alguna regla de una copia que ya no existe.
    Get-NetFirewallRule -DisplayName 'Porteria Sync Agent - receptor LPR*' -ErrorAction SilentlyContinue |
        Remove-NetFirewallRule -ErrorAction SilentlyContinue
    Remove-ArchivosDeCopia $InstallDir $false
}

# ─────────────────────────────────────────────────────────────────────────
# 2. Resumen
# ─────────────────────────────────────────────────────────────────────────

Write-Host ""
Write-Host "══════════════════════════════════════════════════════════════" -ForegroundColor Green
if ($Servicio) {
    Write-Host "  ✓ SE QUITÓ $Servicio" -ForegroundColor Green
} else {
    Write-Host "  ✓ DESINSTALACIÓN COMPLETA" -ForegroundColor Green
}
Write-Host "══════════════════════════════════════════════════════════════" -ForegroundColor Green
Write-Host ""

if ($quedan.Count -gt 0) {
    Write-Host "  Siguen instalados en este computador: $($quedan -join ', ')"
    Write-Host ""
}

if ($KeepConfig -or $KeepLogs) {
    Write-Host "  Archivos preservados en: $InstallDir"
    if ($KeepConfig) {
        Write-Host "    ⚠ config.yaml contiene token sensible — bórralo si ya no lo necesitas." -ForegroundColor Yellow
    }
}

Write-Host "  Si ya no usas el equipo, bórralo también en la plataforma:"
Write-Host "    Cámaras de placas, en https://<tu-conjunto>.cloud.porteriaplus.com/lpr"
Write-Host "  Borrarlo revoca su llave."
Write-Host ""
