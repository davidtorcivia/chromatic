// Human-readable getUserMedia failures. Every capture path (green room and
// in-session) funnels its rejection through here, so a busy device, a vanished
// device, and a real permission block never collapse into the same misleading
// "blocked — allow access" line.

export type CaptureKind = 'Microphone' | 'Camera';

export function gumErrorName(err: unknown): string {
    return (err as { name?: string })?.name ?? '';
}

export function isPermissionError(err: unknown): boolean {
    const name = gumErrorName(err);
    return name === 'NotAllowedError' || name === 'SecurityError';
}

function gumErrorMessage(err: unknown): string {
    return (err as { message?: string })?.message ?? '';
}

// Firefox raises these as a bare DOMException whose `name` carries no useful
// information, so the message text is the only discriminator. They mean the
// device could not be STARTED, which is NOT the same as the site being denied:
// the site permission reads as granted and every capture still fails.
//
// Confirmed causes, in the order worth checking: the OS withholding camera
// access from the browser application (this is what it turned out to be in
// practice — Windows' "let desktop apps access your camera"), a sandboxed
// browser package missing the device interface (snap/flatpak on Linux), or the
// source already being allocated by another app or tab.
function isSourceBusy(err: unknown): boolean {
    return /failed to allocate videosource|concurrent mic process limit|starting video failed|starting audio failed/i.test(
        gumErrorMessage(err)
    );
}

export function describeGumError(err: unknown, kind: CaptureKind): string {
    const lower = kind.toLowerCase();
    if (isSourceBusy(err)) {
        return `${kind} could not be started. This is an OS or app-level block, not a site one — allowing this site is not enough. On Windows check Settings › Privacy & security › ${kind} › "Let desktop apps access your ${lower}"; on macOS System Settings › Privacy & Security › ${kind} and confirm your browser is listed and on. Otherwise another app or Chromatic tab already has the ${lower} open.`;
    }
    switch (gumErrorName(err)) {
        case 'NotAllowedError':
        case 'SecurityError':
            // Firefox keeps a per-tab block after the prompt is dismissed, so the
            // site can look "allowed" in settings while every request still fails
            // instantly. Reloading clears it — say so.
            return `${kind} blocked. Check the permission icon in the address bar (Firefox keeps a temporary block after a dismissed prompt — reload the page to clear it), then allow ${lower} access. On macOS also check System Settings › Privacy › ${kind}.`;
        case 'NotReadableError':
        case 'AbortError':
            return `${kind} is in use by another app or tab — close it (e.g. OBS or another window) and retry.`;
        case 'NotFoundError':
        case 'OverconstrainedError':
            return `No available ${lower} — the saved device may be gone. Unplug/replug it or pick another below.`;
        default: {
            const name = gumErrorName(err);
            return `${kind} unavailable${name ? ` (${name})` : ''}. Check the browser and OS permissions.`;
        }
    }
}
