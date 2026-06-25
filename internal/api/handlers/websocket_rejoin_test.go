package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"chromatic/internal/config"
	"chromatic/internal/database"
	"chromatic/internal/webrtc"
	"chromatic/internal/websocket"

	gorillaws "github.com/gorilla/websocket"
	pionwebrtc "github.com/pion/webrtc/v4"
)

// rejoinTestEnv wires a real DB, hub, SFU and WebSocketHandler behind an
// httptest server so we can exercise the actual (re)connect path a browser
// refresh takes.
type rejoinTestEnv struct {
	t       *testing.T
	db      *database.DB
	hub     *websocket.Hub
	sfu     *webrtc.SFU
	handler *WebSocketHandler
	server  *httptest.Server
	slug    string
	token   string
	name    string
}

func newRejoinTestEnv(t *testing.T) (*rejoinTestEnv, func()) {
	t.Helper()

	db, dbCleanup := database.NewTestDB(t)

	hub := websocket.NewHub()
	go hub.Run()

	sfu, err := webrtc.NewSFU(&config.Config{})
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}

	secret := []byte("test-secret-for-rejoin")
	handler := NewWebSocketHandler(db, hub, sfu, nil, false, secret, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws/room/{slug}", handler.HandleConnection)
	server := httptest.NewServer(mux)

	// Seed a live room with a bound SFU track set (as if OBS were streaming).
	slug := "rejoin-room"
	if _, err := db.Exec(`INSERT INTO rooms (id, slug, name, status) VALUES ('room1', ?, 'Rejoin Room', 'live')`, slug); err != nil {
		t.Fatalf("failed to insert room: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO participants (id, room_id, name, role, color, is_admitted) VALUES ('part1', 'room1', 'Viewer One', 'viewer', '#e63946', 1)`); err != nil {
		t.Fatalf("failed to insert participant: %v", err)
	}

	videoTrack, err := pionwebrtc.NewTrackLocalStaticRTP(
		pionwebrtc.RTPCodecCapability{
			MimeType:    pionwebrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
		}, "video", "chromatic-stream")
	if err != nil {
		t.Fatalf("failed to create video track: %v", err)
	}
	audioTrack, err := pionwebrtc.NewTrackLocalStaticRTP(
		pionwebrtc.RTPCodecCapability{
			MimeType:  pionwebrtc.MimeTypeOpus,
			ClockRate: 48000,
			Channels:  2,
		}, "audio", "chromatic-stream")
	if err != nil {
		t.Fatalf("failed to create audio track: %v", err)
	}
	room := sfu.GetRoomTracks(slug)
	room.VideoTrack = videoTrack
	room.AudioTrack = audioTrack

	tm := NewTokenManager(secret)
	name := "Viewer One"
	token, err := tm.GenerateToken("part1", slug, name, time.Hour)
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	env := &rejoinTestEnv{
		t:       t,
		db:      db,
		hub:     hub,
		sfu:     sfu,
		handler: handler,
		server:  server,
		slug:    slug,
		token:   token,
		name:    name,
	}
	cleanup := func() {
		server.Close()
		sfu.Shutdown()
		hub.Shutdown()
		dbCleanup()
	}
	return env, cleanup
}

func (env *rejoinTestEnv) dial() *gorillaws.Conn {
	env.t.Helper()
	url := "ws" + strings.TrimPrefix(env.server.URL, "http") +
		"/ws/room/" + env.slug + "?token=" + env.token + "&name=" + strings.ReplaceAll(env.name, " ", "%20")
	conn, _, err := gorillaws.DefaultDialer.Dial(url, nil)
	if err != nil {
		env.t.Fatalf("failed to dial websocket: %v", err)
	}
	return conn
}

type wsTestMessage struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// waitForMessages reads from the connection until all wanted message types
// have been seen or the timeout elapses. Returns the collected messages.
func waitForMessages(t *testing.T, conn *gorillaws.Conn, timeout time.Duration, want ...string) map[string]json.RawMessage {
	t.Helper()
	got := make(map[string]json.RawMessage)
	deadline := time.Now().Add(timeout)
	for {
		remaining := len(want)
		for _, w := range want {
			if _, ok := got[w]; ok {
				remaining--
			}
		}
		if remaining == 0 {
			return got
		}
		conn.SetReadDeadline(deadline)
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("did not receive %v within %s (got %v so far): %v", want, timeout, keysOf(got), err)
		}
		var msg wsTestMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatalf("invalid message: %v", err)
		}
		if _, ok := got[msg.Type]; !ok {
			got[msg.Type] = msg.Payload
		}
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func assertRoomStateLive(t *testing.T, payload json.RawMessage) {
	t.Helper()
	var state struct {
		IsLive bool `json:"isLive"`
	}
	if err := json.Unmarshal(payload, &state); err != nil {
		t.Fatalf("invalid room:state payload: %v", err)
	}
	if !state.IsLive {
		t.Fatal("room:state should report isLive=true for a live room")
	}
}

// TestRejoinWhileLive_AfterClose simulates a browser refresh where the old
// WebSocket close is observed by the server BEFORE the new connection arrives.
// The reconnecting client must receive room:state (isLive) and a fresh
// signal:offer within a couple of seconds.
func TestRejoinWhileLive_AfterClose(t *testing.T) {
	env, cleanup := newRejoinTestEnv(t)
	defer cleanup()

	// First connection: must get room:state + signal:offer
	conn1 := env.dial()
	msgs := waitForMessages(t, conn1, 5*time.Second, "room:state", "signal:offer")
	assertRoomStateLive(t, msgs["room:state"])

	// Simulate refresh: abrupt close, then give the server time to run the
	// old client's disconnect cleanup before reconnecting.
	conn1.Close()
	time.Sleep(300 * time.Millisecond)

	conn2 := env.dial()
	defer conn2.Close()
	msgs2 := waitForMessages(t, conn2, 5*time.Second, "room:state", "signal:offer")
	assertRoomStateLive(t, msgs2["room:state"])

	// The new subscriber must still be registered with the SFU after the old
	// connection's teardown paths have all settled.
	time.Sleep(500 * time.Millisecond)
	if !subscriberExists(env.sfu, env.slug, "part1") {
		t.Fatal("subscriber was torn down after rejoin (stale cleanup removed the new subscriber)")
	}
}

// TestRejoinWhileLive_BeforeClose simulates a refresh where the new
// connection arrives BEFORE the server has noticed the old socket dropping
// (typical behind reverse proxies). The hub replaces the old client; the old
// client's deferred disconnect cleanup must not tear down the new
// connection's state.
func TestRejoinWhileLive_BeforeClose(t *testing.T) {
	env, cleanup := newRejoinTestEnv(t)
	defer cleanup()

	conn1 := env.dial()
	msgs := waitForMessages(t, conn1, 5*time.Second, "room:state", "signal:offer")
	assertRoomStateLive(t, msgs["room:state"])

	// Reconnect while the first socket is still open from the client's
	// perspective. The server will close conn1 when registering the new client.
	conn2 := env.dial()
	defer conn2.Close()
	msgs2 := waitForMessages(t, conn2, 5*time.Second, "room:state", "signal:offer")
	assertRoomStateLive(t, msgs2["room:state"])

	conn1.Close()
	time.Sleep(500 * time.Millisecond)
	if !subscriberExists(env.sfu, env.slug, "part1") {
		t.Fatal("subscriber was torn down after rejoin (stale cleanup removed the new subscriber)")
	}
}

func subscriberExists(sfu *webrtc.SFU, slug, id string) bool {
	room := sfu.GetRoomTracksForSlug(slug)
	if room == nil {
		return false
	}
	room.RLockVoiceTracks()
	defer room.RUnlockVoiceTracks()
	return room.Subscribers[id] != nil
}

// browserSim is a minimal stand-in for the web client: it answers server
// offers and exchanges trickle ICE over the WebSocket, mirroring
// web/src/lib/webrtc/manager.ts handleOffer/handleCandidate behavior.
type browserSim struct {
	t    *testing.T
	conn *gorillaws.Conn
	pc   *pionwebrtc.PeerConnection

	connected chan struct{}
	// offerReceived is closed when the first signal:offer arrives, letting
	// tests assert the server actually initiated the subscription handshake.
	offerReceived chan struct{}
	errCh         chan error
	// Serializes writes: gorilla connections do not allow concurrent writers
	// (the pump goroutine and the test goroutine both send).
	writeMu sync.Mutex
}

func newBrowserSim(t *testing.T, conn *gorillaws.Conn) *browserSim {
	t.Helper()
	pc, err := pionwebrtc.NewPeerConnection(pionwebrtc.Configuration{})
	if err != nil {
		t.Fatalf("failed to create client peer connection: %v", err)
	}
	b := &browserSim{t: t, conn: conn, pc: pc, connected: make(chan struct{}), offerReceived: make(chan struct{}), errCh: make(chan error, 1)}
	pc.OnConnectionStateChange(func(state pionwebrtc.PeerConnectionState) {
		if state == pionwebrtc.PeerConnectionStateConnected {
			select {
			case <-b.connected:
			default:
				close(b.connected)
			}
		}
	})
	pc.OnICECandidate(func(c *pionwebrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		b.send("signal:candidate", map[string]interface{}{
			"candidate":     init.Candidate,
			"sdpMid":        init.SDPMid,
			"sdpMLineIndex": init.SDPMLineIndex,
		})
	})
	b.startPump()
	return b
}

func (b *browserSim) send(msgType string, payload interface{}) {
	raw, err := json.Marshal(payload)
	if err != nil {
		b.t.Errorf("marshal payload: %v", err)
		return
	}
	msg, _ := json.Marshal(wsTestMessage{Type: msgType, Payload: raw})
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	b.conn.WriteMessage(gorillaws.TextMessage, msg)
}

// startPump launches the single reader goroutine that services all signaling
// messages for the lifetime of the connection (gorilla connections only allow
// one concurrent reader). All PC mutations happen on this goroutine.
func (b *browserSim) startPump() {
	go func() {
		for {
			_, raw, err := b.conn.ReadMessage()
			if err != nil {
				select {
				case b.errCh <- err:
				default:
				}
				return
			}
			var msg wsTestMessage
			if err := json.Unmarshal(raw, &msg); err != nil {
				continue
			}
			if err := b.handleSignal(msg); err != nil {
				select {
				case b.errCh <- err:
				default:
				}
				return
			}
		}
	}()
}

// handleSignal mirrors the web client's signaling behavior (manager.ts):
// answer server offers/renegotiations (rolling back a pending local offer on
// glare, as the browser does), apply answers to client-initiated offers, and
// add trickle ICE candidates.
func (b *browserSim) handleSignal(msg wsTestMessage) error {
	switch msg.Type {
	case "signal:offer", "signal:renegotiate":
		if msg.Type == "signal:offer" {
			select {
			case <-b.offerReceived:
			default:
				close(b.offerReceived)
			}
		}
		var data struct {
			SDP     string `json:"sdp"`
			OfferID string `json:"offerId"`
		}
		json.Unmarshal(msg.Payload, &data)
		if b.pc.SignalingState() == pionwebrtc.SignalingStateHaveLocalOffer {
			// Glare: server offer wins (matches manager.ts rollback behavior)
			if err := b.pc.SetLocalDescription(pionwebrtc.SessionDescription{Type: pionwebrtc.SDPTypeRollback}); err != nil {
				return err
			}
		}
		offer := pionwebrtc.SessionDescription{Type: pionwebrtc.SDPTypeOffer, SDP: data.SDP}
		if err := b.pc.SetRemoteDescription(offer); err != nil {
			return err
		}
		answer, err := b.pc.CreateAnswer(nil)
		if err != nil {
			return err
		}
		if err := b.pc.SetLocalDescription(answer); err != nil {
			return err
		}
		if msg.Type == "signal:offer" {
			b.send("signal:answer", map[string]interface{}{"sdp": answer.SDP, "offerId": data.OfferID})
		} else {
			b.send("signal:renegotiate-answer", map[string]interface{}{"sdp": answer.SDP, "offerId": data.OfferID})
		}
	case "signal:voice-answer", "signal:answer":
		var data struct {
			SDP string `json:"sdp"`
		}
		json.Unmarshal(msg.Payload, &data)
		if b.pc.SignalingState() == pionwebrtc.SignalingStateHaveLocalOffer {
			if err := b.pc.SetRemoteDescription(pionwebrtc.SessionDescription{Type: pionwebrtc.SDPTypeAnswer, SDP: data.SDP}); err != nil {
				return err
			}
		}
	case "signal:error":
		// The server only sends this when the subscription handshake failed
		// hard; surface it so tests fail with the real cause.
		return fmt.Errorf("server sent signal:error: %s", string(msg.Payload))
	case "signal:candidate":
		var data struct {
			Candidate     string  `json:"candidate"`
			SDPMid        *string `json:"sdpMid"`
			SDPMLineIndex *uint16 `json:"sdpMLineIndex"`
		}
		json.Unmarshal(msg.Payload, &data)
		b.pc.AddICECandidate(pionwebrtc.ICECandidateInit{
			Candidate:     data.Candidate,
			SDPMid:        data.SDPMid,
			SDPMLineIndex: data.SDPMLineIndex,
		})
	}
	return nil
}

// pumpUntilConnected waits for the PC to reach Connected (the pump goroutine
// services signaling in the background).
func (b *browserSim) pumpUntilConnected(timeout time.Duration) error {
	select {
	case <-b.connected:
		return nil
	case err := <-b.errCh:
		return err
	case <-time.After(timeout):
		return errTimeout
	}
}

var errTimeout = fmt.Errorf("timed out waiting for connection")

func (b *browserSim) close() {
	b.pc.Close()
	b.conn.Close()
}

// TestRejoinWhileLive_FullNegotiation walks a viewer through a complete
// offer/answer/ICE handshake, then simulates a page refresh and verifies the
// replacement subscriber also reaches Connected within a couple of seconds.
func TestRejoinWhileLive_FullNegotiation(t *testing.T) {
	env, cleanup := newRejoinTestEnv(t)
	defer cleanup()

	sim1 := newBrowserSim(t, env.dial())
	if err := sim1.pumpUntilConnected(90 * time.Second); err != nil {
		t.Fatalf("first connection never reached Connected: %v", err)
	}

	// Refresh: drop everything client-side, then reconnect with the same token.
	sim1.close()
	time.Sleep(200 * time.Millisecond)

	sim2 := newBrowserSim(t, env.dial())
	defer sim2.close()
	if err := sim2.pumpUntilConnected(90 * time.Second); err != nil {
		t.Fatalf("rejoin connection never reached Connected: %v", err)
	}

	// Stale teardown from the first PC must not have removed the new subscriber.
	time.Sleep(500 * time.Millisecond)
	if !subscriberExists(env.sfu, env.slug, "part1") {
		t.Fatal("subscriber removed after rejoin")
	}
}
