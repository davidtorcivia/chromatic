package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"chromatic/internal/config"
	"chromatic/internal/database"
)

// setupHandlerTest builds a SetupHandler against a migrated test DB and a
// configurable runtime config.
func setupHandlerTest(t *testing.T, cfg *config.Config) (*SetupHandler, func()) {
	t.Helper()
	db, dbCleanup := database.NewTestDB(t)

	if cfg == nil {
		cfg = &config.Config{
			PublicURL:  "http://localhost:3000",
			TurnMode:   config.TurnModeSelfHosted,
			TurnRealm:  "turn.local",
			TurnSecret: "secret",
		}
	}
	handler := NewSetupHandler(db, cfg)
	cleanup := func() { dbCleanup() }
	return handler, cleanup
}

func setupStatus(t *testing.T, handler *SetupHandler) SetupStatusResponse {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/setup/status", nil)
	rr := httptest.NewRecorder()
	handler.Status(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp SetupStatusResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode status: %v", err)
	}
	return resp
}

func checkByID(t *testing.T, status SetupStatusResponse, id string) SetupCheck {
	t.Helper()
	for _, c := range status.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("check %q not found in status", id)
	return SetupCheck{}
}

func TestSetupHandler_FreshLocalDevNeedsStreamKeyAndRoom(t *testing.T) {
	handler, cleanup := setupHandlerTest(t, nil)
	defer cleanup()

	status := setupStatus(t, handler)

	if !status.FirstRun {
		t.Errorf("expected firstRun true on fresh local-dev install, got false")
	}
	if status.ReadyToComplete {
		t.Errorf("expected readyToComplete false before keys/rooms exist")
	}

	if c := checkByID(t, status, "stream-key"); c.Status != SetupCheckNeedsAction {
		t.Errorf("expected stream-key needs-action, got %s", c.Status)
	}
	if c := checkByID(t, status, "room"); c.Status != SetupCheckNeedsAction {
		t.Errorf("expected room needs-action, got %s", c.Status)
	}
	if c := checkByID(t, status, "public-url"); c.Status != SetupCheckReady {
		t.Errorf("expected public-url ready for localhost, got %s", c.Status)
	}
	if c := checkByID(t, status, "security"); c.Status != SetupCheckReady {
		t.Errorf("expected security ready for local dev, got %s", c.Status)
	}
}

func TestSetupHandler_ReadyToCompleteWithKeysRoomAndMatchingTest(t *testing.T) {
	handler, cleanup := setupHandlerTest(t, nil)
	defer cleanup()

	if _, err := handler.db.Exec(
		`INSERT INTO stream_keys (id, name, key_token, created_at) VALUES ('k1','Main','tok1','2026-01-01')`); err != nil {
		t.Fatalf("seed stream key: %v", err)
	}
	if _, err := handler.db.Exec(
		`INSERT INTO rooms (id, slug, name, created_at) VALUES ('r1','studio','Studio','2026-01-02')`); err != nil {
		t.Fatalf("seed room: %v", err)
	}

	sig := currentTURNSignatureFor(handler.db, handler.cfg)
	if _, err := handler.db.Exec(`
		UPDATE config
		SET turn_last_tested_at = CURRENT_TIMESTAMP,
		    turn_last_test_success = 1,
		    turn_last_test_message = 'ok',
		    turn_last_test_signature = ?
		WHERE id = 1`, sig); err != nil {
		t.Fatalf("seed turn test: %v", err)
	}

	status := setupStatus(t, handler)

	if !status.ReadyToComplete {
		t.Fatalf("expected readyToComplete true; checks=%v", status.Checks)
	}
	if status.FirstRun {
		t.Errorf("expected firstRun false once a key and room exist")
	}
	if c := checkByID(t, status, "turn-connectivity"); c.Status != SetupCheckReady {
		t.Errorf("expected turn-connectivity ready for matching passing test, got %s (%s)", c.Status, c.Summary)
	}
	if status.Facts.FirstStreamKeyID == nil || *status.Facts.FirstStreamKeyID != "k1" {
		t.Errorf("expected firstStreamKeyId k1, got %v", status.Facts.FirstStreamKeyID)
	}
	if status.Facts.FirstRoomSlug == nil || *status.Facts.FirstRoomSlug != "studio" {
		t.Errorf("expected firstRoomSlug studio, got %v", status.Facts.FirstRoomSlug)
	}
}

func TestSetupHandler_StaleTestSignatureIsNotReady(t *testing.T) {
	handler, cleanup := setupHandlerTest(t, nil)
	defer cleanup()

	if _, err := handler.db.Exec(
		`INSERT INTO stream_keys (id, name, key_token, created_at) VALUES ('k1','Main','tok1','2026-01-01')`); err != nil {
		t.Fatalf("seed stream key: %v", err)
	}
	if _, err := handler.db.Exec(
		`INSERT INTO rooms (id, slug, name, created_at) VALUES ('r1','studio','Studio','2026-01-02')`); err != nil {
		t.Fatalf("seed room: %v", err)
	}

	if _, err := handler.db.Exec(`
		UPDATE config
		SET turn_last_tested_at = CURRENT_TIMESTAMP,
		    turn_last_test_success = 1,
		    turn_last_test_message = 'ok',
		    turn_last_test_signature = 'stale'
		WHERE id = 1`); err != nil {
		t.Fatalf("seed turn test: %v", err)
	}

	status := setupStatus(t, handler)
	if c := checkByID(t, status, "turn-connectivity"); c.Status != SetupCheckNeedsAction {
		t.Errorf("expected turn-connectivity needs-action for stale signature, got %s", c.Status)
	}
	if status.Facts.TurnLastTestValidForCurrentConfig {
		t.Errorf("expected turnLastTestValidForCurrentConfig false for stale signature")
	}
	if status.ReadyToComplete {
		t.Errorf("expected readyToComplete false when turn-connectivity is stale")
	}
}

