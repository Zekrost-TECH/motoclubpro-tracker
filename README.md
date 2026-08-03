# BikerOS Tracker

Servicio de tracking en tiempo real para BikerOS. Gestiona WebSockets para transmision de posiciones GPS durante rodadas, broadcast periodico a todos los riders conectados, y reenvio de alertas SOS via Redis pub/sub.

## Stack Tecnologico

| Tecnologia | Version | Proposito |
|---|---|---|
| Go | 1.26.1 | Lenguaje principal |
| Fiber (v3) | ^3.1.0 | Web framework + WebSocket support |
| WebSocket | contrib/v3/websocket | Conexiones bidireccionales en tiempo real |
| Redis | go-redis/v9 | Cache de posiciones, pub/sub SOS, autorizacion |
| JWT | golang-jwt/jwt/v5 | Validacion de tokens de autenticacion |

## Arquitectura

- **WebSocket por Evento:** Cada rider se conecta a `/ws/events/:eventId`.
- **Autorizacion:** JWT via middleware `WsAuth`. Solo miembros del evento (RSVP) o admins/lideres del club pueden acceder.
- **Aislamiento Multi-tenant:** El tracker nunca consulta PostgreSQL. La API publica datos de autorizacion en Redis (`event:{id}:members`, `event:{id}:club`).
- **TTL por Rider:** Cada posicion se almacena con `SET ... EX 90` (configurable; default del backend NestJS). Si un rider desconecta, su key expira automaticamente.
- **Broadcast:** Goroutine `startBroadcaster` envia posiciones activas cada 2s (configurable) usando `SCAN + MGET` para eficiencia multi-club.
- **SOS Listener:** Goroutine `startSOSListener` escucha canal Redis `sos:event:*` y reenvia a riders conectados.
- **Graceful Shutdown:** Cierra WebSockets, pub/sub y Redis limpiamente ante SIGINT/SIGTERM.

## Estructura del Proyecto

```
cmd/tracker/
|-- main.go              # Entry point, HTTP server, WS handler, broadcaster

internal/
|-- auth/
|   |-- jwt.go           # Validacion JWT, extraccion de claims
|-- hub/
|   |-- hub.go           # Registro de conexiones WS por evento
|-- middleware/
|   |-- ws_auth.go       # Middleware de autenticacion para WebSocket
|-- redis/
|   |-- redis.go         # Cliente Redis singleton
```

## Flujo de Datos

```
[App Movil] ---(WebSocket)--> [Tracker] ---(SET EX 30)--> [Redis]
                                    |
                                    |--(MGET every 2s)--> [Riders]
                                    |
[API SOS] ---(PUBLISH)--> [Redis] ---> [Tracker] ---(WS)--> [Riders]
```

## Mensajes WebSocket

### Cliente -> Tracker

```json
{
  "type": "position",
  "payload": {
    "lat": 4.7110,
    "lng": -74.0721,
    "speed": 60.5,
    "heading": 180,
    "timestamp": 1718900000000,
    "name": "Juan Perez"
  }
}
```

### Tracker -> Cliente (Broadcast)

```json
{
  "type": "riders",
  "payload": [
    { "lat": 4.7110, "lng": -74.0721, "speed": 60.5, "heading": 180, "userId": "uuid", "name": "Juan Perez", "role": "piloto" },
    ...
  ]
}
```

### SOS Broadcast

```json
{
  "type": "sos",
  "payload": { "userId": "uuid", "type": "pinchazo", "lat": 4.7110, "lng": -74.0721 }
}
```

## Requisitos

- Go 1.26+
- Redis 7+
- Backend API corriendo (para publicar datos de autorizacion en Redis)

## Instalacion

```bash
cd biker-os-tracker

# Dependencias
go mod download

# Variables de entorno (crear .env)
# PORT=8081
# REDIS_URL=redis://localhost:6379
# JWT_SECRET=mismo_secret_que_la_api

# Ejecutar
go run ./cmd/tracker
```

## Scripts

```bash
go run ./cmd/tracker                    # Desarrollo
go build -o tracker ./cmd/tracker       # Compilar binario
./tracker                               # Ejecutar binario
./tracker healthcheck                   # Healthcheck (para Docker)
```

## Variables de Entorno

| Variable | Default | Descripcion |
|---|---|---|
| `PORT` / `WS_PORT` | `8081` | Puerto del servidor |
| `REDIS_URL` | `redis://localhost:6379` | Conexion Redis |
| `JWT_SECRET` | - | **Requerido.** Secret para validar tokens |
| `POSITION_TTL_SEC` | `90` | TTL de cada posicion en Redis |
| `BROADCAST_INTERVAL_SEC` | `2` | Intervalo de broadcast en segundos |

## Docker

```bash
# Build (imagen ~10MB scratch)
docker build -f Dockerfile.dev -t biker-os-tracker:latest .

# Correr
docker run -d -p 8081:8081 --env-file .env biker-os-tracker:latest
```

## Systemd (VPS)

```ini
# /etc/systemd/system/biker-os-tracker.service
[Unit]
Description=BikerOS Tracker
After=network.target

[Service]
Type=simple
WorkingDirectory=/opt/tracker
ExecStart=/opt/tracker/tracker
Restart=on-failure
Environment="PORT=8081"
Environment="REDIS_URL=redis://localhost:6379"
Environment="JWT_SECRET=tu_secret"

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable biker-os-tracker
sudo systemctl start biker-os-tracker
```

## Documentacion Relacionada

- [Guia de Desarrollo Local](../../docs/LOCAL-DEVELOPMENT-GUIDE.md)
- [Guia de Despliegue a Produccion](../../docs/PRODUCTION-DEPLOYMENT-GUIDE.md)

## Licencia

UNLICENSED — Software comercial privado.
