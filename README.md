# porteria-sync-agent

Binario pequeño (~7 MB con `-s -w`) que corre en el PC de la portería del
conjunto. Hace dos cosas:

1. **Sincroniza las listas de placas** desde la plataforma a la cámara LPR
   local: permitidas y negadas. La cámara opera offline-first — la portería
   NUNCA depende del cloud para abrir.

2. **Recibe las lecturas de la cámara** (foto + placa, en el dialecto de cada
   marca) y las reenvía a la plataforma. Sin internet, encola en disco y drena
   cuando vuelve. Nada se descarta por falta de red.

---

## Stack

- Go 1.22+
- `github.com/kardianos/service` — self-install como Windows Service / launchd / systemd
- `gopkg.in/yaml.v3` — config loader

Sin más dependencias externas para minimizar superficie de ataque y tamaño del binario.

---

## Build

```bash
make build          # plataforma actual
make windows        # Windows amd64 (deploy típico en portería)
make linux-arm64    # Raspberry Pi 3/4/5 con SO de 64 bits
make all
make test
```

El release (`.github/workflows/release.yml`, tag `agent-vX.Y.Z`) produce
Windows amd64, Linux amd64, Linux arm64 y macOS arm64, más `install.ps1`
con el SHA-256 y la versión inyectados.

---

## Instalación (operador del conjunto)

**Un solo pegue.** El panel del conjunto arma el bloque completo al registrar
la cámara —con la llave, el token de esa cámara, la URL, la marca y la familia
del modelo ya puestos—; en el PC de la portería se abre PowerShell **como
administrador**, se pega y ya:

```powershell
Set-ExecutionPolicy -Scope Process Bypass -Force
$Token        = 'ppk_...'                          # la llave del conjunto
$DeviceToken  = 'ppd_...'                          # el token de ESTA cámara
$CloudUrl     = 'https://miconjunto.porteriaplus.com'
$CameraType   = 'hikvision'                        # hikvision | dahua | axis
$VendorFamily = 'hikvision_itc'                    # opcional, según el modelo
iex (irm "$CloudUrl/integrations/install-script.ps1")
```

Lo único que pregunta es lo que el panel no puede saber: **la IP de la cámara
y su usuario y contraseña**. Y no se pasan por la URL a propósito: quedarían
en el historial de la shell.

El guion hace, en orden:

1. Crea `C:\PorteriaAgent` y agrega la exclusión de Windows Defender.
2. Descarga el binario **de la misma release que el guion** (la URL lleva el
   tag; nunca `latest`) y verifica el SHA-256. Un guion que no salió de una
   release se detiene en vez de adivinar.
3. Escribe `config.yaml` (solo Admin + SYSTEM pueden leerlo) con las
   secciones `cloud`, `camera`, `poll`, `log` y `receiver` —esta última
   encendida—.
4. **Abre el puerto 8787 en el firewall de Windows** con `New-NetFirewallRule`,
   solo para la IP de la cámara. Idempotente: si la regla existe, la
   actualiza. Sin esto la cámara postea y Windows lo bota en silencio.
5. Registra y arranca el servicio `PorteriaSyncAgent`.
6. Imprime la URL exacta que hay que configurar en la cámara según su marca.

### Los dos tokens, y por qué van los dos

Es el error de instalación más caro y no se nota el día que pasa:

- `Token` (`ppk_…`) dice **de qué conjunto** son las lecturas.
- `DeviceToken` (`ppd_…`) dice **de cuál cámara**.

Sin el segundo, el cloud atribuye todo al primer dispositivo activo del
conjunto. En una portería con cámara de entrada **y** de salida, eso significa
que **las salidas se registran como entradas**: las visitas no se cierran y el
conteo de quién está adentro solo crece. Todo se ve funcionando.

Por eso el instalador lo pide si no viene y no deja seguir sin él. Las llaves
`pa_…` de la plataforma anterior siguen aceptadas.

### A mano, si hace falta

```powershell
mkdir C:\PorteriaAgent; cd C:\PorteriaAgent
# Descarga porteria-agent.exe de la release agent-v1.4.0 (no de latest)
Copy-Item config.example.yaml config.yaml
notepad config.yaml            # token, device_token, IP y credenciales de la cámara
New-NetFirewallRule -DisplayName 'Porteria Sync Agent - receptor LPR' -Direction Inbound -Protocol TCP -LocalPort 8787 -RemoteAddress <IP de la cámara> -Action Allow
.\porteria-agent.exe -install
.\porteria-agent.exe -start
.\porteria-agent.exe -status
Get-Content agent.log -Tail 50
```

