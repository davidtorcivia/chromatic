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

// MaxICECandidates is the maximum number of ICE candidates allowed per session
// to prevent memory exhaustion from ICE candidate flooding attacks
const MaxICECandidates = 50

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

// RoomTracks holds the tracks being distributed to a room
type RoomTracks struct {
	mu                       sync.RWMutex
	RoomSlug                 string
	VideoTrack               *webrtc.TrackLocalStaticRTP
	AudioTrack               *webrtc.TrackLocalStaticRTP
	Subscribers              map[string]*Subscriber
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
	// Rate limit client-to-server ICE candidates (matches ingest limit)
	iceCandidateCount int
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

// SetIngest registers an ingest session
func (s *SFU) SetIngest(streamKeyToken string, session *IngestSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ingests[streamKeyToken] = session
}

// RemoveIngest removes an ingest session
func (s *SFU) RemoveIngest(streamKeyToken string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session, ok := s.ingests[streamKeyToken]; ok {
		// Use sync.Once to prevent double-close panic
		session.closeOnce.Do(func() {
			close(session.done)
		})
		delete(s.ingests, streamKeyToken)
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

// RemoveSubscriberIfMatch removes a subscriber only if the current subscriber
// in the map is the same instance as `expected`. This prevents a stale
// OnConnectionStateChange callback from removing a replacement subscriber
// that was created during a reconnect.
//
// The pointer check and removal happen under a single write lock to prevent
// a TOCTOU race where AddSubscriber replaces the subscriber between the check
// and the removal.
func (rt *RoomTracks) RemoveSubscriberIfMatch(id string, expected *Subscriber) {
	rt.mu.Lock()
	current, ok := rt.Subscribers[id]
	if !ok || current != expected {
		rt.mu.Unlock()
		return
	}
	pc := rt.removeSubscriberLocked(id)
	rt.mu.Unlock()

	// Close outside the lock — pion state callbacks re-enter rt.mu.
	if pc != nil {
		pc.Close()
	}
}

// RemoveSubscriber removes a subscriber from a room
func (rt *RoomTracks) RemoveSubscriber(id string) {
	rt.mu.Lock()
	pc := rt.removeSubscriberLocked(id)
	rt.mu.Unlock()

	// Close outside the lock — pion state callbacks re-enter rt.mu.
	if pc != nil {
		pc.Close()
	}
}

// removeSubscriberLocked performs subscriber removal. Caller must hold rt.mu
// write lock and is responsible for closing the returned PeerConnection AFTER
// releasing the lock (pion fires OnConnectionStateChange callbacks during
// Close that call back into rt.mu, e.g. via RemoveSubscriberIfMatch).
func (rt *RoomTracks) removeSubscriberLocked(id string) *webrtc.PeerConnection {
	var pc *webrtc.PeerConnection
	if sub, ok := rt.Subscribers[id]; ok {
		// Use sync.Once to prevent double-close panic
		sub.closeOnce.Do(func() {
			close(sub.done)
		})
		// Hand the PeerConnection back to the caller for closing.
		pc = sub.PeerConnection
		// Clear ICE callback to release references to the client
		sub.candidateMu.Lock()
		sub.OnICECandidate = nil
		sub.pendingCandidates = nil
		sub.candidateMu.Unlock()
		delete(rt.Subscribers, id)
		// Track subscriber removal
		metrics.Get().ActiveSubscribers.Add(-1)
	}
	// Clean up any voice state owned by this participant and stop its relay.
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
	return pc
}

// BroadcastTrack sends track data to all subscribers
func (rt *RoomTracks) BroadcastTrack(track *webrtc.TrackLocalStaticRTP, data []byte) error {
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	// Write to all subscribers
	// Note: In production, you'd use track.WriteRTP for proper RTP handling
	_, err := track.Write(data)
	return err
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

// CreateSubscriberConnection creates a new subscriber peer connection for a room.
// Returns the peer connection and an SDP offer string to send to the client.
// ICE candidates are NOT included in the offer — they are trickled via
// EnableSubscriberTrickleICE after the offer has been sent to the client.
func (s *SFU) CreateSubscriberConnection(roomSlug, subscriberID string) (*webrtc.PeerConnection, string, error) {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return nil, "", fmt.Errorf("room not found: %s", roomSlug)
	}

	room.mu.RLock()
	videoTrack := room.VideoTrack
	audioTrack := room.AudioTrack
	// Collect existing voice relay tracks so they are included in the initial
	// offer. This avoids a separate renegotiation that would race with the
	// initial offer-answer exchange and corrupt the signaling state.
	var voiceRelayTracks []*webrtc.TrackLocalStaticRTP
	for pid, track := range room.VoiceLocalTracks {
		if pid != subscriberID {
			voiceRelayTracks = append(voiceRelayTracks, track)
		}
	}
	// Include active screen share track for late joiners
	var screenShareTrack *webrtc.TrackLocalStaticRTP
	if room.ScreenShareParticipantID != "" && room.ScreenShareParticipantID != subscriberID && room.ScreenShareLocalTrack != nil {
		screenShareTrack = room.ScreenShareLocalTrack
	}
	room.mu.RUnlock()

	// Create peer connection
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

	// Add video track if available
	if videoTrack != nil {
		sender, err := pc.AddTrack(videoTrack)
		if err != nil {
			pc.Close()
			return nil, "", fmt.Errorf("failed to add video track: %w", err)
		}
		sub.VideoSender = sender
		log.Printf("Added video track to subscriber %s", subscriberID)
	}

	// Add audio track if available
	if audioTrack != nil {
		sender, err := pc.AddTrack(audioTrack)
		if err != nil {
			pc.Close()
			return nil, "", fmt.Errorf("failed to add audio track: %w", err)
		}
		sub.AudioSender = sender
		log.Printf("Added audio track to subscriber %s", subscriberID)
	}

	// Add existing voice relay tracks so subscriber can hear them immediately
	for _, voiceTrack := range voiceRelayTracks {
		if _, err := pc.AddTrack(voiceTrack); err != nil {
			log.Printf("Failed to add voice relay track to subscriber %s: %v", subscriberID, err)
		} else {
			log.Printf("Added existing voice relay track to subscriber %s", subscriberID)
		}
	}

	// Add active screen share track so late joiners see it immediately
	if screenShareTrack != nil {
		if _, err := pc.AddTrack(screenShareTrack); err != nil {
			log.Printf("Failed to add screen share track to subscriber %s: %v", subscriberID, err)
		} else {
			log.Printf("Added existing screen share track to subscriber %s", subscriberID)
		}
	}

	room.AddSubscriber(sub)

	// Request a keyframe from the ingest so the new subscriber can decode immediately
	go s.RequestKeyframe(roomSlug)

	// Handle connection state — only remove on terminal states.
	// "disconnected" is transient and can recover via ICE restart from the client.
	// Capture `sub` so we only remove if this specific subscriber is still current
	// (prevents a stale callback from removing a replacement subscriber on reconnect).
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("Subscriber %s connection state: %s", subscriberID, state)
		switch state {
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed:
			room.RemoveSubscriberIfMatch(subscriberID, sub)
		}
	})

	// Create offer
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		pc.Close()
		room.RemoveSubscriber(subscriberID)
		return nil, "", fmt.Errorf("failed to create offer: %w", err)
	}

	// Set local description — starts ICE gathering in the background.
	// Candidates are buffered and sent via trickle ICE (no blocking wait).
	if err := pc.SetLocalDescription(offer); err != nil {
		pc.Close()
		room.RemoveSubscriber(subscriberID)
		return nil, "", fmt.Errorf("failed to set local description: %w", err)
	}

	return pc, offer.SDP, nil
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

	// Flush any buffered client-to-server ICE candidates
	sub.remoteCandidateMu.Lock()
	sub.remoteDescSet = true
	pending := sub.pendingRemoteCandidates
	sub.pendingRemoteCandidates = nil
	sub.remoteCandidateMu.Unlock()

	for _, c := range pending {
		if err := sub.PeerConnection.AddICECandidate(*c); err != nil {
			log.Printf("Failed to add buffered ICE candidate for %s: %v", subscriberID, err)
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

	// Set the new offer from the client
	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdpOffer,
	}

	if err := sub.PeerConnection.SetRemoteDescription(offer); err != nil {
		return "", fmt.Errorf("failed to set remote description: %w", err)
	}

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
		rolledBack = true
	}

	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdpOffer,
	}

	if err := sub.PeerConnection.SetRemoteDescription(offer); err != nil {
		return "", false, fmt.Errorf("failed to set remote description: %w", err)
	}

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

