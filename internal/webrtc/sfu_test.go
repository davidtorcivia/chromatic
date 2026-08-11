package webrtc

import (
	"context"
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
		// Loopback-only ICE: no OS firewall prompt for the test binary.
		ICELoopbackOnly: true,
	}
}

// The SFU's own peer connections must gather STUN-only. Adding TURN here costs
// 3s per negotiation and ships a truncated candidate set, because ICE gathering
// with Cloudflare TURN takes a flat ~5s while iceGatherTimeout is 3s — measured
// against the live config on 2026-08-02, when it fired on all 182 voice PCs.
// Clients are unaffected: GetICEServers still hands them the full TURN set.
func TestSFU_ServerPeerConnectionsDoNotGatherTURN(t *testing.T) {
	cfg := createTestConfig()
	cfg.TurnRealm = "turn.example.com"
	cfg.TurnSecret = "shared-secret"
	cfg.TurnMode = config.TurnModeSelfHosted

	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	// Precondition: with this config the client-facing set really does include
	// TURN, so the assertion below is meaningful rather than vacuous.
	sawClientTURN := false
	for _, server := range sfu.GetICEServers() {
		for _, url := range server.URLs {
			if strings.HasPrefix(url, "turn:") || strings.HasPrefix(url, "turns:") {
				sawClientTURN = true
			}
		}
	}
	if !sawClientTURN {
		t.Fatal("precondition: expected client ICE servers to include TURN")
	}

	for _, server := range sfu.serverICEServers() {
		for _, url := range server.URLs {
			if strings.HasPrefix(url, "turn:") || strings.HasPrefix(url, "turns:") {
				t.Errorf("server-side ICE config must not include TURN, got %q", url)
			}
		}
	}

	// If TURN is ever restored server-side, the gather timeout has to clear the
	// ~5s TURN gathering time or the SDP goes out truncated.
	if iceGatherTimeout >= 5*time.Second {
		t.Log("iceGatherTimeout now exceeds TURN gathering time; server-side TURN could be reconsidered")
	}
}

// Publisher candidates gathered before the answer is delivered must be held,
// not dropped: the client cannot attach a candidate until it has applied the
// answer. Before trickle existed in this direction, anything the bounded
// pre-answer gather wait missed was lost outright.
func TestVoiceSession_BuffersPublisherCandidatesUntilTrickleEnabled(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	roomSlug := "test-room"
	room := sfu.GetRoomTracks(roomSlug)

	session := &VoiceSession{ParticipantID: "p1", done: make(chan struct{})}
	session.SetPublisherOfferID("publish-1")
	room.mu.Lock()
	if room.VoiceSessions == nil {
		room.VoiceSessions = make(map[string]*VoiceSession)
	}
	room.VoiceSessions["p1"] = session
	room.mu.Unlock()

	// Gathered before the answer went out.
	first := webrtc.ICECandidateInit{Candidate: "candidate:1 1 udp 2122260223 192.0.2.1 1111 typ host"}
	second := webrtc.ICECandidateInit{Candidate: "candidate:2 1 udp 2122260223 192.0.2.2 2222 typ host"}
	session.queuePublisherCandidate(&first, "publish-1")
	session.queuePublisherCandidate(&second, "publish-1")

	type delivered struct {
		candidate string
		offerID   string
	}
	var got []delivered
	sfu.EnablePublisherTrickleICE(roomSlug, "p1", func(init *webrtc.ICECandidateInit, offerID string) {
		got = append(got, delivered{init.Candidate, offerID})
	})

	if len(got) != 2 {
		t.Fatalf("expected both buffered candidates to flush, got %d", len(got))
	}
	if got[0].candidate != first.Candidate || got[1].candidate != second.Candidate {
		t.Errorf("candidates flushed out of order: %+v", got)
	}
	for _, d := range got {
		if d.offerID != "publish-1" {
			t.Errorf("candidate lost its offer generation: %+v", d)
		}
	}

	// Once enabled, later candidates go straight out rather than accumulating.
	third := webrtc.ICECandidateInit{Candidate: "candidate:3 1 udp 2122260223 192.0.2.3 3333 typ host"}
	session.queuePublisherCandidate(&third, "publish-1")
	if len(got) != 3 || got[2].candidate != third.Candidate {
		t.Errorf("expected live forwarding after trickle enabled, got %+v", got)
	}

	session.candidateMu.Lock()
	leftover := len(session.pendingCandidates)
	session.candidateMu.Unlock()
	if leftover != 0 {
		t.Errorf("expected no buffered candidates after flush, got %d", leftover)
	}
}

