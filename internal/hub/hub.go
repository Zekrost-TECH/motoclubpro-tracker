package hub

import (
	"sync"
	"time"

	"github.com/gofiber/contrib/v3/websocket"
)

// writeTimeout caps how long a single write may block, so one slow rider
// cannot stall the broadcast to the rest of the event.
const writeTimeout = 5 * time.Second

// Client wraps a WebSocket connection with its own write mutex.
// Gorilla/fasthttp connections are NOT safe for concurrent writes, so every
// write (broadcaster tick + SOS push) must go through WriteJSON.
type Client struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

// WriteJSON serializes a safe, time-bounded write to the underlying connection.
func (c *Client) WriteJSON(v interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return c.conn.WriteJSON(v)
}

// Hub manages WebSocket connections grouped by event and user.
type Hub struct {
	mu     sync.RWMutex
	events map[string]map[string]*Client
}

// GlobalHub is the singleton instance of the connections hub
var GlobalHub = &Hub{
	events: make(map[string]map[string]*Client),
}

// Register connects a user to a specific event and returns its Client wrapper.
func (h *Hub) Register(eventID, userID string, conn *websocket.Conn) *Client {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.events[eventID] == nil {
		h.events[eventID] = make(map[string]*Client)
	}
	client := &Client{conn: conn}
	h.events[eventID][userID] = client
	return client
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

// GetEventConnections retrieves all active clients for a given event
func (h *Hub) GetEventConnections(eventID string) []*Client {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if eventMap, ok := h.events[eventID]; ok {
		conns := make([]*Client, 0, len(eventMap))
		for _, client := range eventMap {
			conns = append(conns, client)
		}
		return conns
	}
	return nil
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
