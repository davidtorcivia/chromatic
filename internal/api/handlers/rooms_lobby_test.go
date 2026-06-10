package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"chromatic/internal/api/middleware"
	"chromatic/internal/database"
)

// createRoomForTest creates a room through the Create handler and fails the
// test on any non-201 response.
func createRoomForTest(t *testing.T, handler *RoomHandler, body map[string]interface{}) {
	t.Helper()
	bodyBytes, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/rooms", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.Create(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("failed to create room %v: %d %s", body["slug"], rr.Code, rr.Body.String())
	}
}

// joinRoomForTest performs a join request and returns the recorder.
func joinRoomForTest(t *testing.T, handler *RoomHandler, slug string, body map[string]interface{}, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	bodyBytes, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/rooms/"+slug+"/join", bytes.NewReader(bodyBytes))
	req.SetPathValue("slug", slug)
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	handler.Join(rr, req)
	return rr
}

func participantAdmitted(t *testing.T, db *database.DB, participantID string) bool {
	t.Helper()
	var admitted bool
	if err := db.QueryRow(`SELECT is_admitted FROM participants WHERE id = ?`, participantID).Scan(&admitted); err != nil {
		t.Fatalf("failed to query participant %s: %v", participantID, err)
	}
	return admitted
}

// TestRoomHandler_JoinWithAdminSession verifies that a valid admin session
// cookie grants the admin role on join (dashboard "Join as host"), bypassing
// the waiting room and password, while an invalid cookie stays a viewer.
func TestRoomHandler_JoinWithAdminSession(t *testing.T) {
	db, cleanup := database.NewTestDB(t)
	defer cleanup()

	handler := newTestRoomHandler(db)
	handler.SetSessionValidator(func(sessionID string) bool {
		return sessionID == "valid-admin-session"
	})

	createRoomForTest(t, handler, map[string]interface{}{
		"slug":               "host-join-room",
		"name":               "Host Join Room",
		"watermarkMode":      "none",
		"waitingRoomEnabled": true,
		"password":           "secret12",
	})

	t.Run("valid session cookie grants admin", func(t *testing.T) {
		rr := joinRoomForTest(t, handler, "host-join-room", map[string]interface{}{"name": "Host"},
			&http.Cookie{Name: SessionCookieName, Value: "valid-admin-session"})
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var resp map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &resp)
		if resp["role"] != "admin" {
			t.Errorf("expected role admin, got %v", resp["role"])
		}
		if resp["isAdmitted"] != true {
			t.Errorf("expected admin to be admitted (waiting room bypass)")
		}
		if resp["waitingRoom"] != false {
			t.Errorf("expected waitingRoom false for admin")
		}
	})

	t.Run("invalid session cookie stays viewer", func(t *testing.T) {
		rr := joinRoomForTest(t, handler, "host-join-room",
			map[string]interface{}{"name": "Guest", "password": "secret12"},
			&http.Cookie{Name: SessionCookieName, Value: "expired-session"})
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var resp map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &resp)
		if resp["role"] != "viewer" {
			t.Errorf("expected role viewer, got %v", resp["role"])
		}
		if resp["isAdmitted"] != false {
			t.Errorf("expected viewer to wait in the waiting room")
		}
	})

	t.Run("admin token in body still works", func(t *testing.T) {
		rr := joinRoomForTest(t, handler, "host-join-room",
			map[string]interface{}{"name": "Token Host", "adminToken": roomsTestAdminToken}, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var resp map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &resp)
		if resp["role"] != "admin" {
			t.Errorf("expected role admin via body token, got %v", resp["role"])
		}
	})
}

