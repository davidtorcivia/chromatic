// Package database wraps the SQLite connection (WAL mode, bounded pool,
// query deadlines) and runs the embedded migrations.
package database

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"sort"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// QueryTimeout is the deadline applied to handler DB access. SQLite (WAL) is
// fast for this app's small per-room workload, so a query that hasn't returned
// in a few seconds is stuck on the write lock (SQLITE_BUSY) or behind a
// disconnected client that never cancelled. Bounding it prevents one stalled
// query from holding one of the 4 pooled connections and blocking a latency-
// sensitive handler (e.g. OnStreamStart, sendChatHistory on join).
const QueryTimeout = 5 * time.Second

// WithTimeout returns parent (or parent with a QueryTimeout deadline if parent
// has none) so handlers can pass a single context to the *Context DB methods and
// get cancellation on client disconnect PLUS a hard query deadline.
func WithTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	if _, ok := parent.Deadline(); ok {
		// Caller already imposed a deadline; honour it as-is.
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, QueryTimeout)
}

// DB wraps a sql.DB connection with application-specific methods
type DB struct {
	*sql.DB
}

// New creates a new database connection with WAL mode enabled
func New(path string) (*DB, error) {
	// SQLite connection string with WAL mode and other optimizations
	dsn := fmt.Sprintf("%s?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on&_synchronous=NORMAL", path)

	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Verify connection
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	// Connection pool settings: WAL mode allows concurrent readers alongside
	// a single writer, so a small pool avoids serializing every query behind
	// one connection. _busy_timeout=5000 (set in the DSN above) makes
	// concurrent writers wait instead of erroring with SQLITE_BUSY.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)

	log.Printf("Database connected: %s (WAL mode)", path)

	return &DB{db}, nil
}

// Migrate runs all pending migrations
func (db *DB) Migrate() error {
	// Create migrations table if not exists
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS _migrations (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			applied_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		return fmt.Errorf("failed to create migrations table: %w", err)
	}

	// Get list of migration files
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("failed to read migrations directory: %w", err)
	}

	// Sort migrations by name
	var migrations []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			migrations = append(migrations, entry.Name())
		}
	}
	sort.Strings(migrations)

	// Apply each migration
	for _, name := range migrations {
		applied, err := db.isMigrationApplied(name)
		if err != nil {
			return fmt.Errorf("failed to check migration %s: %w", name, err)
		}
		if applied {
			continue
		}

		log.Printf("Applying migration: %s", name)

		content, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("failed to read migration %s: %w", name, err)
		}

		// Execute migration in a transaction
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("failed to begin transaction: %w", err)
		}

		if _, err := tx.Exec(string(content)); err != nil {
			tx.Rollback()
			return fmt.Errorf("failed to execute migration %s: %w", name, err)
		}

		if _, err := tx.Exec("INSERT INTO _migrations (name) VALUES (?)", name); err != nil {
			tx.Rollback()
			return fmt.Errorf("failed to record migration %s: %w", name, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit migration %s: %w", name, err)
		}

		log.Printf("Applied migration: %s", name)
	}

	return nil
}

func (db *DB) isMigrationApplied(name string) (bool, error) {
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM _migrations WHERE name = ?", name).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}
