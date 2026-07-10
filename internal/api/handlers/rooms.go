package handlers

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"chromatic/internal/config"
	"chromatic/internal/database"
	"chromatic/internal/logger"
	"chromatic/internal/metrics"
	"chromatic/internal/models"

	"golang.org/x/crypto/bcrypt"
)

var slugRegex = regexp.MustCompile(`^[a-z0-9-]{3,64}$`)

// Lobby window defaults/bounds for scheduled rooms: guests may enter the
// countdown lobby this many minutes before scheduled_at (per-room override
// via rooms.early_open_minutes).
const (
	defaultEarlyOpenMinutes = 10
	maxEarlyOpenMinutes     = 120
	joinReservationWindow   = 2 * time.Minute
)

// maxWaitingSubscriptions caps concurrent waiting-room SSE connections to
// prevent file-descriptor exhaustion from abusive clients.
const maxWaitingSubscriptions = 100

// WaitingSubscription represents a waiting room participant's SSE connection
type WaitingSubscription struct {
	ParticipantID string
	Channel       chan string // "admitted" or "ended"
}

// WaitingManager tracks waiting room SSE subscriptions
type WaitingManager struct {
	mu            sync.RWMutex
	subscriptions map[string]*WaitingSubscription // key: participantID
}

// NewWaitingManager creates a new waiting manager
func NewWaitingManager() *WaitingManager {
	return &WaitingManager{
		subscriptions: make(map[string]*WaitingSubscription),
	}
}

// Subscribe adds a participant's SSE subscription.
// Returns false if the global subscription cap has been reached.
func (m *WaitingManager) Subscribe(participantID string) (chan string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Re-subscribing the same participant replaces the old subscription and
	// does not count against the cap.
	if existing, ok := m.subscriptions[participantID]; ok {
		close(existing.Channel)
		delete(m.subscriptions, participantID)
	}

	if len(m.subscriptions) >= maxWaitingSubscriptions {
		return nil, false
	}

	ch := make(chan string, 1)
	m.subscriptions[participantID] = &WaitingSubscription{
		ParticipantID: participantID,
		Channel:       ch,
	}
	return ch, true
}

// Unsubscribe removes a participant's SSE subscription
func (m *WaitingManager) Unsubscribe(participantID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if sub, ok := m.subscriptions[participantID]; ok {
		close(sub.Channel)
		delete(m.subscriptions, participantID)
	}
}

// NotifyAdmitted sends admission notification to a participant
func (m *WaitingManager) NotifyAdmitted(participantID string) {
	m.NotifyEvent([]string{participantID}, "admitted")
}

// NotifyAllAdmitted sends admission notification to all waiting participants
func (m *WaitingManager) NotifyAllAdmitted(participantIDs []string) {
	m.NotifyEvent(participantIDs, "admitted")
}

// NotifyEvent pushes an arbitrary SSE event ("admitted", "denied", "open",
// "ended") to each of the given participants' waiting-room subscriptions.
func (m *WaitingManager) NotifyEvent(participantIDs []string, event string) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, id := range participantIDs {
		if sub, ok := m.subscriptions[id]; ok {
			select {
			case sub.Channel <- event:
			default:
				// Channel full or closed, skip
			}
		}
	}
}

// RoomHandler handles room-related HTTP requests
type RoomHandler struct {
	db  *database.DB
	sfu interface {
		BindIngestToRoom(streamKeyToken, roomSlug string) ([]string, error)
		RenegotiateSubscriber(roomSlug, subscriberID string) (string, string, error)
		CloseRoom(roomSlug string)
	}
	hub interface {
		BroadcastJSON(roomSlug string, msgType string, payload interface{}, excludeID string) error
		SendToJSON(roomSlug, clientID, msgType string, payload interface{}) error
		BroadcastToAdminsJSON(roomSlug, msgType string, payload interface{}) error
		RoomClientCount(roomSlug string) int
		CloseRoom(roomSlug string)
	}
	onRoomLive          func(roomSlug string) // Called when room goes live
	tokenManager        *TokenManager         // For generating signed WebSocket tokens
	waitingManager      *WaitingManager       // For waiting room SSE notifications
	adminToken          string                // For validating admin joins (constant-time compared)
	validateSession     SessionValidator      // Admin session cookie validation (join-as-host)
	maxParticipants     int                   // Cap on non-ended participants per room
	uploadPath          string                // Root of per-room upload dirs (for delete-with-files)
	obsReconnectTimeout time.Duration
	productionMode      bool
	joinMu              sync.Mutex
	timerMu             sync.Mutex
	streamEndTimers     map[string]*time.Timer
	// openTimers fire when a scheduled room reaches scheduled_at so lobby
	// participants are auto-admitted without any polling. One timer per room,
	// armed lazily when the first lobby participant joins.
	openTimers map[string]*time.Timer
}

// NewRoomHandler creates a new RoomHandler
func NewRoomHandler(db *database.DB, cfg *config.Config, tokenSecret []byte) *RoomHandler {
	maxParticipants := cfg.MaxParticipantsPerRoom
	if maxParticipants <= 0 {
		maxParticipants = 20
	}
	return &RoomHandler{
		db:                  db,
		tokenManager:        NewTokenManager(tokenSecret),
		waitingManager:      NewWaitingManager(),
		adminToken:          cfg.AdminToken,
		maxParticipants:     maxParticipants,
		uploadPath:          cfg.UploadPath,
		obsReconnectTimeout: cfg.OBSReconnectTimeout,
		productionMode:      cfg.ProductionMode,
		streamEndTimers:     make(map[string]*time.Timer),
		openTimers:          make(map[string]*time.Timer),
	}
}

// SetSessionValidator wires the admin session cookie validator so Join can
// grant the admin role to requests carrying a valid admin session cookie
// (same validator the WebSocket handler uses).
func (h *RoomHandler) SetSessionValidator(validator SessionValidator) {
	h.validateSession = validator
}

// hasAdminSession reports whether the request carries a valid admin session
// cookie (set by POST /api/auth/login).
func (h *RoomHandler) hasAdminSession(r *http.Request) bool {
	if h.validateSession == nil {
		return false
	}
	c, err := r.Cookie(SessionCookieName)
	return err == nil && c.Value != "" && h.validateSession(c.Value)
}

func joinTokenFromRequest(r *http.Request, roomSlug string, allowQuery bool) string {
	if token := r.Header.Get("X-Join-Token"); token != "" {
		return token
	}
	if c, err := r.Cookie(JoinTokenCookieName(roomSlug)); err == nil && c.Value != "" {
		return c.Value
	}
	if allowQuery {
		return r.URL.Query().Get("token")
	}
	return ""
}

// SetSFU sets the SFU reference (for stream binding)
func (h *RoomHandler) SetSFU(sfu interface {
	BindIngestToRoom(streamKeyToken, roomSlug string) ([]string, error)
	RenegotiateSubscriber(roomSlug, subscriberID string) (string, string, error)
	CloseRoom(roomSlug string)
}) {
	h.sfu = sfu
}

// SetHub sets the WebSocket hub reference (for notifications)
func (h *RoomHandler) SetHub(hub interface {
	BroadcastJSON(roomSlug string, msgType string, payload interface{}, excludeID string) error
	SendToJSON(roomSlug, clientID, msgType string, payload interface{}) error
	BroadcastToAdminsJSON(roomSlug, msgType string, payload interface{}) error
	RoomClientCount(roomSlug string) int
	CloseRoom(roomSlug string)
}) {
	h.hub = hub
}

func (h *RoomHandler) closeRoomRuntime(slug string, notify bool) {
	h.cancelOpenTimer(slug)
	h.cancelStreamEndTimer(slug)

	if h.hub != nil && notify {
		_ = h.hub.BroadcastJSON(slug, "room:ended", map[string]interface{}{}, "")
	}
	if h.sfu != nil {
		h.sfu.CloseRoom(slug)
	}
	if h.hub != nil {
		h.hub.CloseRoom(slug)
	}
}

// SetOnRoomLive sets the callback for when a room goes live
func (h *RoomHandler) SetOnRoomLive(callback func(roomSlug string)) {
	h.onRoomLive = callback
}

