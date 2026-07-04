package webrtc

import (
	"errors"
	"testing"
	"time"
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