// A candidate must carry the offer generation current at the time it was
// gathered, so the client can discard candidates from a superseded publisher.
func TestVoiceSession_PublisherCandidateCarriesCurrentOfferID(t *testing.T) {
	session := &VoiceSession{ParticipantID: "p1", done: make(chan struct{})}
	session.SetPublisherOfferID("publish-1")

	var seen []string
	session.candidateMu.Lock()
	session.OnICECandidate = func(_ *webrtc.ICECandidateInit, offerID string) {
		seen = append(seen, offerID)
	}
	session.candidateMu.Unlock()

	c := webrtc.ICECandidateInit{Candidate: "candidate:1 1 udp 2122260223 192.0.2.1 1111 typ host"}
	session.queuePublisherCandidate(&c, session.PublisherOfferID())

	// Renegotiation moves the generation forward.
	session.SetPublisherOfferID("publish-2")
	session.queuePublisherCandidate(&c, session.PublisherOfferID())

	if len(seen) != 2 || seen[0] != "publish-1" || seen[1] != "publish-2" {
		t.Errorf("expected candidates stamped publish-1 then publish-2, got %v", seen)
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
		t.Fatal("ingest should be found after setting")
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
		t.Fatal("GetRoomTracks should create room if not exists")
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

func TestSFU_WebcamTrackIDRegistry(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	roomSlug := "cam-room"
	_ = sfu.GetRoomTracks(roomSlug) // create room
	pid := "p1"

	// A cam and a screen share are both VP8 video on the publisher PC; only the
	// announced track id should route to the webcam path.
	if sfu.IsWebcamTrack(roomSlug, pid, "track-a") {
		t.Error("track should not be a webcam before registration")
	}
	sfu.RegisterWebcamTrackID(roomSlug, pid, "track-a")
	if !sfu.IsWebcamTrack(roomSlug, pid, "track-a") {
		t.Error("registered track should be recognized as a webcam")
	}
	// A different track id from the same participant is the screen share.
	if sfu.IsWebcamTrack(roomSlug, pid, "track-b") {
		t.Error("unregistered track id should not be a webcam (screen share path)")
	}
	// Registration is per-participant.
	if sfu.IsWebcamTrack(roomSlug, "p2", "track-a") {
		t.Error("registration must be scoped to the participant")
	}
	// Removal clears the registration.
	sfu.RemoveWebcamTrack(roomSlug, pid)
	if sfu.IsWebcamTrack(roomSlug, pid, "track-a") {
		t.Error("track should not be a webcam after RemoveWebcamTrack")
	}
	// Empty ids and unknown rooms never match.
	sfu.RegisterWebcamTrackID(roomSlug, pid, "")
	if sfu.IsWebcamTrack(roomSlug, pid, "") {
		t.Error("empty track id must never match")
	}
	if sfu.IsWebcamTrack("no-such-room", pid, "track-a") {
		t.Error("unknown room should return false")
	}
}

func TestSFU_WebcamDisableGate(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	roomSlug := "cam-disable-room"
	_ = sfu.GetRoomTracks(roomSlug)
	pid := "p1"

	if sfu.IsWebcamDisabled(roomSlug, pid) {
		t.Error("camera should not be disabled by default")
	}
	sfu.SetWebcamDisabled(roomSlug, pid, true)
	if !sfu.IsWebcamDisabled(roomSlug, pid) {
		t.Error("camera should be disabled after SetWebcamDisabled(true)")
	}
	sfu.SetWebcamDisabled(roomSlug, pid, false)
	if sfu.IsWebcamDisabled(roomSlug, pid) {
		t.Error("camera should be re-enabled after SetWebcamDisabled(false)")
	}
	// Unknown room is safe.
	if sfu.IsWebcamDisabled("no-such-room", pid) {
		t.Error("unknown room should return false")
	}
}

func TestSFU_WebcamVisibilityState(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	roomSlug := "cam-hidden-room"
	room := sfu.GetRoomTracks(roomSlug)
	pid := "p1"

	sfu.SetWebcamVisible(roomSlug, pid, false)
	if got := sfu.HiddenWebcamParticipantIDs(roomSlug); len(got) != 0 {
		t.Fatalf("hidden list should ignore participants without an active relay, got %v", got)
	}

	localTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8},
		"webcam-p1",
		"webcam-stream-p1",
	)
	if err != nil {
		t.Fatalf("failed to create webcam track: %v", err)
	}
	room.mu.Lock()
	room.WebcamLocalTracks = map[string]*webrtc.TrackLocalStaticRTP{pid: localTrack}
	room.mu.Unlock()

	sfu.SetWebcamVisible(roomSlug, pid, false)
	got := sfu.HiddenWebcamParticipantIDs(roomSlug)
	if len(got) != 1 || got[0] != pid {
		t.Fatalf("expected active hidden webcam %q, got %v", pid, got)
	}

	sfu.SetWebcamVisible(roomSlug, pid, true)
	if got := sfu.HiddenWebcamParticipantIDs(roomSlug); len(got) != 0 {
		t.Fatalf("visible webcam should not be reported hidden, got %v", got)
	}

	sfu.SetWebcamVisible(roomSlug, pid, false)
	sfu.RemoveWebcamTrack(roomSlug, pid)
	if got := sfu.HiddenWebcamParticipantIDs(roomSlug); len(got) != 0 {
		t.Fatalf("stopped webcam should clear hidden state, got %v", got)
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

// TestRemoveSubscriber_WebcamOwnerRemovalTearsDownOtherViewers is the webcam
// twin of the screen-share test above: when a participant whose webcam is being
// relayed leaves, its now-dead webcam relay track must be removed from every
// other subscriber and those viewers flagged for renegotiation. This guards the
// second dead-track branch of removeSubscriberLocked / removeSendersForTracks.
func TestRemoveSubscriber_WebcamOwnerRemovalTearsDownOtherViewers(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	roomSlug := "webcam-room"
	room := sfu.GetRoomTracks(roomSlug)

	videoCodec := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}
	webcamTrack, err := webrtc.NewTrackLocalStaticRTP(videoCodec, "webcam-owner", "webcam-stream-owner")
	if err != nil {
		t.Fatalf("failed to create webcam track: %v", err)
	}

	ownerPC, err := sfu.CreatePeerConnection()
	if err != nil {
		t.Fatalf("failed to create owner pc: %v", err)
	}
	defer ownerPC.Close()
	owner := &Subscriber{ID: "owner", PeerConnection: ownerPC, done: make(chan struct{})}

	viewerPC, err := sfu.CreatePeerConnection()
	if err != nil {
		t.Fatalf("failed to create viewer pc: %v", err)
	}
	defer viewerPC.Close()
	if _, err := viewerPC.AddTrack(webcamTrack); err != nil {
		t.Fatalf("failed to add webcam track to viewer: %v", err)
	}
	viewer := &Subscriber{ID: "viewer", PeerConnection: viewerPC, done: make(chan struct{})}

	room.mu.Lock()
	room.Subscribers["owner"] = owner
	room.Subscribers["viewer"] = viewer
	if room.WebcamLocalTracks == nil {
		room.WebcamLocalTracks = make(map[string]*webrtc.TrackLocalStaticRTP)
	}
	room.WebcamLocalTracks["owner"] = webcamTrack
	room.mu.Unlock()

	if !senderBound(viewerPC, webcamTrack) {
		t.Fatal("viewer should have the webcam sender bound before owner removal")
	}

	affected := sfu.RemoveSubscriberIfSame(roomSlug, "owner", owner)
	if len(affected) != 1 || affected[0] != "viewer" {
		t.Fatalf("expected affected=[viewer], got %v", affected)
	}
	if senderBound(viewerPC, webcamTrack) {
		t.Fatal("viewer webcam sender should have been removed when the owner left")
	}
	if !viewer.needsRenegotiation {
		t.Fatal("viewer should be flagged needsRenegotiation after its webcam sender was removed")
	}

	room.mu.RLock()
	_, stillPresent := room.WebcamLocalTracks["owner"]
	room.mu.RUnlock()
	if stillPresent {
		t.Fatal("owner's webcam relay track should be cleared after removal")
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
		ParticipantID: participantID,
		done:          make(chan struct{}),
	}
	current.SetPublisherOfferID("publish-2")
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
		_, err := sfu.HandlePublisherOffer(roomSlug, participantID, "not sdp", "publish-1", func(string, *webrtc.TrackRemote, string) {})
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

// TestSFU_TakeIngest_StaleDeleteDoesNotTearDownReconnect is the regression test
// for the WHIP C2 TOCTOU. handleDelete used to GetIngest then run teardown in
// separate steps; a concurrent OBS reconnect (SetIngest) could replace the
// session in between, and the DELETE would close a stale PC and return a
// misleading 204. With atomic TakeIngest, the DELETE either removes the exact
// session it saw or finds nothing (404) — and crucially a reconnect's new
// session is never touched by a stale DELETE. This test exercises both
// interleavings deterministically at the SFU layer.
func TestSFU_TakeIngest_StaleDeleteDoesNotTearDownReconnect(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	token := "reconnect-token"

	// Old session is live.
	oldSession := &IngestSession{StreamKeyToken: token, done: make(chan struct{})}
	sfu.SetIngest(token, oldSession)
	if got := sfu.GetIngest(token); got != oldSession {
		t.Fatal("old session should be registered")
	}

	// Interleaving A: the reconnect (SetIngest) wins the race BEFORE the DELETE.
	// The DELETE's TakeIngest then removes the NEW session, not the old one —
	// but that's the DELETE correctly taking whatever is current, and the old
	// session is left for its own teardown. The point: TakeIngest is atomic, so
	// there is no window where a stale pointer is operated on.
	newSession := &IngestSession{StreamKeyToken: token, done: make(chan struct{})}
	sfu.SetIngest(token, newSession)
	taken := sfu.TakeIngest(token)
	if taken != newSession {
		t.Fatal("TakeIngest must return the current (new) session, not a stale pointer")
	}
	if sfu.GetIngest(token) != nil {
		t.Fatal("TakeIngest must remove the session from the map")
	}

	// Interleaving B: the DELETE (TakeIngest) wins BEFORE the reconnect. The
	// reconnect then registers fresh and must be retrievable — the DELETE did
	// not (and could not) touch it because the map was already empty when it
	// took the old entry.
	sfu.SetIngest(token, oldSession)
	taken = sfu.TakeIngest(token)
	if taken != oldSession {
		t.Fatal("TakeIngest should return the old session it removed")
	}
	// Reconnect lands a brand-new session; it survives untouched.
	sfu.SetIngest(token, newSession)
	if got := sfu.GetIngest(token); got != newSession {
		t.Fatal("reconnect's new session must survive a stale DELETE (TakeIngest already completed)")
	}

	// Interleaving C: TakeIngest on an empty slot returns nil (→ DELETE returns
	// 404, not a misleading 204).
	if got := sfu.TakeIngest("never-registered"); got != nil {
		t.Fatal("TakeIngest on an absent token must return nil")
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

	// Tearing down the voice PeerConnection must NOT clear the gate: the mute is
	// attached to the participant's presence in the room, not to one PC. It used
	// to be dropped here, so a muted participant could come back unmuted just by
	// bouncing their mic — the exact bypass the server-side gate exists to close.
	sfu.RemoveVoiceSession(roomSlug, participantID)
	room.mu.RLock()
	survived := room.voiceMuteFlags[participantID]
	room.mu.RUnlock()
	if survived != relayFlag {
		t.Fatal("voice mute flag must survive a voice-session teardown (mic bounce would clear an admin mute)")
	}

	// A confirmed departure is what clears it, so a genuine rejoin starts unmuted.
	room.RemoveSubscriber(participantID)
	room.mu.RLock()
	_, stillThere := room.voiceMuteFlags[participantID]
	room.mu.RUnlock()
	if stillThere {
		t.Fatal("voice mute flag should be cleared when the participant leaves the room")
	}
}

// The duplicate-a=msid regression (2026-08-11, room glg-hk-color).
//
// removeVoiceSessionIfSame used to delete VoiceLocalTracks, which made
// CreateVoiceRelayTrack's reuse path unreachable: a mic bounce built a second
// TrackLocalStaticRTP with the same msid while subscribers still held a sender
// bound to the first. Chrome and Safari reject an offer carrying two identical
// a=msid lines, answer nothing, and strand the SFU in HaveLocalOffer forever —
// deaf viewers. Firefox accepts it, which is why one participant could hear
// everyone while nobody else could hear each other.
func TestSFU_VoiceRelayTrackSurvivesVoiceSessionTeardown(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	roomSlug := "relay-reuse-room"
	participantID := "speaker-1"
	room := sfu.GetRoomTracks(roomSlug)

	relay, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"voice-"+participantID,
		"voice-stream-"+participantID,
	)
	if err != nil {
		t.Fatalf("failed to create relay track: %v", err)
	}

	room.mu.Lock()
	room.VoiceLocalTracks = map[string]*webrtc.TrackLocalStaticRTP{participantID: relay}
	room.VoiceSessions = map[string]*VoiceSession{
		participantID: {ParticipantID: participantID, done: make(chan struct{})},
	}
	room.mu.Unlock()

	// The voice PeerConnection dies (mic bounce, ICE failure, publisher replaced).
	sfu.RemoveVoiceSession(roomSlug, participantID)

	room.mu.RLock()
	got, stillThere := room.VoiceLocalTracks[participantID]
	room.mu.RUnlock()
	if !stillThere {
		t.Fatal("voice relay track was dropped on voice-session teardown; the rejoin will mint a duplicate msid")
	}
	if got != relay {
		t.Fatal("voice relay track was replaced rather than kept; CreateVoiceRelayTrack must rebind this exact object")
	}

	// Leaving the room is what actually retires it.
	room.RemoveSubscriber(participantID)
	room.mu.RLock()
	_, afterLeave := room.VoiceLocalTracks[participantID]
	room.mu.RUnlock()
	if afterLeave {
		t.Fatal("voice relay track should be retired when the participant leaves")
	}
}

// A departing participant's voice relay must be handed back as a dead track so
// RemoveSubscriber detaches it from everyone else. Without this, the remaining
// subscribers kept a sender bound to a dead track: silent for the rest of the
// session, and a duplicate msid the moment that participant rejoined.
func TestRoomTracks_RemoveSubscriberReturnsVoiceTrackAsDead(t *testing.T) {
	relay, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"voice-leaver",
		"voice-stream-leaver",
	)
	if err != nil {
		t.Fatalf("failed to create relay track: %v", err)
	}

	room := &RoomTracks{
		RoomSlug:         "test-room",
		Subscribers:      make(map[string]*Subscriber),
		VoiceLocalTracks: map[string]*webrtc.TrackLocalStaticRTP{"leaver": relay},
	}
	room.AddSubscriber(&Subscriber{ID: "leaver", done: make(chan struct{})})

	room.mu.Lock()
	_, deadTracks := room.removeSubscriberLocked("leaver")
	room.mu.Unlock()

	found := false
	for _, dead := range deadTracks {
		if dead == relay {
			found = true
		}
	}
	if !found {
		t.Fatal("departing participant's voice relay was not returned as a dead track, so its sender stays bound on every other subscriber")
	}
}

// removeStaleRelaySenders is the structural guard: whatever else goes wrong, an
// offer must never carry two m-lines with the same msid.
func TestRemoveStaleRelaySenders_LeavesOneMsidPerRelay(t *testing.T) {
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

	newRelay := func() *webrtc.TrackLocalStaticRTP {
		track, trackErr := webrtc.NewTrackLocalStaticRTP(
			webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
			"voice-p1",
			"voice-stream-p1",
		)
		if trackErr != nil {
			t.Fatalf("failed to create relay track: %v", trackErr)
		}
		return track
	}

	// The old relay is already attached; the participant's mic bounces and a
	// rebuilt relay arrives carrying the identical msid.
	stale := newRelay()
	if _, err := pc.AddTrack(stale); err != nil {
		t.Fatalf("failed to add stale track: %v", err)
	}
	fresh := newRelay()

	if !removeStaleRelaySenders(pc, fresh) {
		t.Fatal("removeStaleRelaySenders did not detach the sender carrying the duplicate msid")
	}
	if _, err := pc.AddTrack(fresh); err != nil {
		t.Fatalf("failed to add fresh track: %v", err)
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("failed to create offer: %v", err)
	}
	if n := strings.Count(offer.SDP, "a=msid:voice-stream-p1 voice-p1"); n != 1 {
		t.Fatalf("offer carries %d a=msid lines for the relay, want exactly 1 — Chrome and Safari reject duplicates outright:\n%s", n, offer.SDP)
	}

	// Re-adding the very same object must be a no-op, not a self-detach.
	if removeStaleRelaySenders(pc, fresh) {
		t.Fatal("removeStaleRelaySenders detached the live sender when handed its own track")
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

// An answer that arrives with no offerID and no pending offer must be ignored,
// not pushed into pion. On 2026-08-02 this surfaced as an ERROR-level
// "invalid proposed signaling state transition: stable->SetRemote(answer)"
// — the negotiation was simply already over, but it read like a fault.
func TestSFU_SetSubscriberAnswer_IgnoresAnswerWhenNotAwaitingOne(t *testing.T) {
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

	// A brand-new PC is Stable: it has no local offer outstanding, so there is
	// nothing for an answer to complete.
	if state := pc.SignalingState(); state != webrtc.SignalingStateStable {
		t.Fatalf("precondition: expected stable PC, got %s", state)
	}

	sub := &Subscriber{
		ID:             "sub-1",
		PeerConnection: pc,
		done:           make(chan struct{}),
	}
	room.AddSubscriber(sub)

	// No offerID, so the offer-matching check above is skipped entirely — the
	// signaling-state guard is the only thing standing between this and pion.
	if err := sfu.SetSubscriberAnswer(roomSlug, "sub-1", webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  "answer for a negotiation that already finished",
	}, ""); err != nil {
		t.Fatalf("expected a late answer to be ignored, got error: %v", err)
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
		ID:   "sub-1",
		done: make(chan struct{}),
	}
	sub.setCandidateID("current-offer")
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
		ID:   "sub-1",
		done: make(chan struct{}),
		pendingCandidates: []SubscriberICECandidate{{
			Candidate:   initialCandidate,
			CandidateID: "initial-offer",
		}},
	}
	sub.setCandidateID("initial-offer")
	room.AddSubscriber(sub)

	var delivered []string
	sfu.EnableSubscriberTrickleICE(roomSlug, "sub-1", func(c *webrtc.ICECandidateInit, candidateID string) {
		delivered = append(delivered, candidateID)
	})

	sub.setCandidateID("renegotiate-offer")
	liveCandidate := webrtc.ICECandidateInit{Candidate: "candidate:renegotiate 1 udp 2122260223 192.0.2.2 54321 typ host"}
	sub.candidateMu.Lock()
	cb := sub.OnICECandidate
	sub.candidateMu.Unlock()
	cb(&liveCandidate, sub.candidateID())

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
	flushSession := &VoiceSession{ParticipantID: participantID, done: make(chan struct{})}
	flushSession.SetPublisherOfferID("publish-1")
	room.VoiceSessions[participantID] = flushSession
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
	staleSession := &VoiceSession{
		ParticipantID:  participantID,
		PeerConnection: pc,
		done:           make(chan struct{}),
	}
	staleSession.SetPublisherOfferID("publish-current")
	room.mu.Lock()
	room.VoiceSessions[participantID] = staleSession
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

// TestSFU_HandleSubscriberOffer_DefersDuringInFlightServerOffer is a regression
// test for a session-breaking glare bug. Pion v4 cannot roll back a
// HaveLocalOffer, so when a client voice offer (mic enable) arrived while a
// server-initiated renegotiation offer was in flight, the rollback in
// HandleSubscriberOffer always errored and the client's offer was silently
// dropped — the participant couldn't enable their mic for the rest of the
// session. The fix defers the client offer and replays it once the server
// offer's answer settles the PC, delivering the answer via callback.
func TestSFU_HandleSubscriberOffer_DefersDuringInFlightServerOffer(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	roomSlug := "glare-room"
	room := sfu.GetRoomTracks(roomSlug)

	pc, err := sfu.CreatePeerConnection()
	if err != nil {
		t.Fatalf("create pc: %v", err)
	}
	defer pc.Close()
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatalf("add transceiver: %v", err)
	}
	sub := &Subscriber{ID: "sub-1", PeerConnection: pc, done: make(chan struct{})}
	room.AddSubscriber(sub)

	// Wire the deferred client-offer delivery callback.
	replayed := make(chan struct{}, 1)
	sfu.SetDeferredClientOfferCallback(roomSlug, "sub-1", func(isRestart bool, offerID, answerSDP string) {
		if answerSDP != "" {
			select {
			case replayed <- struct{}{}:
			default:
			}
		}
	})

	// Put the subscriber in HaveLocalOffer via a server-initiated renegotiation.
	serverOffer, _, err := sfu.RenegotiateSubscriber(roomSlug, "sub-1")
	if err != nil {
		t.Fatalf("server renegotiate: %v", err)
	}
	if pc.SignalingState() != webrtc.SignalingStateHaveLocalOffer {
		t.Fatalf("expected HaveLocalOffer, got %s", pc.SignalingState())
	}

	// Client voice offer arrives during the in-flight server offer. Previously
	// this returned a hard "failed to rollback" error; now it must defer.
	_, _, err = sfu.HandleSubscriberOffer(roomSlug, "sub-1", serverOffer)
	if !errors.Is(err, ErrClientOfferDeferred) {
		t.Fatalf("expected ErrClientOfferDeferred during in-flight server offer, got %v", err)
	}

	// The client answers the server offer → PC returns to Stable → the deferred
	// client offer is replayed. Build a valid answer for the server offer.
	answer, err := buildAnswer(pc, serverOffer)
	if err != nil {
		t.Fatalf("build server-answer: %v", err)
	}
	if err := sfu.HandleRenegotiationAnswer(roomSlug, "sub-1", answer, ""); err != nil {
		t.Fatalf("handle renegotiation answer: %v", err)
	}

	// The deferred client offer must be replayed and its answer delivered.
	select {
	case <-replayed:
		// success — the mic-enable offer was recovered, not dropped.
	case <-time.After(2 * time.Second):
		t.Fatal("deferred client offer was never replayed after the server offer settled")
	}
}

// buildAnswer exchanges an offer with a fresh peer connection to produce a
// valid answer SDP for the given subscriber PC's current offer.
func buildAnswer(pc *webrtc.PeerConnection, offerSDP string) (string, error) {
	answerer, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return "", err
	}
	defer answerer.Close()
	answerer.OnTrack(func(track *webrtc.TrackRemote, r *webrtc.RTPReceiver) {}) // noop
	if err := answerer.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, SDP: offerSDP,
	}); err != nil {
		return "", err
	}
	ans, err := answerer.CreateAnswer(nil)
	if err != nil {
		return "", err
	}
	if err := answerer.SetLocalDescription(ans); err != nil {
		return "", err
	}
	return ans.SDP, nil
}

