package webrtc

import "github.com/pion/webrtc/v4"

// Program-audio Opus relay capability.
//
// Program/content audio (the WHIP/OBS ingest that carries the Resolve playback
// — dialogue, music, the full mix) is relayed as opaque RTP through the SFU.
// We do not decode, remix, resample, or gain-stage it server-side. The only
// thing the SFU commits to is the codec capability the relay tracks advertise,
// and that capability MUST be identical in two places or Pion's RTP rewriter
// cannot route packets without probing for a compatible codec on first packet:
//
//   1. NewSFU's MediaEngine registration (what the SFU negotiates against).
//   2. The WHIP ingest relay track created in handleOffer (what OBS writes to).
//
// This is the single source of truth for that capability. Both production call
// sites use ProgramAudioOpusCapability(); the fmtp never appears as a second
// literal. Cross-package tests (e.g. the WebSocket stream-start/rejoin fixtures
// in internal/api/handlers) also reference it so the test harness mirrors the
// production relay exactly.
//
// stereo=1;sprop-stereo=1 is essential for a post-production app: without them
// Opus negotiates MONO (RFC 7587 defaults stereo to 0), so the SFU's answer to
// OBS would make OBS downmix the program audio to mono at the source and the
// relay offer would make browsers decode mono. With these set, program audio
// stays full stereo end-to-end. We intentionally set NO maxaveragebitrate here
// so the program path is never capped — OBS's own (high) bitrate passes through
// the opaque RTP relay untouched. Per-mode voice bitrate is capped client-side.
//
// Surround/multichannel is deliberately NOT supported by this capability. True
// surround needs a separate multiopus registration end to end; forcing it
// through this two-channel Opus path would downmix at the source and violate
// the "relay program audio untouched" contract. validateSDP rejects multichannel
// ingest (multiopus or >2-channel Opus) with ErrUnsupportedAudioChannels.

// ProgramAudioOpusFmtp is the fmtp line the program-audio Opus relay advertises
// everywhere it is negotiated. See the package comment in opus.go for why each
// parameter is present and why maxaveragebitrate is intentionally absent.
const ProgramAudioOpusFmtp = "minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1"

// ProgramAudioOpusCapability is the shared program-audio Opus relay capability.
// Used by NewSFU (MediaEngine registration, PayloadType 111) and by the WHIP
// ingest handler (relay TrackLocalStaticRTP) so the two stay byte-for-byte in
// lockstep, and by cross-package test fixtures that simulate program ingest.
func ProgramAudioOpusCapability() webrtc.RTPCodecCapability {
	return webrtc.RTPCodecCapability{
		MimeType:    webrtc.MimeTypeOpus,
		ClockRate:   48000,
		Channels:    2,
		SDPFmtpLine: ProgramAudioOpusFmtp,
	}
}
