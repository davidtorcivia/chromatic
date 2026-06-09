package websocket

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"chromatic/internal/metrics"

	"github.com/gorilla/websocket"
)

const (
	// Time allowed to write a message to the peer
	writeWait = 10 * time.Second

	// Time allowed to read the next pong message from the peer
	pongWait = 60 * time.Second

	// Send pings to peer with this period (must be less than pongWait)
	pingPeriod = (pongWait * 9) / 10

	// Maximum message size allowed from peer
	maxMessageSize = 8192
)

// Hub manages all room-based WebSocket connections
type Hub struct {
	mu sync.RWMutex

	// Rooms contain clients organized by room slug
	rooms map[string]*Room

	// Set under mu when Shutdown is called; makes Broadcast a no-op so late
	// callers (e.g. async Pion callbacks) can never block or panic.
	shutdown bool

	// Shutdown channel
	done chan struct{}
}

// Room holds all clients for a specific room
type Room struct {
	Slug    string
	Clients map[string]*Client
}

// Client represents a single WebSocket connection
type Client struct {
	ID        string
	Name      string
	Role      string
	RoomSlug  string
	Color     string
	Hub       *Hub
	Conn      *websocket.Conn
	Send      chan []byte
	Done      chan struct{} // Closed when client disconnects; gates SendJSON
	closeOnce sync.Once     // Ensures Send channel is closed only once
	IsAdmin   bool

	// Media state is written by the read pump (media:toggle) and read by
	// other goroutines (e.g. room-state snapshots), so it is guarded by a mutex.
	mediaMu      sync.Mutex
	audioEnabled bool
	videoEnabled bool

	// Rate limiting for chat messages (30 per minute)
	chatRateLimiter *RateLimiter
	// Rate limiting for cursor updates (20 per second)
	cursorRateLimiter *RateLimiter
}

// AudioEnabled returns whether the client's audio is enabled.
func (c *Client) AudioEnabled() bool {
	c.mediaMu.Lock()
	defer c.mediaMu.Unlock()
	return c.audioEnabled
}

// SetAudioEnabled updates the client's audio state.
func (c *Client) SetAudioEnabled(enabled bool) {
	c.mediaMu.Lock()
	defer c.mediaMu.Unlock()
	c.audioEnabled = enabled
}

// VideoEnabled returns whether the client's video is enabled.
func (c *Client) VideoEnabled() bool {
	c.mediaMu.Lock()
	defer c.mediaMu.Unlock()
	return c.videoEnabled
}

// SetVideoEnabled updates the client's video state.
func (c *Client) SetVideoEnabled(enabled bool) {
	c.mediaMu.Lock()
	defer c.mediaMu.Unlock()
	c.videoEnabled = enabled
}

// RateLimiter tracks rate limits per client
type RateLimiter struct {
	mu             sync.Mutex
	windowStart    time.Time
	requests       int
	maxRequests    int
	windowDuration time.Duration
}

// NewRateLimiter creates a new rate limiter
func NewRateLimiter(maxRequests int, windowDuration time.Duration) *RateLimiter {
	return &RateLimiter{
		windowStart:    time.Now(),
		requests:       0,
		maxRequests:    maxRequests,
		windowDuration: windowDuration,
	}
}

// Allow checks if a request is allowed under the rate limit
func (r *RateLimiter) Allow() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()

	// Reset window if expired
	if now.Sub(r.windowStart) > r.windowDuration {
		r.windowStart = now
		r.requests = 0
	}

	// Check if under limit
	if r.requests >= r.maxRequests {
		return false
	}

	r.requests++
	return true
}

// InitChatRateLimiter initializes the chat message rate limiter
// 30 messages per minute per client
func (c *Client) InitChatRateLimiter() {
	c.chatRateLimiter = NewRateLimiter(30, time.Minute)
}

// AllowChatMessage checks if the client can send another chat message
func (c *Client) AllowChatMessage() bool {
	if c.chatRateLimiter == nil {
		// If not initialized, initialize it now (backwards compatibility)
		c.InitChatRateLimiter()
	}
	return c.chatRateLimiter.Allow()
}

