package auth

import (
	"os"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestValidateJWT(t *testing.T) {
	secret := "test-secret-32-chars-long!!!"
	os.Setenv("JWT_SECRET", secret)
	defer os.Unsetenv("JWT_SECRET")

	validToken := jwt.NewWithClaims(jwt.SigningMethodHS256, &Claims{
		Sub:   "user-1",
		Role:  "admin",
		Clubs: []Club{{ClubID: "club-1", Role: "admin"}},
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
		},
	})
	validTokenStr, _ := validToken.SignedString([]byte(secret))

	t.Run("valid token", func(t *testing.T) {
		claims, err := ValidateJWT("Bearer " + validTokenStr)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if claims.Sub != "user-1" {
			t.Errorf("expected sub=user-1, got %s", claims.Sub)
		}
		if claims.Role != "admin" {
			t.Errorf("expected role=admin, got %s", claims.Role)
		}
		if len(claims.Clubs) != 1 || claims.Clubs[0].ClubID != "club-1" {
			t.Errorf("expected clubs=[club-1], got %v", claims.Clubs)
		}
	})

	t.Run("missing header", func(t *testing.T) {
		_, err := ValidateJWT("")
		if err == nil {
			t.Fatal("expected error for missing header")
		}
	})

	t.Run("invalid format", func(t *testing.T) {
		_, err := ValidateJWT("Basic dXNlcjpwYXNz")
		if err == nil {
			t.Fatal("expected error for invalid format")
		}
	})

	t.Run("wrong secret", func(t *testing.T) {
		badToken, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, &Claims{
			Sub: "user-1",
			RegisteredClaims: jwt.RegisteredClaims{
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
			},
		}).SignedString([]byte("wrong-secret"))
		_, err := ValidateJWT("Bearer " + badToken)
		if err == nil {
			t.Fatal("expected error for wrong secret")
		}
	})

	t.Run("missing JWT_SECRET", func(t *testing.T) {
		os.Unsetenv("JWT_SECRET")
		defer os.Setenv("JWT_SECRET", secret)
		_, err := ValidateJWT("Bearer " + validTokenStr)
		if err == nil {
			t.Fatal("expected error when JWT_SECRET is missing")
		}
	})

	t.Run("expired token", func(t *testing.T) {
		expiredToken := jwt.NewWithClaims(jwt.SigningMethodHS256, &Claims{
			Sub: "user-1",
			RegisteredClaims: jwt.RegisteredClaims{
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(-1 * time.Hour)),
			},
		})
		expiredTokenStr, _ := expiredToken.SignedString([]byte(secret))
		_, err := ValidateJWT("Bearer " + expiredTokenStr)
		if err == nil {
			t.Fatal("expected error for expired token")
		}
	})
}

func TestClaims_IsClubManager(t *testing.T) {
	claims := &Claims{
		Clubs: []Club{
			{ClubID: "club-1", Role: "admin"},
			{ClubID: "club-2", Role: "leader"},
			{ClubID: "club-3", Role: "rider"},
		},
	}

	tests := []struct {
		name   string
		clubID string
		want   bool
	}{
		{"admin club", "club-1", true},
		{"leader club", "club-2", true},
		{"rider club", "club-3", false},
		{"unknown club", "club-4", false},
		{"empty club", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := claims.IsClubManager(tt.clubID); got != tt.want {
				t.Errorf("IsClubManager(%q) = %v, want %v", tt.clubID, got, tt.want)
			}
		})
	}
}
