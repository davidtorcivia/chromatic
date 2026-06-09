package webrtc

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"chromatic/internal/config"
	"chromatic/internal/metrics"

	"github.com/pion/webrtc/v4"
)

func init() {
	// Ensure metrics is initialized for tests
	_ = metrics.Get()
}

func createTestConfig() *config.Config {
	return &config.Config{
		Port:            8080,
		DatabasePath:    ":memory:",
		UploadPath:      "/tmp/uploads",
		TurnRealm:       "",
		TurnSecret:      "",
		TurnExternalURL: "",
	}
}

func TestNewSFU(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	if sfu == nil {
		t.Fatal("SFU should not be nil")
	}

	if sfu.api == nil {
		t.Error("SFU API should not be nil")
	}

	if sfu.ingests == nil {
		t.Error("ingests map should be initialized")
	}

	if sfu.rooms == nil {
		t.Error("rooms map should be initialized")
	}
}

func TestSFU_GetICEServers_Default(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	servers := sfu.GetICEServers()

	// Should have at least the default STUN server
	if len(servers) < 1 {
		t.Error("expected at least 1 ICE server")
	}

	// First should be Google STUN
	if len(servers[0].URLs) == 0 {
		t.Error("first ICE server should have URLs")
	}

	foundStun := false
	for _, server := range servers {
		for _, url := range server.URLs {
			if url == "stun:stun.l.google.com:19302" {
				foundStun = true
			}
		}
	}

	if !foundStun {
		t.Error("expected Google STUN server in ICE servers")
	}
}

func TestSFU_GetICEServers_WithTURN(t *testing.T) {
	cfg := &config.Config{
		TurnRealm:  "turn.example.com",
		TurnSecret: "test-secret",
	}
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	servers := sfu.GetICEServers()

	// Should have STUN + TURN
	if len(servers) < 2 {
		t.Error("expected at least 2 ICE servers with TURN configured")
	}
}

func TestSFU_GetICEServers_WithExternalTURN(t *testing.T) {
	cfg := &config.Config{
		TurnExternalURL:  "turn:external.example.com:3478",
		TurnExternalUser: "user",
		TurnExternalPass: "pass",
	}
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	servers := sfu.GetICEServers()

	// Should have STUN + external TURN
	if len(servers) < 2 {
		t.Error("expected at least 2 ICE servers with external TURN configured")
	}

	// Check external TURN server
	foundExternal := false
	for _, server := range servers {
		for _, url := range server.URLs {
			if url == "turn:external.example.com:3478" {
				foundExternal = true
				if server.Username != "user" || server.Credential != "pass" {
					t.Error("external TURN should have correct credentials")
				}
			}
		}
	}

	if !foundExternal {
		t.Error("expected external TURN server in ICE servers")
	}
}

func TestSFU_GetSetRemoveIngest(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	token := "test-token-123"

	// Initially should be nil
	if sfu.GetIngest(token) != nil {
		t.Error("ingest should be nil before setting")
	}

	// Set ingest
	session := &IngestSession{
		StreamKeyToken: token,
		done:           make(chan struct{}),
	}
	sfu.SetIngest(token, session)

	// Should find it now
	got := sfu.GetIngest(token)
	if got == nil {
		t.Error("ingest should be found after setting")
	}
	if got.StreamKeyToken != token {
		t.Errorf("expected token %s, got %s", token, got.StreamKeyToken)
	}

	// Remove ingest
	sfu.RemoveIngest(token)

	// Should be gone
	if sfu.GetIngest(token) != nil {
		t.Error("ingest should be nil after removal")
	}
}

func TestSFU_GetRoomTracks(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	roomSlug := "test-room"

	// Get or create room
	room1 := sfu.GetRoomTracks(roomSlug)
	if room1 == nil {
		t.Error("GetRoomTracks should create room if not exists")
	}
	if room1.RoomSlug != roomSlug {
		t.Errorf("expected room slug %s, got %s", roomSlug, room1.RoomSlug)
	}

	// Get again - should return same room
	room2 := sfu.GetRoomTracks(roomSlug)
	if room1 != room2 {
		t.Error("GetRoomTracks should return same room on second call")
	}
}

func TestSFU_GetRoomTracksForSlug(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	// Should be nil for nonexistent room
	if sfu.GetRoomTracksForSlug("nonexistent") != nil {
		t.Error("should return nil for nonexistent room")
	}

	// Create room
	roomSlug := "test-room"
	_ = sfu.GetRoomTracks(roomSlug)

	// Now should find it
	room := sfu.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		t.Error("should find existing room")
	}
}

func TestSFU_IsRoomLive(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	// Nonexistent room should not be live
	if sfu.IsRoomLive("nonexistent") {
		t.Error("nonexistent room should not be live")
	}

	// Room without tracks should not be live
	roomSlug := "test-room"
	_ = sfu.GetRoomTracks(roomSlug)
	if sfu.IsRoomLive(roomSlug) {
		t.Error("room without tracks should not be live")
	}
}

