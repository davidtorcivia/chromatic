package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestRotatingFileRotatesAtMaxBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chromatic.log")

	r, err := newRotatingFile(path, 100, 3)
	if err != nil {
		t.Fatalf("newRotatingFile: %v", err)
	}
	defer r.Close()

	line := []byte(strings.Repeat("x", 60) + "\n")
	for i := 0; i < 4; i++ {
		if _, err := r.Write(line); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	// Each write is 61 bytes against a 100-byte cap, so every second write
	// rotates: expect the live file plus at least one backup.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("live log file missing: %v", err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("expected a rotated backup at %s.1: %v", path, err)
	}
}

func TestRotatingFileKeepsAtMostMaxFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chromatic.log")

	r, err := newRotatingFile(path, 50, 2)
	if err != nil {
		t.Fatalf("newRotatingFile: %v", err)
	}
	defer r.Close()

	line := []byte(strings.Repeat("y", 40) + "\n")
	for i := 0; i < 10; i++ {
		if _, err := r.Write(line); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	// maxFiles=2 means .1 and .2 exist and .3 never does, however many
	// rotations happened.
	for _, suffix := range []string{"", ".1", ".2"} {
		if _, err := os.Stat(path + suffix); err != nil {
			t.Errorf("expected %s%s to exist: %v", path, suffix, err)
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Errorf("expected %s.3 to be pruned, stat err = %v", path, err)
	}
}

// A restart must continue the existing file rather than truncate it, or the
// redeploy that ships a fix still destroys the evidence for the bug.
func TestRotatingFileAppendsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chromatic.log")

	first, err := newRotatingFile(path, 1<<20, 3)
	if err != nil {
		t.Fatalf("newRotatingFile: %v", err)
	}
	if _, err := first.Write([]byte("before restart\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := newRotatingFile(path, 1<<20, 3)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()
	if _, err := second.Write([]byte("after restart\n")); err != nil {
		t.Fatalf("write after reopen: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, want := range []string{"before restart", "after restart"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("log file lost %q after reopen; contents: %q", want, data)
		}
	}
}

// The writer sits behind slog, so it must never hand slog an error — a broken
// disk should cost the file copy, not wedge logging or crash the server.
func TestRotatingFileNeverReturnsErrorToSlog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chromatic.log")

	r, err := newRotatingFile(path, 1<<20, 2)
	if err != nil {
		t.Fatalf("newRotatingFile: %v", err)
	}
	defer r.Close()

	// Close the underlying handle behind its back and null it out, simulating
	// a file that has gone away mid-run.
	r.mu.Lock()
	r.file.Close()
	r.mu.Unlock()

	payload := []byte("still needs to be accepted\n")
	n, err := r.Write(payload)
	if err != nil {
		t.Errorf("Write returned an error to slog: %v", err)
	}
	if n != len(payload) {
		t.Errorf("Write reported n = %d, want %d", n, len(payload))
	}
}

func TestRotatingFileConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chromatic.log")

	// Small cap so rotation races with writers under -race.
	r, err := newRotatingFile(path, 256, 3)
	if err != nil {
		t.Fatalf("newRotatingFile: %v", err)
	}
	defer r.Close()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, err := fmt.Fprintf(r, "writer %d line %d\n", id, j); err != nil {
					t.Errorf("concurrent write: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

// A bad path must degrade to stdout-only, not take the server down.
func TestInitializeWithUnwritableFileStillLogs(t *testing.T) {
	dir := t.TempDir()
	// A path whose parent is a regular file: MkdirAll cannot succeed.
	blocker := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	InitializeWithFile(true, filepath.Join(blocker, "chromatic.log"))
	if Logger == nil {
		t.Fatal("logger must still be usable when the file sink cannot be opened")
	}
	Logger.Info("this must not panic")

	// Leave the package logger in a clean state for other tests.
	t.Cleanup(func() { Initialize(false) })
}
