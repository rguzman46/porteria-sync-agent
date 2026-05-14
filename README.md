# porteria-sync-agent

Binario pequeño (~6-7 MB cuando se compila con `-s -w`) que corre en el PC del portero del conjunto. Hace dos cosas:

1. **Whitelist sync (v0.1.0+)**: sincroniza placas autorizadas desde el cloud de Porteria Plus a la cámara LPR local del conjunto. La cámara opera offline-first — la portería NUNCA depende del cloud para abrir.

2. **Captura visual relay (v0.2.0+)**: recibe el snapshot del vehículo desde la cámara LPR (multipart/form-data en `:8787`) y lo reenvía al cloud. Si no hay internet, encola localmente y replay con back-off cuando vuelve. **Cero pérdida de eventos** durante outages.

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

### Modo puller (v0.1.0+) — whitelist sync

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

### Modo receiver (v0.2.0+) — captura visual

```
┌──────────────┐  multipart/form  ┌─────────────────────┐  multipart/form  ┌─────────────────────┐
│ Cámara LPR   │ ───────────────► │ porteria-sync-agent │ ───────────────► │ POST /api/access/   │
│ (Hikvision   │  scene.jpeg +    │  HTTP server :8787  │  event_data +    │   event/multipart   │
│  HTTP Lis-   │  XML metadata    │  Queue file-based   │  snapshot bin    │   (cloud)           │
│  tener)      │  (LAN)           │  Replay worker      │  (TLS)           │ - guarda DO Spaces  │
└──────────────┘                  │                     │                  │ - crea visita+foto  │
                                  └─────────────────────┘                  └─────────────────────┘
                                         │
                                         │ Si cloud caído:
                                         │ encola en disco local
                                         ▼
                                  <queue_dir>/
                                    {ts}_{rand}.json  ← metadata
                                    {ts}_{rand}.bin   ← imagen raw
```

**Drenado de queue cuando vuelve internet**: el replay worker (default tick 30s) lee los más antiguos primero (FIFO por timestamp prefix en filename) y los envía al cloud preservando el `timestamp` original — el cloud reconstruye la cronología correcta en `access_events.ocurrida_en`.

### Resiliencia

| Falla | Comportamiento |
|---|---|
| Internet del conjunto cae | Cámara opera local (whitelist sincronizada antes de la caída). Snapshots se encolan en disco — cero pérdida. |
| Cloud Porteria Plus cae | Idem — el agent reintenta con back-off exponencial (1m → 5m → 15m → 1h), no pánico |
| Cámara se reinicia | La whitelist persiste en flash; en el próximo poll del agent se reverifica |
| Agent se detiene (servicio stop) | Whitelist queda en cámara; portería opera offline. Snapshots quedan en queue local — se drenan cuando arranca de nuevo. |
| PC del portero se apaga | Whitelist queda en cámara; al reencender, el agent re-inicia y resincroniza + drena queue acumulada. |
| Queue local se llena (1GB/10k items) | Eviction FIFO de los más antiguos + warning en log. Defaults absorben ~12 días de outage continuo. |

---

## Logs y monitoreo

```powershell
# Ver últimas 50 líneas del log
Get-Content C:\PorteriaAgent\agent.log -Tail 50

# Seguir en tiempo real
Get-Content C:\PorteriaAgent\agent.log -Wait

# Health check del receiver (sin auth, LAN-only)
curl http://localhost:8787/health
# → {"ok":true,"agent_version":"0.2.0","queue_items":0,"queue_bytes":0,...}
```

Heartbeats reportan al cloud cada 60s. Si el cloud no recibe heartbeats por >2h, el job `acceso:alertar-devices-inactivos` notifica al admin del conjunto (in-app, panel `/integrations`).

### Testear el receiver manualmente

```bash
curl -X POST http://localhost:8787/lpr-event \
  -H "X-Agent-Source: generic" \
  -F 'event_data={"plate":"TEST01","direction":"entry"}' \
  -F 'snapshot=@carro.jpg;type=image/jpeg'
# → 202 Accepted {"ok":true,"event_id":"...","queued":true}
```

Verás en el log del agent: `[receiver] encolado evento placa=TEST01` seguido por `[replay] ✓ enviado evento` cuando el cloud confirme la subida.

---

## Configuración del HTTP Listener Hikvision

Una vez instalado el agent v0.2.0+ con `receiver.enabled: true`, configurar en el panel web de la cámara:

1. `Network > Advanced > HTTP Listening`
2. `Enable HTTP Listening` ✓
3. URL: `http://<IP_PC_PORTERO>:8787/lpr-event`
4. Method: `POST`
5. Content-Type: `multipart/form-data`
6. `Event > Smart Event > Plate Verification > Linkage Method` → `Notify Surveillance Center` ✓
7. `Event > Smart Event > Plate Verification > Picture Upload`:
   - ✅ `Upload Scene Image` (escena completa)
   - ❌ `Upload Plate Cropped Image` (redundante — ya tenemos texto OCR)
   - ❌ `Upload Face Image` (decisión Habeas Data — no almacenamos rostros)

Para guía paso a paso con screenshots: ver [`docs/integracion-lpr-hikvision.md`](../docs/integracion-lpr-hikvision.md) en el repo principal.

---

## Versionado

| Versión | Lo que añade |
|---|---|
| **v0.2.0** (próxima) | Receiver HTTP server `:8787` + queue file-based + replay worker con back-off. La cámara LPR envía multipart al agent, el agent reenvía al cloud. Resiliencia offline natural. |
| v0.1.0 | Whitelist sync + heartbeat. Adapter Hikvision estable (ISAPI v2 + Digest auth). Self-install como Windows Service. |

**Backward compat**: configs v0.1.x sin sección `receiver:` siguen corriendo solo como puller en v0.2.0+ (modo legacy). Para activar captura visual, añadir al yaml:

```yaml
receiver:
  enabled: true
```

---

## Licencia

MIT (cuando se publique como repo independiente `github.com/rguzman46/porteria-sync-agent`).
