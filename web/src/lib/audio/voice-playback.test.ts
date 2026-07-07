import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { VoicePlaybackManager } from './voice-playback';

// Mock audio context. Each node records its connect() targets so the gain-path
// test can assert the audible chain (source -> gain -> destination) is wired.
function makeMockContext() {
    const calls = { sourceToGain: 0, gainToDestination: 0 };
    const destination = { tag: 'destination' };
    const gain = {
        gain: { value: 1 },
        connect: vi.fn((target: unknown) => {
            if (target === destination) calls.gainToDestination++;
        }),
        disconnect: vi.fn()
    };
    const source = {
        connect: vi.fn((target: unknown) => {
            if (target === gain) calls.sourceToGain++;
        }),
        disconnect: vi.fn()
    };
    const ctx = {
        createMediaStreamSource: vi.fn().mockReturnValue(source),
        createGain: vi.fn().mockReturnValue(gain),
        destination
    };
    return { ctx, source, gain, calls };
}

// A minimal EventTarget-like track mock: records listeners and can dispatch.
function makeFakeTrack() {
    const handlers: Record<string, Array<() => void>> = {};
    const track = {
        addEventListener: vi.fn((type: string, h: () => void) => {
            (handlers[type] ??= []).push(h);
        }),
        removeEventListener: vi.fn((type: string, h: () => void) => {
            handlers[type] = (handlers[type] ?? []).filter((x) => x !== h);
        })
    };
    return Object.assign(track as unknown as MediaStreamTrack, {
        dispatch(type: string) {
            for (const h of handlers[type] ?? []) h();
        }
    });
}

const ctxRef: { ctx: ReturnType<typeof makeMockContext> | null } = { ctx: null };

vi.mock('./context', () => ({
    getAudioContext: vi.fn(async () => ctxRef.ctx!.ctx)
}));