func TestRoomTracks_AddRemoveSubscriber(t *testing.T) {
	room := &RoomTracks{
		RoomSlug:    "test-room",
		Subscribers: make(map[string]*Subscriber),
	}

	// Add subscriber
	sub := &Subscriber{
		ID:   "sub-1",
		done: make(chan struct{}),
	}
	room.AddSubscriber(sub)

	if len(room.Subscribers) != 1 {
		t.Errorf("expected 1 subscriber, got %d", len(room.Subscribers))
	}

	// Add another
	sub2 := &Subscriber{
		ID:   "sub-2",
		done: make(chan struct{}),
	}
	room.AddSubscriber(sub2)

	if len(room.Subscribers) != 2 {
		t.Errorf("expected 2 subscribers, got %d", len(room.Subscribers))
	}

	// Remove first
	room.RemoveSubscriber("sub-1")

	if len(room.Subscribers) != 1 {
		t.Errorf("expected 1 subscriber after removal, got %d", len(room.Subscribers))
	}

	// Check it's the right one remaining
	if _, ok := room.Subscribers["sub-2"]; !ok {
		t.Error("sub-2 should still be in subscribers")
	}

	// Remove nonexistent (should not panic)
	room.RemoveSubscriber("nonexistent")
}

func TestSFU_BindIngestToRoom_NotFound(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	_, err = sfu.BindIngestToRoom("nonexistent-token", "test-room")
	if err == nil {
		t.Error("expected error for nonexistent ingest")
	}
}

func TestSFU_BindIngestToRoom_Success(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	token := "test-token"
	roomSlug := "test-room"

	// Create ingest session
	session := &IngestSession{
		StreamKeyToken: token,
		done:           make(chan struct{}),
	}
	sfu.SetIngest(token, session)

	// Bind to room
	_, err = sfu.BindIngestToRoom(token, roomSlug)
	if err != nil {
		t.Fatalf("failed to bind ingest to room: %v", err)
	}

	// Verify room was created
	room := sfu.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		t.Error("room should exist after binding")
	}
}

func TestSFU_Shutdown(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}

	// Add some ingests
	token1 := "token-1"
	session1 := &IngestSession{
		StreamKeyToken: token1,
		done:           make(chan struct{}),
	}
	sfu.SetIngest(token1, session1)

	token2 := "token-2"
	session2 := &IngestSession{
		StreamKeyToken: token2,
		done:           make(chan struct{}),
	}
	sfu.SetIngest(token2, session2)

	// Add a room with subscribers
	room := sfu.GetRoomTracks("test-room")
	sub := &Subscriber{
		ID:   "sub-1",
		done: make(chan struct{}),
	}
	room.AddSubscriber(sub)

	// Shutdown should not panic
	sfu.Shutdown()

	// Verify cleanup
	if sfu.GetIngest(token1) != nil {
		t.Error("ingest should be removed after shutdown")
	}
	if sfu.GetIngest(token2) != nil {
		t.Error("ingest should be removed after shutdown")
	}
	if len(sfu.rooms) != 0 {
		t.Error("rooms map should be cleared after shutdown")
	}
	if len(room.Subscribers) != 0 {
		t.Error("room subscribers should be cleared after shutdown")
	}
}

func TestSFU_CreatePeerConnection(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	pc, err := sfu.CreatePeerConnection()
	if err != nil {
		t.Fatalf("failed to create peer connection: %v", err)
	}
	defer pc.Close()

	if pc == nil {
		t.Error("peer connection should not be nil")
	}
}

func TestSFU_SetSubscriberAnswer_RoomNotFound(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	err = sfu.SetSubscriberAnswer("nonexistent", "sub-1", webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  "",
	})
	if err == nil {
		t.Error("expected error for nonexistent room")
	}
}

func TestSFU_AddSubscriberICECandidate_RoomNotFound(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	err = sfu.AddSubscriberICECandidate("nonexistent", "sub-1", webrtc.ICECandidateInit{Candidate: ""})
	if err == nil {
		t.Error("expected error for nonexistent room")
	}
}

func TestSFU_HandleIceRestart_RoomNotFound(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	_, err = sfu.HandleIceRestart("nonexistent", "sub-1", "")
	if err == nil {
		t.Error("expected error for nonexistent room")
	}
}

func TestSFU_RenegotiateSubscriber_RoomNotFound(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	_, err = sfu.RenegotiateSubscriber("nonexistent", "sub-1")
	if err == nil {
		t.Error("expected error for nonexistent room")
	}
}

func TestSFU_HandleRenegotiationAnswer_RoomNotFound(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	err = sfu.HandleRenegotiationAnswer("nonexistent", "sub-1", "")
	if err == nil {
		t.Error("expected error for nonexistent room")
	}
}

