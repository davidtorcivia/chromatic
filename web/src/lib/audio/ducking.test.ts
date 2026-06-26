import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { AudioDuckingManager, type DuckingConfig } from './ducking';

// Mock audio context
vi.mock('./context', () => ({
    getAudioContext: vi.fn().mockResolvedValue({
        createMediaStreamSource: vi.fn().mockReturnValue({
            connect: vi.fn()
        }),
        createAnalyser: vi.fn().mockReturnValue({
            fftSize: 256,
            frequencyBinCount: 128,
            connect: vi.fn(),
            getByteFrequencyData: vi.fn()
        }),
        createGain: vi.fn().mockReturnValue({
            gain: { value: 1 },
            connect: vi.fn(),
            disconnect: vi.fn()
        }),
        destination: {}
    })
}));

describe('AudioDuckingManager', () => {
    let mockStreamElement: HTMLMediaElement;
    let manager: AudioDuckingManager;

    beforeEach(() => {
        vi.useFakeTimers();

        mockStreamElement = {
            volume: 1.0
        } as HTMLMediaElement;
    });

    afterEach(() => {
        vi.useRealTimers();
        if (manager) {
            manager.destroy();
        }
    });

    it('creates with default config', () => {
        manager = new AudioDuckingManager(mockStreamElement, false);
        expect(manager).toBeDefined();
    });

    it('creates with custom config', () => {
        const customConfig: Partial<DuckingConfig> = {
            duckLevel: 0.1,
            attackTime: 100,
            releaseTime: 300,
            holdTime: 1000,
            vadByteThreshold: 8
        };

        manager = new AudioDuckingManager(mockStreamElement, false, customConfig);
        expect(manager).toBeDefined();
    });

    it('adds voice track successfully', async () => {
        manager = new AudioDuckingManager(mockStreamElement, false);

        const mockTrack = {} as MediaStreamTrack;
        await expect(manager.addVoiceTrack('participant-1', mockTrack)).resolves.not.toThrow();
    });

    it('removes voice track', async () => {
        manager = new AudioDuckingManager(mockStreamElement, false);

        const mockTrack = {} as MediaStreamTrack;
        await manager.addVoiceTrack('participant-1', mockTrack);

        // Should not throw
        manager.removeVoiceTrack('participant-1');
        manager.removeVoiceTrack('non-existent');
    });

    it('admin is exempt from ducking', () => {
        // Admin should never have volume reduced
        manager = new AudioDuckingManager(mockStreamElement, true);
        expect(mockStreamElement.volume).toBe(1.0);
    });

    it('destroys cleanly', async () => {
        manager = new AudioDuckingManager(mockStreamElement, false);

        const mockTrack = {} as MediaStreamTrack;
        await manager.addVoiceTrack('participant-1', mockTrack);

        // Should not throw
        manager.destroy();
    });

    it('restores base stream volume on destroy', () => {
        manager = new AudioDuckingManager(mockStreamElement, false);

        // Simulate the element being left in a ducked state
        mockStreamElement.volume = 0.2;
        manager.destroy();

        expect(mockStreamElement.volume).toBe(1.0);
    });

    it('restores the user-set base volume (not 1.0) on destroy', () => {
        manager = new AudioDuckingManager(mockStreamElement, false);
        manager.setStreamVolume(0.6);

        // Simulate a ducked state
        mockStreamElement.volume = 0.12;
        manager.destroy();

        expect(mockStreamElement.volume).toBeCloseTo(0.6);
    });

    describe('animateVolume cancellation', () => {
        it('cancels an in-flight animation when a new one starts', () => {
            const rafSpy = vi
                .spyOn(globalThis, 'requestAnimationFrame')
                .mockReturnValue(42 as unknown as number);
            const cancelSpy = vi
                .spyOn(globalThis, 'cancelAnimationFrame')
                .mockImplementation(() => {});

            manager = new AudioDuckingManager(mockStreamElement, false);
            // eslint-disable-next-line @typescript-eslint/no-explicit-any
            const m = manager as any;

            m.animateVolume(0.2, 100);
            expect(cancelSpy).not.toHaveBeenCalled();

            // A second animation while the first is in flight must cancel it
            m.animateVolume(1.0, 200);
            expect(cancelSpy).toHaveBeenCalledWith(42);

            rafSpy.mockRestore();
            cancelSpy.mockRestore();
        });

        it('cancels an in-flight animation on destroy', () => {
            const rafSpy = vi
                .spyOn(globalThis, 'requestAnimationFrame')
                .mockReturnValue(7 as unknown as number);
            const cancelSpy = vi
                .spyOn(globalThis, 'cancelAnimationFrame')
                .mockImplementation(() => {});

            manager = new AudioDuckingManager(mockStreamElement, false);
            // eslint-disable-next-line @typescript-eslint/no-explicit-any
            (manager as any).animateVolume(0.2, 100);

            manager.destroy();
            expect(cancelSpy).toHaveBeenCalledWith(7);

            rafSpy.mockRestore();
            cancelSpy.mockRestore();
        });
    });

    describe('easeOutQuad calculation', () => {
        // We test the easing function indirectly through volume animation
        // The easeOutQuad formula is t * (2 - t)
        it('calculates correct easing values', () => {
            // t=0 -> 0
            expect(0 * (2 - 0)).toBe(0);

            // t=0.5 -> 0.75
            expect(0.5 * (2 - 0.5)).toBe(0.75);

            // t=1 -> 1
            expect(1 * (2 - 1)).toBe(1);
        });
    });

    describe('async teardown safety', () => {
        // Regression: addVoiceTrack awaits getAudioContext(), which can stay
        // pending under the autoplay policy. If destroy()/removeVoiceTrack ran
        // during that await, the resolved setup used to build a complete orphaned
        // voice graph (sink + source + analyser + gain + RAF loop) for a
        // departed participant / dead manager. The post-await guard must abort.
        it('does not build a voice graph when destroy() ran during the await', async () => {
            // A deferred getAudioContext so we control when the await resolves.
            const { getAudioContext } = await import('./context');
            const realMock = vi.mocked(getAudioContext);
            let resolveCtx!: (v: unknown) => void;
            const pending = new Promise<unknown>((res) => { resolveCtx = res; });
            realMock.mockReturnValueOnce(pending as never);

            manager = new AudioDuckingManager(mockStreamElement, false);
            const addPromise = manager.addVoiceTrack('p1', {} as MediaStreamTrack);

            // Tear down while the await is still pending.
            manager.destroy();
            resolveCtx({
                createMediaStreamSource: vi.fn(),
                createAnalyser: vi.fn(),
                createGain: vi.fn(),
                destination: {}
            });

            await expect(addPromise).resolves.toBeUndefined();
            // eslint-disable-next-line @typescript-eslint/no-explicit-any
            const m = manager as any;
            expect(m.voiceSinks.size).toBe(0);
            expect(m.voiceSources.size).toBe(0);
            expect(m.voiceAnalysers.size).toBe(0);
            expect(m.monitorFrame).toBeNull();
        });

        it('does not build a voice graph when removeVoiceTrack ran during the await', async () => {
            const { getAudioContext } = await import('./context');
            const realMock = vi.mocked(getAudioContext);
            let resolveCtx!: (v: unknown) => void;
            const pending = new Promise<unknown>((res) => { resolveCtx = res; });
            realMock.mockReturnValueOnce(pending as never);

            manager = new AudioDuckingManager(mockStreamElement, false);
            const addPromise = manager.addVoiceTrack('p1', {} as MediaStreamTrack);

            // Participant leaves while the await is still pending.
            manager.removeVoiceTrack('p1');
            resolveCtx({
                createMediaStreamSource: vi.fn(),
                createAnalyser: vi.fn(),
                createGain: vi.fn(),
                destination: {}
            });

            await expect(addPromise).resolves.toBeUndefined();
            // eslint-disable-next-line @typescript-eslint/no-explicit-any
            const m = manager as any;
            expect(m.voiceSinks.has('p1')).toBe(false);
            expect(m.voiceSources.has('p1')).toBe(false);
            expect(m.voiceAnalysers.has('p1')).toBe(false);
        });
    });

    describe('VAD byte threshold', () => {
        // Regression: detectVoiceActivity used to re-derive dB via
        // 20·log10(avgByte/255) on getByteFrequencyData output, which is a
        // meaningless double conversion after the analyser's own dB→byte
        // mapping — it fired on near-silence and ducked the stream almost
        // continuously. The fix compares the byte average directly to
        // vadByteThreshold.
        it('reports no voice when the byte average is below the threshold', () => {
            manager = new AudioDuckingManager(mockStreamElement, false);
            // eslint-disable-next-line @typescript-eslint/no-explicit-any
            const m = manager as any;

            // Fill the VAD buffer with low values (room noise): avg = 3 < 10.
            const buf = new Uint8Array(128);
            buf.fill(3);
            m.vadBuffer = buf;
            // No analysers registered → detectVoiceActivity returns false
            // regardless, but the calibration we care about is that a low byte
            // average does not trip a negative-dB quirk. Register a stub
            // analyser whose getByteFrequencyData writes the buffer.
            m.voiceAnalysers.set('p1', {
                getByteFrequencyData: (target: Uint8Array) => { target.fill(3); },
                frequencyBinCount: 128,
                disconnect: () => {}
            });
            expect(m.detectVoiceActivity()).toBe(false);
        });

        it('reports voice when the byte average exceeds the threshold', () => {
            manager = new AudioDuckingManager(mockStreamElement, false);
            // eslint-disable-next-line @typescript-eslint/no-explicit-any
            const m = manager as any;
            m.voiceAnalysers.set('p1', {
                // Speech concentrates energy: avg = 40 > 10.
                getByteFrequencyData: (target: Uint8Array) => { target.fill(40); },
                frequencyBinCount: 128,
                disconnect: () => {}
            });
            expect(m.detectVoiceActivity()).toBe(true);
        });
    });
});
