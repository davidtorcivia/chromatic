package webrtc

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"chromatic/internal/config"
	"chromatic/internal/metrics"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/intervalpli"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

// SFU is the Selective Forwarding Unit that manages WebRTC connections
type SFU struct {
	mu     sync.RWMutex
	config *config.Config
	api    *webrtc.API

	turnMode         string
	externalTurnURLs []string
	externalTurnUser string
	externalTurnPass string
	cloudflareTURN   *cloudflareTURNProvider

	// Active ingest sessions keyed by stream key token
	ingests map[string]*IngestSession

	// Track subscribers per room
	rooms map[string]*RoomTracks

	// Shutdown flag
	shutdown bool
}

// MaxICECandidates is the maximum number of ICE candidates allowed per
// negotiation to prevent memory exhaustion from ICE candidate flooding
// attacks. The counter is reset whenever a new negotiation begins (fresh
// answer, client renegotiation offer, ICE restart) — a long session goes
// through many negotiations (TURN credential refreshes, voice/screen-share
// renegotiations, ICE restarts) and a never-resetting cap would eventually
// reject all candidates and make ICE restarts fail permanently.
const MaxICECandidates = 50

// iceGatherTimeout bounds how long we wait for ICE gathering to complete before
// returning the SDP with whatever candidates we already have. Host/srflx
// candidates gather quickly and are usually sufficient; relay candidates can
// trickle later. Without this cap a hung STUN/TURN server freezes the
// handshake indefinitely (and a WHIP response could blow past client/server
// write timeouts).
const iceGatherTimeout = 3 * time.Second

// waitForICEGather blocks until ICE gathering completes or the timeout fires.
// It logs (but does not fail) on timeout — the SDP typed so far is still
// shippable and we'd rather ship a slightly degraded candidate set than hang.
func waitForICEGather(pc *webrtc.PeerConnection) {
	done := webrtc.GatheringCompletePromise(pc)
	select {
	case <-done:
	case <-time.After(iceGatherTimeout):
		log.Printf("ICE gathering timed out after %s; proceeding with current candidates", iceGatherTimeout)
	}
}

// IngestSession represents an active OBS WHIP connection
type IngestSession struct {
	StreamKeyToken    string
	PeerConnection    *webrtc.PeerConnection
	VideoTrack        *webrtc.TrackLocalStaticRTP
	AudioTrack        *webrtc.TrackLocalStaticRTP
	done              chan struct{}
	closeOnce         sync.Once  // Ensures done channel is closed only once
	iceCandidateCount int        // Counter for ICE candidates received
	iceMu             sync.Mutex // Protects iceCandidateCount
	// everConnected records whether the PeerConnection ever reached Connected.
	// Used to avoid spurious stream-end notifications and negative metrics
	// for sessions that never connected.
	everConnected atomic.Bool
	// teardown is set by the WHIP handler and runs the session teardown
	// (metric decrement, ingest removal, stream-end notification) exactly
	// once across Failed/Closed state callbacks and DELETE requests.
	teardown func()
}

// NewIngestSession constructs an IngestSession with its internal lifecycle
// state initialized. Callers outside this package (e.g. test harnesses that
// simulate a WHIP publisher) must use this instead of a struct literal: the
// teardown paths unconditionally close the done channel, which must be non-nil.
func NewIngestSession(streamKeyToken string, pc *webrtc.PeerConnection, videoTrack, audioTrack *webrtc.TrackLocalStaticRTP) *IngestSession {
	return &IngestSession{
		StreamKeyToken: streamKeyToken,
		PeerConnection: pc,
		VideoTrack:     videoTrack,
		AudioTrack:     audioTrack,
		done:           make(chan struct{}),
	}
}

// RoomTracks holds the tracks being distributed to a room
type RoomTracks struct {
	mu                       sync.RWMutex
	RoomSlug                 string
	VideoTrack               *webrtc.TrackLocalStaticRTP
	AudioTrack               *webrtc.TrackLocalStaticRTP
	Subscribers              map[string]*Subscriber
	VoiceSessions            map[string]*VoiceSession               // Participant voice connections (also hosts the server-side mute gate)
	VoiceRemoteTracks        map[string]*webrtc.TrackRemote         // Active voice remote tracks keyed by participant ID
	VoiceLocalTracks         map[string]*webrtc.TrackLocalStaticRTP // Relay local tracks for voice fan-out
	IngestPC                 *webrtc.PeerConnection                 // Reference to ingest PC for PLI requests
	ScreenShareParticipantID string                                 // Who is currently sharing
	ScreenShareRemoteTrack   *webrtc.TrackRemote                    // Incoming screen track
	ScreenShareLocalTrack    *webrtc.TrackLocalStaticRTP            // Relay track for fan-out
	voiceRelayDone           map[string]chan struct{}               // Per-participant cancellation for voice relay goroutines
	screenShareDone          chan struct{}                          // Cancellation for the screen share relay goroutine
}

// Subscriber represents a client receiving the stream
type Subscriber struct {
	ID             string
	PeerConnection *webrtc.PeerConnection
	VideoSender    *webrtc.RTPSender
	AudioSender    *webrtc.RTPSender
	done           chan struct{}
	closeOnce      sync.Once  // Ensures done channel is closed only once
	SignalingMu    sync.Mutex // Serializes SDP operations to prevent concurrent signaling state changes
	// Trickle ICE: callback for sending server-side ICE candidates to the client.
	// Before the callback is set, candidates are buffered in pendingCandidates.
	OnICECandidate    func(c *webrtc.ICECandidateInit)
	pendingCandidates []*webrtc.ICECandidateInit
	candidateMu       sync.Mutex
	// Buffer client-to-server ICE candidates that arrive before SetRemoteDescription.
	// These are flushed when the first answer/remote description is applied.
	pendingRemoteCandidates []*webrtc.ICECandidateInit
	remoteDescSet           bool
	remoteCandidateMu       sync.Mutex
	// Rate limit client-to-server ICE candidates (matches ingest limit).
	// Scoped per negotiation: reset via resetICECandidateBudget whenever a
	// new SDP negotiation begins.
	iceCandidateCount int
	// Pending renegotiation state. Tracks that could not be offered right away
	// (another offer/answer exchange in flight, or signaling lock contention)
	// are never dropped: they are either attached with needsRenegotiation set,
	// or queued in pendingTracks, and flushed at the next point the signaling
	// state returns to stable (FlushPendingRenegotiation).
	// needsRenegotiation is guarded by SignalingMu; pendingTracks and
	// OnRenegotiationNeeded are guarded by pendingMu so they can be recorded
	// even while another goroutine holds SignalingMu.
	needsRenegotiation    bool
	pendingMu             sync.Mutex
	pendingTracks         []*webrtc.TrackLocalStaticRTP
	OnRenegotiationNeeded func(offerSDP string)
}

// pcHasTrack reports whether the peer connection already has a sender bound
// to this exact track (prevents double-attach when an add is retried/queued).
func pcHasTrack(pc *webrtc.PeerConnection, track webrtc.TrackLocal) bool {
	for _, sender := range pc.GetSenders() {
		if sender.Track() == track {
			return true
		}
	}
	return false
}

// resetICECandidateBudget resets the per-negotiation ICE candidate counter.
// Must be called whenever a new SDP negotiation begins (initial/renegotiation
// answer from the client, client-initiated offer, ICE restart) so that long
// sessions with periodic ICE restarts or TURN credential refreshes never
// exhaust the flooding cap and lose the ability to trickle candidates.
func (sub *Subscriber) resetICECandidateBudget() {
	sub.remoteCandidateMu.Lock()
	sub.iceCandidateCount = 0
	sub.remoteCandidateMu.Unlock()
}

// NewSFU creates a new SFU instance
func NewSFU(cfg *config.Config) (*SFU, error) {
	// Create a MediaEngine with specific codecs for our SFU.
	// OBS sends H.264 Baseline with packetization-mode=1. We MUST only register
	// H.264 with PM=1 to prevent browsers (Firefox) from negotiating PM=0 which
	// would cause a black screen since the RTP payload format wouldn't match.
	m := &webrtc.MediaEngine{}

	videoRTCPFeedback := []webrtc.RTCPFeedback{
		{Type: "goog-remb", Parameter: ""},
		{Type: "ccm", Parameter: "fir"},
		{Type: "nack", Parameter: ""},
		{Type: "nack", Parameter: "pli"},
	}

	// H.264 packetization-mode=1 ONLY — PM=0 causes Firefox black screen.
	// Register with explicit PayloadType values matching pion defaults so
	// SDP negotiation produces correct payload type mappings.
	h264Codecs := []struct {
		PayloadType webrtc.PayloadType
		Profile     string
	}{
		{102, "42001f"}, // Constrained Baseline
		{106, "42e01f"}, // Constrained Baseline (alt constraint flags)
		{127, "4d001f"}, // Main Profile (Chrome may prefer this)
	}
	for _, c := range h264Codecs {
		if err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     webrtc.MimeTypeH264,
				ClockRate:    90000,
				SDPFmtpLine:  fmt.Sprintf("level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=%s", c.Profile),
				RTCPFeedback: videoRTCPFeedback,
			},
			PayloadType: c.PayloadType,
		}, webrtc.RTPCodecTypeVideo); err != nil {
			return nil, fmt.Errorf("failed to register H.264 codec: %w", err)
		}
	}

	// VP8 for screen sharing (getDisplayMedia typically uses VP8/VP9)
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:     webrtc.MimeTypeVP8,
			ClockRate:    90000,
			RTCPFeedback: videoRTCPFeedback,
		},
		PayloadType: 96,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, fmt.Errorf("failed to register VP8 codec: %w", err)
	}

	// Opus audio (for both stream audio and voice chat)
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeOpus,
			ClockRate:   48000,
			Channels:    2,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("failed to register Opus codec: %w", err)
	}

	// Create interceptor registry
	i := &interceptor.Registry{}

	// Add PLI (Picture Loss Indication) generator for better video quality
	// Use 1-second interval to ensure frequent keyframes for late-joining subscribers
	intervalPliFactory, err := intervalpli.NewReceiverInterceptor(
		intervalpli.GeneratorInterval(time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create PLI interceptor: %w", err)
	}
	i.Add(intervalPliFactory)

	// Use default interceptors for RTCP
	if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
		return nil, fmt.Errorf("failed to register default interceptors: %w", err)
	}

	// Create API with our MediaEngine and Interceptors
	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(m),
		webrtc.WithInterceptorRegistry(i),
	)

	sfu := &SFU{
		config:           cfg,
		api:              api,
		turnMode:         cfg.TurnMode,
		externalTurnURLs: append([]string(nil), cfg.TurnExternalURLs...),
		externalTurnUser: cfg.TurnExternalUser,
		externalTurnPass: cfg.TurnExternalPass,
		ingests:          make(map[string]*IngestSession),
		rooms:            make(map[string]*RoomTracks),
	}

	// Backward compatibility for callers that set the legacy single URL field.
	if len(sfu.externalTurnURLs) == 0 {
		sfu.externalTurnURLs = splitAndSanitizeTURNURLs(cfg.TurnExternalURL)
	}

	if cfg.HasCloudflareTURN() {
		sfu.cloudflareTURN = newCloudflareTURNProvider(cloudflareTURNProviderConfig{
			KeyID:      cfg.TurnCloudflareKeyID,
			APIToken:   cfg.TurnCloudflareAPIToken,
			TTLSeconds: cfg.TurnCloudflareCredentialTTL,
			Skew:       time.Duration(cfg.TurnCloudflareCredentialSkew) * time.Second,
		})
	}

	log.Println("SFU initialized with H.264 and Opus codecs")

	return sfu, nil
}

