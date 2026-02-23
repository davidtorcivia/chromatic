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

	// Broadcast to all clients in a room
	broadcast chan *RoomMessage

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
	ID           string
	Name         string
	Role         string
	RoomSlug     string
	Color        string
	Hub          *Hub
	Conn         *websocket.Conn
	Send         chan []byte
	done         chan struct{}
	closeOnce    sync.Once // Ensures Send channel is closed only once
	IsAdmin      bool
	AudioEnabled bool
	VideoEnabled bool

	// Rate limiting for chat messages (30 per minute)
	chatRateLimiter *RateLimiter
	// Rate limiting for cursor updates (20 per second)
	cursorRateLimiter *RateLimiter
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

// RoomMessage is a message to be broadcast to a room
type RoomMessage struct {
	RoomSlug string
	Message  []byte
	Exclude  string // Exclude this client ID from broadcast
}

// Message represents a WebSocket message
type Message struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// NewHub creates a new Hub
func NewHub() *Hub {
	return &Hub{
		rooms:     make(map[string]*Room),
		broadcast: make(chan *RoomMessage, 256),
		done:      make(chan struct{}),
	}
}

// Run starts the Hub's main event loop (processes broadcasts)
func (h *Hub) Run() {
	for {
		select {
		case message := <-h.broadcast:
			h.broadcastToRoom(message)

		case <-h.done:
			return
		}
	}
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

	room.Clients[client.ID] = client
	// Track WebSocket connections
	metrics.Get().ActiveWebsockets.Add(1)
	log.Printf("Client %s (%s) joined room %s", client.ID, client.Name, client.RoomSlug)
}

func (h *Hub) unregisterClient(client *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if room, ok := h.rooms[client.RoomSlug]; ok {
		if _, ok := room.Clients[client.ID]; ok {
			delete(room.Clients, client.ID)
			// Use sync.Once to prevent double-close panic
			client.closeOnce.Do(func() {
				close(client.Send)
			})
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
	}
}

func (h *Hub) broadcastToRoom(msg *RoomMessage) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	room, ok := h.rooms[msg.RoomSlug]
	if !ok {
		return
	}

	for id, client := range room.Clients {
		if id == msg.Exclude {
			continue
		}

		select {
		case client.Send <- msg.Message:
		default:
			// Client's send buffer is full, close it safely
			client.closeOnce.Do(func() {
				close(client.Send)
			})
			delete(room.Clients, id)
		}
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

// Broadcast sends a message to all clients in a room
func (h *Hub) Broadcast(roomSlug string, message []byte, excludeID string) {
	h.broadcast <- &RoomMessage{
		RoomSlug: roomSlug,
		Message:  message,
		Exclude:  excludeID,
	}
}

// BroadcastJSON sends a JSON message to all clients in a room
func (h *Hub) BroadcastJSON(roomSlug string, msgType string, payload interface{}, excludeID string) error {
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

// Shutdown gracefully shuts down the hub
func (h *Hub) Shutdown() {
	close(h.done)

	h.mu.Lock()
	defer h.mu.Unlock()

	for _, room := range h.rooms {
		for _, client := range room.Clients {
			client.Conn.Close()
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
