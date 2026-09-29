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
        $CloudUrl = 'https://miconjunto.porteriaplus.com'
        iex (irm "$CloudUrl/integrations/install-script.ps1")

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

    Este script será reemplazado por uno con el SHA real al compilarse
    en GitHub Actions. Los placeholders `__VERSION__` y `__EXPECTED_HASH__`
    se sustituyen en CI.
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
    # De la misma release que este guion. `__VERSION__` lo sustituye CI.
    [string]$BinaryUrl = 'https://github.com/rguzman46/porteria-sync-agent/releases/download/agent-v__VERSION__/porteria-agent.exe',
    [string]$ExpectedHash = '__EXPECTED_HASH__'
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'  # acelera Invoke-WebRequest 10x

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
    exit 1
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
if ($BinaryUrl -like '*__VERSION__*') {
    Write-Host ""
    Write-Host "✗ Este install.ps1 no viene de una release (falta la versión)." -ForegroundColor Red
    Write-Host "  Usa el bloque que arma el panel del conjunto, o pasa -BinaryUrl con la release exacta."
    Write-Host ""
    exit 1
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
    $CloudUrl = Read-Host "URL del cloud (ej: https://catamaran.porteriaplus.com)"
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
if (Get-Service -Name 'PorteriaSyncAgent' -ErrorAction SilentlyContinue) {
    Write-Host "▸ Deteniendo servicio existente para reemplazar binario..."
    Stop-Service -Name 'PorteriaSyncAgent' -Force -ErrorAction SilentlyContinue
    Start-Sleep -Seconds 2
}

Write-Host "▸ Descargando binario desde $BinaryUrl ..."
try {
    Invoke-WebRequest -Uri $BinaryUrl -OutFile $exePath -UseBasicParsing -ErrorAction Stop
    Write-Host "  ✓ Descargado ($(Get-Item $exePath | Select-Object -ExpandProperty Length) bytes)"
} catch {
    Write-Host "  ✗ Error descargando: $($_.Exception.Message)" -ForegroundColor Red
    Write-Host "    Verifica conectividad a internet o que la URL sea correcta."
    exit 1
}

# Verificación de integridad SHA-256 — abortamos si el hash no coincide.
if ($ExpectedHash -and $ExpectedHash -ne '__EXPECTED_HASH__') {
    Write-Host "▸ Verificando SHA-256 ..."
    $actualHash = (Get-FileHash $exePath -Algorithm SHA256).Hash.ToLower()
    $expectedLower = $ExpectedHash.ToLower()
    if ($actualHash -ne $expectedLower) {
        Write-Host "  ✗ Hash NO coincide!" -ForegroundColor Red
        Write-Host "    Esperado: $expectedLower"
        Write-Host "    Recibido: $actualHash"
        Write-Host "    Aborta la instalación. El binario puede estar corrupto o modificado."
        Remove-Item $exePath -Force
        exit 1
    }
    Write-Host "  ✓ SHA-256 OK: $($actualHash.Substring(0,16))..."
} else {
    Write-Host "  ⚠ Hash esperado no proporcionado — saltando verificación (no recomendado para prod)" -ForegroundColor Yellow
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
$ruleName = 'Porteria Sync Agent - receptor LPR'
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
if (Get-Service -Name 'PorteriaSyncAgent' -ErrorAction SilentlyContinue) {
    & $exePath -uninstall 2>&1 | Out-Null
    Start-Sleep -Seconds 2
}

& $exePath -install
if ($LASTEXITCODE -ne 0) {
    Write-Host "  ✗ Error registrando el servicio." -ForegroundColor Red
    exit 1
}
Write-Host "  ✓ Servicio registrado como 'PorteriaSyncAgent'"

Write-Host "▸ Arrancando servicio..."
& $exePath -start
if ($LASTEXITCODE -ne 0) {
    Write-Host "  ⚠ El servicio se registró pero no arrancó. Revisa $InstallDir\agent.log" -ForegroundColor Yellow
} else {
    Start-Sleep -Seconds 3
    Write-Host "  ✓ Servicio corriendo"
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
$svc = Get-Service -Name 'PorteriaSyncAgent' -ErrorAction SilentlyContinue
if ($svc) {
    Write-Host "    $($svc.DisplayName) — $($svc.Status)"
} else {
    Write-Host "    ⚠ Servicio no detectado (revisa $InstallDir\agent.log)" -ForegroundColor Yellow
}

Write-Host ""
Write-Host "  Próximos pasos:"
Write-Host "    1. Verifica logs:    Get-Content $InstallDir\agent.log -Tail 20"
Write-Host "    2. Estado servicio:  Get-Service PorteriaSyncAgent"
Write-Host "    3. Panel cloud:      $CloudUrl/integrations"
Write-Host "       (debes ver 'última sync' actualizado en pocos segundos)"
Write-Host "    4. En la cámara, apunta el envío de eventos a:"
$ipLocal = (Get-NetIPAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue | Where-Object { $_.IPAddress -notlike '127.*' -and $_.IPAddress -notlike '169.254.*' } | Select-Object -First 1).IPAddress
if (-not $ipLocal) { $ipLocal = '<IP de este PC>' }
switch -Wildcard ($CameraType) {
    'dahua*' { Write-Host "       http://${ipLocal}:$ReceiverPort/NotificationInfo/TollgateInfo  (ITSAPI: responde {""Result"": true})" }
    'axis*'  { Write-Host "       http://${ipLocal}:$ReceiverPort/axis-event  (License Plate Verifier, HTTP POST JSON)" }
    default  { Write-Host "       http://${ipLocal}:$ReceiverPort/lpr-event  (HTTP Listening, multipart)" }
}
Write-Host ""
Write-Host "  Para desinstalar:      iex (irm '$CloudUrl/integrations/uninstall-script.ps1')"
Write-Host ""
