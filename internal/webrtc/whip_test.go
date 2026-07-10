package webrtc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// TestValidateSDP_BFrameGate locks in the B-frame latency gate that WHIP ingest
// applies to untrusted OBS offers. Main/High/Extended H.264 profiles can carry
// B-frames, which browsers reorder — adding 2+ seconds of latency — so they must
// be rejected; Baseline (and non-H.264 / profile-less offers, which negotiate
// against the Baseline-only MediaEngine) must pass.
func TestValidateSDP_BFrameGate(t *testing.T) {
	fmtp := func(profileLevelID string) string {
		return "a=rtpmap:96 H264/90000\r\n" +
			"a=fmtp:96 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=" + profileLevelID + "\r\n"
	}

	cases := []struct {
		name    string
		sdp     string
		wantErr bool
	}{
		{name: "baseline passes", sdp: fmtp("42e01f"), wantErr: false},
		{name: "constrained baseline passes", sdp: fmtp("42001f"), wantErr: false},
		{name: "main profile rejected", sdp: fmtp("4d001f"), wantErr: true},
		{name: "high profile rejected", sdp: fmtp("640c1f"), wantErr: true},
		{name: "extended profile rejected", sdp: fmtp("58001f"), wantErr: true},
		{name: "uppercase profile byte rejected", sdp: fmtp("4D001F"), wantErr: true},
		{name: "unknown profile passes with warning", sdp: fmtp("ff001f"), wantErr: false},
		{name: "h264 without profile-level-id passes", sdp: "a=rtpmap:96 H264/90000\r\n", wantErr: false},
		{name: "non-h264 passes", sdp: "a=rtpmap:96 VP8/90000\r\n", wantErr: false},
		{name: "empty sdp passes", sdp: "", wantErr: false},
		// A multi-profile offer with Baseline first must pass: the Baseline-only
		// MediaEngine negotiates Baseline, so the trailing High PT is never used.
		// Rejecting it would break legitimate encoders that advertise both.
		{name: "baseline-first multi-profile passes", sdp: fmtp("42e01f") + "a=rtpmap:98 H264/90000\r\na=fmtp:98 profile-level-id=640c1f\r\n", wantErr: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSDP(tc.sdp)
			if tc.wantErr {
				if !errors.Is(err, ErrBFramesDetected) {
					t.Errorf("validateSDP(%q) = %v, want ErrBFramesDetected", tc.name, err)
				}
			} else if err != nil {
				t.Errorf("validateSDP(%q) = %v, want nil", tc.name, err)
			}
		})
	}
}

// TestKeyPrefix_NoPanicOnShortTokens is a regression test for a process-crashing
// DoS. The WHIP handler logged stream-key prefixes via token[:8], but the token
// arrives from the URL path with no length guarantee — a POST /whip/<short>
// reaching a state-change log would panic the HTTP goroutine and take down the
// whole server. keyPrefix must never panic regardless of token length.
func TestKeyPrefix_NoPanicOnShortTokens(t *testing.T) {
	cases := []string{
		"",                                     // empty
		"a",                                    // 1 byte
		"ab",                                   // 2 bytes
		"abcdef",                               // 6 bytes (< 8)
		"abcdefg",                              // 7 bytes (< 8)
		"abcdefgh",                             // exactly 8
		"abcdefghijklmnopqrstuvwxyz0123456789", // long
	}
	for _, token := range cases {
		got := keyPrefix(token)
		// Must be a prefix of the token and never longer than 8.
		if len(got) > 8 {
			t.Errorf("keyPrefix(%q) = %q, length > 8", token, got)
		}
		if len(got) > len(token) {
			t.Errorf("keyPrefix(%q) = %q, longer than input", token, got)
		}
	}
	// A normal token is truncated to 8 chars.
	if got := keyPrefix("abcdefghijklmnopqrstuvwxyz"); got != "abcdefgh" {
		t.Errorf("keyPrefix(long) = %q, want %q", got, "abcdefgh")
	}
}

