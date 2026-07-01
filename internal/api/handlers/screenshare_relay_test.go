package handlers

import (
	"net/http"
	"testing"
	"time"

	gorillaws "github.com/gorilla/websocket"
	"github.com/pion/rtp"
	pionwebrtc "github.com/pion/webrtc/v4"
)

// screenShareSim extends browserSim with the ability to start a screen share:
// it adds a sendonly VP8 video track, performs the client-initiated
// renegotiation (signal:offer -> signal:voice-answer) and pumps synthetic RTP.
type screenShareSim struct {
	*browserSim
	id string
}

// TestScreenShareRelay_EndToEnd verifies the full screen share path:
// sharer adds a video track via client renegotiation -> server OnTrack ->
// relay track fan-out -> viewer receives the screen share track AND RTP
// packets flow to the viewer.
func TestScreenShareRelay_EndToEnd(t *testing.T) {
	env, cleanup := newRejoinTestEnv(t)
	defer cleanup()

	// Second participant (viewer)
	if _, err := env.db.Exec(`INSERT INTO participants (id, room_id, name, role, color, is_admitted) VALUES ('part2', 'room1', 'Viewer Two', 'viewer', '#2a9d8f', 1)`); err != nil {
		t.Fatalf("failed to insert viewer: %v", err)
	}
	tm := NewTokenManager([]byte("test-secret-for-rejoin"))
	viewerToken, err := tm.GenerateToken("part2", env.slug, "Viewer Two", time.Hour)
	if err != nil {
		t.Fatalf("token: %v", err)
	}

	// Connect sharer (part1) and viewer (part2)
	sharer := newBrowserSim(t, env.dial())
	defer sharer.close()
	if err := sharer.pumpUntilConnected(90 * time.Second); err != nil {
		t.Fatalf("sharer never connected: %v", err)
	}

	viewerConn := dialWith(t, env, viewerToken, "Viewer Two")
	viewer := newBrowserSim(t, viewerConn)
	defer viewer.close()

	// Track reception of the screen share on the viewer side.
	gotScreenTrack := make(chan *pionwebrtc.TrackRemote, 1)
	viewer.pc.OnTrack(func(track *pionwebrtc.TrackRemote, _ *pionwebrtc.RTPReceiver) {
		if track.Kind() == pionwebrtc.RTPCodecTypeVideo && len(track.StreamID()) >= len("screenshare-stream-") && track.StreamID()[:len("screenshare-stream-")] == "screenshare-stream-" {
			select {
			case gotScreenTrack <- track:
			default:
			}
		}
	})
	if err := viewer.pumpUntilConnected(90 * time.Second); err != nil {
		t.Fatalf("viewer never connected: %v", err)
	}

	// Sharer adds a VP8 screen track and renegotiates (mirrors manager.ts startScreenShare)
	screenTrack, err := pionwebrtc.NewTrackLocalStaticRTP(
		pionwebrtc.RTPCodecCapability{MimeType: pionwebrtc.MimeTypeVP8, ClockRate: 90000},
		"screen", "display-stream-abc")
	if err != nil {
		t.Fatalf("create screen track: %v", err)
	}
	if _, err := sharer.pc.AddTrack(screenTrack); err != nil {
		t.Fatalf("add screen track: %v", err)
	}
	offer, err := sharer.pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	if err := sharer.pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local: %v", err)
	}
	sharer.send("signal:offer", map[string]interface{}{"sdp": offer.SDP})

	// Write RTP packets continuously so server OnTrack fires and the relay flows.
	stopRTP := make(chan struct{})
	defer close(stopRTP)
	go func() {
		seq := uint16(0)
		ts := uint32(0)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopRTP:
				return
			case <-ticker.C:
				pkt := &rtp.Packet{
					Header: rtp.Header{
						Version:        2,
						PayloadType:    96,
						SequenceNumber: seq,
						Timestamp:      ts,
						SSRC:           12345,
					},
					Payload: []byte{0x10, 0x00, 0x9d, 0x01, 0x2a, 0x01, 0x02, 0x03},
				}
				seq++
				ts += 3000
				_ = screenTrack.WriteRTP(pkt)
			}
		}
	}()

	// Viewer must receive the screen share track...
	var remoteTrack *pionwebrtc.TrackRemote
	select {
	case remoteTrack = <-gotScreenTrack:
	case <-time.After(90 * time.Second):
		t.Fatal("viewer never received the screen share track")
	}

	// ...and actual RTP packets must flow.
	readDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 1500)
		_, _, err := remoteTrack.Read(buf)
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("failed reading screen share RTP at viewer: %v", err)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("no screen share RTP reached the viewer within 30s (black screen)")
	}
}

func dialWith(t *testing.T, env *rejoinTestEnv, token, name string) *gorillaws.Conn {
	t.Helper()
	url := "ws" + env.server.URL[len("http"):] + "/ws/room/" + env.slug + "?name=" + urlEncodeSpaces(name)
	headers := http.Header{}
	headers.Add("Cookie", (&http.Cookie{Name: JoinTokenCookieName(env.slug), Value: token}).String())
	conn, _, err := gorillaws.DefaultDialer.Dial(url, headers)
	if err != nil {
		t.Fatalf("failed to dial websocket: %v", err)
	}
	return conn
}

func urlEncodeSpaces(s string) string {
	out := ""
	for _, r := range s {
		if r == ' ' {
			out += "%20"
		} else {
			out += string(r)
		}
	}
	return out
}