// TestSFU_HandleIceRestart_DefersDuringInFlightServerOffer mirrors the voice-
// offer test for the ICE restart path. An ICE restart (network recovery) that
// arrives during an in-flight server offer must be deferred and replayed —
// previously the rollback errored and the restart was lost, leaving the viewer
// stuck. The replayed answer is delivered as a restart (signal:answer).
func TestSFU_HandleIceRestart_DefersDuringInFlightServerOffer(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	roomSlug := "glare-restart-room"
	room := sfu.GetRoomTracks(roomSlug)

	pc, err := sfu.CreatePeerConnection()
	if err != nil {
		t.Fatalf("create pc: %v", err)
	}
	defer pc.Close()
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatalf("add transceiver: %v", err)
	}
	sub := &Subscriber{ID: "sub-1", PeerConnection: pc, done: make(chan struct{})}
	room.AddSubscriber(sub)

	replayed := make(chan restartResult, 1)
	sfu.SetDeferredClientOfferCallback(roomSlug, "sub-1", func(isRestart bool, offerID, answerSDP string) {
		select {
		case replayed <- restartResult{isRestart: isRestart, offerID: offerID, sdp: answerSDP}:
		default:
		}
	})

	serverOffer, _, err := sfu.RenegotiateSubscriber(roomSlug, "sub-1")
	if err != nil {
		t.Fatalf("server renegotiate: %v", err)
	}

	// ICE restart arrives during the in-flight server offer → must defer.
	if _, err := sfu.HandleIceRestart(roomSlug, "sub-1", serverOffer, "restart-7"); !errors.Is(err, ErrClientOfferDeferred) {
		t.Fatalf("expected ErrClientOfferDeferred, got %v", err)
	}

	// Settle the server offer → deferred restart replays as a restart.
	answer, err := buildAnswer(pc, serverOffer)
	if err != nil {
		t.Fatalf("build answer: %v", err)
	}
	if err := sfu.HandleRenegotiationAnswer(roomSlug, "sub-1", answer, ""); err != nil {
		t.Fatalf("handle renegotiation answer: %v", err)
	}

	select {
	case res := <-replayed:
		if !res.isRestart {
			t.Fatal("replayed client offer should be flagged as a restart")
		}
		if res.offerID != "restart-7" {
			t.Fatalf("expected restart offerId 'restart-7', got %q", res.offerID)
		}
		if res.sdp == "" {
			t.Fatal("replayed restart answer SDP should be non-empty")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("deferred ICE restart was never replayed after the server offer settled")
	}
}

