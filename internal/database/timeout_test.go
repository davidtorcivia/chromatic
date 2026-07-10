package database

import (
	"context"
	"testing"
	"time"
)

// TestWithTimeout_AppliesDeadline is the core contract test for the DB timeout
// refactor: WithTimeout must return a context that will cancel after
// QueryTimeout when the parent has no deadline, so a slow query or disconnected
// client can't hold a pooled SQLite connection indefinitely and stall a latency-
// sensitive handler (OnStreamStart, sendChatHistory).
func TestWithTimeout_AppliesDeadline(t *testing.T) {
	ctx, cancel := WithTimeout(context.Background())
	defer cancel()

	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("WithTimeout(context.Background()) returned a context with no deadline")
	}
	// The deadline must be ~QueryTimeout from now.
	remaining := time.Until(dl)
	if remaining <= 0 || remaining > QueryTimeout {
		t.Errorf("deadline remaining = %v, want within (0, %v]", remaining, QueryTimeout)
	}

	// The context must actually cancel when the deadline elapses (simulated by
	// cancelling early via the returned cancel).
	cancel()
	if ctx.Err() != context.Canceled {
		t.Errorf("after cancel, ctx.Err() = %v, want context.Canceled", ctx.Err())
	}
}

// TestWithTimeout_RespectsParentDeadline verifies that an existing parent
// deadline is honoured as-is (handlers may already have a tighter deadline).
func TestWithTimeout_RespectsParentDeadline(t *testing.T) {
	parent, parentCancel := context.WithTimeout(context.Background(), 1*time.Hour)
	defer parentCancel()

	ctx, cancel := WithTimeout(parent)
	defer cancel()

	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected a deadline when the parent had one")
	}
	pdl, _ := parent.Deadline()
	if !dl.Equal(pdl) {
		t.Errorf("WithTimeout should honour the parent deadline %v, got %v", pdl, dl)
	}
}

// TestWithTimeout_NilParentIsSafe ensures a nil parent doesn't panic (some
// background call sites pass context.Background(); this guards accidental nil).
func TestWithTimeout_NilParentIsSafe(t *testing.T) {
	ctx, cancel := WithTimeout(nil) //nolint:staticcheck // deliberately nil: this test exists to prove nil is safe
	defer cancel()
	if ctx == nil {
		t.Fatal("WithTimeout(nil) returned nil context")
	}
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("WithTimeout(nil) should still apply a deadline")
	}
}

// TestExecContext_RespectsCancelledContext proves the DB-timeout refactor's
// guarantee at the query layer: ExecContext returns promptly with a context
// error when its context is cancelled, rather than holding a pooled connection
// until the query completes (or forever on a locked DB). Needs CGO/sqlite3.
func TestExecContext_RespectsCancelledContext(t *testing.T) {
	db, cleanup := NewTestDB(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the call

	start := time.Now()
	_, err := db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS _timeout_probe (id INTEGER)")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("ExecContext with a cancelled context should error")
	}
	// Must return near-instantly, not block on the DB.
	if elapsed > time.Second {
		t.Errorf("ExecContext took %v on a cancelled context; expected to return promptly", elapsed)
	}
}