// GetICEServers returns the ICE server configuration for clients
func (s *SFU) GetICEServers() []webrtc.ICEServer {
	servers := []webrtc.ICEServer{
		{
			URLs: []string{"stun:stun.l.google.com:19302"},
		},
	}

	s.mu.RLock()
	turnMode := s.turnMode
	externalTurnURLs := append([]string(nil), s.externalTurnURLs...)
	externalTurnUser := s.externalTurnUser
	externalTurnPass := s.externalTurnPass
	cloudflareProvider := s.cloudflareTURN
	s.mu.RUnlock()

	includeSelfHosted := turnMode != config.TurnModeExternal
	includeExternal := turnMode != config.TurnModeSelfHosted

	// Add built-in/self-hosted TURN if enabled.
	if includeSelfHosted && s.config.TurnRealm != "" && s.config.TurnSecret != "" {
		// Generate time-limited TURN credentials
		username, credential := generateTURNCredentials(s.config.TurnSecret, s.config.TurnRealm)
		servers = append(servers, webrtc.ICEServer{
			URLs:       []string{fmt.Sprintf("turn:%s:3478", s.config.TurnRealm)},
			Username:   username,
			Credential: credential,
		})
	}

	// Add Cloudflare-managed TURN if enabled (credentials are generated/cached server-side).
	if includeExternal && cloudflareProvider != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		server, err := cloudflareProvider.GetICEServer(ctx, externalTurnURLs)
		if err != nil {
			log.Printf("Failed to fetch Cloudflare TURN credentials: %v", err)
		} else if len(server.URLs) > 0 && server.Username != "" && server.Credential != "" {
			servers = append(servers, server)
		}
	}

	// Add static external TURN server if configured.
	if includeExternal && len(externalTurnURLs) > 0 && externalTurnUser != "" && externalTurnPass != "" {
		servers = append(servers, webrtc.ICEServer{
			URLs:       externalTurnURLs,
			Username:   externalTurnUser,
			Credential: externalTurnPass,
		})
	}

	return servers
}

// SetExternalTURNConfig updates external TURN settings at runtime.
// This enables admin/setup changes to apply without restarting the process.
func (s *SFU) SetExternalTURNConfig(urlCSV, username, credential string) {
	urls := splitAndSanitizeTURNURLs(urlCSV)

	s.mu.Lock()
	s.externalTurnURLs = urls
	s.externalTurnUser = strings.TrimSpace(username)
	s.externalTurnPass = strings.TrimSpace(credential)
	s.mu.Unlock()
}

func splitAndSanitizeTURNURLs(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		value := strings.TrimSpace(p)
		if value == "" {
			continue
		}
		result = append(result, value)
	}

	return result
}

// CreatePeerConnection creates a new peer connection with standard configuration
func (s *SFU) CreatePeerConnection() (*webrtc.PeerConnection, error) {
	config := webrtc.Configuration{
		ICEServers: s.GetICEServers(),
	}

	return s.api.NewPeerConnection(config)
}

// GetIngest returns an active ingest session by stream key token
func (s *SFU) GetIngest(streamKeyToken string) *IngestSession {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ingests[streamKeyToken]
}

// SetIngest registers an ingest session. If an ingest with the same token
// already exists (OBS reconnect before the old PC's teardown callback fired),
// the old session is torn down first so its PC doesn't hold resources and so
// its stale OnConnectionStateChange callback can't remove the replacement.
// The old PC is closed outside s.mu — pion state callbacks re-enter SFU locks.
func (s *SFU) SetIngest(streamKeyToken string, session *IngestSession) {
	s.mu.Lock()
	prev, replaced := s.ingests[streamKeyToken]
	s.ingests[streamKeyToken] = session
	s.mu.Unlock()

	if replaced && prev != nil {
		log.Printf("Replacing existing ingest for key %s... (OBS reconnect)", streamKeyToken[:min(8, len(streamKeyToken))])
		prev.closeOnce.Do(func() { close(prev.done) })
		if prev.PeerConnection != nil {
			_ = prev.PeerConnection.Close()
		}
	}
}

// RemoveIngest removes an ingest session unconditionally (used for explicit
// teardown paths).
func (s *SFU) RemoveIngest(streamKeyToken string) {
	s.removeIngestIf(streamKeyToken, nil)
}

// removeIngestIfSame removes the ingest only when the current map entry
// matches the given expected session. Prevents a stale Failed/Closed callback
// from a replaced ingest from killing the new one.
func (s *SFU) removeIngestIfSame(streamKeyToken string, expected *IngestSession) {
	s.removeIngestIf(streamKeyToken, expected)
}

func (s *SFU) removeIngestIf(streamKeyToken string, expected *IngestSession) {
	s.mu.Lock()
	session, ok := s.ingests[streamKeyToken]
	if !ok {
		s.mu.Unlock()
		return
	}
	if expected != nil && session != expected {
		s.mu.Unlock()
		return
	}
	delete(s.ingests, streamKeyToken)
	s.mu.Unlock()

	// Close outside the lock — pion state callbacks re-enter SFU locks.
	session.closeOnce.Do(func() {
		close(session.done)
	})
	if session.PeerConnection != nil {
		_ = session.PeerConnection.Close()
	}
}

// GetRoomTracks gets or creates room tracks for a slug
func (s *SFU) GetRoomTracks(roomSlug string) *RoomTracks {
	s.mu.Lock()
	defer s.mu.Unlock()

	if room, ok := s.rooms[roomSlug]; ok {
		return room
	}

	room := &RoomTracks{
		RoomSlug:    roomSlug,
		Subscribers: make(map[string]*Subscriber),
	}
	s.rooms[roomSlug] = room
	return room
}

// Shutdown gracefully shuts down the SFU.
// PeerConnections are closed AFTER all locks are released: pion fires
// OnConnectionStateChange callbacks that call back into the SFU/room locks
// (e.g. RemoveSubscriberIfMatch), so closing under a lock risks deadlock.
func (s *SFU) Shutdown() {
	var pcs []*webrtc.PeerConnection

	s.mu.Lock()
	s.shutdown = true

	// Detach all ingest sessions and collect their PCs for closing later.
	for _, session := range s.ingests {
		session.closeOnce.Do(func() {
			close(session.done)
		})
		if session.PeerConnection != nil {
			pcs = append(pcs, session.PeerConnection)
		}
	}
	s.ingests = make(map[string]*IngestSession)

	// Detach all rooms and clear the map.
	rooms := make([]*RoomTracks, 0, len(s.rooms))
	for _, room := range s.rooms {
		rooms = append(rooms, room)
	}
	s.rooms = make(map[string]*RoomTracks)
	s.mu.Unlock()

	// Remove subscribers and stop relay goroutines under each room lock,
	// collecting the PCs to close afterwards.
	for _, room := range rooms {
		room.mu.Lock()
		for _, sub := range room.Subscribers {
			sub.closeOnce.Do(func() {
				close(sub.done)
			})
			if sub.PeerConnection != nil {
				pcs = append(pcs, sub.PeerConnection)
			}
		}
		room.Subscribers = make(map[string]*Subscriber)
		for _, vs := range room.VoiceSessions {
			vs.closeOnce.Do(func() {
				close(vs.done)
			})
			if vs.PeerConnection != nil {
				pcs = append(pcs, vs.PeerConnection)
			}
		}
		room.VoiceSessions = nil
		for id, ch := range room.voiceRelayDone {
			close(ch)
			delete(room.voiceRelayDone, id)
		}
		if room.screenShareDone != nil {
			close(room.screenShareDone)
			room.screenShareDone = nil
		}
		room.mu.Unlock()
	}

	// Close all PeerConnections with no locks held.
	for _, pc := range pcs {
		pc.Close()
	}

	log.Println("SFU shutdown complete")
}

