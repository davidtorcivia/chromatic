package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"chromatic/internal/config"
	"chromatic/internal/database"
	"chromatic/internal/webrtc"
	"chromatic/internal/websocket"

	gorillaws "github.com/gorilla/websocket"
	pionwebrtc "github.com/pion/webrtc/v4"
)

// streamStartTestEnv wires a real DB, hub, SFU, WebSocketHandler AND
// RoomHandler (with the production onRoomLive hook) behind an httptest server
// so we can exercise the full "viewer connects before OBS starts streaming"
// sequence end to end.
type streamStartTestEnv struct {
	t           *testing.T
	db          *database.DB
	hub         *websocket.Hub
	sfu         *webrtc.SFU
	roomHandler *RoomHandler
	server      *httptest.Server
	slug        string
	token       string
	name        string
	streamKey   string
}

func newStreamStartTestEnv(t *testing.T, roomStatus string) (*streamStartTestEnv, func()) {
	t.Helper()

	db, dbCleanup := database.NewTestDB(t)

	hub := websocket.NewHub()
	go hub.Run()

	sfu, err := webrtc.NewSFU(&config.Config{})
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}

	secret := []byte("test-secret-for-stream-start")
	wsHandler := NewWebSocketHandler(db, hub, sfu, nil, false, secret, nil)

	// Wire the RoomHandler exactly like router.go does for the stream-start hook.
	roomHandler := NewRoomHandler(db, &config.Config{}, secret)
	roomHandler.SetSFU(sfu)
	roomHandler.SetHub(hub)
	roomHandler.SetOnRoomLive(wsHandler.InitiateSubscriptionsForRoom)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws/room/{slug}", wsHandler.HandleConnection)
	server := httptest.NewServer(mux)

	// Seed a room bound to a stream key. The SFU deliberately has NO RoomTracks
	// for the slug yet — the ingest hasn't connected, which is the state that
	// produced "Failed to create subscriber connection ... room not found" in
	// production.
	slug := "prestream-room"
	streamKey := "stream-token-prestream"
	if _, err := db.Exec(`INSERT INTO stream_keys (id, name, key_token) VALUES ('sk-pre', 'Pre Key', ?)`, streamKey); err != nil {
		t.Fatalf("failed to insert stream key: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO rooms (id, slug, name, status, stream_key_id) VALUES ('room-pre', ?, 'Pre-Stream Room', ?, 'sk-pre')`, slug, roomStatus); err != nil {
		t.Fatalf("failed to insert room: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO participants (id, room_id, name, role, color, is_admitted) VALUES ('part-pre', 'room-pre', 'Early Viewer', 'viewer', '#e63946', 1)`); err != nil {
		t.Fatalf("failed to insert participant: %v", err)
	}

	tm := NewTokenManager(secret)
	name := "Early Viewer"
	token, err := tm.GenerateToken("part-pre", slug, name, time.Hour)
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	env := &streamStartTestEnv{
		t:           t,
		db:          db,
		hub:         hub,
		sfu:         sfu,
		roomHandler: roomHandler,
		server:      server,
		slug:        slug,
		token:       token,
		name:        name,
		streamKey:   streamKey,
	}
	cleanup := func() {
		server.Close()
		sfu.Shutdown()
		hub.Shutdown()
		dbCleanup()
	}
	return env, cleanup
}

func (env *streamStartTestEnv) dial() *gorillaws.Conn {
	env.t.Helper()
	url := "ws" + strings.TrimPrefix(env.server.URL, "http") +
		"/ws/room/" + env.slug + "?token=" + env.token + "&name=" + strings.ReplaceAll(env.name, " ", "%20")
	conn, _, err := gorillaws.DefaultDialer.Dial(url, nil)
	if err != nil {
		env.t.Fatalf("failed to dial websocket: %v", err)
	}
	return conn
}

func TestWebSocketHandler_InitiateSubscriptionCleansUpWhenOfferNotQueued(t *testing.T) {
	hub := websocket.NewHub()
	sfu, err := webrtc.NewSFU(&config.Config{})
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	handler := NewWebSocketHandler(nil, hub, sfu, nil, false, []byte("test-secret"), nil)
	roomSlug := "offer-send-failure-room"

	videoTrack, err := pionwebrtc.NewTrackLocalStaticRTP(
		pionwebrtc.RTPCodecCapability{MimeType: pionwebrtc.MimeTypeH264, ClockRate: 90000},
		"video",
		"stream",
	)
	if err != nil {
		t.Fatalf("failed to create video track: %v", err)
	}
	room := sfu.GetRoomTracks(roomSlug)
	room.VideoTrack = videoTrack

	client := &websocket.Client{
		ID:       "viewer-1",
		Name:     "Viewer",
		RoomSlug: roomSlug,
		Hub:      hub,
		Send:     make(chan []byte, 1),
		Done:     make(chan struct{}),
	}
	client.Send <- []byte(`{"type":"queued"}`)

	if handler.initiateSubscription(client, roomSlug, &subscriptionState{}) {
		t.Fatal("subscription should not be marked created when offer cannot be queued")
	}
	if sfu.HasSubscriber(roomSlug, client.ID) {
		t.Fatal("failed offer send left a phantom SFU subscriber")
	}
	select {
	case <-client.Done:
		// Expected: critical offer send failure closes the client for reconnect.
	case <-time.After(time.Second):
		t.Fatal("client was not closed after critical offer send failure")
	}
}

func TestWebSocketHandler_ResubscribeSendsIceServersBeforeOffer(t *testing.T) {
	hub := websocket.NewHub()
	sfu, err := webrtc.NewSFU(&config.Config{})
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	handler := NewWebSocketHandler(nil, hub, sfu, nil, false, []byte("test-secret"), nil)
	roomSlug := "resubscribe-order-room"

	videoTrack, err := pionwebrtc.NewTrackLocalStaticRTP(
		pionwebrtc.RTPCodecCapability{MimeType: pionwebrtc.MimeTypeH264, ClockRate: 90000},
		"video",
		"stream",
	)
	if err != nil {
		t.Fatalf("failed to create video track: %v", err)
	}
	room := sfu.GetRoomTracks(roomSlug)
	room.VideoTrack = videoTrack

	client := &websocket.Client{
		ID:       "viewer-1",
		Name:     "Viewer",
		RoomSlug: roomSlug,
		Hub:      hub,
		Send:     make(chan []byte, 8),
		Done:     make(chan struct{}),
	}
	handler.subStates[client] = &subscriptionState{}

	handler.handleResubscribe(client)

	first := decodeQueuedWSMessage(t, client)
	second := decodeQueuedWSMessage(t, client)
	if first.Type != "signal:ice-servers" || second.Type != "signal:offer" {
		t.Fatalf("expected resubscribe to queue ice servers before offer, got %s then %s", first.Type, second.Type)
	}
	if !sfu.HasSubscriber(roomSlug, client.ID) {
		t.Fatal("resubscribe did not create replacement subscriber")
	}
}

func decodeQueuedWSMessage(t *testing.T, client *websocket.Client) websocket.Message {
	t.Helper()
	select {
	case raw := <-client.Send:
		var msg websocket.Message
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatalf("invalid queued websocket message: %v", err)
		}
		return msg
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for queued websocket message")
		return websocket.Message{}
	}
}

