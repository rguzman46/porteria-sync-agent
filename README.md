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
PORTERIA_CLOUD_URL=https://miconjunto.porteriaplus.com
PORTERIA_CLOUD_TOKEN=ppk_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
PORTERIA_DEVICE_TOKEN=ppd_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
PORTERIA_CAMERA_HOST=192.168.1.50
PORTERIA_CAMERA_USER=admin
PORTERIA_CAMERA_PASSWORD=...
```

Las env vars sobreescriben los valores del yaml.

### Dos tokens, y no son lo mismo

| | Qué identifica | Dónde se saca |
|---|---|---|
| `token` (`ppk_…`) | **El conjunto** | Integraciones y API keys |
| `device_token` (`ppd_…`) | **Esta cámara** | Al registrarla; se muestra una sola vez |

Sin `device_token` el cloud atribuye las lecturas al primer dispositivo activo
del conjunto. En una portería con cámara de entrada **y** de salida eso
significa que **las salidas se registran como entradas**: el sentido y el modo
son del dispositivo, no de la llave. La visita no se cierra al salir y el conteo
de quién está adentro solo crece.

El agent lo avisa en el log al arrancar si falta. Las instalaciones viejas sin
él siguen funcionando como antes —atribuidas al primer dispositivo—, pero
cualquier instalación nueva debe ponerlo.

### Qué reporta el latido

Cada latido cuenta **cómo le está yendo**, no solo que está vivo:

| Campo | Para qué |
|---|---|
| `agent_version`, `system_info` | Diagnosticar sin ir al PC de la portería |
| `camera_push_ok` / `camera_push_error` | Si logró escribirle la lista a la cámara |
| `queue_size` | Cuántas lecturas quedaron represadas por falta de internet |
| `plates_pushed` | Cuántas placas quedaron en la cámara |

El segundo es el que importa y el que faltaba. **Estar vivo y tener la cámara al
día no son lo mismo**: el agent puede estar corriendo, con internet, bajando la
lista sin un error, y no poder escribirla en la cámara porque le cambiaron la
clave. En ese estado el panel se veía verde mientras la lista de la cámara
llevaba semanas congelada —los residentes nuevos no entran, los pases revocados
siguen abriendo— y nadie se enteraba hasta que alguien reclamaba.

---

## Vendors y familias soportadas

V1.3+ soporta múltiples marcas y **sub-familias** dentro de cada marca (porque diferentes líneas del mismo vendor a veces usan endpoints distintos). El admin elige el modelo exacto en `/integrations` del cloud; el sync agent recibe el `vendor_family` resuelto en el heartbeat/whitelist y carga el adapter correcto automáticamente.

| Familia (`vendor_family`) | Marca | Endpoint | Auth | Modelos soportados | Estado |
|---|---|---|---|---|---|
| `hikvision_traffic` | Hikvision (línea profesional) | `/ISAPI/Traffic/channels/1/vehicleDetect/plateInfo` | Digest | iDS-2CD7A26G0/P-IZHS, iDS-TCM403-MA, iDS-TCM203-A, DS-2CD7A85G0-LPR | ✅ Estable |
| `hikvision_itc` | Hikvision (línea ITC Entrance) | `/ISAPI/ITC/Entrance/VCL` | Digest | DS-TCG405-E, DS-TCG405-E/H, DS-TCG411-E, DS-TCG615-EI | ⏳ Beta |
| `dahua_itc` | Dahua | `/cgi-bin/recordUpdater.cgi` (CGI) | Digest | ITC215-PW6M-IRLZF, ITC237-PU1B-IRZF, ITC413-PW4D-IZ | ⏳ Beta |
| `axis_vapix` | Axis | `/local/lpv/.api` (ACAP LPV) | Digest | P3265-LVE, Q1700-LE (requiere licencia ACAP "License Plate Verifier") | ⏳ Beta |

**Retrocompat con configs viejas (V0.1-V0.2):** `type: hikvision` sin `family` → resuelve a `hikvision_traffic` (línea de producción hoy). `dahua` → `dahua_itc`. `axis` → `axis_vapix`.

### Auto-config plug-and-play

Si `auto_config: true` (default), el agent acepta reconfigurarse en caliente cuando el cloud reporta una `vendor_family` distinta. Útil cuando el admin del conjunto cambia el modelo de la cámara en `/integrations` — el agent detecta el cambio en el siguiente heartbeat (≤60s) y carga el adapter nuevo sin reiniciar el servicio.

Para pin manual (debug, hardware experimental, etc.), pon `auto_config: false` en `config.yaml`.

### Cómo agregar un nuevo vendor / familia

1. **Catálogo en el cloud** (`config/lpr.php` en repo `SaaS`): agrega la marca y/o familia con modelos + endpoint + dialecto auth.
2. **Adapter Go en este repo:** crea `camera_<vendor>.go` que implemente la interfaz `CameraAdapter` (Name, Ping, SyncWhitelist). Usa `digestClient` si la cámara requiere Digest auth (compartido).
3. **Registrarlo en el factory** `camera.go::NewCameraAdapter()` con un nuevo `case "vendor_family":`.
4. **Tests** en `camera_test.go` — al menos: Ping endpoint correcto, SyncWhitelist envía al endpoint esperado con XML/JSON válido.
5. **Receiver dispatch** en `receiver.go` si el vendor empuja eventos al `:8787/lpr-event` con formato distinto (ej. nuevo XML schema). Si reusa el parser `generic` (event_data JSON + snapshot), no hay que tocar nada.

Si tu conjunto usa un vendor que no está listado, contacta `soporte@porteriaplus.com` para que te incluyamos en el beta del adapter.

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
| **v1.4.0** (próxima) | El latido cuenta **cómo le está yendo**: `camera_push_ok`, el motivo cuando falla y el tamaño de la cola. Y manda `X-Device-Token`, sin el cual el cloud atribuía las lecturas al primer dispositivo del conjunto. |
| v1.3.0 | Plug-and-play multi-vendor: Hikvision ITC, Dahua y Axis completos, `digestClient` compartido y auto-configuración del adapter según lo que reporte el cloud. |
| v0.2.0 | Receiver HTTP server `:8787` + queue file-based + replay worker con back-off. La cámara LPR envía multipart al agent, el agent reenvía al cloud. Resiliencia offline natural. |
| v0.1.0 | Whitelist sync + heartbeat. Adapter Hikvision estable (ISAPI v2 + Digest auth). Self-install como Windows Service. |

**Backward compat**: una config sin `cloud.device_token` sigue funcionando —el
cloud cae al primer dispositivo— y una sin sección `receiver:` corre solo como
puller. Los campos nuevos del latido son opcionales del lado del cloud, así que
un agent nuevo contra un cloud viejo tampoco se rompe.

Para activar captura visual, añadir al yaml:

```yaml
receiver:
  enabled: true
```

---

## Licencia

MIT (cuando se publique como repo independiente `github.com/rguzman46/porteria-sync-agent`).
