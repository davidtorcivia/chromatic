package webrtc

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
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

// keyPrefix returns a short, log-safe prefix of a stream-key token. The token
// comes from the URL path and is untrusted in length; slicing token[:8]
// directly panics on tokens shorter than 8 bytes (a panic in an HTTP handler
// goroutine crashes the whole process, so any POST /whip/<short> would be a
// trivial DoS). Mirrors the min(8, len) guard used in sfu.go.
func keyPrefix(token string) string {
	if len(token) > 8 {
		return token[:8]
	}
	return token
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

	// If an existing ingest is holding this stream key (typically an OBS
	// instance reconnecting before its prior PC timed out), replace it rather
	// than returning 409 — the new OBS connection IS the canonical sender.
	// SetIngest below will close the old PC; subscriber connections keep
	// their senders and will be rebound via BindIngestToRoom when the new PC
	// reaches Connected.

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

	// Create local tracks for forwarding.
	// The capability here must match what the MediaEngine registered in NewSFU
	// (H.264 PM=1 Constrained Baseline, Opus 48k stereo) so Pion's rewriter can
	// route RTP cleanly without probing for a compatible codec on first packet.
	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
		},
		"video",
		"chromatic-stream",
	)
	if err != nil {
		pc.Close()
		log.Printf("Failed to create video track: %v", err)
		http.Error(w, "Failed to create video track", http.StatusInternalServerError)
		return
	}

	// Program-audio relay track. Uses the shared ProgramAudioOpusCapability()
	// (see opus.go) so the relay stays in lockstep with the MediaEngine
	// registration in NewSFU — stereo=1;sprop-stereo=1, no bitrate cap — and
	// OBS's program audio is carried/decoded as full stereo, not mono.
	audioTrack, err := webrtc.NewTrackLocalStaticRTP(
		ProgramAudioOpusCapability(),
		"audio",
		"chromatic-stream",
	)

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

		// Decouple the OBS Read loop from WriteRTP fan-out. pion implements
		// TrackLocalStaticRTP.WriteRTP as a synchronous per-subscriber encrypt +
		// UDP socket write, so a single congested viewer's full send buffer
		// would otherwise block this goroutine, back up OBS's read buffer, and
		// stall media (and keyframes) for EVERY viewer. The forwarder queues
		// packets and drains on its own goroutine; on a full buffer it drops the
		// OLDEST packet (a stale live frame is worthless) so the ingest read
		// never blocks and latency stays bounded to the buffer depth.
		// Video: shallow buffer (drop stale frames fast). Audio: deeper (small
		// packets; prefer buffering over dropping talk spurts).
		// Video buffer sized to absorb a full keyframe burst plus fan-out
		// jitter: at 1080p48/8Mbps a keyframe alone can exceed 100 packets, so
		// the old 32-packet depth guaranteed mid-keyframe drops whenever the
		// synchronous WriteRTP drain hitched (a slow subscriber socket, GC, or
		// scheduler stall) — corrupting frames for EVERY viewer at once. A
		// stale frame is still dropped-oldest, so latency stays bounded.
		forwarderBufSize := 512
		if remoteTrack.Kind() == webrtc.RTPCodecTypeAudio {
			forwarderBufSize = 256
		}
		forwarder := newAsyncForwarder(forwarderBufSize, localTrack.WriteRTP)

		// DIAGNOSTIC: report ingest packets dropped by the forwarder. A nonzero
		// delta means the fan-out drain stalled and buffer-full drops corrupted
		// frames for all viewers simultaneously — the signature of sync stutter.
		go func() {
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			var last int64
			for {
				select {
				case <-session.done:
					return
				case <-ticker.C:
					if d := forwarder.Dropped(); d != last {
						log.Printf("WHIP ingest forwarder dropped %d packets (+%d/2s, kind=%s, key=%s...)",
							d, d-last, remoteTrack.Kind(), keyPrefix(token))
						last = d
					}
				}
			}
		}()

		// Forward RTP packets. Exits when the peer connection closes (Read
		// returns an error) or when session teardown closes session.done.
		go func() {
			defer forwarder.Close()
			buf := make([]byte, 1500)
			for {
				n, _, err := remoteTrack.Read(buf)
				if err != nil {
					if err != io.EOF {
						log.Printf("Error reading from remote track: %v", err)
					}
					return
				}
				// Non-blocking done-check so a teardown between Read and Write
				// aborts promptly instead of writing into a dead pipe.
				select {
				case <-session.done:
					return
				default:
				}
				// Hand the raw packet to the forwarder (never blocks; drop-oldest
				// on full). Unmarshal + extension stripping + WriteRTP happen on
				// the forwarder's drain goroutine, off the read loop.
				forwarder.write(buf[:n])
			}
		}()
	})

	// Teardown runs exactly once per session across Failed/Closed state
	// callbacks and explicit DELETE requests, preventing duplicate
	// stream-end broadcasts and negative ingest metrics.
	// removeIngestIfSame is used (rather than RemoveIngest) so that if OBS has
	// already reconnected and a fresh ingest session is live under this token,
	// a stale callback on the old PC doesn't delete the replacement.
	var teardownOnce sync.Once
	session.teardown = func() {
		teardownOnce.Do(func() {
			// Only adjust the metric if this session ever counted itself in.
			if session.everConnected.Load() {
				metrics.Get().ActiveWHIPIngests.Add(-1)
			}
			h.sfu.removeIngestIfSame(token, session)
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
		log.Printf("WHIP connection state: %s (key: %s...)", state, keyPrefix(token))

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
	// PATCH requests arriving during gathering can find it. SetIngest also
	// closes any previous ingest holding this stream key (OBS reconnect).
	h.sfu.SetIngest(token, session)

	// Bound the wait for ICE gathering (see iceGatherTimeout). Host/srflx
	// candidates gather quickly and are sufficient for OBS-to-server
	// connectivity; a slow TURN server must not stall the WHIP response past
	// client/server write timeouts.
	waitForICEGather(pc)

	// Snapshot the answer SDP. LocalDescription can return nil if the peer
	// connection is closed between SetLocalDescription and here; capture the
	// string while we know it's valid.
	localDesc := pc.LocalDescription()
	if localDesc == nil {
		pc.Close()
		h.sfu.removeIngestIfSame(token, session)
		log.Printf("LocalDescription unexpectedly nil after gather")
		http.Error(w, "Failed to finalize answer", http.StatusInternalServerError)
		return
	}
	answerSDP := localDesc.SDP

	// Return the answer
	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", r.URL.String())
	w.WriteHeader(http.StatusCreated)
	if _, err := w.Write([]byte(answerSDP)); err != nil {
		log.Printf("Failed to write WHIP answer: %v", err)
	}

	log.Printf("WHIP session established for key: %s...", keyPrefix(token))
}

// handleICETrickle handles ICE candidate trickling
func (h *WHIPHandler) handleICETrickle(w http.ResponseWriter, r *http.Request, token string) {
	session := h.sfu.GetIngest(token)
	if session == nil {
		http.Error(w, "Session not found", http.StatusNotFound)
		return
	}

	// Sliding-window limit to prevent ICE candidate flooding without permanently
	// blocking a long-lived session that legitimately regathers candidates
	// (network transitions, TURN rotation). The window resets every minute.
	now := time.Now()
	session.iceMu.Lock()
	if now.Sub(session.iceWindowStart) >= whipCandidateWindow {
		session.iceWindowStart = now
		session.iceCandidateCount = 0
	}
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
	// Atomically take (lookup + remove under one lock) the session. This closes
	// the TOCTOU window that a separate GetIngest + teardown had: a concurrent
	// OBS reconnect (SetIngest) could replace the session between the two, and
	// the DELETE would then close a stale PC, leave the live replacement
	// untouched, and return a misleading 204 for a session it didn't actually
	// remove. With TakeIngest, either we get the exact session we remove (and
	// tear it down) or we get nil (it was already replaced → 404, not 204).
	session := h.sfu.TakeIngest(token)
	if session == nil {
		http.Error(w, "Session not found", http.StatusNotFound)
		return
	}

	session.PeerConnection.Close()

	// Run the shared teardown (idempotent): notifies stream end exactly once,
	// even though Close() also fires the Closed state callback asynchronously.
	// The session is already removed from the map by TakeIngest, so teardown's
	// removeIngestIfSame is a harmless no-op; it still drives the metric
	// decrement and onStreamEnd (guarded by teardownOnce).
	if session.teardown != nil {
		session.teardown()
	}

	w.WriteHeader(http.StatusNoContent)
	log.Printf("WHIP session terminated for key: %s...", keyPrefix(token))
}

// validateSDP validates the incoming SDP offer. It rejects two classes of
// configuration that the SFU cannot carry faithfully:
//   - B-frame-capable H.264 profiles (Main/High/Extended), which add 2+ s of
//     reorder latency in browsers.
//   - Multichannel/surround audio (multiopus, or Opus with >2 channels). The
//     SFU relay is a two-channel Opus path; forcing multichannel through it
//     would downmix at the source and violate the "relay program audio
//     untouched" contract. True surround needs a dedicated multiopus path.
func validateSDP(sdp string) error {
	// Reject multichannel/surround audio before anything else: the relay is a
	// two-channel Opus path and must never silently negotiate a downmix.
	if err := validateAudioChannels(sdp); err != nil {
		return err
	}

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
		const key = "profile-level-id="
		for _, param := range params {
			param = strings.TrimSpace(param)
			if len(param) > len(key) && strings.EqualFold(param[:len(key)], key) {
				return param[len(key):]
			}
		}
	}
	return ""
}

// ErrBFramesDetected indicates B-frames are configured
var ErrBFramesDetected = errors.New("B-frames detected: Main/High profile can use B-frames which cause 2+ second latency. Set encoder to Baseline profile or disable B-frames in OBS")

// ErrUnsupportedAudioChannels indicates the offer advertises audio the relay
// cannot carry without downmixing (multichannel surround via multiopus, or an
// Opus channel count above two). Forcing such a stream through the two-channel
// Opus relay would violate the "relay program audio untouched" contract, so the
// WHIP offer is rejected with 422 instead of being silently negotiated down.
var ErrUnsupportedAudioChannels = errors.New("unsupported audio channels: multichannel/surround ingest (multiopus or >2-channel Opus) is not supported; configure OBS for stereo or mono")

// validateAudioChannels rejects audio codec advertisements the two-channel Opus
// relay cannot faithfully carry. multichannel surround over WebRTC uses a
// separate "multiopus" codec, which is draft/non-default and not registered in
// this SFU; an Opus rtpmap advertising more than two channels is malformed for
// this relay. Either would be silently downmixed if negotiated, so they are
// rejected up front with a clear error rather than accepted as a lossy mono
// fallback. Offered codec name + channel count are logged for diagnosis.
func validateAudioChannels(sdp string) error {
	for _, raw := range strings.Split(sdp, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "a=rtpmap:") {
			continue
		}
		// a=rtpmap:<payload-type> <codec>/<clockrate>[/<channels>]
		rest := strings.TrimPrefix(line, "a=rtpmap:")
		spaced := strings.SplitN(rest, " ", 2)
		if len(spaced) != 2 {
			continue
		}
		desc := strings.Split(strings.TrimSpace(spaced[1]), "/")
		codec := strings.ToLower(desc[0])
		if codec == "multiopus" {
			log.Printf("Rejecting multichannel ingest: %s", strings.TrimSpace(spaced[1]))
			return ErrUnsupportedAudioChannels
		}
		if codec == "opus" && len(desc) >= 3 {
			channels := strings.TrimSpace(desc[2])
			if n, err := strconv.Atoi(channels); err == nil && n > 2 {
				log.Printf("Rejecting multichannel ingest: opus/%s/%s", desc[1], channels)
				return ErrUnsupportedAudioChannels
			}
		}
	}
	return nil
}