// AddSubscriber adds a subscriber to a room. If a subscriber with the same
// ID already exists (e.g. page refresh), the old one is closed first so its
// OnConnectionStateChange callback won't remove the new subscriber.
func (rt *RoomTracks) AddSubscriber(sub *Subscriber) {
	var oldPC *webrtc.PeerConnection

	rt.mu.Lock()
	if old, exists := rt.Subscribers[sub.ID]; exists {
		log.Printf("Replacing existing subscriber %s (reconnect)", sub.ID)
		old.closeOnce.Do(func() {
			close(old.done)
		})
		oldPC = old.PeerConnection
		old.candidateMu.Lock()
		old.OnICECandidate = nil
		old.pendingCandidates = nil
		old.candidateMu.Unlock()
		// Don't decrement ActiveSubscribers — we're replacing, not removing
	} else {
		metrics.Get().ActiveSubscribers.Add(1)
	}
	rt.Subscribers[sub.ID] = sub
	rt.mu.Unlock()

	// Close outside the lock — pion state callbacks re-enter rt.mu.
	if oldPC != nil {
		oldPC.Close()
	}
}

// removeSubscriberIfSame removes a subscriber only if the current subscriber
// in the map is the same instance as `expected`. This prevents a stale
// OnConnectionStateChange callback from a replaced (rejoin) subscriber from
// tearing down the live replacement — which is exactly what caused viewers
// to hang indefinitely on reconnect.
//
// The pointer check and removal happen under a single write lock to prevent
// a TOCTOU race where AddSubscriber replaces the subscriber between the check
// and the removal.
func (rt *RoomTracks) removeSubscriberIfSame(id string, expected *Subscriber) {
	rt.mu.Lock()
	current, ok := rt.Subscribers[id]
	if !ok || current != expected {
		rt.mu.Unlock()
		return
	}
	pcs := rt.removeSubscriberLocked(id)
	rt.mu.Unlock()

	// Close outside the lock — pion state callbacks re-enter rt.mu.
	for _, pc := range pcs {
		pc.Close()
	}
}

// RemoveSubscriber removes a subscriber from a room unconditionally.
func (rt *RoomTracks) RemoveSubscriber(id string) {
	rt.mu.Lock()
	pcs := rt.removeSubscriberLocked(id)
	rt.mu.Unlock()

	// Close outside the lock — pion state callbacks re-enter rt.mu.
	for _, pc := range pcs {
		pc.Close()
	}
}

// removeSubscriberLocked performs subscriber removal. Caller must hold rt.mu
// write lock and is responsible for closing the returned PeerConnections AFTER
// releasing the lock (pion fires OnConnectionStateChange callbacks during
// Close that call back into rt.mu, e.g. via removeSubscriberIfSame).
func (rt *RoomTracks) removeSubscriberLocked(id string) []*webrtc.PeerConnection {
	var pcs []*webrtc.PeerConnection
	if sub, ok := rt.Subscribers[id]; ok {
		// Use sync.Once to prevent double-close panic
		sub.closeOnce.Do(func() {
			close(sub.done)
		})
		// Hand the PeerConnection back to the caller for closing.
		if sub.PeerConnection != nil {
			pcs = append(pcs, sub.PeerConnection)
		}
		// Clear ICE callback to release references to the client
		sub.candidateMu.Lock()
		sub.OnICECandidate = nil
		sub.pendingCandidates = nil
		sub.candidateMu.Unlock()
		sub.pendingMu.Lock()
		sub.OnRenegotiationNeeded = nil
		sub.pendingTracks = nil
		sub.pendingMu.Unlock()
		delete(rt.Subscribers, id)
		// Track subscriber removal
		metrics.Get().ActiveSubscribers.Add(-1)
	}
	// Clean up any voice state owned by this participant and stop its relay.
	if vs, ok := rt.VoiceSessions[id]; ok {
		vs.closeOnce.Do(func() {
			close(vs.done)
		})
		if vs.PeerConnection != nil {
			pcs = append(pcs, vs.PeerConnection)
		}
		delete(rt.VoiceSessions, id)
	}
	delete(rt.VoiceRemoteTracks, id)
	delete(rt.VoiceLocalTracks, id)
	if ch, ok := rt.voiceRelayDone[id]; ok {
		close(ch)
		delete(rt.voiceRelayDone, id)
	}
	// Clean up screen share state if this participant was sharing
	if rt.ScreenShareParticipantID == id {
		rt.ScreenShareParticipantID = ""
		rt.ScreenShareRemoteTrack = nil
		rt.ScreenShareLocalTrack = nil
		if rt.screenShareDone != nil {
			close(rt.screenShareDone)
			rt.screenShareDone = nil
		}
	}
	return pcs
}

// BindIngestToRoom binds an ingest session's tracks to a room for distribution.
// Returns a list of subscriber IDs that need renegotiation (new tracks were added, not just replaced).
func (s *SFU) BindIngestToRoom(streamKeyToken, roomSlug string) ([]string, error) {
	// Under s.mu: only look up/create the ingest and room entries.
	s.mu.Lock()
	ingest, ok := s.ingests[streamKeyToken]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("ingest session not found for token")
	}

	room, ok := s.rooms[roomSlug]
	if !ok {
		room = &RoomTracks{
			RoomSlug:    roomSlug,
			Subscribers: make(map[string]*Subscriber),
		}
		s.rooms[roomSlug] = room
	}
	s.mu.Unlock()

	// Under a short room.mu lock: update room fields and snapshot the
	// subscriber list. Per-subscriber work happens after releasing room.mu
	// so we never hold s.mu/room.mu while taking a subscriber's SignalingMu
	// (every other path takes SignalingMu alone — inconsistent ordering
	// would risk deadlock).
	room.mu.Lock()
	room.VideoTrack = ingest.VideoTrack
	room.AudioTrack = ingest.AudioTrack
	room.IngestPC = ingest.PeerConnection
	subs := make([]*Subscriber, 0, len(room.Subscribers))
	for _, sub := range room.Subscribers {
		subs = append(subs, sub)
	}
	room.mu.Unlock()

	var needsRenegotiation []string

	for _, sub := range subs {
		subNeedsReneg := false

		// AddTrack modifies the PC's transceiver list and VideoSender /
		// AudioSender are read+written here — hold SignalingMu to prevent
		// racing with concurrent SDP operations.
		sub.SignalingMu.Lock()

		// Video track
		if sub.VideoSender != nil && ingest.VideoTrack != nil {
			if err := sub.VideoSender.ReplaceTrack(ingest.VideoTrack); err != nil {
				log.Printf("Failed to replace video track for subscriber %s: %v", sub.ID, err)
			}
		} else if sub.VideoSender == nil && ingest.VideoTrack != nil {
			sender, err := sub.PeerConnection.AddTrack(ingest.VideoTrack)
			if err != nil {
				log.Printf("Failed to add video track to subscriber %s: %v", sub.ID, err)
			} else {
				sub.VideoSender = sender
				subNeedsReneg = true
			}
		}

		// Audio track
		if sub.AudioSender != nil && ingest.AudioTrack != nil {
			if err := sub.AudioSender.ReplaceTrack(ingest.AudioTrack); err != nil {
				log.Printf("Failed to replace audio track for subscriber %s: %v", sub.ID, err)
			}
		} else if sub.AudioSender == nil && ingest.AudioTrack != nil {
			sender, err := sub.PeerConnection.AddTrack(ingest.AudioTrack)
			if err != nil {
				log.Printf("Failed to add audio track to subscriber %s: %v", sub.ID, err)
			} else {
				sub.AudioSender = sender
				subNeedsReneg = true
			}
		}

		sub.SignalingMu.Unlock()

		if subNeedsReneg {
			needsRenegotiation = append(needsRenegotiation, sub.ID)
		}
	}

	log.Printf("Bound ingest %s... to room %s", streamKeyToken[:8], roomSlug)
	return needsRenegotiation, nil
}

// RequestKeyframe sends a PLI to the ingest to force a keyframe for all subscribers.
// Called when new subscribers join or when a client requests a stream resync.
func (s *SFU) RequestKeyframe(roomSlug string) {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return
	}

	room.mu.RLock()
	ingestPC := room.IngestPC
	room.mu.RUnlock()

	if ingestPC == nil {
		return
	}

	// Find video receivers and send PLI for each
	for _, receiver := range ingestPC.GetReceivers() {
		track := receiver.Track()
		if track != nil && track.Kind() == webrtc.RTPCodecTypeVideo {
			if err := ingestPC.WriteRTCP([]rtcp.Packet{
				&rtcp.PictureLossIndication{
					MediaSSRC: uint32(track.SSRC()),
				},
			}); err != nil {
				log.Printf("Failed to send PLI for room %s: %v", roomSlug, err)
			} else {
				log.Printf("Sent PLI keyframe request for room %s", roomSlug)
			}
			break
		}
	}
}

// RequestScreenShareKeyframe sends a PLI to the screen-sharing participant's
// PeerConnection to force a keyframe for the screen share track. This ensures
// subscribers can start decoding the screen share immediately rather than
// waiting for the next natural keyframe.
func (s *SFU) RequestScreenShareKeyframe(roomSlug string) {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return
	}

	room.mu.RLock()
	sharerID := room.ScreenShareParticipantID
	sub, ok := room.Subscribers[sharerID]
	room.mu.RUnlock()

	if sharerID == "" || !ok || sub.PeerConnection == nil {
		return
	}

	// Find the video receiver on the sharer's PC (the screen share track)
	for _, receiver := range sub.PeerConnection.GetReceivers() {
		track := receiver.Track()
		if track != nil && track.Kind() == webrtc.RTPCodecTypeVideo {
			if err := sub.PeerConnection.WriteRTCP([]rtcp.Packet{
				&rtcp.PictureLossIndication{
					MediaSSRC: uint32(track.SSRC()),
				},
			}); err != nil {
				log.Printf("Failed to send PLI for screen share in room %s: %v", roomSlug, err)
			} else {
				log.Printf("Sent PLI for screen share keyframe in room %s", roomSlug)
			}
			break
		}
	}
}

