import { describe, it, expect } from 'vitest';
import { describeGumError, isPermissionError } from './gum-error';

// A DOMException as Firefox raises it: the message carries the meaning, the
// name does not.
const domException = (name: string, message: string) => {
    const err = new Error(message);
    err.name = name;
    return err;
};

describe('describeGumError', () => {
    it('reads Firefox "Failed to allocate videosource" as a busy device, not a block', () => {
        // BUG regression: this surfaced as "Camera blocked — allow access",
        // sending users into permission settings that were already correct.
        const msg = describeGumError(domException('DOMException', 'Failed to allocate videosource'), 'Camera');
        expect(msg).toMatch(/already in use/i);
        expect(msg).toMatch(/other Chromatic tab/i);
        expect(msg).not.toMatch(/blocked/i);
    });

    it('does not treat an allocation failure as a permission error', () => {
        expect(isPermissionError(domException('DOMException', 'Failed to allocate videosource'))).toBe(false);
    });

    it('still reports a genuine block as blocked', () => {
        expect(describeGumError(domException('NotAllowedError', 'denied'), 'Camera')).toMatch(/blocked/i);
        expect(isPermissionError(domException('NotAllowedError', 'denied'))).toBe(true);
        expect(isPermissionError(domException("SecurityError", "insecure"))).toBe(true);
    });

    it('distinguishes a busy device from a vanished one', () => {
        expect(describeGumError(domException('NotReadableError', ''), 'Microphone')).toMatch(/in use by another app/i);
        expect(describeGumError(domException('NotFoundError', ''), 'Camera')).toMatch(/No available camera/i);
        expect(describeGumError(domException('OverconstrainedError', ''), 'Camera')).toMatch(/No available camera/i);
    });

    it('names an unrecognized error rather than guessing at permissions', () => {
        expect(describeGumError(domException('WeirdError', 'nope'), 'Camera')).toMatch(/\(WeirdError\)/);
    });
});
