package handlers

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"chromatic/internal/api/middleware"
	"chromatic/internal/database"
	"chromatic/internal/logger"
	"chromatic/internal/metrics"
	"chromatic/internal/webrtc"
	"chromatic/internal/websocket"

	gorillaws "github.com/gorilla/websocket"
	pionwebrtc "github.com/pion/webrtc/v4"
)

// SessionValidator validates an admin session ID and returns true if still active.
type SessionValidator func(sessionID string) bool

// WaitingActions exposes the shared waiting-room admit/deny logic (implemented
// by RoomHandler) so the admin:waiting-approve/deny WebSocket commands behave
// identically to the REST endpoints.
type WaitingActions interface {
	AdmitWaitingParticipant(roomSlug, participantID string) (bool, error)
	DenyWaitingParticipant(roomSlug, participantID string) (bool, error)
}

// WebSocketHandler handles WebSocket connections
type WebSocketHandler struct {
	db              *database.DB
	hub             *websocket.Hub
	sfu             *webrtc.SFU
	originValidator *middleware.OriginValidator
	tokenManager    *TokenManager
	validateSession SessionValidator
	waitingActions  WaitingActions
	upgrader        gorillaws.Upgrader

	// subMu guards subStates: per-connection subscription bookkeeping keyed by
	// the *websocket.Client pointer (a page refresh creates a new Client and
	// therefore fresh state, while the SFU replaces the same-ID subscriber).
	subMu     sync.Mutex
	subStates map[*websocket.Client]*subscriptionState
}

// subscriptionState serializes subscriber creation for one connected client.
// Two paths can race to create the SFU subscriber + send the initial
// signal:offer: the connect path (room already live) and the stream-start
// path (client connected before the ingest existed). Holding mu across the
// whole attempt and recording success in created guarantees the client ends
// up with exactly one subscriber and one offer.
type subscriptionState struct {
	mu      sync.Mutex
	created bool
}

// SetWaitingActions wires the shared waiting-room admit/deny implementation.
func (h *WebSocketHandler) SetWaitingActions(actions WaitingActions) {
	h.waitingActions = actions
}

