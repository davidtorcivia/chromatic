package handlers

import (
	"crypto/subtle"
	"encoding/json"
	"html"
	"net/http"
	"regexp"
	"strings"

	"chromatic/internal/api/middleware"
	"chromatic/internal/database"
	"chromatic/internal/logger"
	"chromatic/internal/metrics"
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
	tokenManager    *TokenManager
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
		tokenManager:    NewTokenManager(adminToken), // Use adminToken as HMAC secret
	}

	h.upgrader = gorillaws.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			allowed := h.originValidator.IsAllowed(origin)
			if !allowed {
				logger.Warn("WebSocket connection rejected", "origin", origin, "reason", "origin not allowed")
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

	// Validate the signed token
	tokenPayload, err := h.tokenManager.ValidateToken(token)
	if err != nil {
		logger.Warn("Invalid WebSocket token", "room", slug, "error", err)
		http.Error(w, "Invalid or expired token", http.StatusUnauthorized)
		return
	}

	// Verify token matches the requested room and name
	if tokenPayload.RoomSlug != slug {
		http.Error(w, "Token not valid for this room", http.StatusForbidden)
		return
	}
	if tokenPayload.Name != name {
		http.Error(w, "Token not valid for this name", http.StatusForbidden)
		return
	}

	// Use participant ID from the validated token
	participantID := tokenPayload.ParticipantID

	// Verify the room exists and get its details
	var roomID string
	var roomStatus string
	var streamKeyID *string
	err = h.db.QueryRow("SELECT id, status, stream_key_id FROM rooms WHERE slug = ?", slug).Scan(&roomID, &roomStatus, &streamKeyID)
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
	var role, color string
	var isAdmitted bool
	err = h.db.QueryRow(`
		SELECT role, COALESCE(color, ''), is_admitted
		FROM participants
		WHERE id = ? AND room_id = ?
	`, participantID, roomID).Scan(&role, &color, &isAdmitted)

	if err != nil {
		// Participant not found - this shouldn't happen with valid tokens
		logger.Warn("Participant not found for valid token", "participant_id", participantID, "room", slug)
		http.Error(w, "Participant not found", http.StatusNotFound)
		return
	}

	// Check if participant is admitted (waiting room check)
	if !isAdmitted {
		http.Error(w, "Not admitted to room", http.StatusForbidden)
		return
	}

	// Upgrade connection
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Error("WebSocket upgrade failed", "participant_id", participantID, "room", slug, "error", err)
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
	// Initialize chat rate limiter: 30 messages per minute
	client.InitChatRateLimiter()

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
			logger.Info("Client disconnected", "participant_id", c.ID, "name", c.Name, "room", c.RoomSlug)
		},
	)
}

// initiateSubscription starts the WebRTC subscription for a client
func (h *WebSocketHandler) initiateSubscription(client *websocket.Client, roomSlug string) {
	// Create subscriber connection and get offer
	_, offer, err := h.sfu.CreateSubscriberConnection(roomSlug, client.ID)
	if err != nil {
		logger.Error("Failed to create subscriber connection", "participant_id", client.ID, "room", roomSlug, "error", err)
		return
	}

	// Send offer to client
	client.SendJSON("signal:offer", map[string]interface{}{
		"sdp": offer.SDP,
	})

	logger.Debug("Sent WebRTC offer to client", "participant_id", client.ID, "room", roomSlug)
}

// InitiateSubscriptionsForRoom sends WebRTC offers to all clients in a room
// Called when a room goes live
func (h *WebSocketHandler) InitiateSubscriptionsForRoom(roomSlug string) {
	clients := h.hub.GetRoomClients(roomSlug)
	for _, client := range clients {
		go h.initiateSubscription(client, roomSlug)
	}
	logger.Info("Initiated subscriptions for room", "room", roomSlug, "client_count", len(clients))
}

