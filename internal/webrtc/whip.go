package webrtc

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"chromatic/internal/metrics"

	"github.com/pion/webrtc/v4"
)

// WHIPHandler handles WHIP protocol requests from OBS
type WHIPHandler struct {
	sfu           *SFU
	validateKey   func(token string) (bool, error)
	onStreamStart func(token string) error
	onStreamEnd   func(token string)
}

// NewWHIPHandler creates a new WHIP handler
func NewWHIPHandler(sfu *SFU, validateKey func(string) (bool, error), onStart func(string) error, onEnd func(string)) *WHIPHandler {
	return &WHIPHandler{
		sfu:           sfu,
		validateKey:   validateKey,
		onStreamStart: onStart,
		onStreamEnd:   onEnd,
	}
}

// ServeHTTP handles WHIP requests
func (h *WHIPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Extract stream key token from path
	token := strings.TrimPrefix(r.URL.Path, "/whip/")
	if token == "" {
		http.Error(w, "Missing stream key", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodPost:
		h.handleOffer(w, r, token)
	case http.MethodPatch:
		h.handleICETrickle(w, r, token)
	case http.MethodDelete:
		h.handleDelete(w, r, token)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleOffer handles the initial SDP offer from OBS
func (h *WHIPHandler) handleOffer(w http.ResponseWriter, r *http.Request, token string) {
	// Validate stream key
	valid, err := h.validateKey(token)
	if err != nil {
		log.Printf("Error validating stream key: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if !valid {
		http.Error(w, "Invalid stream key", http.StatusUnauthorized)
		return
	}

	// Check if already streaming with this key
	if h.sfu.GetIngest(token) != nil {
		http.Error(w, "Stream key already in use", http.StatusConflict)
		return
	}

	// Read SDP offer
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB limit
	if err != nil {
		http.Error(w, "Failed to read offer", http.StatusBadRequest)
		return
	}

	sdpOffer := string(body)

	// Validate SDP - check for B-frames configuration
	if err := validateSDP(sdpOffer); err != nil {
		log.Printf("SDP validation failed for %s: %v", token, err)
		http.Error(w, fmt.Sprintf("Invalid stream configuration: %v", err), http.StatusUnprocessableEntity)
		return
	}

	// Create peer connection
	pc, err := h.sfu.CreatePeerConnection()
	if err != nil {
		log.Printf("Failed to create peer connection: %v", err)
		http.Error(w, "Failed to create connection", http.StatusInternalServerError)
		return
	}

	// Create local tracks for forwarding
	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		"video",
		"chromatic-stream",
	)
	if err != nil {
		pc.Close()
		log.Printf("Failed to create video track: %v", err)
		http.Error(w, "Failed to create video track", http.StatusInternalServerError)
		return
	}

	audioTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		"audio",
		"chromatic-stream",
	)
	if err != nil {
		pc.Close()
		log.Printf("Failed to create audio track: %v", err)
		http.Error(w, "Failed to create audio track", http.StatusInternalServerError)
		return
	}

	// Create ingest session
	session := &IngestSession{
		StreamKeyToken: token,
		PeerConnection: pc,
		VideoTrack:     videoTrack,
		AudioTrack:     audioTrack,
		done:           make(chan struct{}),
	}

	// Set up track handler - forward incoming tracks to local tracks
	pc.OnTrack(func(remoteTrack *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Printf("Received track: %s (kind: %s)", remoteTrack.ID(), remoteTrack.Kind())

		var localTrack *webrtc.TrackLocalStaticRTP
		switch remoteTrack.Kind() {
		case webrtc.RTPCodecTypeVideo:
			localTrack = session.VideoTrack
		case webrtc.RTPCodecTypeAudio:
			localTrack = session.AudioTrack
		default:
			log.Printf("Unknown track kind: %s", remoteTrack.Kind())
			return
		}

		// Forward RTP packets
		go func() {
			buf := make([]byte, 1500)
			for {
				select {
				case <-session.done:
					return
				default:
					n, _, err := remoteTrack.Read(buf)
					if err != nil {
						if err != io.EOF {
							log.Printf("Error reading from remote track: %v", err)
						}
						return
					}

					if _, err := localTrack.Write(buf[:n]); err != nil {
						if err != io.ErrClosedPipe {
							log.Printf("Error writing to local track: %v", err)
						}
						return
					}
				}
			}
		}()
	})

	// Teardown runs exactly once per session across Failed/Closed state
	// callbacks and explicit DELETE requests, preventing duplicate
	// stream-end broadcasts and negative ingest metrics.
	var teardownOnce sync.Once
	session.teardown = func() {
		teardownOnce.Do(func() {
			// Only adjust the metric if this session ever counted itself in.
			if session.everConnected.Load() {
				metrics.Get().ActiveWHIPIngests.Add(-1)
			}
			h.sfu.RemoveIngest(token)
			// Only notify stream end if the stream actually started;
			// otherwise viewers would see a spurious "stream ended" for a
			// session that never connected.
			if session.everConnected.Load() && h.onStreamEnd != nil {
				h.onStreamEnd(token)
			}
		})
	}

	// Handle connection state changes
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("WHIP connection state: %s (key: %s...)", state, token[:8])

		switch state {
		case webrtc.PeerConnectionStateConnected:
			// Only count the first transition to Connected; a reconnect
			// after a transient Disconnected must not double-increment.
			if session.everConnected.CompareAndSwap(false, true) {
				metrics.Get().ActiveWHIPIngests.Add(1)
			}
			if h.onStreamStart != nil {
				if err := h.onStreamStart(token); err != nil {
					log.Printf("Error on stream start: %v", err)
				}
			}
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed:
			// Disconnected is deliberately NOT treated as terminal: Pion
			// transitions to Failed if the connection doesn't recover, and
			// transient network blips resolve back to Connected.
			session.teardown()
		}
	})

	// Set remote description (the offer from OBS)
	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdpOffer,
	}
	if err := pc.SetRemoteDescription(offer); err != nil {
		pc.Close()
		log.Printf("Failed to set remote description: %v", err)
		http.Error(w, "Failed to process offer", http.StatusBadRequest)
		return
	}

	// Create answer
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		pc.Close()
		log.Printf("Failed to create answer: %v", err)
		http.Error(w, "Failed to create answer", http.StatusInternalServerError)
		return
	}

	// Set local description
	if err := pc.SetLocalDescription(answer); err != nil {
		pc.Close()
		log.Printf("Failed to set local description: %v", err)
		http.Error(w, "Failed to set answer", http.StatusInternalServerError)
		return
	}

	// Register the session before waiting for ICE gathering so trickle-ICE
	// PATCH requests arriving during gathering can find it.
	h.sfu.SetIngest(token, session)

	// Bound the wait for ICE gathering. Host/srflx candidates gather quickly
	// and are sufficient for OBS-to-server connectivity; a slow TURN server
	// must not stall the WHIP response past client/server write timeouts.
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	select {
	case <-gatherComplete:
	case <-time.After(3 * time.Second):
		log.Printf("ICE gathering incomplete after 3s for key %s..., responding with partial candidates", token[:8])
	}

	// Return the answer
	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", r.URL.String())
	w.WriteHeader(http.StatusCreated)
	w.Write([]byte(pc.LocalDescription().SDP))

	log.Printf("WHIP session established for key: %s...", token[:8])
}

