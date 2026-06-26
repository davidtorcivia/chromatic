// Audio mode + denoiser engine selection for the talkback (voice-chat) path.
//
// Talkback (default): studio-grade voice cleanup at ultra-low latency — native
// AEC (system-preferred), our own denoiser handling noise reduction, Opus mono
// with DTX/FEC at a voice bitrate.
//
// Studio / Critical-Listening: pristine, full-bandwidth stereo for a mic that
// is carrying reference music or an instrument (the "Sound & Composition" use
// case). No noise suppression / AGC / gate; high-bitrate stereo Opus. Echo
// cancellation stays on by default (laptop-speaker safety) unless the user
// confirms they are on headphones, in which case it is removed for maximum
// fidelity.
//
// The program/content audio (WHIP/OBS ingest) NEVER flows through this path —
// it is a pristine Opus relay and must stay untouched. Everything here only
// affects a participant's own microphone capture.

export type AudioMode = 'talkback' | 'studio';

// Which noise-reduction engine the talkback path uses. 'rnnoise' is the
// ultra-low-latency neural denoiser; 'off' disables in-app noise reduction
// (the browser's native noise suppression is used instead).
export type DenoiserEngine = 'rnnoise' | 'off';

export interface AudioModeState {
    mode: AudioMode;
    // Denoiser used in talkback mode. Ignored in studio mode.
    denoiser: DenoiserEngine;
    // Studio only: user has confirmed headphones, so echo cancellation is
    // disabled for maximum fidelity.
    studioHeadphones: boolean;
}

export const DEFAULT_AUDIO_MODE_STATE: AudioModeState = {
    mode: 'talkback',
    denoiser: 'rnnoise',
    studioHeadphones: false
};

// Per-mode Opus encoder preferences. Applied to the publisher offer (fmtp
// munge) and to the RTCRtpSender encoding (maxBitrate). Opus default ptime
// (20 ms) and minptime=10 are kept as-is; both are honored without munging.
export interface OpusPreferences {
    stereo: boolean;
    dtx: boolean; // fmtp usedtx
    fec: boolean; // fmtp useinbandfec
    maxAverageBitrate: number; // bps, fmtp maxaveragebitrate (receiver hint)
    maxBitrate: number; // bps, RTCRtpSender encodings[0].maxBitrate (send cap)
}

// Voice: mono, DTX (silence is cheap), FEC for packet-loss resilience, a
// modest bitrate that is plenty for clean speech.
export const TALKBACK_OPUS: OpusPreferences = {
    stereo: false,
    dtx: true,
    fec: true,
    maxAverageBitrate: 40_000,
    maxBitrate: 48_000
};

// Studio: full-bandwidth stereo, DTX off (don't gate musical tails), FEC on,
// a high bitrate for transparent music/instrument reproduction.
export const STUDIO_OPUS: OpusPreferences = {
    stereo: true,
    dtx: false,
    fec: true,
    maxAverageBitrate: 256_000,
    maxBitrate: 256_000
};

export function opusPreferencesFor(mode: AudioMode): OpusPreferences {
    return mode === 'studio' ? STUDIO_OPUS : TALKBACK_OPUS;
}

const STORAGE_KEY = 'chromatic_audio_mode';

export function loadAudioModeState(): AudioModeState {
    try {
        const raw = localStorage.getItem(STORAGE_KEY);
        if (!raw) return { ...DEFAULT_AUDIO_MODE_STATE };
        const parsed = JSON.parse(raw) as Partial<AudioModeState>;
        return {
            mode: parsed.mode === 'studio' ? 'studio' : 'talkback',
            denoiser: parsed.denoiser === 'off' ? 'off' : 'rnnoise',
            studioHeadphones: parsed.studioHeadphones === true
        };
    } catch {
        return { ...DEFAULT_AUDIO_MODE_STATE };
    }
}

export function saveAudioModeState(state: AudioModeState): void {
    try {
        localStorage.setItem(STORAGE_KEY, JSON.stringify(state));
    } catch {
        // Storage unavailable (private mode) — the in-session choice still applies.
    }
}

// "Join with camera on" preference, set in the waiting-room green room. When
// true, the session auto-enables the presence cam on join (otherwise cameras
// stay off until the user opts in via the mic→camera nudge). Persisted so the
// choice carries across joins.
const JOIN_CAM_KEY = 'chromatic_join_cam';

export function getJoinWithCamera(): boolean {
    try {
        return localStorage.getItem(JOIN_CAM_KEY) === '1';
    } catch {
        return false;
    }
}

export function setJoinWithCamera(on: boolean): void {
    try {
        localStorage.setItem(JOIN_CAM_KEY, on ? '1' : '0');
    } catch {
        // Storage unavailable — the in-session default (off) applies.
    }
}
