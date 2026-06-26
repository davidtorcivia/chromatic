package webrtc

import (
	"strings"
	"testing"
)

// TestSFU_PreStreamSubscriber verifies a subscriber can be created for a
// room with no ingest bound (lobby voice: participants join and talk
// before the host starts streaming). The offer may have no media
// sections; tracks attach later via BindIngestToRoom / voice relay
// renegotiation, both of which handle senderless subscribers.
func TestSFU_PreStreamSubscriber(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	roomSlug := "prestream-room"
	// Materialize the room with no tracks (what GetRoomTracks does on join)
	room := sfu.GetRoomTracks(roomSlug)
	if room == nil {
		t.Fatal("expected room to be created")
	}
	if sfu.IsRoomLive(roomSlug) {
		t.Fatal("room must not be live before ingest binds")
	}

	pc, _, offerSDP, _, err := sfu.CreateSubscriberConnection(roomSlug, "viewer-1")
	if err != nil {
		t.Fatalf("pre-stream subscriber creation failed: %v", err)
	}
	defer pc.Close()

	if offerSDP == "" || !strings.HasPrefix(offerSDP, "v=0") {
		t.Fatalf("expected a valid SDP offer, got %q", offerSDP)
	}
	if !sfu.HasSubscriber(roomSlug, "viewer-1") {
		t.Fatal("subscriber should be registered with the SFU")
	}
}