// GetRoomTracksForSlug returns the room tracks if they exist
func (s *SFU) GetRoomTracksForSlug(roomSlug string) *RoomTracks {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rooms[roomSlug]
}

// HasSubscriber reports whether a subscriber with the given ID is currently
// registered for the room. Used by the stream-start path to skip clients that
// already completed (or are completing) the connect-time subscription flow.
func (s *SFU) HasSubscriber(roomSlug, subscriberID string) bool {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return false
	}
	room.mu.RLock()
	defer room.mu.RUnlock()
	_, ok := room.Subscribers[subscriberID]
	return ok
}

// CreateSubscriberConnection creates a new subscriber peer connection for a room.
// Returns the peer connection and an SDP offer string to send to the client.
// ICE candidates are NOT included in the offer — they are trickled via
// EnableSubscriberTrickleICE after the offer has been sent to the client.
func (s *SFU) CreateSubscriberConnection(roomSlug, subscriberID string) (*webrtc.PeerConnection, string, error) {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return nil, "", fmt.Errorf("room not found: %s", roomSlug)
	}

	// Create the peer connection outside the lock — NewPeerConnection is a
	// non-trivial construction and we don't want to block BindIngestToRoom or
	// other subscribers while it runs.
	pc, err := s.CreatePeerConnection()
	if err != nil {
		return nil, "", fmt.Errorf("failed to create peer connection: %w", err)
	}

	// Create subscriber record
	sub := &Subscriber{
		ID:             subscriberID,
		PeerConnection: pc,
		done:           make(chan struct{}),
	}

	// Set up trickle ICE: buffer candidates until EnableSubscriberTrickleICE is called.
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		sub.candidateMu.Lock()
		if sub.OnICECandidate != nil {
			cb := sub.OnICECandidate
			sub.candidateMu.Unlock()
			cb(&init)
		} else {
			sub.pendingCandidates = append(sub.pendingCandidates, &init)
			sub.candidateMu.Unlock()
		}
	})

	// Hold the (uncontended) signaling mutex from before the subscriber
	// becomes visible in the map until the initial offer's local description
	// is set. Without this, a concurrent voice/screen-share fan-out can find
	// the subscriber, pass its own stable-state check, and race our
	// CreateOffer/SetLocalDescription — pion then rejects one side with
	// "invalid proposed signaling state transition" and that track is lost
	// for this subscriber. Lock order (SignalingMu → room.mu) is safe: no
	// path acquires SignalingMu while holding room.mu.
	sub.SignalingMu.Lock()
	defer sub.SignalingMu.Unlock()

	// Atomic bind+insert: track reads, AddTrack calls and map insertion happen
	// under a single critical section so BindIngestToRoom can't rewrite
	// room.VideoTrack/AudioTrack between us reading the current tracks and our
	// AddTrack calls — otherwise the new subscriber could end up bound to a
	// just-replaced (dormant) ingest track and stay frozen until the next OBS
	// reconnect. Any BindIngestToRoom that runs after us will find the
	// subscriber in the map and rebind via ReplaceTrack; any that ran before us
	// already committed the tracks we read here.
	room.mu.Lock()

	// Add video track if available
	if room.VideoTrack != nil {
		sender, addErr := pc.AddTrack(room.VideoTrack)
		if addErr != nil {
			room.mu.Unlock()
			pc.Close()
			return nil, "", fmt.Errorf("failed to add video track: %w", addErr)
		}
		sub.VideoSender = sender
		log.Printf("Added video track to subscriber %s", subscriberID)
	}

	// Add audio track if available
	if room.AudioTrack != nil {
		sender, addErr := pc.AddTrack(room.AudioTrack)
		if addErr != nil {
			room.mu.Unlock()
			pc.Close()
			return nil, "", fmt.Errorf("failed to add audio track: %w", addErr)
		}
		sub.AudioSender = sender
		log.Printf("Added audio track to subscriber %s", subscriberID)
	}

	// Add existing voice relay tracks so they are included in the initial
	// offer. This avoids a separate renegotiation that would race with the
	// initial offer-answer exchange and corrupt the signaling state.
	for pid, voiceTrack := range room.VoiceLocalTracks {
		if pid == subscriberID {
			continue
		}
		if _, addErr := pc.AddTrack(voiceTrack); addErr != nil {
			log.Printf("Failed to add voice relay track for %s to subscriber %s: %v", pid, subscriberID, addErr)
		} else {
			log.Printf("Added existing voice relay track to subscriber %s", subscriberID)
		}
	}

	// Add active screen share track so late joiners see it immediately
	screenShareActive := false
	if room.ScreenShareParticipantID != "" && room.ScreenShareParticipantID != subscriberID && room.ScreenShareLocalTrack != nil {
		if _, addErr := pc.AddTrack(room.ScreenShareLocalTrack); addErr != nil {
			log.Printf("Failed to add screen share track to subscriber %s: %v", subscriberID, addErr)
		} else {
			screenShareActive = true
			log.Printf("Added existing screen share track to subscriber %s", subscriberID)
		}
	}

	// Displace any prior subscriber holding this ID (rejoin). Same semantics
	// as AddSubscriber, inlined so the insert is part of the critical section.
	var oldPC *webrtc.PeerConnection
	if old, exists := room.Subscribers[subscriberID]; exists {
		log.Printf("Replacing existing subscriber %s (rejoin); tearing down prior PC", subscriberID)
		old.closeOnce.Do(func() {
			close(old.done)
		})
		oldPC = old.PeerConnection
		old.candidateMu.Lock()
		old.OnICECandidate = nil
		old.pendingCandidates = nil
		old.candidateMu.Unlock()
		// Metric unchanged — replacement cancels the old one out.
	} else {
		metrics.Get().ActiveSubscribers.Add(1)
	}
	room.Subscribers[subscriberID] = sub
	room.mu.Unlock()

	// Close the displaced PC outside the lock — pion state callbacks re-enter rt.mu.
	if oldPC != nil {
		oldPC.Close()
	}

	// Request a keyframe from the ingest so the new subscriber can decode immediately
	go s.RequestKeyframe(roomSlug)
	// Likewise for an in-progress screen share: a viewer attaching to the
	// relay mid-stream sits on black until the sharer produces a keyframe,
	// so nudge the sharer with a PLI right away (the interval PLI
	// interceptor provides the ongoing cadence).
	if screenShareActive {
		go s.RequestScreenShareKeyframe(roomSlug)
	}

	// Handle connection state — only remove on terminal states.
	// "disconnected" is transient and can recover via ICE restart from the client.
	// Capture `sub` so we only remove if this specific subscriber is still current
	// (prevents a stale callback from removing a replacement subscriber on reconnect).
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("Subscriber %s connection state: %s", subscriberID, state)
		switch state {
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed:
			room.removeSubscriberIfSame(subscriberID, sub)
		}
	})

	// Create offer. Error paths use identity-checked removal so a concurrent
	// rejoin that already replaced us isn't torn down by our cleanup; pc.Close
	// is idempotent and covers the case where we were already replaced.
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		room.removeSubscriberIfSame(subscriberID, sub)
		pc.Close()
		return nil, "", fmt.Errorf("failed to create offer: %w", err)
	}

	// Set local description — starts ICE gathering in the background.
	// Candidates are buffered and sent via trickle ICE (no blocking wait).
	if err := pc.SetLocalDescription(offer); err != nil {
		room.removeSubscriberIfSame(subscriberID, sub)
		pc.Close()
		return nil, "", fmt.Errorf("failed to set local description: %w", err)
	}

	// Diagnostic: read the viewer's RTCP for the video stream. Receiver
	// reports with an advancing highest-sequence prove packets arrive (a
	// black screen is then a decode problem); absent/stuck reports mean the
	// transport never delivers video at all.
	if sub.VideoSender != nil {
		go logSubscriberVideoRTCP(sub, subscriberID)
	}

	return pc, offer.SDP, nil
}

// logSubscriberVideoRTCP logs a bounded number of RTCP receiver reports and
// PLI requests from a subscriber's video stream, then exits. Purely
// diagnostic — used to tell "not receiving video" apart from "receiving but
// not decoding" (e.g. Chrome-only black screens).
func logSubscriberVideoRTCP(sub *Subscriber, subscriberID string) {
	const maxReports = 8
	logged := 0
	pliCount := 0
	start := time.Now()
	for logged < maxReports && time.Since(start) < 2*time.Minute {
		select {
		case <-sub.done:
			return
		default:
		}
		pkts, _, err := sub.VideoSender.ReadRTCP()
		if err != nil {
			return
		}
		for _, p := range pkts {
			switch r := p.(type) {
			case *rtcp.ReceiverReport:
				for _, rep := range r.Reports {
					log.Printf("Subscriber %s video RR: highestSeq=%d totalLost=%d fractionLost=%d jitter=%d (PLIs so far: %d)",
						subscriberID, rep.LastSequenceNumber, rep.TotalLost, rep.FractionLost, rep.Jitter, pliCount)
					logged++
				}
			case *rtcp.PictureLossIndication:
				pliCount++
				if pliCount <= 3 || pliCount%20 == 0 {
					log.Printf("Subscriber %s sent video PLI #%d (decoder waiting on a usable keyframe)", subscriberID, pliCount)
				}
			}
		}
	}
}

