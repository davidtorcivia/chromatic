// Package webrtc implements Chromatic's SFU: WHIP ingest from OBS, viewer
// subscriptions, and the voice/screen-share/webcam relay fan-out, built on
// pion. Program-audio purity and end-to-end latency are the design axioms.
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

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/intervalpli"
	"github.com/pion/interceptor/pkg/stats"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// SFU is the Selective Forwarding Unit that manages WebRTC connections
type SFU struct {
	mu     sync.RWMutex
	config *config.Config
	api    *webrtc.API

	// Per-PC stats getters (outbound packet counters for diagnostics).
	// Guarded by statsMu, which also serializes PC creation so the
	// interceptor-factory callback maps to the right PC.
	statsMu                sync.Mutex
	statsGetters           map[*webrtc.PeerConnection]stats.Getter
	takePendingStatsGetter func() stats.Getter

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

	signalingOfferID atomic.Uint64
}

// MaxICECandidates is the maximum number of ICE candidates allowed per
// negotiation to prevent memory exhaustion from ICE candidate flooding
// attacks. The counter is reset whenever a new negotiation begins (fresh
// answer, client renegotiation offer, ICE restart) — a long session goes
// through many negotiations (TURN credential refreshes, voice/screen-share
// renegotiations, ICE restarts) and a never-resetting cap would eventually
// reject all candidates and make ICE restarts fail permanently.
const MaxICECandidates = 50

// whipCandidateWindow is the rolling window over which at most MaxICECandidates
// trickle-ICE candidates are accepted on a WHIP ingest. A lifetime cap would
// silently break connectivity on long sessions that regather candidates; this
// bounds the flooding attack rate without permanently rejecting candidates.
const whipCandidateWindow = time.Minute

// Subscriber trickle-ICE budget. Same sliding-window shape as the WHIP ingest
// above, but a much larger allowance, because a single browser ICE restart can
// legitimately produce a very large burst: on 2026-08-02 one Firefox restart
// trickled more than 50 candidates in about two seconds and the tail was
// rejected with "too many ICE candidates" — 16 candidates thrown away on a
// connection that was trying to repair itself.
//
// The count is high enough for several full regathers (host + srflx across
// IPv4/IPv6, plus the sizeable relay set Cloudflare TURN hands out) while still
// bounding a flood to a fixed rate rather than an unbounded one. The
// per-negotiation reset (resetICECandidateBudget) is kept on top of this: it
// makes the common case start from zero, and the window is the backstop for
// bursts that straddle negotiations.
const (
	maxSubscriberICECandidates = 250
	subscriberCandidateWindow  = time.Minute
)

const keyframeRequestMinInterval = 250 * time.Millisecond

var ErrStaleSubscriberAnswer = errors.New("stale subscriber answer")

var ErrStaleRenegotiationAnswer = errors.New("stale renegotiation answer")

var ErrStaleSubscriberCandidate = errors.New("stale subscriber candidate")

var ErrStalePublisherCandidate = errors.New("stale publisher candidate")

var ErrStalePublisherOffer = errors.New("stale publisher offer")

// ErrClientOfferDeferred signals that a client-initiated offer (voice offer or
// ICE restart) arrived while a server-initiated renegotiation offer was in
// flight (HaveLocalOffer). Pion v4 cannot roll back a HaveLocalOffer, so the
// client offer is queued and replayed once the in-flight server offer's answer
// settles the PC back to Stable. Callers must NOT send an immediate answer;
// the replay delivers it via OnDeferredClientOffer.
var ErrClientOfferDeferred = errors.New("client offer deferred until in-flight server offer settles")

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
	StreamKeyToken string
	PeerConnection *webrtc.PeerConnection
	VideoTrack     *webrtc.TrackLocalStaticRTP
	AudioTrack     *webrtc.TrackLocalStaticRTP
	done           chan struct{}
	closeOnce      sync.Once // Ensures done channel is closed only once
	// iceCandidateCount/iceWindowStart form a sliding-window budget that bounds
	// trickle-ICE flooding per time window (whipCandidateWindow) instead of over
	// the whole session. A monotonically-growing lifetime counter would, on a
	// long color-grading session with several network transitions / ICE
	// regatherings, eventually exhaust MaxICECandidates and reject every later
	// candidate — silently breaking connectivity for the rest of the session.
	iceCandidateCount int
	iceWindowStart    time.Time
	iceMu             sync.Mutex // Protects iceCandidateCount/iceWindowStart
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
	VoiceSessions            map[string]*VoiceSession               // Participant voice connections
	PendingPublisherICE      map[string][]PublisherICECandidate     // Publisher ICE that arrived before publish:offer.
	VoiceRemoteTracks        map[string]*webrtc.TrackRemote         // Active voice remote tracks keyed by participant ID
	VoiceLocalTracks         map[string]*webrtc.TrackLocalStaticRTP // Relay local tracks for voice fan-out
	IngestPC                 *webrtc.PeerConnection                 // Reference to ingest PC for PLI requests
	ScreenShareParticipantID string                                 // Who is currently sharing
	ScreenShareRemoteTrack   *webrtc.TrackRemote                    // Incoming screen track
	ScreenShareLocalTrack    *webrtc.TrackLocalStaticRTP            // Relay track for fan-out
	voiceRelayDone           map[string]chan struct{}               // Per-participant cancellation for voice relay goroutines
	screenShareDone          chan struct{}                          // Cancellation for the screen share relay goroutine
	lastKeyframeRequest      time.Time                              // Coalesces room-level PLI bursts.
	// voiceMuteFlags is the canonical server-enforced mute gate per participant.
	// It is intentionally separate from VoiceSessions: a voice track frequently
	// arrives (and its relay starts) BEFORE the publisher offer, at which point
	// CreateVoiceRelayTrack builds a placeholder VoiceSession. HandleVoiceOffer
	// later replaces that placeholder with a real session, so a flag living on
	// VoiceSession would be orphaned — the running relay would keep reading the
	// placeholder's flag and admin:mute would silently no-op. Keying the gate
	// here by participantID keeps the relay's captured *atomic.Bool and
	// SetVoiceMuted referencing the same object across session replacements.
	voiceMuteFlags map[string]*atomic.Bool
	// Webcam presence relays, keyed by participant ID. Unlike the single room
	// screen share, every participant may have a small cam, so these mirror the
	// voice relay maps. No mute gate — presence cams are opt-in by the owner.
	WebcamLocalTracks map[string]*webrtc.TrackLocalStaticRTP
	webcamRelayDone   map[string]chan struct{}
	// webcamTrackIDs maps participant ID -> the set of RTP track IDs the client
	// announced (via webcam:start) as webcam tracks, so the publisher OnTrack
	// handler can tell a cam video track apart from a screen-share one (both are
	// VP8 video on the same publisher PC).
	webcamTrackIDs map[string]map[string]bool
	// webcamDisabled is the admin "camera off" gate per participant. While set,
	// forwardWebcamTrack refuses to relay that participant's cam (so a client
	// can't bypass it by re-announcing), and any live relay is torn down. It
	// persists across a page refresh (admin intent) and is cleared on a
	// confirmed leave or when the admin re-enables.
	webcamDisabled map[string]bool
	// webcamHidden is the owner's fast hide/show state. The media relay remains
	// attached, but clients hide the tile until the owner shows it again.
	webcamHidden map[string]bool
}

type PublisherICECandidate struct {
	Candidate webrtc.ICECandidateInit
	OfferID   string
}

type SubscriberICECandidate struct {
	Candidate   webrtc.ICECandidateInit
	CandidateID string
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
	OnICECandidate    func(c *webrtc.ICECandidateInit, candidateID string)
	pendingCandidates []SubscriberICECandidate
	candidateMu       sync.Mutex
	// Buffer client-to-server ICE candidates that arrive before SetRemoteDescription.
	// These are flushed when the first answer/remote description is applied.
	pendingRemoteCandidates []*webrtc.ICECandidateInit
	remoteDescSet           bool
	remoteCandidateMu       sync.Mutex
	// Rate limit client-to-server ICE candidates. Sliding window (see
	// maxSubscriberICECandidates) plus a per-negotiation reset via
	// resetICECandidateBudget, so neither a long session nor a single large
	// ICE-restart burst can exhaust the allowance permanently.
	iceCandidateCount int
	iceWindowStart    time.Time
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
	OnRenegotiationNeeded func(offerSDP, offerID string)
	OfferID               string
	// CandidateID is the offer id that currently-gathering trickle ICE
	// candidates are stamped with. It is written under SignalingMu at each
	// (re)negotiation point but READ from pion's ICE-gathering goroutine
	// (OnICECandidate) and from AddSubscriberICECandidate, neither of which
	// holds SignalingMu — so it is an atomic to make those cross-goroutine
	// reads race-free. Use candidateID()/setCandidateID() rather than touching
	// it directly.
	CandidateID          atomic.Pointer[string]
	RenegotiationOfferID string
	// Deferred client offer (glare handling). Pion v4 cannot roll back a
	// HaveLocalOffer, so when a client voice offer or ICE restart arrives while
	// a server-initiated renegotiation offer is in flight, we queue the client
	// offer here and replay it once the server offer's answer returns the PC to
	// Stable (HandleRenegotiationAnswer). pendingClientOfferIsRestart records
	// whether the queued offer was an ICE restart (so the replay uses the right
	// answer transport and offerId). OnDeferredClientOffer delivers the replayed
	// answer back to the client. All guarded by SignalingMu.
	pendingClientOffer          string
	pendingClientOfferID        string
	pendingClientOfferIsRestart bool
	OnDeferredClientOffer       func(isRestart bool, offerID, answerSDP string)
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
	sub.iceWindowStart = time.Now()
	sub.remoteCandidateMu.Unlock()
}

// setCandidateID atomically records the offer id that subsequently-gathered
// trickle candidates should be stamped with. The string parameter is a fresh
// copy per call, so &id is a unique, immutable target safe to publish.
func (sub *Subscriber) setCandidateID(id string) {
	sub.CandidateID.Store(&id)
}

