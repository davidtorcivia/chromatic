import { describe, it, expect } from 'vitest';
import {
    deriveStreamOverlayState,
    type StreamOverlayInputs
} from './stream-overlay';

function inputs(overrides: Partial<StreamOverlayInputs> = {}): StreamOverlayInputs {
    return {
        streamError: null,
        connectionLost: false,
        reconnecting: false,
        needsPlayClick: false,
        streamPaused: false,
        roomLive: false,
        hasStream: false,
        isVideoPlaying: false,
        ...overrides
    };
}

describe('deriveStreamOverlayState', () => {
    it('shows waiting when the room is not live and no tracks arrived', () => {
        expect(deriveStreamOverlayState(inputs())).toBe('waiting');
    });

    it('shows connecting when the room is live but no tracks arrived yet', () => {
        expect(deriveStreamOverlayState(inputs({ roomLive: true }))).toBe('connecting');
    });

    it('shows connecting once tracks arrive but the video is not yet playing', () => {
        expect(
            deriveStreamOverlayState(inputs({ roomLive: true, hasStream: true }))
        ).toBe('connecting');
    });

    // BUG 1 regression: a reloading/late-joining viewer may never see a
    // room:live broadcast — track arrival alone must flip them off the
    // "host hasn't started streaming yet" copy.
    it('never shows waiting once tracks have arrived, even without a live broadcast', () => {
        expect(
            deriveStreamOverlayState(inputs({ roomLive: false, hasStream: true }))
        ).toBe('connecting');
        expect(
            deriveStreamOverlayState(
                inputs({ roomLive: false, hasStream: true, isVideoPlaying: true })
            )
        ).toBe('playing');
    });

    it('shows playing only when the video element has actually started rendering', () => {
        expect(
            deriveStreamOverlayState(
                inputs({ roomLive: true, hasStream: true, isVideoPlaying: true })
            )
        ).toBe('playing');
        // 'playing' requires media, not just the room flag
        expect(
            deriveStreamOverlayState(inputs({ roomLive: true, isVideoPlaying: true }))
        ).toBe('connecting');
    });

    // BUG 1 regression: a blocked autoplay must surface the tap-to-play card
    // rather than leaving the waiting copy up.
    it('shows needs-click when autoplay is blocked, regardless of live state', () => {
        expect(
            deriveStreamOverlayState(
                inputs({ hasStream: true, needsPlayClick: true })
            )
        ).toBe('needs-click');
        expect(
            deriveStreamOverlayState(
                inputs({ roomLive: true, hasStream: true, needsPlayClick: true })
            )
        ).toBe('needs-click');
    });

    it('shows paused when the host stream is paused, even if frames rendered before', () => {
        expect(
            deriveStreamOverlayState(
                inputs({
                    roomLive: true,
                    hasStream: true,
                    isVideoPlaying: true,
                    streamPaused: true
                })
            )
        ).toBe('paused');
    });

    it('prioritizes errors above everything else', () => {
        expect(
            deriveStreamOverlayState(
                inputs({
                    streamError: 'boom',
                    needsPlayClick: true,
                    reconnecting: true,
                    roomLive: true,
                    hasStream: true,
                    isVideoPlaying: true
                })
            )
        ).toBe('error');
    });

    it('prioritizes connection-lost over reconnecting and media states', () => {
        expect(
            deriveStreamOverlayState(
                inputs({
                    connectionLost: true,
                    reconnecting: true,
                    roomLive: true,
                    hasStream: true
                })
            )
        ).toBe('connection-lost');
    });

    it('shows reconnecting while the websocket retries', () => {
        expect(
            deriveStreamOverlayState(
                inputs({ reconnecting: true, roomLive: true, hasStream: true })
            )
        ).toBe('reconnecting');
    });

    // Regression: a failed RTCPeerConnection with a HEALTHY WebSocket. The WS-
    // driven connectionLost/reconnecting flags never flip in that case, so the
    // page's onConnectionStateChange wiring forces isVideoPlaying=false on a
    // degraded PC. This must drop a previously-playing stream out of 'playing'
    // into 'connecting' (the recovery indicator) — not leave the viewer on a
    // frozen frame that reads as live.
    it('drops to connecting when a previously-playing stream degrades (isVideoPlaying cleared)', () => {
        // Was playing fine…
        expect(
            deriveStreamOverlayState(
                inputs({ roomLive: true, hasStream: true, isVideoPlaying: true })
            )
        ).toBe('playing');
        // …PC failed/disconnected: wiring clears isVideoPlaying. The frozen
        // frame must surface as 'connecting' while ICE restart / resubscribe
        // runs, with no WS error to drive connectionLost.
        expect(
            deriveStreamOverlayState(
                inputs({ roomLive: true, hasStream: true, isVideoPlaying: false })
            )
        ).toBe('connecting');
    });
});
