# porteria-sync-agent

Binario pequeño (~6 MB cuando se compila con `-s -w`) que corre en el PC del portero del conjunto. Sincroniza placas autorizadas desde el cloud de Porteria Plus a la cámara LPR local del conjunto.

**Diseño**: offline-first. La cámara YA tiene la whitelist sincronizada y decide localmente — la portería NUNCA depende del cloud para abrir.

---

## Stack

- Go 1.22+
- `github.com/kardianos/service` — self-install como Windows Service / launchd / systemd
- `gopkg.in/yaml.v3` — config loader

Sin más dependencias externas para minimizar superficie de ataque y tamaño del binario.

---

## Build

```bash
# Binario para la plataforma actual
make build

# Cross-compile para Windows (deploy típico portería Colombia)
make windows

# Todas las plataformas
make all
```

El SHA-256 del binario Windows se inyecta en `install.ps1` (Día 6) para que el script verifique integridad post-download.

---

## Instalación (operador del conjunto)

Hoy (V1) la instalación es manual con PowerShell. Día 6 entrega un script `install.ps1` one-shot.

```powershell
# 1. Crear directorio + config
mkdir C:\PorteriaAgent
cd C:\PorteriaAgent

# 2. Descargar binario (CDN de Porteria Plus)
Invoke-WebRequest -Uri "https://cdn.porteriaplus.com/agent/latest/porteria-agent.exe" -OutFile porteria-agent.exe

# 3. Copiar config y editarla
Copy-Item config.example.yaml config.yaml
notepad config.yaml   # rellenar token + camera IP + credenciales

# 4. Instalar como Windows Service y arrancarlo
.\porteria-agent.exe -install
.\porteria-agent.exe -start

# 5. Verificar estado
.\porteria-agent.exe -status
Get-Content agent.log -Tail 50
```

---

## Configuración

Ver `config.example.yaml`. Las credenciales sensibles también pueden inyectarse por variables de entorno (útil para deploy automatizado):

```
PORTERIA_CLOUD_URL=https://catamaran.porteriaplus.com
PORTERIA_CLOUD_TOKEN=pa_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
PORTERIA_CAMERA_HOST=192.168.1.50
PORTERIA_CAMERA_USER=admin
PORTERIA_CAMERA_PASSWORD=...
```

Las env vars sobreescriben los valores del yaml.

---

## Vendors soportados

| Vendor | Estado V1 | API |
|---|---|---|
| Hikvision | ✅ Estable | ISAPI v2 (Digest auth, PUT `/ISAPI/Traffic/.../plateInfo`) |
| Dahua | ⏳ Stub (próximo release) | CGI (Basic/Digest) |
| Axis | ⏳ Stub (próximo release) | VAPIX + ACAP License Plate Verifier |

Si tu conjunto usa otro vendor, contacta `soporte@porteriaplus.com` para que te incluyamos en el beta.

---

## Flujo runtime

```
┌────────────────────┐  every 60s  ┌─────────────────┐
│ porteria-sync-agent├────────────►│ porteriaplus.com│
│  (PC del portero)  │             │  (cloud)        │
└────────────────────┘             └─────────────────┘
        │                                  │
        │   1) POST /api/access/heartbeat  │
        │   2) GET /api/access/whitelist   │
        │      (If-Modified-Since header)  │
        │   ◄── 304 (sin cambios) ó        │
        │       200 + lista de placas       │
        │                                  │
        │ Si hay cambios:                  │
        ▼                                  │
┌────────────────────┐                     │
│ Cámara LPR local   │                     │
│  (Hikvision/etc.)  │                     │
│   PUT plateInfo    │                     │
└────────────────────┘                     │
        │                                  │
        │  La cámara opera 100% offline:   │
        │  detecta placa → consulta su     │
        │  whitelist local → abre talanquera│
        │                                  │
        └──── (cloud no está en el path)  ─┘
```

### Resiliencia

| Falla | Comportamiento |
|---|---|
| Internet del conjunto cae | Cámara opera local (whitelist sincronizada antes de la caída) |
| Cloud Porteria Plus cae | Idem — el agent reintenta con back-off exponencial, no pánico |
| Cámara se reinicia | La whitelist persiste en flash; en el próximo poll del agent se reverifica |
| Agent se detiene (servicio stop) | Whitelist queda en cámara; portería opera offline |
| PC del portero se apaga | Whitelist queda en cámara; al reencender, el agent re-inicia y resincroniza |

---

## Logs y monitoreo

```powershell
# Ver últimas 50 líneas del log
Get-Content C:\PorteriaAgent\agent.log -Tail 50

# Seguir en tiempo real
Get-Content C:\PorteriaAgent\agent.log -Wait
```

Heartbeats reportan al cloud cada 60s. Si el cloud no recibe heartbeats por >2h, el job `acceso:alertar-devices-inactivos` notifica al admin del conjunto (in-app, panel `/integrations`).

---

## Licencia

MIT (cuando se publique como repo independiente `github.com/porteriaplus/porteria-sync-agent`).