// handleICETrickle handles ICE candidate trickling
func (h *WHIPHandler) handleICETrickle(w http.ResponseWriter, r *http.Request, token string) {
	session := h.sfu.GetIngest(token)
	if session == nil {
		http.Error(w, "Session not found", http.StatusNotFound)
		return
	}

	// Check ICE candidate limit to prevent flooding attacks
	session.iceMu.Lock()
	if session.iceCandidateCount >= MaxICECandidates {
		session.iceMu.Unlock()
		http.Error(w, "Too many ICE candidates", http.StatusTooManyRequests)
		return
	}
	session.iceCandidateCount++
	session.iceMu.Unlock()

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		http.Error(w, "Failed to read candidate", http.StatusBadRequest)
		return
	}

	var candidate webrtc.ICECandidateInit
	if err := json.Unmarshal(body, &candidate); err != nil {
		http.Error(w, "Invalid candidate format", http.StatusBadRequest)
		return
	}

	if err := session.PeerConnection.AddICECandidate(candidate); err != nil {
		log.Printf("Failed to add ICE candidate: %v", err)
		http.Error(w, "Failed to add candidate", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleDelete handles stream termination
func (h *WHIPHandler) handleDelete(w http.ResponseWriter, r *http.Request, token string) {
	session := h.sfu.GetIngest(token)
	if session == nil {
		http.Error(w, "Session not found", http.StatusNotFound)
		return
	}

	session.PeerConnection.Close()

	// Run the shared teardown (idempotent): removes the ingest and notifies
	// stream end exactly once, even though Close() also fires the Closed
	// state callback asynchronously.
	if session.teardown != nil {
		session.teardown()
	} else {
		h.sfu.RemoveIngest(token)
	}

	w.WriteHeader(http.StatusNoContent)
	log.Printf("WHIP session terminated for key: %s...", token[:8])
}

// validateSDP validates the incoming SDP offer
// CRITICAL: Checks for B-frame configuration which causes latency issues
func validateSDP(sdp string) error {
	// Check for B-frames in H.264 configuration
	// B-frames in WebRTC cause 2+ second latency due to browser reordering issues

	// H.264 profile-level-id format: 3 bytes (profile_idc, constraints, level_idc)
	// Baseline Profile: 42 (0x42) - no B-frames
	// Constrained Baseline: 42e0 - no B-frames (constraints byte has flag)
	// Main Profile: 4d (0x4d) - CAN use B-frames
	// High Profile: 64 (0x64) - CAN use B-frames

	// Parse profile-level-id from SDP
	profileID := extractProfileLevelID(sdp)
	if profileID == "" {
		// No H.264 profile found - check if there's any H.264 at all
		if !strings.Contains(strings.ToLower(sdp), "h264") {
			log.Printf("Warning: Stream not using H.264 codec, proceeding anyway")
			return nil
		}
		// H.264 present but no profile-level-id - allow with warning
		log.Printf("Warning: H.264 detected but profile-level-id not found in SDP")
		return nil
	}

	// Check the profile byte (first 2 hex digits)
	if len(profileID) >= 2 {
		profileByte := strings.ToLower(profileID[:2])

		switch profileByte {
		case "42":
			// Baseline or Constrained Baseline - safe (no B-frames)
			log.Printf("H.264 Baseline Profile detected - good for low latency streaming")
		case "4d":
			// Main Profile - can use B-frames, reject for safety
			log.Printf("H.264 Main Profile detected (profile-level-id: %s)", profileID)
			return ErrBFramesDetected
		case "64":
			// High Profile - can use B-frames, reject for safety
			log.Printf("H.264 High Profile detected (profile-level-id: %s)", profileID)
			return ErrBFramesDetected
		case "58":
			// Extended Profile - can use B-frames
			log.Printf("H.264 Extended Profile detected (profile-level-id: %s)", profileID)
			return ErrBFramesDetected
		default:
			// Unknown profile - allow with warning
			log.Printf("Unknown H.264 profile detected (profile-level-id: %s), proceeding anyway", profileID)
		}
	}

	return nil
}

// extractProfileLevelID extracts the profile-level-id from SDP fmtp lines
// Example: a=fmtp:97 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f
func extractProfileLevelID(sdp string) string {
	lines := strings.Split(sdp, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "a=fmtp:") {
			continue
		}

		// Look for profile-level-id parameter
		parts := strings.Split(line, " ")
		if len(parts) < 2 {
			continue
		}

		params := strings.Split(parts[1], ";")
		for _, param := range params {
			param = strings.TrimSpace(param)
			if strings.HasPrefix(strings.ToLower(param), "profile-level-id=") {
				parts := strings.SplitN(param, "=", 2)
				if len(parts) == 2 {
					return parts[1]
				}
			}
		}
	}
	return ""
}

// ErrBFramesDetected indicates B-frames are configured
var ErrBFramesDetected = errors.New("B-frames detected: Main/High profile can use B-frames which cause 2+ second latency. Set encoder to Baseline profile or disable B-frames in OBS")