// InitCursorRateLimiter initializes the cursor update rate limiter
// 20 updates per second per client
func (c *Client) InitCursorRateLimiter() {
	c.cursorRateLimiter = NewRateLimiter(20, time.Second)
}

// AllowCursor checks if the client can send another cursor update
func (c *Client) AllowCursor() bool {
	if c.cursorRateLimiter == nil {
		c.InitCursorRateLimiter()
	}
	return c.cursorRateLimiter.Allow()
}

// Message represents a WebSocket message
type Message struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// NewHub creates a new Hub
func NewHub() *Hub {
	return &Hub{
		rooms: make(map[string]*Room),
		done:  make(chan struct{}),
	}
}

// Run blocks until the hub is shut down.
// Broadcasts are fanned out synchronously in the caller's goroutine (per-client
// buffered send channels provide slow-client decoupling), so there is no
// central event loop anymore — a single loop was a head-of-line bottleneck
// where bursts of cursor messages delayed time-critical WebRTC signaling.
// Run is kept so existing `go hub.Run()` call sites remain valid.
func (h *Hub) Run() {
	<-h.done
}

func (h *Hub) registerClient(client *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	room, ok := h.rooms[client.RoomSlug]
	if !ok {
		room = &Room{
			Slug:    client.RoomSlug,
			Clients: make(map[string]*Client),
		}
		h.rooms[client.RoomSlug] = room
		// Track new active room
		metrics.Get().ActiveRooms.Add(1)
	}

	// If an old client with the same ID exists (e.g. page refresh), close it
	// so its ReadPump exits cleanly. The old client's onDisconnect handler
	// will see that it was replaced and skip cleanup.
	if old, exists := room.Clients[client.ID]; exists {
		log.Printf("Replacing existing client %s (%s) in room %s (reconnect)", old.ID, old.Name, client.RoomSlug)
		old.closeOnce.Do(func() {
			close(old.Done)
		})
		if old.Conn != nil {
			old.Conn.Close()
		}
		// Don't increment metrics — we're replacing, not adding
	} else {
		// Track WebSocket connections
		metrics.Get().ActiveWebsockets.Add(1)
	}

	room.Clients[client.ID] = client
	log.Printf("Client %s (%s) joined room %s", client.ID, client.Name, client.RoomSlug)
}

func (h *Hub) unregisterClient(client *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if room, ok := h.rooms[client.RoomSlug]; ok {
		// Only remove if the current client in the map is this exact instance.
		// On page refresh, registerClient replaces the old client with a new one;
		// when the old client's ReadPump exits and calls Unregister, we must NOT
		// remove the replacement client.
		if current, ok := room.Clients[client.ID]; ok && current == client {
			delete(room.Clients, client.ID)
			// Track WebSocket disconnection
			metrics.Get().ActiveWebsockets.Add(-1)
			log.Printf("Client %s (%s) left room %s", client.ID, client.Name, client.RoomSlug)

			// Clean up empty rooms
			if len(room.Clients) == 0 {
				delete(h.rooms, client.RoomSlug)
				// Track room closure
				metrics.Get().ActiveRooms.Add(-1)
			}
		}
		// Always close the client's Done channel regardless of whether it was still current.
		// Send is intentionally never closed — WritePump selects on Done to exit, and
		// leaving Send open avoids send-on-closed-channel panics in broadcast/SendJSON.
		client.closeOnce.Do(func() {
			close(client.Done)
		})
	}
}

// Register adds a client to the hub (synchronous so the client is visible immediately)
func (h *Hub) Register(client *Client) {
	h.registerClient(client)
}

// Unregister removes a client from the hub
func (h *Hub) Unregister(client *Client) {
	h.unregisterClient(client)
}

