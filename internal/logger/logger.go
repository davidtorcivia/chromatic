// Package logger is the shared structured logger (slog) with level control.
package logger

import (
	"context"
	"log/slog"
	"os"
)

// Logger is the application-wide structured logger
var Logger *slog.Logger

// Initialize sets up the structured logger
// In production, outputs JSON; in development, outputs text
func Initialize(production bool) {
	var handler slog.Handler

	opts := &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}

	if production {
		// JSON output for production (easier to parse in log aggregators)
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		// Text output for development (human readable)
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	Logger = slog.New(handler)
	slog.SetDefault(Logger)
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
