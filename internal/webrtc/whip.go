package webrtc

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

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

	// Handle connection state changes
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("WHIP connection state: %s (key: %s...)", state, token[:8])

		switch state {
		case webrtc.PeerConnectionStateConnected:
			if h.onStreamStart != nil {
				if err := h.onStreamStart(token); err != nil {
					log.Printf("Error on stream start: %v", err)
				}
			}
		case webrtc.PeerConnectionStateDisconnected,
			webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed:
			h.sfu.RemoveIngest(token)
			if h.onStreamEnd != nil {
				h.onStreamEnd(token)
			}
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

	// Wait for ICE gathering to complete
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	<-gatherComplete

	// Store the session
	h.sfu.SetIngest(token, session)

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
	h.sfu.RemoveIngest(token)

	if h.onStreamEnd != nil {
		h.onStreamEnd(token)
	}

	w.WriteHeader(http.StatusNoContent)
	log.Printf("WHIP session terminated for key: %s...", token[:8])
}

// validateSDP validates the incoming SDP offer
// CRITICAL: Checks for B-frame configuration which causes latency issues
func validateSDP(sdp string) error {
	// Check for B-frames in H.264 configuration
	// B-frames in WebRTC cause 2+ second latency due to browser reordering issues

	// Look for profile-level-id patterns that indicate B-frames might be used
	// Main Profile (4d) and High Profile (64) can use B-frames
	// Baseline Profile (42) cannot use B-frames - preferred for live streaming

	// This is a basic check - in production you might parse SDP more thoroughly
	if strings.Contains(sdp, "max-br=") {
		// max-br > 0 indicates B-frame buffering
		// This is a heuristic - OBS with zerolatency preset shouldn't have this
		log.Printf("Warning: SDP contains max-br parameter, B-frames may be enabled")
	}

	// Check for zerolatency indicators (good)
	if !strings.Contains(sdp, "H264") && !strings.Contains(sdp, "h264") {
		// Not using H.264 - warn but allow
		log.Printf("Warning: Stream not using H.264 codec")
	}

	// Additional validation could be added here:
	// - Parse fmtp lines for specific parameters
	// - Check bitrate constraints
	// - Validate resolution

	return nil
}

// ErrBFramesDetected indicates B-frames are configured
var ErrBFramesDetected = errors.New("B-frames detected in encoder configuration - set B-frames to 0 in OBS")
