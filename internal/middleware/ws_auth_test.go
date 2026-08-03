package middleware

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	goredis "github.com/redis/go-redis/v9"
)

// fakeExistsChecker implementa existsChecker con resultados configurables.
type fakeExistsChecker struct {
	count int64
	err   error
}

func (f *fakeExistsChecker) Exists(_ context.Context, _ ...string) *goredis.IntCmd {
	cmd := goredis.NewIntCmd(context.Background(), "EXISTS")
	cmd.SetVal(f.count)
	if f.err != nil {
		cmd.SetErr(f.err)
	}
	return cmd
}

func TestCheckBlacklist(t *testing.T) {
	t.Run("nil checker pasa", func(t *testing.T) {
		if err := checkBlacklist(nil, "token"); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})

	t.Run("token vacío pasa", func(t *testing.T) {
		if err := checkBlacklist(&fakeExistsChecker{count: 1}, ""); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})

	t.Run("token blacklisted se rechaza", func(t *testing.T) {
		if err := checkBlacklist(&fakeExistsChecker{count: 1}, "token"); !errors.Is(err, errBlacklisted) {
			t.Fatalf("expected errBlacklisted, got %v", err)
		}
	})

	t.Run("token no blacklisted pasa", func(t *testing.T) {
		if err := checkBlacklist(&fakeExistsChecker{count: 0}, "token"); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})

	t.Run("error de Redis se rechaza (fail-closed)", func(t *testing.T) {
		if err := checkBlacklist(&fakeExistsChecker{err: errors.New("redis down")}, "token"); err == nil {
			t.Fatal("expected error when Redis fails")
		}
	})
}

func TestExtractToken(t *testing.T) {
	newApp := func() *fiber.App {
		app := fiber.New()
		app.Get("/", func(c fiber.Ctx) error {
			return c.SendString(extractToken(c))
		})
		return app
	}

	t.Run("header Authorization", func(t *testing.T) {
		app := newApp()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer abc.def")
		resp, _ := app.Test(req)
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), "abc.def") {
			t.Fatalf("expected token from Authorization header, got %s", body)
		}
	})

	t.Run("query param token", func(t *testing.T) {
		app := newApp()
		req := httptest.NewRequest(http.MethodGet, "/?token=query-token", nil)
		resp, _ := app.Test(req)
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), "query-token") {
			t.Fatalf("expected token from query, got %s", body)
		}
	})

	t.Run("subprotocolo bearer", func(t *testing.T) {
		app := newApp()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Sec-WebSocket-Protocol", "bearer, proto-token")
		resp, _ := app.Test(req)
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), "proto-token") {
			t.Fatalf("expected token from subprotocol, got %s", body)
		}
	})

	t.Run("sin token devuelve vacío", func(t *testing.T) {
		app := newApp()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		resp, _ := app.Test(req)
		body, _ := io.ReadAll(resp.Body)
		if strings.Contains(string(body), "Bearer") {
			t.Fatalf("expected empty token, got %s", body)
		}
	})
}

func TestExtractBearer(t *testing.T) {
	tests := []struct {
		header string
		want   string
	}{
		{"Bearer abc.def", "abc.def"},
		{"bearer abc.def", "abc.def"},
		{"Basic abc", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := extractBearer(tt.header); got != tt.want {
			t.Errorf("extractBearer(%q) = %q, want %q", tt.header, got, tt.want)
		}
	}
}
