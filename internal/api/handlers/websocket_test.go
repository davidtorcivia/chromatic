package handlers

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"chromatic/internal/websocket"
)

// TestSanitizeText tests the XSS sanitization function
func TestSanitizeText(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "plain text",
			input:    "Hello World",
			expected: "Hello World",
		},
		{
			name:     "script tag",
			input:    "<script>alert('xss')</script>",
			expected: "alert('xss')",
		},
		{
			name:     "nested tags",
			input:    "<div><b>Bold</b></div>",
			expected: "Bold",
		},
		{
			name:     "html entities",
			input:    "Hello <World> & \"Friends\"",
			expected: "Hello  & \"Friends\"", // <World> is stripped as it looks like HTML tag
		},
		{
			name:     "img tag with onerror",
			input:    "<img src=x onerror=alert(1)>",
			expected: "",
		},
		{
			name:     "link tag",
			input:    "<a href=\"javascript:alert(1)\">Click me</a>",
			expected: "Click me",
		},
		{
			name:     "whitespace trimming",
			input:    "  Hello World  ",
			expected: "Hello World",
		},
		{
			name:     "mixed content",
			input:    "Normal <b>bold</b> text",
			expected: "Normal bold text",
		},
		{
			name:     "ampersand in text",
			input:    "Rock & Roll",
			expected: "Rock & Roll",
		},
		{
			name:     "quotes in text",
			input:    `He said "Hello"`,
			expected: `He said "Hello"`,
		},
		{
			name:     "less than in text",
			input:    "5 < 10",
			expected: "5 < 10",
		},
		{
			name:     "emoji preserved",
			input:    "Hello 👋 World",
			expected: "Hello 👋 World",
		},
		{
			name:     "unicode preserved",
			input:    "Héllo Wörld 日本語",
			expected: "Héllo Wörld 日本語",
		},
		{
			name:     "svg tag",
			input:    "<svg onload=alert(1)>",
			expected: "",
		},
		{
			name:     "style tag",
			input:    "<style>body{background:red}</style>",
			expected: "body{background:red}",
		},
		{
			name:     "event handler attributes",
			input:    "<div onclick=alert(1)>Click</div>",
			expected: "Click",
		},
		{
			name:     "control characters stripped",
			input:    "Hello\x00\x01World\x7f",
			expected: "HelloWorld",
		},
		{
			name:     "newline and tab preserved",
			input:    "Line1\nLine2\tEnd",
			expected: "Line1\nLine2\tEnd",
		},
		{
			name:     "bidi override characters stripped",
			input:    "Admin\u202E\u202DName\u2066\u2069",
			expected: "AdminName",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizeText(tt.input)
			if result != tt.expected {
				t.Errorf("sanitizeText(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

// TestRateLimiter tests the rate limiter functionality
func TestRateLimiter(t *testing.T) {
	t.Run("allows requests under limit", func(t *testing.T) {
		limiter := websocket.NewRateLimiter(5, time.Minute)

		for i := 0; i < 5; i++ {
			if !limiter.Allow() {
				t.Errorf("request %d should be allowed", i+1)
			}
		}
	})

	t.Run("blocks requests over limit", func(t *testing.T) {
		limiter := websocket.NewRateLimiter(3, time.Minute)

		// Use all 3 requests
		for i := 0; i < 3; i++ {
			limiter.Allow()
		}

		// 4th request should be blocked
		if limiter.Allow() {
			t.Error("4th request should be blocked")
		}
	})

	t.Run("resets after window expires", func(t *testing.T) {
		limiter := websocket.NewRateLimiter(2, 50*time.Millisecond)

		// Use both requests
		limiter.Allow()
		limiter.Allow()

		// Should be blocked
		if limiter.Allow() {
			t.Error("should be blocked before window expires")
		}

		// Wait for window to expire
		time.Sleep(60 * time.Millisecond)

		// Should be allowed again
		if !limiter.Allow() {
			t.Error("should be allowed after window expires")
		}
	})

	t.Run("concurrent access", func(t *testing.T) {
		limiter := websocket.NewRateLimiter(100, time.Minute)
		var wg sync.WaitGroup
		allowed := 0
		var mu sync.Mutex

		// Launch 200 concurrent requests
		for i := 0; i < 200; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if limiter.Allow() {
					mu.Lock()
					allowed++
					mu.Unlock()
				}
			}()
		}

		wg.Wait()

		// Exactly 100 should be allowed
		if allowed != 100 {
			t.Errorf("expected 100 allowed, got %d", allowed)
		}
	})
}

// TestClientChatRateLimiter tests per-client chat rate limiting
func TestClientChatRateLimiter(t *testing.T) {
	t.Run("initializes on first call", func(t *testing.T) {
		client := &websocket.Client{}

		// First call should initialize and allow
		if !client.AllowChatMessage() {
			t.Error("first message should be allowed")
		}
	})

	t.Run("allows 30 messages per minute", func(t *testing.T) {
		client := &websocket.Client{}
		client.InitChatRateLimiter()

		// Should allow 30 messages
		for i := 0; i < 30; i++ {
			if !client.AllowChatMessage() {
				t.Errorf("message %d should be allowed", i+1)
			}
		}

		// 31st message should be blocked
		if client.AllowChatMessage() {
			t.Error("31st message should be blocked")
		}
	})
}

// TestWebSocketMessage tests message parsing
func TestWebSocketMessage(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		expectedType  string
		expectedError bool
	}{
		{
			name:         "valid chat message",
			input:        `{"type":"chat:send","payload":{"content":"Hello"}}`,
			expectedType: "chat:send",
		},
		{
			name:         "valid cursor message",
			input:        `{"type":"cursor","payload":{"x":0.5,"y":0.5,"active":true}}`,
			expectedType: "cursor",
		},
		{
			name:         "valid media toggle",
			input:        `{"type":"media:toggle","payload":{"audio":true}}`,
			expectedType: "media:toggle",
		},
		{
			name:         "valid admin command",
			input:        `{"type":"admin:kick","payload":{"participantId":"abc123"}}`,
			expectedType: "admin:kick",
		},
		{
			name:          "invalid json",
			input:         `{not valid json}`,
			expectedError: true,
		},
		{
			name:          "empty message",
			input:         ``,
			expectedError: true,
		},
		{
			name:         "message without payload",
			input:        `{"type":"ping"}`,
			expectedType: "ping",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var msg websocket.Message
			err := json.Unmarshal([]byte(tt.input), &msg)

			if tt.expectedError {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if msg.Type != tt.expectedType {
				t.Errorf("expected type %q, got %q", tt.expectedType, msg.Type)
			}
		})
	}
}

