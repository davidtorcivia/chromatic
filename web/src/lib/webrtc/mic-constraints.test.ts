import { describe, it, expect } from 'vitest';
import { micConstraints } from './manager';

// echoCancellationType is non-standard; read it off a loosened view.
function asRec(c: MediaTrackConstraints): Record<string, unknown> {
    return c as unknown as Record<string, unknown>;
}

describe('micConstraints', () => {
    it('talkback keeps native EC (system-preferred) and native NS when no in-app denoise', () => {
        const c = micConstraints({ mode: 'talkback', studioHeadphones: false, inAppDenoise: false });
        expect(c.echoCancellation).toBe(true);
        expect(c.noiseSuppression).toBe(true); // native NS carries the load
        expect(c.autoGainControl).toBe(true);
        expect(asRec(c).echoCancellationType).toEqual({ ideal: 'system' });
    });

    it('talkback disables native NS when our denoiser will run', () => {
        const c = micConstraints({ mode: 'talkback', studioHeadphones: false, inAppDenoise: true });
        expect(c.echoCancellation).toBe(true);
        expect(c.noiseSuppression).toBe(false); // RNNoise handles it
        expect(c.autoGainControl).toBe(true);
    });

    it('studio sends pristine: no NS/AGC, EC on by default', () => {
        const c = micConstraints({ mode: 'studio', studioHeadphones: false, inAppDenoise: false });
        expect(c.noiseSuppression).toBe(false);
        expect(c.autoGainControl).toBe(false);
        expect(c.echoCancellation).toBe(true);
        expect(asRec(c).echoCancellationType).toEqual({ ideal: 'system' });
    });

    it('studio + headphones removes echo cancellation entirely', () => {
        const c = micConstraints({ mode: 'studio', studioHeadphones: true, inAppDenoise: false });
        expect(c.echoCancellation).toBe(false);
        expect(c.noiseSuppression).toBe(false);
        expect(c.autoGainControl).toBe(false);
        // No echoCancellationType when EC is off.
        expect(asRec(c).echoCancellationType).toBeUndefined();
    });

    it('applies deviceId as ideal vs exact', () => {
        expect(micConstraints({ mode: 'talkback', studioHeadphones: false, inAppDenoise: false, deviceId: 'mic1' }).deviceId)
            .toEqual({ ideal: 'mic1' });
        expect(micConstraints({ mode: 'talkback', studioHeadphones: false, inAppDenoise: false, deviceId: 'mic1', exact: true }).deviceId)
            .toEqual({ exact: 'mic1' });
    });
});
