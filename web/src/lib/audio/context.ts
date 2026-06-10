// Audio context management with mobile unlock support
//
// IMPORTANT: without a user gesture, AudioContext.resume() returns a promise
// that stays PENDING until a gesture happens (it neither resolves nor
// rejects). Nothing in this module may await it unconditionally — an F5
// reload has no gesture, and awaiting resume() blocked the session page's
// entire onMount (WebSocket never connected; the page hung on the waiting
// overlay forever).

let audioContext: AudioContext | null = null;
let gestureUnlockInstalled = false;

function tryResume(ctx: AudioContext): void {
    if (ctx.state !== 'suspended') return;
    // Fire-and-forget: resolves after a gesture, harmless if it never does.
    ctx.resume().catch(() => {});
    installGestureUnlock();
}

// One-time listeners: the first user gesture anywhere resumes the context.
function installGestureUnlock(): void {
    if (gestureUnlockInstalled || typeof document === 'undefined') return;
    gestureUnlockInstalled = true;
    const unlock = () => {
        if (audioContext && audioContext.state === 'suspended') {
            audioContext.resume().catch(() => {});
        }
        document.removeEventListener('pointerdown', unlock);
        document.removeEventListener('keydown', unlock);
        gestureUnlockInstalled = false;
    };
    document.addEventListener('pointerdown', unlock, { passive: true });
    document.addEventListener('keydown', unlock);
}

// Returns the shared context immediately, even if still suspended — Web
// Audio graphs can be built on a suspended context and start processing
// once it resumes (via the gesture unlock above).
export async function getAudioContext(): Promise<AudioContext> {
    if (!audioContext) {
        try {
            audioContext = new AudioContext();
        } catch (err) {
            console.error('Failed to create AudioContext:', err);
            throw err;
        }
    }

    tryResume(audioContext);
    return audioContext;
}

// Safe to call from anywhere (including without a gesture): it never blocks.
// When called from a click/tap handler it fully unlocks audio on iOS.
export async function unlockAudio(): Promise<void> {
    try {
        const ctx = await getAudioContext();

        // Play a silent buffer to fully unlock on iOS
        const buffer = ctx.createBuffer(1, 1, 22050);
        const source = ctx.createBufferSource();
        source.buffer = buffer;
        source.connect(ctx.destination);
        source.start();
    } catch (err) {
        console.warn('Failed to unlock audio:', err);
    }
}

export function closeAudioContext(): void {
    if (audioContext) {
        try {
            audioContext.close();
        } catch (err) {
            console.warn('Failed to close AudioContext:', err);
        }
        audioContext = null;
    }
}
