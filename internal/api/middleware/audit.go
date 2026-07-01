package middleware

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"strings"
	"time"
)

// AuditDB is the minimal database surface used by AuditLogger.
type AuditDB interface {
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
}

// AuditLoggerConfig configures persistent admin/API audit logging.
type AuditLoggerConfig struct {
	DB             AuditDB
	Actor          string
	SessionCookie  string
	TrustedProxies []string
	IncludeSafe    bool
}

// AuditLogger records request metadata for sensitive admin/auth routes. It
// intentionally stores only the path, not RawQuery, so bearer-like secrets in
// URLs cannot leak into the audit table.
func AuditLogger(cfg AuditLoggerConfig) func(http.Handler) http.Handler {
	trustedProxySet := buildTrustedProxySet(cfg.TrustedProxies)
	actor := strings.TrimSpace(cfg.Actor)
	if actor == "" {
		actor = "admin"
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
			next.ServeHTTP(wrapped, r)

			if cfg.DB == nil || (!cfg.IncludeSafe && isSafeMethod(r.Method)) {
				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			_, err := cfg.DB.ExecContext(ctx, `
				INSERT INTO admin_audit_logs
					(actor, auth_mode, method, path, status_code, client_ip, user_agent)
				VALUES (?, ?, ?, ?, ?, ?, ?)
			`,
				actor,
				auditAuthMode(r, cfg.SessionCookie),
				r.Method,
				r.URL.EscapedPath(),
				wrapped.statusCode,
				getClientIP(r, trustedProxySet),
				truncateAuditField(r.UserAgent(), 512),
			)
			if err != nil {
				log.Printf("audit log insert failed: %v", err)
			}
		})
	}
}

func isSafeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

func auditAuthMode(r *http.Request, sessionCookie string) string {
	if sessionCookie != "" {
		if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
			return "cookie"
		}
	}
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return "bearer"
	}
	return "none"
}

func truncateAuditField(value string, maxLen int) string {
	if maxLen <= 0 || len(value) <= maxLen {
		return value
	}
	return value[:maxLen]
}