// sendRoomState sends the initial room state to a newly connected client
func (h *WebSocketHandler) sendRoomState(client *websocket.Client, slug string) {
	// Get room info including watermark settings
	var roomName string
	var isLive bool
	var roomStatus string
	var watermarkMode string
	var watermarkText *string
	var watermarkLogoPath *string
	var watermarkLogoPosition string
	var watermarkOpacity float64

	err := h.db.QueryRow(`
		SELECT name, status,
			COALESCE(watermark_mode, 'none'),
			watermark_text,
			watermark_logo_path,
			COALESCE(watermark_logo_position, 'bottom-right'),
			COALESCE(watermark_opacity, 0.3)
		FROM rooms WHERE slug = ?
	`, slug).Scan(&roomName, &roomStatus, &watermarkMode, &watermarkText, &watermarkLogoPath, &watermarkLogoPosition, &watermarkOpacity)

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

	// Build room data with watermark config
	roomData := map[string]interface{}{
		"slug":                  slug,
		"name":                  roomName,
		"watermarkMode":         watermarkMode,
		"watermarkLogoPosition": watermarkLogoPosition,
		"watermarkOpacity":      watermarkOpacity,
	}

	// Add optional watermark fields if set
	if watermarkText != nil {
		roomData["watermarkText"] = *watermarkText
	}
	if watermarkLogoPath != nil && *watermarkLogoPath != "" {
		// Construct logo URL for client
		roomData["watermarkLogoUrl"] = "/api/config/logo"
	}

	client.SendJSON("room:state", map[string]interface{}{
		"room":         roomData,
		"participants": participantData,
		"isLive":       isLive,
		"iceServers":   h.sfu.GetICEServers(),
	})
}

// handleMessage handles incoming WebSocket messages
func (h *WebSocketHandler) handleMessage(client *websocket.Client, msg websocket.Message) {
	switch msg.Type {
	case "chat:send":
		metrics.Get().TotalMessagesChat.Add(1)
		h.handleChatSend(client, msg.Payload)
	case "cursor":
		metrics.Get().TotalMessagesCursor.Add(1)
		h.handleCursor(client, msg.Payload)
	case "media:toggle":
		metrics.Get().TotalMessagesMedia.Add(1)
		h.handleMediaToggle(client, msg.Payload)
	case "signal:offer":
		h.handleSignalOffer(client, msg.Payload)
	case "signal:answer":
		h.handleSignalAnswer(client, msg.Payload)
	case "signal:candidate":
		h.handleSignalCandidate(client, msg.Payload)
	case "signal:ice-restart":
		h.handleIceRestart(client, msg.Payload)
	case "signal:renegotiate-answer":
		h.handleRenegotiateAnswer(client, msg.Payload)
	// Admin commands
	case "admin:mute":
		h.handleAdminMute(client, msg.Payload)
	case "admin:kick":
		h.handleAdminKick(client, msg.Payload)
	case "admin:end-session":
		h.handleAdminEndSession(client)
	default:
		logger.Debug("Unknown message type", "type", msg.Type, "participant_id", client.ID)
	}
}

func (h *WebSocketHandler) handleChatSend(client *websocket.Client, payload json.RawMessage) {
	// Check chat rate limit: 30 messages per minute
	if !client.AllowChatMessage() {
		logger.Debug("Chat rate limit exceeded", "participant_id", client.ID, "room", client.RoomSlug)
		// Silently drop the message - don't spam the client with errors
		return
	}

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
	// Client-initiated offer - used for client microphone streams (voice chat)
	var data struct {
		SDP string `json:"sdp"`
	}
	if err := json.Unmarshal(payload, &data); err != nil {
		logger.Warn("Invalid signal offer", "participant_id", client.ID, "error", err)
		return
	}

	logger.Debug("Received voice offer", "participant_id", client.ID, "room", client.RoomSlug)

	// Create a peer connection to receive the client's voice audio
	// and create an answer
	answer, err := h.sfu.HandleVoiceOffer(client.RoomSlug, client.ID, data.SDP, func(participantID string, track *pionwebrtc.TrackRemote) {
		// When we receive the audio track from this client, forward it to others
		h.forwardVoiceTrack(client.RoomSlug, participantID, track)
	})

	if err != nil {
		logger.Error("Failed to handle voice offer", "participant_id", client.ID, "error", err)
		return
	}

	// Send answer back to client
	client.SendJSON("signal:voice-answer", map[string]interface{}{
		"sdp": answer,
	})

	logger.Debug("Sent voice answer", "participant_id", client.ID)
}

