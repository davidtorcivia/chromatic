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

// The msid stream name the SFU gives the OBS program-audio relay track (see
// whip.go — NewTrackLocalStaticRTP(..., "audio", "chromatic-stream")). Voice,
// webcam and screen-share relays use per-participant names, so this is what
// distinguishes the one m-line that carries program audio.
const PROGRAM_STREAM_ID = 'chromatic-stream';

// Split an SDP into its session-level preamble and per-m-line sections.
function splitMediaSections(lines: string[]): { preamble: string[]; sections: string[][] } {
    const preamble: string[] = [];
    const sections: string[][] = [];
    for (const line of lines) {
        if (line.startsWith('m=')) {
            sections.push([line]);
        } else if (sections.length === 0) {
            preamble.push(line);
        } else {
            sections[sections.length - 1].push(line);
        }
    }
    return { preamble, sections };
}

// findProgramAudioMid locates the mid of the m-line carrying OBS program audio
// in a server OFFER, by looking for the relay's msid stream name. Returns null
// when the offer has no such m-line (older server, or an offer that carries no
// program audio yet) — callers then fall back to tuning every Opus m-line.
export function findProgramAudioMid(offerSdp: string): string | null {
    if (!offerSdp) return null;
    const { sections } = splitMediaSections(offerSdp.split(/\r\n|\n/));
    for (const section of sections) {
        if (!section[0].startsWith('m=audio')) continue;
        if (!section.some((l) => l.startsWith('a=msid:') && l.includes(PROGRAM_STREAM_ID))) continue;
        const midLine = section.find((l) => l.startsWith('a=mid:'));
        if (midLine) return midLine.slice('a=mid:'.length).trim();
    }
    return null;
}

// tuneSubscriberAnswerOpus ensures the subscriber's ANSWER advertises full-
// stereo Opus decode for the program stream. Applied to the browser's own
// createAnswer() SDP before setLocalDescription, on both the initial server
// offer and server-initiated renegotiation. Only merges the four receive-side
// params above into the Opus fmtp — never adds sender caps, DTX, or a bitrate
// ceiling, and never touches m-lines, payload ordering, or rtpmap. Returns the
// SDP unchanged when no Opus codec is present.
//
// `programMid` scopes the edit to the single m-line carrying program audio.
// This matters because every audio m-line shares one Opus payload type: without
// scoping we also stamped stereo=1;sprop-stereo=1 onto every *voice* m-line
// (which are mono) and synthesized a=fmtp lines onto m-lines that had none.
// Renegotiation offers are exactly where new voice m-lines appear, and adding
// lines the browser's own createAnswer did not produce is the kind of munge
// Chrome rejects outright — which used to destroy the whole subscription.
// Omitting `programMid` keeps the original whole-SDP behavior, so a failure to
// identify the program m-line degrades to "tune everything" rather than
// silently dropping the program stream to mono.
export function tuneSubscriberAnswerOpus(sdp: string, programMid?: string | null): string {
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

    const { preamble, sections } = splitMediaSections(lines);
    // Only scope when the requested mid is actually present; a stale mid must
    // not result in an answer with no stereo advertised anywhere.
    const scoped =
        programMid != null &&
        sections.some((s) => s.some((l) => l.trim() === `a=mid:${programMid}`));

    const tuneSection = (section: string[]): string[] => {
        const out: string[] = [];
        const haveFmtp = new Set<string>();
        for (const line of section) {
            const m = /^a=fmtp:(\d+)\s+(.*)$/.exec(line);
            if (m && opusPts.has(m[1])) {
                haveFmtp.add(m[1]);
                out.push(`a=fmtp:${m[1]} ${mergeFmtpParams(m[2], SUBSCRIBER_ANSWER_OPUS_PARAMS)}`);
            } else {
                out.push(line);
            }
        }

        // Some answers omit the fmtp line; add one right after the rtpmap for
        // any Opus PT that lacked it so the stereo decode params still apply.
        const missing = [...opusPts].filter((pt) => !haveFmtp.has(pt));
        if (missing.length === 0) return out;

        const fmtpValue = mergeFmtpParams('', SUBSCRIBER_ANSWER_OPUS_PARAMS);
        const withFmtp: string[] = [];
        for (const line of out) {
            withFmtp.push(line);
            const m = /^a=rtpmap:(\d+)\s+opus\/48000/i.exec(line);
            if (m && missing.includes(m[1])) {
                withFmtp.push(`a=fmtp:${m[1]} ${fmtpValue}`);
            }
        }
        return withFmtp;
    };

    const tuned = sections.map((section) => {
        if (scoped && !section.some((l) => l.trim() === `a=mid:${programMid}`)) return section;
        return tuneSection(section);
    });

    return [...preamble, ...tuned.flat()].join(eol);
}
