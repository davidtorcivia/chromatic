import { describe, it, expect } from 'vitest';
import { micConstraints } from './manager';

// echoCancellationType is non-standard; read it off a loosened view.
function asRec(c: MediaTrackConstraints): Record<string, unknown> {
    return c as unknown as Record<string, unknown>;
}

describe('micConstraints', () => {
    it('talkback keeps native EC (system-preferred) and AGC, and passes NS through', () => {
        const off = micConstraints({ mode: 'talkback', studioHeadphones: false, noiseSuppression: false });
        expect(off.echoCancellation).toBe(true);
        expect(off.noiseSuppression).toBe(false); // RNNoise handling, or user chose off
        expect(off.autoGainControl).toBe(true);
        expect(asRec(off).echoCancellationType).toEqual({ ideal: 'system' });

        const on = micConstraints({ mode: 'talkback', studioHeadphones: false, noiseSuppression: true });
        expect(on.noiseSuppression).toBe(true); // native-NS fallback
        expect(on.echoCancellation).toBe(true);
    });

    it('studio sends pristine: NS/AGC always off, EC on by default', () => {
        // Even if a caller passed noiseSuppression: true, studio forces it off.
        const c = micConstraints({ mode: 'studio', studioHeadphones: false, noiseSuppression: true });
        expect(c.noiseSuppression).toBe(false);
        expect(c.autoGainControl).toBe(false);
        expect(c.echoCancellation).toBe(true);
        expect(asRec(c).echoCancellationType).toEqual({ ideal: 'system' });
    });

    it('studio + headphones removes echo cancellation entirely', () => {
        const c = micConstraints({ mode: 'studio', studioHeadphones: true, noiseSuppression: false });
        expect(c.echoCancellation).toBe(false);
        expect(c.noiseSuppression).toBe(false);
        expect(c.autoGainControl).toBe(false);
        // No echoCancellationType when EC is off.
        expect(asRec(c).echoCancellationType).toBeUndefined();
    });

    it('applies deviceId as ideal vs exact', () => {
        expect(
            micConstraints({ mode: 'talkback', studioHeadphones: false, noiseSuppression: false, deviceId: 'mic1' })
                .deviceId
        ).toEqual({ ideal: 'mic1' });
        expect(
            micConstraints({ mode: 'talkback', studioHeadphones: false, noiseSuppression: false, deviceId: 'mic1', exact: true })
                .deviceId
        ).toEqual({ exact: 'mic1' });
    });
});