En Raspberry Pi / Linux: `porteria-agent-linux-arm64 -install` registra la
unidad de systemd; el puerto se abre con `ufw allow from <IP cámara> to any port 8787`.

---

## Configuración

Ver `config.example.yaml`: cada campo está explicado ahí. Las credenciales
también pueden inyectarse por variables de entorno (sobreescriben el yaml):

```
PORTERIA_CLOUD_URL, PORTERIA_CLOUD_TOKEN, PORTERIA_DEVICE_TOKEN,
PORTERIA_CAMERA_TYPE, PORTERIA_CAMERA_FAMILY, PORTERIA_CAMERA_HOST,
PORTERIA_CAMERA_USER, PORTERIA_CAMERA_PASSWORD
```

### Quién puede postear al receptor

El puerto 8787 escucha en toda la LAN de la portería. Una lectura falsa
posteada desde otro PC abriría una visita en el panel con una foto inventada,
y las cámaras no saben mandar un Bearer. Por eso el filtro es por IP de
origen: se acepta `camera.host` y lo que esté en `receiver.allow_from` (IPs o
redes CIDR); el resto recibe **403**. `receiver.allow_any: true` lo apaga,
solo para diagnosticar. `/health` queda abierto: no escribe nada.

Para probar con curl desde el mismo PC, agrega `127.0.0.1` a `allow_from`.

### El log

`log.file` se escribe de verdad (además de la salida estándar, que captura el
gestor de servicios). Relativo → junto al `config.yaml`. Rota por tamaño:
al pasar de `max_size_mb` (10) se renombra a `.1`, `.2`… y se conservan
`keep` (3) copias. Permisos 0600: lleva placas y nombres.

---

## Contrato con la plataforma

Todas las peticiones llevan `Authorization: Bearer <ppk_…>`,
`X-Device-Token: <ppd_…>` y `User-Agent: PorteriaSyncAgent/<versión> (<os>/<arch>)`.

### Lectura — `POST /api/v1/access/event/multipart`

| Parte | Tipo | Contenido |
|---|---|---|
| `event_data` | `application/json` | ver tabla siguiente |
| `snapshot` | `image/jpeg` (o el original) | la escena, ≤ 4 MB (se recomprime si pesa más) |
| `plate_crop` | `image/jpeg` | el recorte de la placa; **solo si la cámara lo mandó** |

`event_data`:

| Campo | Cuándo viaja | Origen |
|---|---|---|
| `plate` | siempre | la cámara, en mayúsculas sin espacios |
| `client_event_id` | siempre | el ID de la cola, o uno estable del adaptador (Dahua: hash de `PicName + AccurateTime`; Axis: `carID` + día) para que los reenvíos de la cámara no dupliquen |
| `direction` | si la cámara lo dice | `entry` \| `exit` (Hikvision forward/reverse, Axis in/out, Dahua best-effort) |
| `timestamp` | si la cámara lo dice; **nunca vacío** | ISO 8601; sin zona el cloud lo lee en hora de Bogotá |
| `confidence` | si la cámara lo dice | tal cual (0–100 o 0–1; el cloud normaliza) |
| `plate_box` | si la cámara lo entrega **en píxeles de la escena** | `{xmin, ymin, xmax, ymax}` — Hikvision `pictureInfo/plateRect`, Axis `plateCoordinates`; Dahua no (unidades sin confirmar, va en `metadata.dahua_bounding_box`) |
| `metadata` | si hay | `source`, y lo propio de cada marca (`lane`, `plate_color`, `pic_name`, `car_id`…) |

Un 2xx borra el evento de la cola. Un **4xx es permanente** (llave revocada,
payload inválido, captura apagada) y se descarta con aviso en el log. Un
**5xx, 429 o error de red es transitorio**: se reintenta con back-off 30 s →
1 min → 2 min → 5 min (tope) **sin límite de intentos**. El único techo es
la cola (10.000 eventos / 1 GB), que expulsa lo más viejo.

### Latido — `POST /api/v1/access/heartbeat`

| Campo | Para qué |
|---|---|
| `agent_version`, `system_info` | Diagnosticar sin ir al PC de la portería |
| `queue_size` | Cuántas lecturas quedaron represadas por falta de internet |
| `camera_push_ok` / `camera_push_error` | Si logró escribirle la lista a la cámara (ausente hasta el primer intento) |
| `plates_pushed` | Cuántas placas quedaron en la cámara |
| `whitelist_supported` / `blocklist_supported` | Qué listas sabe escribir el adaptador activo **de verdad** |

