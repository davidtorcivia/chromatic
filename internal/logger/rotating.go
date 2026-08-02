package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// rotatingFile is a size-bounded io.Writer with numbered backups:
// chromatic.log, chromatic.log.1, ... chromatic.log.N.
//
// Deliberately hand-rolled rather than pulling in a rotation library — this
// module has five direct dependencies and none of them are conveniences.
//
// CRITICAL: nothing in here may call log/slog. This writer sits *behind* the
// slog handler, and Initialize routes the stdlib log package through that same
// handler, so a log call on an error path would re-enter Write while its mutex
// is held. Every failure therefore reports to os.Stderr directly and Write
// always reports success to slog: a logging backend that cannot write to disk
// must degrade to stdout, never wedge or crash the server.
type rotatingFile struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	maxFiles int

	file *os.File
	size int64
}

func newRotatingFile(path string, maxBytes int64, maxFiles int) (*rotatingFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	r := &rotatingFile{path: path, maxBytes: maxBytes, maxFiles: maxFiles}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

// open attaches to the current log file, continuing it if one already exists so
// a restart appends rather than truncating (the whole point is surviving
// redeploys). Caller holds mu, or is the constructor.
func (r *rotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("stat log file: %w", err)
	}
	r.file = f
	r.size = info.Size()
	return nil
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.file == nil {
		// A previous rotation failed to reopen. Keep trying, but never block
		// the caller on it.
		if err := r.open(); err != nil {
			fmt.Fprintf(os.Stderr, "logger: cannot reopen %s: %v\n", r.path, err)
			return len(p), nil
		}
	}

	if r.size+int64(len(p)) > r.maxBytes {
		r.rotate()
	}

	n, err := r.file.Write(p)
	r.size += int64(n)
	if err != nil {
		fmt.Fprintf(os.Stderr, "logger: write to %s failed: %v\n", r.path, err)
	}
	// Always report success: slog must not see a failing writer, and stdout
	// already carries this line.
	return len(p), nil
}

// rotate shifts chromatic.log.N-1 -> .N, the live file -> .1, and opens a fresh
// one. Caller holds mu. Best-effort throughout: if any step fails we keep
// writing to whatever handle we can get rather than losing logging entirely.
func (r *rotatingFile) rotate() {
	if r.file != nil {
		if err := r.file.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "logger: closing %s during rotate: %v\n", r.path, err)
		}
		r.file = nil
	}

	// Drop the oldest, then shift the rest up one. Descending order matters:
	// ascending would overwrite each backup with its predecessor.
	oldest := fmt.Sprintf("%s.%d", r.path, r.maxFiles)
	if err := os.Remove(oldest); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "logger: removing %s: %v\n", oldest, err)
	}
	for i := r.maxFiles - 1; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d", r.path, i)
		to := fmt.Sprintf("%s.%d", r.path, i+1)
		if err := os.Rename(from, to); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "logger: rotating %s -> %s: %v\n", from, to, err)
		}
	}
	if err := os.Rename(r.path, r.path+".1"); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "logger: rotating %s: %v\n", r.path, err)
	}

	if err := r.open(); err != nil {
		fmt.Fprintf(os.Stderr, "logger: reopening %s after rotate: %v\n", r.path, err)
	}
}

// Close flushes and releases the file. Used by tests; the server logs until exit.
func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}
