// Audio context management with mobile unlock support

let audioContext: AudioContext | null = null;

export async function getAudioContext(): Promise<AudioContext> {
    if (!audioContext) {
        try {
            audioContext = new AudioContext();
        } catch (err) {
            console.error('Failed to create AudioContext:', err);
            throw err;
        }
    }

    // Required for mobile browsers
    if (audioContext.state === 'suspended') {
        await audioContext.resume();
    }

    return audioContext;
}

// Must be called from a click/tap handler to unlock audio on iOS
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
