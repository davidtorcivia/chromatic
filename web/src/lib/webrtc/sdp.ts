// SDP munging helpers for per-mode Opus tuning.
//
// We adjust only Opus fmtp PARAMETERS (stereo, usedtx, useinbandfec,
// maxaveragebitrate, minptime) on the publisher's local offer. We never touch
// m-lines, payload ordering, or rtpmap — that is what wedges Chrome/Safari
// renegotiation. Editing fmtp params is the well-trodden, safe lever for
// steering the browser's own Opus encoder per mode (mono talkback vs stereo
// hi-fi studio).

import type { OpusPreferences } from '$lib/audio/audio-mode';

// Merge `params` into an existing fmtp value string ("a=b;c=d"), overriding
// keys that already exist and appending new ones, preserving order otherwise.
function mergeFmtpParams(existing: string, params: Record<string, string>): string {
    const order: string[] = [];
    const map = new Map<string, string>();
    for (const part of existing.split(';')) {
        const t = part.trim();
        if (!t) continue;
        const eq = t.indexOf('=');
        const key = eq === -1 ? t : t.slice(0, eq).trim();
        const val = eq === -1 ? '' : t.slice(eq + 1).trim();
        if (!map.has(key)) order.push(key);
        map.set(key, val);
    }
    for (const [key, val] of Object.entries(params)) {
        if (!map.has(key)) order.push(key);
        map.set(key, val);
    }
    return order.map((k) => (map.get(k) === '' ? k : `${k}=${map.get(k)}`)).join(';');
}

// Returns a copy of `sdp` with Opus fmtp parameters set to reflect `prefs`.
// If no Opus codec is present the SDP is returned unchanged.
export function applyOpusPreferences(sdp: string, prefs: OpusPreferences): string {
    if (!sdp) return sdp;

    // Match line endings of the source (WebRTC uses CRLF; tests may use LF).
    const eol = sdp.includes('\r\n') ? '\r\n' : '\n';
    const lines = sdp.split(/\r\n|\n/);

    // Collect the payload type(s) mapped to Opus.
    const opusPts = new Set<string>();
    for (const line of lines) {
        const m = /^a=rtpmap:(\d+)\s+opus\/48000(?:\/\d+)?/i.exec(line);
        if (m) opusPts.add(m[1]);
    }
    if (opusPts.size === 0) return sdp;

    const params: Record<string, string> = {
        minptime: '10',
        useinbandfec: prefs.fec ? '1' : '0',
        usedtx: prefs.dtx ? '1' : '0',
        stereo: prefs.stereo ? '1' : '0',
        'sprop-stereo': prefs.stereo ? '1' : '0',
        maxaveragebitrate: String(prefs.maxAverageBitrate)
    };

    const out: string[] = [];
    const haveFmtp = new Set<string>();

    for (const line of lines) {
        const m = /^a=fmtp:(\d+)\s+(.*)$/.exec(line);
        if (m && opusPts.has(m[1])) {
            haveFmtp.add(m[1]);
            out.push(`a=fmtp:${m[1]} ${mergeFmtpParams(m[2], params)}`);
        } else {
            out.push(line);
        }
    }

    // Some encoders omit the fmtp line; add one right after the rtpmap for any
    // Opus PT that lacked it so our preferences still take effect.
    const missing = [...opusPts].filter((pt) => !haveFmtp.has(pt));
    if (missing.length > 0) {
        const fmtpValue = mergeFmtpParams('', params);
        const withFmtp: string[] = [];
        for (const line of out) {
            withFmtp.push(line);
            const m = /^a=rtpmap:(\d+)\s+opus\/48000/i.exec(line);
            if (m && missing.includes(m[1])) {
                withFmtp.push(`a=fmtp:${m[1]} ${fmtpValue}`);
            }
        }
        return withFmtp.join(eol);
    }

    return out.join(eol);
}

// Receive-side Opus decode parameters the subscriber answer must advertise so
// the browser decodes the program stream as full stereo — not the RFC 7587 mono
// default — with inband FEC for packet-loss resilience. These are DECODE
// parameters only: we deliberately do NOT include `usedtx` (it would let the
// decoder gate program audio) or `maxaveragebitrate` (it would cap the sender's
// program bitrate, defeating the uncapped relay). Stereo program audio (dialogue,
// music, the full mix) must reach the reviewer untouched; this helper is the
// browser-side bookend to the SFU's stereo Opus relay offer.
const SUBSCRIBER_ANSWER_OPUS_PARAMS: Record<string, string> = {
    minptime: '10',
    useinbandfec: '1',
    stereo: '1',
    'sprop-stereo': '1'
};

// tuneSubscriberAnswerOpus ensures the subscriber's ANSWER advertises full-
// stereo Opus decode for the program stream. Applied to the browser's own
// createAnswer() SDP before setLocalDescription, on both the initial server
// offer and server-initiated renegotiation. Only merges the four receive-side
// params above into the Opus fmtp — never adds sender caps, DTX, or a bitrate
// ceiling, and never touches m-lines, payload ordering, or rtpmap. Returns the
// SDP unchanged when no Opus codec is present.
export function tuneSubscriberAnswerOpus(sdp: string): string {
    if (!sdp) return sdp;

    // Match line endings of the source (WebRTC uses CRLF; tests may use LF).
    const eol = sdp.includes('\r\n') ? '\r\n' : '\n';
    const lines = sdp.split(/\r\n|\n/);

    // Collect the payload type(s) mapped to Opus.
    const opusPts = new Set<string>();
    for (const line of lines) {
        const m = /^a=rtpmap:(\d+)\s+opus\/48000(?:\/\d+)?/i.exec(line);
        if (m) opusPts.add(m[1]);
    }
    if (opusPts.size === 0) return sdp;

    const out: string[] = [];
    const haveFmtp = new Set<string>();

    for (const line of lines) {
        const m = /^a=fmtp:(\d+)\s+(.*)$/.exec(line);
        if (m && opusPts.has(m[1])) {
            haveFmtp.add(m[1]);
            out.push(`a=fmtp:${m[1]} ${mergeFmtpParams(m[2], SUBSCRIBER_ANSWER_OPUS_PARAMS)}`);
        } else {
            out.push(line);
        }
    }

    // Some answers omit the fmtp line; add one right after the rtpmap for any
    // Opus PT that lacked it so the stereo decode params still take effect.
    const missing = [...opusPts].filter((pt) => !haveFmtp.has(pt));
    if (missing.length > 0) {
        const fmtpValue = mergeFmtpParams('', SUBSCRIBER_ANSWER_OPUS_PARAMS);
        const withFmtp: string[] = [];
        for (const line of out) {
            withFmtp.push(line);
            const m = /^a=rtpmap:(\d+)\s+opus\/48000/i.exec(line);
            if (m && missing.includes(m[1])) {
                withFmtp.push(`a=fmtp:${m[1]} ${fmtpValue}`);
            }
        }
        return withFmtp.join(eol);
    }

    return out.join(eol);
}