// startIngest simulates OBS connecting via WHIP: it registers an ingest
// session with bound local tracks and fires the stream-start hook, mirroring
// what whip.go does when the publisher PC reaches Connected.
func (env *streamStartTestEnv) startIngest() {
	env.t.Helper()

	videoTrack, err := pionwebrtc.NewTrackLocalStaticRTP(
		pionwebrtc.RTPCodecCapability{
			MimeType:    pionwebrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
		}, "video", "chromatic-stream")
	if err != nil {
		env.t.Fatalf("failed to create video track: %v", err)
	}
	audioTrack, err := pionwebrtc.NewTrackLocalStaticRTP(
		pionwebrtc.RTPCodecCapability{
			MimeType:  pionwebrtc.MimeTypeOpus,
			ClockRate: 48000,
			Channels:  2,
		}, "audio", "chromatic-stream")
	if err != nil {
		env.t.Fatalf("failed to create audio track: %v", err)
	}

	env.sfu.SetIngest(env.streamKey, webrtc.NewIngestSession(env.streamKey, nil, videoTrack, audioTrack))
	if err := env.roomHandler.OnStreamStart(env.streamKey); err != nil {
		env.t.Fatalf("OnStreamStart failed: %v", err)
	}
}

// assertPreStreamOffer verifies the immediate pre-stream offer: subscribers
// are created on connect even before the ingest binds (the offer carries no
// media yet) so the voice relay has a pipe for lobby conversation. Replaces
// neither initiates the subscription handshake (signal:offer) nor surfaces a
// handshake failure (signal:error, routed to sim.errCh) — connecting before
// the stream starts is a normal pre-stream state, not an error.
func assertPreStreamOffer(t *testing.T, sim *browserSim, wait time.Duration) {
	t.Helper()
	select {
	case <-sim.offerReceived:
		// expected: the empty pre-stream offer arrived promptly
	case err := <-sim.errCh:
		t.Fatalf("signaling error before the pre-stream offer: %v", err)
	case <-time.After(wait):
		t.Fatal("client never received the pre-stream signal:offer")
	}
}