// forwardVoiceTrack forwards a participant's voice track to all other participants in the room
func (h *WebSocketHandler) forwardVoiceTrack(roomSlug, participantID string, track *pionwebrtc.TrackRemote) {
	logger.Debug("Forwarding voice track", "participant_id", participantID, "room", roomSlug)

	// Get all clients in the room except the sender
	clients := h.hub.GetRoomClients(roomSlug)
	for _, client := range clients {
		if client.ID == participantID {
			continue // Don't send to self
		}

		// Add the voice track to this client's subscriber connection
		// This returns a renegotiation offer that must be sent to the client
		offerSDP, err := h.sfu.AddVoiceTrackToSubscriber(roomSlug, client.ID, participantID, track)
		if err != nil {
			logger.Warn("Failed to add voice track to subscriber", "subscriber_id", client.ID, "source_id", participantID, "error", err)
			continue
		}

		// Send renegotiation offer to client so it can receive the new voice track
		client.SendJSON("signal:renegotiate", map[string]interface{}{
			"sdp":           offerSDP,
			"participantId": participantID, // Who the voice track belongs to
		})

		logger.Debug("Sent renegotiation offer for voice track", "subscriber_id", client.ID, "source_id", participantID)
	}
}

func (h *WebSocketHandler) handleSignalAnswer(client *websocket.Client, payload json.RawMessage) {
	var data struct {
		SDP string `json:"sdp"`
	}
	if err := json.Unmarshal(payload, &data); err != nil {
		logger.Warn("Invalid signal answer", "participant_id", client.ID, "error", err)
		return
	}

	answer := pionwebrtc.SessionDescription{
		Type: pionwebrtc.SDPTypeAnswer,
		SDP:  data.SDP,
	}

	if err := h.sfu.SetSubscriberAnswer(client.RoomSlug, client.ID, answer); err != nil {
		logger.Error("Failed to set subscriber answer", "participant_id", client.ID, "error", err)
		return
	}

	logger.Debug("Set WebRTC answer from client", "participant_id", client.ID)
}

func (h *WebSocketHandler) handleSignalCandidate(client *websocket.Client, payload json.RawMessage) {
	var data struct {
		Candidate        string  `json:"candidate"`
		SDPMid           *string `json:"sdpMid"`
		SDPMLineIndex    *uint16 `json:"sdpMLineIndex"`
		UsernameFragment *string `json:"usernameFragment,omitempty"`
	}
	if err := json.Unmarshal(payload, &data); err != nil {
		logger.Warn("Invalid ICE candidate", "participant_id", client.ID, "error", err)
		return
	}

	candidate := pionwebrtc.ICECandidateInit{
		Candidate:        data.Candidate,
		SDPMid:           data.SDPMid,
		SDPMLineIndex:    data.SDPMLineIndex,
		UsernameFragment: data.UsernameFragment,
	}

	if err := h.sfu.AddSubscriberICECandidate(client.RoomSlug, client.ID, candidate); err != nil {
		logger.Warn("Failed to add ICE candidate", "participant_id", client.ID, "error", err)
		return
	}

	logger.Debug("Added ICE candidate from client", "participant_id", client.ID)
}

func (h *WebSocketHandler) handleIceRestart(client *websocket.Client, payload json.RawMessage) {
	var data struct {
		SDP string `json:"sdp"`
	}
	if err := json.Unmarshal(payload, &data); err != nil {
		logger.Warn("Invalid ICE restart request", "participant_id", client.ID, "error", err)
		return
	}

	logger.Info("Processing ICE restart", "participant_id", client.ID, "room", client.RoomSlug)

	// Handle the ICE restart offer and get answer
	answer, err := h.sfu.HandleIceRestart(client.RoomSlug, client.ID, data.SDP)
	if err != nil {
		logger.Error("Failed to handle ICE restart", "participant_id", client.ID, "error", err)
		return
	}

	// Send answer back to client
	client.SendJSON("signal:answer", map[string]interface{}{
		"sdp": answer,
	})

	logger.Debug("Sent ICE restart answer", "participant_id", client.ID)
}

func (h *WebSocketHandler) handleRenegotiateAnswer(client *websocket.Client, payload json.RawMessage) {
	var data struct {
		SDP string `json:"sdp"`
	}
	if err := json.Unmarshal(payload, &data); err != nil {
		logger.Warn("Invalid renegotiate answer", "participant_id", client.ID, "error", err)
		return
	}

	logger.Debug("Processing renegotiation answer", "participant_id", client.ID, "room", client.RoomSlug)

	// Process the renegotiation answer
	if err := h.sfu.HandleRenegotiationAnswer(client.RoomSlug, client.ID, data.SDP); err != nil {
		logger.Error("Failed to handle renegotiation answer", "participant_id", client.ID, "error", err)
		return
	}

	logger.Debug("Renegotiation completed", "participant_id", client.ID)
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
