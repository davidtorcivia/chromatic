package webrtc

import (
	"context"
	"fmt"
	"log"
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
}

// MaxICECandidates is the maximum number of ICE candidates allowed per session
// to prevent memory exhaustion from ICE candidate flooding attacks
const MaxICECandidates = 50

// iceGatherTimeout bounds how long we wait for ICE gathering to complete before
// returning the SDP with whatever candidates we already have. Host candidates
// are usually sufficient for LAN use; server-reflexive / relay candidates can
// trickle later via ICE restart. Without this cap a hung STUN/TURN server
// freezes the handshake indefinitely.
const iceGatherTimeout = 5 * time.Second

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

// localSDP returns the peer connection's local SDP, or an error if it was
// torn down between set and read (which happens under reconnect storms).
// Previously a nil dereference here would panic the request goroutine.
func localSDP(pc *webrtc.PeerConnection) (string, error) {
	if ld := pc.LocalDescription(); ld != nil {
		return ld.SDP, nil
	}
	return "", fmt.Errorf("local description unavailable (peer closed before SDP read)")
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
}

// RoomTracks holds the tracks being distributed to a room
type RoomTracks struct {
	mu               sync.RWMutex
	RoomSlug         string
	VideoTrack       *webrtc.TrackLocalStaticRTP
	AudioTrack       *webrtc.TrackLocalStaticRTP
	Subscribers      map[string]*Subscriber
	VoiceSessions    map[string]*VoiceSession                // Participant voice connections
	VoiceRemoteTracks map[string]*webrtc.TrackRemote          // Active voice remote tracks keyed by participant ID
	VoiceLocalTracks  map[string]*webrtc.TrackLocalStaticRTP  // Relay local tracks for voice fan-out
	IngestPC         *webrtc.PeerConnection                  // Reference to ingest PC for PLI requests
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

// Shutdown gracefully shuts down the SFU
func (s *SFU) Shutdown() {
	s.mu.Lock()

	// Collect tokens to avoid modifying map while iterating
	tokens := make([]string, 0, len(s.ingests))
	for token := range s.ingests {
		tokens = append(tokens, token)
	}

	// Close all ingest sessions
	for _, token := range tokens {
		if session, ok := s.ingests[token]; ok {
			session.closeOnce.Do(func() {
				close(session.done)
			})
			if session.PeerConnection != nil {
				session.PeerConnection.Close()
			}
			delete(s.ingests, token)
		}
	}

	// Close all subscriber connections
	for _, room := range s.rooms {
		room.mu.Lock()
		for id, sub := range room.Subscribers {
			sub.closeOnce.Do(func() {
				close(sub.done)
			})
			if sub.PeerConnection != nil {
				sub.PeerConnection.Close()
			}
			delete(room.Subscribers, id)
		}
		room.mu.Unlock()
	}
	s.mu.Unlock()

	log.Println("SFU shutdown complete")
}

// AddSubscriber adds a subscriber to a room. If a subscriber with the same ID
// already exists (rejoin after WebSocket reconnect) the old subscriber's peer
// connection is closed and torn down *before* the new one takes its slot.
// Without this, the old PC's OnConnectionStateChange(closed) callback would
// fire later and delete the freshly-created replacement from the map — which
// is exactly what caused viewers to hang indefinitely on reconnect.
func (rt *RoomTracks) AddSubscriber(sub *Subscriber) {
	rt.mu.Lock()
	prev, replaced := rt.Subscribers[sub.ID]
	rt.Subscribers[sub.ID] = sub
	rt.mu.Unlock()

	if replaced {
		log.Printf("Replacing existing subscriber %s (rejoin); tearing down prior PC", sub.ID)
		prev.closeOnce.Do(func() {
			close(prev.done)
		})
		if prev.PeerConnection != nil {
			_ = prev.PeerConnection.Close()
		}
		// Don't decrement ActiveSubscribers here — the new sub is replacing it,
		// so net count is unchanged. The Add(1) below would otherwise double-count.
		return
	}

	metrics.Get().ActiveSubscribers.Add(1)
}

// RemoveSubscriber removes a subscriber from a room and tears down its peer
// connection. Idempotent: safe to call from OnConnectionStateChange and from
// direct code paths. Pion's close is invoked outside the map lock to avoid
// blocking sibling subscribers while teardown RTCP traffic drains.
func (rt *RoomTracks) RemoveSubscriber(id string) {
	rt.removeSubscriberIf(id, nil)
}

// removeSubscriberIfSame removes the subscriber only if the map entry for id
// points to the given sub. This prevents an old PC's terminal-state callback
// from tearing down its replacement after a participant rejoined with the
// same ID.
func (rt *RoomTracks) removeSubscriberIfSame(id string, expected *Subscriber) {
	rt.removeSubscriberIf(id, expected)
}

func (rt *RoomTracks) removeSubscriberIf(id string, expected *Subscriber) {
	rt.mu.Lock()
	sub, ok := rt.Subscribers[id]
	if !ok {
		rt.mu.Unlock()
		return
	}
	if expected != nil && sub != expected {
		// The map now holds a different subscriber (same ID, fresh connection).
		// Leave it alone — the caller is the old, stale subscriber's shadow.
		rt.mu.Unlock()
		return
	}
	delete(rt.Subscribers, id)
	// Clean up any voice state owned by this participant. The relay goroutine
	// will exit on its own when Read() returns EOF from the closed PC below.
	delete(rt.VoiceRemoteTracks, id)
	delete(rt.VoiceLocalTracks, id)
	rt.mu.Unlock()

	sub.closeOnce.Do(func() {
		close(sub.done)
	})
	if sub.PeerConnection != nil {
		_ = sub.PeerConnection.Close()
	}
	metrics.Get().ActiveSubscribers.Add(-1)
}

// BindIngestToRoom binds an ingest session's tracks to a room for distribution.
// Returns a list of subscriber IDs that need renegotiation (new tracks were added, not just replaced).
func (s *SFU) BindIngestToRoom(streamKeyToken, roomSlug string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ingest, ok := s.ingests[streamKeyToken]
	if !ok {
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

	var needsRenegotiation []string

	room.mu.Lock()
	room.VideoTrack = ingest.VideoTrack
	room.AudioTrack = ingest.AudioTrack
	room.IngestPC = ingest.PeerConnection
	for _, sub := range room.Subscribers {
		subNeedsReneg := false

		// Video track
		if sub.VideoSender != nil && ingest.VideoTrack != nil {
			if err := sub.VideoSender.ReplaceTrack(ingest.VideoTrack); err != nil {
				log.Printf("Failed to replace video track for subscriber %s: %v", sub.ID, err)
			}
		} else if sub.VideoSender == nil && ingest.VideoTrack != nil {
			// Subscriber joined before ingest — add the track now
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

		if subNeedsReneg {
			needsRenegotiation = append(needsRenegotiation, sub.ID)
		}
	}
	room.mu.Unlock()

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

// GetRoomTracksForSlug returns the room tracks if they exist
func (s *SFU) GetRoomTracksForSlug(roomSlug string) *RoomTracks {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rooms[roomSlug]
}

// CreateSubscriberConnection creates a new subscriber peer connection for a room
// Returns the peer connection and an SDP offer to send to the client.
//
// Track binding and map insertion happen under a single critical section so
// BindIngestToRoom can't rewrite room.VideoTrack/AudioTrack between us reading
// the current tracks and our AddTrack calls — otherwise the new subscriber
// could end up bound to a just-replaced (dormant) ingest track and stay frozen
// until the next OBS reconnect.
func (s *SFU) CreateSubscriberConnection(roomSlug, subscriberID string) (*webrtc.PeerConnection, *webrtc.SessionDescription, error) {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return nil, nil, fmt.Errorf("room not found: %s", roomSlug)
	}

	// Create the peer connection outside the lock — NewPeerConnection is a
	// non-trivial construction and we don't want to block BindIngestToRoom or
	// other subscribers while it runs.
	pc, err := s.CreatePeerConnection()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create peer connection: %w", err)
	}

	sub := &Subscriber{
		ID:             subscriberID,
		PeerConnection: pc,
		done:           make(chan struct{}),
	}

	// Atomic bind+insert. Any BindIngestToRoom that runs after us will find
	// the subscriber in the map and rebind via ReplaceTrack; any that ran
	// before us already committed the tracks we're about to read.
	room.mu.Lock()
	videoTrack := room.VideoTrack
	audioTrack := room.AudioTrack

	if videoTrack != nil {
		sender, addErr := pc.AddTrack(videoTrack)
		if addErr != nil {
			room.mu.Unlock()
			pc.Close()
			return nil, nil, fmt.Errorf("failed to add video track: %w", addErr)
		}
		sub.VideoSender = sender
	}
	if audioTrack != nil {
		sender, addErr := pc.AddTrack(audioTrack)
		if addErr != nil {
			room.mu.Unlock()
			pc.Close()
			return nil, nil, fmt.Errorf("failed to add audio track: %w", addErr)
		}
		sub.AudioSender = sender
	}
	// Voice relay tracks — add each existing speaker's shared relay so the
	// initial offer includes them (avoids a second renegotiation that would
	// race with the initial answer and wedge the signaling state).
	for pid, relay := range room.VoiceLocalTracks {
		if pid == subscriberID {
			continue
		}
		if _, addErr := pc.AddTrack(relay); addErr != nil {
			log.Printf("Failed to add voice relay track for %s to subscriber %s: %v", pid, subscriberID, addErr)
		}
	}

	// Displace any prior subscriber holding this ID (rejoin).
	prev, replaced := room.Subscribers[subscriberID]
	room.Subscribers[subscriberID] = sub
	room.mu.Unlock()

	if replaced {
		log.Printf("Replacing existing subscriber %s (rejoin); tearing down prior PC", subscriberID)
		prev.closeOnce.Do(func() { close(prev.done) })
		if prev.PeerConnection != nil {
			_ = prev.PeerConnection.Close()
		}
		// Metric unchanged — replacement cancels the old one out.
	} else {
		metrics.Get().ActiveSubscribers.Add(1)
	}

	// Request a keyframe from the ingest so the new subscriber can decode immediately
	go s.RequestKeyframe(roomSlug)

	// Handle connection state — only remove on terminal states.
	// "disconnected" is transient and can recover via ICE restart from the client.
	// Capture the sub pointer so that if the participant has rejoined (a new
	// subscriber already sits under this ID in the map), our terminal-state
	// callback is a no-op instead of killing the replacement.
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("Subscriber %s connection state: %s", subscriberID, state)
		switch state {
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed:
			room.removeSubscriberIfSame(subscriberID, sub)
		}
	})

	// Create offer. Error paths use identity-checked removal so a concurrent
	// rejoin that already replaced us isn't torn down by our cleanup.
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		room.removeSubscriberIfSame(subscriberID, sub)
		return nil, nil, fmt.Errorf("failed to create offer: %w", err)
	}

	if err := pc.SetLocalDescription(offer); err != nil {
		room.removeSubscriberIfSame(subscriberID, sub)
		return nil, nil, fmt.Errorf("failed to set local description: %w", err)
	}

	// Wait for ICE gathering (bounded — see iceGatherTimeout).
	waitForICEGather(pc)

	return pc, pc.LocalDescription(), nil
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

	log.Printf("Set answer for subscriber %s", subscriberID)
	return nil
}

