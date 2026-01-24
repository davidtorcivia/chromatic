package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"time"

	"chromatic/internal/database"
	"chromatic/internal/models"

	"golang.org/x/crypto/bcrypt"
)

var slugRegex = regexp.MustCompile(`^[a-z0-9-]{3,64}$`)

// RoomHandler handles room-related HTTP requests
type RoomHandler struct {
	db  *database.DB
	sfu interface {
		BindIngestToRoom(streamKeyToken, roomSlug string) error
	}
	hub interface {
		BroadcastJSON(roomSlug string, msgType string, payload interface{}, excludeID string) error
	}
	onRoomLive func(roomSlug string) // Called when room goes live
}

// NewRoomHandler creates a new RoomHandler
func NewRoomHandler(db *database.DB) *RoomHandler {
	return &RoomHandler{db: db}
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
		http.Error(w, "Database error", http.StatusInternalServerError)
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
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}
		rooms = append(rooms, room)
	}

	respondJSON(w, rooms)
}

// CreateRoomRequest is the request body for creating a room
type CreateRoomRequest struct {
	Slug               string     `json:"slug"`
	Name               string     `json:"name"`
	ScheduledAt        *time.Time `json:"scheduledAt,omitempty"`
	DurationMinutes    *int       `json:"durationMinutes,omitempty"`
	Password           *string    `json:"password,omitempty"`
	WaitingRoomEnabled bool       `json:"waitingRoomEnabled"`
	StreamKeyID        *string    `json:"streamKeyId,omitempty"`
	WatermarkMode      string     `json:"watermarkMode"`
	WatermarkText      *string    `json:"watermarkText,omitempty"`
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

	// Validate name
	if len(req.Name) < 1 || len(req.Name) > 100 {
		http.Error(w, "Name must be 1-100 characters", http.StatusBadRequest)
		return
	}

	// Generate ID
	id := generateID()

	// Hash password if provided
	var passwordHash *string
	if req.Password != nil && len(*req.Password) >= 4 {
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

	// Insert room
	_, err := h.db.Exec(`
		INSERT INTO rooms (
			id, slug, name, scheduled_at, duration_minutes, password_hash,
			waiting_room_enabled, stream_key_id, watermark_mode, watermark_text
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, id, req.Slug, req.Name, req.ScheduledAt, req.DurationMinutes, passwordHash,
		req.WaitingRoomEnabled, req.StreamKeyID, req.WatermarkMode, req.WatermarkText)

	if err != nil {
		// Check for unique constraint violation
		http.Error(w, "Slug already exists", http.StatusConflict)
		return
	}

	// Fetch the created room
	room := models.Room{
		ID:                 id,
		Slug:               req.Slug,
		Name:               req.Name,
		ScheduledAt:        req.ScheduledAt,
		DurationMinutes:    req.DurationMinutes,
		HasPassword:        passwordHash != nil,
		WaitingRoomEnabled: req.WaitingRoomEnabled,
		StreamKeyID:        req.StreamKeyID,
		WatermarkMode:      req.WatermarkMode,
		WatermarkText:      req.WatermarkText,
		Status:             models.RoomStatusPending,
		CreatedAt:          time.Now(),
	}

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

	var updates map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Build update query dynamically
	// Note: In production, use a proper query builder
	allowed := map[string]string{
		"name":               "name",
		"scheduledAt":        "scheduled_at",
		"durationMinutes":    "duration_minutes",
		"waitingRoomEnabled": "waiting_room_enabled",
		"streamKeyId":        "stream_key_id",
		"watermarkMode":      "watermark_mode",
		"watermarkText":      "watermark_text",
	}

	query := "UPDATE rooms SET "
	args := []interface{}{}
	first := true

	for key, col := range allowed {
		if val, ok := updates[key]; ok {
			if !first {
				query += ", "
			}
			query += col + " = ?"
			args = append(args, val)
			first = false
		}
	}

	if len(args) == 0 {
		http.Error(w, "No valid fields to update", http.StatusBadRequest)
		return
	}

	query += " WHERE slug = ?"
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

	// Validate name
	if len(req.Name) < 1 || len(req.Name) > 50 {
		http.Error(w, "Name must be 1-50 characters", http.StatusBadRequest)
		return
	}

	// Get room
	var roomID, passwordHash string
	var waitingRoom bool
	var roomStatus string

	err := h.db.QueryRow(`
		SELECT id, COALESCE(password_hash, ''), waiting_room_enabled, status
		FROM rooms WHERE slug = ?
	`, slug).Scan(&roomID, &passwordHash, &waitingRoom, &roomStatus)

	if err != nil {
		http.Error(w, "Room not found", http.StatusNotFound)
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

	// Generate join token
	token := generateID()

	response := map[string]interface{}{
		"participantId": participantID,
		"token":         token,
		"isAdmitted":    isAdmitted,
		"waitingRoom":   waitingRoom && !isAdmitted,
		"color":         color,
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

	w.WriteHeader(http.StatusNoContent)
}

// AdmitAll admits all waiting participants
func (h *RoomHandler) AdmitAll(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")

	_, err := h.db.Exec(`
		UPDATE participants SET is_admitted = TRUE
		WHERE room_id = (SELECT id FROM rooms WHERE slug = ?) AND is_admitted = FALSE
	`, slug)

	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// CheckParticipantStatus checks if a participant has been admitted
func (h *RoomHandler) CheckParticipantStatus(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	participantID := r.PathValue("id")

	var isAdmitted bool
	var roomStatus string

	err := h.db.QueryRow(`
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

	// Find the room bound to this stream key
	var roomSlug string
	err := h.db.QueryRow(`
		SELECT r.slug FROM rooms r
		JOIN stream_keys sk ON sk.id = r.stream_key_id
		WHERE sk.key_token = ? AND r.status = 'pending'
		ORDER BY r.scheduled_at NULLS LAST, r.created_at
		LIMIT 1
	`, streamKeyToken).Scan(&roomSlug)

	if err != nil {
		// No room bound to this stream key yet - that's okay
		return nil
	}

	// Update room status to live
	_, err = h.db.Exec(`
		UPDATE rooms SET status = 'live', started_at = ?
		WHERE slug = ? AND status = 'pending'
	`, now, roomSlug)
	if err != nil {
		return err
	}

	// Bind ingest tracks to room for distribution
	if h.sfu != nil {
		if err := h.sfu.BindIngestToRoom(streamKeyToken, roomSlug); err != nil {
			// Log but don't fail - room is already marked live
			log.Printf("Warning: failed to bind ingest to room %s: %v", roomSlug, err)
		}
	}

	// Notify all connected clients that room is live
	if h.hub != nil {
		h.hub.BroadcastJSON(roomSlug, "room:live", map[string]interface{}{}, "")
	}

	// Initiate WebRTC subscriptions for all connected clients
	if h.onRoomLive != nil {
		h.onRoomLive(roomSlug)
	}

	log.Printf("Room %s is now live", roomSlug)
	return nil
}

// OnStreamEnd is called when OBS stops streaming
func (h *RoomHandler) OnStreamEnd(streamKeyToken string) {
	// Note: We don't automatically end the room when OBS disconnects
	// The admin may want to reconnect. Use the timeout logic in the SFU instead.
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

// HealthCheck is a simple health check endpoint
func HealthCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}
