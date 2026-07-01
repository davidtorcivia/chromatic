package middleware

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type auditCall struct {
	query string
	args  []interface{}
}

type fakeAuditDB struct {
	calls []auditCall
	err   error
}

func (f *fakeAuditDB) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	copied := append([]interface{}(nil), args...)
	f.calls = append(f.calls, auditCall{query: query, args: copied})
	if f.err != nil {
		return nil, f.err
	}
	return auditResult(1), nil
}

type auditResult int64

func (r auditResult) LastInsertId() (int64, error) { return int64(r), nil }
func (r auditResult) RowsAffected() (int64, error) { return 1, nil }

func TestAuditLoggerRecordsMutatingRequestsWithoutSecrets(t *testing.T) {
	db := &fakeAuditDB{}
	handler := AuditLogger(AuditLoggerConfig{
		DB:             db,
		Actor:          "admin",
		SessionCookie:  "chromatic_session",
		TrustedProxies: []string{"127.0.0.1"},
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/rooms/demo/end?token=secret", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	req.Header.Set("X-Forwarded-For", "203.0.113.8")
	req.Header.Set("User-Agent", "Audit Test Agent")
	req.AddCookie(&http.Cookie{Name: "chromatic_session", Value: "session-id"})
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusCreated)
	}
	if len(db.calls) != 1 {
		t.Fatalf("audit calls = %d, want 1", len(db.calls))
	}
	args := db.calls[0].args
	if got := args[0]; got != "admin" {
		t.Fatalf("actor = %v, want admin", got)
	}
	if got := args[1]; got != "cookie" {
		t.Fatalf("auth_mode = %v, want cookie", got)
	}
	if got := args[2]; got != http.MethodPost {
		t.Fatalf("method = %v, want POST", got)
	}
	if got := args[3]; got != "/api/rooms/demo/end" {
		t.Fatalf("path = %v, want path without query string", got)
	}
	if got := args[4]; got != http.StatusCreated {
		t.Fatalf("status = %v, want %d", got, http.StatusCreated)
	}
	if got := args[5]; got != "203.0.113.8" {
		t.Fatalf("client_ip = %v, want trusted forwarded client", got)
	}
	if got := args[6]; got != "Audit Test Agent" {
		t.Fatalf("user_agent = %v, want request user agent", got)
	}
}

func TestAuditLoggerSkipsSafeMethodsByDefault(t *testing.T) {
	db := &fakeAuditDB{}
	handler := AuditLogger(AuditLoggerConfig{DB: db})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/rooms", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if len(db.calls) != 0 {
		t.Fatalf("safe method produced %d audit calls, want 0", len(db.calls))
	}
}

func TestAuditLoggerCanIncludeSafeMethods(t *testing.T) {
	db := &fakeAuditDB{}
	handler := AuditLogger(AuditLoggerConfig{DB: db, IncludeSafe: true})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/rooms", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if len(db.calls) != 1 {
		t.Fatalf("audit calls = %d, want 1", len(db.calls))
	}
}

func TestAuditLoggerDoesNotFailRequestWhenInsertFails(t *testing.T) {
	db := &fakeAuditDB{err: errors.New("database unavailable")}
	handler := AuditLogger(AuditLoggerConfig{DB: db})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))

	req := httptest.NewRequest(http.MethodDelete, "/api/files/file-id", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusAccepted)
	}
	if len(db.calls) != 1 {
		t.Fatalf("audit calls = %d, want 1", len(db.calls))
	}
}