// TestPreStreamJoin_GetsOfferOnStreamStart reproduces the production bug
// sequence exactly:
//
//  1. The room is marked live in the DB (OBS reconnect window / server
//     restart) but the SFU has no RoomTracks for the slug.
//  2. A viewer connects — must receive room:state and an immediate
//     pre-stream signal:offer (no media sections yet; the subscriber
//     exists so lobby voice can flow before the picture).
//  3. The WHIP ingest starts (BindIngestToRoom + stream-start hook).
//  4. The ingest tracks attach to the existing subscriber and the
//     renegotiation must reach Connected WITHOUT the client reconnecting.
func TestPreStreamJoin_GetsOfferOnStreamStart(t *testing.T) {
	env, cleanup := newStreamStartTestEnv(t, "live")
	defer cleanup()

	conn := env.dial()

	// Pre-stream: room:state arrives normally.
	waitForMessages(t, conn, 5*time.Second, "room:state")
	conn.SetReadDeadline(time.Time{})

	// Hand the connection to the browser sim BEFORE the stream starts so it
	// services signaling the same way the real client would; the empty
	// pre-stream offer must arrive promptly.
	sim := newBrowserSim(t, conn)
	defer sim.close()
	assertPreStreamOffer(t, sim, 5*time.Second)

	// OBS connects: tracks attach to the existing subscriber and arrive as
	// a renegotiation the sim answers on the same connection.
	env.startIngest()

	if err := sim.pumpUntilConnected(90 * time.Second); err != nil {
		t.Fatalf("pre-stream joiner never reached Connected after stream start: %v", err)
	}

	if !subscriberExists(env.sfu, env.slug, "part-pre") {
		t.Fatal("no SFU subscriber registered for the pre-stream joiner")
	}
}

// TestPreStreamJoin_PendingRoom_GetsOfferOnStreamStart covers the same flow
// for a pending room going live for the first time (the room:live branch of
// OnStreamStart): the early joiner must get its offer through the shared
// ensure-subscription path without a duplicate subscriber handshake.
func TestPreStreamJoin_PendingRoom_GetsOfferOnStreamStart(t *testing.T) {
	env, cleanup := newStreamStartTestEnv(t, "pending")
	defer cleanup()

	conn := env.dial()
	msgs := waitForMessages(t, conn, 5*time.Second, "room:state")
	var state struct {
		IsLive bool `json:"isLive"`
	}
	if err := json.Unmarshal(msgs["room:state"], &state); err != nil {
		t.Fatalf("invalid room:state payload: %v", err)
	}
	if state.IsLive {
		t.Fatal("room:state should report isLive=false before the stream starts")
	}
	conn.SetReadDeadline(time.Time{})

	sim := newBrowserSim(t, conn)
	defer sim.close()
	assertPreStreamOffer(t, sim, 5*time.Second)

	env.startIngest()

	if err := sim.pumpUntilConnected(90 * time.Second); err != nil {
		t.Fatalf("pre-stream joiner never reached Connected after the room went live: %v", err)
	}

	if !subscriberExists(env.sfu, env.slug, "part-pre") {
		t.Fatal("no SFU subscriber registered for the pre-stream joiner")
	}

	var status string
	if err := env.db.QueryRow(`SELECT status FROM rooms WHERE slug = ?`, env.slug).Scan(&status); err != nil || status != "live" {
		t.Fatalf("room should be live after stream start (status=%q err=%v)", status, err)
	}
}
