package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"chromatic/internal/database"
	"chromatic/internal/logger"
)

func init() {
	// Initialize logger for tests
	logger.Initialize(false)
}

func setupAuthTest(t *testing.T) (*AuthHandler, func()) {
	db, dbCleanup := database.NewTestDB(t)
	handler := NewAuthHandler(db, "test-admin-token", false)
	return handler, dbCleanup
}

func TestAuthHandler_Login_Success(t *testing.T) {
	handler, cleanup := setupAuthTest(t)
	defer cleanup()

	body := map[string]string{"token": "test-admin-token"}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.Login(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d: %s", http.StatusOK, rr.Code, rr.Body.String())
	}

	// Check response
	var resp map[string]interface{}
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["success"] != true {
		t.Errorf("expected success: true, got %v", resp["success"])
	}

	// Check cookie was set
	cookies := rr.Result().Cookies()
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == SessionCookieName {
			sessionCookie = c
			break
		}
	}

	if sessionCookie == nil {
		t.Error("expected session cookie to be set")
	} else {
		if !sessionCookie.HttpOnly {
			t.Error("session cookie should be HttpOnly")
		}
		if sessionCookie.SameSite != http.SameSiteStrictMode {
			t.Error("session cookie should have SameSite=Strict")
		}
	}
}

func TestAuthHandler_Login_InvalidToken(t *testing.T) {
	handler, cleanup := setupAuthTest(t)
	defer cleanup()

	body := map[string]string{"token": "wrong-token"}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.Login(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected status %d, got %d", http.StatusUnauthorized, rr.Code)
	}
}

func TestAuthHandler_Login_InvalidMethod(t *testing.T) {
	handler, cleanup := setupAuthTest(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/api/auth/login", nil)
	rr := httptest.NewRecorder()

	handler.Login(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status %d, got %d", http.StatusMethodNotAllowed, rr.Code)
	}
}

func TestAuthHandler_Login_InvalidBody(t *testing.T) {
	handler, cleanup := setupAuthTest(t)
	defer cleanup()

	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader([]byte("invalid json")))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.Login(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, rr.Code)
	}
}

func TestAuthHandler_Logout(t *testing.T) {
	handler, cleanup := setupAuthTest(t)
	defer cleanup()

	// First login to get a session
	loginBody := map[string]string{"token": "test-admin-token"}
	loginBytes, _ := json.Marshal(loginBody)
	loginReq := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(loginBytes))
	loginReq.Header.Set("Content-Type", "application/json")
	loginRR := httptest.NewRecorder()
	handler.Login(loginRR, loginReq)

	// Get the session cookie
	var sessionCookie *http.Cookie
	for _, c := range loginRR.Result().Cookies() {
		if c.Name == SessionCookieName {
			sessionCookie = c
			break
		}
	}

	if sessionCookie == nil {
		t.Fatal("no session cookie from login")
	}

	// Now logout
	logoutReq := httptest.NewRequest("POST", "/api/auth/logout", nil)
	logoutReq.AddCookie(sessionCookie)
	logoutRR := httptest.NewRecorder()

	handler.Logout(logoutRR, logoutReq)

	if logoutRR.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, logoutRR.Code)
	}

	// Check that cookie is cleared (MaxAge = -1)
	for _, c := range logoutRR.Result().Cookies() {
		if c.Name == SessionCookieName && c.MaxAge != -1 {
			t.Error("session cookie should have MaxAge=-1 after logout")
		}
	}

	// Verify session is no longer valid
	if handler.ValidateSession(sessionCookie.Value) {
		t.Error("session should be invalid after logout")
	}
}

func TestAuthHandler_Logout_InvalidMethod(t *testing.T) {
	handler, cleanup := setupAuthTest(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/api/auth/logout", nil)
	rr := httptest.NewRecorder()

	handler.Logout(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status %d, got %d", http.StatusMethodNotAllowed, rr.Code)
	}
}

func TestAuthHandler_ValidateSession(t *testing.T) {
	handler, cleanup := setupAuthTest(t)
	defer cleanup()

	// Login to create a valid session
	loginBody := map[string]string{"token": "test-admin-token"}
	loginBytes, _ := json.Marshal(loginBody)
	loginReq := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(loginBytes))
	loginReq.Header.Set("Content-Type", "application/json")
	loginRR := httptest.NewRecorder()
	handler.Login(loginRR, loginReq)

	// Get session ID from cookie
	var sessionID string
	for _, c := range loginRR.Result().Cookies() {
		if c.Name == SessionCookieName {
			sessionID = c.Value
			break
		}
	}

	if sessionID == "" {
		t.Fatal("no session ID from login")
	}

	// Test valid session
	if !handler.ValidateSession(sessionID) {
		t.Error("session should be valid after login")
	}

	// Test invalid session
	if handler.ValidateSession("invalid-session-id") {
		t.Error("invalid session ID should not be valid")
	}
}

func TestAuthHandler_EmptyToken(t *testing.T) {
	handler, cleanup := setupAuthTest(t)
	defer cleanup()

	body := map[string]string{"token": ""}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.Login(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected status %d for empty token, got %d", http.StatusUnauthorized, rr.Code)
	}
}