func (h *RoomHandler) cancelStreamEndTimer(roomSlug string) {
	h.timerMu.Lock()
	defer h.timerMu.Unlock()

	if timer, ok := h.streamEndTimers[roomSlug]; ok {
		timer.Stop()
		delete(h.streamEndTimers, roomSlug)
	}
}

func (h *RoomHandler) scheduleStreamEnd(roomSlug string) {
	if h.obsReconnectTimeout <= 0 {
		return
	}

	h.timerMu.Lock()
	if timer, ok := h.streamEndTimers[roomSlug]; ok {
		timer.Stop()
	}

	h.streamEndTimers[roomSlug] = time.AfterFunc(h.obsReconnectTimeout, func() {
		now := time.Now()
		ctx, cancel := database.WithTimeout(context.Background())
		result, err := h.db.ExecContext(ctx, `
			UPDATE rooms SET status = 'ended', ended_at = ?
			WHERE slug = ? AND status = 'live'
		`, now, roomSlug)
		cancel()
		if err != nil {
			logger.Error("Failed to end room after reconnect timeout", "room", roomSlug, "error", err)
			return
		}

		affected, _ := result.RowsAffected()
		if affected > 0 {
			h.closeRoomRuntime(roomSlug, true)
		}

		h.timerMu.Lock()
		delete(h.streamEndTimers, roomSlug)
		h.timerMu.Unlock()
	})

	h.timerMu.Unlock()
}

// ensureOpenTimer arms (at most one) timer per room that fires at
// scheduled_at and runs the room-open flow. Armed lazily when a lobby
// participant joins, so idle scheduled rooms cost nothing.
func (h *RoomHandler) ensureOpenTimer(slug string, scheduledAt time.Time) {
	h.timerMu.Lock()
	defer h.timerMu.Unlock()

	if _, ok := h.openTimers[slug]; ok {
		return
	}

	delay := time.Until(scheduledAt)
	if delay < 0 {
		delay = 0
	}
	h.openTimers[slug] = time.AfterFunc(delay, func() {
		h.timerMu.Lock()
		delete(h.openTimers, slug)
		h.timerMu.Unlock()
		h.handleRoomOpen(slug)
	})
}

func (h *RoomHandler) cancelOpenTimer(slug string) {
	h.timerMu.Lock()
	defer h.timerMu.Unlock()

	if timer, ok := h.openTimers[slug]; ok {
		timer.Stop()
		delete(h.openTimers, slug)
	}
}

// handleRoomOpen runs when a room opens (scheduled_at reached, an admin
// opened it early, or the first stream arrived):
//   - waiting_room_enabled=false: every lobby participant is auto-admitted —
//     the same DB update + SSE "admitted" notification a manual admit sends.
//   - waiting_room_enabled=true: lobby participants get an "open" SSE event
//     so their countdown crossfades into the approval-waiting state; admins
//     already received waiting:joined popups at join time.
func (h *RoomHandler) handleRoomOpen(slug string) {
	h.cancelOpenTimer(slug)

	var roomID string
	var waitingRoom bool
	var roomStatus string
	roomCtx, roomCancel := database.WithTimeout(context.Background())
	err := h.db.QueryRowContext(roomCtx, `
		SELECT id, waiting_room_enabled, status FROM rooms WHERE slug = ?
	`, slug).Scan(&roomID, &waitingRoom, &roomStatus)
	roomCancel()
	if err != nil || roomStatus == "ended" {
		return
	}

	waitCtx, waitCancel := database.WithTimeout(context.Background())
	rows, err := h.db.QueryContext(waitCtx, `SELECT id FROM participants WHERE room_id = ? AND is_admitted = FALSE`, roomID)
	if err != nil {
		waitCancel()
		logger.Error("Failed to list lobby participants on room open", "room", slug, "error", err)
		return
	}
	var waitingIDs []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			waitingIDs = append(waitingIDs, id)
		}
	}
	if err := rows.Err(); err != nil {
		logger.Error("Lobby participant iteration failed on room open", "room", slug, "error", err)
	}
	rows.Close()
	waitCancel()

	if len(waitingIDs) == 0 {
		return
	}

	if waitingRoom {
		// Switch lobby viewers to the normal approval flow. No admission here,
		// so the SELECT list above is the notification source of truth.
		h.waitingManager.NotifyEvent(waitingIDs, "open")
		logger.Info("Room opened; lobby switched to approval flow", "room", slug, "waiting", len(waitingIDs))
		return
	}

	// Auto-admit everyone in the lobby. Use UPDATE...RETURNING to admit and
	// capture the set atomically — the SELECT above ran in a separate statement,
	// so a participant joining between the SELECT and this UPDATE would be
	// admitted but absent from waitingIDs (no notification, metric drift).
	// RETURNING guarantees the notification list is exactly who we admitted.
	admitCtx, admitCancel := database.WithTimeout(context.Background())
	admitRows, qErr := h.db.QueryContext(admitCtx, `
		UPDATE participants SET is_admitted = TRUE WHERE room_id = ? AND is_admitted = FALSE
		RETURNING id
	`, roomID)
	if qErr != nil {
		admitCancel()
		logger.Error("Failed to auto-admit lobby participants", "room", slug, "error", qErr)
		return
	}
	admittedIDs := waitingIDs[:0:0]
	for admitRows.Next() {
		var id string
		if admitRows.Scan(&id) == nil {
			admittedIDs = append(admittedIDs, id)
		}
	}
	admitRows.Close()
	admitCancel()
	metrics.Get().WaitingParticipants.Add(-int64(len(admittedIDs)))
	h.waitingManager.NotifyAllAdmitted(admittedIDs)
	if h.hub != nil {
		h.hub.BroadcastToAdminsJSON(slug, "lobby:count", map[string]interface{}{"count": 0})
	}
	logger.Info("Room opened; lobby auto-admitted", "room", slug, "admitted", len(admittedIDs))
}

// maybeRunMissedOpen lazily recovers from a lost open timer (e.g. a server
// restart): for a scheduled room that should already be open but still has
// unadmitted lobby participants in a waiting-room-disabled room, run the open
// flow now; for one that opens in the future, re-arm the timer. Called from
// the lobby's SSE/status endpoints, so there is no global polling.
func (h *RoomHandler) maybeRunMissedOpen(slug string) {
	var roomID string
	var waitingRoom bool
	var scheduledAt, openedAt *time.Time
	var roomStatus string
	ctx, cancel := database.WithTimeout(context.Background())
	err := h.db.QueryRowContext(ctx, `
		SELECT id, waiting_room_enabled, scheduled_at, opened_at, status
		FROM rooms WHERE slug = ?
	`, slug).Scan(&roomID, &waitingRoom, &scheduledAt, &openedAt, &roomStatus)
	cancel()
	if err != nil || roomStatus == "ended" || scheduledAt == nil {
		return
	}
	if openedAt == nil && time.Now().Before(*scheduledAt) {
		// Not open yet — make sure the open timer survives restarts.
		h.ensureOpenTimer(slug, *scheduledAt)
		return
	}
	// Open already: only the auto-admit case needs recovery (waiting-room
	// rooms are in the normal approval flow and must not be re-notified).
	if !waitingRoom && h.countWaiting(roomID) > 0 {
		h.handleRoomOpen(slug)
	}
}

// OpenRoom (POST /api/rooms/{slug}/open, admin auth) opens a scheduled room
// ahead of time: sets opened_at and runs the auto-admission flow.
func (h *RoomHandler) OpenRoom(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")

	var roomStatus string
	var openedAt *time.Time
	getCtx, getCancel := database.WithTimeout(r.Context())
	err := h.db.QueryRowContext(getCtx, `SELECT status, opened_at FROM rooms WHERE slug = ?`, slug).Scan(&roomStatus, &openedAt)
	getCancel()
	if err != nil {
		http.Error(w, "Room not found", http.StatusNotFound)
		return
	}
	if roomStatus == "ended" {
		http.Error(w, "Room has ended", http.StatusGone)
		return
	}

	now := time.Now()
	if openedAt == nil {
		updCtx, updCancel := database.WithTimeout(r.Context())
		_, err := h.db.ExecContext(updCtx, `
			UPDATE rooms SET opened_at = ? WHERE slug = ? AND opened_at IS NULL
		`, now, slug)
		updCancel()
		if err != nil {
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}
		openedAt = &now
	}

	h.handleRoomOpen(slug)

	respondJSON(w, map[string]interface{}{
		"openedAt": openedAt.UTC(),
	})
}

