package handlers

import (
	"crypto/subtle"
	"encoding/json"
	"html"
	"log"
	"net/http"
	"regexp"
	"strings"

	"chromatic/internal/api/middleware"
	"chromatic/internal/database"
	"chromatic/internal/webrtc"
	"chromatic/internal/websocket"

	gorillaws "github.com/gorilla/websocket"
	pionwebrtc "github.com/pion/webrtc/v4"
)

// WebSocketHandler handles WebSocket connections
type WebSocketHandler struct {
	db              *database.DB
	hub             *websocket.Hub
	sfu             *webrtc.SFU
	originValidator *middleware.OriginValidator
	adminToken      string
	upgrader        gorillaws.Upgrader
}

// NewWebSocketHandler creates a new WebSocketHandler
func NewWebSocketHandler(db *database.DB, hub *websocket.Hub, sfu *webrtc.SFU, allowedOrigins []string, productionMode bool, adminToken string) *WebSocketHandler {
	validator := middleware.NewOriginValidator(allowedOrigins, productionMode)

	h := &WebSocketHandler{
		db:              db,
		hub:             hub,
		sfu:             sfu,
		originValidator: validator,
		adminToken:      adminToken,
	}

	h.upgrader = gorillaws.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			allowed := h.originValidator.IsAllowed(origin)
			if !allowed {
				log.Printf("WebSocket connection rejected: origin %s not allowed", origin)
			}
			return allowed
		},
	}

	return h
}

// htmlTagsRegex matches HTML tags for stripping
var htmlTagsRegex = regexp.MustCompile(`<[^>]*>`)

// sanitizeText removes HTML tags and escapes any remaining special characters
// to prevent XSS attacks in chat messages and participant names
func sanitizeText(input string) string {
	// Strip HTML tags
	stripped := htmlTagsRegex.ReplaceAllString(input, "")
	// Trim whitespace and limit consecutive whitespace
	stripped = strings.TrimSpace(stripped)
	// Escape HTML entities for any remaining special chars
	escaped := html.EscapeString(stripped)
	return escaped
}

// HandleConnection handles WebSocket connection upgrades
func (h *WebSocketHandler) HandleConnection(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	token := r.URL.Query().Get("token")
	name := r.URL.Query().Get("name")
	adminAuth := r.URL.Query().Get("admin") // Optional admin token for admin access

	if token == "" || name == "" {
		http.Error(w, "Missing token or name", http.StatusBadRequest)
		return
	}

	// Check if this is an admin connection (using constant-time comparison)
	isAdminAuth := adminAuth != "" && subtle.ConstantTimeCompare([]byte(adminAuth), []byte(h.adminToken)) == 1

	// Verify the room exists and get its details
	var roomID string
	var roomStatus string
	var streamKeyID *string
	err := h.db.QueryRow("SELECT id, status, stream_key_id FROM rooms WHERE slug = ?", slug).Scan(&roomID, &roomStatus, &streamKeyID)
	if err != nil {
		http.Error(w, "Room not found", http.StatusNotFound)
		return
	}

	// Check if room has ended
	if roomStatus == "ended" {
		http.Error(w, "Room has ended", http.StatusGone)
		return
	}

	// Look up participant to determine role and verify admission
	var participantID, role, color string
	var isAdmitted bool
	err = h.db.QueryRow(`
		SELECT id, role, COALESCE(color, ''), is_admitted
		FROM participants
		WHERE room_id = ? AND name = ?
		ORDER BY joined_at DESC LIMIT 1
	`, roomID, name).Scan(&participantID, &role, &color, &isAdmitted)

	if err != nil {
		// Create participant record if not exists (for admin or direct join)
		participantID = token
		role = "viewer"
		color = assignColor(token)
		isAdmitted = true // Default to admitted for WebSocket connections
	}

	// Check if participant is admitted (waiting room check)
	if !isAdmitted {
		http.Error(w, "Not admitted to room", http.StatusForbidden)
		return
	}

	// Upgrade connection
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}

	// Create client
	// Admin if authenticated via admin token OR has admin role in database
	isAdmin := isAdminAuth || role == "admin"
	if isAdminAuth {
		role = "admin" // Set role to admin for authenticated admin
	}
	client := &websocket.Client{
		ID:           participantID,
		Name:         name,
		Role:         role,
		RoomSlug:     slug,
		Hub:          h.hub,
		Conn:         conn,
		Send:         make(chan []byte, 256),
		IsAdmin:      isAdmin,
		AudioEnabled: true,
		VideoEnabled: true,
	}

	// Assign color if not set
	if color == "" {
		color = assignColor(participantID)
	}
	client.Color = color

	// Register with hub
	h.hub.Register(client)

	// Send initial room state
	h.sendRoomState(client, slug)

	// Notify others of new participant
	h.hub.BroadcastJSON(slug, "participant:joined", map[string]interface{}{
		"participant": map[string]interface{}{
			"id":           client.ID,
			"name":         client.Name,
			"role":         client.Role,
			"color":        client.Color,
			"audioEnabled": client.AudioEnabled,
			"videoEnabled": client.VideoEnabled,
		},
	}, client.ID)

	// If room is live, initiate WebRTC subscription
	if roomStatus == "live" {
		go h.initiateSubscription(client, slug)
	}

	// Start write pump
	go client.WritePump()

	// Start read pump with disconnect handler
	go client.ReadPumpWithDisconnect(
		func(c *websocket.Client, msg websocket.Message) {
			h.handleMessage(c, msg)
		},
		func(c *websocket.Client) {
			// Broadcast participant:left when client disconnects
			h.hub.BroadcastJSON(c.RoomSlug, "participant:left", map[string]interface{}{
				"participantId": c.ID,
			}, "")
			log.Printf("Client %s (%s) disconnected from room %s", c.ID, c.Name, c.RoomSlug)
		},
	)
}

