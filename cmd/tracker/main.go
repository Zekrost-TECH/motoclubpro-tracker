package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/DeijoseDevelop/motoclubpro-tracker/internal/hub"
	"github.com/DeijoseDevelop/motoclubpro-tracker/internal/middleware"
	"github.com/DeijoseDevelop/motoclubpro-tracker/internal/redis"

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

// trackKey: key individual por rider → TTL independiente por persona
func trackKey(eventID, userID string) string {
	return "track:" + eventID + ":" + userID
}

// trackPattern: patrón SCAN para obtener todos los riders de un evento
func trackPattern(eventID string) string {
	return "track:" + eventID + ":*"
}

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file, usando variables de entorno del sistema")
	}

	redis.InitRedis()

	app := fiber.New(fiber.Config{
		AppName: "IronBykers Tracker v1.0",
	})

	app.Use(fiberLogger.New())
	app.Use(fiberRecover.New())

	app.Get("/health", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok", "service": "tracker"})
	})

	app.Use("/ws", middleware.WsAuth())
	app.Get("/ws/events/:eventId", websocket.New(handleRider))

	go startBroadcaster()
	go startSOSListener()

	port := os.Getenv("WS_PORT")
	if port == "" {
		port = "8081"
	}

	log.Printf("Tracker escuchando en :%s\n", port)
	if err := app.Listen(":" + port); err != nil {
		log.Fatalf("Error iniciando tracker: %v", err)
	}
}

func handleRider(c *websocket.Conn) {
	eventID := c.Params("eventId")
	userID := c.Locals("userID").(string)
	role := c.Locals("role").(string)

	hub.GlobalHub.Register(eventID, userID, c)
	defer func() {
		hub.GlobalHub.Unregister(eventID, userID)
		// Eliminar posición inmediatamente al desconectar (sin esperar el TTL)
		redis.Client.Del(context.Background(), trackKey(eventID, userID))
	}()

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

		if mt != websocket.TextMessage {
			continue
		}

		// Keepalive ping/pong
		raw := string(msgBytes)
		if strings.Contains(raw, `"type":"ping"`) || strings.Contains(raw, `"type": "ping"`) {
			c.WriteJSON(fiber.Map{"type": "pong"})
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
	ticker := time.NewTicker(broadcastInterval())
	defer ticker.Stop()

	for range ticker.C {
		events := hub.GlobalHub.GetActiveEvents()

		for _, eventID := range events {
			ctx := context.Background()

			// ── Fix SCAN en lugar de HGETALL ─────────────────────────────────
			// SCAN track:{eventId}:* → solo devuelve keys cuyo TTL no expiró.
			// Los riders inactivos (sin señal GPS) desaparecen solos del mapa
			// cuando su key expira, sin necesidad de lógica de limpieza manual.
			var riders []RiderStatus
			var cursor uint64

			for {
				keys, nextCursor, err := redis.Client.Scan(ctx, cursor, trackPattern(eventID), 100).Result()
				if err != nil {
					break
				}

				for _, key := range keys {
					val, err := redis.Client.Get(ctx, key).Result()
					if err != nil {
						continue
					}
					var status RiderStatus
					if json.Unmarshal([]byte(val), &status) == nil {
						riders = append(riders, status)
					}
				}

				cursor = nextCursor
				if cursor == 0 {
					break
				}
			}

			if len(riders) == 0 {
				continue
			}

			broadcastMsg := map[string]interface{}{
				"type":    "riders",
				"payload": riders,
			}

			for _, conn := range hub.GlobalHub.GetEventConnections(eventID) {
				conn.WriteJSON(broadcastMsg)
			}
		}
	}
}

// startSOSListener escucha el canal Redis sos:* publicado por NestJS
// y hace broadcast inmediato a todos los riders del evento afectado.
func startSOSListener() {
	pubsub := redis.Client.PSubscribe(context.Background(), "sos:*")
	defer pubsub.Close()

	for msg := range pubsub.Channel() {
		// Canal: "sos:{eventId}"
		parts := strings.Split(msg.Channel, ":")
		if len(parts) != 2 {
			continue
		}
		eventID := parts[1]

		var sosPayload interface{}
		if json.Unmarshal([]byte(msg.Payload), &sosPayload) != nil {
			continue
		}

		broadcastMsg := map[string]interface{}{
			"type":    "sos",
			"payload": sosPayload,
		}

		for _, conn := range hub.GlobalHub.GetEventConnections(eventID) {
			conn.WriteJSON(broadcastMsg)
		}
	}
}