**Estar vivo y tener la cámara al día no son lo mismo**: el agent puede estar
corriendo, con internet, bajando la lista sin un error, y no poder escribirla
en la cámara porque le cambiaron la clave. Sin `camera_push_ok` ese estado era
invisible. Y sin `*_supported`, el panel prometía una lista negra que el
adaptador nunca escribía.

La respuesta trae `whitelist_version`. Si algún día trae `device`, el agente
lo acepta para reconfigurarse; hoy eso llega con el whitelist.

### Lista — `GET /api/v1/access/whitelist`

Con `If-Modified-Since`; 304 si nada cambió. El cuerpo trae `plates`
(permitidas) y **`blocked_plates`** (negadas, solo severidad `block`), en
claves separadas para que un agente viejo ignore las negadas en vez de
tomarlas por permitidas. El agente escribe las dos en la cámara donde el
vendor lo soporta (tabla siguiente). Opcionalmente `device.vendor_family`
para reconfigurar el adaptador en caliente.

---

## Vendors y familias soportadas

| Familia | Escritura de listas | Permitidas | Negadas | Recepción de lecturas | Estado |
|---|---|---|---|---|---|
| `hikvision_traffic` | `PUT /ISAPI/Traffic/channels/1/vehicleDetect/plateInfo` (`plateType` 0/1) | ✅ | ✅ | HTTP Listening multipart → `/lpr-event` | ✅ Estable |
| `hikvision_itc` | `PUT /ISAPI/ITC/Entrance/VCL` (`plateType` 0/1) | ✅ | ✅ | ídem | ⏳ Beta |
| `hikvision_anpr_audit` | `PUT /ISAPI/Traffic/channels/1/licensePlateAuditData?fileType=xml` (`listType` whiteList/blackList) | ✅ | ✅ | ídem | ⚠ Por confirmar |
| `dahua_itc` | **no confirmada** (ver abajo) | ❌ | ❌ | ITSAPI JSON → `/NotificationInfo/TollgateInfo` | ⏳ Solo recepción |
| `axis_vapix` | `/local/fflprapp/api.cgi` (`addplate`/`delplate`, listas `allowlist`/`blocklist`) | ✅ | ✅ | License Plate Verifier JSON → `/axis-event` | ⏳ Beta |

Auth: HTTP Digest en todos.

### Hikvision: tres endpoints, y hay que confirmar con la cámara al lado

Según la guía «How to integrate with Hikvision LPR function via ISAPI»
(v1.0.0) y lo reportado por integradores, la lista se escribe en uno de tres
sitios según la línea y el firmware:

- `vehicleDetect/plateInfo` — línea Traffic (`hikvision_traffic`).
- `ITC/Entrance/VCL` — línea ITC Entrance y firmwares viejos (`hikvision_itc`).
- `licensePlateAuditData?fileType=xml` — firmwares recientes, p. ej.
  iDS-2CD7A26 (`hikvision_anpr_audit`). Permitidas y negadas en un solo
  archivo. **El endpoint está documentado; el cuerpo XML no se ha validado
  contra un firmware físico.** Si la cámara responde 400, `GET …?fileType=xml`
  devuelve el formato exacto y lo único que hay que ajustar es
  `buildHikvisionAuditXML` en `camera_hikvision_audit.go`.

El día de la instalación se prueba el que corresponda al modelo; si la cámara
responde 404 se cambia `family` (o el modelo en el panel, que reconfigura el
agente solo) y se vuelve a probar. `docs/piloto-lpr.md` del repo principal
tiene la lista de comprobación.

**Recepción**: la cámara hace POST multipart con `anpr.xml` +
`licensePlatePicture.jpg` (recorte) + `detectionPicture.jpg` (escena). El
agente identifica cada imagen **por nombre** (parte, archivo o
`pictureInfoList` del XML), nunca por orden; con dos imágenes sin nombre, la
primera es la escena y **no se adivina** el recorte. Los rostros
(`face`, `driver`) se descartan siempre (Habeas Data). Se responde **200 con
cuerpo vacío**, que es lo que la cámara espera; un evento que no es ANPR
también se contesta 200 para que no insista.

