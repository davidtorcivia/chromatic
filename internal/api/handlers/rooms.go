package handlers

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
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

const earlyAccessWindow = 10 * time.Minute

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

// Subscribe adds a participant's SSE subscription
func (m *WaitingManager) Subscribe(participantID string) chan string {
	m.mu.Lock()
	defer m.mu.Unlock()

	ch := make(chan string, 1)
	m.subscriptions[participantID] = &WaitingSubscription{
		ParticipantID: participantID,
		Channel:       ch,
	}
	return ch
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
	m.mu.RLock()
	defer m.mu.RUnlock()

	if sub, ok := m.subscriptions[participantID]; ok {
		select {
		case sub.Channel <- "admitted":
		default:
			// Channel full or closed, skip
		}
	}
}

// NotifyAllAdmitted sends admission notification to all waiting participants
func (m *WaitingManager) NotifyAllAdmitted(participantIDs []string) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, id := range participantIDs {
		if sub, ok := m.subscriptions[id]; ok {
			select {
			case sub.Channel <- "admitted":
			default:
			}
		}
	}
}

// RoomHandler handles room-related HTTP requests
type RoomHandler struct {
	db  *database.DB
	sfu interface {
		BindIngestToRoom(streamKeyToken, roomSlug string) error
	}
	hub interface {
		BroadcastJSON(roomSlug string, msgType string, payload interface{}, excludeID string) error
	}
	onRoomLive          func(roomSlug string) // Called when room goes live
	tokenManager        *TokenManager         // For generating signed WebSocket tokens
	waitingManager      *WaitingManager       // For waiting room SSE notifications
	obsReconnectTimeout time.Duration
	timerMu             sync.Mutex
	streamEndTimers     map[string]*time.Timer
}

// NewRoomHandler creates a new RoomHandler
func NewRoomHandler(db *database.DB, cfg *config.Config, tokenSecret string) *RoomHandler {
	return &RoomHandler{
		db:                  db,
		tokenManager:        NewTokenManager(tokenSecret),
		waitingManager:      NewWaitingManager(),
		obsReconnectTimeout: cfg.OBSReconnectTimeout,
		streamEndTimers:     make(map[string]*time.Timer),
	}
}

// SetSFU sets the SFU reference (for stream binding)
func (h *RoomHandler) SetSFU(sfu interface {
	BindIngestToRoom(streamKeyToken, roomSlug string) error
}) {
	h.sfu = sfu
}

// SetHub sets the WebSocket hub reference (for notifications)
func (h *RoomHandler) SetHub(hub interface {
	BroadcastJSON(roomSlug string, msgType string, payload interface{}, excludeID string) error
}) {
	h.hub = hub
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
		result, err := h.db.Exec(`
			UPDATE rooms SET status = 'ended', ended_at = ?
			WHERE slug = ? AND status = 'live'
		`, now, roomSlug)
		if err != nil {
			logger.Error("Failed to end room after reconnect timeout", "room", roomSlug, "error", err)
			return
		}

		affected, _ := result.RowsAffected()
		if affected > 0 && h.hub != nil {
			h.hub.BroadcastJSON(roomSlug, "room:ended", map[string]interface{}{}, "")
		}

		h.timerMu.Lock()
		delete(h.streamEndTimers, roomSlug)
		h.timerMu.Unlock()
	})

	h.timerMu.Unlock()
}

