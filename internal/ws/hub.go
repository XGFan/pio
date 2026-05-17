// Package ws is the broadcast hub for /api/v1/events. Every accepted
// WebSocket subscriber receives every event until it disconnects.
// Events are JSON objects; the wire format is intentionally loose so the
// SwiftUI client can do schema-on-read.
package ws

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Event is one broadcast payload.
type Event struct {
	Type string                 `json:"type"`
	Data map[string]any         `json:"data,omitempty"`
	At   time.Time              `json:"at"`
}

// Hub fan-outs events to all currently-connected WebSocket clients.
type Hub struct {
	mu      sync.Mutex
	clients map[*websocket.Conn]chan Event
	log     *slog.Logger
	upgr    websocket.Upgrader
}

// NewHub wires an empty hub. The Upgrader accepts any Origin because the
// daemon is loopback-only by default.
func NewHub(log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{
		clients: map[*websocket.Conn]chan Event{},
		log:     log,
		upgr:    websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }},
	}
}

// Broadcast queues event for every connected client. Slow clients have a
// 64-event ring; once full, the client is dropped.
func (h *Hub) Broadcast(eventType string, data map[string]any) {
	ev := Event{Type: eventType, Data: data, At: time.Now().UTC()}
	h.mu.Lock()
	defer h.mu.Unlock()
	for conn, ch := range h.clients {
		select {
		case ch <- ev:
		default:
			// Slow client — drop it; the WS goroutine will see channel-close and exit.
			close(ch)
			delete(h.clients, conn)
		}
	}
}

// ServeHTTP upgrades the request to a WebSocket and pumps events.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := h.upgr.Upgrade(w, r, nil)
	if err != nil {
		h.log.Warn("ws upgrade failed", "err", err)
		return
	}
	ch := make(chan Event, 64)
	h.mu.Lock()
	h.clients[conn] = ch
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		if existing, ok := h.clients[conn]; ok {
			delete(h.clients, conn)
			// channel may already be closed by Broadcast's slow-client path
			select {
			case <-existing:
			default:
				close(existing)
			}
		}
		h.mu.Unlock()
		_ = conn.Close()
	}()

	// Reader goroutine: drain pings/control frames; exit on client close.
	go func() {
		for {
			if _, _, err := conn.NextReader(); err != nil {
				return
			}
		}
	}()

	for ev := range ch {
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := conn.WriteJSON(ev); err != nil {
			return
		}
	}
}

// ServeHTTPFunc is the http.HandlerFunc-shaped adapter.
func (h *Hub) ServeHTTPFunc() http.HandlerFunc { return h.ServeHTTP }

// EncodeJSON returns the event marshaled to bytes; useful in tests.
func EncodeJSON(eventType string, data map[string]any) []byte {
	b, _ := json.Marshal(Event{Type: eventType, Data: data, At: time.Now().UTC()})
	return b
}

// Close shuts down all subscribers. Future Broadcast calls become no-ops.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for conn, ch := range h.clients {
		select {
		case <-ch:
		default:
			close(ch)
		}
		_ = conn.Close()
		delete(h.clients, conn)
	}
}
