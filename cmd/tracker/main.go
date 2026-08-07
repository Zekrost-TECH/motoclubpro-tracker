package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/DeijoseDevelop/biker-os-tracker/internal/auth"
	"github.com/DeijoseDevelop/biker-os-tracker/internal/hub"
	"github.com/DeijoseDevelop/biker-os-tracker/internal/middleware"
	"github.com/DeijoseDevelop/biker-os-tracker/internal/redis"

	"github.com/gofiber/contrib/v3/websocket"
	"github.com/gofiber/fiber/v3"
	fiberLogger "github.com/gofiber/fiber/v3/middleware/logger"
	fiberLimiter "github.com/gofiber/fiber/v3/middleware/limiter"
	fiberRecover "github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/joho/godotenv"
)

// PositionMsg — mensaje entrante del cliente
type PositionMsg struct {
	Type    string      `json:"type"`
	Payload RiderStatus `json:"payload"`
}

// RiderStatus — datos de posición + identidad del rider.
// name llega del cliente en el payload (el JWT solo tiene sub y role).
type RiderStatus struct {
	Lat       float64 `json:"lat"`
	Lng       float64 `json:"lng"`
	Speed     float64 `json:"speed"`
	Heading   float64 `json:"heading"`
	Timestamp int64   `json:"timestamp"`
	Role      string  `json:"role,omitempty"`
	UserID    string  `json:"userId,omitempty"`
	Name      string  `json:"name,omitempty"`
	Battery   int     `json:"battery"` // 0-100; 0 es un valor válido (batería agotada)
	// IsCharging indica si el dispositivo del rider está cargando.
	IsCharging bool `json:"isCharging,omitempty"`
}

func positionTTL() time.Duration {
	ttl, _ := strconv.Atoi(os.Getenv("POSITION_TTL_SEC"))
	if ttl == 0 {
		// 90s por defecto: en carretera la señal se cae con frecuencia y un
		// TTL de 30s hacía desaparecer riders que seguían rodando. Debe
		// coincidir con el default del backend NestJS (tracker.service.ts).
		ttl = 90
	}
	return time.Duration(ttl) * time.Second
}

func broadcastInterval() time.Duration {
	interval, _ := strconv.Atoi(os.Getenv("BROADCAST_INTERVAL_SEC"))
	if interval == 0 {
		interval = 2
	}
	return time.Duration(interval) * time.Second
}

// uuidRegex matches RFC 4122 UUIDs (with or without hyphens).
var uuidRegex = regexp.MustCompile(`^[0-9a-fA-F]{8}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{12}$`)

func isValidEventID(id string) bool { return uuidRegex.MatchString(id) }

// trackKey: key individual por rider → TTL independiente por persona
func trackKey(eventID, userID string) string {
	return "track:" + eventID + ":" + userID
}

// trackPattern: patrón SCAN para obtener todos los riders de un evento
func trackPattern(eventID string) string {
	return "track:" + eventID + ":*"
}

// Claves de autorización mantenidas por el backend (ver contrato Redis).
func eventMembersKey(eventID string) string { return "event:" + eventID + ":members" }
func eventClubKey(eventID string) string    { return "event:" + eventID + ":club" }

// eventRolesKey: HASH userId → ride_role (rol canónico del radar).
// Lo publica el backend (syncTrackerAuth, RSVP, updateAttendeeRole). Si existe,
// tiene prioridad sobre el rol que envía el cliente (ROD-07).
func eventRolesKey(eventID string) string { return "event:" + eventID + ":roles" }

// validPosition: sanity check de coordenadas y velocidad antes de guardar y
// broadcastear (ROD-08). Descarta GPS corrupto/manipulado.
func validPosition(s RiderStatus) bool {
	if s.Lat < -90 || s.Lat > 90 || s.Lng < -180 || s.Lng > 180 {
		return false
	}
	if s.Speed < 0 || s.Speed > 500 {
		return false
	}
	return true
}

