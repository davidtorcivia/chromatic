package database

import (
	"os"
	"path/filepath"
	"testing"
)

// NewTestDB creates a temporary test database with migrations applied.
// Returns the database and a cleanup function that should be deferred.
func NewTestDB(t *testing.T) (*DB, func()) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "chromatic-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	dbPath := filepath.Join(tmpDir, "test.db")
	db, err := New(dbPath)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		t.Fatalf("failed to create database: %v", err)
	}

	// Run migrations to create tables
	if err := db.Migrate(); err != nil {
		_ = db.Close()
		_ = os.RemoveAll(tmpDir)
		t.Fatalf("failed to run migrations: %v", err)
	}

	cleanup := func() {
		_ = db.Close()
		_ = os.RemoveAll(tmpDir)
	}

	return db, cleanup
}
