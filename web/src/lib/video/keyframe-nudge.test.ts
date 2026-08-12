import { describe, it, expect } from 'vitest';
import {
    decideKeyframeNudge,
    type KeyframeNudgeInputs
} from './keyframe-nudge';

function inputs(overrides: Partial<KeyframeNudgeInputs> = {}): KeyframeNudgeInputs {
    return {
        isVideoPlaying: false,
        connectionState: 'connected',
        attempts: 0,
        maxAttempts: 3,
        ...overrides
    };
}

describe('decideKeyframeNudge', () => {
    it('stops once frames are rendering', () => {
        expect(decideKeyframeNudge(inputs({ isVideoPlaying: true }))).toBe('stop');
    });

    it('stops even with the attempt budget spent', () => {
        expect(
            decideKeyframeNudge(inputs({ isVideoPlaying: true, attempts: 3 }))
        ).toBe('stop');
    });

    it('nudges when connected with attempts left', () => {
        expect(decideKeyframeNudge(inputs())).toBe('nudge');
        expect(decideKeyframeNudge(inputs({ attempts: 2 }))).toBe('nudge');
    });

    it('escalates once the nudges are exhausted against a live transport', () => {
        expect(decideKeyframeNudge(inputs({ attempts: 3 }))).toBe('escalate');
    });

    // The 2026-08-11 regression: the cycle is armed by ontrack, which runs
    // during setRemoteDescription, so it expires while a TURN-relayed viewer
    // is still completing ICE. Escalating there tears down a peer connection
    // that was about to succeed, and no PLI could have helped because the
    // transport was not up yet.
    it('waits instead of escalating while the transport is still coming up', () => {
        for (const connectionState of ['new', 'connecting'] as const) {
            expect(decideKeyframeNudge(inputs({ connectionState, attempts: 3 }))).toBe('wait');
        }
    });

    it('waits rather than spending an attempt while connecting', () => {
        expect(decideKeyframeNudge(inputs({ connectionState: 'connecting' }))).toBe('wait');
    });

    // 'failed' and 'closed' have their own recovery owner (the manager's
    // ICE-restart chain). Parking on them here would leave a dead peer
    // connection polling forever instead of being rebuilt.
    it('does not wait on a peer connection that is already dead', () => {
        for (const connectionState of ['failed', 'closed'] as const) {
            expect(decideKeyframeNudge(inputs({ connectionState, attempts: 3 }))).toBe('escalate');
            expect(decideKeyframeNudge(inputs({ connectionState }))).toBe('nudge');
        }
    });

    it('escalates when there is no peer connection left to wait for', () => {
        expect(
            decideKeyframeNudge(inputs({ connectionState: null, attempts: 3 }))
        ).toBe('escalate');
    });

    it('treats a disconnected transport as live enough to nudge and escalate', () => {
        expect(
            decideKeyframeNudge(inputs({ connectionState: 'disconnected' }))
        ).toBe('nudge');
        expect(
            decideKeyframeNudge(inputs({ connectionState: 'disconnected', attempts: 3 }))
        ).toBe('escalate');
    });
});