// CreateVoiceRelayTrack creates a single local relay track for a voice source and
// starts one goroutine to forward RTP packets from the remote track. Writing to
// a TrackLocalStaticRTP fans out to all PeerConnections it has been added to,
// so we only need one reader per remote track.
func (s *SFU) CreateVoiceRelayTrack(roomSlug, participantID string, remoteTrack *webrtc.TrackRemote) (*webrtc.TrackLocalStaticRTP, error) {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return nil, fmt.Errorf("room not found: %s", roomSlug)
	}

	localTrack, err := webrtc.NewTrackLocalStaticRTP(
		remoteTrack.Codec().RTPCodecCapability,
		fmt.Sprintf("voice-%s", participantID),
		fmt.Sprintf("voice-stream-%s", participantID),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create relay track: %w", err)
	}

	done := make(chan struct{})

	room.mu.Lock()
	if room.VoiceLocalTracks == nil {
		room.VoiceLocalTracks = make(map[string]*webrtc.TrackLocalStaticRTP)
	}
	room.VoiceLocalTracks[participantID] = localTrack
	if room.voiceRelayDone == nil {
		room.voiceRelayDone = make(map[string]chan struct{})
	}
	// Stop any previous relay goroutine for this participant before
	// replacing it (e.g. mic restarted) so it can't leak.
	if prev, ok := room.voiceRelayDone[participantID]; ok {
		close(prev)
	}
	room.voiceRelayDone[participantID] = done
	room.mu.Unlock()

	// Single forwarding goroutine — reads from the remote track once
	// and writes to the local track, which fans out to all bound PCs.
	go relayTrackLoop(remoteTrack, localTrack, done, "Voice", participantID)

	log.Printf("Created voice relay track for %s in room %s", participantID, roomSlug)
	return localTrack, nil
}

