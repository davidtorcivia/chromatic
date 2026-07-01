-- Persistent audit trail for admin and authentication-sensitive actions.
-- The application records method/path/status/auth mode and request metadata
-- without storing bearer tokens, cookies, query strings, or request bodies.
CREATE TABLE admin_audit_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    actor TEXT NOT NULL DEFAULT 'admin',
    auth_mode TEXT NOT NULL DEFAULT 'unknown',
    method TEXT NOT NULL,
    path TEXT NOT NULL,
    status_code INTEGER NOT NULL,
    client_ip TEXT,
    user_agent TEXT
);

CREATE INDEX idx_admin_audit_logs_created_at ON admin_audit_logs(created_at);
CREATE INDEX idx_admin_audit_logs_path ON admin_audit_logs(path);