// EnableSubscriberTrickleICE sets the ICE candidate callback for a subscriber
// and flushes any candidates that were buffered before the offer was sent.
// Must be called AFTER the offer SDP has been sent to the client to ensure
// correct message ordering (offer arrives before candidates).
func (s *SFU) EnableSubscriberTrickleICE(roomSlug, subscriberID string, onICECandidate func(*webrtc.ICECandidateInit)) {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return
	}

	room.mu.RLock()
	sub, ok := room.Subscribers[subscriberID]
	room.mu.RUnlock()
	if !ok {
		return
	}

	sub.candidateMu.Lock()
	sub.OnICECandidate = onICECandidate
	pending := sub.pendingCandidates
	sub.pendingCandidates = nil
	sub.candidateMu.Unlock()

	for _, c := range pending {
		onICECandidate(c)
	}
}

// SetSubscriberAnswer sets the answer from a subscriber client
func (s *SFU) SetSubscriberAnswer(roomSlug, subscriberID string, answer webrtc.SessionDescription) error {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return fmt.Errorf("room not found: %s", roomSlug)
	}

	room.mu.RLock()
	sub, ok := room.Subscribers[subscriberID]
	room.mu.RUnlock()

	if !ok {
		return fmt.Errorf("subscriber not found: %s", subscriberID)
	}

	sub.SignalingMu.Lock()
	defer sub.SignalingMu.Unlock()

	if err := sub.PeerConnection.SetRemoteDescription(answer); err != nil {
		return fmt.Errorf("failed to set remote description: %w", err)
	}

	// Flush any buffered client-to-server ICE candidates and start a fresh
	// per-negotiation candidate budget for the trickle that follows.
	sub.remoteCandidateMu.Lock()
	sub.remoteDescSet = true
	sub.iceCandidateCount = 0
	pending := sub.pendingRemoteCandidates
	sub.pendingRemoteCandidates = nil
	sub.remoteCandidateMu.Unlock()

	for _, c := range pending {
		if err := sub.PeerConnection.AddICECandidate(*c); err != nil {
			log.Printf("Failed to add buffered ICE candidate for %s: %v", subscriberID, err)
		}
	}

	// Diagnostic: a subscriber whose answer rejected the ingest's H.264
	// profile gets silent black video (audio fine) — make that visible.
	if sub.VideoSender != nil {
		params := sub.VideoSender.GetParameters()
		if len(params.Codecs) == 0 {
			log.Printf("WARNING: subscriber %s negotiated NO video codec — video will be black", subscriberID)
		} else {
			log.Printf("Subscriber %s outbound video codec: %s %s", subscriberID, params.Codecs[0].MimeType, params.Codecs[0].SDPFmtpLine)
		}
	}

	log.Printf("Set answer for subscriber %s (flushed %d buffered candidates)", subscriberID, len(pending))
	return nil
}

// AddSubscriberICECandidate adds an ICE candidate from a subscriber.
// If the remote description hasn't been set yet, the candidate is buffered
// and will be applied when SetSubscriberAnswer is called.
func (s *SFU) AddSubscriberICECandidate(roomSlug, subscriberID string, candidate webrtc.ICECandidateInit) error {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return fmt.Errorf("room not found: %s", roomSlug)
	}

	room.mu.RLock()
	sub, ok := room.Subscribers[subscriberID]
	room.mu.RUnlock()

	if !ok {
		return fmt.Errorf("subscriber not found: %s", subscriberID)
	}

	// Enforce ICE candidate limit to prevent flooding attacks
	sub.remoteCandidateMu.Lock()
	sub.iceCandidateCount++
	if sub.iceCandidateCount > MaxICECandidates {
		sub.remoteCandidateMu.Unlock()
		return fmt.Errorf("too many ICE candidates from subscriber %s", subscriberID)
	}
	if !sub.remoteDescSet {
		// Buffer until remote description is set
		sub.pendingRemoteCandidates = append(sub.pendingRemoteCandidates, &candidate)
		sub.remoteCandidateMu.Unlock()
		return nil
	}
	sub.remoteCandidateMu.Unlock()

	if err := sub.PeerConnection.AddICECandidate(candidate); err != nil {
		return fmt.Errorf("failed to add ICE candidate: %w", err)
	}

	return nil
}

// HandleIceRestart handles an ICE restart request from a subscriber
func (s *SFU) HandleIceRestart(roomSlug, subscriberID, sdpOffer string) (string, error) {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return "", fmt.Errorf("room not found: %s", roomSlug)
	}

	room.mu.RLock()
	sub, ok := room.Subscribers[subscriberID]
	room.mu.RUnlock()

	if !ok {
		return "", fmt.Errorf("subscriber not found: %s", subscriberID)
	}

	sub.SignalingMu.Lock()
	defer sub.SignalingMu.Unlock()

	// If the server is still waiting on an answer for a prior renegotiation
	// offer, Pion refuses SetRemoteDescription(offer) with an "offer collision"
	// error and the ICE restart is silently lost. Roll the server's offer back
	// so we can accept the client's restart — any tracks from that offer are
	// still attached to the PC and will re-emerge in a follow-up renegotiation.
	if sub.PeerConnection.SignalingState() == webrtc.SignalingStateHaveLocalOffer {
		log.Printf("ICE restart arrived during pending server offer; rolling back for subscriber %s", subscriberID)
		if err := sub.PeerConnection.SetLocalDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeRollback}); err != nil {
			return "", fmt.Errorf("failed to rollback server offer for ICE restart: %w", err)
		}
		// Tracks from the rolled-back offer are still attached but were never
		// negotiated — make sure a follow-up offer actually happens.
		sub.needsRenegotiation = true
	}

	// Set the new offer from the client
	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdpOffer,
	}

	if err := sub.PeerConnection.SetRemoteDescription(offer); err != nil {
		return "", fmt.Errorf("failed to set remote description: %w", err)
	}

	// An ICE restart starts a brand-new candidate exchange (fresh ufrag/pwd),
	// so the per-negotiation candidate budget starts over too. Without this,
	// repeated restarts on a long session exhaust the cap and every later
	// restart fails with "too many ICE candidates".
	sub.resetICECandidateBudget()

	// Create answer
	answer, err := sub.PeerConnection.CreateAnswer(nil)
	if err != nil {
		return "", fmt.Errorf("failed to create answer: %w", err)
	}

	if err := sub.PeerConnection.SetLocalDescription(answer); err != nil {
		return "", fmt.Errorf("failed to set local description: %w", err)
	}

	// ICE candidates are trickled via the subscriber's OnICECandidate callback
	log.Printf("ICE restart completed for subscriber %s", subscriberID)
	return answer.SDP, nil
}

// HandleSubscriberOffer processes a client-initiated offer on an existing subscriber connection.
// Used for renegotiation when a client adds a microphone track.
// Returns (answer SDP, whether a server-side offer was rolled back, error).
func (s *SFU) HandleSubscriberOffer(roomSlug, subscriberID, sdpOffer string) (string, bool, error) {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return "", false, fmt.Errorf("room not found: %s", roomSlug)
	}

	room.mu.RLock()
	sub, ok := room.Subscribers[subscriberID]
	room.mu.RUnlock()

	if !ok {
		return "", false, fmt.Errorf("subscriber not found: %s", subscriberID)
	}

	sub.SignalingMu.Lock()
	defer sub.SignalingMu.Unlock()

	rolledBack := false

	// Handle "glare": if the server has a pending offer (have-local-offer) when
	// the client sends its own offer, rollback the server's offer to accept the
	// client's. Tracks added by the rolled-back offer are still on the PC and
	// will be included in a follow-up renegotiation.
	if sub.PeerConnection.SignalingState() == webrtc.SignalingStateHaveLocalOffer {
		log.Printf("Rolling back server offer for subscriber %s to handle client offer (glare)", subscriberID)
		if err := sub.PeerConnection.SetLocalDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeRollback}); err != nil {
			return "", false, fmt.Errorf("failed to rollback: %w", err)
		}
		// Tracks from the rolled-back offer are still attached but were never
		// negotiated — flag them for the post-answer flush.
		sub.needsRenegotiation = true
		rolledBack = true
	}

	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdpOffer,
	}

	if err := sub.PeerConnection.SetRemoteDescription(offer); err != nil {
		return "", false, fmt.Errorf("failed to set remote description: %w", err)
	}

	// New client-initiated negotiation — fresh per-negotiation candidate budget.
	sub.resetICECandidateBudget()

	answer, err := sub.PeerConnection.CreateAnswer(nil)
	if err != nil {
		return "", false, fmt.Errorf("failed to create answer: %w", err)
	}

	if err := sub.PeerConnection.SetLocalDescription(answer); err != nil {
		return "", false, fmt.Errorf("failed to set local description: %w", err)
	}

	// ICE candidates are trickled via the subscriber's OnICECandidate callback
	log.Printf("Renegotiation completed for subscriber %s (rolledBack=%v)", subscriberID, rolledBack)
	return answer.SDP, rolledBack, nil
}

// GetIngestForRoom finds the ingest session bound to a room's stream key
func (s *SFU) GetIngestForRoom(roomSlug string, getStreamKeyToken func(slug string) (string, error)) (*IngestSession, error) {
	token, err := getStreamKeyToken(roomSlug)
	if err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	ingest, ok := s.ingests[token]
	if !ok {
		return nil, fmt.Errorf("no active ingest for room")
	}

	return ingest, nil
}

// IsRoomLive checks if a room has active tracks
func (s *SFU) IsRoomLive(roomSlug string) bool {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return false
	}

	room.mu.RLock()
	defer room.mu.RUnlock()

	return room.VideoTrack != nil || room.AudioTrack != nil
}