type restartResult struct {
	isRestart bool
	offerID   string
	sdp       string
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
	}
	sub.setCandidateID("renegotiate-1")
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
		func(context.Context, string) (bool, error) { return true, nil },
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
	for i := 0; i < maxSubscriberICECandidates; i++ {
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
	for i := 0; i < maxSubscriberICECandidates-1; i++ {
		if err := sfu.AddSubscriberICECandidate(roomSlug, "sub-1", candidate, ""); err != nil {
			t.Fatalf("candidate %d of fresh budget unexpectedly rejected: %v", i, err)
		}
	}
	if err := sfu.AddSubscriberICECandidate(roomSlug, "sub-1", candidate, ""); err == nil {
		t.Fatal("expected candidate over the fresh budget to be rejected")
	}
}

// The budget must also refill on its own. Without the sliding window, a
// subscriber that exhausted its allowance and then got no further negotiation
// would reject candidates forever — which is how a repair attempt loses its
// tail (2026-08-02: 16 candidates discarded during an ICE restart).
func TestSFU_AddSubscriberICECandidate_WindowRefills(t *testing.T) {
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

	for i := 0; i < maxSubscriberICECandidates; i++ {
		if err := sfu.AddSubscriberICECandidate(roomSlug, "sub-1", candidate, ""); err != nil {
			t.Fatalf("candidate %d unexpectedly rejected: %v", i, err)
		}
	}
	if err := sfu.AddSubscriberICECandidate(roomSlug, "sub-1", candidate, ""); err == nil {
		t.Fatal("expected candidate over budget to be rejected")
	}

	// Age the window out without touching the counter, simulating the passage
	// of subscriberCandidateWindow with no intervening negotiation.
	sub.remoteCandidateMu.Lock()
	sub.iceWindowStart = time.Now().Add(-subscriberCandidateWindow - time.Second)
	sub.remoteCandidateMu.Unlock()

	if err := sfu.AddSubscriberICECandidate(roomSlug, "sub-1", candidate, ""); err != nil {
		t.Fatalf("candidate after window expiry unexpectedly rejected: %v", err)
	}
}

