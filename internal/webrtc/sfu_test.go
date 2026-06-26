package webrtc

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
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

// TestRemoveSubscriber_SharerRemovalTearsDownOtherViewers is a regression test
// for frozen screen-share frames. When the active sharer's subscriber is
// removed abruptly (e.g. its peer connection Failed while the WebSocket lived),
// removeSubscriberLocked previously cleared the room's screen-share state but
// left every OTHER viewer's sender bound to the now-dead relay local track — a
// frozen last frame. It must instead remove that sender from each other
// subscriber and report them so callers can renegotiate.
func TestRemoveSubscriber_SharerRemovalTearsDownOtherViewers(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	roomSlug := "screen-room"
	room := sfu.GetRoomTracks(roomSlug)

	// Build a screen-share relay local track and bind it to two viewer PCs.
	videoCodec := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}
	localTrack, err := webrtc.NewTrackLocalStaticRTP(videoCodec, "screenshare-sharer", "screenshare-stream-sharer")
	if err != nil {
		t.Fatalf("failed to create local track: %v", err)
	}

	sharerPC, err := sfu.CreatePeerConnection()
	if err != nil {
		t.Fatalf("failed to create sharer pc: %v", err)
	}
	defer sharerPC.Close()
	sharer := &Subscriber{ID: "sharer", PeerConnection: sharerPC, done: make(chan struct{})}

	viewerPC, err := sfu.CreatePeerConnection()
	if err != nil {
		t.Fatalf("failed to create viewer pc: %v", err)
	}
	defer viewerPC.Close()
	viewerSender, err := viewerPC.AddTrack(localTrack)
	if err != nil {
		t.Fatalf("failed to add screen share track to viewer: %v", err)
	}
	_ = viewerSender
	viewer := &Subscriber{ID: "viewer", PeerConnection: viewerPC, done: make(chan struct{})}

	room.mu.Lock()
	room.Subscribers["sharer"] = sharer
	room.Subscribers["viewer"] = viewer
	room.ScreenShareParticipantID = "sharer"
	room.ScreenShareLocalTrack = localTrack
	room.mu.Unlock()

	// Sanity: the viewer has the screen-share sender bound.
	if !senderBound(viewerPC, localTrack) {
		t.Fatal("viewer should have the screen-share sender bound before sharer removal")
	}

	// Remove the sharer abruptly via the IfSame path (mimics a Failed PC).
	affected := sfu.RemoveSubscriberIfSame(roomSlug, "sharer", sharer)
	if len(affected) != 1 || affected[0] != "viewer" {
		t.Fatalf("expected affected=[viewer], got %v", affected)
	}

	// The viewer's screen-share sender must be gone (no frozen frame)…
	if senderBound(viewerPC, localTrack) {
		t.Fatal("viewer screen-share sender should have been removed when the sharer left")
	}
	// …and the viewer flagged for renegotiation so the m-line is torn down.
	if !viewer.needsRenegotiation {
		t.Fatal("viewer should be flagged needsRenegotiation after its screen-share sender was removed")
	}

	// Room screen-share state cleared.
	room.mu.RLock()
	cleared := room.ScreenShareParticipantID == "" && room.ScreenShareLocalTrack == nil
	room.mu.RUnlock()
	if !cleared {
		t.Fatal("room screen-share state should be cleared after sharer removal")
	}
}