// VoiceSession represents a participant's voice connection
type VoiceSession struct {
	ParticipantID  string
	PeerConnection *webrtc.PeerConnection
	AudioTrack     *webrtc.TrackLocalStaticRTP
	done           chan struct{}
	closeOnce      sync.Once
	// Muted is the server-enforced gate for this participant's voice RTP.
	// When true, the voice relay forwarder drops incoming packets instead of
	// writing them to the local track — so admin:mute is effective even if a
	// malicious client ignores the client-side mute request.
	Muted atomic.Bool
}

// HandleVoiceOffer processes an offer from a client wanting to send voice audio
// over a dedicated peer connection. Returns the SDP answer to send back.
// (The primary voice path is the subscriber PC; this remains for clients that
// negotiate a standalone voice PC.)
func (s *SFU) HandleVoiceOffer(roomSlug, participantID, offerSDP string, onTrack func(participantID string, track *webrtc.TrackRemote)) (string, error) {
	// Create a peer connection
	pc, err := s.CreatePeerConnection()
	if err != nil {
		return "", fmt.Errorf("failed to create peer connection: %w", err)
	}

	// Set the handler for when we receive the audio track
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Printf("Received voice track from %s: %s", participantID, track.Kind())
		if track.Kind() == webrtc.RTPCodecTypeAudio {
			onTrack(participantID, track)
		}
	})

	// Set the remote description (the offer)
	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  offerSDP,
	}
	if err := pc.SetRemoteDescription(offer); err != nil {
		pc.Close()
		return "", fmt.Errorf("failed to set remote description: %w", err)
	}

	// Create answer
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		pc.Close()
		return "", fmt.Errorf("failed to create answer: %w", err)
	}

	// Set local description
	if err := pc.SetLocalDescription(answer); err != nil {
		pc.Close()
		return "", fmt.Errorf("failed to set local description: %w", err)
	}

	// Wait for ICE gathering (bounded — see iceGatherTimeout).
	waitForICEGather(pc)

	// Store the voice session. If the participant already had one (rejoin),
	// close the old PC first so its terminal-state callback can't later tear
	// down the replacement.
	newSession := &VoiceSession{
		ParticipantID:  participantID,
		PeerConnection: pc,
		done:           make(chan struct{}),
	}
	room := s.GetRoomTracks(roomSlug)
	room.mu.Lock()
	if room.VoiceSessions == nil {
		room.VoiceSessions = make(map[string]*VoiceSession)
	}
	prev, replaced := room.VoiceSessions[participantID]
	room.VoiceSessions[participantID] = newSession
	room.mu.Unlock()

	if replaced && prev != nil {
		log.Printf("Replacing existing voice session for %s (rejoin)", participantID)
		prev.closeOnce.Do(func() { close(prev.done) })
		if prev.PeerConnection != nil {
			_ = prev.PeerConnection.Close()
		}
	}

	// Handle connection state. Only act on terminal states — "disconnected" is
	// transient and can recover. Identity-check so a zombie callback from a
	// previously-replaced session can't delete the current one.
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("Voice connection state for %s: %s", participantID, state)
		switch state {
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed:
			s.removeVoiceSessionIfSame(roomSlug, participantID, newSession)
		}
	})

	ld := pc.LocalDescription()
	if ld == nil {
		pc.Close()
		return "", fmt.Errorf("local description nil after gather")
	}
	return ld.SDP, nil
}

// RemoveVoiceSession removes a voice session
func (s *SFU) RemoveVoiceSession(roomSlug, participantID string) {
	s.removeVoiceSessionIfSame(roomSlug, participantID, nil)
}

// removeVoiceSessionIfSame removes the participant's voice session only when
// the map entry matches the given expected session. Prevents a zombie callback
// from a replaced (rejoin) session from tearing down the live replacement.
// If expected is nil, removal is unconditional (used for explicit cleanup).
func (s *SFU) removeVoiceSessionIfSame(roomSlug, participantID string, expected *VoiceSession) {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return
	}

	room.mu.Lock()
	session, ok := room.VoiceSessions[participantID]
	if !ok {
		room.mu.Unlock()
		return
	}
	if expected != nil && session != expected {
		room.mu.Unlock()
		return
	}
	delete(room.VoiceSessions, participantID)
	delete(room.VoiceRemoteTracks, participantID)
	delete(room.VoiceLocalTracks, participantID)
	// Stop the relay goroutine for this participant, if any.
	if ch, ok := room.voiceRelayDone[participantID]; ok {
		close(ch)
		delete(room.voiceRelayDone, participantID)
	}
	room.mu.Unlock()

	session.closeOnce.Do(func() {
		close(session.done)
	})
	if session.PeerConnection != nil {
		_ = session.PeerConnection.Close()
	}
	log.Printf("Removed voice session for %s from room %s", participantID, roomSlug)
}

// SetVoiceMuted toggles the server-side mute flag for a participant's voice
// relay. The relay forwarder drops incoming RTP packets while muted, so the
// effect is immediate and can't be circumvented by a client that ignores the
// admin:muted broadcast.
func (s *SFU) SetVoiceMuted(roomSlug, participantID string, muted bool) {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return
	}
	room.mu.RLock()
	session := room.VoiceSessions[participantID]
	room.mu.RUnlock()
	if session != nil {
		session.Muted.Store(muted)
	}
}

// StoreVoiceRemoteTrack stores the remote track reference for forwarding to late joiners
func (s *SFU) StoreVoiceRemoteTrack(roomSlug, participantID string, track *webrtc.TrackRemote) {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return
	}

	room.mu.Lock()
	defer room.mu.Unlock()

	if room.VoiceRemoteTracks == nil {
		room.VoiceRemoteTracks = make(map[string]*webrtc.TrackRemote)
	}
	room.VoiceRemoteTracks[participantID] = track
}

// RemoveVoiceRemoteTrack removes a stored voice remote track
func (s *SFU) RemoveVoiceRemoteTrack(roomSlug, participantID string) {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return
	}

	room.mu.Lock()
	defer room.mu.Unlock()

	delete(room.VoiceRemoteTracks, participantID)
}

// RLockVoiceTracks acquires a read lock for accessing voice tracks
func (rt *RoomTracks) RLockVoiceTracks() {
	rt.mu.RLock()
}

// RUnlockVoiceTracks releases the read lock for voice tracks
func (rt *RoomTracks) RUnlockVoiceTracks() {
	rt.mu.RUnlock()
}

// GetVoiceRemoteTrackLocked returns a voice remote track (caller must hold RLock)
func (rt *RoomTracks) GetVoiceRemoteTrackLocked(participantID string) *webrtc.TrackRemote {
	if rt.VoiceRemoteTracks == nil {
		return nil
	}
	return rt.VoiceRemoteTracks[participantID]
}

// GetActiveVoiceSessions returns the participant IDs that have active voice remote tracks in a room
func (s *SFU) GetActiveVoiceSessions(roomSlug string) []string {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return nil
	}

	room.mu.RLock()
	defer room.mu.RUnlock()

	ids := make([]string, 0, len(room.VoiceRemoteTracks))
	for id := range room.VoiceRemoteTracks {
		ids = append(ids, id)
	}
	return ids
}

// CreateVoiceRelayTrack returns the shared local relay track for a voice
// source, creating it on first use. On subsequent calls (speaker rejoin), the
// EXISTING local track is reused and only the forwarding goroutine is swapped
// to read from the new remote track. This is important: if we created a new
// local track on every rejoin, every subscriber would accumulate a dormant
// sender (bound to the old track) plus a new one — leaking transceivers over
// time and forcing unnecessary renegotiations.
//
// The second return value reports whether a NEW relay was created; the caller
// uses this to decide whether to fan the track out to subscribers (new) or
// skip that step (reuse — subscribers' existing senders already carry it).
//
// Writing to a TrackLocalStaticRTP fans out to all PeerConnections it has been
// added to, so we only need one reader per remote track.
func (s *SFU) CreateVoiceRelayTrack(roomSlug, participantID string, remoteTrack *webrtc.TrackRemote) (*webrtc.TrackLocalStaticRTP, bool, error) {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return nil, false, fmt.Errorf("room not found: %s", roomSlug)
	}

	done := make(chan struct{})

	room.mu.Lock()
	if room.VoiceLocalTracks == nil {
		room.VoiceLocalTracks = make(map[string]*webrtc.TrackLocalStaticRTP)
	}
	existing, reused := room.VoiceLocalTracks[participantID]
	var localTrack *webrtc.TrackLocalStaticRTP
	if reused {
		localTrack = existing
	} else {
		created, err := webrtc.NewTrackLocalStaticRTP(
			remoteTrack.Codec().RTPCodecCapability,
			fmt.Sprintf("voice-%s", participantID),
			fmt.Sprintf("voice-stream-%s", participantID),
		)
		if err != nil {
			room.mu.Unlock()
			return nil, false, fmt.Errorf("failed to create relay track: %w", err)
		}
		localTrack = created
		room.VoiceLocalTracks[participantID] = localTrack
	}
	// Capture the Muted flag pointer now so the forwarder doesn't need a map
	// lookup per packet. The voice usually arrives over the subscriber PC, so
	// there may be no VoiceSession yet — create a placeholder to host the
	// server-side mute gate (SetVoiceMuted looks it up here).
	if room.VoiceSessions == nil {
		room.VoiceSessions = make(map[string]*VoiceSession)
	}
	vs := room.VoiceSessions[participantID]
	if vs == nil {
		vs = &VoiceSession{
			ParticipantID: participantID,
			done:          make(chan struct{}),
		}
		room.VoiceSessions[participantID] = vs
	}
	mutedFlag := &vs.Muted
	if room.voiceRelayDone == nil {
		room.voiceRelayDone = make(map[string]chan struct{})
	}
	// Stop any previous relay goroutine for this participant before
	// replacing it (e.g. mic restarted or speaker rejoined) so it can't leak
	// and there's no duplicate writer to the shared local track.
	if prev, ok := room.voiceRelayDone[participantID]; ok {
		close(prev)
	}
	room.voiceRelayDone[participantID] = done
	room.mu.Unlock()

	// Single forwarding goroutine — reads from the remote track once
	// and writes to the local track, which fans out to all bound PCs.
	go relayTrackLoop(remoteTrack, localTrack, done, mutedFlag, "Voice", participantID)

	if reused {
		log.Printf("Rebound voice relay for %s in room %s (speaker rejoin)", participantID, roomSlug)
	} else {
		log.Printf("Created voice relay track for %s in room %s", participantID, roomSlug)
	}
	return localTrack, !reused, nil
}

