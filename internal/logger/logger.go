// Package logger is the shared structured logger (slog) with level control.
package logger

import (
	"context"
	"io"
	"log/slog"
	"os"
)

// Logger is the application-wide structured logger
var Logger *slog.Logger

// Log file rotation. 50MB x 5 keeps roughly a week of busy sessions in ~250MB,
// which is the right order for post-mortems: the 2026-08-02 incident was 1.6MB
// for a 100-minute session.
const (
	logFileMaxBytes = 50 * 1024 * 1024
	logFileMaxFiles = 5
)

// Initialize sets up the structured logger
// In production, outputs JSON; in development, outputs text
func Initialize(production bool) {
	InitializeWithFile(production, "")
}

// InitializeWithFile is Initialize plus an optional on-disk copy of every line.
//
// Container stdout is not durable: `docker compose up -d` recreates the
// container and takes the whole json-file log with it. That is not theoretical
// — the 2026-08-02 session, which every fix from that day was diagnosed from,
// was destroyed by the redeploy that shipped those fixes, and survived only
// because a copy had been taken by hand. Pointing logPath at a persistent
// volume gives incident history a life beyond the container.
//
// stdout is always kept as well, so `docker logs` still works and a
// non-writable log path costs visibility rather than causing an outage.
func InitializeWithFile(production bool, logPath string) {
	opts := &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}

	var out io.Writer = os.Stdout
	var fileErr error
	if logPath != "" {
		if rf, err := newRotatingFile(logPath, logFileMaxBytes, logFileMaxFiles); err != nil {
			fileErr = err
		} else {
			out = io.MultiWriter(os.Stdout, rf)
		}
	}

	var handler slog.Handler
	if production {
		// JSON output for production (easier to parse in log aggregators)
		handler = slog.NewJSONHandler(out, opts)
	} else {
		// Text output for development (human readable)
		handler = slog.NewTextHandler(out, opts)
	}

	Logger = slog.New(handler)
	// Also routes the stdlib log package (used throughout internal/webrtc)
	// through this handler, so file logging covers those lines too.
	slog.SetDefault(Logger)

	// Report the failure only once the logger exists, so it lands in the same
	// stream as everything else rather than vanishing.
	if fileErr != nil {
		Logger.Warn("File logging disabled; logs will not survive container recreation",
			"path", logPath, "error", fileErr)
	} else if logPath != "" {
		Logger.Info("File logging enabled", "path", logPath,
			"max_bytes", logFileMaxBytes, "max_files", logFileMaxFiles)
	}
}

// WithRequestID returns a logger with request ID context
func WithRequestID(ctx context.Context, requestID string) *slog.Logger {
	return Logger.With(slog.String("request_id", requestID))
}

// WithRoom returns a logger with room context
func WithRoom(roomSlug string) *slog.Logger {
	return Logger.With(slog.String("room", roomSlug))
}

// WithParticipant returns a logger with participant context
func WithParticipant(participantID string) *slog.Logger {
	return Logger.With(slog.String("participant_id", participantID))
}

// Info logs an info message
func Info(msg string, args ...any) {
	Logger.Info(msg, args...)
}

// Warn logs a warning message
func Warn(msg string, args ...any) {
	Logger.Warn(msg, args...)
}

// Error logs an error message
func Error(msg string, args ...any) {
	Logger.Error(msg, args...)
}

// Debug logs a debug message
func Debug(msg string, args ...any) {
	Logger.Debug(msg, args...)
}
