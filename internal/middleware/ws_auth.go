package middleware

import (
	"github.com/DeijoseDevelop/motoclubpro-tracker/internal/auth"
	"github.com/gofiber/contrib/v3/websocket"
	"github.com/gofiber/fiber/v3"
)

// WsAuth middleware upgrades the HTTP connection to WebSocket after validating the JWT.
func WsAuth() fiber.Handler {
	return func(c fiber.Ctx) error {
		if websocket.IsWebSocketUpgrade(c) {
			claims, err := auth.ValidateJWT(c.Get("Authorization"))
			if err != nil {
				// Fiber v3 error return
				return fiber.ErrUnauthorized
			}
			
			c.Locals("userID", claims.Sub)
			c.Locals("role", claims.Role)
			
			return c.Next()
		}
		return fiber.ErrUpgradeRequired
	}
}
