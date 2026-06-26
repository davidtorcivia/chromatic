import { describe, it, expect } from 'vitest';
import { applyOpusPreferences } from './sdp';
import { TALKBACK_OPUS, STUDIO_OPUS } from '$lib/audio/audio-mode';

// A representative Chrome-style audio offer (Opus PT 111, default fmtp).
const OFFER = [
    'v=0',
    'o=- 1 2 IN IP4 127.0.0.1',
    's=-',
    't=0 0',
    'm=audio 9 UDP/TLS/RTP/SAVPF 111 63',
    'c=IN IP4 0.0.0.0',
    'a=rtpmap:111 opus/48000/2',
    'a=fmtp:111 minptime=10;useinbandfec=1',
    'a=rtpmap:63 red/48000/2'
].join('\r\n');

function opusFmtp(sdp: string): string {
    const line = sdp.split('\r\n').find((l) => l.startsWith('a=fmtp:111'));
    return line ?? '';
}

describe('applyOpusPreferences', () => {
    it('sets mono + DTX + voice settings for talkback', () => {
        const fmtp = opusFmtp(applyOpusPreferences(OFFER, TALKBACK_OPUS));
        expect(fmtp).toContain('stereo=0');
        expect(fmtp).toContain('sprop-stereo=0');
        expect(fmtp).toContain('usedtx=1');
        expect(fmtp).toContain('useinbandfec=1');
        expect(fmtp).toContain(`maxaveragebitrate=${TALKBACK_OPUS.maxAverageBitrate}`);
        // Existing params are preserved, not duplicated.
        expect(fmtp).toContain('minptime=10');
        expect(fmtp.match(/useinbandfec=/g)?.length).toBe(1);
    });

    it('sets stereo + high bitrate, DTX off for studio', () => {
        const fmtp = opusFmtp(applyOpusPreferences(OFFER, STUDIO_OPUS));
        expect(fmtp).toContain('stereo=1');
        expect(fmtp).toContain('sprop-stereo=1');
        expect(fmtp).toContain('usedtx=0');
        expect(fmtp).toContain(`maxaveragebitrate=${STUDIO_OPUS.maxAverageBitrate}`);
    });

    it('only touches the Opus payload, leaving other lines intact', () => {
        const out = applyOpusPreferences(OFFER, STUDIO_OPUS);
        expect(out).toContain('a=rtpmap:63 red/48000/2');
        expect(out).toContain('m=audio 9 UDP/TLS/RTP/SAVPF 111 63');
        // RED has no fmtp added.
        expect(out.split('\r\n').filter((l) => l.startsWith('a=fmtp:63'))).toHaveLength(0);
    });

    it('preserves CRLF line endings', () => {
        const out = applyOpusPreferences(OFFER, TALKBACK_OPUS);
        expect(out).toContain('\r\n');
        expect(out).not.toContain('\n\n');
    });

    it('returns SDP unchanged when no Opus codec is present', () => {
        const noOpus = 'v=0\r\nm=video 9 UDP/TLS/RTP/SAVPF 96\r\na=rtpmap:96 VP8/90000';
        expect(applyOpusPreferences(noOpus, STUDIO_OPUS)).toBe(noOpus);
    });

    it('adds an fmtp line when the encoder omitted one', () => {
        const noFmtp = ['m=audio 9 UDP/TLS/RTP/SAVPF 111', 'a=rtpmap:111 opus/48000/2'].join('\r\n');
        const out = applyOpusPreferences(noFmtp, STUDIO_OPUS);
        const fmtp = opusFmtp(out);
        expect(fmtp).toContain('stereo=1');
        expect(fmtp).toContain('useinbandfec=1');
    });
});
