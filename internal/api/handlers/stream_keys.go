package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"chromatic/internal/database"
)

// StreamKeyHandler handles stream key-related HTTP requests
type StreamKeyHandler struct {
	db *database.DB
}

// NewStreamKeyHandler creates a new StreamKeyHandler
func NewStreamKeyHandler(db *database.DB) *StreamKeyHandler {
	return &StreamKeyHandler{db: db}
}

// StreamKey represents a stream key response
type StreamKey struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	KeyToken  string `json:"keyToken"`
	CreatedAt string `json:"createdAt"`
}

// List returns all stream keys
func (h *StreamKeyHandler) List(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.Query(`
		SELECT id, name, key_token, created_at FROM stream_keys ORDER BY created_at DESC
	`)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	keys := []StreamKey{}
	for rows.Next() {
		var key StreamKey
		if err := rows.Scan(&key.ID, &key.Name, &key.KeyToken, &key.CreatedAt); err != nil {
			continue
		}
		keys = append(keys, key)
	}

	respondJSON(w, keys)
}

// CreateStreamKeyRequest is the request body for creating a stream key
type CreateStreamKeyRequest struct {
	Name string `json:"name"`
}

// Create creates a new stream key
func (h *StreamKeyHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req CreateStreamKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if len(req.Name) < 1 || len(req.Name) > 100 {
		http.Error(w, "Name must be 1-100 characters", http.StatusBadRequest)
		return
	}

	id := generateStreamKeyID()
	keyToken := generateKeyToken()

	_, err := h.db.Exec(`
		INSERT INTO stream_keys (id, name, key_token) VALUES (?, ?, ?)
	`, id, req.Name, keyToken)

	if err != nil {
		http.Error(w, "Failed to create stream key", http.StatusInternalServerError)
		return
	}

	key := StreamKey{
		ID:       id,
		Name:     req.Name,
		KeyToken: keyToken,
	}

	w.WriteHeader(http.StatusCreated)
	respondJSON(w, key)
}

// Delete deletes a stream key
func (h *StreamKeyHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	result, err := h.db.Exec("DELETE FROM stream_keys WHERE id = ?", id)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		http.Error(w, "Stream key not found", http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ValidateKey checks if a stream key token is valid
func (h *StreamKeyHandler) ValidateKey(token string) (bool, error) {
	var count int
	err := h.db.QueryRow("SELECT COUNT(*) FROM stream_keys WHERE key_token = ?", token).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// GetKeyID gets the stream key ID for a token
func (h *StreamKeyHandler) GetKeyID(token string) (string, error) {
	var id string
	err := h.db.QueryRow("SELECT id FROM stream_keys WHERE key_token = ?", token).Scan(&id)
	return id, err
}

func generateStreamKeyID() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

func generateKeyToken() string {
	// Generate a longer, more secure token for stream keys
	bytes := make([]byte, 32)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}
