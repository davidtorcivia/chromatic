package handlers

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"chromatic/internal/database"
	"chromatic/internal/logger"
)

const (
	// SessionCookieName is the name of the session cookie
	SessionCookieName = "chromatic_session"
	// SessionDuration is how long sessions last
	SessionDuration = 24 * time.Hour
)

// AuthHandler handles authentication endpoints
type AuthHandler struct {
	db             *database.DB
	adminToken     string
	productionMode bool
}

// Session represents an authenticated session
type Session struct {
	ID        string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// NewAuthHandler creates a new auth handler with database persistence
func NewAuthHandler(db *database.DB, adminToken string, productionMode bool) *AuthHandler {
	h := &AuthHandler{
		db:             db,
		adminToken:     adminToken,
		productionMode: productionMode,
	}

	// Start session cleanup goroutine
	go h.cleanupExpiredSessions()

	return h
}

// cleanupExpiredSessions periodically removes expired sessions from the database
func (h *AuthHandler) cleanupExpiredSessions() {
	ticker := time.NewTicker(15 * time.Minute)
	for range ticker.C {
		ctx, cancel := database.WithTimeout(context.Background())
		result, err := h.db.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at < ?", time.Now())
		cancel()
		if err != nil {
			logger.Error("Failed to cleanup expired sessions", "error", err)
			continue
		}
		if affected, _ := result.RowsAffected(); affected > 0 {
			logger.Debug("Cleaned up expired sessions", "count", affected)
		}
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

	// Validate admin token with constant-time comparison. Reject an empty
	// configured token: two empty strings compare equal, so without the
	// length check an empty ADMIN_TOKEN would authenticate an empty token.
	if len(h.adminToken) == 0 || subtle.ConstantTimeCompare([]byte(req.Token), []byte(h.adminToken)) != 1 {
		http.Error(w, "Invalid token", http.StatusUnauthorized)
		return
	}

	// Generate session ID
	sessionID, err := generateSessionID()
	if err != nil {
		logger.Error("Failed to generate session ID", "error", err)
		http.Error(w, "Authentication service temporarily unavailable", http.StatusInternalServerError)
		return
	}

	// Create session in database
	expiresAt := time.Now().Add(SessionDuration)
	ctx, cancel := database.WithTimeout(r.Context())
	_, err = h.db.ExecContext(ctx,
		"INSERT INTO sessions (id, expires_at) VALUES (?, ?)",
		sessionID, expiresAt,
	)
	cancel()
	if err != nil {
		logger.Error("Failed to create session", "error", err)
		http.Error(w, "Authentication service temporarily unavailable", http.StatusInternalServerError)
		return
	}

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

	logger.Info("Admin logged in successfully")
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
		// Remove session from database
		ctx, cancel := database.WithTimeout(r.Context())
		_, err = h.db.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?", cookie.Value)
		cancel()
		if err != nil {
			logger.Warn("Failed to delete session from database", "error", err)
		}
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
	ctx, cancel := database.WithTimeout(context.Background())
	defer cancel()
	var expiresAt time.Time
	err := h.db.QueryRowContext(ctx,
		"SELECT expires_at FROM sessions WHERE id = ?",
		sessionID,
	).Scan(&expiresAt)

	if err != nil {
		// Session not found or database error
		return false
	}

	if time.Now().After(expiresAt) {
		// Session expired, clean it up
		ctx2, cancel2 := database.WithTimeout(context.Background())
		_, _ = h.db.ExecContext(ctx2, "DELETE FROM sessions WHERE id = ?", sessionID)
		cancel2()
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