// NewWebSocketHandler creates a new WebSocketHandler.
// tokenSecret is the derived secret used to sign/verify join tokens (never
// the raw admin token). validateSession is used to check the admin session
// cookie on upgrade and again on each privileged action so that a mid-session
// logout drops privileges.
func NewWebSocketHandler(
	db *database.DB,
	hub *websocket.Hub,
	sfu *webrtc.SFU,
	allowedOrigins []string,
	productionMode bool,
	tokenSecret []byte,
	validateSession SessionValidator,
) *WebSocketHandler {
	validator := middleware.NewOriginValidator(allowedOrigins, productionMode)

	h := &WebSocketHandler{
		db:              db,
		hub:             hub,
		sfu:             sfu,
		originValidator: validator,
		tokenManager:    NewTokenManager(tokenSecret),
		validateSession: validateSession,
		subStates:       make(map[*websocket.Client]*subscriptionState),
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

// HandleConnection handles WebSocket connection upgrades
func (h *WebSocketHandler) HandleConnection(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	token := r.URL.Query().Get("token")
	name := r.URL.Query().Get("name")

	if token == "" || name == "" {
		http.Error(w, "Missing token or name", http.StatusBadRequest)
		return
	}

	// Admin status comes from the httpOnly session cookie set by POST
	// /api/auth/login, or from the participant's DB role (assigned at join
	// time when a valid admin token was provided in the join request body).
	// Never accept a long-lived credential in the URL (logs / browser
	// history / referrer).
	var adminSessionID string
	if h.validateSession != nil {
		if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" && h.validateSession(c.Value) {
			adminSessionID = c.Value
		}
	}
	isAdminAuth := adminSessionID != ""

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
		// The display name was bound into the signed token at join time and is
		// authoritative. The query-string name can drift (the client caches the
		// last-used name in localStorage, which another join may overwrite), and
		// a page refresh mid-session must never hard-fail on a stale cached
		// name — that left viewers stuck on the "waiting for stream" screen
		// with the WebSocket reconnect loop failing forever.
		logger.Debug("WebSocket name differs from token; using token name",
			"room", slug, "query_name", name, "token_name", tokenPayload.Name)
		name = tokenPayload.Name
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
	var canScreenshare bool
	err = h.db.QueryRow(`
		SELECT role, COALESCE(color, ''), is_admitted, COALESCE(can_screenshare, FALSE)
		FROM participants
		WHERE id = ? AND room_id = ?
	`, participantID, roomID).Scan(&role, &color, &isAdmitted, &canScreenshare)

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
	// Admin if authenticated via a valid admin session cookie OR the
	// participant's role in the database is admin (assigned at join time when
	// a valid admin token was provided in the join request body).
	isAdmin := isAdminAuth || role == "admin"
	if isAdminAuth && role != "admin" {
		role = "admin"
	}
	client := &websocket.Client{
		ID:             participantID,
		Name:           name,
		Role:           role,
		RoomSlug:       slug,
		Hub:            h.hub,
		Conn:           conn,
		Send:           make(chan []byte, 256),
		Done:           make(chan struct{}),
		IsAdmin:        isAdmin,
		AdminSessionID: adminSessionID,
	}
	client.SetAudioEnabled(false)
	client.SetVideoEnabled(true)
	// Initialize chat rate limiter: 30 messages per minute
	client.InitChatRateLimiter()
	// Initialize cursor rate limiter: 20 updates per second
	client.InitCursorRateLimiter()

	// Assign color if not set
	if color == "" {
		color = assignColor(participantID)
	}
	client.Color = color

	// Create the subscription state BEFORE registering with the hub so any
	// client visible to the stream-start path (which iterates hub clients) has
	// an entry. The entry is removed in the disconnect handler below.
	h.subMu.Lock()
	h.subStates[client] = &subscriptionState{}
	h.subMu.Unlock()

	// Register with hub
	h.hub.Register(client)

	// Send initial room state and chat history
	h.sendRoomState(client, slug)
	h.sendChatHistory(client, slug)
	// Admins joining mid-session still see pending waiting-room requests
	h.sendWaitingState(client, slug)

	// Notify others of new participant
	h.hub.BroadcastJSON(slug, "participant:joined", map[string]interface{}{
		"participant": map[string]interface{}{
			"id":             client.ID,
			"name":           client.Name,
			"role":           client.Role,
			"color":          client.Color,
			"audioEnabled":   client.AudioEnabled(),
			"videoEnabled":   client.VideoEnabled(),
			"canScreenshare": canScreenshare || isAdmin,
		},
	}, client.ID)

	// If the stream is already up, initiate the WebRTC subscription. The room
	// can be marked live in the DB while the SFU has no bound ingest yet (OBS
	// reconnecting, or a server restart) — that is a normal pre-stream state:
	// ensureSubscription defers, and the stream-start path creates the
	// subscriber the moment the ingest binds. The SFU-side check additionally
	// covers the race where the DB status was read as "pending" just before
	// OnStreamStart committed: it runs after hub registration, so a client
	// that misses the stream-start client snapshot sees the SFU live here.
	if roomStatus == "live" || h.sfu.IsRoomLive(slug) {
		go h.ensureSubscription(client, slug)
	}

	// Start write pump
	go client.WritePump()

	// Start read pump with disconnect handler
	go client.ReadPumpWithDisconnect(
		func(c *websocket.Client, msg websocket.Message) {
			h.handleMessage(c, msg)
		},
		func(c *websocket.Client) {
			// This Client object is dead either way (replaced or disconnected),
			// so drop its subscription state. Keyed by pointer, so a refresh
			// replacement (a different Client) keeps its own entry.
			h.subMu.Lock()
			delete(h.subStates, c)
			h.subMu.Unlock()

			// Clean up screen share if this participant was sharing. This runs
			// BEFORE the replaced-check on purpose: a page refresh replaces the
			// Client but the getDisplayMedia capture died with the old page, so
			// the share must be torn down for everyone either way (they can
			// re-share after reloading).
			roomTracks := h.sfu.GetRoomTracksForSlug(c.RoomSlug)
			if roomTracks != nil {
				roomTracks.RLockVoiceTracks()
				wasSharing := roomTracks.ScreenShareParticipantID == c.ID
				roomTracks.RUnlockVoiceTracks()
				if wasSharing {
					affected := h.sfu.RemoveScreenShareTrack(c.RoomSlug)
					h.hub.BroadcastJSON(c.RoomSlug, "screenshare:stopped", map[string]interface{}{}, "")
					// Renegotiate affected subscribers
					for _, subID := range affected {
						go h.renegotiateSubscriber(c.RoomSlug, subID)
					}
					logger.Info("Screen share stopped (sharer disconnected)", "sharer", c.ID, "room", c.RoomSlug)
				}
			}

			// Atomically unregister ONLY if this client is still current. If a
			// new connection (page refresh) already replaced it, skip the
			// remaining cleanup — the participant is still in the room. The
			// check and the removal must be one atomic step: a separate
			// check-then-cleanup races with the replacement registering, and
			// the stale participant:left broadcast would remove a present
			// participant from everyone's UI.
			if !h.hub.UnregisterIfCurrent(c) {
				logger.Info("Client replaced (page refresh), skipping cleanup", "participant_id", c.ID, "name", c.Name, "room", c.RoomSlug)
				return
			}
			// Broadcast participant:left when client disconnects
			h.hub.BroadcastJSON(c.RoomSlug, "participant:left", map[string]interface{}{
				"participantId": c.ID,
			}, "")
			logger.Info("Client disconnected", "participant_id", c.ID, "name", c.Name, "room", c.RoomSlug)
		},
	)
}

// ensureSubscription runs the create-subscriber + send-offer flow for a
// client exactly once. It is the single entry point shared by the connect
// path (room already live) and the stream-start path (client connected before
// the ingest existed). Attempts for one client are serialized and success is
// recorded, so a client connecting at the same moment the stream starts never
// receives two subscribers/offers; a pre-stream attempt (SFU room not live
// yet) leaves the state unset so the stream-start path retries it.
func (h *WebSocketHandler) ensureSubscription(client *websocket.Client, roomSlug string) {
	h.subMu.Lock()
	st, ok := h.subStates[client]
	h.subMu.Unlock()
	if !ok {
		// Client already disconnected (state removed by the disconnect handler).
		return
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if st.created && h.sfu.HasSubscriber(roomSlug, client.ID) {
		// Already subscribed and the subscriber is still registered with the
		// SFU — an ingest (re)bind reuses it via ReplaceTrack/renegotiation,
		// so creating a replacement here would force a needless re-handshake.
		return
	}
	if h.initiateSubscription(client, roomSlug) {
		st.created = true
	}
}

// initiateSubscription starts the WebRTC subscription for a client. Returns
// true when a subscriber was created and the offer sent; false when the
// attempt should be retried later (the stream isn't up yet, or subscriber
// creation failed and the next stream-start may succeed).
func (h *WebSocketHandler) initiateSubscription(client *websocket.Client, roomSlug string) bool {
	// Pre-stream state: the SFU has no ingest tracks bound for this room yet
	// (viewer connected before OBS started, OBS reconnecting, or a server
	// restart). This is normal, not an error — the stream-start path runs this
	// flow again for clients without a subscriber once the ingest binds.
	if !h.sfu.IsRoomLive(roomSlug) {
		logger.Debug("Stream not started yet; deferring subscriber creation until ingest binds",
			"participant_id", client.ID, "room", roomSlug)
		return false
	}

	// Create subscriber connection and get offer (no ICE candidates yet — trickle ICE)
	pc, offerSDP, err := h.sfu.CreateSubscriberConnection(roomSlug, client.ID)
	if err != nil {
		logger.Error("Failed to create subscriber connection", "participant_id", client.ID, "room", roomSlug, "error", err)
		// Tell the client the handshake failed so it can surface an error and
		// offer a retry instead of sitting on "Connecting…" forever.
		client.SendJSON("signal:error", map[string]interface{}{
			"code":    "subscription-failed",
			"message": "Failed to initialize stream. Please refresh the page to retry.",
		})
		return false
	}

	// Listen for inbound tracks from this participant (voice audio + screen share video)
	pc.OnTrack(func(track *pionwebrtc.TrackRemote, receiver *pionwebrtc.RTPReceiver) {
		streamID := track.StreamID()

		// Screen share tracks are identified by stream ID prefix
		if strings.HasPrefix(streamID, "screenshare-stream-") {
			h.forwardScreenShareTrack(roomSlug, client.ID, track)
			return
		}

		if track.Kind() == pionwebrtc.RTPCodecTypeVideo {
			// Video track from subscriber — must be screen share
			h.forwardScreenShareTrack(roomSlug, client.ID, track)
			return
		}

		if track.Kind() == pionwebrtc.RTPCodecTypeAudio {
			h.forwardVoiceTrack(roomSlug, client.ID, track)
		}
	})

	// Send offer to client FIRST (before enabling trickle ICE)
	client.SendJSON("signal:offer", map[string]interface{}{
		"sdp": offerSDP,
	})

	// Enable trickle ICE: flush any buffered candidates and send future ones directly.
	// This must happen AFTER the offer is sent to guarantee correct message ordering.
	h.sfu.EnableSubscriberTrickleICE(roomSlug, client.ID, func(init *pionwebrtc.ICECandidateInit) {
		client.SendJSON("signal:candidate", map[string]interface{}{
			"candidate":     init.Candidate,
			"sdpMid":        init.SDPMid,
			"sdpMLineIndex": init.SDPMLineIndex,
		})
	})

	// Wire the deferred-renegotiation push: tracks attached while another
	// offer/answer exchange was in flight get their follow-up offer sent
	// here once signaling settles, instead of being dropped.
	h.sfu.SetSubscriberRenegotiationCallback(roomSlug, client.ID, func(sdp string) {
		client.SendJSON("signal:renegotiate", map[string]interface{}{
			"sdp": sdp,
		})
		logger.Debug("Sent deferred renegotiation offer", "participant_id", client.ID, "room", roomSlug)
	})

	logger.Debug("Sent WebRTC offer to client (trickle ICE)", "participant_id", client.ID, "room", roomSlug)
	return true
}

// handleClientDebug mirrors client-side flow breadcrumbs (e.g. the screen
// share pipeline) into the server log so failures on remote testers'
// machines are diagnosable without access to their browser console.
func (h *WebSocketHandler) handleClientDebug(client *websocket.Client, payload json.RawMessage) {
	var data struct {
		Event  string `json:"event"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(payload, &data); err != nil || data.Event == "" {
		return
	}
	if len(data.Event) > 64 {
		data.Event = data.Event[:64]
	}
	if len(data.Detail) > 256 {
		data.Detail = data.Detail[:256]
	}
	logger.Info("Client debug", "event", data.Event, "detail", data.Detail,
		"participant_id", client.ID, "name", client.Name, "room", client.RoomSlug)
}

// handleResubscribe forces a fresh subscriber for a client whose media path
// failed beyond ICE-restart repair (e.g. dead TURN allocation). It reruns the
// full subscription flow; CreateSubscriberConnection displaces the old
// subscriber atomically (same as a rejoin), so voice relay state and the
// client's WS session are untouched — only the media transport is rebuilt.
func (h *WebSocketHandler) handleResubscribe(client *websocket.Client) {
	roomSlug := client.RoomSlug
	logger.Info("Client requested fresh subscription", "participant_id", client.ID, "room", roomSlug)

	h.subMu.Lock()
	st, ok := h.subStates[client]
	h.subMu.Unlock()
	if !ok {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.created = h.initiateSubscription(client, roomSlug)
}

// InitiateSubscriptionsForRoom sends WebRTC offers to clients in a room that
// don't have an SFU subscriber yet. Called from the stream-start path AFTER
// BindIngestToRoom, so viewers/hosts who connected before OBS started get
// their offer the moment the stream goes live. Clients that already hold a
// live subscriber are skipped — the ingest bind rebinds their tracks in place
// (ReplaceTrack) or queues them for renegotiation.
func (h *WebSocketHandler) InitiateSubscriptionsForRoom(roomSlug string) {
	clients := h.hub.GetRoomClients(roomSlug)
	for _, client := range clients {
		go h.ensureSubscription(client, roomSlug)
	}
	logger.Info("Ensured subscriptions for room", "room", roomSlug, "client_count", len(clients))
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
	var watermarkPosX, watermarkPosY *float64
	var watermarkScale float64
	var scheduledAt, openedAt *time.Time
	var earlyOpenMinutes int
	var waitingRoomEnabled bool

	err := h.db.QueryRow(`
		SELECT name, status,
			COALESCE(watermark_mode, 'none'),
			watermark_text,
			watermark_logo_path,
			COALESCE(watermark_logo_position, 'bottom-right'),
			COALESCE(watermark_opacity, 0.3),
			watermark_pos_x,
			watermark_pos_y,
			COALESCE(watermark_scale, 1.0),
			scheduled_at,
			opened_at,
			COALESCE(early_open_minutes, 10),
			waiting_room_enabled
		FROM rooms WHERE slug = ?
	`, slug).Scan(&roomName, &roomStatus, &watermarkMode, &watermarkText, &watermarkLogoPath, &watermarkLogoPosition, &watermarkOpacity, &watermarkPosX, &watermarkPosY, &watermarkScale,
		&scheduledAt, &openedAt, &earlyOpenMinutes, &waitingRoomEnabled)

	if err != nil {
		return
	}

	isLive = roomStatus == "live"

	// Persistent screen share approvals (admins are implicitly allowed)
	canShare := make(map[string]bool)
	if rows, err := h.db.Query(`
		SELECT p.id, COALESCE(p.can_screenshare, FALSE)
		FROM participants p
		JOIN rooms r ON r.id = p.room_id
		WHERE r.slug = ?
	`, slug); err == nil {
		defer rows.Close()
		for rows.Next() {
			var id string
			var allowed bool
			if rows.Scan(&id, &allowed) == nil {
				canShare[id] = allowed
			}
		}
	}

	// Get participants
	participants := h.hub.GetRoomClients(slug)
	participantData := make([]map[string]interface{}, 0)
	for _, p := range participants {
		participantData = append(participantData, map[string]interface{}{
			"id":             p.ID,
			"name":           p.Name,
			"role":           p.Role,
			"color":          p.Color,
			"audioEnabled":   p.AudioEnabled(),
			"videoEnabled":   p.VideoEnabled(),
			"canScreenshare": canShare[p.ID] || p.IsAdmin,
		})
	}

	// Build room data with watermark config
	roomData := map[string]interface{}{
		"slug":                  slug,
		"name":                  roomName,
		"watermarkMode":         watermarkMode,
		"watermarkLogoPosition": watermarkLogoPosition,
		"watermarkOpacity":      watermarkOpacity,
		"watermarkScale":        watermarkScale,
		"earlyOpenMinutes":      earlyOpenMinutes,
		"waitingRoomEnabled":    waitingRoomEnabled,
	}
	// Scheduling/open state for the admin "open room now" banner
	if scheduledAt != nil {
		roomData["scheduledAt"] = scheduledAt.UTC()
	}
	if openedAt != nil {
		roomData["openedAt"] = openedAt.UTC()
	}

	// Add optional watermark fields if set
	if watermarkText != nil {
		roomData["watermarkText"] = *watermarkText
	}
	// Custom watermark position (fractional center); absent = legacy placement
	if watermarkPosX != nil {
		roomData["watermarkPosX"] = *watermarkPosX
	}
	if watermarkPosY != nil {
		roomData["watermarkPosY"] = *watermarkPosY
	}
	if watermarkLogoPath != nil && *watermarkLogoPath != "" {
		// Construct logo URL for client
		roomData["watermarkLogoUrl"] = "/api/config/logo"
	}

	// Include active screen share state for late joiners
	var screenShareData interface{}
	roomTracks := h.sfu.GetRoomTracksForSlug(slug)
	if roomTracks != nil {
		roomTracks.RLockVoiceTracks() // reuse RLock helper
		if roomTracks.ScreenShareParticipantID != "" {
			// Look up the sharer's name
			sharerName := roomTracks.ScreenShareParticipantID
			for _, p := range participantData {
				if p["id"] == roomTracks.ScreenShareParticipantID {
					if n, ok := p["name"].(string); ok {
						sharerName = n
					}
					break
				}
			}
			screenShareData = map[string]interface{}{
				"participantId": roomTracks.ScreenShareParticipantID,
				"name":          sharerName,
			}
		}
		roomTracks.RUnlockVoiceTracks()
	}

	client.SendJSON("room:state", map[string]interface{}{
		"room":         roomData,
		"participants": participantData,
		"isLive":       isLive,
		"iceServers":   h.sfu.GetICEServers(),
		"screenShare":  screenShareData,
	})
}

// sendWaitingState sends the current waiting-room roster (waiting:list) or
// countdown-lobby headcount (lobby:count) to a newly connected admin so an
// admin joining mid-session still sees pending requests. No-op for guests.
func (h *WebSocketHandler) sendWaitingState(client *websocket.Client, slug string) {
	if !client.IsAdmin {
		return
	}

	var roomID string
	var waitingRoomEnabled bool
	if err := h.db.QueryRow(`
		SELECT id, waiting_room_enabled FROM rooms WHERE slug = ?
	`, slug).Scan(&roomID, &waitingRoomEnabled); err != nil {
		return
	}

	rows, err := h.db.Query(`
		SELECT id, name, joined_at FROM participants
		WHERE room_id = ? AND is_admitted = FALSE
		ORDER BY joined_at
	`, roomID)
	if err != nil {
		logger.Warn("Failed to load waiting roster", "room", slug, "error", err)
		return
	}
	defer rows.Close()

	waiting := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, name string
		var joinedAt time.Time
		if rows.Scan(&id, &name, &joinedAt) != nil {
			continue
		}
		waiting = append(waiting, map[string]interface{}{
			"participantId": id,
			"name":          sanitizeText(name),
			"joinedAt":      joinedAt.UTC(),
		})
	}

	if waitingRoomEnabled {
		client.SendJSON("waiting:list", map[string]interface{}{
			"participants": waiting,
		})
	} else {
		client.SendJSON("lobby:count", map[string]interface{}{
			"count": len(waiting),
		})
	}
}

// sendChatHistory sends the most recent persisted chat messages to a newly
// connected client. We fetch the tail via DESC+LIMIT and reverse in memory so
// the client sees oldest-first; older messages beyond chatHistoryLimit are
// considered archive-only. A large room with 10k messages would otherwise
// block each new joiner on a full scan.
func (h *WebSocketHandler) sendChatHistory(client *websocket.Client, slug string) {
	const chatHistoryLimit = 50
	rows, err := h.db.Query(`
		SELECT m.id, m.participant_id, p.name, m.type, m.content, m.created_at
		FROM messages m
		JOIN participants p ON p.id = m.participant_id
		WHERE m.room_id = (SELECT id FROM rooms WHERE slug = ?)
		ORDER BY m.created_at DESC
		LIMIT ?
	`, slug, chatHistoryLimit)
	if err != nil {
		logger.Warn("Failed to load chat history", "room", slug, "error", err)
		return
	}
	defer rows.Close()

	messages := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, participantID, participantName, msgType, content string
		var createdAt time.Time
		if err := rows.Scan(&id, &participantID, &participantName, &msgType, &content, &createdAt); err != nil {
			continue
		}

		msg := map[string]interface{}{
			"id":              id,
			"participantId":   participantID,
			"participantName": sanitizeText(participantName),
			"type":            msgType,
			"content":         content,
			"timestamp":       createdAt.UnixMilli(),
		}

		// For file messages, parse the stored JSON content back into a file object
		if msgType == "file" {
			var fileData map[string]interface{}
			if json.Unmarshal([]byte(content), &fileData) == nil {
				msg["file"] = fileData
				msg["content"] = ""
			}
		}

		messages = append(messages, msg)
	}

	// Rows came back newest-first so the client receives oldest-first.
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}

	if len(messages) > 0 {
		client.SendJSON("chat:history", map[string]interface{}{
			"messages": messages,
		})
	}
}

// handleMessage handles incoming WebSocket messages
func (h *WebSocketHandler) handleMessage(client *websocket.Client, msg websocket.Message) {
	switch msg.Type {
	case "chat:send":
		metrics.Get().TotalMessagesChat.Add(1)
		h.handleChatSend(client, msg.Payload)
	case "chat:file":
		metrics.Get().TotalMessagesChat.Add(1)
		h.handleChatFile(client, msg.Payload)
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
	case "signal:resync":
		h.handleResync(client)
	case "signal:resubscribe":
		h.handleResubscribe(client)
	case "client:debug":
		h.handleClientDebug(client, msg.Payload)
	case "signal:ice-servers-request":
		h.handleICEServersRequest(client)
	// Admin commands
	case "admin:mute":
		h.handleAdminMute(client, msg.Payload)
	case "admin:delete-message":
		h.handleAdminDeleteMessage(client, msg.Payload)
	case "admin:kick":
		h.handleAdminKick(client, msg.Payload)
	case "admin:end-session":
		h.handleAdminEndSession(client)
	// Waiting room (in-session approval popups)
	case "admin:waiting-approve":
		h.handleWaitingApprove(client, msg.Payload)
	case "admin:waiting-deny":
		h.handleWaitingDeny(client, msg.Payload)
	// Screen sharing
	case "screenshare:request":
		h.handleScreenShareRequest(client)
	case "admin:screenshare-approve":
		h.handleScreenShareApprove(client, msg.Payload)
	case "admin:screenshare-deny":
		h.handleScreenShareDeny(client, msg.Payload)
	case "admin:screenshare-revoke":
		h.handleScreenShareRevoke(client, msg.Payload)
	case "screenshare:stop":
		h.handleScreenShareStop(client)
	default:
		logger.Debug("Unknown message type", "type", msg.Type, "participant_id", client.ID)
	}
}

func (h *WebSocketHandler) handleChatSend(client *websocket.Client, payload json.RawMessage) {
	// Check chat rate limit: 30 messages per minute
	if !client.AllowChatMessage() {
		logger.Debug("Chat rate limit exceeded", "participant_id", client.ID, "room", client.RoomSlug)
		return
	}

	var data struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(payload, &data); err != nil {
		return
	}

	sanitizedContent := sanitizeText(data.Content)
	if len(sanitizedContent) == 0 || len(sanitizedContent) > 2000 {
		return
	}

	now := time.Now()
	msgID := generateID()

	// Persist to database
	h.db.Exec(`INSERT INTO messages (id, room_id, participant_id, type, content, created_at)
		SELECT ?, r.id, ?, 'text', ?, ? FROM rooms r WHERE r.slug = ?`,
		msgID, client.ID, sanitizedContent, now, client.RoomSlug)

	// Broadcast to all in room
	h.hub.BroadcastJSON(client.RoomSlug, "chat:message", map[string]interface{}{
		"id":              msgID,
		"participantId":   client.ID,
		"participantName": sanitizeText(client.Name),
		"type":            "text",
		"content":         sanitizedContent,
		"timestamp":       now.UnixMilli(),
	}, "")
}

func (h *WebSocketHandler) handleChatFile(client *websocket.Client, payload json.RawMessage) {
	// Apply the same rate limit as chat messages
	if !client.AllowChatMessage() {
		logger.Debug("Chat rate limit exceeded", "participant_id", client.ID, "room", client.RoomSlug)
		return
	}

	var data struct {
		FileID string `json:"fileId"`
	}
	if err := json.Unmarshal(payload, &data); err != nil {
		return
	}
	if data.FileID == "" {
		return
	}

	var (
		fileID       string
		originalName string
		mimeType     string
		roomSlug     string
	)

	err := h.db.QueryRow(`
		SELECT f.id, f.original_name, f.mime_type, r.slug
		FROM files f
		JOIN rooms r ON r.id = f.room_id
		WHERE f.id = ?
	`, data.FileID).Scan(&fileID, &originalName, &mimeType, &roomSlug)
	if err != nil {
		return
	}

	if roomSlug != client.RoomSlug {
		return
	}

	filePayload := map[string]interface{}{
		"id":       fileID,
		"name":     originalName,
		"mimeType": mimeType,
		"url":      fmt.Sprintf("/api/files/%s", fileID),
	}

	if strings.HasPrefix(mimeType, "image/") {
		filePayload["thumbnailUrl"] = fmt.Sprintf("/api/files/%s/thumbnail", fileID)
	}

	now := time.Now()
	msgID := generateID()

	// Persist to database (store file reference as JSON content)
	fileJSON, _ := json.Marshal(filePayload)
	h.db.Exec(`INSERT INTO messages (id, room_id, participant_id, type, content, created_at)
		SELECT ?, r.id, ?, 'file', ?, ? FROM rooms r WHERE r.slug = ?`,
		msgID, client.ID, string(fileJSON), now, client.RoomSlug)

	h.hub.BroadcastJSON(client.RoomSlug, "chat:message", map[string]interface{}{
		"id":              msgID,
		"participantId":   client.ID,
		"participantName": sanitizeText(client.Name),
		"type":            "file",
		"content":         "",
		"file":            filePayload,
		"timestamp":       now.UnixMilli(),
	}, "")
}

// maxCursorBatchPoints caps the number of position samples accepted per
// cursor message. Clients batch coalesced pointer samples into one message
// per ~33ms send tick and subsample to this cap themselves; anything beyond
// it is dropped server-side. Must match MAX_BATCH_POINTS in laser.ts.
const maxCursorBatchPoints = 20

func (h *WebSocketHandler) handleCursor(client *websocket.Client, payload json.RawMessage) {
	if !client.AllowCursor() {
		return
	}

	type cursorPoint struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	}
	var data struct {
		// Batched format: dense coalesced pointer samples for one send tick.
		Points []cursorPoint `json:"points"`
		// Legacy single-point format (used when Points is absent).
		X       *float64 `json:"x"`
		Y       *float64 `json:"y"`
		Active  bool     `json:"active"`
		Release bool     `json:"release"`
		// Which video the pointer is on: "video" (main stream, default)
		// or "share" (screen-share pane).
		Surface string `json:"surface"`
	}
	if err := json.Unmarshal(payload, &data); err != nil {
		return
	}
	if data.Surface != "share" {
		data.Surface = "video"
	}

	raw := data.Points
	if len(raw) == 0 {
		// Backward compatibility: single {x,y} becomes a one-point batch.
		if data.X == nil || data.Y == nil {
			return
		}
		raw = []cursorPoint{{X: *data.X, Y: *data.Y}}
	}
	// Cap batch length (drop extras beyond the cap).
	if len(raw) > maxCursorBatchPoints {
		raw = raw[:maxCursorBatchPoints]
	}

	// Validate every point: reject NaN/Inf samples, clamp the rest to [0,1].
	points := make([]map[string]interface{}, 0, len(raw))
	var lastX, lastY float64
	for _, p := range raw {
		if math.IsNaN(p.X) || math.IsNaN(p.Y) || math.IsInf(p.X, 0) || math.IsInf(p.Y, 0) {
			continue
		}
		x := math.Min(1, math.Max(0, p.X))
		y := math.Min(1, math.Max(0, p.Y))
		points = append(points, map[string]interface{}{"x": x, "y": y})
		lastX, lastY = x, y
	}
	if len(points) == 0 {
		return
	}

	// Broadcast the batched shape to all. Top-level x/y mirror the newest
	// point for backward compatibility with single-point consumers.
	h.hub.BroadcastJSON(client.RoomSlug, "cursor", map[string]interface{}{
		"participantId":   client.ID,
		"participantName": client.Name,
		"color":           client.Color,
		"points":          points,
		"x":               lastX,
		"y":               lastY,
		"active":          data.Active,
		"release":         data.Release && !data.Active,
		"surface":         data.Surface,
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
		client.SetAudioEnabled(*data.Audio)
	}
	if data.Video != nil {
		client.SetVideoEnabled(*data.Video)
	}

	// Notify others
	h.hub.BroadcastJSON(client.RoomSlug, "participant:updated", map[string]interface{}{
		"participant": map[string]interface{}{
			"id":           client.ID,
			"audioEnabled": client.AudioEnabled(),
			"videoEnabled": client.VideoEnabled(),
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

	// Apply the offer to the existing subscriber connection and create an answer.
	answer, rolledBack, err := h.sfu.HandleSubscriberOffer(client.RoomSlug, client.ID, data.SDP)

	if err != nil {
		logger.Error("Failed to handle voice offer", "participant_id", client.ID, "error", err)
		return
	}

	// Send answer back to client
	client.SendJSON("signal:voice-answer", map[string]interface{}{
		"sdp": answer,
	})

	logger.Debug("Sent voice answer", "participant_id", client.ID, "rolled_back", rolledBack)

	// Flush any deferred renegotiation: tracks attached mid-negotiation or
	// from a rolled-back server offer get their follow-up offer now, after
	// the answer above, preserving message order on the websocket.
	h.sfu.FlushPendingRenegotiation(client.RoomSlug, client.ID)
}

// forwardVoiceTrack forwards a participant's voice track to all other
// participants in the room. On first arrival for a given speaker it creates
// a shared relay local track and fans it out to every subscriber with a
// renegotiation. On subsequent arrivals (speaker rejoins) the relay track is
// reused — subscribers' existing senders are already bound to it, so no fan-
// out or renegotiation is needed; we only rebind the forwarding goroutine.
func (h *WebSocketHandler) forwardVoiceTrack(roomSlug, participantID string, track *pionwebrtc.TrackRemote) {
	logger.Debug("Forwarding voice track", "participant_id", participantID, "room", roomSlug)

	h.sfu.StoreVoiceRemoteTrack(roomSlug, participantID, track)

	relayTrack, isNew, err := h.sfu.CreateVoiceRelayTrack(roomSlug, participantID, track)
	if err != nil {
		logger.Error("Failed to create voice relay track", "participant_id", participantID, "error", err)
		return
	}

	if isNew {
		// First time seeing this speaker — AddTrack on every subscriber and
		// trigger a renegotiation so the browser creates a receiver for it.
		h.forwardVoiceTrackToClients(roomSlug, participantID, relayTrack, "")
		// A fresh renegotiation can stall Firefox's video decoder briefly;
		// a PLI nudges it to recover immediately.
		h.sfu.RequestKeyframe(roomSlug)
	}
}

// forwardVoiceTrackToClients adds a shared voice relay track to clients in
// the room. If targetClientID is non-empty, only that specific client
// receives the track. Each subscriber is handled in its own goroutine so a
// slow / renegotiating subscriber doesn't block voice fan-out to everyone
// else (small rooms, 2–8 viewers per the product spec).
func (h *WebSocketHandler) forwardVoiceTrackToClients(roomSlug, voiceOwnerID string, localTrack *pionwebrtc.TrackLocalStaticRTP, targetClientID string) {
	clients := h.hub.GetRoomClients(roomSlug)
	for _, client := range clients {
		if client.ID == voiceOwnerID {
			continue // Don't send to self
		}
		if targetClientID != "" && client.ID != targetClientID {
			continue
		}

		go func(c *websocket.Client) {
			offerSDP, err := h.sfu.AddVoiceTrackToSubscriber(roomSlug, c.ID, voiceOwnerID, localTrack)
			if err != nil {
				logger.Warn("Failed to add voice track to subscriber", "subscriber_id", c.ID, "source_id", voiceOwnerID, "error", err)
				return
			}
			if offerSDP == "" {
				// Track attached/queued mid-negotiation; the follow-up offer is
				// pushed via the subscriber's renegotiation callback once the
				// in-flight exchange settles.
				return
			}

			c.SendJSON("signal:renegotiate", map[string]interface{}{
				"sdp":           offerSDP,
				"participantId": voiceOwnerID,
			})

			logger.Debug("Sent renegotiation offer for voice track", "subscriber_id", c.ID, "source_id", voiceOwnerID)
		}(client)
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

	// Signaling is stable again — push any renegotiation that was deferred
	// while this offer/answer exchange was in flight (e.g. voice tracks).
	h.sfu.FlushPendingRenegotiation(client.RoomSlug, client.ID)
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

	// If a server offer was rolled back to accept this restart, its tracks
	// still need a follow-up offer — flush after the answer is sent.
	h.sfu.FlushPendingRenegotiation(client.RoomSlug, client.ID)
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

	// Chain any renegotiation that queued up while this one was in flight
	// (e.g. two speakers joined voice back-to-back).
	h.sfu.FlushPendingRenegotiation(client.RoomSlug, client.ID)
}

// handleICEServersRequest returns a fresh set of ICE servers (including
// refreshed Cloudflare TURN credentials). Long sessions (4–8 h for color
// grading) outlive the 1 h default Cloudflare TTL; clients periodically
// request fresh creds so that any ICE restart that happens later gathers
// with valid credentials instead of hanging.
func (h *WebSocketHandler) handleICEServersRequest(client *websocket.Client) {
	servers := h.sfu.GetICEServers()
	client.SendJSON("signal:ice-servers", map[string]interface{}{
		"iceServers": servers,
	})
	logger.Debug("Sent refreshed ICE servers", "participant_id", client.ID, "count", len(servers))
}

func (h *WebSocketHandler) handleResync(client *websocket.Client) {
	// No-op when the room has no live ingest — PLI to a nonexistent receiver
	// is wasted work, and avoids log spam if a client spams resync while the
	// publisher is offline.
	if !h.sfu.IsRoomLive(client.RoomSlug) {
		logger.Debug("Ignoring resync request for inactive room", "participant_id", client.ID, "room", client.RoomSlug)
		return
	}
	logger.Debug("Processing resync/keyframe request", "participant_id", client.ID, "room", client.RoomSlug)
	h.sfu.RequestKeyframe(client.RoomSlug)
}

// requireAdmin returns true if the client is still a valid admin right now.
// It re-checks the session cookie captured at upgrade time so a mid-session
// logout immediately revokes admin powers. Callers should log & return on false.
func (h *WebSocketHandler) requireAdmin(client *websocket.Client, action string) bool {
	if !client.IsAdmin {
		return false
	}
	if client.AdminSessionID != "" && h.validateSession != nil && !h.validateSession(client.AdminSessionID) {
		logger.Warn("Admin session revoked mid-connection; denying action",
			"action", action, "participant_id", client.ID, "room", client.RoomSlug)
		client.IsAdmin = false
		client.AdminSessionID = ""
		return false
	}
	return true
}

// handleAdminDeleteMessage removes a chat message from the persisted history
// (moderation) and tells every connected client to drop it from their
// transcript. File messages keep their uploaded file — admins manage those
// separately from the room settings Files section.
func (h *WebSocketHandler) handleAdminDeleteMessage(client *websocket.Client, payload json.RawMessage) {
	if !h.requireAdmin(client, "delete-message") {
		return
	}

	var data struct {
		MessageID string `json:"messageId"`
	}
	if err := json.Unmarshal(payload, &data); err != nil || data.MessageID == "" {
		return
	}

	res, err := h.db.Exec(`DELETE FROM messages
		WHERE id = ? AND room_id = (SELECT id FROM rooms WHERE slug = ?)`,
		data.MessageID, client.RoomSlug)
	if err != nil {
		logger.Error("Failed to delete chat message", "message_id", data.MessageID, "room", client.RoomSlug, "error", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return
	}

	h.hub.BroadcastJSON(client.RoomSlug, "chat:message-deleted", map[string]interface{}{
		"id": data.MessageID,
	}, "")
	logger.Info("Chat message deleted by admin", "message_id", data.MessageID, "room", client.RoomSlug, "admin_id", client.ID)
}

func (h *WebSocketHandler) handleAdminMute(client *websocket.Client, payload json.RawMessage) {
	if !h.requireAdmin(client, "mute") {
		return
	}

	var data struct {
		ParticipantID string `json:"participantId"`
	}
	if err := json.Unmarshal(payload, &data); err != nil {
		return
	}

	// Gate voice RTP at the server — admin:muted as a broadcast-only hint is
	// advisory, and a malicious client could simply ignore it and keep
	// sending. Flipping the relay's mute flag drops the packets regardless
	// of what the client chooses to do.
	h.sfu.SetVoiceMuted(client.RoomSlug, data.ParticipantID, true)

	h.hub.BroadcastJSON(client.RoomSlug, "admin:muted", map[string]interface{}{
		"participantId": data.ParticipantID,
	}, "")
	logger.Info("Admin mute", "by", client.ID, "target", data.ParticipantID, "room", client.RoomSlug)
}

func (h *WebSocketHandler) handleAdminKick(client *websocket.Client, payload json.RawMessage) {
	if !h.requireAdmin(client, "kick") {
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

	// Force disconnect — closing the connection triggers ReadPump exit,
	// which fires onDisconnect and broadcasts participant:left.
	if target := h.hub.GetClient(client.RoomSlug, data.ParticipantID); target != nil {
		target.Conn.Close()
	}

	logger.Info("Admin kick", "by", client.ID, "target", data.ParticipantID, "room", client.RoomSlug)
}

func (h *WebSocketHandler) handleAdminEndSession(client *websocket.Client) {
	if !h.requireAdmin(client, "end-session") {
		return
	}

	// Mark room as ended in the database
	_, err := h.db.Exec(`
		UPDATE rooms SET status = 'ended', ended_at = ?
		WHERE slug = ? AND status != 'ended'
	`, time.Now(), client.RoomSlug)
	if err != nil {
		logger.Error("Failed to end session", "room", client.RoomSlug, "error", err)
	}

	// Broadcast session end to all
	h.hub.BroadcastJSON(client.RoomSlug, "room:ended", map[string]interface{}{}, "")
}

// handleWaitingApprove admits a waiting participant from the in-session
// approval popup. Delegates to the same shared logic as the REST admit
// endpoint (DB update + SSE notification + waiting:resolved broadcast).
func (h *WebSocketHandler) handleWaitingApprove(client *websocket.Client, payload json.RawMessage) {
	if !h.requireAdmin(client, "waiting-approve") {
		return
	}
	if h.waitingActions == nil {
		return
	}

	var data struct {
		ParticipantID string `json:"participantId"`
	}
	if err := json.Unmarshal(payload, &data); err != nil || data.ParticipantID == "" {
		return
	}

	found, err := h.waitingActions.AdmitWaitingParticipant(client.RoomSlug, data.ParticipantID)
	if err != nil {
		logger.Error("Failed to admit waiting participant", "participant_id", data.ParticipantID, "admin", client.ID, "room", client.RoomSlug, "error", err)
		return
	}
	if !found {
		logger.Debug("Waiting participant already resolved", "participant_id", data.ParticipantID, "room", client.RoomSlug)
		return
	}
	logger.Info("Waiting participant approved", "participant_id", data.ParticipantID, "admin", client.ID, "room", client.RoomSlug)
}

// handleWaitingDeny denies a waiting participant from the in-session approval
// popup, reusing the shared REST deny logic.
func (h *WebSocketHandler) handleWaitingDeny(client *websocket.Client, payload json.RawMessage) {
	if !h.requireAdmin(client, "waiting-deny") {
		return
	}
	if h.waitingActions == nil {
		return
	}

	var data struct {
		ParticipantID string `json:"participantId"`
	}
	if err := json.Unmarshal(payload, &data); err != nil || data.ParticipantID == "" {
		return
	}

	found, err := h.waitingActions.DenyWaitingParticipant(client.RoomSlug, data.ParticipantID)
	if err != nil {
		logger.Error("Failed to deny waiting participant", "participant_id", data.ParticipantID, "admin", client.ID, "room", client.RoomSlug, "error", err)
		return
	}
	if !found {
		logger.Debug("Waiting participant already resolved", "participant_id", data.ParticipantID, "room", client.RoomSlug)
		return
	}
	logger.Info("Waiting participant denied", "participant_id", data.ParticipantID, "admin", client.ID, "room", client.RoomSlug)
}

// handleScreenShareRequest processes a guest's request to share their screen
func (h *WebSocketHandler) handleScreenShareRequest(client *websocket.Client) {
	roomTracks := h.sfu.GetRoomTracksForSlug(client.RoomSlug)
	if roomTracks != nil {
		roomTracks.RLockVoiceTracks()
		active := roomTracks.ScreenShareParticipantID
		roomTracks.RUnlockVoiceTracks()
		if active != "" {
			// Already an active screen share
			client.SendJSON("screenshare:denied", map[string]interface{}{
				"reason": "Another participant is already sharing their screen",
			})
			return
		}
	}

	// Admin auto-approves their own request
	if client.IsAdmin {
		client.SendJSON("screenshare:approved", map[string]interface{}{})
		return
	}

	// Previously-approved participants skip the admin prompt entirely:
	// approval is persistent (can_screenshare) until explicitly revoked.
	if h.participantCanScreenshare(client.RoomSlug, client.ID) {
		client.SendJSON("screenshare:approved", map[string]interface{}{})
		logger.Debug("Screen share auto-approved (persistent approval)", "participant_id", client.ID, "room", client.RoomSlug)
		return
	}

	// Find admin(s) in the room and send pending request
	clients := h.hub.GetRoomClients(client.RoomSlug)
	for _, c := range clients {
		if c.IsAdmin {
			c.SendJSON("screenshare:pending", map[string]interface{}{
				"participantId": client.ID,
				"name":          client.Name,
			})
		}
	}
	logger.Debug("Screen share request sent to admin", "participant_id", client.ID, "room", client.RoomSlug)
}

// participantCanScreenshare reports whether the participant holds a
// persistent screen share approval.
func (h *WebSocketHandler) participantCanScreenshare(roomSlug, participantID string) bool {
	var allowed bool
	err := h.db.QueryRow(`
		SELECT COALESCE(p.can_screenshare, FALSE)
		FROM participants p
		JOIN rooms r ON r.id = p.room_id
		WHERE p.id = ? AND r.slug = ?
	`, participantID, roomSlug).Scan(&allowed)
	return err == nil && allowed
}

// setParticipantScreenshare persists the screen share approval flag and
// broadcasts the updated participant state so admin UIs stay in sync.
func (h *WebSocketHandler) setParticipantScreenshare(roomSlug, participantID string, allowed bool) {
	if _, err := h.db.Exec(`
		UPDATE participants SET can_screenshare = ?
		WHERE id = ? AND room_id = (SELECT id FROM rooms WHERE slug = ?)
	`, allowed, participantID, roomSlug); err != nil {
		logger.Error("Failed to persist screen share approval", "participant_id", participantID, "room", roomSlug, "error", err)
		return
	}
	h.hub.BroadcastJSON(roomSlug, "participant:updated", map[string]interface{}{
		"participant": map[string]interface{}{
			"id":             participantID,
			"canScreenshare": allowed,
		},
	}, "")
}

// handleScreenShareApprove processes admin approval of a screen share request
func (h *WebSocketHandler) handleScreenShareApprove(client *websocket.Client, payload json.RawMessage) {
	if !h.requireAdmin(client, "screenshare-approve") {
		return
	}

	var data struct {
		ParticipantID string `json:"participantId"`
	}
	if err := json.Unmarshal(payload, &data); err != nil {
		return
	}

	// Persist the approval: once granted it sticks until explicitly revoked,
	// so the participant never has to re-prompt the admin.
	h.setParticipantScreenshare(client.RoomSlug, data.ParticipantID, true)

	// Only trigger an immediate capture when nobody else is sharing — the
	// permission itself is recorded either way.
	roomTracks := h.sfu.GetRoomTracksForSlug(client.RoomSlug)
	if roomTracks != nil {
		roomTracks.RLockVoiceTracks()
		active := roomTracks.ScreenShareParticipantID
		roomTracks.RUnlockVoiceTracks()
		if active != "" {
			return
		}
	}

	// Send approval to the requester
	h.hub.SendToJSON(client.RoomSlug, data.ParticipantID, "screenshare:approved", map[string]interface{}{})
	logger.Info("Screen share approved", "participant_id", data.ParticipantID, "admin", client.ID, "room", client.RoomSlug)
}

// handleScreenShareRevoke clears a participant's persistent screen share
// approval and stops their active share, if any.
func (h *WebSocketHandler) handleScreenShareRevoke(client *websocket.Client, payload json.RawMessage) {
	if !h.requireAdmin(client, "screenshare-revoke") {
		return
	}

	var data struct {
		ParticipantID string `json:"participantId"`
	}
	if err := json.Unmarshal(payload, &data); err != nil {
		return
	}

	h.setParticipantScreenshare(client.RoomSlug, data.ParticipantID, false)

	// If the participant is currently sharing, tear the share down.
	roomTracks := h.sfu.GetRoomTracksForSlug(client.RoomSlug)
	if roomTracks != nil {
		roomTracks.RLockVoiceTracks()
		sharerID := roomTracks.ScreenShareParticipantID
		roomTracks.RUnlockVoiceTracks()
		if sharerID == data.ParticipantID {
			affected := h.sfu.RemoveScreenShareTrack(client.RoomSlug)
			h.hub.BroadcastJSON(client.RoomSlug, "screenshare:stopped", map[string]interface{}{}, "")
			for _, subID := range affected {
				go h.renegotiateSubscriber(client.RoomSlug, subID)
			}
		}
	}

	// Clear any pending request UI on the target.
	h.hub.SendToJSON(client.RoomSlug, data.ParticipantID, "screenshare:denied", map[string]interface{}{
		"reason": "Screen share permission was revoked",
	})
	logger.Info("Screen share permission revoked", "participant_id", data.ParticipantID, "admin", client.ID, "room", client.RoomSlug)
}

// handleScreenShareDeny processes admin denial of a screen share request
func (h *WebSocketHandler) handleScreenShareDeny(client *websocket.Client, payload json.RawMessage) {
	if !h.requireAdmin(client, "screenshare-deny") {
		return
	}

	var data struct {
		ParticipantID string `json:"participantId"`
	}
	if err := json.Unmarshal(payload, &data); err != nil {
		return
	}

	h.hub.SendToJSON(client.RoomSlug, data.ParticipantID, "screenshare:denied", map[string]interface{}{})
	logger.Info("Screen share denied", "participant_id", data.ParticipantID, "admin", client.ID, "room", client.RoomSlug)
}

// handleScreenShareStop processes a request to stop screen sharing
func (h *WebSocketHandler) handleScreenShareStop(client *websocket.Client) {
	roomTracks := h.sfu.GetRoomTracksForSlug(client.RoomSlug)
	if roomTracks == nil {
		return
	}

	// Only the sharer or admin can stop
	roomTracks.RLockVoiceTracks()
	sharerID := roomTracks.ScreenShareParticipantID
	roomTracks.RUnlockVoiceTracks()

	if sharerID == "" {
		return
	}
	if client.ID != sharerID && !client.IsAdmin {
		return
	}

	affected := h.sfu.RemoveScreenShareTrack(client.RoomSlug)
	h.hub.BroadcastJSON(client.RoomSlug, "screenshare:stopped", map[string]interface{}{}, "")

	// Renegotiate affected subscribers to remove the track
	for _, subID := range affected {
		go h.renegotiateSubscriber(client.RoomSlug, subID)
	}

	logger.Info("Screen share stopped", "stopped_by", client.ID, "sharer", sharerID, "room", client.RoomSlug)
}

// forwardScreenShareTrack forwards a participant's screen share track to all other participants.
// Modeled on forwardVoiceTrack — creates a single relay track for fan-out.
func (h *WebSocketHandler) forwardScreenShareTrack(roomSlug, participantID string, track *pionwebrtc.TrackRemote) {
	logger.Debug("Forwarding screen share track", "participant_id", participantID, "room", roomSlug)

	relayTrack, err := h.sfu.CreateScreenShareRelayTrack(roomSlug, participantID, track)
	if err != nil {
		logger.Error("Failed to create screen share relay track", "participant_id", participantID, "error", err)
		return
	}

	// Add the relay track to all other subscribers
	clients := h.hub.GetRoomClients(roomSlug)
	for _, client := range clients {
		if client.ID == participantID {
			continue
		}

		offerSDP, err := h.sfu.AddScreenShareTrackToSubscriber(roomSlug, client.ID, participantID, relayTrack)
		if err != nil {
			logger.Warn("Failed to add screen share track to subscriber", "subscriber_id", client.ID, "source_id", participantID, "error", err)
			continue
		}
		if offerSDP == "" {
			// Attached/queued mid-negotiation; offer pushed via the
			// subscriber's renegotiation callback when signaling settles.
			continue
		}

		client.SendJSON("signal:renegotiate", map[string]interface{}{
			"sdp": offerSDP,
		})
	}

	// Broadcast that screen share has started
	sharerName := participantID
	for _, c := range clients {
		if c.ID == participantID {
			sharerName = c.Name
			break
		}
	}
	h.hub.BroadcastJSON(roomSlug, "screenshare:started", map[string]interface{}{
		"participantId": participantID,
		"name":          sharerName,
	}, "")

	// Request keyframes: main stream (renegotiation may disrupt browser decoders)
	// and screen share (so subscribers can decode immediately).
	h.sfu.RequestKeyframe(roomSlug)
	h.sfu.RequestScreenShareKeyframe(roomSlug)

	logger.Info("Screen share track forwarded", "participant_id", participantID, "room", roomSlug)
}

// renegotiateSubscriber creates a renegotiation offer for a subscriber and sends it
func (h *WebSocketHandler) renegotiateSubscriber(roomSlug, subscriberID string) {
	offerSDP, err := h.sfu.RenegotiateSubscriber(roomSlug, subscriberID)
	if err != nil {
		logger.Warn("Failed to renegotiate subscriber", "subscriber_id", subscriberID, "error", err)
		return
	}

	client := h.hub.GetClient(roomSlug, subscriberID)
	if client != nil {
		client.SendJSON("signal:renegotiate", map[string]interface{}{
			"sdp": offerSDP,
		})
	}
}