Configuración en la cámara: `Network > Advanced > HTTP Listening` →
URL `http://<IP_PC_PORTERO>:8787/lpr-event`, POST, multipart; en
`Event > Smart Event > Plate Verification > Picture Upload` dejar escena y
recorte de placa, **nunca** rostro.

### Dahua: recibe, no escribe (todavía)

La cámara hace `POST /NotificationInfo/TollgateInfo` (lectura) y
`POST /NotificationInfo/KeepAlive` (latido) al receptor, con JSON
(`Picture.Plate.PlateNumber`, `Confidence`, `BoundingBox`, `CutoutPic.Content`
y `NormalPic.Content` en base64, `SnapInfo.AccurateTime`). El agente responde
**`{"Result": true}`**: con cualquier otra cosa la cámara reenvía el mismo
evento en bucle, y por si lo hace igual, `client_event_id` es un hash de
`PicName + AccurateTime` para que el cloud deduplique.

La **escritura de listas no está confirmada**. La versión anterior escribía en
`recordUpdater.cgi?name=AccessControlCardList`, que es la tabla de *tarjetas*
del control de acceso, no la de placas: la cámara la aceptaba y no la usaba
para abrir. Se quitó. El candidato documentado es
`name=TrafficRedList` / `TrafficBlackList`; hasta probarlo con una cámara al
lado, el adaptador reporta `whitelist_supported=false` y
`blocklist_supported=false`, el agente no intenta el push y el panel lo
muestra. Con Dahua, hoy, la cámara abre con la lista que se le cargue a mano y
la plataforma recibe las lecturas.

En la cámara: `Setting > Network > Platform Server` (o ITSAPI) → dirección
del PC, puerto 8787.

### Axis: License Plate Verifier

El ACAP hace HTTP POST con JSON: `plateASCII`/`plateUTF8`, `plateConfidence`,
`carMoveDirection` (in/out), `carID`, `carState` (`new`/`update`/`lost`),
`plateCoordinates` `[left, top, width, height]` en píxeles, `imageType`
(`plate`/`frame`/`vehicle`) e `imageArray` en base64. El agente toma **solo
`new`** por carro (o el último `lost` si en el lote no vino el `new`),
ignora los `update`, y junta las imágenes del mismo `carID` del lote. Las
`imagesURI` no se siguen: exigirían pedirle la foto a la cámara con
credenciales desde el receptor.

Listas: `addplate`/`delplate` por placa sobre `allowlist` y `blocklist`. El
agente retira lo que estaba en el push anterior y ya no está; la conciliación
contra lo que la cámara tiene de verdad necesita `export<lista>`, cuyo formato
hay que confirmar con la cámara. La vigencia no se manda (formato sin
confirmar): el cloud ya excluye lo vencido y la placa sale en el siguiente
push. El endpoint `/local/lpv/.api` de la versión anterior no aparece en la
documentación de Axis y se reemplazó.

En la cámara: en la app License Plate Verifier, `Event > HTTP POST` →
`http://<IP_PC_PORTERO>:8787/axis-event`.

### Auto-config plug-and-play

Si `auto_config: true` (default), el agent se reconfigura en caliente cuando
el cloud reporta una `vendor_family` distinta con el whitelist. Útil cuando
el admin cambia el modelo de la cámara en el panel: el agent carga el adapter
nuevo sin reiniciar el servicio.

### Cómo agregar un nuevo vendor / familia

1. **Catálogo en el cloud** (`backend/app/tenant/lpr/catalogo.py`): una línea.
2. **Adapter Go en este repo:** `camera_<vendor>.go` que implemente
   `CameraAdapter` (`Name`, `Ping`, `Capacidades`, `SyncWhitelist`). Si el
   endpoint no está confirmado contra hardware, `Capacidades()` lo dice.
3. **Registrarlo** en `camera.go::NewCameraAdapter()` y en `familiasValidas`
   de `config.go`.
4. **Tests** en `camera_test.go` / `camera_listas_test.go`.
5. **Receiver** en `receiver.go` si el vendor empuja en un formato propio.

---

## Flujo runtime