func TestSetupHandler_CompleteReturns409WhenNotReady(t *testing.T) {
	handler, cleanup := setupHandlerTest(t, nil)
	defer cleanup()

	req := httptest.NewRequest("POST", "/api/setup/complete", nil)
	rr := httptest.NewRecorder()
	handler.Complete(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 when not ready, got %d", rr.Code)
	}

	var completed *string
	if err := handler.db.QueryRow(`SELECT setup_completed_at FROM config WHERE id = 1`).Scan(&completed); err != nil {
		t.Fatalf("read completed: %v", err)
	}
	if completed != nil {
		t.Errorf("expected setup_completed_at NULL on rejected complete, got %v", completed)
	}
}

func TestSetupHandler_CompleteStampsAndClearsDismissalWhenReady(t *testing.T) {
	handler, cleanup := setupHandlerTest(t, nil)
	defer cleanup()

	if _, err := handler.db.Exec(
		`INSERT INTO stream_keys (id, name, key_token, created_at) VALUES ('k1','Main','tok1','2026-01-01')`); err != nil {
		t.Fatalf("seed stream key: %v", err)
	}
	if _, err := handler.db.Exec(
		`INSERT INTO rooms (id, slug, name, created_at) VALUES ('r1','studio','Studio','2026-01-02')`); err != nil {
		t.Fatalf("seed room: %v", err)
	}

	sig := currentTURNSignatureFor(handler.db, handler.cfg)
	if _, err := handler.db.Exec(`UPDATE config SET turn_last_tested_at=CURRENT_TIMESTAMP, turn_last_test_success=1, turn_last_test_signature=?, setup_dismissed_at=CURRENT_TIMESTAMP WHERE id=1`, sig); err != nil {
		t.Fatalf("seed: %v", err)
	}

	req := httptest.NewRequest("POST", "/api/setup/complete", nil)
	rr := httptest.NewRecorder()
	handler.Complete(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 when ready, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp SetupStatusResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.CompletedAt == nil {
		t.Errorf("expected completedAt set after complete")
	}
	if resp.DismissedAt != nil {
		t.Errorf("expected dismissedAt cleared after complete, got %v", resp.DismissedAt)
	}
}

func TestSetupHandler_DismissStampsDismissedAt(t *testing.T) {
	handler, cleanup := setupHandlerTest(t, nil)
	defer cleanup()

	req := httptest.NewRequest("POST", "/api/setup/dismiss", nil)
	rr := httptest.NewRecorder()
	handler.Dismiss(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var resp SetupStatusResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.DismissedAt == nil {
		t.Errorf("expected dismissedAt set after dismiss")
	}
}

func TestSetupHandler_BrandingOptionalDoesNotBlockCompletion(t *testing.T) {
	handler, cleanup := setupHandlerTest(t, nil)
	defer cleanup()

	if _, err := handler.db.Exec(
		`INSERT INTO stream_keys (id, name, key_token, created_at) VALUES ('k1','Main','tok1','2026-01-01')`); err != nil {
		t.Fatalf("seed stream key: %v", err)
	}
	if _, err := handler.db.Exec(
		`INSERT INTO rooms (id, slug, name, created_at) VALUES ('r1','studio','Studio','2026-01-02')`); err != nil {
		t.Fatalf("seed room: %v", err)
	}
	sig := currentTURNSignatureFor(handler.db, handler.cfg)
	if _, err := handler.db.Exec(`UPDATE config SET turn_last_tested_at=CURRENT_TIMESTAMP, turn_last_test_success=1, turn_last_test_signature=? WHERE id=1`, sig); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Branding is not configured: the check is optional, not ready, but must
	// not block completion.
	status := setupStatus(t, handler)
	if c := checkByID(t, status, "branding"); c.Status != SetupCheckOptional {
		t.Errorf("expected branding optional when unset, got %s", c.Status)
	}
	if !status.ReadyToComplete {
		t.Errorf("expected readyToComplete true even with optional branding unset")
	}
}

func TestPublicURLCheck_SchemeValidation(t *testing.T) {
	cases := []struct {
		url   string
		want  SetupCheckStatus
		note  string
	}{
		{"http://localhost:3000", SetupCheckReady, "local http is ready"},
		{"http://127.0.0.1:3000", SetupCheckReady, "loopback http is ready"},
		{"https://stream.example.com", SetupCheckReady, "non-local https is ready"},
		{"ftp://localhost:3000", SetupCheckNeedsAction, "non-http(s) scheme must not be ready"},
		{"gopher://127.0.0.1", SetupCheckNeedsAction, "non-http(s) scheme must not be ready"},
		{"http://stream.example.com", SetupCheckNeedsAction, "non-local http is not ready"},
		{"", SetupCheckNeedsAction, "empty url is not ready"},
		{"stream.example.com", SetupCheckNeedsAction, "scheme-less url is not ready"},
	}
	for _, tc := range cases {
		t.Run(tc.note, func(t *testing.T) {
			got := publicURLCheck(tc.url).Status
			if got != tc.want {
				t.Errorf("publicURLCheck(%q) status = %s, want %s", tc.url, got, tc.want)
			}
		})
	}
}