describe('VoicePlaybackManager', () => {
    let mockStreamElement: HTMLMediaElement;
    let manager: VoicePlaybackManager;

    beforeEach(() => {
        vi.useFakeTimers();
        ctxRef.ctx = makeMockContext();
        mockStreamElement = { volume: 1.0 } as HTMLMediaElement;
        // addVoiceTrack bails when MediaStream is absent (jsdom); stub it so the
        // gain-path tests reach the audio graph.
        vi.stubGlobal('MediaStream', class MockMediaStream {
            tracks: unknown[];
            constructor(tracks: unknown[]) { this.tracks = tracks; }
        });
        // jsdom's HTMLAudioElement.play() is a no-op returning undefined; the
        // manager chains .play().catch(), so provide a sink that resolves.
        vi.stubGlobal('Audio', class MockAudio {
            srcObject: unknown = null;
            muted = false;
            play() { return Promise.resolve(); }
        });
    });

    afterEach(() => {
        vi.useRealTimers();
        vi.unstubAllGlobals();
        if (manager) manager.destroy();
    });

    it('setStreamVolume is the only thing that moves the program element volume', () => {
        manager = new VoicePlaybackManager(mockStreamElement);
        manager.setStreamVolume(0.6);
        expect(mockStreamElement.volume).toBe(0.6);
    });

    it('clamps the program volume to [0,1]', () => {
        manager = new VoicePlaybackManager(mockStreamElement);
        manager.setStreamVolume(2);
        expect(mockStreamElement.volume).toBe(1);
        manager.setStreamVolume(-1);
        expect(mockStreamElement.volume).toBe(0);
    });

    it('applies voice volume to each voice gain node', async () => {
        manager = new VoicePlaybackManager(mockStreamElement);
        manager.setVoiceVolume(0.4);
        await manager.addVoiceTrack('p1', {} as MediaStreamTrack);
        expect(ctxRef.ctx!.gain.gain.value).toBe(0.4);

        // A later volume change reaches the live node too.
        manager.setVoiceVolume(0.9);
        expect(ctxRef.ctx!.gain.gain.value).toBe(0.9);
    });

    it('connects audible voice through the source -> gain -> destination path', async () => {
        manager = new VoicePlaybackManager(mockStreamElement);
        await manager.addVoiceTrack('p1', {} as MediaStreamTrack);

        expect(ctxRef.ctx!.ctx.createMediaStreamSource).toHaveBeenCalledTimes(1);
        expect(ctxRef.ctx!.ctx.createGain).toHaveBeenCalledTimes(1);
        expect(ctxRef.ctx!.calls.sourceToGain).toBe(1);
        expect(ctxRef.ctx!.calls.gainToDestination).toBe(1);
    });

    it('does NOT duck the program volume in response to voice activity', async () => {
        // The regression this guards: the former AudioDuckingManager ramped the
        // program element to 20% whenever a voice analyser saw energy. There is
        // no analyser and no ducking loop now, so adding a voice track and
        // letting time pass must leave the program volume exactly where the user
        // set it.
        manager = new VoicePlaybackManager(mockStreamElement);
        manager.setStreamVolume(0.7);
        await manager.addVoiceTrack('p1', {} as MediaStreamTrack);

        // Advance well past any former attack/hold/release timers.
        await vi.advanceTimersByTimeAsync(2000);

        expect(mockStreamElement.volume).toBe(0.7);
    });

    it('restores the user-set program volume on destroy', () => {
        manager = new VoicePlaybackManager(mockStreamElement);
        manager.setStreamVolume(0.5);
        // Simulate something else (e.g. a stale caller) touching the element.
        mockStreamElement.volume = 0.2;
        manager.destroy();
        expect(mockStreamElement.volume).toBe(0.5);
    });

    it('disconnects a participant voice graph on removeVoiceTrack', async () => {
        manager = new VoicePlaybackManager(mockStreamElement);
        await manager.addVoiceTrack('p1', {} as MediaStreamTrack);

        manager.removeVoiceTrack('p1');

        expect(ctxRef.ctx!.source.disconnect).toHaveBeenCalledTimes(1);
        expect(ctxRef.ctx!.gain.disconnect).toHaveBeenCalledTimes(1);
    });

    describe('async teardown safety', () => {
        // The autoplay policy can keep getAudioContext() pending across a
        // destroy()/removeVoiceTrack(). The resolved setup must not build an
        // orphaned voice graph (sink + source + gain) for a departed participant.
        it('aborts addVoiceTrack when destroyed during the await', async () => {
            manager = new VoicePlaybackManager(mockStreamElement);

            const pending = manager.addVoiceTrack('p1', {} as MediaStreamTrack);
            manager.destroy();
            await pending;

            expect(ctxRef.ctx!.ctx.createMediaStreamSource).not.toHaveBeenCalled();
            expect(ctxRef.ctx!.ctx.createGain).not.toHaveBeenCalled();
        });

        it('aborts addVoiceTrack when the participant is removed during the await', async () => {
            manager = new VoicePlaybackManager(mockStreamElement);

            const pending = manager.addVoiceTrack('p1', {} as MediaStreamTrack);
            manager.removeVoiceTrack('p1');
            await pending;

            expect(ctxRef.ctx!.ctx.createMediaStreamSource).not.toHaveBeenCalled();
            expect(ctxRef.ctx!.ctx.createGain).not.toHaveBeenCalled();
        });
    });

    it('tears the voice graph down when the remote track ends', async () => {
        manager = new VoicePlaybackManager(mockStreamElement);
        const track = makeFakeTrack();
        await manager.addVoiceTrack('p1', track);

        // Setup wired the audible graph and registered an ended listener.
        expect(track.addEventListener).toHaveBeenCalledWith('ended', expect.any(Function));
        expect(ctxRef.ctx!.calls.sourceToGain).toBe(1);

        // A remote track can end without a participant:left (publisher PC
        // tear-down / ICE failure). The manager must clean up, not leak.
        track.dispatch('ended');
        expect(ctxRef.ctx!.source.disconnect).toHaveBeenCalledTimes(1);
        expect(ctxRef.ctx!.gain.disconnect).toHaveBeenCalledTimes(1);

        // The listener is detached once the graph is gone, so a second 'ended'
        // cannot re-enter removeVoiceTrack.
        expect(track.removeEventListener).toHaveBeenCalledWith('ended', expect.any(Function));
        track.dispatch('ended');
        expect(ctxRef.ctx!.source.disconnect).toHaveBeenCalledTimes(1);
    });

    it('does not let an ended predecessor track evict its renegotiated successor', async () => {
        manager = new VoicePlaybackManager(mockStreamElement);
        const trackA = makeFakeTrack();
        await manager.addVoiceTrack('p1', trackA);
        // Renegotiation replaces the track; addVoiceTrack tears A down first.
        const trackB = makeFakeTrack();
        await manager.addVoiceTrack('p1', trackB);

        // The post-swap disconnect count from tearing A down.
        const swaps = ctxRef.ctx!.source.disconnect.mock.calls.length;

        // The predecessor ending must NOT tear down the successor's graph.
        trackA.dispatch('ended');
        expect(ctxRef.ctx!.source.disconnect.mock.calls.length).toBe(swaps);
        expect(trackB.addEventListener).toHaveBeenCalledWith('ended', expect.any(Function));

        // The successor ending DOES clean up.
        trackB.dispatch('ended');
        expect(ctxRef.ctx!.source.disconnect.mock.calls.length).toBe(swaps + 1);
    });
});