func senderBound(pc *webrtc.PeerConnection, track *webrtc.TrackLocalStaticRTP) bool {
	for _, s := range pc.GetSenders() {
		if s.Track() == track {
			return true
		}
	}
	return false
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

func TestRoomTracks_ScreenShareKeyframeTargetPrefersPublisherPC(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	publisherPC, err := sfu.CreatePeerConnection()
	if err != nil {
		t.Fatalf("failed to create publisher PC: %v", err)
	}
	defer publisherPC.Close()

	subscriberPC, err := sfu.CreatePeerConnection()
	if err != nil {
		t.Fatalf("failed to create subscriber PC: %v", err)
	}
	defer subscriberPC.Close()

	room := &RoomTracks{
		RoomSlug:                 "test-room",
		ScreenShareParticipantID: "p1",
		Subscribers: map[string]*Subscriber{
			"p1": {ID: "p1", PeerConnection: subscriberPC},
		},
		VoiceSessions: map[string]*VoiceSession{
			"p1": {ParticipantID: "p1", PeerConnection: publisherPC},
		},
	}

	if got := room.screenShareKeyframePeerConnectionLocked("p1"); got != publisherPC {
		t.Fatal("screen-share keyframe target should prefer the dedicated publisher PC")
	}
}

func TestRoomTracks_ScreenShareKeyframeTargetFallsBackToSubscriberPC(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	subscriberPC, err := sfu.CreatePeerConnection()
	if err != nil {
		t.Fatalf("failed to create subscriber PC: %v", err)
	}
	defer subscriberPC.Close()

	room := &RoomTracks{
		RoomSlug:                 "test-room",
		ScreenShareParticipantID: "p1",
		Subscribers: map[string]*Subscriber{
			"p1": {ID: "p1", PeerConnection: subscriberPC},
		},
	}

	if got := room.screenShareKeyframePeerConnectionLocked("p1"); got != subscriberPC {
		t.Fatal("screen-share keyframe target should fall back to the subscriber PC")
	}
}

func TestRoomTracks_KeyframeRequestsAreCoalesced(t *testing.T) {
	room := &RoomTracks{RoomSlug: "test-room"}
	now := time.Unix(1_700_000_000, 0)

	if !room.markKeyframeRequestLocked(now) {
		t.Fatal("first keyframe request should pass")
	}
	if room.markKeyframeRequestLocked(now.Add(keyframeRequestMinInterval - time.Millisecond)) {
		t.Fatal("duplicate keyframe request inside coalescing window should be skipped")
	}
	if !room.markKeyframeRequestLocked(now.Add(keyframeRequestMinInterval)) {
		t.Fatal("keyframe request at coalescing boundary should pass")
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

func TestSFU_AbortPublisherOffer_RemovesOnlyMatchingOffer(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	roomSlug := "test-room"
	participantID := "p1"
	room := sfu.GetRoomTracks(roomSlug)

	current := &VoiceSession{
		ParticipantID:    participantID,
		PublisherOfferID: "publish-2",
		done:             make(chan struct{}),
	}
	room.mu.Lock()
	if room.VoiceSessions == nil {
		room.VoiceSessions = make(map[string]*VoiceSession)
	}
	room.VoiceSessions[participantID] = current
	room.mu.Unlock()

	if err := sfu.AbortPublisherOffer(roomSlug, participantID, "publish-1"); !errors.Is(err, ErrStalePublisherOffer) {
		t.Fatalf("expected stale publisher offer error, got %v", err)
	}
	room.mu.RLock()
	got := room.VoiceSessions[participantID]
	room.mu.RUnlock()
	if got != current {
		t.Fatal("stale publish answer cleanup removed the current publisher")
	}

	if err := sfu.AbortPublisherOffer(roomSlug, participantID, "publish-2"); err != nil {
		t.Fatalf("abort current publisher failed: %v", err)
	}
	room.mu.RLock()
	_, stillThere := room.VoiceSessions[participantID]
	room.mu.RUnlock()
	if stillThere {
		t.Fatal("matching publish answer cleanup did not remove publisher")
	}
	select {
	case <-current.done:
		// closed as expected
	default:
		t.Fatal("publisher done channel was not closed")
	}
}

func TestSFU_HandlePublisherOffer_WaitsForExistingSignaling(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	roomSlug := "test-room"
	participantID := "p1"
	pc, err := sfu.CreatePeerConnection()
	if err != nil {
		t.Fatalf("failed to create peer connection: %v", err)
	}
	defer pc.Close()

	room := sfu.GetRoomTracks(roomSlug)
	vs := &VoiceSession{
		ParticipantID:  participantID,
		PeerConnection: pc,
		done:           make(chan struct{}),
	}
	room.mu.Lock()
	if room.VoiceSessions == nil {
		room.VoiceSessions = make(map[string]*VoiceSession)
	}
	room.VoiceSessions[participantID] = vs
	room.mu.Unlock()

	vs.SignalingMu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := sfu.HandlePublisherOffer(roomSlug, participantID, "not sdp", "publish-1", func(string, *webrtc.TrackRemote) {})
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("HandlePublisherOffer returned before signaling lock released: %v", err)
	case <-time.After(50 * time.Millisecond):
		// Expected: an in-flight publisher SDP operation blocks renegotiation.
	}

	vs.SignalingMu.Unlock()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected invalid SDP error after signaling lock released")
		}
	case <-time.After(time.Second):
		t.Fatal("HandlePublisherOffer did not resume after signaling lock released")
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

// TestSFU_BindIngestToRoom_ReplacesExistingSender verifies that rebinding an
// ingest to a room whose subscriber already carries a video sender (a stream
// restart in OBS) correctly re-establishes the media path instead of leaving
// the viewer on a dead sender. ReplaceTrack on a healthy sender is the common
// path; this also covers the recovery fallback (RemoveTrack + AddTrack) that
// runs when ReplaceTrack itself fails, ensuring renegotiation is requested so
// the m-line is refreshed rather than silently swallowed.
func TestSFU_BindIngestToRoom_ReplacesExistingSender(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	token := "restart-token"
	roomSlug := "restart-room"
	room := sfu.GetRoomTracks(roomSlug)

	// Subscriber with a real PC and an initial video sender bound to a stale
	// local track (simulating a prior OBS stream).
	staleTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "stale", "stale-stream")
	if err != nil {
		t.Fatalf("create stale track: %v", err)
	}
	pc, err := sfu.CreatePeerConnection()
	if err != nil {
		t.Fatalf("create pc: %v", err)
	}
	defer pc.Close()
	staleSender, err := pc.AddTrack(staleTrack)
	if err != nil {
		t.Fatalf("add stale track: %v", err)
	}
	sub := &Subscriber{
		ID:             "sub-1",
		PeerConnection: pc,
		VideoSender:    staleSender,
		done:           make(chan struct{}),
	}
	room.AddSubscriber(sub)

	// New ingest with a fresh video track.
	freshTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "fresh", "fresh-stream")
	if err != nil {
		t.Fatalf("create fresh track: %v", err)
	}
	session := &IngestSession{
		StreamKeyToken: token,
		VideoTrack:     freshTrack,
		done:           make(chan struct{}),
	}
	sfu.SetIngest(token, session)

	// Rebind. The subscriber already has a sender, so ReplaceTrack runs. On the
	// success path no renegotiation is needed (in-place replacement); on the
	// fallback path renegotiation IS requested. Either way this must not error
	// and must not leave the sender pointing at the stale track.
	_, err = sfu.BindIngestToRoom(token, roomSlug)
	if err != nil {
		t.Fatalf("rebind failed: %v", err)
	}

	// The sender must now carry the fresh track, not the stale one.
	if sub.VideoSender == nil || sub.VideoSender.Track() != freshTrack {
		t.Fatal("subscriber video sender should reference the fresh ingest track after rebind")
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

// TestSFU_VoiceMuteGate_StableAcrossSessionReplacement is a regression test for
// a silent admin-mute bypass. The voice relay's mute gate is captured by pointer
// when the relay starts. Voice frequently arrives BEFORE the publisher offer, so
// CreateVoiceRelayTrack used to host the gate on a *placeholder* VoiceSession;
// HandleVoiceOffer then replaced that placeholder with a real session, orphaning
// the pointer the relay still read — making SetVoiceMuted a no-op for that
// participant. The gate now lives on RoomTracks.voiceMuteFlags, decoupled from
// VoiceSession identity, so it survives the replacement.
func TestSFU_VoiceMuteGate_StableAcrossSessionReplacement(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	roomSlug := "mute-room"
	participantID := "speaker-1"
	room := sfu.GetRoomTracks(roomSlug)

	// Simulate the relay-start path: CreateVoiceRelayTrack allocates the shared
	// flag and the running relay captures a pointer to it.
	room.mu.Lock()
	if room.voiceMuteFlags == nil {
		room.voiceMuteFlags = make(map[string]*atomic.Bool)
	}
	relayFlag := &atomic.Bool{}
	room.voiceMuteFlags[participantID] = relayFlag
	// A placeholder VoiceSession exists before the offer arrives.
	placeholder := &VoiceSession{ParticipantID: participantID, done: make(chan struct{})}
	room.VoiceSessions = map[string]*VoiceSession{participantID: placeholder}
	room.mu.Unlock()

	// HandleVoiceOffer replaces the placeholder with a brand-new session object.
	realSession := &VoiceSession{ParticipantID: participantID, done: make(chan struct{})}
	room.mu.Lock()
	room.VoiceSessions[participantID] = realSession
	room.mu.Unlock()

	// Admin mutes the participant AFTER the replacement.
	sfu.SetVoiceMuted(roomSlug, participantID, true)

	// The relay reads the flag it captured at start. It must reflect the mute —
	// this is what was broken: the relay kept reading the orphaned placeholder.
	if !relayFlag.Load() {
		t.Fatal("relay's captured mute flag was not flipped by SetVoiceMuted after session replacement (admin mute bypass)")
	}

	// And the canonical map flag must be the same object the relay holds, so
	// there is exactly one source of truth.
	room.mu.RLock()
	canonical := room.voiceMuteFlags[participantID]
	room.mu.RUnlock()
	if canonical != relayFlag {
		t.Fatal("voiceMuteFlags entry must be the same *atomic.Bool the relay captured")
	}
	if realSession.Muted.Load() {
		// mirrored too, for back-compat consumers
	} else {
		t.Fatal("SetVoiceMuted should also mirror onto VoiceSession.Muted")
	}

	// Cleanup must remove the flag so a rejoiner starts unmuted.
	sfu.RemoveVoiceSession(roomSlug, participantID)
	room.mu.RLock()
	_, stillThere := room.voiceMuteFlags[participantID]
	room.mu.RUnlock()
	if stillThere {
		t.Fatal("voice mute flag should be cleared when the voice session is removed")
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
	}, "")
	if err == nil {
		t.Error("expected error for nonexistent room")
	}
}

func TestSFU_SetSubscriberAnswer_IgnoresStaleOfferID(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	roomSlug := "test-room"
	room := sfu.GetRoomTracks(roomSlug)
	pc, err := sfu.CreatePeerConnection()
	if err != nil {
		t.Fatalf("failed to create peer connection: %v", err)
	}
	defer pc.Close()

	sub := &Subscriber{
		ID:             "sub-1",
		PeerConnection: pc,
		done:           make(chan struct{}),
		OfferID:        "current-offer",
	}
	room.AddSubscriber(sub)

	err = sfu.SetSubscriberAnswer(roomSlug, "sub-1", webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  "stale answer",
	}, "old-offer")
	if !errors.Is(err, ErrStaleSubscriberAnswer) {
		t.Fatalf("expected stale answer error, got %v", err)
	}
}

