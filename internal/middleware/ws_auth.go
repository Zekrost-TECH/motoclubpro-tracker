package middleware

import (
	"context"
	"strings"
	"time"

	"github.com/DeijoseDevelop/biker-os-tracker/internal/auth"
	"github.com/DeijoseDevelop/biker-os-tracker/internal/redis"
	"github.com/gofiber/contrib/v3/websocket"
	"github.com/gofiber/fiber/v3"
)

// WsAuth middleware upgrades the HTTP connection to WebSocket after validating the JWT.
// It also rejects tokens revoked by the backend (logout) via the Redis blacklist,
// keeping session revocation consistent across both services.
func WsAuth() fiber.Handler {
	return func(c fiber.Ctx) error {
		if !websocket.IsWebSocketUpgrade(c) {
			return fiber.ErrUpgradeRequired
		}

		authHeader := c.Get("Authorization")
		claims, err := auth.ValidateJWT(authHeader)
		if err != nil {
			return fiber.ErrUnauthorized
		}

		// Revocación de tokens: el backend agrega blacklist:{token} en logout.
		if rawToken := extractBearer(authHeader); rawToken != "" && redis.Client != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if exists, _ := redis.Client.Exists(ctx, "blacklist:"+rawToken).Result(); exists > 0 {
				return fiber.ErrUnauthorized
			}
		}

		c.Locals("userID", claims.Sub)
		c.Locals("role", claims.Role)
		c.Locals("claims", claims)

		return c.Next()
	}
}

func extractBearer(header string) string {
	parts := strings.Split(header, " ")
	if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
		return parts[1]
	}
	return ""
}