// resolveRiderRole: prioridad del rol canónico almacenado en Redis (event:{id}:roles);
// si no existe, el rol que envía el cliente; si tampoco, el rol del JWT.
func resolveRiderRole(storedRole, clientRole, jwtRole string) string {
	storedRole = strings.TrimSpace(storedRole)
	if storedRole != "" {
		return storedRole
	}
	if clientRole != "" {
		return clientRole
	}
	return jwtRole
}

// authorize decide si un rider puede ver el tracking de un evento.
// Aislamiento multi-tenant: solo miembros del evento (RSVP) o admins/líderes
// del club dueño del evento. El tracker NUNCA consulta Postgres: la verdad
// de autorización la publica el backend en Redis.
// Las funciones isMemberFn y getClubFn son inyectadas para facilitar tests.
func authorize(
	isMemberFn func(ctx context.Context, key string, member interface{}) (bool, error),
	getClubFn func(ctx context.Context, key string) (string, error),
	eventID, userID string, claims *auth.Claims,
) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if isMember, err := isMemberFn(ctx, eventMembersKey(eventID), userID); err == nil && isMember {
		return true
	}

	if clubID, err := getClubFn(ctx, eventClubKey(eventID)); err == nil && claims.IsClubManager(clubID) {
		return true
	}

	return false
}

// runHealthcheck permite usar el propio binario como healthcheck en imágenes
// scratch (sin wget/curl): `tracker healthcheck`.
func runHealthcheck() {
	port := os.Getenv("WS_PORT")
	if port == "" {
		port = "8081"
	}
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://localhost:" + port + "/health")
	if err != nil || resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
	os.Exit(0)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		runHealthcheck()
	}

	if err := godotenv.Load(); err != nil {
		log.Println("No .env file, usando variables de entorno del sistema")
	}

	if os.Getenv("JWT_SECRET") == "" {
		log.Fatal("JWT_SECRET no configurado: el tracker no puede validar tokens")
	}

	redis.InitRedis()

	app := fiber.New(fiber.Config{
		AppName: "BikerOS Tracker",
	})

	app.Use(fiberLogger.New())
	app.Use(fiberRecover.New())

	// Rate limit de handshakes WebSocket por IP (máx 60 upgrades/minuto):
	// evita que un cliente abra cientos de conexiones WS.
	app.Use("/ws", fiberLimiter.New(fiberLimiter.Config{
		Max:        60,
		Expiration: time.Minute,
		LimitReached: func(c fiber.Ctx) error {
			return fiber.ErrTooManyRequests
		},
	}))

	app.Get("/health", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok", "service": "tracker"})
	})

	app.Use("/ws", middleware.WsAuth())
	app.Get("/ws/events/:eventId", websocket.New(handleRider, websocket.Config{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		Subprotocols:    []string{"bearer"},
	}))

	go startBroadcaster()

	sosCtx, sosCancel := context.WithCancel(context.Background())
	go startSOSListener(sosCtx)

	// ROD-02/09: el backend publica cambios de estado en event:{id}:status;
	// el tracker cierra los WS del evento y purga sus posiciones al terminar.
	statusCtx, statusCancel := context.WithCancel(context.Background())
	go startEventStatusListener(statusCtx)

	port := os.Getenv("WS_PORT")
	if port == "" {
		port = "8081"
	}

	// Arrancar el servidor en una goroutine para poder hacer graceful shutdown.
	go func() {
		log.Printf("Tracker escuchando en :%s\n", port)
		if err := app.Listen(":" + port); err != nil {
			log.Fatalf("Error iniciando tracker: %v", err)
		}
	}()

	// Graceful shutdown: cerrar conexiones WS y Redis limpiamente en deploys.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Señal de apagado recibida, cerrando tracker...")

	// Detener los listeners primero para que cierren los pubsub antes de matar Redis.
	sosCancel()
	statusCancel()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := app.ShutdownWithContext(ctx); err != nil {
		log.Printf("Error en shutdown del servidor: %v", err)
	}
	if err := redis.Client.Close(); err != nil {
		log.Printf("Error cerrando Redis: %v", err)
	}
	log.Println("Tracker apagado correctamente")
}

const wsReadTimeout = 60 * time.Second