// TestRoomHandler_LobbyJoin covers the scheduled-room gating: 403 before the
// lobby window, countdown-lobby join (unadmitted + lobby payload) inside the
// window, admin bypass, and the per-room early_open_minutes width.
func TestRoomHandler_LobbyJoin(t *testing.T) {
	db, cleanup := database.NewTestDB(t)
	defer cleanup()

	handler := newTestRoomHandler(db)

	farFuture := time.Now().Add(2 * time.Hour).UTC()
	soon := time.Now().Add(5 * time.Minute).UTC()

	// Default 10-minute window, starts in 2h: joins are rejected.
	createRoomForTest(t, handler, map[string]interface{}{
		"slug":          "lobby-closed",
		"name":          "Closed Lobby",
		"watermarkMode": "none",
		"scheduledAt":   farFuture.Format(time.RFC3339),
	})
	// 30-minute window, starts in 5m: joins land in the countdown lobby.
	createRoomForTest(t, handler, map[string]interface{}{
		"slug":             "lobby-open",
		"name":             "Open Lobby",
		"watermarkMode":    "none",
		"scheduledAt":      soon.Format(time.RFC3339),
		"earlyOpenMinutes": 30,
	})
	// Waiting-room-enabled scheduled room: lobby payload says so.
	createRoomForTest(t, handler, map[string]interface{}{
		"slug":               "lobby-waiting",
		"name":               "Lobby With Waiting Room",
		"watermarkMode":      "none",
		"waitingRoomEnabled": true,
		"scheduledAt":        soon.Format(time.RFC3339),
		"earlyOpenMinutes":   30,
	})

	t.Run("before lobby window is rejected", func(t *testing.T) {
		rr := joinRoomForTest(t, handler, "lobby-closed", map[string]interface{}{"name": "Early Bird"}, nil)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("expected 403 before the lobby window, got %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("inside lobby window joins the countdown lobby", func(t *testing.T) {
		rr := joinRoomForTest(t, handler, "lobby-open", map[string]interface{}{"name": "On Time"}, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var resp struct {
			ParticipantID string `json:"participantId"`
			IsAdmitted    bool   `json:"isAdmitted"`
			WaitingRoom   bool   `json:"waitingRoom"`
			ServerTime    string `json:"serverTime"`
			Lobby         *struct {
				ScheduledAt        time.Time `json:"scheduledAt"`
				OpensAt            time.Time `json:"opensAt"`
				WaitingRoomEnabled bool      `json:"waitingRoomEnabled"`
			} `json:"lobby"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to parse join response: %v", err)
		}
		if resp.IsAdmitted {
			t.Errorf("lobby joiner must not be admitted before the room opens")
		}
		if !resp.WaitingRoom {
			t.Errorf("lobby joiner should be routed to the waiting page")
		}
		if resp.Lobby == nil {
			t.Fatalf("expected lobby payload, got none: %s", rr.Body.String())
		}
		if resp.Lobby.WaitingRoomEnabled {
			t.Errorf("expected waitingRoomEnabled false in lobby payload")
		}
		if !resp.Lobby.ScheduledAt.Equal(soon.Truncate(time.Second)) && resp.Lobby.ScheduledAt.Sub(soon).Abs() > time.Second {
			t.Errorf("lobby scheduledAt mismatch: got %v want %v", resp.Lobby.ScheduledAt, soon)
		}
		wantOpens := soon.Add(-30 * time.Minute)
		if resp.Lobby.OpensAt.Sub(wantOpens).Abs() > time.Second {
			t.Errorf("lobby opensAt mismatch: got %v want %v", resp.Lobby.OpensAt, wantOpens)
		}
		if resp.ServerTime == "" {
			t.Errorf("expected serverTime in join response")
		}
		if participantAdmitted(t, db, resp.ParticipantID) {
			t.Errorf("expected is_admitted=false in DB for lobby joiner")
		}
	})

	t.Run("waiting-room lobby payload flags approval flow", func(t *testing.T) {
		rr := joinRoomForTest(t, handler, "lobby-waiting", map[string]interface{}{"name": "Reviewer"}, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var resp map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &resp)
		lobby, ok := resp["lobby"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected lobby payload: %s", rr.Body.String())
		}
		if lobby["waitingRoomEnabled"] != true {
			t.Errorf("expected waitingRoomEnabled true in lobby payload")
		}
	})

	t.Run("admins bypass scheduling entirely", func(t *testing.T) {
		rr := joinRoomForTest(t, handler, "lobby-closed",
			map[string]interface{}{"name": "Host", "adminToken": roomsTestAdminToken}, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 for admin, got %d: %s", rr.Code, rr.Body.String())
		}
		var resp map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &resp)
		if resp["isAdmitted"] != true {
			t.Errorf("expected admin admitted before the lobby window")
		}
		if resp["lobby"] != nil {
			t.Errorf("admins must not receive a lobby payload")
		}
	})

	t.Run("opened room admits normally before scheduled_at", func(t *testing.T) {
		if _, err := db.Exec(`UPDATE rooms SET opened_at = ? WHERE slug = 'lobby-open'`, time.Now()); err != nil {
			t.Fatalf("failed to mark room opened: %v", err)
		}
		rr := joinRoomForTest(t, handler, "lobby-open", map[string]interface{}{"name": "After Open"}, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var resp map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &resp)
		if resp["isAdmitted"] != true {
			t.Errorf("expected admitted join once the room is opened early")
		}
		if resp["lobby"] != nil {
			t.Errorf("no lobby payload expected once the room is open")
		}
	})
}

// TestRoomHandler_AutoAdmitOnOpen verifies the open endpoint auto-admits the
// countdown lobby of a waiting-room-disabled room (DB update + the same SSE
// "admitted" notification a manual admit sends) and that waiting-room-enabled
// rooms switch to the approval flow ("open" event, still unadmitted).
func TestRoomHandler_AutoAdmitOnOpen(t *testing.T) {
	db, cleanup := database.NewTestDB(t)
	defer cleanup()

	handler := newTestRoomHandler(db)
	scheduled := time.Now().Add(30 * time.Minute).UTC()

	createRoomForTest(t, handler, map[string]interface{}{
		"slug":             "open-auto",
		"name":             "Auto Admit Room",
		"watermarkMode":    "none",
		"scheduledAt":      scheduled.Format(time.RFC3339),
		"earlyOpenMinutes": 60,
	})
	createRoomForTest(t, handler, map[string]interface{}{
		"slug":               "open-approval",
		"name":               "Approval Room",
		"watermarkMode":      "none",
		"waitingRoomEnabled": true,
		"scheduledAt":        scheduled.Format(time.RFC3339),
		"earlyOpenMinutes":   60,
	})

	joinLobby := func(slug string) string {
		rr := joinRoomForTest(t, handler, slug, map[string]interface{}{"name": "Lobby Guest"}, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("lobby join failed: %d %s", rr.Code, rr.Body.String())
		}
		var resp map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &resp)
		return resp["participantId"].(string)
	}

	autoID := joinLobby("open-auto")
	approvalID := joinLobby("open-approval")

	// Subscribe both lobby guests to SSE notifications before opening.
	autoCh, ok := handler.waitingManager.Subscribe(autoID)
	if !ok {
		t.Fatalf("failed to subscribe auto participant")
	}
	approvalCh, ok := handler.waitingManager.Subscribe(approvalID)
	if !ok {
		t.Fatalf("failed to subscribe approval participant")
	}

	openRoom := func(slug string) {
		req := httptest.NewRequest("POST", "/api/rooms/"+slug+"/open", nil)
		req.SetPathValue("slug", slug)
		rr := httptest.NewRecorder()
		handler.OpenRoom(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("open endpoint failed for %s: %d %s", slug, rr.Code, rr.Body.String())
		}
		var resp map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &resp)
		if resp["openedAt"] == nil {
			t.Errorf("expected openedAt in open response")
		}
	}

	openRoom("open-auto")
	openRoom("open-approval")

	// Auto-admit: DB flipped + "admitted" SSE event.
	if !participantAdmitted(t, db, autoID) {
		t.Errorf("expected lobby guest auto-admitted when waiting room is disabled")
	}
	select {
	case event := <-autoCh:
		if event != "admitted" {
			t.Errorf("expected 'admitted' event, got %q", event)
		}
	case <-time.After(time.Second):
		t.Errorf("timed out waiting for the admitted SSE event")
	}

	// Approval flow: still unadmitted, "open" event delivered.
	if participantAdmitted(t, db, approvalID) {
		t.Errorf("waiting-room participant must not be auto-admitted on open")
	}
	select {
	case event := <-approvalCh:
		if event != "open" {
			t.Errorf("expected 'open' event, got %q", event)
		}
	case <-time.After(time.Second):
		t.Errorf("timed out waiting for the open SSE event")
	}

	// opened_at persisted
	var openedAt *time.Time
	if err := db.QueryRow(`SELECT opened_at FROM rooms WHERE slug = 'open-auto'`).Scan(&openedAt); err != nil || openedAt == nil {
		t.Errorf("expected opened_at to be set (err=%v)", err)
	}
}

// TestRoomHandler_OpenTimerAutoAdmit verifies the per-room timer path: a
// lobby participant is auto-admitted when scheduled_at arrives, without any
// admin action.
func TestRoomHandler_OpenTimerAutoAdmit(t *testing.T) {
	db, cleanup := database.NewTestDB(t)
	defer cleanup()

	handler := newTestRoomHandler(db)
	// Enough lead time that the join definitely lands before scheduled_at,
	// even under -race; RFC3339Nano keeps sub-second precision so the timer
	// still fires quickly.
	scheduled := time.Now().Add(1500 * time.Millisecond).UTC()

	createRoomForTest(t, handler, map[string]interface{}{
		"slug":             "timer-room",
		"name":             "Timer Room",
		"watermarkMode":    "none",
		"scheduledAt":      scheduled.Format(time.RFC3339Nano),
		"earlyOpenMinutes": 60,
	})

	rr := joinRoomForTest(t, handler, "timer-room", map[string]interface{}{"name": "Patient Guest"}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("lobby join failed: %d %s", rr.Code, rr.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(rr.Body.Bytes(), &resp)
	participantID := resp["participantId"].(string)

	if participantAdmitted(t, db, participantID) {
		t.Fatalf("participant must start unadmitted in the lobby")
	}

	// Wait for scheduled_at to pass and the timer to fire.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if participantAdmitted(t, db, participantID) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("lobby participant was not auto-admitted after scheduled_at")
}

// TestRoomHandler_OpenEndpointAuth verifies POST /api/rooms/{slug}/open sits
// behind admin auth (same RequireAuth middleware the router applies).
func TestRoomHandler_OpenEndpointAuth(t *testing.T) {
	db, cleanup := database.NewTestDB(t)
	defer cleanup()

	handler := newTestRoomHandler(db)
	createRoomForTest(t, handler, map[string]interface{}{
		"slug":          "auth-open-room",
		"name":          "Auth Open Room",
		"watermarkMode": "none",
		"scheduledAt":   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/rooms/{slug}/open", handler.OpenRoom)
	protected := middleware.RequireAuth(middleware.AuthConfig{
		AdminToken:    roomsTestAdminToken,
		SessionCookie: SessionCookieName,
		ValidateSession: func(sessionID string) bool {
			return sessionID == "valid-admin-session"
		},
	})(mux)

	t.Run("unauthenticated request is rejected", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/rooms/auth-open-room/open", nil)
		rr := httptest.NewRecorder()
		protected.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 without credentials, got %d", rr.Code)
		}
		var openedAt *time.Time
		db.QueryRow(`SELECT opened_at FROM rooms WHERE slug = 'auth-open-room'`).Scan(&openedAt)
		if openedAt != nil {
			t.Errorf("room must not open without auth")
		}
	})

	t.Run("bearer token opens the room", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/rooms/auth-open-room/open", nil)
		req.Header.Set("Authorization", "Bearer "+roomsTestAdminToken)
		rr := httptest.NewRecorder()
		protected.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("expected 200 with bearer token, got %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("session cookie opens the room", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/rooms/auth-open-room/open", nil)
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "valid-admin-session"})
		rr := httptest.NewRecorder()
		protected.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("expected 200 with session cookie, got %d: %s", rr.Code, rr.Body.String())
		}
	})
}

// TestRoomHandler_DenyWaitingParticipant verifies the shared deny logic:
// participant removed, "denied" SSE event delivered.
func TestRoomHandler_DenyWaitingParticipant(t *testing.T) {
	db, cleanup := database.NewTestDB(t)
	defer cleanup()

	handler := newTestRoomHandler(db)
	createRoomForTest(t, handler, map[string]interface{}{
		"slug":               "deny-room",
		"name":               "Deny Room",
		"watermarkMode":      "none",
		"waitingRoomEnabled": true,
	})

	rr := joinRoomForTest(t, handler, "deny-room", map[string]interface{}{"name": "Denied Guest"}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("join failed: %d %s", rr.Code, rr.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(rr.Body.Bytes(), &resp)
	participantID := resp["participantId"].(string)

	ch, ok := handler.waitingManager.Subscribe(participantID)
	if !ok {
		t.Fatalf("failed to subscribe participant")
	}

	found, err := handler.DenyWaitingParticipant("deny-room", participantID)
	if err != nil {
		t.Fatalf("deny failed: %v", err)
	}
	if !found {
		t.Fatalf("expected deny to match the waiting participant")
	}

	select {
	case event := <-ch:
		if event != "denied" {
			t.Errorf("expected 'denied' event, got %q", event)
		}
	case <-time.After(time.Second):
		t.Errorf("timed out waiting for the denied SSE event")
	}

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM participants WHERE id = ?`, participantID).Scan(&count)
	if count != 0 {
		t.Errorf("denied participant should be removed, found %d rows", count)
	}

	// Denying again reports not-found (idempotent resolution).
	found, err = handler.DenyWaitingParticipant("deny-room", participantID)
	if err != nil || found {
		t.Errorf("expected second deny to be a no-op (found=%v err=%v)", found, err)
	}
}

// TestRoomHandler_OpenOnStreamStart verifies the stream-start hook: when the
// publisher goes live on a scheduled, unopened room, opened_at is set and the
// lobby auto-admits.
func TestRoomHandler_OpenOnStreamStart(t *testing.T) {
	db, cleanup := database.NewTestDB(t)
	defer cleanup()

	handler := newTestRoomHandler(db)
	scheduled := time.Now().Add(30 * time.Minute).UTC()

	if _, err := db.Exec(`INSERT INTO stream_keys (id, name, key_token) VALUES ('sk1', 'Suite', 'stream-token-1')`); err != nil {
		t.Fatalf("failed to insert stream key: %v", err)
	}

	createRoomForTest(t, handler, map[string]interface{}{
		"slug":             "stream-open-room",
		"name":             "Stream Open Room",
		"watermarkMode":    "none",
		"scheduledAt":      scheduled.Format(time.RFC3339),
		"earlyOpenMinutes": 60,
		"streamKeyId":      "sk1",
	})

	rr := joinRoomForTest(t, handler, "stream-open-room", map[string]interface{}{"name": "Lobby Guest"}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("lobby join failed: %d %s", rr.Code, rr.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(rr.Body.Bytes(), &resp)
	participantID := resp["participantId"].(string)

	if err := handler.OnStreamStart("stream-token-1"); err != nil {
		t.Fatalf("OnStreamStart failed: %v", err)
	}

	var openedAt *time.Time
	if err := db.QueryRow(`SELECT opened_at FROM rooms WHERE slug = 'stream-open-room'`).Scan(&openedAt); err != nil || openedAt == nil {
		t.Errorf("expected opened_at set by stream start (err=%v)", err)
	}
	if !participantAdmitted(t, db, participantID) {
		t.Errorf("expected lobby guest admitted when the stream went live")
	}
}