// List returns all rooms with optional status filter
func (h *RoomHandler) List(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")

	query := `
		SELECT id, slug, name, scheduled_at, duration_minutes, 
		       password_hash IS NOT NULL as has_password, waiting_room_enabled,
		       stream_key_id, watermark_mode, watermark_text, watermark_logo_path,
		       watermark_logo_position, watermark_opacity, status, 
		       created_at, started_at, ended_at
		FROM rooms
	`
	args := []interface{}{}

	if status != "" {
		query += " WHERE status = ?"
		args = append(args, status)
	}

	query += " ORDER BY created_at DESC"

	rows, err := h.db.Query(query, args...)
	if err != nil {
		logger.Error("Failed to list rooms", "error", err, "status_filter", status)
		http.Error(w, "Failed to retrieve rooms", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var rooms []models.Room
	for rows.Next() {
		var room models.Room
		err := rows.Scan(
			&room.ID, &room.Slug, &room.Name, &room.ScheduledAt, &room.DurationMinutes,
			&room.HasPassword, &room.WaitingRoomEnabled, &room.StreamKeyID,
			&room.WatermarkMode, &room.WatermarkText, &room.WatermarkLogoPath,
			&room.WatermarkLogoPosition, &room.WatermarkOpacity, &room.Status,
			&room.CreatedAt, &room.StartedAt, &room.EndedAt,
		)
		if err != nil {
			logger.Error("Failed to scan room row", "error", err)
			http.Error(w, "Failed to retrieve rooms", http.StatusInternalServerError)
			return
		}
		rooms = append(rooms, room)
	}

	respondJSON(w, rooms)
}

// CreateRoomRequest is the request body for creating a room
type CreateRoomRequest struct {
	Slug                  string     `json:"slug"`
	Name                  string     `json:"name"`
	ScheduledAt           *time.Time `json:"scheduledAt,omitempty"`
	DurationMinutes       *int       `json:"durationMinutes,omitempty"`
	Password              *string    `json:"password,omitempty"`
	WaitingRoomEnabled    bool       `json:"waitingRoomEnabled"`
	StreamKeyID           *string    `json:"streamKeyId,omitempty"`
	WatermarkMode         string     `json:"watermarkMode"`
	WatermarkText         *string    `json:"watermarkText,omitempty"`
	WatermarkLogoPosition string     `json:"watermarkLogoPosition,omitempty"`
	WatermarkOpacity      *float64   `json:"watermarkOpacity,omitempty"`
}

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
	id := generateID()

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

	// Insert room
	_, err := h.db.Exec(`
		INSERT INTO rooms (
			id, slug, name, scheduled_at, duration_minutes, password_hash,
			waiting_room_enabled, stream_key_id, watermark_mode, watermark_text,
			watermark_logo_position, watermark_opacity
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, id, req.Slug, req.Name, req.ScheduledAt, req.DurationMinutes, passwordHash,
		req.WaitingRoomEnabled, req.StreamKeyID, req.WatermarkMode, req.WatermarkText,
		req.WatermarkLogoPosition, watermarkOpacity)

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
		HasPassword:           passwordHash != nil,
		WaitingRoomEnabled:    req.WaitingRoomEnabled,
		StreamKeyID:           req.StreamKeyID,
		WatermarkMode:         req.WatermarkMode,
		WatermarkText:         req.WatermarkText,
		WatermarkLogoPosition: req.WatermarkLogoPosition,
		WatermarkOpacity:      watermarkOpacity,
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

	if len(setClauses) == 0 {
		http.Error(w, "No valid fields to update", http.StatusBadRequest)
		return
	}

	query := "UPDATE rooms SET " + strings.Join(setClauses, ", ") + " WHERE slug = ?"
	args = append(args, slug)

	result, err := h.db.Exec(query, args...)
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

// Delete deletes a room
func (h *RoomHandler) Delete(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")

	result, err := h.db.Exec("DELETE FROM rooms WHERE slug = ?", slug)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		http.Error(w, "Room not found", http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// EndSession ends a live session
func (h *RoomHandler) EndSession(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")

	now := time.Now()
	result, err := h.db.Exec(`
		UPDATE rooms SET status = 'ended', ended_at = ? WHERE slug = ? AND status = 'live'
	`, now, slug)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		http.Error(w, "Room not found or not live", http.StatusNotFound)
		return
	}

	if h.hub != nil {
		h.hub.BroadcastJSON(slug, "room:ended", map[string]interface{}{}, "")
	}

	w.WriteHeader(http.StatusNoContent)
}

// PublicInfo returns public information about a room (for join flow)
func (h *RoomHandler) PublicInfo(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")

	var info struct {
		Name               string     `json:"name"`
		HasPassword        bool       `json:"hasPassword"`
		WaitingRoomEnabled bool       `json:"waitingRoomEnabled"`
		Status             string     `json:"status"`
		ScheduledAt        *time.Time `json:"scheduledAt,omitempty"`
	}

	err := h.db.QueryRow(`
		SELECT name, password_hash IS NOT NULL, waiting_room_enabled, status, scheduled_at
		FROM rooms WHERE slug = ?
	`, slug).Scan(&info.Name, &info.HasPassword, &info.WaitingRoomEnabled, &info.Status, &info.ScheduledAt)

	if err != nil {
		http.Error(w, "Room not found", http.StatusNotFound)
		return
	}

	respondJSON(w, info)
}

// JoinRequest is the request body for joining a room
type JoinRequest struct {
	Name     string  `json:"name"`
	Password *string `json:"password,omitempty"`
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
	var scheduledAt *time.Time

	err := h.db.QueryRow(`
		SELECT id, COALESCE(password_hash, ''), waiting_room_enabled, status, scheduled_at
		FROM rooms WHERE slug = ?
	`, slug).Scan(&roomID, &passwordHash, &waitingRoom, &roomStatus, &scheduledAt)

	if err != nil {
		http.Error(w, "Room not found", http.StatusNotFound)
		return
	}

	if roomStatus == "ended" {
		http.Error(w, "Room has ended", http.StatusGone)
		return
	}

	if scheduledAt != nil && time.Now().Before(scheduledAt.Add(-earlyAccessWindow)) {
		http.Error(w, "Session not open yet", http.StatusForbidden)
		return
	}

	// Check password
	if passwordHash != "" {
		if req.Password == nil {
			http.Error(w, "Password required", http.StatusUnauthorized)
			return
		}
		if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(*req.Password)); err != nil {
			http.Error(w, "Invalid password", http.StatusUnauthorized)
			return
		}
	}

	// Create participant
	participantID := generateID()
	color := assignColor(participantID)
	isAdmitted := !waitingRoom // Auto-admit if no waiting room

	_, err = h.db.Exec(`
		INSERT INTO participants (id, room_id, name, role, color, is_admitted)
		VALUES (?, ?, ?, 'viewer', ?, ?)
	`, participantID, roomID, req.Name, color, isAdmitted)

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

	// Track join request
	metrics.Get().TotalJoinRequests.Add(1)
	if !isAdmitted {
		// Track waiting participant
		metrics.Get().WaitingParticipants.Add(1)
	}

	response := map[string]interface{}{
		"participantId": participantID,
		"token":         token,
		"isAdmitted":    isAdmitted,
		"waitingRoom":   waitingRoom && !isAdmitted,
		"color":         color,
		"name":          req.Name,
	}

	respondJSON(w, response)
}

// ListWaiting lists participants in the waiting room
func (h *RoomHandler) ListWaiting(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")

	rows, err := h.db.Query(`
		SELECT p.id, p.name, p.joined_at
		FROM participants p
		JOIN rooms r ON r.id = p.room_id
		WHERE r.slug = ? AND p.is_admitted = FALSE
		ORDER BY p.joined_at
	`, slug)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var participants []map[string]interface{}
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

	respondJSON(w, participants)
}

// AdmitParticipant admits a specific participant from waiting room
func (h *RoomHandler) AdmitParticipant(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	participantID := r.PathValue("id")

	result, err := h.db.Exec(`
		UPDATE participants SET is_admitted = TRUE
		WHERE id = ? AND room_id = (SELECT id FROM rooms WHERE slug = ?)
	`, participantID, slug)

	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		http.Error(w, "Participant not found", http.StatusNotFound)
		return
	}

	// Track admitted participant
	metrics.Get().WaitingParticipants.Add(-1)

	// Notify waiting participant via SSE
	h.waitingManager.NotifyAdmitted(participantID)

	w.WriteHeader(http.StatusNoContent)
}

// AdmitAll admits all waiting participants
func (h *RoomHandler) AdmitAll(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")

	// First, get list of waiting participants for notification
	rows, err := h.db.Query(`
		SELECT p.id FROM participants p
		JOIN rooms r ON r.id = p.room_id
		WHERE r.slug = ? AND p.is_admitted = FALSE
	`, slug)
	if err != nil {
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
	rows.Close()

	// Update all to admitted
	_, err = h.db.Exec(`
		UPDATE participants SET is_admitted = TRUE
		WHERE room_id = (SELECT id FROM rooms WHERE slug = ?) AND is_admitted = FALSE
	`, slug)

	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	// Track admitted participants
	metrics.Get().WaitingParticipants.Add(-int64(len(waitingIDs)))

	// Notify all waiting participants via SSE
	h.waitingManager.NotifyAllAdmitted(waitingIDs)

	w.WriteHeader(http.StatusNoContent)
}

// CheckParticipantStatus checks if a participant has been admitted
func (h *RoomHandler) CheckParticipantStatus(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	participantID := r.PathValue("id")
	token := r.URL.Query().Get("token")

	if token == "" {
		http.Error(w, "Missing token", http.StatusUnauthorized)
		return
	}

	payload, err := h.tokenManager.ValidateToken(token)
	if err != nil || payload.ParticipantID != participantID || payload.RoomSlug != slug {
		http.Error(w, "Invalid token", http.StatusUnauthorized)
		return
	}

	var isAdmitted bool
	var roomStatus string

	err = h.db.QueryRow(`
		SELECT p.is_admitted, r.status
		FROM participants p
		JOIN rooms r ON r.id = p.room_id
		WHERE r.slug = ? AND p.id = ?
	`, slug, participantID).Scan(&isAdmitted, &roomStatus)

	if err != nil {
		http.Error(w, "Participant not found", http.StatusNotFound)
		return
	}

	respondJSON(w, map[string]interface{}{
		"isAdmitted": isAdmitted,
		"roomStatus": roomStatus,
	})
}

// OnStreamStart is called when OBS starts streaming
func (h *RoomHandler) OnStreamStart(streamKeyToken string) error {
	now := time.Now()

	// Use transaction to ensure atomicity of room lookup and status update
	// This prevents race conditions with multiple OBS connections
	tx, err := h.db.Begin()
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
	err = tx.QueryRow(`
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
		result, err := tx.Exec(`
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

	// Bind ingest tracks to room for distribution
	if h.sfu != nil {
		if err := h.sfu.BindIngestToRoom(streamKeyToken, roomSlug); err != nil {
			// Log but don't fail - room is already marked live
			logger.Warn("Failed to bind ingest to room", "room", roomSlug, "error", err)
		}
	}

	if roomStatus == "pending" {
		// Notify all connected clients that room is live
		if h.hub != nil {
			h.hub.BroadcastJSON(roomSlug, "room:live", map[string]interface{}{}, "")
		}

		// Initiate WebRTC subscriptions for all connected clients
		if h.onRoomLive != nil {
			h.onRoomLive(roomSlug)
		}
	} else if h.hub != nil {
		h.hub.BroadcastJSON(roomSlug, "stream:resumed", map[string]interface{}{}, "")
	}

	logger.Info("Room is now live", "room", roomSlug)
	return nil
}

// OnStreamEnd is called when OBS stops streaming
func (h *RoomHandler) OnStreamEnd(streamKeyToken string) {
	// Find the room associated with this stream key
	var roomSlug string
	err := h.db.QueryRow(`
		SELECT r.slug FROM rooms r
		JOIN stream_keys sk ON sk.id = r.stream_key_id
		WHERE sk.key_token = ? AND r.status = 'live'
	`, streamKeyToken).Scan(&roomSlug)

	if err != nil {
		// No live room for this stream key - that's fine
		return
	}

	// Notify connected clients that the stream has paused
	// The admin may reconnect, so we don't end the room
	if h.hub != nil {
		h.hub.BroadcastJSON(roomSlug, "stream:paused", map[string]interface{}{
			"message": "Stream disconnected. Waiting for reconnection...",
		}, "")
	}

	// Schedule room end if OBS does not reconnect in time
	h.scheduleStreamEnd(roomSlug)

	logger.Info("Stream paused (OBS disconnected)", "room", roomSlug)
}

func (h *RoomHandler) getRoomBySlug(slug string) (*models.Room, error) {
	var room models.Room
	err := h.db.QueryRow(`
		SELECT id, slug, name, scheduled_at, duration_minutes,
		       password_hash IS NOT NULL, waiting_room_enabled, stream_key_id,
		       watermark_mode, watermark_text, watermark_logo_path,
		       watermark_logo_position, watermark_opacity, status,
		       created_at, started_at, ended_at
		FROM rooms WHERE slug = ?
	`, slug).Scan(
		&room.ID, &room.Slug, &room.Name, &room.ScheduledAt, &room.DurationMinutes,
		&room.HasPassword, &room.WaitingRoomEnabled, &room.StreamKeyID,
		&room.WatermarkMode, &room.WatermarkText, &room.WatermarkLogoPath,
		&room.WatermarkLogoPosition, &room.WatermarkOpacity, &room.Status,
		&room.CreatedAt, &room.StartedAt, &room.EndedAt,
	)
	return &room, err
}

// Helper functions

func generateID() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

var cursorColors = []string{
	"#e63946", "#f4a261", "#2a9d8f", "#264653",
	"#e76f51", "#8338ec", "#ff006e", "#3a86ff",
}

func assignColor(id string) string {
	// Simple hash-based color assignment
	hash := 0
	for _, c := range id {
		hash = (hash*31 + int(c)) % len(cursorColors)
	}
	return cursorColors[hash]
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
	token := r.URL.Query().Get("token")

	if token == "" {
		http.Error(w, "Missing token", http.StatusUnauthorized)
		return
	}

	payload, err := h.tokenManager.ValidateToken(token)
	if err != nil || payload.ParticipantID != participantID || payload.RoomSlug != slug {
		http.Error(w, "Invalid token", http.StatusUnauthorized)
		return
	}

	// Verify participant exists and is in waiting state
	var isAdmitted bool
	var roomStatus string
	err = h.db.QueryRow(`
		SELECT p.is_admitted, r.status
		FROM participants p
		JOIN rooms r ON r.id = p.room_id
		WHERE r.slug = ? AND p.id = ?
	`, slug, participantID).Scan(&isAdmitted, &roomStatus)

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

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Subscribe to notifications
	ch := h.waitingManager.Subscribe(participantID)
	defer h.waitingManager.Unsubscribe(participantID)

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