func handleRider(c *websocket.Conn) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Recovered in handleRider: %v", r)
		}
	}()

	eventID := c.Params("eventId")
	if !isValidEventID(eventID) {
		_ = c.WriteJSON(fiber.Map{"type": "error", "message": "invalid event id"})
		_ = c.Close()
		return
	}

	userID, _ := c.Locals("userID").(string)
	role, _ := c.Locals("role").(string)
	claims, _ := c.Locals("claims").(*auth.Claims)

	// Aislamiento multi-tenant: rechazar si el rider no pertenece al evento/club.
	isMember := func(ctx context.Context, key string, member interface{}) (bool, error) {
		return redis.Client.SIsMember(ctx, key, member).Result()
	}
	getClub := func(ctx context.Context, key string) (string, error) {
		return redis.Client.Get(ctx, key).Result()
	}
	if userID == "" || claims == nil || !authorize(isMember, getClub, eventID, userID, claims) {
		_ = c.WriteJSON(fiber.Map{"type": "error", "message": "unauthorized for this event"})
		_ = c.Close()
		return
	}

	client := hub.GlobalHub.Register(eventID, userID, c)
	defer func() {
		hub.GlobalHub.UnregisterClient(eventID, client)
		// NO borrar track:{event}:{user} al desconectar: el rider puede seguir
		// enviando posiciones por HTTP fallback (app en background, pantalla
		// apagada). Si realmente dejó de rodar, el TTL expira la key solo.
	}()

	// Detectar conexiones zombie: si no hay mensaje en 60s, ReadMessage falla.
	_ = c.SetReadDeadline(time.Now().Add(wsReadTimeout))

	ttl := positionTTL()

	// name se acumula entre mensajes: el cliente lo envía en el primer update
	// y puede actualizarlo si cambia (por ejemplo tras editar el perfil).
	// Si un mensaje llega sin name usamos el último que recibimos.
	var lastName string

	// Rate limit por conexión: los clientes legítimos envían 1 posición cada
	// ~3s (throttle del app). Se limita a 100 mensajes por ventana de 30s
	// para evitar abuso/amplificación.
	const (
		rateWindow = 30 * time.Second
		rateMax    = 100
	)
	rateCount := 0
	rateWindowAt := time.Now()

	for {
		// Si el cliente fue cerrado desde otra goroutine (purgeEvent por fin de
		// rodada, reconexión), salir aunque un mensaje hubiera renovado el
		// deadline: el socket real solo se cierra cuando este handler retorna.
		if client.IsClosed() {
			break
		}

		mt, msgBytes, err := c.ReadMessage()
		if err != nil {
			break
		}

		// Renovar el deadline en cada mensaje recibido (keepalive implícito).
		_ = c.SetReadDeadline(time.Now().Add(wsReadTimeout))

		if mt != websocket.TextMessage {
			continue
		}

		// Rechazar payloads excesivamente grandes (>64KB) para evitar OOM.
		if len(msgBytes) > 64*1024 {
			_ = client.WriteJSON(fiber.Map{"type": "error", "message": "payload too large"})
			continue
		}

		var msg PositionMsg
		if err := json.Unmarshal(msgBytes, &msg); err != nil {
			log.Printf("Mensaje JSON inválido descartado (event=%s user=%s): %v", eventID, userID, err)
			continue
		}

		rateCount++
		if time.Since(rateWindowAt) > rateWindow {
			rateCount = 1
			rateWindowAt = time.Now()
		}
		if rateCount > rateMax {
			_ = client.WriteJSON(fiber.Map{"type": "error", "message": "rate limit exceeded"})
			break
		}

		// Keepalive ping/pong (a través del Client para no escribir concurrentemente)
		if msg.Type == "ping" {
			_ = client.WriteJSON(fiber.Map{"type": "pong"})
			continue
		}

		if msg.Type == "position" {
			status := msg.Payload
			// userID SIEMPRE del JWT: nadie puede suplantar a otro rider.
			status.UserID = userID

			// ROD-08: descartar coordenadas fuera de rango antes de tocar Redis.
			if !validPosition(status) {
				_ = client.WriteJSON(fiber.Map{"type": "error", "message": "invalid position"})
				continue
			}

			// ROD-07: rol canónico desde Redis (event:{id}:roles), publicado por
			// el backend en RSVP/syncTrackerAuth/updateAttendeeRole. Si existe,
			// tiene prioridad sobre el rol del cliente y del JWT.
			ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
			storedRole, _ := redis.Client.HGet(ctx2, eventRolesKey(eventID), userID).Result()
			cancel2()

			status.Role = resolveRiderRole(storedRole, msg.Payload.Role, role)

			// name viene del cliente — actualizar si viene, conservar el anterior si no
			if msg.Payload.Name != "" {
				lastName = msg.Payload.Name
			}
			status.Name = lastName

			valBytes, _ := json.Marshal(status)

			// ── Fix TTL por rider ─────────────────────────────────────────────
			// SET track:{eventId}:{userId} <json> EX 30
			// Cada rider tiene su propio key con TTL independiente.
			// Si Carlos deja de enviar posición, su key expira solo
			// sin afectar a los demás riders del mismo evento.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if err := redis.Client.Set(ctx, trackKey(eventID, userID), string(valBytes), ttl).Err(); err != nil {
				log.Printf("Error guardando posición en Redis (event=%s user=%s): %v", eventID, userID, err)
			}
			cancel()
		}
	}
}