// TestIngestICECandidateWindow_Resets verifies the WHIP trickle-ICE candidate
// budget is a sliding window, not a monotonically-growing lifetime counter.
// Previously a long-lived OBS session that regathered candidates (network
// changes, ICE restarts) would climb to MaxICECandidates and then reject every
// later candidate for the rest of the session, silently breaking connectivity.
// Now the budget resets every whipCandidateWindow.
func TestIngestICECandidateWindow_Resets(t *testing.T) {
	session := &IngestSession{}

	// Fill the window to the cap.
	for i := 0; i < MaxICECandidates; i++ {
		now := time.Now()
		session.iceMu.Lock()
		if now.Sub(session.iceWindowStart) >= whipCandidateWindow {
			session.iceWindowStart = now
			session.iceCandidateCount = 0
		}
		if session.iceCandidateCount >= MaxICECandidates {
			session.iceMu.Unlock()
			t.Fatalf("candidate %d rejected within the first window", i)
		}
		session.iceCandidateCount++
		session.iceMu.Unlock()
	}

	// At the cap now — a candidate in the SAME window is rejected.
	session.iceMu.Lock()
	atCap := session.iceCandidateCount >= MaxICECandidates
	session.iceMu.Unlock()
	if !atCap {
		t.Fatal("expected budget to be at cap after filling the window")
	}

	// After the window elapses, the budget resets and a candidate is accepted.
	session.iceWindowStart = time.Now().Add(-whipCandidateWindow - time.Second)
	now := time.Now()
	session.iceMu.Lock()
	if now.Sub(session.iceWindowStart) >= whipCandidateWindow {
		session.iceWindowStart = now
		session.iceCandidateCount = 0
	}
	accepted := session.iceCandidateCount < MaxICECandidates
	session.iceMu.Unlock()
	if !accepted {
		t.Fatal("candidate budget did not reset after the window elapsed (long-session connectivity bug)")
	}
}

// opusFmtpFromSDP returns the fmtp value for the Opus payload type in an SDP
// (matched via a=rtpmap:<pt> opus/...), or "" if the SDP carries no Opus codec
// or no Opus fmtp line. Shared by the WHIP and SFU stereo-negotiation tests.
func opusFmtpFromSDP(sdp string) string {
	opusPTs := make(map[string]bool)
	for _, raw := range strings.Split(sdp, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "a=rtpmap:") {
			continue
		}
		rest := strings.TrimPrefix(line, "a=rtpmap:")
		spaced := strings.SplitN(rest, " ", 2)
		if len(spaced) != 2 {
			continue
		}
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(spaced[1])), "opus/") {
			opusPTs[strings.TrimSpace(spaced[0])] = true
		}
	}
	for _, raw := range strings.Split(sdp, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "a=fmtp:") {
			continue
		}
		head := strings.TrimPrefix(line, "a=fmtp:")
		spaced := strings.SplitN(head, " ", 2)
		if len(spaced) != 2 {
			continue
		}
		if opusPTs[strings.TrimSpace(spaced[0])] {
			return strings.TrimSpace(spaced[1])
		}
	}
	return ""
}

// assertProgramStereoFmtp verifies the fmtp value carries the program-audio
// stereo decode parameters and intentionally no bitrate cap.
func assertProgramStereoFmtp(t *testing.T, fmtp, label string) {
	t.Helper()
	if fmtp == "" {
		t.Fatalf("%s: no Opus fmtp line found in SDP", label)
	}
	for _, want := range []string{"stereo=1", "sprop-stereo=1", "minptime=10", "useinbandfec=1"} {
		if !strings.Contains(fmtp, want) {
			t.Errorf("%s: Opus fmtp %q missing %q", label, fmtp, want)
		}
	}
	if strings.Contains(fmtp, "maxaveragebitrate") {
		t.Errorf("%s: Opus fmtp %q must not cap program bitrate with maxaveragebitrate", label, fmtp)
	}
}

// TestValidateSDP_MultichannelAudioGate locks in the multichannel ingest guard.
// The relay is a two-channel Opus path; multichannel surround (multiopus, or an
// Opus rtpmap advertising >2 channels) must be rejected with
// ErrUnsupportedAudioChannels rather than silently negotiated into a lossy
// downmix. Mono/stereo/channel-less Opus and non-Opus codecs must pass.
func TestValidateSDP_MultichannelAudioGate(t *testing.T) {
	cases := []struct {
		name    string
		sdp     string
		wantErr error
	}{
		{name: "multiopus rejected", sdp: "a=rtpmap:112 multiopus/48000/6\r\n", wantErr: ErrUnsupportedAudioChannels},
		{name: "8-channel opus rejected", sdp: "a=rtpmap:111 opus/48000/8\r\n", wantErr: ErrUnsupportedAudioChannels},
		{name: "6-channel opus rejected", sdp: "a=rtpmap:111 opus/48000/6\r\n", wantErr: ErrUnsupportedAudioChannels},
		{name: "3-channel opus rejected", sdp: "a=rtpmap:111 opus/48000/3\r\n", wantErr: ErrUnsupportedAudioChannels},
		{name: "stereo opus passes", sdp: "a=rtpmap:111 opus/48000/2\r\n", wantErr: nil},
		{name: "mono opus passes", sdp: "a=rtpmap:111 opus/48000/1\r\n", wantErr: nil},
		{name: "channel-less opus passes", sdp: "a=rtpmap:111 opus/48000\r\n", wantErr: nil},
		{name: "video only passes", sdp: "a=rtpmap:96 VP8/90000\r\n", wantErr: nil},
		{name: "empty sdp passes", sdp: "", wantErr: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSDP(tc.sdp)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("validateSDP(%q) = %v, want %v", tc.name, err, tc.wantErr)
				}
			} else if err != nil {
				t.Errorf("validateSDP(%q) = %v, want nil", tc.name, err)
			}
		})
	}
}