// candidateID atomically loads the current candidate offer id (empty before the
// first setCandidateID).
func (sub *Subscriber) candidateID() string {
	if p := sub.CandidateID.Load(); p != nil {
		return *p
	}
	return ""
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

	// NOTE: do NOT register the sdes:mid header extension here. It was tried
	// as a fix for Chrome's BUNDLE demux failures, but pion then stamps the
	// MID extension onto the relayed OBS RTP packets and Firefox/Safari stop
	// decoding them entirely (packets arrive — receiver reports advance —
	// but every frame is dropped before the decoder, with no PLI recovery).

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

	// Opus audio (for both stream audio and voice chat).
	//
	// The codec capability is the shared program-audio definition
	// (programAudioOpusCapability): stereo=1;sprop-stereo=1 with no bitrate cap,
	// so OBS's program audio stays full stereo through the opaque RTP relay and
	// is never downmixed. See opus.go for why this MUST match the WHIP ingest
	// relay track. Per-mode voice bitrate is capped client-side (TALKBACK_OPUS /
	// STUDIO_OPUS in the publisher offer, not here).
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: ProgramAudioOpusCapability(),
		PayloadType:        111,
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

	// Per-SSRC send/receive counters (pion's native GetStats does not expose
	// outbound RTP stats). The factory hands each new PC's stats getter to a
	// slot; CreatePeerConnection (serialized) claims it for the PC it just
	// built. Used to answer definitively whether the SFU is actually sending
	// video to a given subscriber.
	statsFactory, err := stats.NewInterceptor()
	if err != nil {
		return nil, fmt.Errorf("failed to create stats interceptor: %w", err)
	}
	var pendingStatsMu sync.Mutex
	var pendingStatsGetter stats.Getter
	statsFactory.OnNewPeerConnection(func(_ string, g stats.Getter) {
		pendingStatsMu.Lock()
		pendingStatsGetter = g
		pendingStatsMu.Unlock()
	})
	i.Add(statsFactory)
	takePendingStatsGetter := func() stats.Getter {
		pendingStatsMu.Lock()
		defer pendingStatsMu.Unlock()
		g := pendingStatsGetter
		pendingStatsGetter = nil
		return g
	}

	// Create API with our MediaEngine and Interceptors
	// Restrict ICE gathering to real network interfaces. With host
	// networking this box otherwise advertises ~50 candidates (every docker
	// bridge, veth, libvirt and tailscale interface), which slows ICE and
	// lets clients nominate pairs through interfaces that silently drop
	// large packets — audio (small) flows while video (full-size) vanishes.
	se := webrtc.SettingEngine{}
	se.SetInterfaceFilter(func(name string) bool {
		for _, prefix := range []string{"veth", "br-", "docker", "vnet", "virbr", "tailscale", "wg", "lxc", "cni"} {
			if strings.HasPrefix(name, prefix) {
				return false
			}
		}
		return true
	})
	if cfg.ICELoopbackOnly {
		// Test mode: gather only loopback candidates and skip mDNS so the
		// test binary never listens on a real interface (see the field doc
		// in config.Config — this is what stops the per-build OS firewall
		// prompt during `go test`).
		se.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
		se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	}

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(m),
		webrtc.WithInterceptorRegistry(i),
		webrtc.WithSettingEngine(se),
	)

	sfu := &SFU{
		config:                 cfg,
		api:                    api,
		turnMode:               cfg.TurnMode,
		externalTurnURLs:       append([]string(nil), cfg.TurnExternalURLs...),
		externalTurnUser:       cfg.TurnExternalUser,
		externalTurnPass:       cfg.TurnExternalPass,
		ingests:                make(map[string]*IngestSession),
		rooms:                  make(map[string]*RoomTracks),
		statsGetters:           make(map[*webrtc.PeerConnection]stats.Getter),
		takePendingStatsGetter: takePendingStatsGetter,
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

	// Serialize creation so the stats-interceptor factory callback (which
	// fires inside NewPeerConnection) is claimed for THIS PC. Also sweep
	// entries for closed PCs so the map stays bounded.
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	for old := range s.statsGetters {
		if old.ConnectionState() == webrtc.PeerConnectionStateClosed {
			delete(s.statsGetters, old)
		}
	}
	pc, err := s.api.NewPeerConnection(config)
	if err != nil {
		return nil, err
	}
	if s.takePendingStatsGetter != nil {
		if g := s.takePendingStatsGetter(); g != nil {
			s.statsGetters[pc] = g
		}
	}
	return pc, nil
}

