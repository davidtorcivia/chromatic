package webrtc

import (
	"fmt"
	"log"
	"sync"

	"chromatic/internal/config"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/intervalpli"
	"github.com/pion/webrtc/v4"
)

// SFU is the Selective Forwarding Unit that manages WebRTC connections
type SFU struct {
	mu     sync.RWMutex
	config *config.Config
	api    *webrtc.API

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
}

// RoomTracks holds the tracks being distributed to a room
type RoomTracks struct {
	mu            sync.RWMutex
	RoomSlug      string
	VideoTrack    *webrtc.TrackLocalStaticRTP
	AudioTrack    *webrtc.TrackLocalStaticRTP
	Subscribers   map[string]*Subscriber
	VoiceSessions map[string]*VoiceSession // Participant voice connections
}

// Subscriber represents a client receiving the stream
type Subscriber struct {
	ID             string
	PeerConnection *webrtc.PeerConnection
	done           chan struct{}
	closeOnce      sync.Once // Ensures done channel is closed only once
}

// NewSFU creates a new SFU instance
func NewSFU(cfg *config.Config) (*SFU, error) {
	// Create a MediaEngine with default codecs
	m := &webrtc.MediaEngine{}

	// Register H.264 codec for video (primary for color work)
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
		},
		PayloadType: 96,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, fmt.Errorf("failed to register H264 codec: %w", err)
	}

	// Register Opus codec for audio
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeOpus,
			ClockRate: 48000,
			Channels:  2,
		},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("failed to register Opus codec: %w", err)
	}

	// Create interceptor registry
	i := &interceptor.Registry{}

	// Add PLI (Picture Loss Indication) generator for better video quality
	intervalPliFactory, err := intervalpli.NewReceiverInterceptor()
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
		config:  cfg,
		api:     api,
		ingests: make(map[string]*IngestSession),
		rooms:   make(map[string]*RoomTracks),
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

	// Add configured TURN server if available
	if s.config.TurnRealm != "" {
		// Generate time-limited TURN credentials
		username, credential := generateTURNCredentials(s.config.TurnSecret, s.config.TurnRealm)
		servers = append(servers, webrtc.ICEServer{
			URLs:       []string{fmt.Sprintf("turn:%s:3478", s.config.TurnRealm)},
			Username:   username,
			Credential: credential,
		})
	}

	// Add external TURN if configured
	if s.config.TurnExternalURL != "" {
		servers = append(servers, webrtc.ICEServer{
			URLs:       []string{s.config.TurnExternalURL},
			Username:   s.config.TurnExternalUser,
			Credential: s.config.TurnExternalPass,
		})
	}

	return servers
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

// Shutdown gracefully shuts down the SFU
func (s *SFU) Shutdown() {
	s.mu.Lock()
	s.shutdown = true

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

// AddSubscriber adds a subscriber to a room
func (rt *RoomTracks) AddSubscriber(sub *Subscriber) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.Subscribers[sub.ID] = sub
}