func TestSFU_AddSubscriberICECandidate_RoomNotFound(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	err = sfu.AddSubscriberICECandidate("nonexistent", "sub-1", webrtc.ICECandidateInit{Candidate: ""}, "")
	if err == nil {
		t.Error("expected error for nonexistent room")
	}
}

func TestSFU_AddSubscriberICECandidate_IgnoresStaleCandidateID(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	roomSlug := "test-room"
	room := sfu.GetRoomTracks(roomSlug)
	sub := &Subscriber{
		ID:          "sub-1",
		done:        make(chan struct{}),
		CandidateID: "current-offer",
	}
	room.AddSubscriber(sub)

	candidate := webrtc.ICECandidateInit{Candidate: "candidate:1 1 udp 2122260223 192.0.2.1 54321 typ host"}
	err = sfu.AddSubscriberICECandidate(roomSlug, "sub-1", candidate, "old-offer")
	if !errors.Is(err, ErrStaleSubscriberCandidate) {
		t.Fatalf("expected stale subscriber candidate error, got %v", err)
	}
	if len(sub.pendingRemoteCandidates) != 0 {
		t.Fatalf("stale candidate should not be buffered, got %d pending", len(sub.pendingRemoteCandidates))
	}
}

func TestSFU_EnableSubscriberTrickleICE_UsesDynamicCandidateIDs(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	roomSlug := "test-room"
	room := sfu.GetRoomTracks(roomSlug)
	initialCandidate := webrtc.ICECandidateInit{Candidate: "candidate:initial 1 udp 2122260223 192.0.2.1 54321 typ host"}
	sub := &Subscriber{
		ID:          "sub-1",
		done:        make(chan struct{}),
		CandidateID: "initial-offer",
		pendingCandidates: []SubscriberICECandidate{{
			Candidate:   initialCandidate,
			CandidateID: "initial-offer",
		}},
	}
	room.AddSubscriber(sub)

	var delivered []string
	sfu.EnableSubscriberTrickleICE(roomSlug, "sub-1", func(c *webrtc.ICECandidateInit, candidateID string) {
		delivered = append(delivered, candidateID)
	})

	sub.CandidateID = "renegotiate-offer"
	liveCandidate := webrtc.ICECandidateInit{Candidate: "candidate:renegotiate 1 udp 2122260223 192.0.2.2 54321 typ host"}
	sub.candidateMu.Lock()
	cb := sub.OnICECandidate
	sub.candidateMu.Unlock()
	cb(&liveCandidate, sub.CandidateID)

	if got, want := delivered, []string{"initial-offer", "renegotiate-offer"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("delivered candidate IDs = %v, want %v", got, want)
	}
}

