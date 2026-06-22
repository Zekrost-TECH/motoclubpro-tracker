package hub

import (
	"testing"

	"github.com/gofiber/contrib/v3/websocket"
)

// newTestHub creates a fresh Hub instance for isolation between tests.
func newTestHub() *Hub {
	return &Hub{events: make(map[string]map[string]*Client)}
}

func TestHub_RegisterUnregister(t *testing.T) {
	h := newTestHub()

	// Use nil connection; we only test the data structure, not actual I/O.
	var conn *websocket.Conn

	h.Register("event-1", "user-1", conn)
	if got := h.GetActiveEvents(); len(got) != 1 || got[0] != "event-1" {
		t.Fatalf("expected active events=[event-1], got %v", got)
	}

	clients := h.GetEventConnections("event-1")
	if len(clients) != 1 {
		t.Fatalf("expected 1 client for event-1, got %d", len(clients))
	}

	h.Unregister("event-1", "user-1")
	if got := h.GetActiveEvents(); len(got) != 0 {
		t.Fatalf("expected 0 active events after unregister, got %v", got)
	}
}

func TestHub_MultipleUsersSameEvent(t *testing.T) {
	h := newTestHub()
	var conn *websocket.Conn

	h.Register("event-1", "user-1", conn)
	h.Register("event-1", "user-2", conn)

	clients := h.GetEventConnections("event-1")
	if len(clients) != 2 {
		t.Fatalf("expected 2 clients, got %d", len(clients))
	}

	h.Unregister("event-1", "user-1")
	clients = h.GetEventConnections("event-1")
	if len(clients) != 1 {
		t.Fatalf("expected 1 client after unregister user-1, got %d", len(clients))
	}

	h.Unregister("event-1", "user-2")
	if got := h.GetActiveEvents(); len(got) != 0 {
		t.Fatalf("expected 0 active events after all unregister, got %v", got)
	}
}

func TestHub_MultipleEvents(t *testing.T) {
	h := newTestHub()
	var conn *websocket.Conn

	h.Register("event-1", "user-1", conn)
	h.Register("event-2", "user-1", conn)
	h.Register("event-2", "user-2", conn)

	if got := h.GetActiveEvents(); len(got) != 2 {
		t.Fatalf("expected 2 active events, got %d (%v)", len(got), got)
	}

	if clients := h.GetEventConnections("event-1"); len(clients) != 1 {
		t.Fatalf("expected 1 client for event-1, got %d", len(clients))
	}
	if clients := h.GetEventConnections("event-2"); len(clients) != 2 {
		t.Fatalf("expected 2 clients for event-2, got %d", len(clients))
	}
}

func TestHub_GetEventConnections_UnknownEvent(t *testing.T) {
	h := newTestHub()
	if clients := h.GetEventConnections("nonexistent"); clients != nil {
		t.Fatalf("expected nil for unknown event, got %v", clients)
	}
}
