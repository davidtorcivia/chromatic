package handlers

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"
)

const (
	// SessionCookieName is the name of the session cookie
	SessionCookieName = "chromatic_session"
	// SessionDuration is how long sessions last
	SessionDuration = 24 * time.Hour
)

// AuthHandler handles authentication endpoints
type AuthHandler struct {
	adminToken     string
	productionMode bool

	// Session store (in-memory for simplicity)
	sessions   map[string]*Session
	sessionsMu sync.RWMutex
}

// Session represents an authenticated session
type Session struct {
	ID        string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// NewAuthHandler creates a new auth handler
func NewAuthHandler(adminToken string, productionMode bool) *AuthHandler {
	h := &AuthHandler{
		adminToken:     adminToken,
		productionMode: productionMode,
		sessions:       make(map[string]*Session),
	}

	// Start session cleanup goroutine
	go h.cleanupExpiredSessions()

	return h
}

// cleanupExpiredSessions periodically removes expired sessions
func (h *AuthHandler) cleanupExpiredSessions() {
	ticker := time.NewTicker(15 * time.Minute)
	for range ticker.C {
		h.sessionsMu.Lock()
		now := time.Now()
		for id, session := range h.sessions {
			if now.After(session.ExpiresAt) {
				delete(h.sessions, id)
			}
		}
		h.sessionsMu.Unlock()
	}
}

// Login handles admin login requests
func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Token string `json:"token"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Validate admin token with constant-time comparison
	if subtle.ConstantTimeCompare([]byte(req.Token), []byte(h.adminToken)) != 1 {
		http.Error(w, "Invalid token", http.StatusUnauthorized)
		return
	}

	// Generate session ID
	sessionID, err := generateSessionID()
	if err != nil {
		log.Printf("Failed to generate session ID: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Create session
	session := &Session{
		ID:        sessionID,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(SessionDuration),
	}

	h.sessionsMu.Lock()
	h.sessions[sessionID] = session
	h.sessionsMu.Unlock()

	// Set httpOnly cookie
	cookie := &http.Cookie{
		Name:     SessionCookieName,
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		Secure:   h.productionMode, // Only require Secure in production
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(SessionDuration.Seconds()),
	}
	http.SetCookie(w, cookie)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Login successful",
	})

	log.Printf("Admin logged in successfully")
}

// Logout handles admin logout requests
func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get session from cookie
	cookie, err := r.Cookie(SessionCookieName)
	if err == nil {
		// Remove session from store
		h.sessionsMu.Lock()
		delete(h.sessions, cookie.Value)
		h.sessionsMu.Unlock()
	}

	// Clear cookie
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   h.productionMode,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1, // Delete cookie
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Logged out",
	})
}

// ValidateSession checks if a session ID is valid
func (h *AuthHandler) ValidateSession(sessionID string) bool {
	h.sessionsMu.RLock()
	session, exists := h.sessions[sessionID]
	h.sessionsMu.RUnlock()

	if !exists {
		return false
	}

	if time.Now().After(session.ExpiresAt) {
		// Session expired, clean it up
		h.sessionsMu.Lock()
		delete(h.sessions, sessionID)
		h.sessionsMu.Unlock()
		return false
	}

	return true
}

// generateSessionID generates a cryptographically secure session ID
func generateSessionID() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}
