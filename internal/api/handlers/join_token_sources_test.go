package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"chromatic/internal/config"
)

func TestJoinTokenFromRequest_QueryPolicy(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/events?token=query-token", nil)

	if got := joinTokenFromRequest(req, "review-room", false); got != "" {
		t.Fatalf("expected query token to be ignored when disallowed, got %q", got)
	}
	if got := joinTokenFromRequest(req, "review-room", true); got != "query-token" {
		t.Fatalf("expected development query token, got %q", got)
	}

	req.AddCookie(&http.Cookie{Name: JoinTokenCookieName("review-room"), Value: "cookie-token"})
	if got := joinTokenFromRequest(req, "review-room", false); got != "cookie-token" {
		t.Fatalf("expected cookie token to be preferred, got %q", got)
	}
}

func TestFileHandlerGetJoinToken_ProductionDisallowsQuery(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/rooms/review/files/file-1?token=query-token", nil)

	prod := NewFileHandler(nil, &config.Config{ProductionMode: true}, []byte("test-secret"))
	if got := prod.getJoinToken(req, "review-room"); got != "" {
		t.Fatalf("expected production query token to be ignored, got %q", got)
	}

	dev := NewFileHandler(nil, &config.Config{ProductionMode: false}, []byte("test-secret"))
	if got := dev.getJoinToken(req, "review-room"); got != "query-token" {
		t.Fatalf("expected development query token, got %q", got)
	}

	req.AddCookie(&http.Cookie{Name: JoinTokenCookieName("review-room"), Value: "cookie-token"})
	if got := prod.getJoinToken(req, "review-room"); got != "cookie-token" {
		t.Fatalf("expected cookie token in production, got %q", got)
	}

	req.Header.Set("X-Join-Token", "header-token")
	if got := prod.getJoinToken(req, "review-room"); got != "header-token" {
		t.Fatalf("expected header token to be preferred, got %q", got)
	}
}

func TestWebSocketHandler_ProductionIgnoresQueryToken(t *testing.T) {
	secret := []byte("test-secret")
	tm := NewTokenManager(secret)
	token, err := tm.GenerateToken("participant-1", "review-room", "Viewer One", time.Hour)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	handler := NewWebSocketHandler(nil, nil, nil, nil, true, secret, nil)
	req := httptest.NewRequest(http.MethodGet, "/ws/room/review-room?token="+token+"&name=Viewer%20One", nil)
	req.SetPathValue("slug", "review-room")
	rec := httptest.NewRecorder()

	handler.HandleConnection(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected missing-token response, got status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Missing token") {
		t.Fatalf("expected missing-token body, got %q", rec.Body.String())
	}
}

func TestBuildJoinResponse_TokenPolicy(t *testing.T) {
	serverTime := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	prod := buildJoinResponse("participant-1", "signed-token", true, "#48b6a6", "Viewer", "viewer", serverTime, false)
	if _, ok := prod["token"]; ok {
		t.Fatalf("production join response must not expose signed join token: %#v", prod)
	}
	if prod["participantId"] != "participant-1" || prod["isAdmitted"] != true {
		t.Fatalf("production response lost required session metadata: %#v", prod)
	}

	dev := buildJoinResponse("participant-1", "signed-token", true, "#48b6a6", "Viewer", "viewer", serverTime, true)
	if dev["token"] != "signed-token" {
		t.Fatalf("development/API compatibility response should include token, got %#v", dev["token"])
	}
}