// relayTrackLoop forwards RTP packets from a remote track to a local relay
// track until the done channel closes or the remote track ends. A periodic
// read deadline lets the loop observe cancellation even when no packets
// arrive (e.g. when PeerConnection closure is delayed), so the goroutine
// cannot leak.
func relayTrackLoop(remoteTrack *webrtc.TrackRemote, localTrack *webrtc.TrackLocalStaticRTP, done <-chan struct{}, label, participantID string) {
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
// connection and creates a renegotiation offer. Transient failures (signaling
// lock contention, signaling state not stable — e.g. a client mic offer in
// flight) are retried with backoff instead of permanently dropping the track
// for this subscriber. Retries abort early if the subscriber is removed or
// its connection reaches a terminal state.
func (s *SFU) addTrackAndRenegotiate(sub *Subscriber, localTrack *webrtc.TrackLocalStaticRTP, label string) (string, error) {
	const maxAttempts = 3
	backoff := 500 * time.Millisecond

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-sub.done:
				return "", fmt.Errorf("subscriber %s removed while waiting to add %s track", sub.ID, label)
			case <-time.After(backoff):
			}
			backoff *= 2
		}

		offerSDP, retryable, err := s.tryAddTrackAndRenegotiate(sub, localTrack)
		if err == nil {
			return offerSDP, nil
		}
		lastErr = err
		if !retryable {
			return "", err
		}
		log.Printf("Transient failure adding %s track to subscriber %s (attempt %d/%d): %v", label, sub.ID, attempt, maxAttempts, err)
	}

	log.Printf("Giving up adding %s track to subscriber %s after %d attempts: %v", label, sub.ID, maxAttempts, lastErr)
	return "", lastErr
}

// tryAddTrackAndRenegotiate performs a single attempt of the add-track +
// renegotiate operation. The retryable result indicates whether the failure
// is transient (lock contention or in-flight renegotiation) and worth retrying.
func (s *SFU) tryAddTrackAndRenegotiate(sub *Subscriber, localTrack *webrtc.TrackLocalStaticRTP) (offerSDP string, retryable bool, err error) {
	// Guard: don't touch a PC that is already closed/failed
	connState := sub.PeerConnection.ConnectionState()
	if connState == webrtc.PeerConnectionStateClosed || connState == webrtc.PeerConnectionStateFailed {
		return "", false, fmt.Errorf("subscriber %s connection in terminal state: %s", sub.ID, connState)
	}

	// Wait for the signaling mutex with a timeout. Another goroutine
	// (e.g. HandleSubscriberOffer processing a client mic offer) may hold
	// the lock, so we poll rather than block indefinitely.
	if !sub.trySignalingLock(2 * time.Second) {
		return "", true, fmt.Errorf("timed out waiting for signaling lock for subscriber %s", sub.ID)
	}
	defer sub.SignalingMu.Unlock()

	// After acquiring the lock, re-check connection state
	connState = sub.PeerConnection.ConnectionState()
	if connState == webrtc.PeerConnectionStateClosed || connState == webrtc.PeerConnectionStateFailed {
		return "", false, fmt.Errorf("subscriber %s connection in terminal state: %s", sub.ID, connState)
	}

	// A non-stable signaling state means another offer/answer exchange is in
	// flight; it should settle shortly, so this is retryable.
	if sub.PeerConnection.SignalingState() != webrtc.SignalingStateStable {
		return "", true, fmt.Errorf("signaling state not stable for subscriber %s: %s", sub.ID, sub.PeerConnection.SignalingState())
	}

	// Add the shared relay track to the subscriber's peer connection.
	// Pion fans out writes to all PCs that have added this track.
	// Failures past this point are NOT retryable: the track may already be
	// attached, and retrying would add it twice.
	if _, err := sub.PeerConnection.AddTrack(localTrack); err != nil {
		return "", false, fmt.Errorf("failed to add track: %w", err)
	}

	// Create renegotiation offer to notify subscriber of new track
	offer, err := sub.PeerConnection.CreateOffer(nil)
	if err != nil {
		return "", false, fmt.Errorf("failed to create renegotiation offer: %w", err)
	}

	if err := sub.PeerConnection.SetLocalDescription(offer); err != nil {
		return "", false, fmt.Errorf("failed to set local description: %w", err)
	}

	// ICE candidates are trickled via the subscriber's OnICECandidate callback
	return offer.SDP, false, nil
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
	go relayTrackLoop(remoteTrack, localTrack, done, "Screen share", participantID)

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
