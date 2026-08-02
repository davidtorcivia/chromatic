import { describe, it, expect } from 'vitest';
import { applyOpusPreferences, findProgramAudioMid, tuneSubscriberAnswerOpus } from './sdp';
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

// A server offer the subscriber answers: Opus PT 111 with a minimal fmtp, plus
// a VP8 video PT to confirm non-Opus lines are left untouched.
const SUBSCRIBER_OFFER = [
    'v=0',
    'o=- 1 2 IN IP4 127.0.0.1',
    's=-',
    't=0 0',
    'm=audio 9 UDP/TLS/RTP/SAVPF 111',
    'c=IN IP4 0.0.0.0',
    'a=rtpmap:111 opus/48000/2',
    'a=fmtp:111 minptime=10;useinbandfec=1',
    'm=video 9 UDP/TLS/RTP/SAVPF 96',
    'a=rtpmap:96 VP8/90000'
].join('\r\n');

describe('tuneSubscriberAnswerOpus', () => {
    it('adds the stereo decode params to an existing Opus fmtp', () => {
        const fmtp = opusFmtp(tuneSubscriberAnswerOpus(SUBSCRIBER_OFFER));
        expect(fmtp).toContain('stereo=1');
        expect(fmtp).toContain('sprop-stereo=1');
        expect(fmtp).toContain('minptime=10');
        expect(fmtp).toContain('useinbandfec=1');
        // Existing params are preserved, not duplicated.
        expect(fmtp.match(/minptime=/g)?.length).toBe(1);
    });

    it('adds an fmtp line when the answer omitted one entirely', () => {
        const noFmtp = ['m=audio 9 UDP/TLS/RTP/SAVPF 111', 'a=rtpmap:111 opus/48000/2'].join('\r\n');
        const fmtp = opusFmtp(tuneSubscriberAnswerOpus(noFmtp));
        expect(fmtp).toContain('stereo=1');
        expect(fmtp).toContain('sprop-stereo=1');
    });

    it('never introduces usedtx or maxaveragebitrate (no decode gating / bitrate cap)', () => {
        const fmtp = opusFmtp(tuneSubscriberAnswerOpus(SUBSCRIBER_OFFER));
        expect(fmtp).not.toContain('usedtx');
        expect(fmtp).not.toContain('maxaveragebitrate');
    });

    it('leaves non-Opus lines and payload ordering intact', () => {
        const out = tuneSubscriberAnswerOpus(SUBSCRIBER_OFFER);
        expect(out).toContain('a=rtpmap:96 VP8/90000');
        expect(out).toContain('m=audio 9 UDP/TLS/RTP/SAVPF 111');
        // VP8 gains no fmtp.
        expect(out.split('\r\n').filter((l) => l.startsWith('a=fmtp:96'))).toHaveLength(0);
    });

    it('preserves CRLF line endings', () => {
        const out = tuneSubscriberAnswerOpus(SUBSCRIBER_OFFER);
        expect(out).toContain('\r\n');
        expect(out).not.toContain('\n\n');
    });

    it('preserves LF-only line endings', () => {
        const lfOffer = SUBSCRIBER_OFFER.replaceAll('\r\n', '\n');
        const out = tuneSubscriberAnswerOpus(lfOffer);
        expect(out).not.toContain('\r\n');
        expect(out).toContain('stereo=1');
    });

    it('returns SDP unchanged when no Opus codec is present', () => {
        const noOpus = 'v=0\r\nm=video 9 UDP/TLS/RTP/SAVPF 96\r\na=rtpmap:96 VP8/90000';
        expect(tuneSubscriberAnswerOpus(noOpus)).toBe(noOpus);
    });
});