func TestRoomTracks_RemoveSubscriberStopsVoiceRelay(t *testing.T) {
	relayDone := make(chan struct{})
	room := &RoomTracks{
		RoomSlug:       "test-room",
		Subscribers:    make(map[string]*Subscriber),
		voiceRelayDone: map[string]chan struct{}{"sub-1": relayDone},
	}

	sub := &Subscriber{
		ID:   "sub-1",
		done: make(chan struct{}),
	}
	room.AddSubscriber(sub)
	room.RemoveSubscriber("sub-1")

	select {
	case <-relayDone:
		// closed as expected
	default:
		t.Error("voice relay done channel should be closed when subscriber is removed")
	}
	if _, ok := room.voiceRelayDone["sub-1"]; ok {
		t.Error("voice relay done entry should be deleted on subscriber removal")
	}
}

func TestExtractProfileLevelID(t *testing.T) {
	cases := []struct {
		name string
		sdp  string
		want string
	}{
		{
			name: "standard fmtp line",
			sdp:  "a=fmtp:97 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
			want: "42e01f",
		},
		{
			name: "uppercase parameter with CRLF",
			sdp:  "a=fmtp:102 PROFILE-LEVEL-ID=4d001f\r\n",
			want: "4d001f",
		},
		{
			name: "no profile-level-id",
			sdp:  "a=fmtp:96 apt=102",
			want: "",
		},
		{
			name: "profile-id must not match",
			sdp:  "a=fmtp:98 profile-id=0",
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractProfileLevelID(tc.sdp); got != tc.want {
				t.Errorf("extractProfileLevelID(%q) = %q, want %q", tc.sdp, got, tc.want)
			}
		})
	}
}

// TestWHIPHandler_TeardownFiresOnce verifies that the WHIP session teardown
// (ingest removal + stream-end notification) runs exactly once across DELETE
// requests and connection state callbacks, and that a session that never
// reached Connected does not produce a stream-end notification.
func TestWHIPHandler_TeardownFiresOnce(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	var endCalls int32
	handler := NewWHIPHandler(sfu,
		func(string) (bool, error) { return true, nil },
		func(string) error { return nil },
		func(string) { atomic.AddInt32(&endCalls, 1) },
	)

	establishSession := func(t *testing.T, token string) *IngestSession {
		t.Helper()

		// Build a real SDP offer with sendonly audio/video, like OBS would send.
		offerPC, err := sfu.CreatePeerConnection()
		if err != nil {
			t.Fatalf("failed to create offer PC: %v", err)
		}
		t.Cleanup(func() { offerPC.Close() })

		for _, kind := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
			if _, err := offerPC.AddTransceiverFromKind(kind, webrtc.RTPTransceiverInit{
				Direction: webrtc.RTPTransceiverDirectionSendonly,
			}); err != nil {
				t.Fatalf("failed to add %s transceiver: %v", kind, err)
			}
		}

		offer, err := offerPC.CreateOffer(nil)
		if err != nil {
			t.Fatalf("failed to create offer: %v", err)
		}
		if err := offerPC.SetLocalDescription(offer); err != nil {
			t.Fatalf("failed to set local description: %v", err)
		}

		req := httptest.NewRequest(http.MethodPost, "/whip/"+token, strings.NewReader(offer.SDP))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("expected 201 from WHIP offer, got %d: %s", rec.Code, rec.Body.String())
		}

		session := sfu.GetIngest(token)
		if session == nil {
			t.Fatal("ingest session should be registered after offer")
		}
		return session
	}

	t.Run("never connected session fires no stream end", func(t *testing.T) {
		token := "whip-never-connected-token"
		session := establishSession(t, token)

		req := httptest.NewRequest(http.MethodDelete, "/whip/"+token, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("expected 204 from DELETE, got %d", rec.Code)
		}

		if sfu.GetIngest(token) != nil {
			t.Error("ingest should be removed after DELETE")
		}
		// Simulate the async Closed state callback racing the DELETE path.
		session.teardown()
		if got := atomic.LoadInt32(&endCalls); got != 0 {
			t.Errorf("never-connected session should not fire stream end, got %d calls", got)
		}
	})

	t.Run("connected session fires stream end exactly once", func(t *testing.T) {
		token := "whip-connected-token-1"
		session := establishSession(t, token)
		// Pretend the session reached Connected.
		session.everConnected.Store(true)

		req := httptest.NewRequest(http.MethodDelete, "/whip/"+token, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("expected 204 from DELETE, got %d", rec.Code)
		}

		// Simulate the async Failed/Closed callbacks firing after DELETE.
		session.teardown()
		session.teardown()
		// Give the real Closed state callback (from pc.Close) time to fire too.
		time.Sleep(200 * time.Millisecond)

		if got := atomic.LoadInt32(&endCalls); got != 1 {
			t.Errorf("stream end should fire exactly once, got %d calls", got)
		}
		if sfu.GetIngest(token) != nil {
			t.Error("ingest should be removed after teardown")
		}
	})
}

func TestMaxICECandidates_Constant(t *testing.T) {
	// Verify the constant is defined and reasonable
	if MaxICECandidates < 10 {
		t.Error("MaxICECandidates should be at least 10")
	}
	if MaxICECandidates > 1000 {
		t.Error("MaxICECandidates should not be more than 1000")
	}
}
