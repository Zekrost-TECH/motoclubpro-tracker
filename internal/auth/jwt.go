package auth

import (
	"errors"
	"os"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

// Club represents a club membership embedded in the JWT.
type Club struct {
	ClubID string `json:"club_id"`
	Role   string `json:"role"`
}

// Claims represents the expected JWT payload emitted by the NestJS backend.
type Claims struct {
	Sub   string `json:"sub"`
	Role  string `json:"role"`
	Clubs []Club `json:"clubs"`
	jwt.RegisteredClaims
}

// IsClubManager returns true if the user is admin or leader of the given club.
func (c *Claims) IsClubManager(clubID string) bool {
	if clubID == "" {
		return false
	}
	for _, club := range c.Clubs {
		if club.ClubID == clubID && (club.Role == "admin" || club.Role == "lider") {
			return true
		}
	}
	return false
}

// ValidateJWT parses the raw Authorization header and returns the claims
func ValidateJWT(authHeader string) (*Claims, error) {
	if authHeader == "" {
		return nil, errors.New("missing authorization header")
	}

	parts := strings.Split(authHeader, " ")
	if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
		return nil, errors.New("invalid authorization header format")
	}

	tokenStr := parts[1]
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		return nil, errors.New("JWT_SECRET not configured")
	}

	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		// Ensure the signing method is HMAC
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(secret), nil
	})

	if err != nil {
		return nil, err
	}

	if claims, ok := token.Claims.(*Claims); ok && token.Valid {
		return claims, nil
	}

	return nil, errors.New("invalid token")
}
