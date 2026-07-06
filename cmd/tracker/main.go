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
}

func positionTTL() time.Duration {
	ttl, _ := strconv.Atoi(os.Getenv("POSITION_TTL_SEC"))
	if ttl == 0 {
		ttl = 30
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
		AppName: "Ironbikers Tracker v1.0",
	})

	app.Use(fiberLogger.New())
	app.Use(fiberRecover.New())

	app.Get("/health", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok", "service": "tracker"})
	})

	app.Use("/ws", middleware.WsAuth())
	app.Get("/ws/events/:eventId", websocket.New(handleRider, websocket.Config{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
	}))

	go startBroadcaster()

	sosCtx, sosCancel := context.WithCancel(context.Background())
	go startSOSListener(sosCtx)

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

	// Detener el SOS listener primero para que cierre el pubsub antes de matar Redis.
	sosCancel()

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
		hub.GlobalHub.Unregister(eventID, userID)
		// Eliminar posición inmediatamente al desconectar (sin esperar el TTL)
		redis.Client.Del(context.Background(), trackKey(eventID, userID))
	}()

	// Detectar conexiones zombie: si no hay mensaje en 60s, ReadMessage falla.
	_ = c.SetReadDeadline(time.Now().Add(wsReadTimeout))

	ttl := positionTTL()

	// name se acumula entre mensajes: el cliente lo envía en el primer update
	// y puede actualizarlo si cambia (por ejemplo tras editar el perfil).
	// Si un mensaje llega sin name usamos el último que recibimos.
	var lastName string

	for {
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

		// Keepalive ping/pong (a través del Client para no escribir concurrentemente)
		raw := string(msgBytes)
		if strings.Contains(raw, `"type":"ping"`) || strings.Contains(raw, `"type": "ping"`) {
			_ = client.WriteJSON(fiber.Map{"type": "pong"})
			continue
		}

		var msg PositionMsg
		if err := json.Unmarshal(msgBytes, &msg); err != nil {
			continue
		}

		if msg.Type == "position" {
			status := msg.Payload
			// Sobreescribir con datos del JWT (no confiamos en lo que el cliente envíe)
			status.UserID = userID
			status.Role = role

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
			redis.Client.Set(context.Background(), trackKey(eventID, userID), string(valBytes), ttl)
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
		events := hub.GlobalHub.GetActiveEvents()

		for _, eventID := range events {
			ctx := context.Background()

			// ── SCAN + MGET ──────────────────────────────────────────────────
			// SCAN track:{eventId}:* → solo devuelve keys cuyo TTL no expiró.
			// Recolectamos todas las keys y hacemos UN solo MGET por evento, en
			// lugar de N round-trips GET (cuello de botella bajo carga multi-club).
			var keys []string
			var cursor uint64

			for {
				batch, nextCursor, err := redis.Client.Scan(ctx, cursor, trackPattern(eventID), 100).Result()
				if err != nil {
					break
				}
				keys = append(keys, batch...)
				cursor = nextCursor
				if cursor == 0 {
					break
				}
			}

			if len(keys) == 0 {
				continue
			}

			vals, err := redis.Client.MGet(ctx, keys...).Result()
			if err != nil {
				continue
			}

			riders := make([]RiderStatus, 0, len(vals))
			for _, v := range vals {
				str, ok := v.(string)
				if !ok {
					continue
				}
				var status RiderStatus
				if json.Unmarshal([]byte(str), &status) == nil {
					riders = append(riders, status)
				}
			}

			if len(riders) == 0 {
				continue
			}

			broadcastMsg := map[string]interface{}{
				"type":    "riders",
				"payload": riders,
			}

			for _, client := range hub.GlobalHub.GetEventConnections(eventID) {
				_ = client.WriteJSON(broadcastMsg)
			}
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
			if json.Unmarshal([]byte(msg.Payload), &broadcastMsg) != nil {
				continue
			}

			for _, client := range hub.GlobalHub.GetEventConnections(eventID) {
				_ = client.WriteJSON(broadcastMsg)
			}
		}
	}
}