// broadcastEventPositions envía las posiciones activas de un evento a sus
// clientes conectados (SCAN + MGET con timeout para no bloquear el ciclo).
func broadcastEventPositions(eventID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// ── SCAN + MGET ──────────────────────────────────────────────────
	// SCAN track:{eventId}:* → solo devuelve keys cuyo TTL no expiró.
	// Recolectamos todas las keys y hacemos UN solo MGET por evento, en
	// lugar de N round-trips GET (cuello de botella bajo carga multi-club).
	var keys []string
	var cursor uint64

	for {
		batch, nextCursor, err := redis.Client.Scan(ctx, cursor, trackPattern(eventID), 100).Result()
		if err != nil {
			return
		}
		keys = append(keys, batch...)
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}

	if len(keys) == 0 {
		return
	}

	vals, err := redis.Client.MGet(ctx, keys...).Result()
	if err != nil {
		return
	}

	riders := make([]RiderStatus, 0, len(vals))
	for _, v := range vals {
		str, ok := v.(string)
		if !ok {
			continue
		}
		var status RiderStatus
		if err := json.Unmarshal([]byte(str), &status); err != nil {
			log.Printf("Posición almacenada con JSON inválido (key %q): %v", v, err)
			continue
		}
		riders = append(riders, status)
	}

	if len(riders) == 0 {
		return
	}

	broadcastMsg := map[string]interface{}{
		"type":    "riders",
		"payload": riders,
	}

	for _, client := range hub.GlobalHub.GetEventConnections(eventID) {
		if err := client.WriteJSON(broadcastMsg); err != nil {
			// Cliente muerto: purgarlo del hub y cerrar su conexión
			// para no seguir escribiendo sobre sockets rotos.
			hub.GlobalHub.UnregisterClient(eventID, client)
		}
	}
}

// startBroadcaster envía las posiciones activas a todos los clientes cada N segundos.
func startBroadcaster() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Recovered in startBroadcaster: %v", r)
		}
	}()

	ticker := time.NewTicker(broadcastInterval())
	defer ticker.Stop()

	for range ticker.C {
		for _, eventID := range hub.GlobalHub.GetActiveEvents() {
			broadcastEventPositions(eventID)
		}
	}
}

// parseStatusChannel extrae el eventId de un canal "event:{eventId}:status".
// Devuelve ("", false) si el canal no coincide con el contrato.
func parseStatusChannel(channel string) (eventID string, ok bool) {
	parts := strings.SplitN(channel, ":", 3)
	if len(parts) != 3 || parts[0] != "event" || parts[2] != "status" {
		return "", false
	}
	return parts[1], true
}

