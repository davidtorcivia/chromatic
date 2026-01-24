-- Admin sessions table for persistent authentication
CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    expires_at DATETIME NOT NULL
);

-- Index for cleanup queries
CREATE INDEX idx_sessions_expires ON sessions(expires_at);