// initiateSubscription starts the WebRTC subscription for a client
func (h *WebSocketHandler) initiateSubscription(client *websocket.Client, roomSlug string) {
	// Create subscriber connection and get offer
	_, offer, err := h.sfu.CreateSubscriberConnection(roomSlug, client.ID)
	if err != nil {
		log.Printf("Failed to create subscriber connection for %s: %v", client.ID, err)
		return
	}

	// Send offer to client
	client.SendJSON("signal:offer", map[string]interface{}{
		"sdp": offer.SDP,
	})

	log.Printf("Sent WebRTC offer to client %s for room %s", client.ID, roomSlug)
}

// InitiateSubscriptionsForRoom sends WebRTC offers to all clients in a room
// Called when a room goes live
func (h *WebSocketHandler) InitiateSubscriptionsForRoom(roomSlug string) {
	clients := h.hub.GetRoomClients(roomSlug)
	for _, client := range clients {
		go h.initiateSubscription(client, roomSlug)
	}
	log.Printf("Initiated subscriptions for %d clients in room %s", len(clients), roomSlug)
}

// sendRoomState sends the initial room state to a newly connected client
func (h *WebSocketHandler) sendRoomState(client *websocket.Client, slug string) {
	// Get room info
	var roomName string
	var isLive bool
	var roomStatus string

	err := h.db.QueryRow(`
		SELECT name, status FROM rooms WHERE slug = ?
	`, slug).Scan(&roomName, &roomStatus)

	if err != nil {
		return
	}

	isLive = roomStatus == "live"

	// Get participants
	participants := h.hub.GetRoomClients(slug)
	participantData := make([]map[string]interface{}, 0)
	for _, p := range participants {
		participantData = append(participantData, map[string]interface{}{
			"id":           p.ID,
			"name":         p.Name,
			"role":         p.Role,
			"color":        p.Color,
			"audioEnabled": p.AudioEnabled,
			"videoEnabled": p.VideoEnabled,
		})
	}

	client.SendJSON("room:state", map[string]interface{}{
		"room": map[string]interface{}{
			"slug": slug,
			"name": roomName,
		},
		"participants": participantData,
		"isLive":       isLive,
		"iceServers":   h.sfu.GetICEServers(),
	})
}

// handleMessage handles incoming WebSocket messages
func (h *WebSocketHandler) handleMessage(client *websocket.Client, msg websocket.Message) {
	switch msg.Type {
	case "chat:send":
		h.handleChatSend(client, msg.Payload)
	case "cursor":
		h.handleCursor(client, msg.Payload)
	case "media:toggle":
		h.handleMediaToggle(client, msg.Payload)
	case "signal:offer":
		h.handleSignalOffer(client, msg.Payload)
	case "signal:answer":
		h.handleSignalAnswer(client, msg.Payload)
	case "signal:candidate":
		h.handleSignalCandidate(client, msg.Payload)
	// Admin commands
	case "admin:mute":
		h.handleAdminMute(client, msg.Payload)
	case "admin:kick":
		h.handleAdminKick(client, msg.Payload)
	case "admin:end-session":
		h.handleAdminEndSession(client)
	default:
		log.Printf("Unknown message type: %s", msg.Type)
	}
}

func (h *WebSocketHandler) handleChatSend(client *websocket.Client, payload json.RawMessage) {
	var data struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(payload, &data); err != nil {
		return
	}

	// Sanitize content to prevent XSS
	sanitizedContent := sanitizeText(data.Content)

	if len(sanitizedContent) == 0 || len(data.Content) > 2000 {
		return
	}

	// Broadcast to all in room
	h.hub.BroadcastJSON(client.RoomSlug, "chat:message", map[string]interface{}{
		"participantId":   client.ID,
		"participantName": sanitizeText(client.Name),
		"type":            "text",
		"content":         sanitizedContent,
	}, "")
}