// statsGetterFor returns the per-SSRC stats getter for a PC, if known.
func (s *SFU) statsGetterFor(pc *webrtc.PeerConnection) stats.Getter {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	return s.statsGetters[pc]
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

// TakeIngest atomically looks up and removes the ingest for a token, returning
// the removed session. It exists so WHIP DELETE can tear down the session it
// actually saw without a TOCTOU window: with a separate GetIngest + teardown, a
// concurrent OBS reconnect (SetIngest) can replace the session between the two,
// and the DELETE would then close a stale PC and return a misleading 204 for a
// session that no longer matches. By taking under one lock, the caller is
// guaranteed either the exact session it removes (then nil) or nothing. The
// caller is responsible for closing the PC and running teardown.
func (s *SFU) TakeIngest(streamKeyToken string) *IngestSession {
	s.mu.Lock()
	session := s.ingests[streamKeyToken]
	delete(s.ingests, streamKeyToken)
	s.mu.Unlock()
	return session
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
		RoomSlug:            roomSlug,
		Subscribers:         make(map[string]*Subscriber),
		PendingPublisherICE: make(map[string][]PublisherICECandidate),
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
		for id, ch := range room.webcamRelayDone {
			close(ch)
			delete(room.webcamRelayDone, id)
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

// CloseRoom tears down all WebRTC state for one room: subscribers, voice
// sessions, relays, screen share, webcams, and the currently bound OBS ingest.
// PeerConnections are closed after locks are released because Pion callbacks
// re-enter SFU/room locks during Close.
func (s *SFU) CloseRoom(roomSlug string) {
	var pcs []*webrtc.PeerConnection
	var ingests []*IngestSession

	s.mu.Lock()
	room := s.rooms[roomSlug]
	if room == nil {
		s.mu.Unlock()
		return
	}
	delete(s.rooms, roomSlug)

	room.mu.Lock()
	ingestPC := room.IngestPC
	if ingestPC != nil {
		for _, session := range s.ingests {
			if session != nil && session.PeerConnection == ingestPC {
				ingests = append(ingests, session)
			}
		}
	}

	subIDs := make([]string, 0, len(room.Subscribers))
	for id := range room.Subscribers {
		subIDs = append(subIDs, id)
	}
	for _, id := range subIDs {
		subPCs, _ := room.removeSubscriberLocked(id)
		pcs = append(pcs, subPCs...)
	}

	for id, vs := range room.VoiceSessions {
		vs.closeOnce.Do(func() {
			close(vs.done)
		})
		if vs.PeerConnection != nil {
			pcs = append(pcs, vs.PeerConnection)
		}
		delete(room.VoiceSessions, id)
	}
	for id, ch := range room.voiceRelayDone {
		close(ch)
		delete(room.voiceRelayDone, id)
	}
	for id, ch := range room.webcamRelayDone {
		close(ch)
		delete(room.webcamRelayDone, id)
	}
	if room.screenShareDone != nil {
		close(room.screenShareDone)
		room.screenShareDone = nil
	}

	room.VideoTrack = nil
	room.AudioTrack = nil
	room.IngestPC = nil
	room.ScreenShareParticipantID = ""
	room.ScreenShareRemoteTrack = nil
	room.ScreenShareLocalTrack = nil
	room.PendingPublisherICE = nil
	room.VoiceRemoteTracks = nil
	room.VoiceLocalTracks = nil
	room.voiceMuteFlags = nil
	room.WebcamLocalTracks = nil
	room.webcamTrackIDs = nil
	room.webcamDisabled = nil
	room.webcamHidden = nil
	room.mu.Unlock()
	s.mu.Unlock()

	for _, session := range ingests {
		if session.teardown != nil {
			session.teardown()
			continue
		}
		session.closeOnce.Do(func() {
			close(session.done)
		})
		if session.PeerConnection != nil {
			_ = session.PeerConnection.Close()
		}
	}
	for _, pc := range pcs {
		_ = pc.Close()
	}
	log.Printf("Closed WebRTC room %s", roomSlug)
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
// and the removal. Returns the IDs of other subscribers affected by a
// screen-share sender teardown (when the removed subscriber was the sharer).
func (rt *RoomTracks) removeSubscriberIfSame(id string, expected *Subscriber) []string {
	rt.mu.Lock()
	current, ok := rt.Subscribers[id]
	if !ok || current != expected {
		rt.mu.Unlock()
		return nil
	}
	pcs, deadTracks := rt.removeSubscriberLocked(id)
	rt.mu.Unlock()

	// Close outside the lock — pion state callbacks re-enter rt.mu.
	for _, pc := range pcs {
		pc.Close()
	}
	// Tear the departed participant's dead relay tracks out of the remaining
	// subscribers under their SignalingMu (never under rt.mu), and return the
	// affected IDs for the caller to renegotiate.
	return rt.removeSendersForTracks(deadTracks, id)
}

// RemoveSubscriber removes a subscriber from a room unconditionally.
func (rt *RoomTracks) RemoveSubscriber(id string) {
	rt.mu.Lock()
	pcs, deadTracks := rt.removeSubscriberLocked(id)
	rt.mu.Unlock()

	// Close outside the lock — pion state callbacks re-enter rt.mu.
	for _, pc := range pcs {
		pc.Close()
	}
	// Detach the departed participant's dead relay tracks from the remaining
	// subscribers (flags them for renegotiation under SignalingMu).
	rt.removeSendersForTracks(deadTracks, id)
}

// removeSubscriberLocked performs subscriber removal. Caller must hold rt.mu
// write lock and is responsible for closing the returned PeerConnections AFTER
// releasing the lock (pion fires OnConnectionStateChange callbacks during
// Close that call back into rt.mu, e.g. via removeSubscriberIfSame).
//
// When the removed subscriber was the screen sharer or a webcam owner, its
// relay local track(s) become dead and must be torn down from every OTHER
// subscriber, otherwise viewers keep a frozen last frame bound to a dead track.
// The abrupt path (subscriber PC Failed while the WS survives, or any removal
// that clears ScreenShareLocalTrack before RemoveScreenShareTrack runs) reaches
// here, and RemoveScreenShareTrack alone can't fix it after the local track is
// already nil (it early-returns).
//
// Those dead tracks are RETURNED rather than removed here: removing a sender and
// flagging renegotiation mutate another subscriber's PeerConnection and
// needsRenegotiation, both guarded by that subscriber's SignalingMu — which we
// must NOT take while holding rt.mu (every other path takes SignalingMu alone;
// inconsistent ordering would risk deadlock). The caller passes the returned
// tracks to removeSendersForTracks AFTER releasing rt.mu.
func (rt *RoomTracks) removeSubscriberLocked(id string) (pcs []*webrtc.PeerConnection, deadTracks []*webrtc.TrackLocalStaticRTP) {
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
	delete(rt.voiceMuteFlags, id)
	delete(rt.VoiceRemoteTracks, id)
	delete(rt.VoiceLocalTracks, id)
	if ch, ok := rt.voiceRelayDone[id]; ok {
		close(ch)
		delete(rt.voiceRelayDone, id)
	}
	// Always drop any announced cam track ids for the departing participant —
	// even when no relay was ever created (a webcam:start whose cam never
	// arrived would otherwise leave a permanent entry).
	delete(rt.webcamTrackIDs, id)
	delete(rt.webcamHidden, id)
	// Confirmed leave clears the admin camera gate (it intentionally persists
	// across a page refresh, but not across a real departure/rejoin).
	delete(rt.webcamDisabled, id)
	// Clean up this participant's webcam relay and remove its sender from every
	// other subscriber so they don't keep a frozen cam tile bound to a dead
	// track. Mirrors the abrupt screen-share teardown below.
	if webcamLocal, ok := rt.WebcamLocalTracks[id]; ok {
		delete(rt.WebcamLocalTracks, id)
		if ch, ok := rt.webcamRelayDone[id]; ok {
			close(ch)
			delete(rt.webcamRelayDone, id)
		}
		if webcamLocal != nil {
			// Torn down from every other subscriber by the caller under each
			// subscriber's SignalingMu (see removeSendersForTracks); doing it
			// here would touch another subscriber's PeerConnection/
			// needsRenegotiation without its SignalingMu.
			deadTracks = append(deadTracks, webcamLocal)
		}
	}
	// Clean up screen share state if this participant was sharing. Remove the
	// sender from every other subscriber BEFORE nulling the local track, so
	// viewers don't keep a frozen frame bound to the now-dead relay track. This
	// covers the abrupt path that RemoveScreenShareTrack can't reach once the
	// local track is gone.
	if rt.ScreenShareParticipantID == id {
		sharerLocalTrack := rt.ScreenShareLocalTrack
		rt.ScreenShareParticipantID = ""
		rt.ScreenShareRemoteTrack = nil
		rt.ScreenShareLocalTrack = nil
		if rt.screenShareDone != nil {
			close(rt.screenShareDone)
			rt.screenShareDone = nil
		}
		if sharerLocalTrack != nil {
			// Removed from other subscribers by the caller under SignalingMu
			// (see removeSendersForTracks), for the same reason as the webcam
			// track above.
			deadTracks = append(deadTracks, sharerLocalTrack)
		}
	}
	return pcs, deadTracks
}

// removeSendersForTracks removes, from every subscriber except excludeID, any
// sender whose track is one of deadTracks (a departed participant's now-dead
// webcam/screen-share relay tracks), flags each touched subscriber for
// renegotiation, and returns the affected IDs so the caller can offer.
//
// It takes each subscriber's SignalingMu — and never rt.mu — so it MUST be
// called after rt.mu is released. This mirrors RemoveScreenShareTrack and keeps
// needsRenegotiation / PeerConnection mutations under the one lock that guards
// them, closing the race with concurrent track-add renegotiation.
func (rt *RoomTracks) removeSendersForTracks(deadTracks []*webrtc.TrackLocalStaticRTP, excludeID string) []string {
	if len(deadTracks) == 0 {
		return nil
	}

	rt.mu.RLock()
	subs := make([]*Subscriber, 0, len(rt.Subscribers))
	ids := make([]string, 0, len(rt.Subscribers))
	for sid, sub := range rt.Subscribers {
		if sid == excludeID {
			continue
		}
		subs = append(subs, sub)
		ids = append(ids, sid)
	}
	rt.mu.RUnlock()

	var affected []string
	for i, sub := range subs {
		sub.SignalingMu.Lock()
		removed := false
		for _, sender := range sub.PeerConnection.GetSenders() {
			track := sender.Track()
			if track == nil {
				continue
			}
			isDead := false
			for _, dead := range deadTracks {
				if track == dead {
					isDead = true
					break
				}
			}
			if !isDead {
				continue
			}
			if err := sub.PeerConnection.RemoveTrack(sender); err != nil {
				log.Printf("Failed to remove dead relay sender from %s during participant removal: %v", ids[i], err)
			} else {
				removed = true
			}
		}
		if removed {
			sub.needsRenegotiation = true
			affected = append(affected, ids[i])
		}
		sub.SignalingMu.Unlock()
	}
	return affected
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
				// A swallowed ReplaceTrack failure leaves the subscriber's
				// sender bound to the prior (possibly dead) track and no
				// renegotiation ever re-establishes media — the viewer stays
				// frozen. Recover by dropping the stale sender and re-adding
				// the new track, which forces a renegotiation offer.
				log.Printf("ReplaceTrack failed for video (%s); falling back to re-add: %v", sub.ID, err)
				if rmErr := sub.PeerConnection.RemoveTrack(sub.VideoSender); rmErr != nil {
					log.Printf("Failed to remove stale video sender for %s: %v", sub.ID, rmErr)
				}
				sender, addErr := sub.PeerConnection.AddTrack(ingest.VideoTrack)
				if addErr != nil {
					log.Printf("Failed to re-add video track to subscriber %s: %v", sub.ID, addErr)
				} else {
					sub.VideoSender = sender
					subNeedsReneg = true
				}
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
				log.Printf("ReplaceTrack failed for audio (%s); falling back to re-add: %v", sub.ID, err)
				if rmErr := sub.PeerConnection.RemoveTrack(sub.AudioSender); rmErr != nil {
					log.Printf("Failed to remove stale audio sender for %s: %v", sub.ID, rmErr)
				}
				sender, addErr := sub.PeerConnection.AddTrack(ingest.AudioTrack)
				if addErr != nil {
					log.Printf("Failed to re-add audio track to subscriber %s: %v", sub.ID, addErr)
				} else {
					sub.AudioSender = sender
					subNeedsReneg = true
				}
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

	room.mu.Lock()
	ingestPC := room.IngestPC
	if ingestPC == nil {
		room.mu.Unlock()
		return
	}
	if !room.markKeyframeRequestLocked(time.Now()) {
		room.mu.Unlock()
		return
	}
	room.mu.Unlock()

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

func (rt *RoomTracks) markKeyframeRequestLocked(now time.Time) bool {
	if !rt.lastKeyframeRequest.IsZero() && now.Sub(rt.lastKeyframeRequest) < keyframeRequestMinInterval {
		return false
	}
	rt.lastKeyframeRequest = now
	return true
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
	pc := room.screenShareKeyframePeerConnectionLocked(sharerID)
	room.mu.RUnlock()

	if sharerID == "" || pc == nil {
		return
	}

	// Find the video receiver on the sharer's PC (the screen share track)
	for _, receiver := range pc.GetReceivers() {
		track := receiver.Track()
		if track != nil && track.Kind() == webrtc.RTPCodecTypeVideo {
			if err := pc.WriteRTCP([]rtcp.Packet{
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

// screenShareKeyframePeerConnectionLocked returns the PeerConnection that
// receives the sharer's screen-share RTP. New clients publish screen share on
// the dedicated publisher PC stored in VoiceSessions; the subscriber fallback
// supports older in-place publishing paths.
// Caller must hold room.mu for reading or writing.
func (rt *RoomTracks) screenShareKeyframePeerConnectionLocked(sharerID string) *webrtc.PeerConnection {
	if sharerID == "" {
		return nil
	}
	if vs := rt.VoiceSessions[sharerID]; vs != nil && vs.PeerConnection != nil {
		return vs.PeerConnection
	}
	if sub := rt.Subscribers[sharerID]; sub != nil {
		return sub.PeerConnection
	}
	return nil
}

// RequestWebcamKeyframe sends a PLI to a participant's publisher PC for their
// webcam track, forcing an immediate keyframe so a freshly-subscribed (or
// renegotiated) viewer can decode the cam right away instead of sitting on a
// black tile until the next natural keyframe. A cam shares the publisher PC
// with the (optional) screen share, so we PLI only the receiver whose track id
// the client announced as a webcam (webcam:start) — never the screen share's.
func (s *SFU) RequestWebcamKeyframe(roomSlug, participantID string) {
	if participantID == "" {
		return
	}
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return
	}

	room.mu.RLock()
	// The webcam publishes on the same PC as screen share / voice.
	pc := room.screenShareKeyframePeerConnectionLocked(participantID)
	camIDs := room.webcamTrackIDs[participantID]
	room.mu.RUnlock()

	if pc == nil || len(camIDs) == 0 {
		return
	}

	for _, receiver := range pc.GetReceivers() {
		track := receiver.Track()
		if track == nil || track.Kind() != webrtc.RTPCodecTypeVideo {
			continue
		}
		if !camIDs[track.ID()] {
			continue // a screen-share video receiver on the same PC — skip
		}
		if err := pc.WriteRTCP([]rtcp.Packet{
			&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())},
		}); err != nil {
			log.Printf("Failed to send PLI for webcam %s in room %s: %v", participantID, roomSlug, err)
		} else {
			log.Printf("Sent PLI for webcam keyframe (%s) in room %s", participantID, roomSlug)
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

// RemoveSubscriberIfSame removes a subscriber only if the current room entry
// still points at the expected subscriber instance. Disconnect cleanup uses
// this to release a confirmed-leaving viewer without tearing down a fast
// reconnect that already replaced the subscriber with the same participant ID.
//
// Returns the IDs of other subscribers whose screen-share sender was torn down
// because the removed viewer was the active sharer; callers renegotiate those.
func (s *SFU) RemoveSubscriberIfSame(roomSlug, subscriberID string, expected *Subscriber) []string {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil || expected == nil {
		return nil
	}
	return room.removeSubscriberIfSame(subscriberID, expected)
}

// CreateSubscriberConnection creates a new subscriber peer connection for a room.
// Returns the peer connection, subscriber record, SDP offer string and offer ID
// to send to the client.
// ICE candidates are NOT included in the offer — they are trickled via
// EnableSubscriberTrickleICE after the offer has been sent to the client.
func (s *SFU) CreateSubscriberConnection(roomSlug, subscriberID string) (*webrtc.PeerConnection, *Subscriber, string, string, error) {
	// Materialize the room on demand: subscribers are now created on
	// connect, before any ingest (or voice path) has built the RoomTracks
	// entry — the caller has already validated the join.
	room := s.GetRoomTracks(roomSlug)
	if room == nil {
		return nil, nil, "", "", fmt.Errorf("room not found: %s", roomSlug)
	}

	// Create the peer connection outside the lock — NewPeerConnection is a
	// non-trivial construction and we don't want to block BindIngestToRoom or
	// other subscribers while it runs.
	pc, err := s.CreatePeerConnection()
	if err != nil {
		return nil, nil, "", "", fmt.Errorf("failed to create peer connection: %w", err)
	}

	// Create subscriber record
	offerID := s.nextSignalingOfferID()
	sub := &Subscriber{
		ID:             subscriberID,
		PeerConnection: pc,
		done:           make(chan struct{}),
		OfferID:        offerID,
	}
	sub.setCandidateID(offerID)

	// Set up trickle ICE: buffer candidates until EnableSubscriberTrickleICE is called.
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		candidateID := sub.candidateID()
		sub.candidateMu.Lock()
		if sub.OnICECandidate != nil {
			cb := sub.OnICECandidate
			sub.candidateMu.Unlock()
			cb(&init, candidateID)
		} else {
			sub.pendingCandidates = append(sub.pendingCandidates, SubscriberICECandidate{Candidate: init, CandidateID: candidateID})
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
			return nil, nil, "", "", fmt.Errorf("failed to add video track: %w", addErr)
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
			return nil, nil, "", "", fmt.Errorf("failed to add audio track: %w", addErr)
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

	// Add existing webcam relay tracks so late joiners see active cams in the
	// initial offer (avoids a separate renegotiation racing the first exchange).
	var activeWebcamPIDs []string
	for pid, webcamTrack := range room.WebcamLocalTracks {
		if pid == subscriberID {
			continue
		}
		if _, addErr := pc.AddTrack(webcamTrack); addErr != nil {
			log.Printf("Failed to add webcam relay track for %s to subscriber %s: %v", pid, subscriberID, addErr)
		} else {
			activeWebcamPIDs = append(activeWebcamPIDs, pid)
			log.Printf("Added existing webcam relay track to subscriber %s", subscriberID)
		}
	}

	// Pre-stream there may be no tracks at all, and an SDP without a
	// single m-line carries no ICE credentials (clients reject it with
	// "no ice-ufrag"). A recvonly audio transceiver guarantees a valid
	// offer and matches this PC's design — it already receives the
	// client's voice (see OnTrack in the ws handler); later relay
	// AddTrack calls upgrade it to sendrecv.
	if room.VideoTrack == nil && room.AudioTrack == nil && len(room.VoiceLocalTracks) == 0 && len(room.WebcamLocalTracks) == 0 {
		if _, trErr := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		}); trErr != nil {
			log.Printf("Failed to add placeholder audio transceiver for subscriber %s: %v", subscriberID, trErr)
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
	// Same for any cams already on: nudge each owner so the joiner's cam tiles
	// fill in immediately instead of sitting black until the next keyframe.
	for _, pid := range activeWebcamPIDs {
		pid := pid
		go s.RequestWebcamKeyframe(roomSlug, pid)
	}

	// Handle connection state — only remove on terminal states.
	// "disconnected" is transient and can recover via ICE restart from the client.
	// Capture `sub` so we only remove if this specific subscriber is still current
	// (prevents a stale callback from removing a replacement subscriber on reconnect).
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("Subscriber %s connection state: %s", subscriberID, state)
		switch state {
		case webrtc.PeerConnectionStateConnected:
			logSelectedICEPair("subscriber", subscriberID, pc)
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
		return nil, nil, "", "", fmt.Errorf("failed to create offer: %w", err)
	}

	// Set local description — starts ICE gathering in the background.
	// Candidates are buffered and sent via trickle ICE (no blocking wait).
	if err := pc.SetLocalDescription(offer); err != nil {
		room.removeSubscriberIfSame(subscriberID, sub)
		pc.Close()
		return nil, nil, "", "", fmt.Errorf("failed to set local description: %w", err)
	}

	// Diagnostic: read the viewer's RTCP for the video stream. Receiver
	// reports with an advancing highest-sequence prove packets arrive (a
	// black screen is then a decode problem); absent/stuck reports mean the
	// transport never delivers video at all.
	// Snapshot the sender under the SignalingMu held here and hand it to the
	// diagnostic goroutine. Re-reading sub.VideoSender inside the loop would
	// race BindIngestToRoom, which reassigns the field (under SignalingMu) on
	// the ReplaceTrack-failure rebind path.
	if videoSender := sub.VideoSender; videoSender != nil {
		go logSubscriberVideoRTCP(sub, videoSender, subscriberID)
	}

	return pc, sub, offer.SDP, sub.OfferID, nil
}

func (s *SFU) nextSignalingOfferID() string {
	return fmt.Sprintf("%d", s.signalingOfferID.Add(1))
}

// logSubscriberVideoRTCP logs a bounded number of RTCP receiver reports and
// PLI requests from a subscriber's video stream, then exits. Purely
// diagnostic — used to tell "not receiving video" apart from "receiving but
// not decoding" (e.g. Chrome-only black screens).
func logSubscriberVideoRTCP(sub *Subscriber, sender *webrtc.RTPSender, subscriberID string) {
	const maxReports = 8
	logged := 0
	pliCount := 0
	nackCount := 0
	nackPackets := 0
	start := time.Now()
	for logged < maxReports && time.Since(start) < 2*time.Minute {
		select {
		case <-sub.done:
			return
		default:
		}
		pkts, _, err := sender.ReadRTCP()
		if err != nil {
			return
		}
		for _, p := range pkts {
			switch r := p.(type) {
			case *rtcp.ReceiverReport:
				for _, rep := range r.Reports {
					log.Printf("Subscriber %s video RR: highestSeq=%d totalLost=%d fractionLost=%d jitter=%d (PLIs: %d, NACKs: %d for %d pkts)",
						subscriberID, rep.LastSequenceNumber, rep.TotalLost, rep.FractionLost, rep.Jitter, pliCount, nackCount, nackPackets)
					logged++
				}
			case *rtcp.PictureLossIndication:
				pliCount++
				if pliCount <= 3 || pliCount%20 == 0 {
					log.Printf("Subscriber %s sent video PLI #%d (decoder waiting on a usable keyframe)", subscriberID, pliCount)
				}
			case *rtcp.TransportLayerNack:
				// Counts whether viewers ASK for retransmissions — if these
				// stay zero while totalLost climbs, loss recovery is dead and
				// every oversized keyframe arrives corrupted.
				nackCount++
				for _, pair := range r.Nacks {
					nackPackets += 1 + popCount16(uint16(pair.LostPackets))
				}
			}
		}
	}
}

func popCount16(v uint16) int {
	n := 0
	for v != 0 {
		n += int(v & 1)
		v >>= 1
	}
	return n
}

// EnableSubscriberTrickleICE sets the ICE candidate callback for a subscriber
// and flushes any candidates that were buffered before the offer was sent.
// Must be called AFTER the offer SDP has been sent to the client to ensure
// correct message ordering (offer arrives before candidates).
func (s *SFU) EnableSubscriberTrickleICE(roomSlug, subscriberID string, onICECandidate func(*webrtc.ICECandidateInit, string)) {
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
		onICECandidate(&c.Candidate, c.CandidateID)
	}
}

// SetSubscriberAnswer sets the answer from a subscriber client.
// When offerID is present, it must match the currently pending initial offer;
// otherwise this is a delayed answer for a replaced subscriber and must not be
// applied to the live PeerConnection.
func (s *SFU) SetSubscriberAnswer(roomSlug, subscriberID string, answer webrtc.SessionDescription, offerID string) error {
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

	if offerID != "" {
		if sub.OfferID == "" {
			return fmt.Errorf("%w: got %s, no initial offer pending", ErrStaleSubscriberAnswer, offerID)
		}
		if offerID != sub.OfferID {
			return fmt.Errorf("%w: got %s, want %s", ErrStaleSubscriberAnswer, offerID, sub.OfferID)
		}
	}

	// An answer only applies to a PC that is still waiting for one. Without
	// this, an answer arriving after the PC already settled (a duplicate, or a
	// client that omits offerID and therefore skips the check above) reaches
	// pion as stable->SetRemote(answer) and surfaces as an ERROR-level
	// InvalidModificationError — alarming in the log, but nothing is actually
	// wrong: the negotiation it belonged to is simply over. HandleRenegotiationAnswer
	// has carried this same guard; the initial-answer path was missing it.
	if state := sub.PeerConnection.SignalingState(); state != webrtc.SignalingStateHaveLocalOffer {
		log.Printf("Ignoring stale subscriber answer for %s (state: %s)", subscriberID, state)
		return nil
	}

	if err := sub.PeerConnection.SetRemoteDescription(answer); err != nil {
		return fmt.Errorf("failed to set remote description: %w", err)
	}
	if offerID != "" {
		sub.OfferID = ""
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

	// Diagnostic: inspect what the browser actually accepted in its answer.
	// A rejected video m-line (port 0) or an H.264-free codec list means the
	// browser cannot decode our stream at all — silent black video with
	// working audio (seen with Chromium builds lacking proprietary codecs).
	summary := summarizeVideoAnswer(answer.SDP)
	if !strings.Contains(summary, "H264") {
		log.Printf("WARNING: subscriber %s answer accepts no H264 video — stream will be black for this browser (%s)", subscriberID, summary)
	} else {
		log.Printf("Subscriber %s answer video: %s", subscriberID, summary)
	}

	log.Printf("Set answer for subscriber %s (flushed %d buffered candidates)", subscriberID, len(pending))

	// One-shot probe: 10s in, log what pion actually SENT to this
	// subscriber. Distinguishes "server never sends video to this browser"
	// (binding bug here) from "sent but lost/dropped client-side".
	go s.probeOutboundVideo(sub, subscriberID)

	return nil
}

// probeOutboundVideo logs per-SSRC outbound packet counts for every video
// sender on a subscriber, 10s after a negotiation completes.
func (s *SFU) probeOutboundVideo(sub *Subscriber, subscriberID string) {
	select {
	case <-sub.done:
		return
	case <-time.After(10 * time.Second):
	}

	getter := s.statsGetterFor(sub.PeerConnection)
	if getter == nil {
		log.Printf("Subscriber %s: no stats getter for outbound probe", subscriberID)
		return
	}
	for _, sender := range sub.PeerConnection.GetSenders() {
		track := sender.Track()
		if track == nil || track.Kind() != webrtc.RTPCodecTypeVideo {
			continue
		}
		params := sender.GetParameters()
		if len(params.Encodings) == 0 {
			log.Printf("Subscriber %s outbound video %q: NO ENCODINGS (track unbound — browser cannot receive it)", subscriberID, track.ID())
			continue
		}
		ssrc := uint32(params.Encodings[0].SSRC)
		st := getter.Get(ssrc)
		if st == nil {
			log.Printf("Subscriber %s outbound video %q ssrc=%d: NO STATS — zero packets ever sent on this track", subscriberID, track.ID(), ssrc)
			continue
		}
		log.Printf("Subscriber %s outbound video %q ssrc=%d: packetsSent=%d bytesSent=%d nackRecv=%d pliRecv=%d",
			subscriberID, track.ID(), ssrc,
			st.OutboundRTPStreamStats.PacketsSent, st.OutboundRTPStreamStats.BytesSent,
			st.OutboundRTPStreamStats.NACKCount, st.OutboundRTPStreamStats.PLICount)
	}
}

// summarizeVideoAnswer extracts the first video m-line's port, direction,
// and payload-type→codec/profile mapping from an SDP answer for diagnostics.
func summarizeVideoAnswer(sdp string) string {
	inVideo := false
	seenVideo := false
	port := ""
	direction := "?"
	var codecs []string
	fmtps := map[string]string{}
	var order []string
	rtpmaps := map[string]string{}
	for _, ln := range strings.Split(sdp, "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "m=") {
			if strings.HasPrefix(ln, "m=video") && !seenVideo {
				inVideo = true
				seenVideo = true
				if fields := strings.Fields(ln); len(fields) > 1 {
					port = fields[1]
				}
			} else {
				inVideo = false
			}
			continue
		}
		if !inVideo {
			continue
		}
		switch {
		case strings.HasPrefix(ln, "a=rtpmap:"):
			rest := ln[len("a=rtpmap:"):]
			if idx := strings.Index(rest, " "); idx > 0 {
				pt := rest[:idx]
				rtpmaps[pt] = strings.SplitN(rest[idx+1:], "/", 2)[0]
				order = append(order, pt)
			}
		case strings.HasPrefix(ln, "a=fmtp:"):
			rest := ln[len("a=fmtp:"):]
			if idx := strings.Index(rest, " "); idx > 0 {
				fmtps[rest[:idx]] = rest[idx+1:]
			}
		case ln == "a=recvonly" || ln == "a=sendrecv" || ln == "a=sendonly" || ln == "a=inactive":
			direction = ln[2:]
		}
	}
	if !seenVideo {
		return "no video m-line"
	}
	for _, pt := range order {
		entry := pt + ":" + rtpmaps[pt]
		if f := fmtps[pt]; f != "" {
			if m := strings.Index(f, "profile-level-id="); m >= 0 && m+17+6 <= len(f) {
				entry += "/" + f[m+17:m+17+6]
			}
		}
		codecs = append(codecs, entry)
	}
	return fmt.Sprintf("port=%s dir=%s codecs=%v", port, direction, codecs)
}

// AddSubscriberICECandidate adds an ICE candidate from a subscriber.
// If the remote description hasn't been set yet, the candidate is buffered
// and will be applied when SetSubscriberAnswer is called.
func (s *SFU) AddSubscriberICECandidate(roomSlug, subscriberID string, candidate webrtc.ICECandidateInit, candidateID string) error {
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

	if currentID := sub.candidateID(); candidateID != "" && currentID != "" && candidateID != currentID {
		return fmt.Errorf("%w: got %s, want %s", ErrStaleSubscriberCandidate, candidateID, currentID)
	}

	// Enforce ICE candidate limit to prevent flooding attacks. Sliding window:
	// a legitimate regather (ICE restart, network transition) refills rather
	// than being permanently penalised by earlier negotiations.
	now := time.Now()
	sub.remoteCandidateMu.Lock()
	if sub.iceWindowStart.IsZero() || now.Sub(sub.iceWindowStart) >= subscriberCandidateWindow {
		sub.iceWindowStart = now
		sub.iceCandidateCount = 0
	}
	if sub.iceCandidateCount >= maxSubscriberICECandidates {
		sub.remoteCandidateMu.Unlock()
		return fmt.Errorf("too many ICE candidates from subscriber %s", subscriberID)
	}
	sub.iceCandidateCount++
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
func (s *SFU) HandleIceRestart(roomSlug, subscriberID, sdpOffer, offerID string) (string, error) {
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

	// Glare: an ICE restart arrived while a server-initiated renegotiation
	// offer is in flight (HaveLocalOffer). Pion v4 cannot roll back a
	// HaveLocalOffer, so we defer the restart exactly like a client voice offer:
	// it is replayed once the in-flight server offer's answer settles the PC.
	// The ICE restart's urgency (the connection is failing) is still served —
	// the restart applies as soon as the server offer resolves, and the pending
	// server offer's renegotiation (e.g. a voice track add) completes in the
	// meantime rather than being destroyed by a rollback that can't happen.
	if sub.PeerConnection.SignalingState() == webrtc.SignalingStateHaveLocalOffer {
		log.Printf("Deferring ICE restart for subscriber %s (server offer in flight)", subscriberID)
		sub.pendingClientOffer = sdpOffer
		sub.pendingClientOfferID = offerID
		sub.pendingClientOfferIsRestart = true
		return "", ErrClientOfferDeferred
	}

	// Set the new offer from the client
	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdpOffer,
	}

	if err := sub.PeerConnection.SetRemoteDescription(offer); err != nil {
		return "", fmt.Errorf("failed to set remote description: %w", err)
	}

	sub.setCandidateID(offerID)

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

	// Glare: a client voice offer arrived while a server-initiated renegotiation
	// offer is in flight (HaveLocalOffer). Pion v4 rejects SetLocalDescription
	// (rollback) from HaveLocalOffer, so we CANNOT roll the server offer back to
	// accept this one inline. Instead, defer the client offer: once the client
	// answers our pending server offer (HandleRenegotiationAnswer), the PC
	// returns to Stable and the deferred client offer is replayed there. Without
	// this, the rollback would error out and the client's mic-enable would be
	// silently dropped for the rest of the session.
	if sub.PeerConnection.SignalingState() == webrtc.SignalingStateHaveLocalOffer {
		log.Printf("Deferring client voice offer for subscriber %s (server offer in flight)", subscriberID)
		sub.pendingClientOffer = sdpOffer
		sub.pendingClientOfferID = ""
		sub.pendingClientOfferIsRestart = false
		return "", false, ErrClientOfferDeferred
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
	log.Printf("Voice renegotiation completed for subscriber %s", subscriberID)
	return answer.SDP, false, nil
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
	// publisherOfferID identifies the most recent publisher offer for this
	// session (stale-offer/candidate rejection). It is read on paths that hold
	// room.mu and written on paths that hold SignalingMu — two different locks
	// that must not nest in that order — so access is atomic instead of being
	// guarded by either. Use PublisherOfferID()/SetPublisherOfferID().
	publisherOfferID atomic.Value // string
	done             chan struct{}
	closeOnce        sync.Once
	SignalingMu      sync.Mutex
	// Muted is the server-enforced gate for this participant's voice RTP.
	// When true, the voice relay forwarder drops incoming packets instead of
	// writing them to the local track — so admin:mute is effective even if a
	// malicious client ignores the client-side mute request.
	Muted atomic.Bool
}

// PublisherOfferID returns the ID of the most recent publisher offer, or ""
// if none has been recorded.
func (vs *VoiceSession) PublisherOfferID() string {
	id, _ := vs.publisherOfferID.Load().(string)
	return id
}

// SetPublisherOfferID records the ID of the publisher offer currently being
// negotiated so later candidates/aborts for a superseded offer are rejected.
func (vs *VoiceSession) SetPublisherOfferID(id string) {
	vs.publisherOfferID.Store(id)
}

// logSelectedICEPair inspects the PC's stats for the nominated ICE candidate
// pair and logs the local/remote candidate types (host/srflx/prflx/relay),
// protocol and addresses. Used to tell a direct LAN path apart from a
// Cloudflare TURN relay path when diagnosing high-bitrate media loss.
func logSelectedICEPair(tag, participantID string, pc *webrtc.PeerConnection) {
	if pc == nil {
		return
	}
	stats := pc.GetStats()
	cands := make(map[string]webrtc.ICECandidateStats)
	for _, st := range stats {
		if c, ok := st.(webrtc.ICECandidateStats); ok {
			cands[c.ID] = c
		}
	}
	for _, st := range stats {
		p, ok := st.(webrtc.ICECandidatePairStats)
		if !ok || !p.Nominated {
			continue
		}
		l := cands[p.LocalCandidateID]
		r := cands[p.RemoteCandidateID]
		relay := ""
		if l.RelayProtocol != "" {
			relay = " relayProto=" + l.RelayProtocol
		}
		log.Printf("ICE selected pair [%s %s]: local=%s/%s %s:%d%s remote=%s/%s %s:%d state=%s",
			tag, participantID,
			l.CandidateType, l.Protocol, l.IP, l.Port, relay,
			r.CandidateType, r.Protocol, r.IP, r.Port, p.State)
		return
	}
	log.Printf("ICE selected pair [%s %s]: none nominated yet", tag, participantID)
}

// HandlePublisherOffer processes an offer on a participant's dedicated
// publisher peer connection (microphone + screen share). The client is
// ALWAYS the offerer on this PC and the server only answers; combined with
// the server-offer-only subscriber PC this removes signaling glare entirely
// (Chrome's demuxer-criteria crashes and Safari's unrecoverable "Failed to
// set SSL role" wedges both stem from mixing offer directions on one PC).
// An offer arriving for a live publisher renegotiates it in place (e.g.
// adding a screen-share track to an existing mic session).
func (s *SFU) HandlePublisherOffer(roomSlug, participantID, offerSDP, offerID string, onTrack func(participantID string, track *webrtc.TrackRemote, mid string)) (string, error) {
	room := s.GetRoomTracks(roomSlug)

	room.mu.Lock()
	if room.VoiceSessions == nil {
		room.VoiceSessions = make(map[string]*VoiceSession)
	}
	existing := room.VoiceSessions[participantID]
	room.mu.Unlock()

	// Renegotiate a live publisher in place. The ICE/DTLS transport is
	// already established, so the answer needs no candidate gathering.
	if existing != nil && existing.PeerConnection != nil {
		existing.SignalingMu.Lock()
		defer existing.SignalingMu.Unlock()

		state := existing.PeerConnection.ConnectionState()
		if state != webrtc.PeerConnectionStateFailed && state != webrtc.PeerConnectionStateClosed {
			pc := existing.PeerConnection
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offerSDP}); err != nil {
				return "", fmt.Errorf("failed to set publisher remote description: %w", err)
			}
			answer, err := pc.CreateAnswer(nil)
			if err != nil {
				return "", fmt.Errorf("failed to create publisher answer: %w", err)
			}
			if err := pc.SetLocalDescription(answer); err != nil {
				return "", fmt.Errorf("failed to set publisher local description: %w", err)
			}
			existing.SetPublisherOfferID(offerID)
			log.Printf("Publisher renegotiated for %s in room %s", participantID, roomSlug)
			return answer.SDP, nil
		}
	}

	return s.HandleVoiceOffer(roomSlug, participantID, offerSDP, offerID, onTrack)
}

// AbortPublisherOffer tears down the publisher session created/renegotiated
// for an offer whose answer could not be delivered. Leaving it alive would
// make the server think publishing is ready while the browser remains stuck
// waiting for the missing answer.
func (s *SFU) AbortPublisherOffer(roomSlug, participantID, offerID string) error {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return fmt.Errorf("room not found: %s", roomSlug)
	}

	room.mu.RLock()
	session, ok := room.VoiceSessions[participantID]
	room.mu.RUnlock()
	if !ok {
		return fmt.Errorf("publisher session not found: %s", participantID)
	}

	if current := session.PublisherOfferID(); offerID != "" && current != "" && offerID != current {
		return fmt.Errorf("%w: got %s, want %s", ErrStalePublisherOffer, offerID, current)
	}

	s.removeVoiceSessionIfSame(roomSlug, participantID, session)
	return nil
}

// AddPublisherCandidate adds a trickled ICE candidate from the client to its
// publisher peer connection.
func (s *SFU) AddPublisherCandidate(roomSlug, participantID string, candidate webrtc.ICECandidateInit, offerID string) error {
	room := s.GetRoomTracks(roomSlug)

	room.mu.Lock()
	vs := room.VoiceSessions[participantID]
	if vs == nil || vs.PeerConnection == nil {
		if room.PendingPublisherICE == nil {
			room.PendingPublisherICE = make(map[string][]PublisherICECandidate)
		}
		pending := room.PendingPublisherICE[participantID]
		if len(pending) >= MaxICECandidates {
			room.mu.Unlock()
			return fmt.Errorf("too many early publisher ICE candidates from %s", participantID)
		}
		room.PendingPublisherICE[participantID] = append(pending, PublisherICECandidate{Candidate: candidate, OfferID: offerID})
		room.mu.Unlock()
		return nil
	}
	if current := vs.PublisherOfferID(); offerID != "" && current != "" && offerID != current {
		room.mu.Unlock()
		return ErrStalePublisherCandidate
	}
	pc := vs.PeerConnection
	room.mu.Unlock()

	return pc.AddICECandidate(candidate)
}

func (s *SFU) flushPendingPublisherCandidates(room *RoomTracks, participantID string, pc *webrtc.PeerConnection) {
	room.mu.Lock()
	pending := room.PendingPublisherICE[participantID]
	delete(room.PendingPublisherICE, participantID)
	var offerID string
	if vs := room.VoiceSessions[participantID]; vs != nil {
		offerID = vs.PublisherOfferID()
	}
	room.mu.Unlock()

	for _, pendingCandidate := range pending {
		if pendingCandidate.OfferID != "" && offerID != "" && pendingCandidate.OfferID != offerID {
			log.Printf("Ignoring stale buffered publisher ICE candidate for %s", participantID)
			continue
		}
		if err := pc.AddICECandidate(pendingCandidate.Candidate); err != nil {
			log.Printf("Failed to add buffered publisher ICE candidate for %s: %v", participantID, err)
		}
	}
}

// HandleVoiceOffer processes an offer from a client wanting to send media
// over a dedicated publisher peer connection. Returns the SDP answer (with
// candidates pre-gathered) to send back.
func (s *SFU) HandleVoiceOffer(roomSlug, participantID, offerSDP, offerID string, onTrack func(participantID string, track *webrtc.TrackRemote, mid string)) (string, error) {
	// Create a peer connection
	pc, err := s.CreatePeerConnection()
	if err != nil {
		return "", fmt.Errorf("failed to create peer connection: %w", err)
	}

	// Forward every published track (mic audio AND screen-share video) —
	// the caller routes by kind.
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		// Resolve the transceiver mid for this track. The mid is the negotiation
		// anchor (identical on client and server) and — unlike track.ID(), which
		// Firefox/Safari rewrite in the SDP msid — reliably distinguishes the
		// webcam m-line from the screen-share m-line on the publisher PC.
		mid := ""
		for _, tr := range pc.GetTransceivers() {
			if tr.Receiver() == receiver {
				mid = tr.Mid()
				break
			}
		}
		log.Printf("Received published track from %s: %s (mid=%s)", participantID, track.Kind(), mid)
		onTrack(participantID, track, mid)
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
	newSession.SetPublisherOfferID(offerID)
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
	s.flushPendingPublisherCandidates(room, participantID, pc)

	// Handle connection state. Only act on terminal states — "disconnected" is
	// transient and can recover. Identity-check so a zombie callback from a
	// previously-replaced session can't delete the current one.
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("Voice connection state for %s: %s", participantID, state)
		switch state {
		case webrtc.PeerConnectionStateConnected:
			logSelectedICEPair("publisher", participantID, pc)
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
	delete(room.PendingPublisherICE, participantID)
	delete(room.VoiceRemoteTracks, participantID)
	delete(room.VoiceLocalTracks, participantID)
	delete(room.voiceMuteFlags, participantID)
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
	flag := room.voiceMuteFlags[participantID]
	session := room.VoiceSessions[participantID]
	room.mu.RUnlock()
	// Prefer the canonical shared flag the live relay reads; also mirror onto
	// the VoiceSession.Muted atomic so any consumer still reading it (and the
	// legacy test path) stays consistent.
	if flag != nil {
		flag.Store(muted)
	}
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
	// Capture the shared server-side mute flag so the forwarder doesn't need a
	// map lookup per packet and — critically — stays in sync with SetVoiceMuted
	// even when HandleVoiceOffer later replaces a placeholder VoiceSession with
	// a real one. The flag lives on RoomTracks (not VoiceSession) so its identity
	// is stable for the lifetime of the participant's voice relay.
	if room.voiceMuteFlags == nil {
		room.voiceMuteFlags = make(map[string]*atomic.Bool)
	}
	mutedFlag := room.voiceMuteFlags[participantID]
	if mutedFlag == nil {
		mutedFlag = &atomic.Bool{}
		room.voiceMuteFlags[participantID] = mutedFlag
	}
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
	var pkt rtp.Packet
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
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}
		// Strip the publisher's RTP header extensions — their IDs/values are
		// scoped to the publisher's negotiation and misroute streams in
		// subscribers' BUNDLE demux (see the WHIP forward loop for details).
		stripHeaderExtensions(&pkt.Header)
		if err := localTrack.WriteRTP(&pkt); err != nil {
			log.Printf("%s relay write error for %s: %v", label, participantID, err)
			return
		}
	}
}

// stripHeaderExtensions removes all RTP header extensions from a packet so
// relayed media carries none of the source negotiation's extension IDs.
func stripHeaderExtensions(h *rtp.Header) {
	h.Extension = false
	h.ExtensionProfile = 0
	h.Extensions = nil
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
// Returns the renegotiation offer SDP and offer ID that need to be sent to the
// subscriber.
func (s *SFU) AddVoiceTrackToSubscriber(roomSlug, subscriberID, voiceOwnerID string, localTrack *webrtc.TrackLocalStaticRTP) (string, string, error) {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return "", "", fmt.Errorf("room not found: %s", roomSlug)
	}

	room.mu.RLock()
	sub, ok := room.Subscribers[subscriberID]
	room.mu.RUnlock()

	if !ok {
		return "", "", fmt.Errorf("subscriber not found: %s", subscriberID)
	}

	offerSDP, offerID, err := s.addTrackAndRenegotiate(sub, localTrack, "voice")
	if err != nil {
		return "", "", err
	}

	log.Printf("Added voice track from %s to subscriber %s", voiceOwnerID, subscriberID)
	return offerSDP, offerID, nil
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
func (s *SFU) addTrackAndRenegotiate(sub *Subscriber, localTrack *webrtc.TrackLocalStaticRTP, label string) (string, string, error) {
	const maxAttempts = 3
	backoff := 250 * time.Millisecond

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-sub.done:
				return "", "", fmt.Errorf("subscriber %s removed while waiting to add %s track", sub.ID, label)
			case <-time.After(backoff):
			}
			backoff *= 2
		}

		offerSDP, offerID, deferred, retryable, err := s.tryAddTrackAndRenegotiate(sub, localTrack)
		if err == nil {
			if deferred {
				log.Printf("Attached %s track to subscriber %s mid-negotiation; offer deferred to next stable state", label, sub.ID)
				return "", "", nil
			}
			return offerSDP, offerID, nil
		}
		if !retryable {
			return "", "", err
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
	return "", "", nil
}

// tryAddTrackAndRenegotiate performs a single attempt of the add-track +
// renegotiate operation. deferred=true means the track was attached but the
// offer must wait for the in-flight negotiation to settle. retryable=true
// (with err) means the signaling lock was busy and the caller should retry.
func (s *SFU) tryAddTrackAndRenegotiate(sub *Subscriber, localTrack *webrtc.TrackLocalStaticRTP) (offerSDP string, offerID string, deferred bool, retryable bool, err error) {
	// Guard: don't touch a PC that is already closed/failed
	connState := sub.PeerConnection.ConnectionState()
	if connState == webrtc.PeerConnectionStateClosed || connState == webrtc.PeerConnectionStateFailed {
		return "", "", false, false, fmt.Errorf("subscriber %s connection in terminal state: %s", sub.ID, connState)
	}

	// Wait for the signaling mutex with a timeout. Another goroutine
	// (e.g. HandleSubscriberOffer processing a client mic offer) may hold
	// the lock, so we poll rather than block indefinitely.
	if !sub.trySignalingLock(2 * time.Second) {
		return "", "", false, true, fmt.Errorf("timed out waiting for signaling lock for subscriber %s", sub.ID)
	}
	defer sub.SignalingMu.Unlock()

	// After acquiring the lock, re-check connection state
	connState = sub.PeerConnection.ConnectionState()
	if connState == webrtc.PeerConnectionStateClosed || connState == webrtc.PeerConnectionStateFailed {
		return "", "", false, false, fmt.Errorf("subscriber %s connection in terminal state: %s", sub.ID, connState)
	}

	// Attach the shared relay track now — AddTrack is legal in any signaling
	// state and pion fans out writes to all PCs that have added this track.
	if !pcHasTrack(sub.PeerConnection, localTrack) {
		if _, err := sub.PeerConnection.AddTrack(localTrack); err != nil {
			return "", "", false, false, fmt.Errorf("failed to add track: %w", err)
		}
	}

	// A non-stable signaling state means another offer/answer exchange is in
	// flight. The track is attached; defer the offer to the next flush point
	// instead of failing (the old behavior permanently lost the track).
	if sub.PeerConnection.SignalingState() != webrtc.SignalingStateStable {
		sub.needsRenegotiation = true
		return "", "", true, false, nil
	}

	// Create renegotiation offer to notify subscriber of new track
	offer, err := sub.PeerConnection.CreateOffer(nil)
	if err != nil {
		sub.needsRenegotiation = true
		return "", "", false, false, fmt.Errorf("failed to create renegotiation offer: %w", err)
	}

	if err := sub.PeerConnection.SetLocalDescription(offer); err != nil {
		// Track is attached; mark for a follow-up offer so it isn't lost.
		sub.needsRenegotiation = true
		return "", "", false, false, fmt.Errorf("failed to set local description: %w", err)
	}

	sub.RenegotiationOfferID = s.nextSignalingOfferID()
	sub.setCandidateID(sub.RenegotiationOfferID)

	// ICE candidates are trickled via the subscriber's OnICECandidate callback
	return offer.SDP, sub.RenegotiationOfferID, false, false, nil
}

// flushPendingRenegotiationLocked attaches any queued tracks and, if any
// tracks were attached while signaling was busy, creates a follow-up
// renegotiation offer and pushes it via OnRenegotiationNeeded.
// Caller must hold sub.SignalingMu.
func (s *SFU) flushPendingRenegotiationLocked(sub *Subscriber) {
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
	sub.RenegotiationOfferID = s.nextSignalingOfferID()
	sub.setCandidateID(sub.RenegotiationOfferID)
	sub.needsRenegotiation = false
	log.Printf("Flushed pending renegotiation for subscriber %s", sub.ID)
	// Deliver outside the signaling-critical path.
	go cb(offer.SDP, sub.RenegotiationOfferID)
}

// tryFlushPendingRenegotiation acquires the signaling lock (bounded) and
// flushes pending renegotiation work for a subscriber.
func (s *SFU) tryFlushPendingRenegotiation(sub *Subscriber, lockTimeout time.Duration) {
	if !sub.trySignalingLock(lockTimeout) {
		return
	}
	defer sub.SignalingMu.Unlock()
	s.flushPendingRenegotiationLocked(sub)
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
func (s *SFU) SetSubscriberRenegotiationCallback(roomSlug, subscriberID string, cb func(offerSDP, offerID string)) {
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
func (s *SFU) RenegotiateSubscriber(roomSlug, subscriberID string) (string, string, error) {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return "", "", fmt.Errorf("room not found: %s", roomSlug)
	}

	room.mu.RLock()
	sub, ok := room.Subscribers[subscriberID]
	room.mu.RUnlock()

	if !ok {
		return "", "", fmt.Errorf("subscriber not found: %s", subscriberID)
	}

	sub.SignalingMu.Lock()
	defer sub.SignalingMu.Unlock()

	// Only renegotiate if in stable state
	if sub.PeerConnection.SignalingState() != webrtc.SignalingStateStable {
		return "", "", fmt.Errorf("cannot renegotiate subscriber %s: signaling state is %s", subscriberID, sub.PeerConnection.SignalingState())
	}

	// Create new offer
	offer, err := sub.PeerConnection.CreateOffer(nil)
	if err != nil {
		return "", "", fmt.Errorf("failed to create offer: %w", err)
	}

	if err := sub.PeerConnection.SetLocalDescription(offer); err != nil {
		return "", "", fmt.Errorf("failed to set local description: %w", err)
	}

	sub.RenegotiationOfferID = s.nextSignalingOfferID()
	sub.setCandidateID(sub.RenegotiationOfferID)

	// ICE candidates are trickled via the subscriber's OnICECandidate callback
	log.Printf("Renegotiation offer created for subscriber %s", subscriberID)
	return offer.SDP, sub.RenegotiationOfferID, nil
}

// AbortSubscriberRenegotiation retires a subscriber whose server-created
// renegotiation offer could not be delivered. Pion does not support local SDP
// rollback here, so keeping the peer connection would leave it wedged in
// have-local-offer until a reconnect.
func (s *SFU) AbortSubscriberRenegotiation(roomSlug, subscriberID, offerID string) error {
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

	if offerID != "" && sub.RenegotiationOfferID != "" && offerID != sub.RenegotiationOfferID {
		sub.SignalingMu.Unlock()
		return fmt.Errorf("stale renegotiation offer: got %s, want %s", offerID, sub.RenegotiationOfferID)
	}
	if sub.PeerConnection.SignalingState() != webrtc.SignalingStateHaveLocalOffer {
		sub.SignalingMu.Unlock()
		return nil
	}
	sub.SignalingMu.Unlock()

	room.removeSubscriberIfSame(subscriberID, sub)
	return nil
}

// HandleRenegotiationAnswer processes an answer from a subscriber during renegotiation
func (s *SFU) HandleRenegotiationAnswer(roomSlug, subscriberID, sdpAnswer, offerID string) error {
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

	if offerID != "" && sub.RenegotiationOfferID != "" && offerID != sub.RenegotiationOfferID {
		return fmt.Errorf("%w: got %s, want %s", ErrStaleRenegotiationAnswer, offerID, sub.RenegotiationOfferID)
	}

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
	sub.RenegotiationOfferID = ""

	// The answer completes a server-initiated renegotiation; any candidates
	// the client trickles for it count against a fresh budget.
	sub.resetICECandidateBudget()

	// Diagnostics: what did this browser accept for video, and does the SFU
	// actually transmit on every video sender (incl. screen-share relays)?
	log.Printf("Subscriber %s renegotiation answer video: %s", subscriberID, summarizeVideoAnswer(sdpAnswer))
	go s.probeOutboundVideo(sub, subscriberID)

	log.Printf("Renegotiation answer processed for subscriber %s", subscriberID)

	// Replay a client offer (voice offer or ICE restart) that was deferred
	// because it arrived during this server offer (HaveLocalOffer glare that
	// Pion v4 can't roll back). The PC is now Stable, so the deferred offer can
	// finally be applied and answered. The answer is delivered via the
	// subscriber's deferred-offer callback rather than returned here, since this
	// handler's return value answers the *server* offer, not the client's.
	s.replayDeferredClientOfferLocked(sub, subscriberID)

	return nil
}

// replayDeferredClientOffer applies a client voice offer or ICE restart that was
// queued while a server-initiated offer was in flight, now that the PC is Stable
// again. Caller must hold sub.SignalingMu.
func (s *SFU) replayDeferredClientOfferLocked(sub *Subscriber, subscriberID string) {
	deferredSDP := sub.pendingClientOffer
	deferredOfferID := sub.pendingClientOfferID
	isRestart := sub.pendingClientOfferIsRestart
	cb := sub.OnDeferredClientOffer
	if deferredSDP == "" || cb == nil {
		// Nothing deferred, or no way to deliver the answer yet. Leave it
		// pending; SetDeferredClientOfferCallback flushes when wired.
		return
	}
	// Guard against a caller that reaches us before the PC is Stable: applying
	// an offer during HaveLocalOffer would collide and (since we clear pending
	// state below) drop the deferred offer. Leave it pending for the next
	// settling point instead.
	if sub.PeerConnection.SignalingState() != webrtc.SignalingStateStable {
		return
	}
	// Clear the pending state before replay so a failure doesn't loop.
	sub.pendingClientOffer = ""
	sub.pendingClientOfferID = ""
	sub.pendingClientOfferIsRestart = false

	offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: deferredSDP}
	if err := sub.PeerConnection.SetRemoteDescription(offer); err != nil {
		log.Printf("Failed to apply deferred client offer for %s: %v", subscriberID, err)
		return
	}
	sub.setCandidateID(deferredOfferID)
	sub.resetICECandidateBudget()

	answer, err := sub.PeerConnection.CreateAnswer(nil)
	if err != nil {
		log.Printf("Failed to create deferred client offer answer for %s: %v", subscriberID, err)
		return
	}
	if err := sub.PeerConnection.SetLocalDescription(answer); err != nil {
		log.Printf("Failed to set deferred client offer answer for %s: %v", subscriberID, err)
		return
	}
	log.Printf("Replayed deferred client offer for subscriber %s (restart=%v)", subscriberID, isRestart)
	// Deliver outside the signaling lock to avoid re-entrancy into SendJSON paths.
	go cb(isRestart, deferredOfferID, answer.SDP)
}

// SetDeferredClientOfferCallback wires the delivery path for a client voice
// offer or ICE restart that was deferred during HaveLocalOffer glare. Flushes
// immediately if a deferred offer is already waiting (e.g. the callback was
// wired after the offer arrived).
func (s *SFU) SetDeferredClientOfferCallback(roomSlug, subscriberID string, cb func(isRestart bool, offerID, answerSDP string)) {
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
	sub.SignalingMu.Lock()
	sub.OnDeferredClientOffer = cb
	hasPending := sub.pendingClientOffer != ""
	sub.SignalingMu.Unlock()
	if hasPending {
		// A deferred offer is waiting and the PC may already be Stable (server
		// offer answered before the callback was wired). Replay it now.
		sub.SignalingMu.Lock()
		s.replayDeferredClientOfferLocked(sub, subscriberID)
		sub.SignalingMu.Unlock()
	}
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
func (s *SFU) AddScreenShareTrackToSubscriber(roomSlug, subscriberID, sharerID string, localTrack *webrtc.TrackLocalStaticRTP) (string, string, error) {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return "", "", fmt.Errorf("room not found: %s", roomSlug)
	}

	room.mu.RLock()
	sub, ok := room.Subscribers[subscriberID]
	room.mu.RUnlock()

	if !ok {
		return "", "", fmt.Errorf("subscriber not found: %s", subscriberID)
	}

	offerSDP, offerID, err := s.addTrackAndRenegotiate(sub, localTrack, "screen share")
	if err != nil {
		return "", "", err
	}

	log.Printf("Added screen share track from %s to subscriber %s", sharerID, subscriberID)
	return offerSDP, offerID, nil
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

// ---- Webcam presence relays ------------------------------------------------
// A participant's webcam is a small VP8 video track that fans out to every
// other participant. Architecturally it is the voice relay (per-participant
// keyed maps) using the screen-share video machinery, with no mute gate.

// RegisterWebcamTrackID records that a participant's RTP video track with the
// given ID is a webcam (not a screen share). Called from the webcam:start
// signal BEFORE the publish offer is negotiated, so the publisher OnTrack
// handler (which otherwise treats all video as screen share) can route it.
func (s *SFU) RegisterWebcamTrackID(roomSlug, participantID, trackID string) {
	if trackID == "" {
		return
	}
	// GetRoomTracks (not ...ForSlug) so a webcam:start that races ahead of room
	// materialization on join still registers — otherwise the cam would be
	// mis-routed to the screen-share path when OnTrack fires.
	room := s.GetRoomTracks(roomSlug)
	if room == nil {
		return
	}
	room.mu.Lock()
	defer room.mu.Unlock()
	if room.webcamTrackIDs == nil {
		room.webcamTrackIDs = make(map[string]map[string]bool)
	}
	ids := room.webcamTrackIDs[participantID]
	if ids == nil {
		ids = make(map[string]bool)
		room.webcamTrackIDs[participantID] = ids
	}
	ids[trackID] = true
}

// IsWebcamTrack reports whether the given RTP track ID was announced as a
// webcam for the participant. Reading from nil maps is safe and returns false.
func (s *SFU) IsWebcamTrack(roomSlug, participantID, trackID string) bool {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return false
	}
	room.mu.RLock()
	defer room.mu.RUnlock()
	return room.webcamTrackIDs[participantID][trackID]
}

// CreateWebcamRelayTrack creates (or reuses) a per-participant webcam relay
// track and starts a single forwarding goroutine. Mirrors CreateVoiceRelayTrack
// without the mute gate. The bool reports whether a NEW relay was created (so
// the caller knows to fan it out to subscribers).
func (s *SFU) CreateWebcamRelayTrack(roomSlug, participantID string, remoteTrack *webrtc.TrackRemote) (*webrtc.TrackLocalStaticRTP, bool, error) {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return nil, false, fmt.Errorf("room not found: %s", roomSlug)
	}

	done := make(chan struct{})

	room.mu.Lock()
	if room.WebcamLocalTracks == nil {
		room.WebcamLocalTracks = make(map[string]*webrtc.TrackLocalStaticRTP)
	}
	existing, reused := room.WebcamLocalTracks[participantID]
	var localTrack *webrtc.TrackLocalStaticRTP
	if reused {
		localTrack = existing
	} else {
		created, err := webrtc.NewTrackLocalStaticRTP(
			remoteTrack.Codec().RTPCodecCapability,
			fmt.Sprintf("webcam-%s", participantID),
			fmt.Sprintf("webcam-stream-%s", participantID),
		)
		if err != nil {
			room.mu.Unlock()
			return nil, false, fmt.Errorf("failed to create webcam relay track: %w", err)
		}
		localTrack = created
		room.WebcamLocalTracks[participantID] = localTrack
	}
	if room.webcamRelayDone == nil {
		room.webcamRelayDone = make(map[string]chan struct{})
	}
	// Stop any previous relay goroutine for this participant before replacing it
	// (cam restarted) so it can't leak or double-write the shared local track.
	if prev, ok := room.webcamRelayDone[participantID]; ok {
		close(prev)
	}
	room.webcamRelayDone[participantID] = done
	room.mu.Unlock()

	go relayTrackLoop(remoteTrack, localTrack, done, nil, "Webcam", participantID)

	if reused {
		log.Printf("Rebound webcam relay for %s in room %s", participantID, roomSlug)
	} else {
		log.Printf("Created webcam relay track for %s in room %s", participantID, roomSlug)
	}
	return localTrack, !reused, nil
}

// AddWebcamTrackToSubscriber adds a participant's webcam relay track to a
// subscriber and creates a renegotiation offer. Same pattern as voice.
func (s *SFU) AddWebcamTrackToSubscriber(roomSlug, subscriberID, ownerID string, localTrack *webrtc.TrackLocalStaticRTP) (string, string, error) {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return "", "", fmt.Errorf("room not found: %s", roomSlug)
	}
	room.mu.RLock()
	sub, ok := room.Subscribers[subscriberID]
	room.mu.RUnlock()
	if !ok {
		return "", "", fmt.Errorf("subscriber not found: %s", subscriberID)
	}
	offerSDP, offerID, err := s.addTrackAndRenegotiate(sub, localTrack, "webcam")
	if err != nil {
		return "", "", err
	}
	log.Printf("Added webcam track from %s to subscriber %s", ownerID, subscriberID)
	return offerSDP, offerID, nil
}

// RemoveWebcamTrack stops a participant's webcam relay and removes its sender
// from all subscribers. Returns affected subscriber IDs for renegotiation.
func (s *SFU) RemoveWebcamTrack(roomSlug, participantID string) []string {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return nil
	}

	room.mu.Lock()
	localTrack := room.WebcamLocalTracks[participantID]
	delete(room.WebcamLocalTracks, participantID)
	delete(room.webcamTrackIDs, participantID)
	delete(room.webcamHidden, participantID)
	if ch, ok := room.webcamRelayDone[participantID]; ok {
		close(ch)
		delete(room.webcamRelayDone, participantID)
	}
	room.mu.Unlock()

	if localTrack == nil {
		return nil
	}

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
		sub.SignalingMu.Lock()
		for _, sender := range sub.PeerConnection.GetSenders() {
			if sender.Track() == localTrack {
				if err := sub.PeerConnection.RemoveTrack(sender); err != nil {
					log.Printf("Failed to remove webcam sender from %s: %v", subID, err)
				} else {
					affected = append(affected, subID)
				}
				break
			}
		}
		sub.SignalingMu.Unlock()
	}
	log.Printf("Removed webcam track from %s in room %s, affected %d subscribers", participantID, roomSlug, len(affected))
	return affected
}

// SetWebcamDisabled is the admin camera gate. Disabling tears down any live
// relay (returning affected subscriber IDs to renegotiate) and blocks future
// relays for that participant until re-enabled. Enabling just clears the gate;
// the participant may then turn their cam back on.
func (s *SFU) SetWebcamDisabled(roomSlug, participantID string, disabled bool) []string {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return nil
	}
	room.mu.Lock()
	if room.webcamDisabled == nil {
		room.webcamDisabled = make(map[string]bool)
	}
	if disabled {
		room.webcamDisabled[participantID] = true
	} else {
		delete(room.webcamDisabled, participantID)
	}
	room.mu.Unlock()

	if disabled {
		// Tear down whatever they currently have (RemoveWebcamTrack locks room.mu
		// itself, hence the unlock above).
		return s.RemoveWebcamTrack(roomSlug, participantID)
	}
	return nil
}

// IsWebcamDisabled reports whether an admin has gated this participant's camera.
func (s *SFU) IsWebcamDisabled(roomSlug, participantID string) bool {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return false
	}
	room.mu.RLock()
	defer room.mu.RUnlock()
	return room.webcamDisabled[participantID]
}

// SetWebcamVisible records the owner's fast hide/show state for late joiners.
func (s *SFU) SetWebcamVisible(roomSlug, participantID string, visible bool) {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return
	}
	room.mu.Lock()
	defer room.mu.Unlock()
	if visible {
		delete(room.webcamHidden, participantID)
		return
	}
	if room.webcamHidden == nil {
		room.webcamHidden = make(map[string]bool)
	}
	room.webcamHidden[participantID] = true
}

// HiddenWebcamParticipantIDs returns active webcam owners whose tiles are hidden.
func (s *SFU) HiddenWebcamParticipantIDs(roomSlug string) []string {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return nil
	}
	room.mu.RLock()
	defer room.mu.RUnlock()
	ids := make([]string, 0, len(room.webcamHidden))
	for id := range room.webcamHidden {
		if _, active := room.WebcamLocalTracks[id]; active {
			ids = append(ids, id)
		}
	}
	return ids
}

// HasWebcam reports whether a participant currently has an active webcam relay.
// Used by the disconnect path to tear cams down on a page refresh.
func (s *SFU) HasWebcam(roomSlug, participantID string) bool {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return false
	}
	room.mu.RLock()
	defer room.mu.RUnlock()
	_, ok := room.WebcamLocalTracks[participantID]
	return ok
}
