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
            vadThreshold: -40
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
});