func (h *WebSocketHandler) handleCursor(client *websocket.Client, payload json.RawMessage) {
	var data struct {
		X      float64 `json:"x"`
		Y      float64 `json:"y"`
		Active bool    `json:"active"`
	}
	if err := json.Unmarshal(payload, &data); err != nil {
		return
	}

	// Broadcast cursor to all (including sender for latency feedback)
	h.hub.BroadcastJSON(client.RoomSlug, "cursor", map[string]interface{}{
		"participantId":   client.ID,
		"participantName": client.Name,
		"color":           client.Color,
		"x":               data.X,
		"y":               data.Y,
		"active":          data.Active,
	}, "")
}

func (h *WebSocketHandler) handleMediaToggle(client *websocket.Client, payload json.RawMessage) {
	var data struct {
		Audio *bool `json:"audio,omitempty"`
		Video *bool `json:"video,omitempty"`
	}
	if err := json.Unmarshal(payload, &data); err != nil {
		return
	}

	if data.Audio != nil {
		client.AudioEnabled = *data.Audio
	}
	if data.Video != nil {
		client.VideoEnabled = *data.Video
	}

	// Notify others
	h.hub.BroadcastJSON(client.RoomSlug, "participant:updated", map[string]interface{}{
		"participant": map[string]interface{}{
			"id":           client.ID,
			"audioEnabled": client.AudioEnabled,
			"videoEnabled": client.VideoEnabled,
		},
	}, client.ID)
}

func (h *WebSocketHandler) handleSignalOffer(client *websocket.Client, payload json.RawMessage) {
	// Client-initiated offer - used for client webcam/mic streams
	// For now, we only support server-initiated subscription (handleSignalAnswer)
	log.Printf("Received signal offer from %s (client media not yet supported)", client.ID)
}

func (h *WebSocketHandler) handleSignalAnswer(client *websocket.Client, payload json.RawMessage) {
	var data struct {
		SDP string `json:"sdp"`
	}
	if err := json.Unmarshal(payload, &data); err != nil {
		log.Printf("Invalid signal answer from %s: %v", client.ID, err)
		return
	}

	answer := pionwebrtc.SessionDescription{
		Type: pionwebrtc.SDPTypeAnswer,
		SDP:  data.SDP,
	}

	if err := h.sfu.SetSubscriberAnswer(client.RoomSlug, client.ID, answer); err != nil {
		log.Printf("Failed to set subscriber answer for %s: %v", client.ID, err)
		return
	}

	log.Printf("Set WebRTC answer from client %s", client.ID)
}

func (h *WebSocketHandler) handleSignalCandidate(client *websocket.Client, payload json.RawMessage) {
	var data struct {
		Candidate        string  `json:"candidate"`
		SDPMid           *string `json:"sdpMid"`
		SDPMLineIndex    *uint16 `json:"sdpMLineIndex"`
		UsernameFragment *string `json:"usernameFragment,omitempty"`
	}
	if err := json.Unmarshal(payload, &data); err != nil {
		log.Printf("Invalid ICE candidate from %s: %v", client.ID, err)
		return
	}

	candidate := pionwebrtc.ICECandidateInit{
		Candidate:        data.Candidate,
		SDPMid:           data.SDPMid,
		SDPMLineIndex:    data.SDPMLineIndex,
		UsernameFragment: data.UsernameFragment,
	}

	if err := h.sfu.AddSubscriberICECandidate(client.RoomSlug, client.ID, candidate); err != nil {
		log.Printf("Failed to add ICE candidate for %s: %v", client.ID, err)
		return
	}

	log.Printf("Added ICE candidate from client %s", client.ID)
}

func (h *WebSocketHandler) handleAdminMute(client *websocket.Client, payload json.RawMessage) {
	if !client.IsAdmin {
		return
	}

	var data struct {
		ParticipantID string `json:"participantId"`
	}
	if err := json.Unmarshal(payload, &data); err != nil {
		return
	}

	h.hub.BroadcastJSON(client.RoomSlug, "admin:muted", map[string]interface{}{
		"participantId": data.ParticipantID,
	}, "")
}

func (h *WebSocketHandler) handleAdminKick(client *websocket.Client, payload json.RawMessage) {
	if !client.IsAdmin {
		return
	}

	var data struct {
		ParticipantID string  `json:"participantId"`
		Reason        *string `json:"reason,omitempty"`
	}
	if err := json.Unmarshal(payload, &data); err != nil {
		return
	}

	// Send kick message to the target
	h.hub.SendToJSON(client.RoomSlug, data.ParticipantID, "kicked", map[string]interface{}{
		"reason": data.Reason,
	})

	// Notify others
	h.hub.BroadcastJSON(client.RoomSlug, "participant:left", map[string]interface{}{
		"participantId": data.ParticipantID,
	}, "")
}

func (h *WebSocketHandler) handleAdminEndSession(client *websocket.Client) {
	if !client.IsAdmin {
		return
	}

	// Broadcast session end to all
	h.hub.BroadcastJSON(client.RoomSlug, "room:ended", map[string]interface{}{}, "")
}