// TestChatMessageParsing tests chat message payload parsing
func TestChatMessageParsing(t *testing.T) {
	tests := []struct {
		name            string
		payload         string
		expectedContent string
		shouldBeValid   bool
	}{
		{
			name:            "valid message",
			payload:         `{"content":"Hello, world!"}`,
			expectedContent: "Hello, world!",
			shouldBeValid:   true,
		},
		{
			name:          "empty content",
			payload:       `{"content":""}`,
			shouldBeValid: false,
		},
		{
			name:          "whitespace only",
			payload:       `{"content":"   "}`,
			shouldBeValid: false,
		},
		{
			name:            "content with html stripped becomes empty",
			payload:         `{"content":"<script></script>"}`,
			expectedContent: "",
			shouldBeValid:   false,
		},
		{
			name:          "long content",
			payload:       `{"content":"` + string(make([]byte, 2001)) + `"}`,
			shouldBeValid: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var data struct {
				Content string `json:"content"`
			}
			err := json.Unmarshal([]byte(tt.payload), &data)
			if err != nil {
				if tt.shouldBeValid {
					t.Errorf("unexpected parse error: %v", err)
				}
				return
			}

			sanitized := sanitizeText(data.Content)
			isValid := len(sanitized) > 0 && len(sanitized) <= 2000

			if isValid != tt.shouldBeValid {
				t.Errorf("validity: got %v, want %v (sanitized: %q)", isValid, tt.shouldBeValid, sanitized)
			}

			if tt.shouldBeValid && sanitized != tt.expectedContent {
				t.Errorf("content: got %q, want %q", sanitized, tt.expectedContent)
			}
		})
	}
}