// A renegotiated answer: program audio (mid 0) plus a relayed voice track
// (mid 2). Both share Opus PT 111, which is why the munge has to be scoped by
// m-line rather than by payload type.
const MULTI_AUDIO_ANSWER = [
    'v=0',
    'o=- 1 2 IN IP4 127.0.0.1',
    's=-',
    't=0 0',
    'm=audio 9 UDP/TLS/RTP/SAVPF 111',
    'a=mid:0',
    'a=rtpmap:111 opus/48000/2',
    'a=fmtp:111 minptime=10;useinbandfec=1',
    'm=video 9 UDP/TLS/RTP/SAVPF 96',
    'a=mid:1',
    'a=rtpmap:96 VP8/90000',
    'm=audio 9 UDP/TLS/RTP/SAVPF 111',
    'a=mid:2',
    'a=rtpmap:111 opus/48000/2'
].join('\r\n');

// The matching server offer, where only the program m-line carries the
// chromatic-stream msid.
const MULTI_AUDIO_OFFER = [
    'v=0',
    'm=audio 9 UDP/TLS/RTP/SAVPF 111',
    'a=mid:0',
    'a=msid:chromatic-stream audio',
    'a=rtpmap:111 opus/48000/2',
    'm=video 9 UDP/TLS/RTP/SAVPF 96',
    'a=mid:1',
    'a=msid:chromatic-stream video',
    'a=rtpmap:96 VP8/90000',
    'm=audio 9 UDP/TLS/RTP/SAVPF 111',
    'a=mid:2',
    'a=msid:voice-stream-abc voice-abc',
    'a=rtpmap:111 opus/48000/2'
].join('\r\n');

function sectionFor(sdp: string, mid: string): string {
    const sections = sdp.split(/\r\n(?=m=)/);
    return sections.find((s) => s.includes(`a=mid:${mid}`)) ?? '';
}

describe('findProgramAudioMid', () => {
    it('finds the mid of the program audio m-line', () => {
        expect(findProgramAudioMid(MULTI_AUDIO_OFFER)).toBe('0');
    });

    it('returns null when no program audio m-line is present', () => {
        const voiceOnly = ['m=audio 9 UDP/TLS/RTP/SAVPF 111', 'a=mid:2', 'a=msid:voice-stream-abc voice-abc'].join(
            '\r\n'
        );
        expect(findProgramAudioMid(voiceOnly)).toBeNull();
    });

    it('ignores the video m-line of the program stream', () => {
        expect(findProgramAudioMid(MULTI_AUDIO_OFFER)).not.toBe('1');
    });
});

describe('tuneSubscriberAnswerOpus scoping', () => {
    it('stamps stereo on the program m-line only, leaving voice mono', () => {
        const out = tuneSubscriberAnswerOpus(MULTI_AUDIO_ANSWER, '0');
        expect(sectionFor(out, '0')).toContain('stereo=1');
        expect(sectionFor(out, '2')).not.toContain('stereo=1');
    });

    it('does not synthesize an fmtp line on a voice m-line that had none', () => {
        // Adding lines the browser did not author is what Chrome rejects as a
        // modification, which used to destroy the whole subscription.
        const out = tuneSubscriberAnswerOpus(MULTI_AUDIO_ANSWER, '0');
        expect(sectionFor(out, '2')).not.toContain('a=fmtp:111');
    });

    it('tunes every Opus m-line when the program mid is unknown', () => {
        // Degrading to the old whole-SDP behavior is deliberate: losing program
        // stereo is worse than over-applying the decode params.
        const out = tuneSubscriberAnswerOpus(MULTI_AUDIO_ANSWER, null);
        expect(sectionFor(out, '0')).toContain('stereo=1');
        expect(sectionFor(out, '2')).toContain('stereo=1');
    });

    it('falls back to tuning everything when the mid is not in the answer', () => {
        const out = tuneSubscriberAnswerOpus(MULTI_AUDIO_ANSWER, '99');
        expect(sectionFor(out, '0')).toContain('stereo=1');
    });

    it('preserves m-line count and ordering', () => {
        const out = tuneSubscriberAnswerOpus(MULTI_AUDIO_ANSWER, '0');
        const mLines = out.split('\r\n').filter((l) => l.startsWith('m='));
        expect(mLines).toEqual([
            'm=audio 9 UDP/TLS/RTP/SAVPF 111',
            'm=video 9 UDP/TLS/RTP/SAVPF 96',
            'm=audio 9 UDP/TLS/RTP/SAVPF 111'
        ]);
    });
});