func TestSFU_AddPublisherCandidate_BuffersBeforeSession(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	roomSlug := "test-room"
	participantID := "speaker-1"
	candidate := webrtc.ICECandidateInit{Candidate: "candidate:1 1 udp 2122260223 192.0.2.1 54321 typ host"}

	if err := sfu.AddPublisherCandidate(roomSlug, participantID, candidate, "publish-1"); err != nil {
		t.Fatalf("early publisher candidate should be buffered: %v", err)
	}

	room := sfu.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		t.Fatal("room should exist after buffering early publisher candidate")
	}
	room.mu.RLock()
	pending := room.PendingPublisherICE[participantID]
	room.mu.RUnlock()
	if len(pending) != 1 {
		t.Fatalf("expected one buffered publisher candidate, got %d", len(pending))
	}
	if pending[0].OfferID != "publish-1" {
		t.Fatalf("buffered publisher candidate offer ID = %q, want publish-1", pending[0].OfferID)
	}

	for i := 1; i < MaxICECandidates; i++ {
		if err := sfu.AddPublisherCandidate(roomSlug, participantID, candidate, "publish-1"); err != nil {
			t.Fatalf("candidate %d unexpectedly rejected: %v", i, err)
		}
	}
	if err := sfu.AddPublisherCandidate(roomSlug, participantID, candidate, "publish-1"); err == nil {
		t.Fatal("expected early publisher candidate over budget to be rejected")
	}

	room.mu.Lock()
	room.PendingPublisherICE[participantID] = []PublisherICECandidate{{Candidate: candidate, OfferID: "publish-1"}}
	if room.VoiceSessions == nil {
		room.VoiceSessions = make(map[string]*VoiceSession)
	}
	room.VoiceSessions[participantID] = &VoiceSession{ParticipantID: participantID, PublisherOfferID: "publish-1", done: make(chan struct{})}
	room.mu.Unlock()
	pc, err := sfu.CreatePeerConnection()
	if err != nil {
		t.Fatalf("failed to create peer connection: %v", err)
	}
	defer pc.Close()

	sfu.flushPendingPublisherCandidates(room, participantID, pc)

	room.mu.RLock()
	_, stillPending := room.PendingPublisherICE[participantID]
	room.mu.RUnlock()
	if stillPending {
		t.Fatal("buffered publisher candidates should be cleared after flush")
	}
}

