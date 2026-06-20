package hub

import (
	"sync"

	"github.com/gofiber/contrib/v3/websocket"
)

// Hub manages WebSocket connections by event and user
type Hub struct {
	mu     sync.RWMutex
	events map[string]map[string]*websocket.Conn
}

// GlobalHub is the singleton instance of the connections hub
var GlobalHub = &Hub{
	events: make(map[string]map[string]*websocket.Conn),
}

// Register connects a user to a specific event
func (h *Hub) Register(eventID, userID string, conn *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.events[eventID] == nil {
		h.events[eventID] = make(map[string]*websocket.Conn)
	}
	h.events[eventID][userID] = conn
}

// Unregister disconnects a user from a specific event
func (h *Hub) Unregister(eventID, userID string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if eventMap, ok := h.events[eventID]; ok {
		delete(eventMap, userID)
		if len(eventMap) == 0 {
			delete(h.events, eventID)
		}
	}
}

// GetEventConnections retrieves all active connections for a given event
func (h *Hub) GetEventConnections(eventID string) []*websocket.Conn {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var conns []*websocket.Conn
	if eventMap, ok := h.events[eventID]; ok {
		for _, conn := range eventMap {
			conns = append(conns, conn)
		}
	}
	return conns
}

// GetActiveEvents returns a list of active event IDs
func (h *Hub) GetActiveEvents() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()

	events := make([]string, 0, len(h.events))
	for eventID := range h.events {
		events = append(events, eventID)
	}
	return events
}