// List returns all rooms with optional status filter
func (h *RoomHandler) List(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")

	query := `
		SELECT id, slug, name, scheduled_at, duration_minutes,
		       COALESCE(early_open_minutes, 10), opened_at,
		       password_hash IS NOT NULL as has_password, waiting_room_enabled,
		       stream_key_id, watermark_mode, watermark_text, watermark_logo_path,
		       watermark_logo_position, watermark_opacity,
		       watermark_pos_x, watermark_pos_y, COALESCE(watermark_scale, 1.0),
		       max_participants, status,
		       created_at, started_at, ended_at
		FROM rooms
	`
	args := []interface{}{}

	if status != "" {
		query += " WHERE status = ?"
		args = append(args, status)
	}

	query += " ORDER BY created_at DESC"

	listCtx, listCancel := database.WithTimeout(r.Context())
	rows, err := h.db.QueryContext(listCtx, query, args...)
	if err != nil {
		listCancel()
		logger.Error("Failed to list rooms", "error", err, "status_filter", status)
		http.Error(w, "Failed to retrieve rooms", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	defer listCancel()

	rooms := []models.Room{}
	for rows.Next() {
		var room models.Room
		err := rows.Scan(
			&room.ID, &room.Slug, &room.Name, &room.ScheduledAt, &room.DurationMinutes,
			&room.EarlyOpenMinutes, &room.OpenedAt,
			&room.HasPassword, &room.WaitingRoomEnabled, &room.StreamKeyID,
			&room.WatermarkMode, &room.WatermarkText, &room.WatermarkLogoPath,
			&room.WatermarkLogoPosition, &room.WatermarkOpacity,
			&room.WatermarkPosX, &room.WatermarkPosY, &room.WatermarkScale,
			&room.MaxParticipants, &room.Status,
			&room.CreatedAt, &room.StartedAt, &room.EndedAt,
		)
		if err != nil {
			logger.Error("Failed to scan room row", "error", err)
			http.Error(w, "Failed to retrieve rooms", http.StatusInternalServerError)
			return
		}
		rooms = append(rooms, room)
	}
	// rows.Next() returns false on both end-of-data and a mid-iteration error
	// (e.g. the query deadline firing); without this check a timeout would be
	// served as a silently truncated room list with a 200.
	if err := rows.Err(); err != nil {
		logger.Error("Room list iteration failed", "error", err)
		http.Error(w, "Failed to retrieve rooms", http.StatusInternalServerError)
		return
	}

	respondJSON(w, rooms)
}

// CreateRoomRequest is the request body for creating a room
type CreateRoomRequest struct {
	Slug                  string     `json:"slug"`
	Name                  string     `json:"name"`
	ScheduledAt           *time.Time `json:"scheduledAt,omitempty"`
	DurationMinutes       *int       `json:"durationMinutes,omitempty"`
	EarlyOpenMinutes      *int       `json:"earlyOpenMinutes,omitempty"`
	Password              *string    `json:"password,omitempty"`
	WaitingRoomEnabled    bool       `json:"waitingRoomEnabled"`
	StreamKeyID           *string    `json:"streamKeyId,omitempty"`
	WatermarkMode         string     `json:"watermarkMode"`
	WatermarkText         *string    `json:"watermarkText,omitempty"`
	WatermarkLogoPosition string     `json:"watermarkLogoPosition,omitempty"`
	WatermarkOpacity      *float64   `json:"watermarkOpacity,omitempty"`
	WatermarkPosX         *float64   `json:"watermarkPosX,omitempty"`
	WatermarkPosY         *float64   `json:"watermarkPosY,omitempty"`
	WatermarkScale        *float64   `json:"watermarkScale,omitempty"`
	MaxParticipants       *int       `json:"maxParticipants,omitempty"`
}

// clamp restricts v to the inclusive range [lo, hi].
func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Watermark scale bounds: 1.0 = default size, clamped to a sane range.
const (
	watermarkScaleMin = 0.25
	watermarkScaleMax = 3.0
)

// Create creates a new room
func (h *RoomHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req CreateRoomRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Validate slug
	if !slugRegex.MatchString(req.Slug) {
		http.Error(w, "Invalid slug format (lowercase letters, numbers, hyphens, 3-64 chars)", http.StatusBadRequest)
		return
	}

	// Validate and sanitize name
	req.Name = sanitizeText(req.Name)
	if len(req.Name) < 1 || len(req.Name) > 100 {
		http.Error(w, "Name must be 1-100 characters", http.StatusBadRequest)
		return
	}

	// Generate ID
	id, err := generateID()
	if err != nil {
		logger.Error("Failed to generate room ID", "error", err)
		http.Error(w, "Failed to create room", http.StatusInternalServerError)
		return
	}

	// Hash password if provided (minimum 8 characters for security)
	var passwordHash *string
	if req.Password != nil {
		if len(*req.Password) < 8 {
			http.Error(w, "Password must be at least 8 characters", http.StatusBadRequest)
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(*req.Password), bcrypt.DefaultCost)
		if err != nil {
			http.Error(w, "Failed to process password", http.StatusInternalServerError)
			return
		}
		hashStr := string(hash)
		passwordHash = &hashStr
	}

	// Default watermark mode
	if req.WatermarkMode == "" {
		req.WatermarkMode = "none"
	}
	if req.WatermarkMode != "none" && req.WatermarkMode != "text" && req.WatermarkMode != "logo" && req.WatermarkMode != "both" {
		http.Error(w, "Invalid watermark mode", http.StatusBadRequest)
		return
	}

	if req.WatermarkLogoPosition == "" {
		req.WatermarkLogoPosition = "bottom-right"
	}
	switch req.WatermarkLogoPosition {
	case "top-left", "top-right", "bottom-left", "bottom-right":
	default:
		http.Error(w, "Invalid watermark logo position", http.StatusBadRequest)
		return
	}

	watermarkOpacity := 0.3
	if req.WatermarkOpacity != nil {
		if *req.WatermarkOpacity < 0 || *req.WatermarkOpacity > 1 {
			http.Error(w, "Watermark opacity must be between 0 and 1", http.StatusBadRequest)
			return
		}
		watermarkOpacity = *req.WatermarkOpacity
	}

	if req.WatermarkText != nil {
		text := sanitizeText(*req.WatermarkText)
		req.WatermarkText = &text
	}

	// Watermark position (fractional center, clamped to 0-1; nil = legacy placement)
	if req.WatermarkPosX != nil {
		v := clamp(*req.WatermarkPosX, 0, 1)
		req.WatermarkPosX = &v
	}
	if req.WatermarkPosY != nil {
		v := clamp(*req.WatermarkPosY, 0, 1)
		req.WatermarkPosY = &v
	}

	// Watermark scale (clamped; default 1.0)
	watermarkScale := 1.0
	if req.WatermarkScale != nil {
		watermarkScale = clamp(*req.WatermarkScale, watermarkScaleMin, watermarkScaleMax)
	}

	// Per-room participant limit (nil = global default)
	if req.MaxParticipants != nil && (*req.MaxParticipants < 1 || *req.MaxParticipants > 100) {
		http.Error(w, "Participant limit must be between 1 and 100", http.StatusBadRequest)
		return
	}

	// Lobby window for scheduled rooms (minutes before start; default 10)
	earlyOpenMinutes := defaultEarlyOpenMinutes
	if req.EarlyOpenMinutes != nil {
		if *req.EarlyOpenMinutes < 0 || *req.EarlyOpenMinutes > maxEarlyOpenMinutes {
			http.Error(w, "Lobby window must be between 0 and 120 minutes", http.StatusBadRequest)
			return
		}
		earlyOpenMinutes = *req.EarlyOpenMinutes
	}

	// Insert room
	insertCtx, insertCancel := database.WithTimeout(r.Context())
	_, err = h.db.ExecContext(insertCtx, `
		INSERT INTO rooms (
			id, slug, name, scheduled_at, duration_minutes, early_open_minutes,
			password_hash,
			waiting_room_enabled, stream_key_id, watermark_mode, watermark_text,
			watermark_logo_position, watermark_opacity, watermark_pos_x,
			watermark_pos_y, watermark_scale, max_participants
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, id, req.Slug, req.Name, req.ScheduledAt, req.DurationMinutes, earlyOpenMinutes,
		passwordHash,
		req.WaitingRoomEnabled, req.StreamKeyID, req.WatermarkMode, req.WatermarkText,
		req.WatermarkLogoPosition, watermarkOpacity, req.WatermarkPosX,
		req.WatermarkPosY, watermarkScale, req.MaxParticipants)
	insertCancel()

	if err != nil {
		// Check for unique constraint violation
		http.Error(w, "Slug already exists", http.StatusConflict)
		return
	}

	// Fetch the created room
	room := models.Room{
		ID:                    id,
		Slug:                  req.Slug,
		Name:                  req.Name,
		ScheduledAt:           req.ScheduledAt,
		DurationMinutes:       req.DurationMinutes,
		EarlyOpenMinutes:      earlyOpenMinutes,
		HasPassword:           passwordHash != nil,
		WaitingRoomEnabled:    req.WaitingRoomEnabled,
		StreamKeyID:           req.StreamKeyID,
		WatermarkMode:         req.WatermarkMode,
		WatermarkText:         req.WatermarkText,
		WatermarkLogoPosition: req.WatermarkLogoPosition,
		WatermarkOpacity:      watermarkOpacity,
		WatermarkPosX:         req.WatermarkPosX,
		WatermarkPosY:         req.WatermarkPosY,
		WatermarkScale:        watermarkScale,
		MaxParticipants:       req.MaxParticipants,
		Status:                models.RoomStatusPending,
		CreatedAt:             time.Now(),
	}

	// Track room creation
	metrics.Get().TotalRoomsCreated.Add(1)

	w.WriteHeader(http.StatusCreated)
	respondJSON(w, room)
}

// Get returns a single room by slug
func (h *RoomHandler) Get(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	room, err := h.getRoomBySlug(slug)
	if err != nil {
		http.Error(w, "Room not found", http.StatusNotFound)
		return
	}

	respondJSON(w, room)
}

// Update updates a room
func (h *RoomHandler) Update(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")

	var updates map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if len(updates) == 0 {
		http.Error(w, "No valid fields to update", http.StatusBadRequest)
		return
	}

	setClauses := make([]string, 0)
	args := make([]interface{}, 0)

	if raw, ok := updates["name"]; ok {
		var name string
		if err := json.Unmarshal(raw, &name); err != nil {
			http.Error(w, "Invalid name", http.StatusBadRequest)
			return
		}
		name = sanitizeText(name)
		if len(name) < 1 || len(name) > 100 {
			http.Error(w, "Name must be 1-100 characters", http.StatusBadRequest)
			return
		}
		setClauses = append(setClauses, "name = ?")
		args = append(args, name)
	}

	if raw, ok := updates["scheduledAt"]; ok {
		if string(raw) == "null" {
			setClauses = append(setClauses, "scheduled_at = NULL")
		} else {
			var scheduledAt time.Time
			if err := json.Unmarshal(raw, &scheduledAt); err != nil {
				http.Error(w, "Invalid scheduledAt", http.StatusBadRequest)
				return
			}
			setClauses = append(setClauses, "scheduled_at = ?")
			args = append(args, scheduledAt)
		}
	}

	if raw, ok := updates["durationMinutes"]; ok {
		var duration int
		if err := json.Unmarshal(raw, &duration); err != nil {
			http.Error(w, "Invalid durationMinutes", http.StatusBadRequest)
			return
		}
		setClauses = append(setClauses, "duration_minutes = ?")
		args = append(args, duration)
	}

	if raw, ok := updates["earlyOpenMinutes"]; ok {
		var minutes int
		if err := json.Unmarshal(raw, &minutes); err != nil {
			http.Error(w, "Invalid earlyOpenMinutes", http.StatusBadRequest)
			return
		}
		if minutes < 0 || minutes > maxEarlyOpenMinutes {
			http.Error(w, "Lobby window must be between 0 and 120 minutes", http.StatusBadRequest)
			return
		}
		setClauses = append(setClauses, "early_open_minutes = ?")
		args = append(args, minutes)
	}

	if raw, ok := updates["password"]; ok {
		if string(raw) == "null" {
			setClauses = append(setClauses, "password_hash = NULL")
		} else {
			var password string
			if err := json.Unmarshal(raw, &password); err != nil {
				http.Error(w, "Invalid password", http.StatusBadRequest)
				return
			}
			if password == "" {
				setClauses = append(setClauses, "password_hash = NULL")
			} else if len(password) < 8 {
				http.Error(w, "Password must be at least 8 characters", http.StatusBadRequest)
				return
			} else {
				hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
				if err != nil {
					http.Error(w, "Failed to process password", http.StatusInternalServerError)
					return
				}
				setClauses = append(setClauses, "password_hash = ?")
				args = append(args, string(hash))
			}
		}
	}

	if raw, ok := updates["waitingRoomEnabled"]; ok {
		var waiting bool
		if err := json.Unmarshal(raw, &waiting); err != nil {
			http.Error(w, "Invalid waitingRoomEnabled", http.StatusBadRequest)
			return
		}
		setClauses = append(setClauses, "waiting_room_enabled = ?")
		args = append(args, waiting)
	}

	if raw, ok := updates["streamKeyId"]; ok {
		if string(raw) == "null" {
			setClauses = append(setClauses, "stream_key_id = NULL")
		} else {
			var streamKeyID string
			if err := json.Unmarshal(raw, &streamKeyID); err != nil {
				http.Error(w, "Invalid streamKeyId", http.StatusBadRequest)
				return
			}
			setClauses = append(setClauses, "stream_key_id = ?")
			args = append(args, streamKeyID)
		}
	}

	if raw, ok := updates["watermarkMode"]; ok {
		var mode string
		if err := json.Unmarshal(raw, &mode); err != nil {
			http.Error(w, "Invalid watermarkMode", http.StatusBadRequest)
			return
		}
		if mode != "none" && mode != "text" && mode != "logo" && mode != "both" {
			http.Error(w, "Invalid watermark mode", http.StatusBadRequest)
			return
		}
		setClauses = append(setClauses, "watermark_mode = ?")
		args = append(args, mode)
	}

	if raw, ok := updates["watermarkText"]; ok {
		if string(raw) == "null" {
			setClauses = append(setClauses, "watermark_text = NULL")
		} else {
			var text string
			if err := json.Unmarshal(raw, &text); err != nil {
				http.Error(w, "Invalid watermarkText", http.StatusBadRequest)
				return
			}
			text = sanitizeText(text)
			setClauses = append(setClauses, "watermark_text = ?")
			args = append(args, text)
		}
	}

	if raw, ok := updates["watermarkLogoPosition"]; ok {
		var position string
		if err := json.Unmarshal(raw, &position); err != nil {
			http.Error(w, "Invalid watermarkLogoPosition", http.StatusBadRequest)
			return
		}
		switch position {
		case "top-left", "top-right", "bottom-left", "bottom-right":
		default:
			http.Error(w, "Invalid watermark logo position", http.StatusBadRequest)
			return
		}
		setClauses = append(setClauses, "watermark_logo_position = ?")
		args = append(args, position)
	}

	if raw, ok := updates["watermarkOpacity"]; ok {
		var opacity float64
		if err := json.Unmarshal(raw, &opacity); err != nil {
			http.Error(w, "Invalid watermarkOpacity", http.StatusBadRequest)
			return
		}
		if opacity < 0 || opacity > 1 {
			http.Error(w, "Watermark opacity must be between 0 and 1", http.StatusBadRequest)
			return
		}
		setClauses = append(setClauses, "watermark_opacity = ?")
		args = append(args, opacity)
	}

	// Watermark position: fractional center (clamped 0-1), null = legacy placement
	for key, column := range map[string]string{
		"watermarkPosX": "watermark_pos_x",
		"watermarkPosY": "watermark_pos_y",
	} {
		raw, ok := updates[key]
		if !ok {
			continue
		}
		if string(raw) == "null" {
			setClauses = append(setClauses, column+" = NULL")
			continue
		}
		var pos float64
		if err := json.Unmarshal(raw, &pos); err != nil {
			http.Error(w, "Invalid "+key, http.StatusBadRequest)
			return
		}
		setClauses = append(setClauses, column+" = ?")
		args = append(args, clamp(pos, 0, 1))
	}

	if raw, ok := updates["watermarkScale"]; ok {
		var scale float64
		if err := json.Unmarshal(raw, &scale); err != nil {
			http.Error(w, "Invalid watermarkScale", http.StatusBadRequest)
			return
		}
		setClauses = append(setClauses, "watermark_scale = ?")
		args = append(args, clamp(scale, watermarkScaleMin, watermarkScaleMax))
	}

	if raw, ok := updates["maxParticipants"]; ok {
		if string(raw) == "null" {
			setClauses = append(setClauses, "max_participants = NULL")
		} else {
			var limit int
			if err := json.Unmarshal(raw, &limit); err != nil {
				http.Error(w, "Invalid maxParticipants", http.StatusBadRequest)
				return
			}
			if limit < 1 || limit > 100 {
				http.Error(w, "Participant limit must be between 1 and 100", http.StatusBadRequest)
				return
			}
			setClauses = append(setClauses, "max_participants = ?")
			args = append(args, limit)
		}
	}

	if len(setClauses) == 0 {
		http.Error(w, "No valid fields to update", http.StatusBadRequest)
		return
	}

	query := "UPDATE rooms SET " + strings.Join(setClauses, ", ") + " WHERE slug = ?"
	args = append(args, slug)

	updCtx, updCancel := database.WithTimeout(r.Context())
	result, err := h.db.ExecContext(updCtx, query, args...)
	updCancel()
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		http.Error(w, "Room not found", http.StatusNotFound)
		return
	}

	// Return updated room
	room, _ := h.getRoomBySlug(slug)
	respondJSON(w, room)
}

// Delete deletes a room. With ?deleteFiles=true the room's uploaded files
// (UPLOAD_PATH/{roomID}/, including thumbnails) are removed from disk as
// well — the DB rows for files/messages/participants cascade either way.
func (h *RoomHandler) Delete(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	deleteFiles := r.URL.Query().Get("deleteFiles") == "true"

	// Resolve the room ID before the row disappears — the upload directory
	// is keyed by ID, not slug.
	var roomID string
	idCtx, idCancel := database.WithTimeout(r.Context())
	if err := h.db.QueryRowContext(idCtx, "SELECT id FROM rooms WHERE slug = ?", slug).Scan(&roomID); err != nil {
		idCancel()
		http.Error(w, "Room not found", http.StatusNotFound)
		return
	}
	idCancel()

	delCtx, delCancel := database.WithTimeout(r.Context())
	result, err := h.db.ExecContext(delCtx, "DELETE FROM rooms WHERE slug = ?", slug)
	delCancel()
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		http.Error(w, "Room not found", http.StatusNotFound)
		return
	}

	if deleteFiles && roomID != "" && h.uploadPath != "" {
		dir := filepath.Join(h.uploadPath, roomID)
		// Same containment rule as file serving: never unlink outside the
		// upload root.
		if isPathWithin(h.uploadPath, dir) {
			if err := os.RemoveAll(dir); err != nil {
				logger.Warn("Failed to remove room upload directory", "room_id", roomID, "error", err)
			} else {
				logger.Info("Removed room upload directory", "room_id", roomID, "room", slug)
			}
		}
	}

	h.closeRoomRuntime(slug, true)

	w.WriteHeader(http.StatusNoContent)
}

// EndSession ends a live session
func (h *RoomHandler) EndSession(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")

	now := time.Now()
	endCtx, endCancel := database.WithTimeout(r.Context())
	result, err := h.db.ExecContext(endCtx, `
		UPDATE rooms SET status = 'ended', ended_at = ? WHERE slug = ? AND status = 'live'
	`, now, slug)
	endCancel()
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		http.Error(w, "Room not found or not live", http.StatusNotFound)
		return
	}

	h.closeRoomRuntime(slug, true)

	w.WriteHeader(http.StatusNoContent)
}

// PublicInfo returns public information about a room (for join flow).
// serverTime lets clients anchor lobby countdowns to the server clock
// instead of trusting the local one.
func (h *RoomHandler) PublicInfo(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")

	var info struct {
		Name               string     `json:"name"`
		HasPassword        bool       `json:"hasPassword"`
		WaitingRoomEnabled bool       `json:"waitingRoomEnabled"`
		Status             string     `json:"status"`
		ScheduledAt        *time.Time `json:"scheduledAt,omitempty"`
		EarlyOpenMinutes   int        `json:"earlyOpenMinutes"`
		OpenedAt           *time.Time `json:"openedAt,omitempty"`
		ServerTime         time.Time  `json:"serverTime"`
	}

	infoCtx, infoCancel := database.WithTimeout(r.Context())
	err := h.db.QueryRowContext(infoCtx, `
		SELECT name, password_hash IS NOT NULL, waiting_room_enabled, status, scheduled_at,
		       COALESCE(early_open_minutes, 10), opened_at
		FROM rooms WHERE slug = ?
	`, slug).Scan(&info.Name, &info.HasPassword, &info.WaitingRoomEnabled, &info.Status, &info.ScheduledAt,
		&info.EarlyOpenMinutes, &info.OpenedAt)
	infoCancel()

	if err != nil {
		http.Error(w, "Room not found", http.StatusNotFound)
		return
	}

	info.ServerTime = time.Now().UTC()
	respondJSON(w, info)
}

// JoinRequest is the request body for joining a room
type JoinRequest struct {
	Name       string  `json:"name"`
	Password   *string `json:"password,omitempty"`
	AdminToken *string `json:"adminToken,omitempty"`
}

// Join handles the room join flow
func (h *RoomHandler) Join(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")

	var req JoinRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	req.Name = sanitizeText(req.Name)
	// Validate name
	if len(req.Name) < 1 || len(req.Name) > 50 {
		http.Error(w, "Name must be 1-50 characters", http.StatusBadRequest)
		return
	}

	// Get room
	var roomID, passwordHash string
	var waitingRoom bool
	var roomStatus string
	var scheduledAt, openedAt *time.Time
	var earlyOpenMinutes int
	var roomMaxParticipants *int

	joinCtx, joinCancel := database.WithTimeout(r.Context())
	err := h.db.QueryRowContext(joinCtx, `
		SELECT id, COALESCE(password_hash, ''), waiting_room_enabled, status, scheduled_at,
		       COALESCE(early_open_minutes, 10), opened_at, max_participants
		FROM rooms WHERE slug = ?
	`, slug).Scan(&roomID, &passwordHash, &waitingRoom, &roomStatus, &scheduledAt,
		&earlyOpenMinutes, &openedAt, &roomMaxParticipants)
	joinCancel()

	if err != nil {
		http.Error(w, "Room not found", http.StatusNotFound)
		return
	}

	if roomStatus == "ended" {
		http.Error(w, "Room has ended", http.StatusGone)
		return
	}

	// Admin join: a valid admin session cookie (dashboard "Join as host") or
	// the admin token in the request body (never from the URL, where it would
	// be logged by proxies). Admins bypass scheduling, the waiting room and
	// the password check. An explicitly provided token must always be valid,
	// even when a session cookie is also present.
	isAdmin := h.hasAdminSession(r)
	if req.AdminToken != nil {
		if h.adminToken == "" || subtle.ConstantTimeCompare([]byte(*req.AdminToken), []byte(h.adminToken)) != 1 {
			http.Error(w, "Invalid admin token", http.StatusUnauthorized)
			return
		}
		isAdmin = true
	}

	// Scheduled-room gating (admins always bypass). A room counts as open
	// once scheduled_at has passed, an admin opened it early (opened_at), or
	// the stream is already live. Between (scheduled_at - early_open_minutes)
	// and open, guests land in the countdown lobby: a participant row with
	// is_admitted=false plus a `lobby` payload so the client renders a
	// countdown instead of an approval queue.
	now := time.Now()
	roomOpen := scheduledAt == nil || openedAt != nil || roomStatus == "live" || !now.Before(*scheduledAt)
	inLobby := false
	var opensAt time.Time
	if !isAdmin && !roomOpen {
		opensAt = scheduledAt.Add(-time.Duration(earlyOpenMinutes) * time.Minute)
		if now.Before(opensAt) {
			http.Error(w, "Session not open yet", http.StatusForbidden)
			return
		}
		inLobby = true
	}

	// Check password (admins bypass)
	if passwordHash != "" && !isAdmin {
		if req.Password == nil {
			http.Error(w, "Password required", http.StatusUnauthorized)
			return
		}
		if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(*req.Password)); err != nil {
			http.Error(w, "Invalid password", http.StatusUnauthorized)
			return
		}
	}

	// Enforce participant cap (room status was already verified as non-ended).
	// A per-room limit overrides the global default when set.
	maxParticipants := h.maxParticipants
	if roomMaxParticipants != nil && *roomMaxParticipants > 0 {
		maxParticipants = *roomMaxParticipants
	}
	h.joinMu.Lock()
	defer h.joinMu.Unlock()

	// Count currently connected viewers plus short-lived join reservations.
	// The reservation window covers the gap between POST /join and the
	// WebSocket opening; without it, a burst of joins can all pass while the
	// hub still reports zero clients. Waiting/lobby participants are counted
	// until admitted or denied so approval queues cannot grow past capacity.
	currentParticipants := h.countRecentParticipants(roomID, joinReservationWindow)
	if h.hub != nil {
		if connected := h.hub.RoomClientCount(slug); connected > currentParticipants {
			currentParticipants = connected
		}
	}
	currentParticipants += h.countWaiting(roomID)
	if currentParticipants >= maxParticipants {
		http.Error(w, "Room is full", http.StatusServiceUnavailable)
		return
	}

	// Create participant. Admins bypass the waiting room; lobby joiners stay
	// unadmitted until the room opens (then waiting-room-disabled rooms
	// auto-admit, waiting-room-enabled rooms fall into the approval flow).
	participantID, err := generateID()
	if err != nil {
		logger.Error("Failed to generate participant ID", "error", err, "room", slug)
		http.Error(w, "Failed to join room", http.StatusInternalServerError)
		return
	}
	color := h.assignRoomColor(roomID, participantID)
	isAdmitted := isAdmin || (!waitingRoom && !inLobby)
	role := "viewer"
	if isAdmin {
		role = "admin"
	}

	insCtx, insCancel := database.WithTimeout(r.Context())
	_, err = h.db.ExecContext(insCtx, `
		INSERT INTO participants (id, room_id, name, role, color, is_admitted)
		VALUES (?, ?, ?, ?, ?, ?)
	`, participantID, roomID, req.Name, role, color, isAdmitted)
	insCancel()

	if err != nil {
		http.Error(w, "Failed to join room", http.StatusInternalServerError)
		return
	}

	// Generate signed join token (valid for 24 hours)
	token, err := h.tokenManager.GenerateToken(participantID, slug, req.Name, 24*time.Hour)
	if err != nil {
		logger.Error("Failed to generate join token", "error", err, "room", slug, "participant", participantID)
		http.Error(w, "Failed to generate authentication token", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     JoinTokenCookieName(slug),
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   h.productionMode,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int((24 * time.Hour).Seconds()),
	})

	// Track join request
	metrics.Get().TotalJoinRequests.Add(1)
	if !isAdmitted {
		// Track waiting participant
		metrics.Get().WaitingParticipants.Add(1)
	}

	// Tell in-session admins about the new arrival: approval popup for
	// waiting-room participants, lobby headcount for countdown lobbies.
	if !isAdmitted && h.hub != nil {
		if waitingRoom {
			h.hub.BroadcastToAdminsJSON(slug, "waiting:joined", map[string]interface{}{
				"participantId": participantID,
				"name":          req.Name,
				"joinedAt":      now.UTC(),
			})
		} else {
			h.hub.BroadcastToAdminsJSON(slug, "lobby:count", map[string]interface{}{
				"count": h.countWaiting(roomID),
			})
		}
	}

	// Arm the auto-open timer so lobby participants are admitted the moment
	// scheduled_at arrives — no polling.
	if inLobby {
		h.ensureOpenTimer(slug, *scheduledAt)
	}

	response := buildJoinResponse(participantID, token, isAdmitted, color, req.Name, role, now.UTC(), !h.productionMode)
	if inLobby {
		response["lobby"] = map[string]interface{}{
			"scheduledAt":        scheduledAt.UTC(),
			"opensAt":            opensAt.UTC(),
			"waitingRoomEnabled": waitingRoom,
		}
	}

	respondJSON(w, response)
}

func buildJoinResponse(participantID, token string, isAdmitted bool, color, name, role string, serverTime time.Time, includeToken bool) map[string]interface{} {
	response := map[string]interface{}{
		"participantId": participantID,
		"isAdmitted":    isAdmitted,
		"waitingRoom":   !isAdmitted,
		"color":         color,
		"name":          name,
		"role":          role,
		"serverTime":    serverTime,
	}
	if includeToken {
		response["token"] = token
	}
	return response
}

// countWaiting returns the number of unadmitted participants in a room.
func (h *RoomHandler) countWaiting(roomID string) int {
	var n int
	ctx, cancel := database.WithTimeout(context.Background())
	err := h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM participants WHERE room_id = ? AND is_admitted = FALSE`, roomID).Scan(&n)
	cancel()
	if err != nil {
		return 0
	}
	return n
}

func (h *RoomHandler) countRecentParticipants(roomID string, window time.Duration) int {
	var n int
	cutoff := time.Now().Add(-window)
	ctx, cancel := database.WithTimeout(context.Background())
	err := h.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM participants
		WHERE room_id = ? AND joined_at >= ? AND is_admitted = TRUE
	`, roomID, cutoff).Scan(&n)
	cancel()
	if err != nil {
		return 0
	}
	return n
}

// ListWaiting lists participants in the waiting room
func (h *RoomHandler) ListWaiting(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")

	listCtx, listCancel := database.WithTimeout(r.Context())
	rows, err := h.db.QueryContext(listCtx, `
		SELECT p.id, p.name, p.joined_at
		FROM participants p
		JOIN rooms r ON r.id = p.room_id
		WHERE r.slug = ? AND p.is_admitted = FALSE
		ORDER BY p.joined_at
	`, slug)
	if err != nil {
		listCancel()
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	defer listCancel()

	participants := []map[string]interface{}{}
	for rows.Next() {
		var id, name string
		var joinedAt time.Time
		if err := rows.Scan(&id, &name, &joinedAt); err != nil {
			continue
		}
		participants = append(participants, map[string]interface{}{
			"id":       id,
			"name":     name,
			"joinedAt": joinedAt,
		})
	}
	// Distinguish end-of-data from a mid-iteration error so a timeout isn't
	// served as a silently truncated waiting list.
	if err := rows.Err(); err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	respondJSON(w, participants)
}

// AdmitWaitingParticipant flips a waiting participant to admitted, notifies
// them over SSE, and tells the room's admins the request is resolved. This is
// the single admit implementation shared by the REST endpoint and the
// admin:waiting-approve WebSocket command. Returns false when no waiting
// participant matched.
func (h *RoomHandler) AdmitWaitingParticipant(slug, participantID string) (bool, error) {
	ctx, cancel := database.WithTimeout(context.Background())
	result, err := h.db.ExecContext(ctx, `
		UPDATE participants SET is_admitted = TRUE
		WHERE id = ? AND is_admitted = FALSE
		  AND room_id = (SELECT id FROM rooms WHERE slug = ?)
	`, participantID, slug)
	cancel()
	if err != nil {
		return false, err
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		return false, nil
	}

	// Track admitted participant
	metrics.Get().WaitingParticipants.Add(-1)

	// Notify waiting participant via SSE
	h.waitingManager.NotifyAdmitted(participantID)

	// Dismiss this request on every admin's approval stack
	if h.hub != nil {
		h.hub.BroadcastToAdminsJSON(slug, "waiting:resolved", map[string]interface{}{
			"participantId": participantID,
			"action":        "approved",
		})
	}
	return true, nil
}

// DenyWaitingParticipant removes a waiting participant, notifies them over
// SSE, and tells the room's admins the request is resolved. Shared by the
// REST endpoint and the admin:waiting-deny WebSocket command.
func (h *RoomHandler) DenyWaitingParticipant(slug, participantID string) (bool, error) {
	// Notify before deleting the row so the SSE handler can still validate
	// the subscription; the connection closes right after the event anyway.
	ctx, cancel := database.WithTimeout(context.Background())
	result, err := h.db.ExecContext(ctx, `
		DELETE FROM participants
		WHERE id = ? AND is_admitted = FALSE
		  AND room_id = (SELECT id FROM rooms WHERE slug = ?)
	`, participantID, slug)
	cancel()
	if err != nil {
		return false, err
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		return false, nil
	}

	metrics.Get().WaitingParticipants.Add(-1)

	h.waitingManager.NotifyEvent([]string{participantID}, "denied")

	if h.hub != nil {
		h.hub.BroadcastToAdminsJSON(slug, "waiting:resolved", map[string]interface{}{
			"participantId": participantID,
			"action":        "denied",
		})
	}
	return true, nil
}

// AdmitParticipant admits a specific participant from waiting room
func (h *RoomHandler) AdmitParticipant(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	participantID := r.PathValue("id")

	found, err := h.AdmitWaitingParticipant(slug, participantID)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "Participant not found", http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// DenyParticipant denies (removes) a specific waiting participant
func (h *RoomHandler) DenyParticipant(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	participantID := r.PathValue("id")

	found, err := h.DenyWaitingParticipant(slug, participantID)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "Participant not found", http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// AdmitAll admits all waiting participants
func (h *RoomHandler) AdmitAll(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")

	// Atomically admit and return the admitted set in one statement. The
	// previous SELECT-then-UPDATE was a TOCTOU: a participant joining between
	// the two statements would be admitted by the UPDATE but absent from the
	// notification list — their lobby UI never advanced, and the
	// WaitingParticipants gauge drifted (decremented by the wrong count).
	// UPDATE...RETURNING captures exactly the rows this call admitted.
	admitCtx, admitCancel := database.WithTimeout(r.Context())
	rows, err := h.db.QueryContext(admitCtx, `
		UPDATE participants SET is_admitted = TRUE
		WHERE room_id = (SELECT id FROM rooms WHERE slug = ?) AND is_admitted = FALSE
		RETURNING id
	`, slug)
	if err != nil {
		admitCancel()
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	var waitingIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			waitingIDs = append(waitingIDs, id)
		}
	}
	// A mid-iteration error means participants were admitted in the DB but not
	// captured here — they'd miss their SSE admit notification. Surface it.
	if err := rows.Err(); err != nil {
		logger.Error("Admit-all RETURNING iteration failed; some admitted participants may not be notified",
			"room", slug, "error", err)
	}
	rows.Close()
	admitCancel()

	// Track admitted participants
	metrics.Get().WaitingParticipants.Add(-int64(len(waitingIDs)))

	// Notify all waiting participants via SSE
	h.waitingManager.NotifyAllAdmitted(waitingIDs)

	// Dismiss every pending request on the admins' approval stacks
	if h.hub != nil {
		for _, id := range waitingIDs {
			h.hub.BroadcastToAdminsJSON(slug, "waiting:resolved", map[string]interface{}{
				"participantId": id,
				"action":        "approved",
			})
		}
		h.hub.BroadcastToAdminsJSON(slug, "lobby:count", map[string]interface{}{"count": 0})
	}

	w.WriteHeader(http.StatusNoContent)
}

// CheckParticipantStatus checks if a participant has been admitted.
// The join token is taken from the X-Join-Token header or the HttpOnly
// per-room join cookie. Query params are logged by proxies and must not carry
// credentials.
func (h *RoomHandler) CheckParticipantStatus(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	participantID := r.PathValue("id")
	token := joinTokenFromRequest(r, slug, false)

	if token == "" {
		http.Error(w, "Missing token", http.StatusUnauthorized)
		return
	}

	payload, err := h.tokenManager.ValidateToken(token)
	if err != nil || payload.ParticipantID != participantID || payload.RoomSlug != slug {
		http.Error(w, "Invalid token", http.StatusUnauthorized)
		return
	}

	// Recover from a lost open timer (server restart) before reading state.
	h.maybeRunMissedOpen(slug)

	var isAdmitted bool
	var roomStatus string

	statusCtx, statusCancel := database.WithTimeout(r.Context())
	err = h.db.QueryRowContext(statusCtx, `
		SELECT p.is_admitted, r.status
		FROM participants p
		JOIN rooms r ON r.id = p.room_id
		WHERE r.slug = ? AND p.id = ?
	`, slug, participantID).Scan(&isAdmitted, &roomStatus)
	statusCancel()

	if err != nil {
		http.Error(w, "Participant not found", http.StatusNotFound)
		return
	}

	respondJSON(w, map[string]interface{}{
		"isAdmitted": isAdmitted,
		"roomStatus": roomStatus,
		"serverTime": time.Now().UTC(),
	})
}

// OnStreamStart is called when OBS starts streaming
func (h *RoomHandler) OnStreamStart(streamKeyToken string) error {
	now := time.Now()

	// Use transaction to ensure atomicity of room lookup and status update
	// This prevents race conditions with multiple OBS connections
	ctx, cancel := database.WithTimeout(context.Background())
	defer cancel()
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		if tx != nil {
			tx.Rollback()
		}
	}()

	// Find the room bound to this stream key (prefer live room on reconnect)
	var roomSlug string
	var roomStatus string
	err = tx.QueryRowContext(ctx, `
		SELECT r.slug, r.status FROM rooms r
		JOIN stream_keys sk ON sk.id = r.stream_key_id
		WHERE sk.key_token = ? AND r.status IN ('pending', 'live')
		ORDER BY
			CASE WHEN r.status = 'live' THEN 0 ELSE 1 END,
			CASE WHEN r.scheduled_at IS NULL THEN 1 ELSE 0 END,
			r.scheduled_at,
			r.created_at
		LIMIT 1
	`, streamKeyToken).Scan(&roomSlug, &roomStatus)

	if err != nil {
		if err == sql.ErrNoRows {
			tx.Rollback()
			tx = nil
			return nil
		}
		return fmt.Errorf("failed to find room for stream key: %w", err)
	}

	// Update room status to live when transitioning from pending
	if roomStatus == "pending" {
		result, err := tx.ExecContext(ctx, `
			UPDATE rooms SET status = 'live', started_at = ?
			WHERE slug = ? AND status = 'pending'
		`, now, roomSlug)
		if err != nil {
			return fmt.Errorf("failed to update room status: %w", err)
		}

		// Check if we actually updated a row (another transaction might have beaten us)
		rowsAffected, _ := result.RowsAffected()
		if rowsAffected == 0 {
			// Room was already taken by another stream, rollback
			tx.Rollback()
			tx = nil
			return nil
		}
	}

	// Commit the transaction
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}
	tx = nil // Prevent deferred rollback

	// Cancel any pending stream end timer
	h.cancelStreamEndTimer(roomSlug)

	// Going live opens a scheduled room early: mark opened_at and run the
	// lobby auto-admission flow. Only relevant while scheduled_at is still in
	// the future — past that point the open timer / join gating already
	// treats the room as open.
	openCtx, openCancel := database.WithTimeout(context.Background())
	res, err := h.db.ExecContext(openCtx, `
		UPDATE rooms SET opened_at = ?
		WHERE slug = ? AND opened_at IS NULL AND scheduled_at IS NOT NULL AND scheduled_at > ?
	`, now, roomSlug, now)
	openCancel()
	if err == nil {
		if affected, _ := res.RowsAffected(); affected > 0 {
			h.handleRoomOpen(roomSlug)
		}
	}

	// Bind ingest tracks to room for distribution
	var subsNeedingReneg []string
	if h.sfu != nil {
		var err error
		subsNeedingReneg, err = h.sfu.BindIngestToRoom(streamKeyToken, roomSlug)
		if err != nil {
			// Log but don't fail - room is already marked live
			logger.Warn("Failed to bind ingest to room", "room", roomSlug, "error", err)
		}
	}

	if h.hub != nil {
		if roomStatus == "pending" {
			// Notify all connected clients that room is live
			h.hub.BroadcastJSON(roomSlug, "room:live", map[string]interface{}{}, "")
		} else {
			h.hub.BroadcastJSON(roomSlug, "stream:resumed", map[string]interface{}{}, "")
		}
	}

	// Renegotiate with existing subscribers whose ingest tracks were freshly
	// added by the bind (they had a subscriber but no tracks).
	if h.sfu != nil && h.hub != nil {
		for _, subID := range subsNeedingReneg {
			offerSDP, offerID, err := h.sfu.RenegotiateSubscriber(roomSlug, subID)
			if err != nil {
				logger.Warn("Failed to renegotiate subscriber after ingest bind", "subscriber", subID, "error", err)
				continue
			}
			h.hub.SendToJSON(roomSlug, subID, "signal:renegotiate", map[string]interface{}{
				"sdp":     offerSDP,
				"offerId": offerID,
			})
			logger.Debug("Sent renegotiation offer to subscriber", "subscriber", subID, "room", roomSlug)
		}
	}

	// Create subscribers + send offers for connected clients that have no SFU
	// subscriber yet. Runs in BOTH branches: clients that connected before the
	// ingest existed (room pending, or marked live during an OBS reconnect /
	// after a server restart) could not subscribe at connect time and would
	// otherwise never receive a signal:offer.
	if h.onRoomLive != nil {
		h.onRoomLive(roomSlug)
	}

	logger.Info("Room is now live", "room", roomSlug)
	return nil
}

// OnStreamEnd is called when OBS stops streaming
func (h *RoomHandler) OnStreamEnd(streamKeyToken string) {
	// Find the room associated with this stream key
	var roomSlug string
	ctx, cancel := database.WithTimeout(context.Background())
	err := h.db.QueryRowContext(ctx, `
		SELECT r.slug FROM rooms r
		JOIN stream_keys sk ON sk.id = r.stream_key_id
		WHERE sk.key_token = ? AND r.status = 'live'
	`, streamKeyToken).Scan(&roomSlug)
	cancel()

	if err != nil {
		// No live room for this stream key - that's fine
		return
	}

	// Notify connected clients that the stream has paused.
	// Rooms never auto-end — only an admin can end the session.
	if h.hub != nil {
		h.hub.BroadcastJSON(roomSlug, "stream:paused", map[string]interface{}{
			"message": "Stream disconnected. Waiting for reconnection...",
		}, "")
	}

	logger.Info("Stream paused (OBS disconnected)", "room", roomSlug)
}

func (h *RoomHandler) getRoomBySlug(slug string) (*models.Room, error) {
	var room models.Room
	ctx, cancel := database.WithTimeout(context.Background())
	err := h.db.QueryRowContext(ctx, `
		SELECT id, slug, name, scheduled_at, duration_minutes,
		       COALESCE(early_open_minutes, 10), opened_at,
		       password_hash IS NOT NULL, waiting_room_enabled, stream_key_id,
		       watermark_mode, watermark_text, watermark_logo_path,
		       watermark_logo_position, watermark_opacity,
		       watermark_pos_x, watermark_pos_y, COALESCE(watermark_scale, 1.0),
		       max_participants, status,
		       created_at, started_at, ended_at
		FROM rooms WHERE slug = ?
	`, slug).Scan(
		&room.ID, &room.Slug, &room.Name, &room.ScheduledAt, &room.DurationMinutes,
		&room.EarlyOpenMinutes, &room.OpenedAt,
		&room.HasPassword, &room.WaitingRoomEnabled, &room.StreamKeyID,
		&room.WatermarkMode, &room.WatermarkText, &room.WatermarkLogoPath,
		&room.WatermarkLogoPosition, &room.WatermarkOpacity,
		&room.WatermarkPosX, &room.WatermarkPosY, &room.WatermarkScale,
		&room.MaxParticipants, &room.Status,
		&room.CreatedAt, &room.StartedAt, &room.EndedAt,
	)
	cancel()
	return &room, err
}

// Helper functions

func generateID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// High-visibility participant colors: bright, saturated, maximally separated
// hues that read clearly as laser pointers over dark, color-critical footage.
var cursorColors = []string{
	"#ff3b30", // red
	"#00e5ff", // cyan
	"#ffd60a", // yellow
	"#30d158", // green
	"#ff2d92", // pink
	"#ff9500", // orange
	"#4da3ff", // blue
	"#bf5af2", // purple
}

// assignColor is the hash-based fallback used when every palette color is
// already taken in the room (or the lookup fails).
func assignColor(id string) string {
	hash := 0
	for _, c := range id {
		hash = (hash*31 + int(c)) % len(cursorColors)
	}
	return cursorColors[hash]
}

// assignRoomColor picks the first palette color not already in use in the
// room, so concurrent participants get maximally distinct colors instead of
// hash-collision repeats. Falls back to hash assignment once the palette is
// exhausted.
func (h *RoomHandler) assignRoomColor(roomID, participantID string) string {
	ctx, cancel := database.WithTimeout(context.Background())
	rows, err := h.db.QueryContext(ctx, `SELECT color FROM participants WHERE room_id = ?`, roomID)
	if err != nil {
		cancel()
		return assignColor(participantID)
	}
	defer rows.Close()
	defer cancel()

	used := make(map[string]bool)
	for rows.Next() {
		var c string
		if rows.Scan(&c) == nil {
			used[c] = true
		}
	}
	if err := rows.Err(); err != nil {
		// Partial color census — fall back to hash assignment rather than
		// confidently picking a color that may already be in use.
		return assignColor(participantID)
	}

	for _, c := range cursorColors {
		if !used[c] {
			return c
		}
	}
	return assignColor(participantID)
}

func respondJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

// WaitingEvents provides Server-Sent Events for waiting room status updates
// This replaces polling with push notifications for instant admission notification
func (h *RoomHandler) WaitingEvents(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	participantID := r.PathValue("id")
	// Prefer the X-Join-Token header or HttpOnly join cookie. Query fallback
	// stays development-only because EventSource cannot set request headers.
	token := joinTokenFromRequest(r, slug, !h.productionMode)

	if token == "" {
		http.Error(w, "Missing token", http.StatusUnauthorized)
		return
	}

	// SSE connections are long-lived; disable the server's write deadline so
	// http.Server.WriteTimeout doesn't kill the stream mid-wait.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
		logger.Debug("Failed to clear write deadline for SSE", "error", err)
	}

	payload, err := h.tokenManager.ValidateToken(token)
	if err != nil || payload.ParticipantID != participantID || payload.RoomSlug != slug {
		http.Error(w, "Invalid token", http.StatusUnauthorized)
		return
	}

	// Recover from a lost open timer (server restart) before reading state.
	h.maybeRunMissedOpen(slug)

	// Verify participant exists and is in waiting state
	var isAdmitted bool
	var roomStatus string
	verifyCtx, verifyCancel := database.WithTimeout(r.Context())
	err = h.db.QueryRowContext(verifyCtx, `
		SELECT p.is_admitted, r.status
		FROM participants p
		JOIN rooms r ON r.id = p.room_id
		WHERE r.slug = ? AND p.id = ?
	`, slug, participantID).Scan(&isAdmitted, &roomStatus)
	verifyCancel()

	if err != nil {
		http.Error(w, "Participant not found", http.StatusNotFound)
		return
	}

	// If already admitted or room ended, return immediately
	if isAdmitted {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		fmt.Fprintf(w, "data: {\"event\":\"admitted\"}\n\n")
		return
	}

	if roomStatus == "ended" {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		fmt.Fprintf(w, "data: {\"event\":\"ended\"}\n\n")
		return
	}

	// Subscribe to notifications (capped to prevent fd exhaustion)
	ch, ok := h.waitingManager.Subscribe(participantID)
	if !ok {
		http.Error(w, "Too many waiting connections, please retry", http.StatusServiceUnavailable)
		return
	}
	defer h.waitingManager.Unsubscribe(participantID)

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Send initial heartbeat
	fmt.Fprintf(w, ": heartbeat\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// Wait for events or client disconnect
	ctx := r.Context()
	ticker := time.NewTicker(30 * time.Second) // Send periodic heartbeats
	defer ticker.Stop()

	for {
		select {
		case event, ok := <-ch:
			if !ok {
				return // Channel closed
			}
			fmt.Fprintf(w, "data: {\"event\":\"%s\"}\n\n", event)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return // Close connection after notification

		case <-ticker.C:
			// Send heartbeat to keep connection alive
			fmt.Fprintf(w, ": heartbeat\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}

		case <-ctx.Done():
			// Client disconnected
			return
		}
	}
}

// HealthCheck is a simple health check endpoint
func HealthCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}