func TestSFU_AddPublisherCandidate_IgnoresStaleOfferID(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	roomSlug := "test-room"
	participantID := "speaker-1"
	pc, err := sfu.CreatePeerConnection()
	if err != nil {
		t.Fatalf("failed to create peer connection: %v", err)
	}
	defer pc.Close()

	room := sfu.GetRoomTracks(roomSlug)
	if room.VoiceSessions == nil {
		room.VoiceSessions = make(map[string]*VoiceSession)
	}
	room.mu.Lock()
	room.VoiceSessions[participantID] = &VoiceSession{
		ParticipantID:    participantID,
		PeerConnection:   pc,
		PublisherOfferID: "publish-current",
		done:             make(chan struct{}),
	}
	room.mu.Unlock()

	candidate := webrtc.ICECandidateInit{Candidate: "candidate:1 1 udp 2122260223 192.0.2.1 54321 typ host"}
	err = sfu.AddPublisherCandidate(roomSlug, participantID, candidate, "publish-old")
	if !errors.Is(err, ErrStalePublisherCandidate) {
		t.Fatalf("expected stale publisher candidate error, got %v", err)
	}
}

func TestSFU_HandleIceRestart_RoomNotFound(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	_, err = sfu.HandleIceRestart("nonexistent", "sub-1", "", "")
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

	_, _, err = sfu.RenegotiateSubscriber("nonexistent", "sub-1")
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

	err = sfu.HandleRenegotiationAnswer("nonexistent", "sub-1", "", "")
	if err == nil {
		t.Error("expected error for nonexistent room")
	}
}