// RemoveSubscriber removes a subscriber from a room
func (rt *RoomTracks) RemoveSubscriber(id string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if sub, ok := rt.Subscribers[id]; ok {
		// Use sync.Once to prevent double-close panic
		sub.closeOnce.Do(func() {
			close(sub.done)
		})
		delete(rt.Subscribers, id)
	}
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

// BindIngestToRoom binds an ingest session's tracks to a room for distribution
func (s *SFU) BindIngestToRoom(streamKeyToken, roomSlug string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	ingest, ok := s.ingests[streamKeyToken]
	if !ok {
		return fmt.Errorf("ingest session not found for token")
	}

	room, ok := s.rooms[roomSlug]
	if !ok {
		room = &RoomTracks{
			RoomSlug:    roomSlug,
			Subscribers: make(map[string]*Subscriber),
		}
		s.rooms[roomSlug] = room
	}

	room.mu.Lock()
	room.VideoTrack = ingest.VideoTrack
	room.AudioTrack = ingest.AudioTrack
	room.mu.Unlock()

	log.Printf("Bound ingest %s... to room %s", streamKeyToken[:8], roomSlug)
	return nil
}

// GetRoomTracksForSlug returns the room tracks if they exist
func (s *SFU) GetRoomTracksForSlug(roomSlug string) *RoomTracks {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rooms[roomSlug]
}

// CreateSubscriberConnection creates a new subscriber peer connection for a room
// Returns the peer connection and an SDP offer to send to the client
func (s *SFU) CreateSubscriberConnection(roomSlug, subscriberID string) (*webrtc.PeerConnection, *webrtc.SessionDescription, error) {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return nil, nil, fmt.Errorf("room not found: %s", roomSlug)
	}

	room.mu.RLock()
	videoTrack := room.VideoTrack
	audioTrack := room.AudioTrack
	room.mu.RUnlock()

	// Create peer connection
	pc, err := s.CreatePeerConnection()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create peer connection: %w", err)
	}

	// Add video track if available
	if videoTrack != nil {
		_, err = pc.AddTrack(videoTrack)
		if err != nil {
			pc.Close()
			return nil, nil, fmt.Errorf("failed to add video track: %w", err)
		}
		log.Printf("Added video track to subscriber %s", subscriberID)
	}

	// Add audio track if available
	if audioTrack != nil {
		_, err = pc.AddTrack(audioTrack)
		if err != nil {
			pc.Close()
			return nil, nil, fmt.Errorf("failed to add audio track: %w", err)
		}
		log.Printf("Added audio track to subscriber %s", subscriberID)
	}

	// Create subscriber record
	sub := &Subscriber{
		ID:             subscriberID,
		PeerConnection: pc,
		done:           make(chan struct{}),
	}

	room.AddSubscriber(sub)

	// Handle connection state
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("Subscriber %s connection state: %s", subscriberID, state)
		switch state {
		case webrtc.PeerConnectionStateDisconnected,
			webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed:
			room.RemoveSubscriber(subscriberID)
		}
	})

	// Create offer
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		pc.Close()
		room.RemoveSubscriber(subscriberID)
		return nil, nil, fmt.Errorf("failed to create offer: %w", err)
	}

	// Set local description
	if err := pc.SetLocalDescription(offer); err != nil {
		pc.Close()
		room.RemoveSubscriber(subscriberID)
		return nil, nil, fmt.Errorf("failed to set local description: %w", err)
	}

	// Wait for ICE gathering
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	<-gatherComplete

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
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	<-gatherComplete

	// Store the voice session
	room := s.GetRoomTracks(roomSlug)
	room.mu.Lock()
	if room.VoiceSessions == nil {
		room.VoiceSessions = make(map[string]*VoiceSession)
	}
	room.VoiceSessions[participantID] = &VoiceSession{
		ParticipantID:  participantID,
		PeerConnection: pc,
		done:           make(chan struct{}),
	}
	room.mu.Unlock()

	// Handle connection state
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("Voice connection state for %s: %s", participantID, state)
		switch state {
		case webrtc.PeerConnectionStateDisconnected,
			webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed:
			s.RemoveVoiceSession(roomSlug, participantID)
		}
	})

	return pc.LocalDescription().SDP, nil
}

// RemoveVoiceSession removes a voice session
func (s *SFU) RemoveVoiceSession(roomSlug, participantID string) {
	room := s.GetRoomTracksForSlug(roomSlug)
	if room == nil {
		return
	}

	room.mu.Lock()
	defer room.mu.Unlock()

	if session, ok := room.VoiceSessions[participantID]; ok {
		session.closeOnce.Do(func() {
			close(session.done)
		})
		if session.PeerConnection != nil {
			session.PeerConnection.Close()
		}
		delete(room.VoiceSessions, participantID)
		log.Printf("Removed voice session for %s from room %s", participantID, roomSlug)
	}
}

// AddVoiceTrackToSubscriber adds a voice track from one participant to another's subscriber connection
func (s *SFU) AddVoiceTrackToSubscriber(roomSlug, subscriberID, voiceOwnerID string, remoteTrack *webrtc.TrackRemote) error {
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

	// Create a local track to forward the remote track
	localTrack, err := webrtc.NewTrackLocalStaticRTP(
		remoteTrack.Codec().RTPCodecCapability,
		fmt.Sprintf("voice-%s", voiceOwnerID),
		fmt.Sprintf("voice-stream-%s", voiceOwnerID),
	)
	if err != nil {
		return fmt.Errorf("failed to create local track: %w", err)
	}

	// Add the track to the subscriber's peer connection
	_, err = sub.PeerConnection.AddTrack(localTrack)
	if err != nil {
		return fmt.Errorf("failed to add track: %w", err)
	}

	// Start forwarding RTP packets from remote to local track
	go func() {
		buf := make([]byte, 1500)
		for {
			n, _, err := remoteTrack.Read(buf)
			if err != nil {
				log.Printf("Voice track read error for %s: %v", voiceOwnerID, err)
				return
			}
			if _, err := localTrack.Write(buf[:n]); err != nil {
				log.Printf("Voice track write error to %s: %v", subscriberID, err)
				return
			}
		}
	}()

	log.Printf("Added voice track from %s to subscriber %s", voiceOwnerID, subscriberID)
	return nil
}