// relayTrackLoop forwards RTP packets from a remote track to a local relay
// track until the done channel closes or the remote track ends. A periodic
// read deadline lets the loop observe cancellation even when no packets
// arrive (e.g. when PeerConnection closure is delayed), so the goroutine
// cannot leak.
//
// If muted is non-nil, packets are dropped while it is set: this is the
// server-enforced admin mute gate, effective even if a malicious client
// ignores the client-side mute request.
func relayTrackLoop(remoteTrack *webrtc.TrackRemote, localTrack *webrtc.TrackLocalStaticRTP, done <-chan struct{}, muted *atomic.Bool, label, participantID string) {
	buf := make([]byte, 1500)
	for {
		select {
		case <-done:
			log.Printf("%s relay stopped for %s", label, participantID)
			return
		default:
		}

		_ = remoteTrack.SetReadDeadline(time.Now().Add(time.Second))
		n, _, err := remoteTrack.Read(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue // deadline expired — loop to re-check done
			}
			log.Printf("%s relay read ended for %s: %v", label, participantID, err)
			return
		}
		// Server-enforced mute gate. Dropping packets here keeps the
		// admin:mute contract honest.
		if muted != nil && muted.Load() {
			continue
		}
		if _, err := localTrack.Write(buf[:n]); err != nil {
			log.Printf("%s relay write error for %s: %v", label, participantID, err)
			return
		}
	}
}

// GetVoiceRelayTrack returns the relay local track for a voice source (if one exists)
func (s *SFU) GetVoiceRelayTrack(roomSlug, participantID string) *webrtc.TrackLocalStaticRTP {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return nil
	}

	room.mu.RLock()
	defer room.mu.RUnlock()

	if room.VoiceLocalTracks == nil {
		return nil
	}
	return room.VoiceLocalTracks[participantID]
}

// AddVoiceTrackToSubscriber adds a shared voice relay track to a subscriber's
// peer connection and creates a renegotiation offer.
// Returns the renegotiation offer SDP that needs to be sent to the subscriber.
func (s *SFU) AddVoiceTrackToSubscriber(roomSlug, subscriberID, voiceOwnerID string, localTrack *webrtc.TrackLocalStaticRTP) (string, error) {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return "", fmt.Errorf("room not found: %s", roomSlug)
	}

	room.mu.RLock()
	sub, ok := room.Subscribers[subscriberID]
	room.mu.RUnlock()

	if !ok {
		return "", fmt.Errorf("subscriber not found: %s", subscriberID)
	}

	offerSDP, err := s.addTrackAndRenegotiate(sub, localTrack, "voice")
	if err != nil {
		return "", err
	}

	log.Printf("Added voice track from %s to subscriber %s", voiceOwnerID, subscriberID)
	return offerSDP, nil
}

// trySignalingLock attempts to acquire the subscriber's signaling mutex,
// polling until the timeout elapses or the subscriber is removed (done closes).
func (sub *Subscriber) trySignalingLock(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if sub.SignalingMu.TryLock() {
			return true
		}
		select {
		case <-sub.done:
			return false
		default:
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// addTrackAndRenegotiate adds a local relay track to a subscriber's peer
// connection and creates a renegotiation offer. The track is NEVER dropped:
// if another offer/answer exchange is in flight the track is attached anyway
// (legal mid-negotiation) and a follow-up offer is pushed when signaling
// settles; if the signaling lock can't be acquired the track is queued and
// attached at the next flush point. Returns "" with nil error when the
// renegotiation was deferred — the offer will be delivered via the
// subscriber's OnRenegotiationNeeded callback instead.
func (s *SFU) addTrackAndRenegotiate(sub *Subscriber, localTrack *webrtc.TrackLocalStaticRTP, label string) (string, error) {
	const maxAttempts = 3
	backoff := 250 * time.Millisecond

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-sub.done:
				return "", fmt.Errorf("subscriber %s removed while waiting to add %s track", sub.ID, label)
			case <-time.After(backoff):
			}
			backoff *= 2
		}

		offerSDP, deferred, retryable, err := s.tryAddTrackAndRenegotiate(sub, localTrack)
		if err == nil {
			if deferred {
				log.Printf("Attached %s track to subscriber %s mid-negotiation; offer deferred to next stable state", label, sub.ID)
				return "", nil
			}
			return offerSDP, nil
		}
		if !retryable {
			return "", err
		}
		log.Printf("Signaling lock busy adding %s track to subscriber %s (attempt %d/%d): %v", label, sub.ID, attempt, maxAttempts, err)
	}

	// Persistent lock contention: queue the track so the next flush point
	// (answer applied, renegotiation completed, ICE restart) attaches it.
	sub.pendingMu.Lock()
	sub.pendingTracks = append(sub.pendingTracks, localTrack)
	sub.pendingMu.Unlock()
	log.Printf("Queued %s track for subscriber %s after signaling lock contention", label, sub.ID)
	// Best-effort async flush in case no further signaling traffic arrives.
	go s.tryFlushPendingRenegotiation(sub, 5*time.Second)
	return "", nil
}

// tryAddTrackAndRenegotiate performs a single attempt of the add-track +
// renegotiate operation. deferred=true means the track was attached but the
// offer must wait for the in-flight negotiation to settle. retryable=true
// (with err) means the signaling lock was busy and the caller should retry.
func (s *SFU) tryAddTrackAndRenegotiate(sub *Subscriber, localTrack *webrtc.TrackLocalStaticRTP) (offerSDP string, deferred bool, retryable bool, err error) {
	// Guard: don't touch a PC that is already closed/failed
	connState := sub.PeerConnection.ConnectionState()
	if connState == webrtc.PeerConnectionStateClosed || connState == webrtc.PeerConnectionStateFailed {
		return "", false, false, fmt.Errorf("subscriber %s connection in terminal state: %s", sub.ID, connState)
	}

	// Wait for the signaling mutex with a timeout. Another goroutine
	// (e.g. HandleSubscriberOffer processing a client mic offer) may hold
	// the lock, so we poll rather than block indefinitely.
	if !sub.trySignalingLock(2 * time.Second) {
		return "", false, true, fmt.Errorf("timed out waiting for signaling lock for subscriber %s", sub.ID)
	}
	defer sub.SignalingMu.Unlock()

	// After acquiring the lock, re-check connection state
	connState = sub.PeerConnection.ConnectionState()
	if connState == webrtc.PeerConnectionStateClosed || connState == webrtc.PeerConnectionStateFailed {
		return "", false, false, fmt.Errorf("subscriber %s connection in terminal state: %s", sub.ID, connState)
	}

	// Attach the shared relay track now — AddTrack is legal in any signaling
	// state and pion fans out writes to all PCs that have added this track.
	if !pcHasTrack(sub.PeerConnection, localTrack) {
		if _, err := sub.PeerConnection.AddTrack(localTrack); err != nil {
			return "", false, false, fmt.Errorf("failed to add track: %w", err)
		}
	}

	// A non-stable signaling state means another offer/answer exchange is in
	// flight. The track is attached; defer the offer to the next flush point
	// instead of failing (the old behavior permanently lost the track).
	if sub.PeerConnection.SignalingState() != webrtc.SignalingStateStable {
		sub.needsRenegotiation = true
		return "", true, false, nil
	}

	// Create renegotiation offer to notify subscriber of new track
	offer, err := sub.PeerConnection.CreateOffer(nil)
	if err != nil {
		sub.needsRenegotiation = true
		return "", false, false, fmt.Errorf("failed to create renegotiation offer: %w", err)
	}

	if err := sub.PeerConnection.SetLocalDescription(offer); err != nil {
		// Track is attached; mark for a follow-up offer so it isn't lost.
		sub.needsRenegotiation = true
		return "", false, false, fmt.Errorf("failed to set local description: %w", err)
	}

	// ICE candidates are trickled via the subscriber's OnICECandidate callback
	return offer.SDP, false, false, nil
}

// flushPendingRenegotiationLocked attaches any queued tracks and, if any
// tracks were attached while signaling was busy, creates a follow-up
// renegotiation offer and pushes it via OnRenegotiationNeeded.
// Caller must hold sub.SignalingMu.
func (sub *Subscriber) flushPendingRenegotiationLocked() {
	connState := sub.PeerConnection.ConnectionState()
	if connState == webrtc.PeerConnectionStateClosed || connState == webrtc.PeerConnectionStateFailed {
		sub.pendingMu.Lock()
		sub.pendingTracks = nil
		sub.pendingMu.Unlock()
		return
	}

	sub.pendingMu.Lock()
	pending := sub.pendingTracks
	sub.pendingTracks = nil
	cb := sub.OnRenegotiationNeeded
	sub.pendingMu.Unlock()

	for _, t := range pending {
		if pcHasTrack(sub.PeerConnection, t) {
			continue
		}
		if _, err := sub.PeerConnection.AddTrack(t); err != nil {
			log.Printf("Failed to attach queued track for subscriber %s: %v", sub.ID, err)
			continue
		}
		sub.needsRenegotiation = true
	}

	if !sub.needsRenegotiation {
		return
	}
	// Not stable yet — the next flush point (answer applied) will offer.
	if sub.PeerConnection.SignalingState() != webrtc.SignalingStateStable {
		return
	}
	// Callback not wired yet — flag stays set; flushed once it is wired.
	if cb == nil {
		return
	}

	offer, err := sub.PeerConnection.CreateOffer(nil)
	if err != nil {
		log.Printf("Failed to create flush renegotiation offer for subscriber %s: %v", sub.ID, err)
		return
	}
	if err := sub.PeerConnection.SetLocalDescription(offer); err != nil {
		log.Printf("Failed to set flush renegotiation offer for subscriber %s: %v", sub.ID, err)
		return
	}
	sub.needsRenegotiation = false
	log.Printf("Flushed pending renegotiation for subscriber %s", sub.ID)
	// Deliver outside the signaling-critical path.
	go cb(offer.SDP)
}