```
┌────────────────────┐  cada 60 s   ┌─────────────────┐
│ porteria-sync-agent├─────────────►│ porteriaplus.com│
│  (PC del portero)  │              │  (API)          │
└────────────────────┘              └─────────────────┘
        │   1) POST /api/v1/access/heartbeat
        │   2) GET  /api/v1/access/whitelist (If-Modified-Since)
        │      ◄── 304 ó 200 + plates + blocked_plates
        ▼
┌────────────────────┐
│ Cámara LPR local   │  PUT lista (permitidas + negadas)
└────────────────────┘
        │  La cámara decide sola: placa → lista local → talanquera.
        └── (el cloud no está en el camino de abrir)

┌──────────────┐  POST (multipart/JSON)  ┌─────────────────────┐  multipart   ┌──────────────────────────┐
│ Cámara LPR   │ ──────────────────────► │ agent :8787         │ ───────────► │ POST /api/v1/access/     │
│              │  foto + placa           │  filtro por IP      │  event_data  │   event/multipart        │
│              │                         │  cola en disco      │  snapshot    │                          │
└──────────────┘                         │  replay con backoff │  plate_crop  └──────────────────────────┘
                                         └─────────────────────┘
```

### Resiliencia

| Falla | Comportamiento |
|---|---|
| Internet del conjunto cae | Cámara opera local. Fotos se encolan en disco — cero pérdida. |
| Cloud cae (5xx) o red intermitente | Reintento con back-off 30 s → 5 min tope, **sin límite de intentos**. |
| Cloud rechaza (4xx) | Descarte con aviso: reintentar no lo arregla. 429 cuenta como transitorio. |
| Foto de más de 4 MB | Se recomprime como JPEG (calidad descendente, luego mitad de resolución) antes de encolar. |
| Cámara se reinicia | La lista persiste en flash; en el próximo poll se reverifica. |
| Agent se detiene / PC se apaga | Lista queda en cámara; la cola en disco se drena al arrancar. |
| Cola llena (1 GB / 10k) | Expulsión FIFO de los más antiguos + warning. ~12 días de corte continuo. |
| Otro PC de la LAN postea | 403. |

---

## Logs y monitoreo

```powershell
Get-Content C:\PorteriaAgent\agent.log -Tail 50
Get-Content C:\PorteriaAgent\agent.log -Wait
curl http://localhost:8787/health
# → {"ok":true,"agent_version":"1.4.0","queue_items":0,"queue_bytes":0,...}
```

### Probar el receptor a mano

Con `127.0.0.1` en `receiver.allow_from`:

```bash
curl -X POST http://localhost:8787/lpr-event \
  -H "X-Agent-Source: generic" \
  -F 'event_data={"plate":"TEST01","direction":"entry","confidence":0.97}' \
  -F 'snapshot=@carro.jpg;type=image/jpeg' \
  -F 'plate_crop=@placa.jpg;type=image/jpeg'
# → 202 Accepted {"ok":true,"event_id":"...","queued":true}
```

En el log: `[receiver] encolado evento placa=TEST01` y luego
`[replay] ✓ enviado evento` cuando el cloud confirme.

Contra la plataforma real (verifica los nombres de los campos del latido):
`PP_URL=… PP_KEY=ppk_… PP_DEVICE=ppd_… go test -run TestContraPlataformaReal -v`.

---

## Versionado

| Versión | Lo que añade |
|---|---|
| **v1.4.0** | Contrato v1 de la API (`/api/v1/access/...`, llave `ppk_`, `client_event_id`, `confidence`, `plate_box`, `plate_crop`, sin `timestamp` vacío). Lista negra en Hikvision y Axis; `whitelist_supported`/`blocklist_supported` en el latido. Receptores nativos Dahua ITSAPI y Axis LPV; Hikvision identifica escena y recorte por nombre y recibe 200 vacío. Filtro por IP de origen (403), receiver encendido de fábrica, firewall desde el instalador, binario de la misma release. Reintentos sin descarte por red/5xx, back-off tope 5 min. Fotos > 4 MB recomprimidas. Cola 0600/0700. `log.file` con rotación. Build linux-arm64. |
| v1.3.0 | Plug-and-play multi-vendor, `digestClient` compartido y auto-configuración del adapter. |
| v0.2.0 | Receiver `:8787` + cola en disco + replay worker. |
| v0.1.0 | Whitelist sync + heartbeat. Adapter Hikvision. Self-install como Windows Service. |

**Compatibilidad**: una config sin `cloud.device_token` sigue funcionando (el
cloud cae al primer dispositivo). Una sin sección `receiver:` ahora **sí**
recibe fotos: para apagarlo hay que ponerlo explícito. Las llaves `pa_…`
siguen aceptadas. Los campos nuevos del latido y del evento son opcionales
del lado del cloud.

---

## Licencia

MIT.
