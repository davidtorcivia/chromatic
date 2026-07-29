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
// information, so the message text is the only discriminator. Both mean the
// device could not be STARTED (not that it was blocked): the source is already
// allocated — commonly by another Chromatic tab, since one camera cannot be
// opened twice with differing constraints.
function isSourceBusy(err: unknown): boolean {
    return /failed to allocate videosource|concurrent mic process limit|starting video failed|starting audio failed/i.test(
        gumErrorMessage(err)
    );
}

export function describeGumError(err: unknown, kind: CaptureKind): string {
    const lower = kind.toLowerCase();
    if (isSourceBusy(err)) {
        return `${kind} could not be started — it is already in use. Close any other Chromatic tab or window, and any other app using the ${lower} (OBS, Zoom, Photo Booth), then retry. If it persists, fully quit and reopen the browser.`;
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
