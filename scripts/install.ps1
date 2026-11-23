<#
.SYNOPSIS
    Instala Porteria Sync Agent en este PC del portero.

.DESCRIPTION
    Descarga el binario, verifica integridad SHA-256, agrega exclusiones de
    Windows Defender, genera config.yaml con los parámetros provistos y
    registra el agent como Windows Service que arranca automáticamente.

    Diseñado para ser invocado one-shot desde el panel admin del conjunto:

        Set-ExecutionPolicy -Scope Process Bypass -Force
        $Token = 'ppk_...'        # la llave del conjunto
        $DeviceToken = 'ppd_...'  # el token de ESTA cámara
        $CloudUrl = 'https://miconjunto.cloud.porteriaplus.com'
        $guion = irm "$CloudUrl/integrations/install-script.ps1"
        & ([scriptblock]::Create($guion)) -Token $Token -DeviceToken $DeviceToken -CloudUrl $CloudUrl

    O bajado de la release en GitHub, en PowerShell como administrador:

        .\install.ps1 -CloudUrl https://miconjunto.cloud.porteriaplus.com

    El panel del conjunto arma ese bloque completo al registrar la cámara:
    se copia y se pega, no se escribe nada.

    Si Token / DeviceToken / CloudUrl no se pasan vía scope o parámetro, los
    pide interactivamente. Pide siempre las credenciales de la cámara
    interactivamente (no se pasan por URL para evitar fugas en historiales
    de shell).

    El binario se descarga de la MISMA release que este guion (nunca de
    `latest`): la portería queda con la versión que se probó contra esta
    plataforma, no con la que alguien publique mañana.

.NOTES
    Versión: __VERSION__
    SHA-256 esperado del binario Windows: __EXPECTED_HASH__

    El guion fuente lleva marcadores que `cmd/publicar-guiones` reemplaza al
    armar la release; el mismo paso lo deja en ASCII puro para que Windows
    PowerShell 5.1 lo lea bien con cualquier codificación.
#>

