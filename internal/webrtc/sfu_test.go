package webrtc

import (
	"testing"

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

// TestRoomTracks_AddSubscriber_ReplacesOldOnRejoin is the regression test for
// the "hang forever on reconnect" bug: when a participant rejoins with the
// same ID, the old subscriber must be displaced (done channel closed, PC
// referenced for teardown) so that its stale Failed/Closed callback can no
// longer destroy the replacement.
func TestRoomTracks_AddSubscriber_ReplacesOldOnRejoin(t *testing.T) {
	room := &RoomTracks{
		RoomSlug:    "test-room",
		Subscribers: make(map[string]*Subscriber),
	}

	old := &Subscriber{ID: "p1", done: make(chan struct{})}
	room.AddSubscriber(old)

	newSub := &Subscriber{ID: "p1", done: make(chan struct{})}
	room.AddSubscriber(newSub)

	if len(room.Subscribers) != 1 {
		t.Fatalf("expected 1 subscriber after rejoin, got %d", len(room.Subscribers))
	}
	if room.Subscribers["p1"] != newSub {
		t.Fatalf("expected new subscriber in map, got old")
	}

	select {
	case <-old.done:
		// good — old's done was closed by the replacement
	default:
		t.Fatal("old subscriber's done channel was not closed on replacement")
	}

	// The simulation of a stale old-PC callback firing must NOT remove the
	// replacement. This is the critical identity-check we rely on.
	room.removeSubscriberIfSame("p1", old)
	if room.Subscribers["p1"] != newSub {
		t.Fatal("stale callback for replaced subscriber removed the live replacement")
	}

	// But an identity-matched removal DOES clear it.
	room.removeSubscriberIfSame("p1", newSub)
	if _, ok := room.Subscribers["p1"]; ok {
		t.Fatal("matched removeSubscriberIfSame did not remove the subscriber")
	}
}

func TestSFU_SetIngest_ReplacesOldOnReconnect(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	token := "obs-key-1"
	old := &IngestSession{StreamKeyToken: token, done: make(chan struct{})}
	newIng := &IngestSession{StreamKeyToken: token, done: make(chan struct{})}

	sfu.SetIngest(token, old)
	sfu.SetIngest(token, newIng)

	// Old session's done must be closed on replacement.
	select {
	case <-old.done:
	default:
		t.Fatal("old ingest's done channel was not closed on replacement")
	}

	// Stale callback must not remove the live replacement.
	sfu.removeIngestIfSame(token, old)
	if got := sfu.GetIngest(token); got != newIng {
		t.Fatalf("stale ingest callback removed the replacement (got %v)", got)
	}

	// Identity-matched removal clears it.
	sfu.removeIngestIfSame(token, newIng)
	if got := sfu.GetIngest(token); got != nil {
		t.Fatal("matched removeIngestIfSame did not clear the ingest")
	}
}

func TestSFU_RemoveVoiceSessionIfSame_IgnoresStaleCallback(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	roomSlug := "test-room"
	// Pre-create the room so GetRoomTracksForSlug finds it.
	sfu.GetRoomTracks(roomSlug)

	participantID := "p1"
	room := sfu.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		t.Fatal("expected room to exist")
	}
	if room.VoiceSessions == nil {
		room.VoiceSessions = make(map[string]*VoiceSession)
	}

	oldVS := &VoiceSession{ParticipantID: participantID, done: make(chan struct{})}
	newVS := &VoiceSession{ParticipantID: participantID, done: make(chan struct{})}

	room.mu.Lock()
	room.VoiceSessions[participantID] = oldVS
	room.mu.Unlock()

	// Replace: caller would normally close old outside the map operation.
	room.mu.Lock()
	room.VoiceSessions[participantID] = newVS
	room.mu.Unlock()

	// Simulate stale callback firing for oldVS — must not remove newVS.
	sfu.removeVoiceSessionIfSame(roomSlug, participantID, oldVS)
	room.mu.RLock()
	got := room.VoiceSessions[participantID]
	room.mu.RUnlock()
	if got != newVS {
		t.Fatal("stale voice-session callback removed the live replacement")
	}

	// Matched removal clears it.
	sfu.removeVoiceSessionIfSame(roomSlug, participantID, newVS)
	room.mu.RLock()
	_, stillThere := room.VoiceSessions[participantID]
	room.mu.RUnlock()
	if stillThere {
		t.Fatal("matched removeVoiceSessionIfSame did not remove the session")
	}
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
}

// TestSFU_SetVoiceMuted verifies the server-side mute gate toggles the
// per-session atomic that the voice relay forwarder checks on each RTP packet.
// Without this gate, admin:mute is advisory-only — a client that ignores the
// broadcast would keep sending audio to every subscriber.
func TestSFU_SetVoiceMuted(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	roomSlug := "test-room"
	participantID := "p1"
	room := sfu.GetRoomTracks(roomSlug)
	if room.VoiceSessions == nil {
		room.VoiceSessions = make(map[string]*VoiceSession)
	}
	vs := &VoiceSession{
		ParticipantID: participantID,
		done:          make(chan struct{}),
	}
	room.mu.Lock()
	room.VoiceSessions[participantID] = vs
	room.mu.Unlock()

	if vs.Muted.Load() {
		t.Fatal("new voice session should start unmuted")
	}

	sfu.SetVoiceMuted(roomSlug, participantID, true)
	if !vs.Muted.Load() {
		t.Fatal("SetVoiceMuted(true) did not flip the atomic")
	}

	sfu.SetVoiceMuted(roomSlug, participantID, false)
	if vs.Muted.Load() {
		t.Fatal("SetVoiceMuted(false) did not clear the atomic")
	}

	// Non-existent room / participant must be a no-op (not panic).
	sfu.SetVoiceMuted("nope", participantID, true)
	sfu.SetVoiceMuted(roomSlug, "nobody", true)
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

func TestSFU_RemoveVoiceSession_NonexistentRoom(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	// Should not panic
	sfu.RemoveVoiceSession("nonexistent", "participant-1")
}

func TestSFU_RemoveVoiceSession_NonexistentParticipant(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	// Create room
	_ = sfu.GetRoomTracks("test-room")

	// Should not panic
	sfu.RemoveVoiceSession("test-room", "nonexistent")
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