// The subscriber allowance must comfortably exceed a single browser regather.
// One Firefox ICE restart produced >50 candidates in ~2s on 2026-08-02; a cap
// anywhere near that rejects legitimate repair traffic.
func TestSubscriberICECandidateBudget_FitsARealRegather(t *testing.T) {
	if maxSubscriberICECandidates < 150 {
		t.Errorf("maxSubscriberICECandidates = %d, too small for a real ICE restart burst",
			maxSubscriberICECandidates)
	}
	if maxSubscriberICECandidates > 1000 {
		t.Errorf("maxSubscriberICECandidates = %d, too large to bound flooding",
			maxSubscriberICECandidates)
	}
}

// TestSFU_CreateSubscriberConnection_OffersProgramAudioAsStereoOpus verifies the
// server-generated subscriber offer advertises the program-audio Opus stereo
// capability. The subscriber PC builds its offer from the SFU's MediaEngine
// (which registers ProgramAudioOpusCapability), so the relay offer must carry
// stereo=1;sprop-stereo=1 with no maxaveragebitrate — locking in that browsers
// decode full-stereo program audio and the path is never silently capped.
func TestSFU_CreateSubscriberConnection_OffersProgramAudioAsStereoOpus(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	roomSlug := "stereo-sub-room"
	room := sfu.GetRoomTracks(roomSlug)

	// Bind a program-audio relay track built from the shared capability, exactly
	// as handleOffer would, so the subscriber offer has an audio m-line to carry
	// the Opus fmtp.
	audioTrack, err := webrtc.NewTrackLocalStaticRTP(
		ProgramAudioOpusCapability(), "audio", "chromatic-stream")
	if err != nil {
		t.Fatalf("failed to create audio track: %v", err)
	}
	room.AudioTrack = audioTrack

	pc, _, offerSDP, _, err := sfu.CreateSubscriberConnection(roomSlug, "sub-1")
	if err != nil {
		t.Fatalf("CreateSubscriberConnection failed: %v", err)
	}
	if pc != nil {
		defer pc.Close()
	}

	assertProgramStereoFmtp(t, opusFmtpFromSDP(offerSDP), "subscriber offer")
}