func TestSFU_HandleRenegotiationAnswer_IgnoresStaleOfferID(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	roomSlug := "test-room"
	room := sfu.GetRoomTracks(roomSlug)
	pc, err := sfu.CreatePeerConnection()
	if err != nil {
		t.Fatalf("failed to create peer connection: %v", err)
	}
	defer pc.Close()

	sub := &Subscriber{
		ID:                   "sub-1",
		PeerConnection:       pc,
		done:                 make(chan struct{}),
		RenegotiationOfferID: "current-offer",
	}
	room.AddSubscriber(sub)

	err = sfu.HandleRenegotiationAnswer(roomSlug, "sub-1", "stale answer", "old-offer")
	if !errors.Is(err, ErrStaleRenegotiationAnswer) {
		t.Fatalf("expected stale renegotiation answer error, got %v", err)
	}
}

func TestSFU_AbortSubscriberRenegotiation_RemovesUndeliveredOffer(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	roomSlug := "test-room"
	room := sfu.GetRoomTracks(roomSlug)
	pc, err := sfu.CreatePeerConnection()
	if err != nil {
		t.Fatalf("failed to create peer connection: %v", err)
	}
	defer pc.Close()

	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatalf("failed to add transceiver: %v", err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("failed to create offer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("failed to set local description: %v", err)
	}

	sub := &Subscriber{
		ID:                   "sub-1",
		PeerConnection:       pc,
		done:                 make(chan struct{}),
		RenegotiationOfferID: "renegotiate-1",
		CandidateID:          "renegotiate-1",
	}
	room.AddSubscriber(sub)

	if err := sfu.AbortSubscriberRenegotiation(roomSlug, "sub-1", "renegotiate-1"); err != nil {
		t.Fatalf("abort failed: %v", err)
	}
	if _, ok := room.Subscribers["sub-1"]; ok {
		t.Fatal("subscriber with undelivered offer was not removed")
	}
	select {
	case <-sub.done:
		// closed as expected
	default:
		t.Fatal("subscriber done channel was not closed")
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

// TestSFU_AddSubscriberICECandidate_BudgetResetsPerNegotiation is the
// regression test for the "too many ICE candidates from subscriber" failures
// seen on long sessions: the per-subscriber candidate counter never reset, so
// TURN refreshes / ICE restarts eventually exhausted it and all later
// candidates (including the ones needed for the ICE restart itself) were
// rejected. The budget is now per-negotiation.
func TestSFU_AddSubscriberICECandidate_BudgetResetsPerNegotiation(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	roomSlug := "test-room"
	room := sfu.GetRoomTracks(roomSlug)

	sub := &Subscriber{ID: "sub-1", done: make(chan struct{})}
	room.AddSubscriber(sub)

	candidate := webrtc.ICECandidateInit{Candidate: "candidate:1 1 udp 2122260223 192.0.2.1 54321 typ host"}

	// Exhaust the budget. remoteDescSet is false, so candidates are buffered
	// and never touch the PeerConnection.
	for i := 0; i < MaxICECandidates; i++ {
		if err := sfu.AddSubscriberICECandidate(roomSlug, "sub-1", candidate, ""); err != nil {
			t.Fatalf("candidate %d unexpectedly rejected: %v", i, err)
		}
	}
	if err := sfu.AddSubscriberICECandidate(roomSlug, "sub-1", candidate, ""); err == nil {
		t.Fatal("expected candidate over budget to be rejected")
	}

	// A new negotiation resets the budget; candidates flow again.
	sub.resetICECandidateBudget()
	if err := sfu.AddSubscriberICECandidate(roomSlug, "sub-1", candidate, ""); err != nil {
		t.Fatalf("candidate after budget reset unexpectedly rejected: %v", err)
	}

	// ...and the fresh budget is still bounded.
	for i := 0; i < MaxICECandidates-1; i++ {
		if err := sfu.AddSubscriberICECandidate(roomSlug, "sub-1", candidate, ""); err != nil {
			t.Fatalf("candidate %d of fresh budget unexpectedly rejected: %v", i, err)
		}
	}
	if err := sfu.AddSubscriberICECandidate(roomSlug, "sub-1", candidate, ""); err == nil {
		t.Fatal("expected candidate over the fresh budget to be rejected")
	}
}