// AddSubscriberICECandidate adds an ICE candidate from a subscriber
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
	}

	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdpOffer,
	}

	if err := sub.PeerConnection.SetRemoteDescription(offer); err != nil {
		return "", fmt.Errorf("failed to set remote description: %w", err)
	}

	answer, err := sub.PeerConnection.CreateAnswer(nil)
	if err != nil {
		return "", fmt.Errorf("failed to create answer: %w", err)
	}

	if err := sub.PeerConnection.SetLocalDescription(answer); err != nil {
		return "", fmt.Errorf("failed to set local description: %w", err)
	}

	// Wait for ICE gathering so the answer includes candidates.
	// Bounded — see iceGatherTimeout.
	waitForICEGather(sub.PeerConnection)

	log.Printf("ICE restart completed for subscriber %s", subscriberID)
	return localSDP(sub.PeerConnection)
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

	// Wait for ICE gathering
	waitForICEGather(sub.PeerConnection)

	log.Printf("Renegotiation completed for subscriber %s (rolledBack=%v)", subscriberID, rolledBack)
	sdp, err := localSDP(sub.PeerConnection)
	if err != nil {
		return "", rolledBack, err
	}
	return sdp, rolledBack, nil
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

// HandleVoiceOffer processes an offer from a client wanting to send voice audio
// Returns the SDP answer to send back to the client
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

	// Wait for ICE gathering
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
	room.mu.Unlock()

	session.closeOnce.Do(func() {
		close(session.done)
	})
	if session.PeerConnection != nil {
		_ = session.PeerConnection.Close()
	}
	log.Printf("Removed voice session for %s from room %s", participantID, roomSlug)
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
func (s *SFU) CreateVoiceRelayTrack(roomSlug, participantID string, remoteTrack *webrtc.TrackRemote) (*webrtc.TrackLocalStaticRTP, bool, error) {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return nil, false, fmt.Errorf("room not found: %s", roomSlug)
	}

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
	// lookup per packet. When a speaker rejoins, a new VoiceSession with its
	// own Muted atomic is installed; the new forwarder captures that one.
	var mutedFlag *atomic.Bool
	if vs := room.VoiceSessions[participantID]; vs != nil {
		mutedFlag = &vs.Muted
	}
	room.mu.Unlock()

	// Forwarding goroutine — reads from this session's remote track and
	// writes to the shared local track. If the speaker rejoined, the old
	// session's goroutine has already returned (Read errored on closed PC),
	// so there's no duplicate writer.
	go func() {
		buf := make([]byte, 1500)
		for {
			n, _, err := remoteTrack.Read(buf)
			if err != nil {
				log.Printf("Voice relay read ended for %s: %v", participantID, err)
				return
			}
			// Server-enforced mute gate. Dropping packets here keeps the
			// admin:mute contract honest even if a malicious client ignores
			// the client-side mute request.
			if mutedFlag != nil && mutedFlag.Load() {
				continue
			}
			if _, err := localTrack.Write(buf[:n]); err != nil {
				log.Printf("Voice relay write error for %s: %v", participantID, err)
				return
			}
		}
	}()

	if reused {
		log.Printf("Rebound voice relay for %s in room %s (speaker rejoin)", participantID, roomSlug)
	} else {
		log.Printf("Created voice relay track for %s in room %s", participantID, roomSlug)
	}
	return localTrack, !reused, nil
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

	// Guard: don't touch a PC that is already closed/failed
	if cs := sub.PeerConnection.ConnectionState(); cs == webrtc.PeerConnectionStateClosed || cs == webrtc.PeerConnectionStateFailed {
		return "", fmt.Errorf("subscriber %s connection in terminal state: %s", subscriberID, cs)
	}

	sub.SignalingMu.Lock()
	defer sub.SignalingMu.Unlock()

	// Re-check connection state after acquiring the lock.
	if cs := sub.PeerConnection.ConnectionState(); cs == webrtc.PeerConnectionStateClosed || cs == webrtc.PeerConnectionStateFailed {
		return "", fmt.Errorf("subscriber %s connection in terminal state: %s", subscriberID, cs)
	}

	// Wait for the signaling state to return to stable. Between the offer
	// going out (lock released) and its answer being applied (HandleRenegotiation-
	// Answer takes the lock again), the PC sits in have-local-offer. If we
	// fail here instead of waiting, a voice track add racing a prior
	// renegotiation is silently dropped and the subscriber never hears that
	// speaker. We release+reacquire the lock between polls so the answer
	// goroutine can make progress.
	{
		deadline := time.Now().Add(15 * time.Second)
		for sub.PeerConnection.SignalingState() != webrtc.SignalingStateStable {
			if time.Now().After(deadline) {
				return "", fmt.Errorf("subscriber %s: signaling still not stable after 15s (state=%s)", subscriberID, sub.PeerConnection.SignalingState())
			}
			sub.SignalingMu.Unlock()
			time.Sleep(50 * time.Millisecond)
			sub.SignalingMu.Lock()
			// PC may have been closed while we were waiting.
			if cs := sub.PeerConnection.ConnectionState(); cs == webrtc.PeerConnectionStateClosed || cs == webrtc.PeerConnectionStateFailed {
				return "", fmt.Errorf("subscriber %s connection in terminal state: %s", subscriberID, cs)
			}
		}
	}

	// Add the shared relay track to the subscriber's peer connection.
	// Pion fans out writes to all PCs that have added this track.
	_, err := sub.PeerConnection.AddTrack(localTrack)
	if err != nil {
		return "", fmt.Errorf("failed to add track: %w", err)
	}

	log.Printf("Added voice track from %s to subscriber %s", voiceOwnerID, subscriberID)

	// Create renegotiation offer to notify subscriber of new track
	offer, err := sub.PeerConnection.CreateOffer(nil)
	if err != nil {
		return "", fmt.Errorf("failed to create renegotiation offer: %w", err)
	}

	if err := sub.PeerConnection.SetLocalDescription(offer); err != nil {
		return "", fmt.Errorf("failed to set local description: %w", err)
	}

	// Wait for ICE gathering
	waitForICEGather(sub.PeerConnection)

	return localSDP(sub.PeerConnection)
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

	// Wait for ICE gathering
	waitForICEGather(sub.PeerConnection)

	log.Printf("Renegotiation offer created for subscriber %s", subscriberID)
	return localSDP(sub.PeerConnection)
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