// TestWHIPHandler_AnswerNegotiatesStereoOpus drives a real WHIP POST with a
// Pion-generated audio offer and asserts the answer carries the program-audio
// Opus stereo decode parameters (stereo=1;sprop-stereo=1, no bitrate cap), and
// that the stored ingest relay track exposes the matching two-channel stereo
// capability. This is the end-to-end lock that OBS cannot be made to downmix
// program audio to mono through Chromatic's relay.
func TestWHIPHandler_AnswerNegotiatesStereoOpus(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	handler := NewWHIPHandler(sfu,
		func(context.Context, string) (bool, error) { return true, nil },
		func(string) error { return nil },
		func(string) {},
	)

	// Build a real OBS-like offer: sendonly audio (stereo Opus) + sendonly video.
	offerPC, err := sfu.CreatePeerConnection()
	if err != nil {
		t.Fatalf("failed to create offer PC: %v", err)
	}
	defer offerPC.Close()
	for _, kind := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
		if _, err := offerPC.AddTransceiverFromKind(kind, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionSendonly,
		}); err != nil {
			t.Fatalf("failed to add %s transceiver: %v", kind, err)
		}
	}
	offer, err := offerPC.CreateOffer(nil)
	if err != nil {
		t.Fatalf("failed to create offer: %v", err)
	}
	if err := offerPC.SetLocalDescription(offer); err != nil {
		t.Fatalf("failed to set local description: %v", err)
	}

	token := "stereo-opus-token"
	req := httptest.NewRequest(http.MethodPost, "/whip/"+token, strings.NewReader(offer.SDP))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 from WHIP offer, got %d: %s", rec.Code, rec.Body.String())
	}

	// The answer must negotiate Opus with the program-audio stereo parameters.
	answerSDP := rec.Body.String()
	assertProgramStereoFmtp(t, opusFmtpFromSDP(answerSDP), "WHIP answer")

	// The stored ingest relay track must carry the matching two-channel stereo
	// capability so Pion's rewriter routes RTP without a codec probe.
	session := sfu.GetIngest(token)
	if session == nil {
		t.Fatal("ingest session should be registered after offer")
	}
	if session.AudioTrack == nil {
		t.Fatal("ingest session audio relay track should be set")
	}
	codec := session.AudioTrack.Codec()
	if codec.Channels != 2 {
		t.Errorf("ingest audio relay Channels = %d, want 2", codec.Channels)
	}
	if codec.SDPFmtpLine != ProgramAudioOpusFmtp {
		t.Errorf("ingest audio relay fmtp = %q, want %q", codec.SDPFmtpLine, ProgramAudioOpusFmtp)
	}
}

// TestWHIPHandler_RejectsMultichannelIngest verifies the WHIP offer path
// returns HTTP 422 (not a silent two-channel negotiation) for a surround offer,
// so OBS is told to reconfigure rather than having its mix silently collapsed.
func TestWHIPHandler_RejectsMultichannelIngest(t *testing.T) {
	cfg := createTestConfig()
	sfu, err := NewSFU(cfg)
	if err != nil {
		t.Fatalf("failed to create SFU: %v", err)
	}
	defer sfu.Shutdown()

	handler := NewWHIPHandler(sfu,
		func(context.Context, string) (bool, error) { return true, nil },
		func(string) error { return nil },
		func(string) {},
	)

	multiopusOffer := "v=0\r\n" +
		"o=- 1 1 IN IP4 0.0.0.0\r\n" +
		"s=-\r\n" +
		"t=0 0\r\n" +
		"m=audio 9 UDP/TLS/RTP/SAVPF 112\r\n" +
		"a=rtpmap:112 multiopus/48000/6\r\n"

	req := httptest.NewRequest(http.MethodPost, "/whip/multiopus-token", strings.NewReader(multiopusOffer))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for multichannel ingest, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), ErrUnsupportedAudioChannels.Error()) {
		t.Errorf("expected body to mention unsupported audio channels, got: %s", rec.Body.String())
	}
}