[CmdletBinding()]
param(
    # La llave del conjunto (`ppk_…`): dice de qué conjunto son las lecturas.
    [string]$Token,
    # El token de ESTA cámara (`ppd_…`): dice de cuál.
    #
    # No es opcional en la práctica. Sin él, el cloud atribuye todo al primer
    # dispositivo activo del conjunto, y en una portería con cámara de entrada
    # y de salida eso significa que **las salidas se registran como entradas**:
    # la visita no se cierra y el conteo de quién está adentro solo crece.
    [string]$DeviceToken,
    [string]$CloudUrl,
    [string]$CameraType = 'hikvision',
    # V1.3 plug-and-play: familia específica del vendor cuando hay líneas con
    # endpoints distintos. Si vacío, el agent deriva del CameraType:
    #   hikvision → hikvision_traffic (línea profesional iDS-*)
    #   dahua     → dahua_itc
    #   axis      → axis_vapix
    # El panel `/integrations` del cloud lo pre-puebla automáticamente cuando
    # el admin elige el modelo de cámara desde el dropdown.
    [string]$VendorFamily = '',
    [string]$CameraHost,
    [int]$CameraPort = 80,
    [string]$CameraUser = 'admin',
    [string]$CameraPassword,
    [string]$InstallDir = 'C:\PorteriaAgent',
    # Puerto en el que el agent recibe las lecturas de la cámara. Se abre en
    # el firewall de Windows solo para la IP de la cámara.
    [int]$ReceiverPort = 8787,
    # No tocar la energía del PC (ver «El PC no se suspende»). Solo para un PC
    # que no es de la portería y donde alguien ya decidió cómo se suspende.
    [switch]$SinCambiarEnergia,
    # De la misma release que este guion. `__VERSION__` lo sustituye CI.
    [string]$BinaryUrl = 'https://github.com/rguzman46/porteria-sync-agent/releases/download/agent-v__VERSION__/porteria-agent.exe',
    [string]$ExpectedHash = '__EXPECTED_HASH__',
    # El desinstalador de la misma release: queda en C:\PorteriaAgent para que
    # quitar el agente no dependa de volver a encontrarlo.
    [string]$UninstallUrl = 'https://github.com/rguzman46/porteria-sync-agent/releases/download/agent-v__VERSION__/uninstall.ps1',
    [string]$UninstallHash = '__UNINSTALL_HASH__'
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'  # acelera Invoke-WebRequest 10x

# TLS 1.2 encendido antes de bajar nada. Un Windows 10 sin actualizar (con
# .NET 4.5/4.6) negocia por defecto SSL3/TLS 1.0, y GitHub y la plataforma
# exigen TLS 1.2: la descarga falla con «No se puede crear un canal seguro
# SSL/TLS». En un PC de portería es lo más probable. `-bor` suma, no quita lo
# que el sistema ya tenga (TLS 1.3 donde exista).
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

# Para detenerse se usa `return`, nunca `exit`. El bloque del panel corre este
# guion con `& ([scriptblock]::Create(...))` en la ventana de quien instala, y
# ahí `exit` cierra la ventana entera: el técnico ve que se cierra sola y no
# alcanza a leer por qué (2026-11-20). `return` termina el guion y deja el
# mensaje en pantalla, igual con `.\install.ps1`.

# ─────────────────────────────────────────────────────────────────────────
# 0. Validar elevación (Admin)
# ─────────────────────────────────────────────────────────────────────────

$principal = New-Object System.Security.Principal.WindowsPrincipal(
    [System.Security.Principal.WindowsIdentity]::GetCurrent()
)
if (-not $principal.IsInRole([System.Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Host ""
    Write-Host "✗ Este script requiere PowerShell ejecutado como Administrador." -ForegroundColor Red
    Write-Host "  Click derecho en PowerShell → 'Ejecutar como administrador' y vuelve a correrlo."
    Write-Host ""
    return
}

# ─────────────────────────────────────────────────────────────────────────
# 1. Recoger parámetros faltantes interactivamente
# ─────────────────────────────────────────────────────────────────────────

# El Token y CloudUrl pueden venir del scope llamante (iex con variables
# pre-definidas) o del parámetro. Si ninguno, los pedimos.
if (-not $Token -and $PSCmdlet.SessionState.PSVariable.Get('Token')) {
    $Token = $PSCmdlet.SessionState.PSVariable.Get('Token').Value
}
if (-not $DeviceToken -and $PSCmdlet.SessionState.PSVariable.Get('DeviceToken')) {
    $DeviceToken = $PSCmdlet.SessionState.PSVariable.Get('DeviceToken').Value
}
if (-not $CloudUrl -and $PSCmdlet.SessionState.PSVariable.Get('CloudUrl')) {
    $CloudUrl = $PSCmdlet.SessionState.PSVariable.Get('CloudUrl').Value
}
# CameraType y VendorFamily se pueden pasar como $CameraType / $VendorFamily en
# el scope llamante para que el bloque del panel los pre-pueble sin que el
# admin escriba nada. CameraType tiene default, así que se mira si el
# parámetro vino de verdad antes de tomar el del scope.
if (-not $PSBoundParameters.ContainsKey('CameraType') -and $PSCmdlet.SessionState.PSVariable.Get('CameraType')) {
    $delScope = $PSCmdlet.SessionState.PSVariable.Get('CameraType').Value
    if ($delScope) { $CameraType = $delScope }
}
if (-not $VendorFamily -and $PSCmdlet.SessionState.PSVariable.Get('VendorFamily')) {
    $VendorFamily = $PSCmdlet.SessionState.PSVariable.Get('VendorFamily').Value
}
$CameraType = $CameraType.ToLower().Trim()

# Un guion que no salió de una release no sabe de qué release bajar el
# binario. Se corta acá, no se cae a `latest`.
#
# No se compara contra el marcador escrito tal cual: la release lo reemplaza
# en todo el archivo, también aquí, y la v1.7.0 quedó comparando contra su
# propia versión y negándose a correr siempre. `agent-v` seguido de `_` solo
# lo tiene el fuente.
if ($BinaryUrl -like '*/agent-v_*') {
    Write-Host ""
    Write-Host "✗ Este install.ps1 no viene de una release (falta la versión)." -ForegroundColor Red
    Write-Host "  Usa el bloque que arma el panel del conjunto, o pasa -BinaryUrl con la release exacta."
    Write-Host ""
    return
}

while (-not $Token) {
    $Token = Read-Host "Llave del conjunto (empieza con 'ppk_')"
    if (-not $Token.StartsWith('ppk_')) {
        Write-Host "  ⚠ La llave del conjunto empieza con 'ppk_'" -ForegroundColor Yellow
        $Token = $null
    }
}

# Se pide, no se asume: una instalación sin él arranca con el sentido de las
# lecturas invertido y nadie se entera hasta que alguien pregunta por qué el
# conjunto dice tener treinta visitantes adentro.
while (-not $DeviceToken) {
    $DeviceToken = Read-Host "Token de esta camara (empieza con 'ppd_')"
    if (-not $DeviceToken.StartsWith('ppd_')) {
        Write-Host "  ⚠ El token de la camara empieza con 'ppd_'" -ForegroundColor Yellow
        $DeviceToken = $null
    }
}

while (-not $CloudUrl) {
    $CloudUrl = Read-Host "URL del conjunto (ej: https://miconjunto.cloud.porteriaplus.com)"
    if (-not $CloudUrl.StartsWith('https://')) {
        Write-Host "  ⚠ La URL debe empezar con 'https://'" -ForegroundColor Yellow
        $CloudUrl = $null
    }
}

# Las credenciales de la cámara SIEMPRE se piden interactivamente — no
# vienen del panel para evitar fugas en historiales de shell.
if (-not $CameraHost) {
    $CameraHost = Read-Host "IP local de la cámara LPR (ej: 192.168.1.50)"
}

if (-not $CameraPassword) {
    $secPwd = Read-Host "Contraseña admin de la cámara" -AsSecureString
    $CameraPassword = [System.Net.NetworkCredential]::new('', $secPwd).Password
}

# ─────────────────────────────────────────────────────────────────────────
# 1.b Qué copia del agente es esta
# ─────────────────────────────────────────────────────────────────────────
#
# Cada equipo de la portería (la cámara de la reja, el terminal de la puerta)
# es una copia del agente con su carpeta, su servicio y su puerto, y se
# reconoce por su token (`ppd_…`). El primero va en C:\PorteriaAgent como
# siempre; los siguientes en C:\PorteriaAgent\equipo-N, con el servicio
# PorteriaSyncAgent-N y el puerto 8786+N. Volver a pegar el bloque de un
# equipo ya instalado lo actualiza en su misma carpeta.

function Get-TokenDelConfig([string]$ruta) {
    if (-not (Test-Path $ruta)) { return $null }
    $m = Select-String -Path $ruta -Pattern '^\s*device_token:\s*"?([^"\s]+)"?' | Select-Object -First 1
    if ($m) { return $m.Matches[0].Groups[1].Value }
    return $null
}

$ServiceName = 'PorteriaSyncAgent'
# La carpeta que se protege (paso 2): la raíz, que cubre también equipo-N.
$CarpetaRaiz = $InstallDir
if (-not $PSBoundParameters.ContainsKey('InstallDir')) {
    $tokenRaiz = Get-TokenDelConfig (Join-Path $InstallDir 'config.yaml')
    if ($tokenRaiz -and $tokenRaiz -ne $DeviceToken) {
        $n = $null
        foreach ($carpeta in (Get-ChildItem $InstallDir -Directory -Filter 'equipo-*' -ErrorAction SilentlyContinue)) {
            if ((Get-TokenDelConfig (Join-Path $carpeta.FullName 'config.yaml')) -eq $DeviceToken) {
                $n = [int]($carpeta.Name -replace '^equipo-', '')
                break
            }
        }
        if (-not $n) {
            $n = 2
            while (Test-Path (Join-Path $InstallDir "equipo-$n")) { $n++ }
        }
        $InstallDir = Join-Path $InstallDir "equipo-$n"
        $ServiceName = "PorteriaSyncAgent-$n"
        if (-not $PSBoundParameters.ContainsKey('ReceiverPort')) { $ReceiverPort = 8786 + $n }
        Write-Host "▸ Este PC ya tiene otro equipo instalado: este va como equipo $n (puerto $ReceiverPort)." -ForegroundColor Cyan
    }
}

Write-Host ""
Write-Host "══════════════════════════════════════════════════════════════" -ForegroundColor Cyan
Write-Host "  Porteria Sync Agent — Instalación __VERSION__" -ForegroundColor Cyan
Write-Host "══════════════════════════════════════════════════════════════" -ForegroundColor Cyan
Write-Host ""

# ─────────────────────────────────────────────────────────────────────────
# 2. Crear directorio de instalación
# ─────────────────────────────────────────────────────────────────────────

Write-Host "▸ Preparando $InstallDir ..."
if (-not (Test-Path $InstallDir)) {
    New-Item -ItemType Directory -Path $InstallDir | Out-Null
}

# Solo SYSTEM y Administradores. Una carpeta nueva en C:\ hereda «Usuarios
# autenticados: Modificar», y el servicio corre como SYSTEM: cualquier
# usuario del PC (la cuenta del portero) podía cambiar porteria-agent.exe y
# quedarse con el equipo, alterar uninstall.ps1 antes de que lo corra un
# administrador, o leer agent.log, que lleva placas y nombres. Se aplica en
# cada instalación, así que volver a pegar el bloque corrige un PC viejo.
# Dueño, herencia cortada y cada archivo de adentro reiniciado: una carpeta
# creada antes por otro usuario no conserva sus permisos.
function Set-PermisosDeLaCarpeta([string]$carpeta) {
    # Solo una ruta completa, con unidad y barra: «C:» o «agente» se leerían
    # contra la carpeta actual de la ventana.
    if ($carpeta -notmatch '^[A-Za-z]:\\') {
        throw "$carpeta no es una ruta completa (como C:\PorteriaAgent)"
    }
    $carpeta = [System.IO.Path]::GetFullPath($carpeta).TrimEnd('\')
    # Un -InstallDir mal puesto (C:\, C:\Windows…) dejaría el disco solo para
    # administradores. La carpeta del agente nunca es la raíz de un disco ni
    # una del sistema.
    $prohibidas = @($env:SystemRoot, $env:ProgramFiles, ${env:ProgramFiles(x86)},
        $env:ProgramData, (Split-Path $env:PUBLIC -Parent)) |
        Where-Object { $_ } | ForEach-Object { $_.TrimEnd('\') }
    if ([System.IO.Path]::GetPathRoot($carpeta).TrimEnd('\') -eq $carpeta -or $prohibidas -contains $carpeta) {
        throw "$carpeta no puede ser la carpeta del agente; usa una propia, como C:\PorteriaAgent"
    }
    $acl =New-Object System.Security.AccessControl.DirectorySecurity
    $acl.SetOwner((New-Object System.Security.Principal.SecurityIdentifier('S-1-5-32-544')))
    $acl.SetAccessRuleProtection($true, $false)
    foreach ($sid in 'S-1-5-18', 'S-1-5-32-544') {   # SYSTEM, Administradores
        $acl.AddAccessRule((New-Object System.Security.AccessControl.FileSystemAccessRule(
            (New-Object System.Security.Principal.SecurityIdentifier($sid)), 'FullControl',
            'ContainerInherit, ObjectInherit', 'None', 'Allow')))
    }
    Set-Acl -LiteralPath $carpeta -AclObject $acl
    if (Get-ChildItem -LiteralPath $carpeta -Force | Select-Object -First 1) {
        & icacls.exe "$carpeta\*" /setowner '*S-1-5-32-544' /T /C /Q | Out-Null
        & icacls.exe "$carpeta\*" /reset /T /C /Q | Out-Null
        if ($LASTEXITCODE -ne 0) { throw "icacls terminó con código $LASTEXITCODE" }
    }
}
try {
    Set-PermisosDeLaCarpeta $CarpetaRaiz
    # El desinstalador va siempre en C:\PorteriaAgent (más abajo), también
    # con un -InstallDir propio.
    if ((Test-Path 'C:\PorteriaAgent') -and
        [System.IO.Path]::GetFullPath($CarpetaRaiz).TrimEnd('\') -ne 'C:\PorteriaAgent') {
        Set-PermisosDeLaCarpeta 'C:\PorteriaAgent'
    }
    Write-Host "  ✓ Carpeta restringida a Administradores y SYSTEM"
} catch {
    Write-Host "  ✗ No se pudieron restringir los permisos de ${CarpetaRaiz}: $($_.Exception.Message)" -ForegroundColor Red
    Write-Host "    Sin eso, cualquier usuario del PC podría cambiar el agente. No se instala." -ForegroundColor Red
    return
}

# ─────────────────────────────────────────────────────────────────────────
# 3. Agregar exclusiones de Windows Defender ANTES de descargar
# ─────────────────────────────────────────────────────────────────────────
# Sin esto, Defender puede borrar el binario al detectarlo como "unsigned"
# durante el download. El binario es safe; firmamos con SHA-256 verificado.

Write-Host "▸ Agregando exclusiones en Windows Defender..."
try {
    Add-MpPreference -ExclusionPath $InstallDir -ErrorAction Stop
    Add-MpPreference -ExclusionProcess "$InstallDir\porteria-agent.exe" -ErrorAction Stop
    Write-Host "  ✓ Exclusiones agregadas: $InstallDir y porteria-agent.exe"
} catch {
    Write-Host "  ⚠ No se pudieron agregar exclusiones automáticamente: $($_.Exception.Message)" -ForegroundColor Yellow
    Write-Host "  Si Windows Defender bloquea el binario, agrégalas manualmente en"
    Write-Host "  Seguridad de Windows → Protección contra virus → Configuración → Exclusiones."
}

# ─────────────────────────────────────────────────────────────────────────
# 4. Descargar binario y verificar SHA-256
# ─────────────────────────────────────────────────────────────────────────

$exePath = Join-Path $InstallDir 'porteria-agent.exe'

# Si hay un servicio corriendo, detenerlo antes de sobrescribir el .exe.
if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) {
    Write-Host "▸ Deteniendo servicio existente para reemplazar binario..."
    Stop-Service -Name $ServiceName -Force -ErrorAction SilentlyContinue
    Start-Sleep -Seconds 2
}

Write-Host "▸ Descargando binario desde $BinaryUrl ..."
try {
    Invoke-WebRequest -Uri $BinaryUrl -OutFile $exePath -UseBasicParsing -ErrorAction Stop
    Write-Host "  ✓ Descargado ($(Get-Item $exePath | Select-Object -ExpandProperty Length) bytes)"
} catch {
    Write-Host "  ✗ Error descargando: $($_.Exception.Message)" -ForegroundColor Red
    Write-Host "    Verifica conectividad a internet o que la URL sea correcta."
    return
}

# Verificación de integridad SHA-256 — abortamos si el hash no coincide.
# Se mira la forma del hash y no si sigue siendo el marcador: la release
# también reemplaza el marcador de la comparación, y la v1.7.0 se saltaba la
# verificación siempre.
if ($ExpectedHash -match '^[0-9a-fA-F]{64}$') {
    Write-Host "▸ Verificando SHA-256 ..."
    $actualHash = (Get-FileHash $exePath -Algorithm SHA256).Hash.ToLower()
    $expectedLower = $ExpectedHash.ToLower()
    if ($actualHash -ne $expectedLower) {
        Write-Host "  ✗ Hash NO coincide!" -ForegroundColor Red
        Write-Host "    Esperado: $expectedLower"
        Write-Host "    Recibido: $actualHash"
        Write-Host "    Aborta la instalación. El binario puede estar corrupto o modificado."
        Remove-Item $exePath -Force
        return
    }
    Write-Host "  ✓ SHA-256 OK: $($actualHash.Substring(0,16))..."
} else {
    Write-Host "  ⚠ Hash esperado no proporcionado — saltando verificación (no recomendado para prod)" -ForegroundColor Yellow
}

# El desinstalador, de la misma release y con su hash. Si falla, la
# instalación sigue: el agente funciona igual y el desinstalador está en
# GitHub.
$uninstallPath = 'C:\PorteriaAgent\uninstall.ps1'
if ($UninstallHash -match '^[0-9a-fA-F]{64}$') {
    try {
        $tmp = "$uninstallPath.descarga"
        Invoke-WebRequest -Uri $UninstallUrl -OutFile $tmp -UseBasicParsing -ErrorAction Stop
        if ((Get-FileHash $tmp -Algorithm SHA256).Hash.ToLower() -eq $UninstallHash.ToLower()) {
            Move-Item $tmp $uninstallPath -Force
        } else {
            Remove-Item $tmp -Force
            $uninstallPath = $null
        }
    } catch {
        $uninstallPath = $null
    }
} else {
    $uninstallPath = $null
}

# ─────────────────────────────────────────────────────────────────────────
# 5. Generar config.yaml
# ─────────────────────────────────────────────────────────────────────────

$configPath = Join-Path $InstallDir 'config.yaml'

# Si ya existe un config, hacemos backup antes de sobreescribir.
if (Test-Path $configPath) {
    $backup = "$configPath.bak-$(Get-Date -Format 'yyyyMMddHHmmss')"
    Copy-Item $configPath $backup
    Write-Host "▸ Config previo respaldado en: $backup"
}

Write-Host "▸ Generando config.yaml ..."

# Si VendorFamily empieza con prefijo de marca distinta al CameraType
# (ej. type=dahua + family=hikvision_itc — inconsistente), warning + ignorar.
# El cloud no debería enviarlo así pero defensa adicional.
if ($VendorFamily) {
    if (-not $VendorFamily.StartsWith($CameraType)) {
        Write-Host "  ⚠ VendorFamily '$VendorFamily' no coincide con type '$CameraType' — usando type solo." -ForegroundColor Yellow
        $VendorFamily = ''
    }
}

$familyLine = if ($VendorFamily) { "  family: $VendorFamily" } else { '  family: ""' }

$configContent = @"
# Generado por install.ps1 el $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss')
# NO commitear este archivo — contiene secretos. chmod 600 equivalente.

cloud:
  base_url: $CloudUrl
  token: $Token
  device_token: $DeviceToken

camera:
  type: $CameraType
$familyLine
  host: $CameraHost
  port: $CameraPort
  user: $CameraUser
  password: $CameraPassword
  auto_config: true

poll:
  interval_seconds: 60

log:
  file: agent.log
  level: info
  max_size_mb: 10
  keep: 3

# Recepción de las lecturas (foto + placa) desde la cámara. La cámara se
# configura para postear a http://<IP de este PC>:$ReceiverPort/lpr-event
# (Hikvision), /NotificationInfo/TollgateInfo (Dahua) o /axis-event (Axis).
# Solo se aceptan POST desde camera.host; agrega IPs en allow_from si hace
# falta. allow_any: true apaga el filtro (solo para diagnosticar).
receiver:
  enabled: true
  bind_address: 0.0.0.0:$ReceiverPort
  allow_from: []
  allow_any: false
"@
Set-Content -Path $configPath -Value $configContent -Encoding UTF8

# Lock down config.yaml: solo Administradores y SYSTEM (que es como corre
# el servicio) pueden leerlo. Quita herencia + restringe ACL.
try {
    $acl = Get-Acl $configPath
    $acl.SetAccessRuleProtection($true, $false) # corta herencia
    $admins = New-Object System.Security.AccessControl.FileSystemAccessRule(
        'BUILTIN\Administrators', 'FullControl', 'Allow'
    )
    $system = New-Object System.Security.AccessControl.FileSystemAccessRule(
        'NT AUTHORITY\SYSTEM', 'FullControl', 'Allow'
    )
    $acl.AddAccessRule($admins)
    $acl.AddAccessRule($system)
    Set-Acl -Path $configPath -AclObject $acl
    Write-Host "  ✓ config.yaml escrito y restringido a Admin + SYSTEM"
} catch {
    Write-Host "  ⚠ No se pudo restringir ACL de config.yaml: $($_.Exception.Message)" -ForegroundColor Yellow
}

# ─────────────────────────────────────────────────────────────────────────
# 6. Abrir el puerto del receiver en el firewall de Windows
# ─────────────────────────────────────────────────────────────────────────
# Sin esto la cámara postea y Windows lo bota en silencio: el agent se ve
# vivo, la lista se sincroniza y nunca llega una foto. Idempotente: si la
# regla ya existe se actualiza (por si cambió la IP de la cámara o el puerto).
# Solo para la IP de la cámara — el agent además filtra por origen.

Write-Host "▸ Abriendo el puerto $ReceiverPort en el firewall para $CameraHost ..."
# El mismo nombre que calcula el agente (`reglaDeFirewall`): el panel local
# la actualiza cuando el técnico cambia la IP del equipo.
$ruleName = 'Porteria Sync Agent - receptor LPR'
if ($ServiceName -ne 'PorteriaSyncAgent') { $ruleName = "$ruleName ($ServiceName)" }
try {
    $regla = Get-NetFirewallRule -DisplayName $ruleName -ErrorAction SilentlyContinue
    if ($regla) {
        Set-NetFirewallRule -DisplayName $ruleName -LocalPort $ReceiverPort -RemoteAddress $CameraHost -Enabled True | Out-Null
        Write-Host "  ✓ Regla existente actualizada"
    } else {
        New-NetFirewallRule -DisplayName $ruleName -Direction Inbound -Protocol TCP `
            -LocalPort $ReceiverPort -RemoteAddress $CameraHost -Action Allow -Profile Any | Out-Null
        Write-Host "  ✓ Regla creada (TCP $ReceiverPort entrante desde $CameraHost)"
    }
} catch {
    Write-Host "  ⚠ No se pudo crear la regla de firewall: $($_.Exception.Message)" -ForegroundColor Yellow
    Write-Host "    Ábrela a mano: TCP $ReceiverPort entrante desde $CameraHost, o la cámara no podrá mandar fotos."
}

# ─────────────────────────────────────────────────────────────────────────
# 7. Instalar como Windows Service y arrancarlo
# ─────────────────────────────────────────────────────────────────────────

Write-Host "▸ Registrando como Windows Service..."

# Si ya está instalado, desinstalar primero para refrescar la config.
if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) {
    & $exePath -config $configPath -service-name $ServiceName -uninstall 2>&1 | Out-Null
    Start-Sleep -Seconds 2
}

& $exePath -config $configPath -service-name $ServiceName -install
if ($LASTEXITCODE -ne 0) {
    Write-Host "  ✗ Error registrando el servicio." -ForegroundColor Red
    return
}
Write-Host "  ✓ Servicio registrado como '$ServiceName'"

Write-Host "▸ Arrancando servicio..."
& $exePath -config $configPath -service-name $ServiceName -start
if ($LASTEXITCODE -ne 0) {
    Write-Host "  ⚠ El servicio se registró pero no arrancó. Revisa $InstallDir\agent.log" -ForegroundColor Yellow
} else {
    Start-Sleep -Seconds 3
    Write-Host "  ✓ Servicio corriendo"
}

# ─────────────────────────────────────────────────────────────────────────
# 7.b El PC no se suspende
# ─────────────────────────────────────────────────────────────────────────
# El servicio arranca solo con Windows —sin que nadie inicie sesión— y se
# reinicia si se cae, pero un PC suspendido no corre nada: la talanquera
# sigue abriendo con la última lista, y la lista deja de ponerse al día.
# Solo conectado a la corriente (`-ac`): un portátil en batería sigue
# cuidando su batería. La pantalla sí puede apagarse.
if (-not $SinCambiarEnergia) {
    Write-Host "▸ Evitando que el PC se suspenda con corriente..."
    try {
        & powercfg.exe /change standby-timeout-ac 0 | Out-Null
        & powercfg.exe /change hibernate-timeout-ac 0 | Out-Null
        Write-Host "  ✓ Con corriente, el PC ya no se suspende ni hiberna (la pantalla sí puede apagarse)"
    } catch {
        Write-Host "  ⚠ No se pudo cambiar la energía: $($_.Exception.Message)" -ForegroundColor Yellow
        Write-Host "    Configúralo a mano: Configuración → Sistema → Energía → Suspender: Nunca."
    }
}

# ─────────────────────────────────────────────────────────────────────────
# 8. Verificación final
# ─────────────────────────────────────────────────────────────────────────

Write-Host ""
Write-Host "══════════════════════════════════════════════════════════════" -ForegroundColor Green
Write-Host "  ✓ INSTALACIÓN COMPLETA" -ForegroundColor Green
Write-Host "══════════════════════════════════════════════════════════════" -ForegroundColor Green
Write-Host ""
Write-Host "  Estado del servicio:"
$svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if ($svc) {
    Write-Host "    $($svc.DisplayName) — $($svc.Status)"
} else {
    Write-Host "    ⚠ Servicio no detectado (revisa $InstallDir\agent.log)" -ForegroundColor Yellow
}

Write-Host ""
Write-Host "  Próximos pasos:"
Write-Host "    1. Verifica logs:    Get-Content $InstallDir\agent.log -Tail 20"
Write-Host "    2. Estado y datos del equipo: abre http://localhost:$ReceiverPort en este PC"
Write-Host "    3. En el panel del conjunto: $CloudUrl/lpr"
Write-Host "       (el equipo debe pasar a 'en línea' en un minuto)"
Write-Host "    4. En la cámara, apunta el envío de eventos a:"
$ipLocal = (Get-NetIPAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue | Where-Object { $_.IPAddress -notlike '127.*' -and $_.IPAddress -notlike '169.254.*' } | Select-Object -First 1).IPAddress
if (-not $ipLocal) { $ipLocal = '<IP de este PC>' }
$familiaOTipo = if ($VendorFamily) { $VendorFamily } else { $CameraType }
switch -Wildcard ($familiaOTipo) {
    'hikvision_dsk1t*' { Write-Host "       http://${ipLocal}:$ReceiverPort/acs-event  (HTTP Listening del terminal)" }
    'dahua*' { Write-Host "       http://${ipLocal}:$ReceiverPort/NotificationInfo/TollgateInfo  (ITSAPI: responde {""Result"": true})" }
    'axis*'  { Write-Host "       http://${ipLocal}:$ReceiverPort/axis-event  (License Plate Verifier, HTTP POST JSON)" }
    default  { Write-Host "       http://${ipLocal}:$ReceiverPort/lpr-event  (HTTP Listening, multipart)" }
}
Write-Host ""
if ($uninstallPath) {
    Write-Host "  Para quitar solo este equipo (PowerShell como administrador):"
    Write-Host "    powershell -ExecutionPolicy Bypass -File $uninstallPath -Servicio $ServiceName"
    Write-Host "  Para quitar todos los equipos de este computador:"
    Write-Host "    powershell -ExecutionPolicy Bypass -File $uninstallPath"
} else {
    Write-Host "  Para desinstalar: uninstall.ps1 de https://github.com/rguzman46/porteria-sync-agent/releases/tag/agent-v__VERSION__"
}
Write-Host ""

# Copias huérfanas: la plataforma ya no reconoce su llave (las borraron en
# Cámaras de placas o les generaron un token nuevo) y se quedan dando 401.
# El agente lo anota en su estado.json (`llave_rechazada`). No se quitan
# solas —un 401 se revisa antes de borrar nada—: se avisa con el comando.
# Del estado.json solo se usa un nombre con la forma del instalador.
$raizDelAgente = 'C:\PorteriaAgent'
$archivosDeEstado = @(Join-Path $raizDelAgente 'estado.json') +
    @(Get-ChildItem $raizDelAgente -Directory -Filter 'equipo-*' -ErrorAction SilentlyContinue |
        ForEach-Object { Join-Path $_.FullName 'estado.json' })
foreach ($archivo in $archivosDeEstado) {
    if (-not (Test-Path $archivo)) { continue }
    try { $otro = Get-Content $archivo -Raw -Encoding UTF8 | ConvertFrom-Json } catch { continue }
    if ($otro.llave_rechazada -ne $true) { continue }
    if ([string]$otro.servicio -notmatch '^PorteriaSyncAgent(-\d{1,3})?$') { continue }
    if ($otro.servicio -eq $ServiceName) { continue }
    Write-Host "  ⚠ $($otro.servicio) es un equipo cuya llave la plataforma ya no reconoce" -ForegroundColor Yellow
    Write-Host "    (borraron el equipo en Cámaras de placas, le generaron un token nuevo o su llave se desactivó)." -ForegroundColor Yellow
    Write-Host "    Si este equipo nuevo lo reemplaza, quítalo:" -ForegroundColor Yellow
    Write-Host "    powershell -ExecutionPolicy Bypass -File $raizDelAgente\uninstall.ps1 -Servicio $($otro.servicio)" -ForegroundColor Yellow
    Write-Host ""
}
