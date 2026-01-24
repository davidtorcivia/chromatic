package logger

import (
	"context"
	"testing"
)

func TestInitialize_Development(t *testing.T) {
	Initialize(false)

	if Logger == nil {
		t.Error("Logger should not be nil after initialization")
	}
}

func TestInitialize_Production(t *testing.T) {
	Initialize(true)

	if Logger == nil {
		t.Error("Logger should not be nil after initialization")
	}

	// Reset to dev mode for other tests
	Initialize(false)
}

func TestWithRequestID(t *testing.T) {
	Initialize(false)

	logger := WithRequestID(context.Background(), "test-request-123")
	if logger == nil {
		t.Error("WithRequestID should return a logger")
	}
}

func TestWithRoom(t *testing.T) {
	Initialize(false)

	logger := WithRoom("test-room")
	if logger == nil {
		t.Error("WithRoom should return a logger")
	}
}

func TestWithParticipant(t *testing.T) {
	Initialize(false)

	logger := WithParticipant("participant-123")
	if logger == nil {
		t.Error("WithParticipant should return a logger")
	}
}

func TestInfo(t *testing.T) {
	Initialize(false)

	// Should not panic
	Info("test info message", "key", "value")
}

func TestWarn(t *testing.T) {
	Initialize(false)

	// Should not panic
	Warn("test warning message", "key", "value")
}

func TestError(t *testing.T) {
	Initialize(false)

	// Should not panic
	Error("test error message", "key", "value")
}

func TestDebug(t *testing.T) {
	Initialize(false)

	// Should not panic
	Debug("test debug message", "key", "value")
}
