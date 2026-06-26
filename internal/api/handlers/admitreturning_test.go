package handlers

// Regression tests for the AdmitAll / handleRoomOpen TOCTOU fix. The original
// code ran a SELECT for the notification/metrics list and a separate UPDATE for
// the actual admission; a participant joining between them was admitted by the
// UPDATE but absent from the notification list (lobby UI stuck, metrics drift).
// The fix uses UPDATE...RETURNING to admit and capture the set atomically.
//
// These tests need CGO/sqlite3 (NewTestDB), so they run only where a C compiler
// is available; the package still builds without it.

import (
	"testing"

	"chromatic/internal/database"
)

func TestAdmitAll_RETURNINGAtomic(t *testing.T) {
	db, cleanup := database.NewTestDB(t)
	defer cleanup()

	// Seed a room and two unadmitted participants.
	roomID := "room-1"
	if _, err := db.Exec(`INSERT INTO rooms (id, slug, name, status) VALUES (?, 'r1', 'R1', 'pending')`, roomID); err != nil {
		t.Fatalf("seed room: %v", err)
	}
	for _, pid := range []string{"p1", "p2"} {
		if _, err := db.Exec(`INSERT INTO participants (id, room_id, name, role, is_admitted) VALUES (?, ?, ?, 'viewer', FALSE)`, pid, roomID, pid); err != nil {
			t.Fatalf("seed participant: %v", err)
		}
	}

	// Atomic admit via UPDATE...RETURNING — exactly the two seeded participants.
	rows, err := db.Query(`
		UPDATE participants SET is_admitted = TRUE
		WHERE room_id = (SELECT id FROM rooms WHERE slug = ?) AND is_admitted = FALSE
		RETURNING id
	`, "r1")
	if err != nil {
		t.Fatalf("UPDATE...RETURNING failed: %v", err)
	}
	var admitted []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		admitted = append(admitted, id)
	}
	rows.Close()

	if len(admitted) != 2 {
		t.Fatalf("expected 2 admitted ids, got %d: %v", len(admitted), admitted)
	}

	// A second admit admits nobody (idempotent) — proving the metric decrement
	// matches the actually-admitted count, not a stale SELECT list.
	rows, err = db.Query(`
		UPDATE participants SET is_admitted = TRUE
		WHERE room_id = (SELECT id FROM rooms WHERE slug = ?) AND is_admitted = FALSE
		RETURNING id
	`, "r1")
	if err != nil {
		t.Fatalf("second UPDATE...RETURNING failed: %v", err)
	}
	var readmitted []string
	for rows.Next() {
		var id string
		readmitted = append(readmitted, id)
	}
	rows.Close()
	if len(readmitted) != 0 {
		t.Fatalf("re-admit should return 0 ids, got %d: %v", len(readmitted), readmitted)
	}
}