// Broadcast sends a message to all clients in a room.
// The fan-out happens synchronously in the caller's goroutine: per-client
// buffered Send channels decouple slow clients, and there is no shared
// channel that could block forever after Shutdown.
func (h *Hub) Broadcast(roomSlug string, message []byte, excludeID string) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if h.shutdown {
		// No-op after shutdown; async callbacks (e.g. Pion) may still fire.
		return
	}

	room, ok := h.rooms[roomSlug]
	if !ok {
		return
	}

	for id, client := range room.Clients {
		if id == excludeID {
			continue
		}

		select {
		case <-client.Done:
			// Client is already disconnecting, skip
			continue
		case client.Send <- message:
		default:
			// Client's send buffer is full — close Done so pumps exit.
			// Do NOT delete from map here: we only hold RLock, and map
			// mutation under RLock is a data race. unregisterClient will
			// clean up the map entry when ReadPump exits.
			client.closeOnce.Do(func() {
				close(client.Done)
			})
			if client.Conn != nil {
				client.Conn.Close()
			}
		}
	}
}

// BroadcastJSON sends a JSON message to all clients in a room.
// Marshal errors are logged here because most callers ignore the return value.
func (h *Hub) BroadcastJSON(roomSlug string, msgType string, payload interface{}, excludeID string) error {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		log.Printf("BroadcastJSON: failed to marshal payload for %q in room %s: %v", msgType, roomSlug, err)
		return err
	}

	msg := Message{
		Type:    msgType,
		Payload: payloadBytes,
	}

	msgBytes, err := json.Marshal(msg)
	if err != nil {
		log.Printf("BroadcastJSON: failed to marshal message %q in room %s: %v", msgType, roomSlug, err)
		return err
	}

	h.Broadcast(roomSlug, msgBytes, excludeID)
	return nil
}

// SendTo sends a message to a specific client
func (h *Hub) SendTo(roomSlug, clientID string, message []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if room, ok := h.rooms[roomSlug]; ok {
		if client, ok := room.Clients[clientID]; ok {
			select {
			case client.Send <- message:
			default:
				// Client's send buffer is full
			}
		}
	}
}

// SendToJSON sends a JSON message to a specific client
func (h *Hub) SendToJSON(roomSlug, clientID, msgType string, payload interface{}) error {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	msg := Message{
		Type:    msgType,
		Payload: payloadBytes,
	}

	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	h.SendTo(roomSlug, clientID, msgBytes)
	return nil
}

// GetRoomClients returns all clients in a room
func (h *Hub) GetRoomClients(roomSlug string) []*Client {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var clients []*Client
	if room, ok := h.rooms[roomSlug]; ok {
		for _, client := range room.Clients {
			clients = append(clients, client)
		}
	}
	return clients
}

// GetClient returns a specific client
func (h *Hub) GetClient(roomSlug, clientID string) *Client {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if room, ok := h.rooms[roomSlug]; ok {
		return room.Clients[clientID]
	}
	return nil
}

// IsCurrentClient checks if the given client is still the active client for its ID.
// Returns false if the client was replaced by a new connection (e.g. page refresh).
func (h *Hub) IsCurrentClient(client *Client) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if room, ok := h.rooms[client.RoomSlug]; ok {
		if current, ok := room.Clients[client.ID]; ok {
			return current == client
		}
	}
	return false
}

// Shutdown gracefully shuts down the hub
func (h *Hub) Shutdown() {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.shutdown {
		return
	}
	h.shutdown = true
	close(h.done)

	for _, room := range h.rooms {
		for _, client := range room.Clients {
			client.closeOnce.Do(func() {
				close(client.Done)
			})
			if client.Conn != nil {
				client.Conn.Close()
			}
		}
	}

	log.Println("WebSocket hub shutdown complete")
}

// RoomExists checks if a room has any clients
func (h *Hub) RoomExists(roomSlug string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.rooms[roomSlug]
	return ok
}

// ClientCount returns the number of clients in a room
func (h *Hub) ClientCount(roomSlug string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if room, ok := h.rooms[roomSlug]; ok {
		return len(room.Clients)
	}
	return 0
}