// purgeEvent: al terminar/cancelar una rodada, cierra los sockets de ese evento
// (notificando el cambio de estado a las apps) y purga las posiciones track:*.
func purgeEvent(eventID string, payload map[string]interface{}) {
	// 1. Notificar y cerrar cada cliente conectado al evento.
	// Close() despierta el read loop del handler (ver hub.Client.Close):
	// el socket real se cierra cuando handleRider retorna.
	for _, client := range hub.GlobalHub.GetEventConnections(eventID) {
		_ = client.WriteJSON(payload)
		client.Close()
		hub.GlobalHub.UnregisterClient(eventID, client)
	}

	// 2. Purgar las posiciones del evento (track:{eventId}:*) para que el
	//    radar muera de inmediato (no esperar el TTL).
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var keys []string
	var cursor uint64
	for {
		batch, nextCursor, err := redis.Client.Scan(ctx, cursor, trackPattern(eventID), 100).Result()
		if err != nil {
			log.Printf("Error purgando posiciones (event=%s): %v", eventID, err)
			return
		}
		keys = append(keys, batch...)
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}

	if len(keys) > 0 {
		if err := redis.Client.Del(ctx, keys...).Err(); err != nil {
			log.Printf("Error borrando posiciones (event=%s): %v", eventID, err)
		}
	}

	// 3. Quitar la autorización del evento (members/roles): si un rider intenta
	//    reconectar después de terminar la rodada, el tracker lo rechaza en
	//    lugar de dejar un socket colgado esperando broadcasts que no llegarán.
	//    El backend re-crea ambas claves cuando la rodada vuelve a en_curso
	//    (syncTrackerAuth en updateStatus).
	if err := redis.Client.Del(ctx, eventMembersKey(eventID), eventRolesKey(eventID)).Err(); err != nil {
		log.Printf("Error borrando autorización (event=%s): %v", eventID, err)
	}
}

// startEventStatusListener escucha el canal Redis event:{id}:status publicado
// por el backend cuando una rodada cambia de estado. Solo actúa cuando el
// evento deja de estar en curso: cierra los WS del evento y purga sus
// posiciones. Si vuelve a en_curso, no hace nada (los riders reconectan).
func startEventStatusListener(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Recovered in startEventStatusListener: %v", r)
		}
	}()

	pubsub := redis.Client.PSubscribe(context.Background(), "event:*:status")
	defer func() {
		_ = pubsub.Close()
	}()

	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			eventID, ok := parseStatusChannel(msg.Channel)
			if !ok {
				continue
			}

			var payload map[string]interface{}
			if err := json.Unmarshal([]byte(msg.Payload), &payload); err != nil {
				log.Printf("Payload de estado con JSON inválido en canal %q: %v", msg.Channel, err)
				continue
			}

			status, _ := payload["payload"].(map[string]interface{})["status"].(string)
			if status == "en_curso" {
				continue
			}

			log.Printf("Rodada %s ya no está en curso (status=%s) — cerrando radar", eventID, status)
			purgeEvent(eventID, payload)
		}
	}
}

// startSOSListener escucha SOLO los SOS a nivel de evento (sos:event:{id})
// publicados por NestJS y los reenvía a los riders conectados a ese evento.
// Los SOS de club/global son responsabilidad de FCM push, no del tracker.
func startSOSListener(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Recovered in startSOSListener: %v", r)
		}
	}()

	pubsub := redis.Client.PSubscribe(context.Background(), "sos:event:*")
	defer func() {
		_ = pubsub.Close()
	}()

	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			// Canal: "sos:event:{eventId}"
			parts := strings.SplitN(msg.Channel, ":", 3)
			if len(parts) != 3 {
				continue
			}
			eventID := parts[2]

			var broadcastMsg map[string]interface{}
			if err := json.Unmarshal([]byte(msg.Payload), &broadcastMsg); err != nil {
				log.Printf("Payload SOS con JSON inválido en canal %q: %v", msg.Channel, err)
				continue
			}

			for _, client := range hub.GlobalHub.GetEventConnections(eventID) {
				if err := client.WriteJSON(broadcastMsg); err != nil {
					// Cliente muerto: purgarlo del hub y cerrar su conexión
					// para no seguir escribiendo sobre sockets rotos.
					hub.GlobalHub.UnregisterClient(eventID, client)
				}
			}
		}
	}
}