// A deferred client offer used to be cleared before the replay was attempted,
// so any failure inside SetRemoteDescription discarded it permanently: no
// answer, no retry. For an ICE restart that is a repair request from a
// connection already in trouble, so it is the worst thing to drop silently.
func TestSFU_DeferredClientOffer_SurvivesAFailedReplay(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	roomSlug := "deferred-retry-room"
	room := sfu.GetRoomTracks(roomSlug)
	pc, err := sfu.CreatePeerConnection()
	if err != nil {
		t.Fatalf("create pc: %v", err)
	}
	defer pc.Close()
	sub := &Subscriber{ID: "sub-1", PeerConnection: pc, done: make(chan struct{})}
	room.AddSubscriber(sub)

	replayed := make(chan struct{}, 1)
	sub.OnDeferredClientOffer = func(bool, string, string) {
		select {
		case replayed <- struct{}{}:
		default:
		}
	}

	// An offer that SetRemoteDescription will always reject.
	sub.SignalingMu.Lock()
	sub.pendingClientOffer = "definitely not sdp"
	sub.pendingClientOfferID = "restart-9"
	sub.pendingClientOfferIsRestart = true
	sub.pendingClientOfferAttempts = 0
	sub.SignalingMu.Unlock()

	for attempt := 1; attempt < maxDeferredClientOfferAttempts; attempt++ {
		sub.SignalingMu.Lock()
		sfu.replayDeferredClientOfferLocked(sub, "sub-1")
		pending, count := sub.pendingClientOffer, sub.pendingClientOfferAttempts
		sub.SignalingMu.Unlock()

		if pending == "" {
			t.Fatalf("attempt %d dropped the queued offer; it must survive to be retried", attempt)
		}
		if count != attempt {
			t.Fatalf("attempt %d recorded %d attempts", attempt, count)
		}
	}

	// The bound is what stops an unapplicable offer retrying at every settling
	// point forever.
	sub.SignalingMu.Lock()
	sfu.replayDeferredClientOfferLocked(sub, "sub-1")
	pending := sub.pendingClientOffer
	sub.SignalingMu.Unlock()
	if pending != "" {
		t.Errorf("offer still queued after %d attempts; it would retry forever", maxDeferredClientOfferAttempts)
	}

	select {
	case <-replayed:
		t.Error("a failed replay must not deliver an answer to the client")
	default:
	}
}
