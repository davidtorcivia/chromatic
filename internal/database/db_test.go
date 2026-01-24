package database

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNew_ValidPath(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "chromatic-db-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "test.db")
	db, err := New(dbPath)
	if err != nil {
		t.Fatalf("failed to create database: %v", err)
	}
	defer db.Close()

	if db.DB == nil {
		t.Error("database connection should not be nil")
	}
}

func TestNew_InvalidPath(t *testing.T) {
	// Try to create database in a path that doesn't exist and can't be created
	_, err := New("/nonexistent/path/that/doesnt/exist/test.db")
	if err == nil {
		t.Error("expected error for invalid path")
	}
}

func TestMigrate(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "chromatic-migrate-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "test.db")
	db, err := New(dbPath)
	if err != nil {
		t.Fatalf("failed to create database: %v", err)
	}
	defer db.Close()

	// Run migrations
	if err := db.Migrate(); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	// Verify migrations table exists
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM _migrations").Scan(&count)
	if err != nil {
		t.Fatalf("failed to query migrations: %v", err)
	}

	if count == 0 {
		t.Error("expected at least one migration to be recorded")
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "chromatic-migrate-idem-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "test.db")
	db, err := New(dbPath)
	if err != nil {
		t.Fatalf("failed to create database: %v", err)
	}
	defer db.Close()

	// Run migrations twice - should be idempotent
	if err := db.Migrate(); err != nil {
		t.Fatalf("first migration failed: %v", err)
	}

	if err := db.Migrate(); err != nil {
		t.Fatalf("second migration failed: %v", err)
	}

	// Count should be the same after running twice
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM _migrations").Scan(&count)
	if err != nil {
		t.Fatalf("failed to query migrations: %v", err)
	}

	// Run again and verify count doesn't increase
	if err := db.Migrate(); err != nil {
		t.Fatalf("third migration failed: %v", err)
	}

	var newCount int
	err = db.QueryRow("SELECT COUNT(*) FROM _migrations").Scan(&newCount)
	if err != nil {
		t.Fatalf("failed to query migrations again: %v", err)
	}

	if newCount != count {
		t.Errorf("migration count changed from %d to %d after re-running", count, newCount)
	}
}

func TestMigrate_Tables(t *testing.T) {
	db, cleanup := NewTestDB(t)
	defer cleanup()

	// Verify core tables exist
	tables := []string{"rooms", "participants", "files", "sessions"}

	for _, table := range tables {
		var name string
		err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
		if err != nil {
			t.Errorf("table %s should exist: %v", table, err)
		}
	}
}

func TestIsMigrationApplied(t *testing.T) {
	db, cleanup := NewTestDB(t)
	defer cleanup()

	// 001_initial.sql should be applied
	applied, err := db.isMigrationApplied("001_initial.sql")
	if err != nil {
		t.Fatalf("failed to check migration: %v", err)
	}

	if !applied {
		t.Error("001_initial.sql should be marked as applied")
	}

	// Nonexistent migration should not be applied
	applied, err = db.isMigrationApplied("999_nonexistent.sql")
	if err != nil {
		t.Fatalf("failed to check nonexistent migration: %v", err)
	}

	if applied {
		t.Error("nonexistent migration should not be marked as applied")
	}
}

func TestNewTestDB(t *testing.T) {
	db, cleanup := NewTestDB(t)

	if db == nil {
		t.Fatal("NewTestDB returned nil database")
	}

	// Verify we can query
	var result int
	err := db.QueryRow("SELECT 1").Scan(&result)
	if err != nil {
		t.Fatalf("failed to query test database: %v", err)
	}

	if result != 1 {
		t.Errorf("expected 1, got %d", result)
	}

	cleanup()

	// After cleanup, queries should fail
	err = db.QueryRow("SELECT 1").Scan(&result)
	if err == nil {
		t.Error("expected error after cleanup")
	}
}

func TestDB_ForeignKeys(t *testing.T) {
	db, cleanup := NewTestDB(t)
	defer cleanup()

	// Verify foreign keys are enabled
	var fkEnabled int
	err := db.QueryRow("PRAGMA foreign_keys").Scan(&fkEnabled)
	if err != nil {
		t.Fatalf("failed to check foreign_keys pragma: %v", err)
	}

	if fkEnabled != 1 {
		t.Error("foreign keys should be enabled")
	}
}

func TestDB_WALMode(t *testing.T) {
	db, cleanup := NewTestDB(t)
	defer cleanup()

	// Verify WAL mode
	var journalMode string
	err := db.QueryRow("PRAGMA journal_mode").Scan(&journalMode)
	if err != nil {
		t.Fatalf("failed to check journal_mode pragma: %v", err)
	}

	if journalMode != "wal" {
		t.Errorf("expected WAL mode, got %s", journalMode)
	}
}