// tryFlushPendingRenegotiation acquires the signaling lock (bounded) and
// flushes pending renegotiation work for a subscriber.
func (s *SFU) tryFlushPendingRenegotiation(sub *Subscriber, lockTimeout time.Duration) {
	if !sub.trySignalingLock(lockTimeout) {
		return
	}
	defer sub.SignalingMu.Unlock()
	sub.flushPendingRenegotiationLocked()
}

// FlushPendingRenegotiation flushes deferred track adds / renegotiations for
// a subscriber. Handlers call this after a signaling exchange completes
// (answer applied, client offer answered, ICE restart answered) so any tracks
// attached mid-negotiation get their follow-up offer, in order, on the same
// websocket as the exchange that unblocked them.
func (s *SFU) FlushPendingRenegotiation(roomSlug, subscriberID string) {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return
	}
	room.mu.RLock()
	sub, ok := room.Subscribers[subscriberID]
	room.mu.RUnlock()
	if !ok {
		return
	}
	s.tryFlushPendingRenegotiation(sub, 5*time.Second)
}

// SetSubscriberRenegotiationCallback registers the callback used to push
// server-initiated renegotiation offers that were deferred while another
// negotiation was in flight. Flushes immediately if work is already pending.
func (s *SFU) SetSubscriberRenegotiationCallback(roomSlug, subscriberID string, cb func(offerSDP string)) {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return
	}
	room.mu.RLock()
	sub, ok := room.Subscribers[subscriberID]
	room.mu.RUnlock()
	if !ok {
		return
	}
	sub.pendingMu.Lock()
	sub.OnRenegotiationNeeded = cb
	sub.pendingMu.Unlock()
	go s.tryFlushPendingRenegotiation(sub, 5*time.Second)
}

// RenegotiateSubscriber creates a new offer for a subscriber after tracks have changed
// This is used when voice tracks are added or removed
func (s *SFU) RenegotiateSubscriber(roomSlug, subscriberID string) (string, error) {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return "", fmt.Errorf("room not found: %s", roomSlug)
	}

	room.mu.RLock()
	sub, ok := room.Subscribers[subscriberID]
	room.mu.RUnlock()

	if !ok {
		return "", fmt.Errorf("subscriber not found: %s", subscriberID)
	}

	sub.SignalingMu.Lock()
	defer sub.SignalingMu.Unlock()

	// Only renegotiate if in stable state
	if sub.PeerConnection.SignalingState() != webrtc.SignalingStateStable {
		return "", fmt.Errorf("cannot renegotiate subscriber %s: signaling state is %s", subscriberID, sub.PeerConnection.SignalingState())
	}

	// Create new offer
	offer, err := sub.PeerConnection.CreateOffer(nil)
	if err != nil {
		return "", fmt.Errorf("failed to create offer: %w", err)
	}

	if err := sub.PeerConnection.SetLocalDescription(offer); err != nil {
		return "", fmt.Errorf("failed to set local description: %w", err)
	}

	// ICE candidates are trickled via the subscriber's OnICECandidate callback
	log.Printf("Renegotiation offer created for subscriber %s", subscriberID)
	return offer.SDP, nil
}

// HandleRenegotiationAnswer processes an answer from a subscriber during renegotiation
func (s *SFU) HandleRenegotiationAnswer(roomSlug, subscriberID, sdpAnswer string) error {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return fmt.Errorf("room not found: %s", roomSlug)
	}

	room.mu.RLock()
	sub, ok := room.Subscribers[subscriberID]
	room.mu.RUnlock()

	if !ok {
		return fmt.Errorf("subscriber not found: %s", subscriberID)
	}

	sub.SignalingMu.Lock()
	defer sub.SignalingMu.Unlock()

	// Ignore stale answers: if the PC is not in have-local-offer (e.g. because
	// the offer was rolled back to accept a client-initiated offer), there is
	// nothing to apply this answer to.
	if sub.PeerConnection.SignalingState() != webrtc.SignalingStateHaveLocalOffer {
		log.Printf("Ignoring stale renegotiation answer for subscriber %s (state: %s)", subscriberID, sub.PeerConnection.SignalingState())
		return nil
	}

	answer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  sdpAnswer,
	}

	if err := sub.PeerConnection.SetRemoteDescription(answer); err != nil {
		return fmt.Errorf("failed to set remote description: %w", err)
	}

	// The answer completes a server-initiated renegotiation; any candidates
	// the client trickles for it count against a fresh budget.
	sub.resetICECandidateBudget()

	log.Printf("Renegotiation answer processed for subscriber %s", subscriberID)
	return nil
}

// CreateScreenShareRelayTrack creates a relay track for screen share fan-out,
// mirroring the voice relay pattern. One goroutine reads from the remote track
// and writes to the local track, which fans out to all bound PeerConnections.
func (s *SFU) CreateScreenShareRelayTrack(roomSlug, participantID string, remoteTrack *webrtc.TrackRemote) (*webrtc.TrackLocalStaticRTP, error) {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return nil, fmt.Errorf("room not found: %s", roomSlug)
	}

	localTrack, err := webrtc.NewTrackLocalStaticRTP(
		remoteTrack.Codec().RTPCodecCapability,
		fmt.Sprintf("screenshare-%s", participantID),
		fmt.Sprintf("screenshare-stream-%s", participantID),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create screen share relay track: %w", err)
	}

	done := make(chan struct{})

	room.mu.Lock()
	// Stop any previous screen share relay goroutine before replacing it.
	if room.screenShareDone != nil {
		close(room.screenShareDone)
	}
	room.screenShareDone = done
	room.ScreenShareParticipantID = participantID
	room.ScreenShareRemoteTrack = remoteTrack
	room.ScreenShareLocalTrack = localTrack
	room.mu.Unlock()

	// Single forwarding goroutine — reads from the remote track once
	// and writes to the local track, which fans out to all bound PCs.
	go relayTrackLoop(remoteTrack, localTrack, done, nil, "Screen share", participantID)

	log.Printf("Created screen share relay track for %s in room %s", participantID, roomSlug)
	return localTrack, nil
}

// AddScreenShareTrackToSubscriber adds the screen share relay track to a
// subscriber's peer connection and creates a renegotiation offer.
// Same pattern as AddVoiceTrackToSubscriber.
func (s *SFU) AddScreenShareTrackToSubscriber(roomSlug, subscriberID, sharerID string, localTrack *webrtc.TrackLocalStaticRTP) (string, error) {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return "", fmt.Errorf("room not found: %s", roomSlug)
	}

	room.mu.RLock()
	sub, ok := room.Subscribers[subscriberID]
	room.mu.RUnlock()

	if !ok {
		return "", fmt.Errorf("subscriber not found: %s", subscriberID)
	}

	offerSDP, err := s.addTrackAndRenegotiate(sub, localTrack, "screen share")
	if err != nil {
		return "", err
	}

	log.Printf("Added screen share track from %s to subscriber %s", sharerID, subscriberID)
	return offerSDP, nil
}

// RemoveScreenShareTrack removes the screen share track from all subscribers
// and clears the screen share state. Returns the list of subscriber IDs that
// were affected (for renegotiation).
func (s *SFU) RemoveScreenShareTrack(roomSlug string) []string {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return nil
	}

	room.mu.Lock()
	localTrack := room.ScreenShareLocalTrack
	sharerID := room.ScreenShareParticipantID
	room.ScreenShareParticipantID = ""
	room.ScreenShareRemoteTrack = nil
	room.ScreenShareLocalTrack = nil
	// Stop the relay goroutine
	if room.screenShareDone != nil {
		close(room.screenShareDone)
		room.screenShareDone = nil
	}
	room.mu.Unlock()

	if localTrack == nil || sharerID == "" {
		return nil
	}

	// Remove the screen share sender from all subscribers
	room.mu.RLock()
	subIDs := make([]string, 0, len(room.Subscribers))
	for id := range room.Subscribers {
		subIDs = append(subIDs, id)
	}
	room.mu.RUnlock()

	var affected []string
	for _, subID := range subIDs {
		room.mu.RLock()
		sub, ok := room.Subscribers[subID]
		room.mu.RUnlock()
		if !ok {
			continue
		}

		// Acquire SignalingMu to prevent racing with concurrent SDP operations
		sub.SignalingMu.Lock()
		// Find and remove the sender for the screen share track
		for _, sender := range sub.PeerConnection.GetSenders() {
			if sender.Track() == localTrack {
				if err := sub.PeerConnection.RemoveTrack(sender); err != nil {
					log.Printf("Failed to remove screen share sender from %s: %v", subID, err)
				} else {
					affected = append(affected, subID)
				}
				break
			}
		}
		sub.SignalingMu.Unlock()
	}

	log.Printf("Removed screen share track from %s in room %s, affected %d subscribers", sharerID, roomSlug, len(affected))
	return affected
}
