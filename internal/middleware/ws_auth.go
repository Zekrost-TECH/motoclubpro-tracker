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

		authHeader := extractToken(c)
		claims, err := auth.ValidateJWT(authHeader)
		if err != nil {
			return fiber.ErrUnauthorized
		}

		// Revocación de tokens: el backend agrega blacklist:{token} en logout.
		if rawToken := extractBearer(authHeader); rawToken != "" && redis.Client != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			exists, err := redis.Client.Exists(ctx, "blacklist:"+rawToken).Result()
			if err != nil {
				// Fail-closed: si Redis no responde no podemos confirmar que
				// el token siga válido → rechazar en lugar de dejar pasar.
				return fiber.ErrUnauthorized
			}
			if exists > 0 {
				return fiber.ErrUnauthorized
			}
		}

		c.Locals("userID", claims.Sub)
		c.Locals("role", claims.Role)
		c.Locals("claims", claims)

		return c.Next()
	}
}

// extractToken intenta obtener el token de múltiples fuentes, ya que el cliente
// puede enviarlo de distintas formas según la plataforma:
//   - Header Authorization: Bearer <token>
//   - Query param ?token=<token>
//   - Subprotocolo WebSocket: Sec-WebSocket-Protocol: bearer, <token>
func extractToken(c fiber.Ctx) string {
	if header := c.Get("Authorization"); header != "" {
		return header
	}

	if token := c.Query("token"); token != "" {
		return "Bearer " + token
	}

	if proto := c.Get("Sec-WebSocket-Protocol"); proto != "" {
		// El cliente envía ["bearer", token] y el header resultante es
		// "bearer, <token>" o "bearer,<token>".
		parts := strings.Split(proto, ",")
		for i, p := range parts {
			p = strings.TrimSpace(p)
			if strings.EqualFold(p, "bearer") && i+1 < len(parts) {
				return "Bearer " + strings.TrimSpace(parts[i+1])
			}
		}
		// Si solo viene el token directamente
		return "Bearer " + strings.TrimSpace(proto)
	}

	return ""
}

func extractBearer(header string) string {
	parts := strings.Split(header, " ")
	if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
		return parts[1]
	}
	return ""
}