// TestCursorMessageParsing tests cursor message payload parsing
func TestCursorMessageParsing(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantX   float64
		wantY   float64
		wantErr bool
	}{
		{
			name:    "valid coordinates",
			payload: `{"x":0.5,"y":0.75,"active":true}`,
			wantX:   0.5,
			wantY:   0.75,
		},
		{
			name:    "boundary coordinates - min",
			payload: `{"x":0,"y":0,"active":true}`,
			wantX:   0,
			wantY:   0,
		},
		{
			name:    "boundary coordinates - max",
			payload: `{"x":1,"y":1,"active":true}`,
			wantX:   1,
			wantY:   1,
		},
		{
			name:    "negative coordinates",
			payload: `{"x":-0.5,"y":-0.5,"active":true}`,
			wantX:   -0.5,
			wantY:   -0.5,
		},
		{
			name:    "coordinates over 1",
			payload: `{"x":1.5,"y":2.0,"active":true}`,
			wantX:   1.5,
			wantY:   2.0,
		},
		{
			name:    "missing active flag",
			payload: `{"x":0.5,"y":0.5}`,
			wantX:   0.5,
			wantY:   0.5,
		},
		{
			name:    "string coordinates",
			payload: `{"x":"invalid","y":"invalid","active":true}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var data struct {
				X      float64 `json:"x"`
				Y      float64 `json:"y"`
				Active bool    `json:"active"`
			}
			err := json.Unmarshal([]byte(tt.payload), &data)

			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if data.X != tt.wantX || data.Y != tt.wantY {
				t.Errorf("got (%v, %v), want (%v, %v)", data.X, data.Y, tt.wantX, tt.wantY)
			}
		})
	}
}

// TestMediaToggleParsing tests media toggle message parsing
func TestMediaToggleParsing(t *testing.T) {
	tests := []struct {
		name      string
		payload   string
		wantAudio *bool
		wantVideo *bool
	}{
		{
			name:      "audio only",
			payload:   `{"audio":true}`,
			wantAudio: boolPtr(true),
			wantVideo: nil,
		},
		{
			name:      "video only",
			payload:   `{"video":false}`,
			wantAudio: nil,
			wantVideo: boolPtr(false),
		},
		{
			name:      "both audio and video",
			payload:   `{"audio":true,"video":false}`,
			wantAudio: boolPtr(true),
			wantVideo: boolPtr(false),
		},
		{
			name:      "neither",
			payload:   `{}`,
			wantAudio: nil,
			wantVideo: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var data struct {
				Audio *bool `json:"audio,omitempty"`
				Video *bool `json:"video,omitempty"`
			}
			err := json.Unmarshal([]byte(tt.payload), &data)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if !ptrEqual(data.Audio, tt.wantAudio) {
				t.Errorf("audio: got %v, want %v", ptrVal(data.Audio), ptrVal(tt.wantAudio))
			}
			if !ptrEqual(data.Video, tt.wantVideo) {
				t.Errorf("video: got %v, want %v", ptrVal(data.Video), ptrVal(tt.wantVideo))
			}
		})
	}
}

// TestAdminCommandParsing tests admin command message parsing
func TestAdminCommandParsing(t *testing.T) {
	t.Run("kick command", func(t *testing.T) {
		payload := `{"participantId":"user123","reason":"Disruptive behavior"}`
		var data struct {
			ParticipantID string  `json:"participantId"`
			Reason        *string `json:"reason,omitempty"`
		}
		err := json.Unmarshal([]byte(payload), &data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if data.ParticipantID != "user123" {
			t.Errorf("participantId: got %q, want %q", data.ParticipantID, "user123")
		}
		if data.Reason == nil || *data.Reason != "Disruptive behavior" {
			t.Errorf("reason: got %v, want %q", strPtrVal(data.Reason), "Disruptive behavior")
		}
	})

	t.Run("mute command", func(t *testing.T) {
		payload := `{"participantId":"user456"}`
		var data struct {
			ParticipantID string `json:"participantId"`
		}
		err := json.Unmarshal([]byte(payload), &data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if data.ParticipantID != "user456" {
			t.Errorf("participantId: got %q, want %q", data.ParticipantID, "user456")
		}
	})

	t.Run("kick command without reason", func(t *testing.T) {
		payload := `{"participantId":"user789"}`
		var data struct {
			ParticipantID string  `json:"participantId"`
			Reason        *string `json:"reason,omitempty"`
		}
		err := json.Unmarshal([]byte(payload), &data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if data.ParticipantID != "user789" {
			t.Errorf("participantId: got %q, want %q", data.ParticipantID, "user789")
		}
		if data.Reason != nil {
			t.Errorf("reason should be nil, got %v", *data.Reason)
		}
	})
}

// TestSignalMessageParsing tests WebRTC signaling message parsing
func TestSignalMessageParsing(t *testing.T) {
	t.Run("offer message", func(t *testing.T) {
		payload := `{"sdp":"v=0\r\no=- 12345..."}`
		var data struct {
			SDP string `json:"sdp"`
		}
		err := json.Unmarshal([]byte(payload), &data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if data.SDP == "" {
			t.Error("SDP should not be empty")
		}
	})

	t.Run("answer message", func(t *testing.T) {
		payload := `{"sdp":"v=0\r\no=- 67890..."}`
		var data struct {
			SDP string `json:"sdp"`
		}
		err := json.Unmarshal([]byte(payload), &data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if data.SDP == "" {
			t.Error("SDP should not be empty")
		}
	})

	t.Run("ICE candidate message", func(t *testing.T) {
		sdpMid := "0"
		sdpMLineIndex := uint16(0)
		payload := `{"candidate":"candidate:1 1 udp 2122260223...","sdpMid":"0","sdpMLineIndex":0}`
		var data struct {
			Candidate     string  `json:"candidate"`
			SDPMid        *string `json:"sdpMid"`
			SDPMLineIndex *uint16 `json:"sdpMLineIndex"`
		}
		err := json.Unmarshal([]byte(payload), &data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if data.Candidate == "" {
			t.Error("candidate should not be empty")
		}
		if data.SDPMid == nil || *data.SDPMid != sdpMid {
			t.Errorf("sdpMid: got %v, want %v", strPtrVal(data.SDPMid), sdpMid)
		}
		if data.SDPMLineIndex == nil || *data.SDPMLineIndex != sdpMLineIndex {
			t.Errorf("sdpMLineIndex: got %v, want %v", data.SDPMLineIndex, sdpMLineIndex)
		}
	})
}

// TestHubRoomManagement tests the WebSocket hub room operations
func TestHubRoomManagement(t *testing.T) {
	hub := websocket.NewHub()
	go hub.Run()
	defer hub.Shutdown()

	t.Run("room creation on first client", func(t *testing.T) {
		if hub.RoomExists("test-room") {
			t.Error("room should not exist before any client joins")
		}
	})

	t.Run("client count for empty room", func(t *testing.T) {
		count := hub.ClientCount("test-room")
		if count != 0 {
			t.Errorf("expected 0 clients, got %d", count)
		}
	})

	t.Run("get clients for non-existent room", func(t *testing.T) {
		clients := hub.GetRoomClients("non-existent")
		if len(clients) != 0 {
			t.Errorf("expected 0 clients, got %d", len(clients))
		}
	})

	t.Run("get client from non-existent room", func(t *testing.T) {
		client := hub.GetClient("non-existent", "user1")
		if client != nil {
			t.Error("expected nil client")
		}
	})
}

// TestBroadcastJSONFormat tests the JSON message format for broadcasts
func TestBroadcastJSONFormat(t *testing.T) {
	tests := []struct {
		name    string
		msgType string
		payload interface{}
	}{
		{
			name:    "chat message",
			msgType: "chat:message",
			payload: map[string]interface{}{
				"participantId":   "user1",
				"participantName": "Test User",
				"type":            "text",
				"content":         "Hello, world!",
			},
		},
		{
			name:    "cursor update",
			msgType: "cursor",
			payload: map[string]interface{}{
				"participantId":   "user1",
				"participantName": "Test User",
				"color":           "#ff0000",
				"x":               0.5,
				"y":               0.5,
				"active":          true,
			},
		},
		{
			name:    "participant joined",
			msgType: "participant:joined",
			payload: map[string]interface{}{
				"participant": map[string]interface{}{
					"id":           "user2",
					"name":         "New User",
					"role":         "viewer",
					"color":        "#00ff00",
					"audioEnabled": true,
					"videoEnabled": true,
				},
			},
		},
		{
			name:    "participant left",
			msgType: "participant:left",
			payload: map[string]interface{}{
				"participantId": "user2",
			},
		},
		{
			name:    "room ended",
			msgType: "room:ended",
			payload: map[string]interface{}{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payloadBytes, err := json.Marshal(tt.payload)
			if err != nil {
				t.Fatalf("failed to marshal payload: %v", err)
			}

			msg := websocket.Message{
				Type:    tt.msgType,
				Payload: payloadBytes,
			}

			msgBytes, err := json.Marshal(msg)
			if err != nil {
				t.Fatalf("failed to marshal message: %v", err)
			}

			// Verify it can be parsed back
			var parsed websocket.Message
			if err := json.Unmarshal(msgBytes, &parsed); err != nil {
				t.Errorf("failed to parse message: %v", err)
			}

			if parsed.Type != tt.msgType {
				t.Errorf("type: got %q, want %q", parsed.Type, tt.msgType)
			}
		})
	}
}

// Helper functions
func boolPtr(b bool) *bool {
	return &b
}

func ptrEqual(a, b *bool) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func ptrVal(p *bool) string {
	if p == nil {
		return "nil"
	}
	if *p {
		return "true"
	}
	return "false"
}

func strPtrVal(p *string) string {
	if p == nil {
		return "nil"
	}
	return *p
}
