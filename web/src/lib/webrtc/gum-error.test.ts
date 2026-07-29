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
    it('points Firefox\'s "Failed to allocate videosource" at the OS-level block', () => {
        // BUG regression: this surfaced as "Camera blocked — allow access in
        // your browser", sending users into SITE permission settings that were
        // already correct. The real cause (confirmed in the field) was Windows
        // withholding camera access from the browser application.
        const msg = describeGumError(domException('DOMException', 'Failed to allocate videosource'), 'Camera');
        expect(msg).toMatch(/Windows/);
        expect(msg).toMatch(/macOS/);
        expect(msg).toMatch(/not a site one/i);
        expect(msg).toMatch(/another app or Chromatic tab/i);
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
