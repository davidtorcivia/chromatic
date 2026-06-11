// Short notification chimes for room events. Pure WebAudio (no asset
// files): each chime is a couple of sine notes with a soft envelope on
// the shared AudioContext. Failures are swallowed — a missed chime must
// never break the session (e.g. context still suspended before any gesture).

import { getAudioContext } from './context';

const CHIME_GAIN = 0.12; // gentle — admins may be mid-conversation

// Single mute point for every UI sound, persisted across sessions.
const SOUNDS_KEY = "chromatic_ui_sounds";
let uiSoundsEnabled = true;
try {
    uiSoundsEnabled = localStorage.getItem(SOUNDS_KEY) !== "off";
} catch {
    // Storage unavailable (private mode) — default on.
}

export function getUiSoundsEnabled(): boolean {
    return uiSoundsEnabled;
}

export function setUiSoundsEnabled(on: boolean): void {
    uiSoundsEnabled = on;
    try {
        localStorage.setItem(SOUNDS_KEY, on ? "on" : "off");
    } catch {
        // In-session preference still applies.
    }
}

function playNotes(
    notes: { freq: number; at: number; dur: number }[],
    gain: number = CHIME_GAIN,
    alwaysPlay = false,
): void {
    // Admin action prompts (waiting-room doorbell, share requests) bypass
    // the mute: a host who silences ambience must still hear someone
    // waiting at the door.
    if (!uiSoundsEnabled && !alwaysPlay) return;
    void (async () => {
        try {
            const ctx = await getAudioContext();
            if (ctx.state !== 'running') return;
            const now = ctx.currentTime;
            for (const n of notes) {
                const osc = ctx.createOscillator();
                const g = ctx.createGain();
                osc.type = 'sine';
                osc.frequency.value = n.freq;
                const start = now + n.at;
                const end = start + n.dur;
                g.gain.setValueAtTime(0, start);
                g.gain.linearRampToValueAtTime(gain, start + 0.015);
                g.gain.exponentialRampToValueAtTime(0.0001, end);
                osc.connect(g);
                g.connect(ctx.destination);
                osc.start(start);
                osc.stop(end + 0.05);
                osc.onended = () => {
                    osc.disconnect();
                    g.disconnect();
                };
            }
        } catch {
            // No audio available — silently skip.
        }
    })();
}

/** Two quick ascending notes: someone is asking to share their screen. */
export function playShareRequestChime(): void {
    playNotes(
        [
            { freq: 660, at: 0, dur: 0.18 },
            { freq: 880, at: 0.14, dur: 0.26 },
        ],
        CHIME_GAIN,
        true,
    );
}

/** Soft descending doorbell: someone joined the waiting room. */
export function playWaitingRoomChime(): void {
    playNotes(
        [
            { freq: 784, at: 0, dur: 0.22 },
            { freq: 523, at: 0.18, dur: 0.34 },
        ],
        CHIME_GAIN,
        true,
    );
}

/** Soft rising fifth (C5 → G5): someone entered the room. Quieter than
 *  the admin prompts — ambient awareness, not a call to action. */
export function playJoinChime(): void {
    playNotes(
        [
            { freq: 523.25, at: 0, dur: 0.16 },
            { freq: 783.99, at: 0.12, dur: 0.3 },
        ],
        0.08,
    );
}

/** Mirrored falling fifth (E5 → A4), pitched apart from the waiting-room
 *  doorbell: someone left the room. */
export function playLeaveChime(): void {
    playNotes(
        [
            { freq: 659.25, at: 0, dur: 0.16 },
            { freq: 440, at: 0.12, dur: 0.3 },
        ],
        0.08,
    );
}

// Chat sounds are an octave pair (D6 in / D5 out) at whisper level —
// feedback you feel more than hear. Incoming is rate-limited so a burst
// of messages reads as one.
let lastReceiveChimeAt = 0;

/** Barely-there high tick: a chat message arrived. */
export function playChatReceiveChime(): void {
    const now = Date.now();
    if (now - lastReceiveChimeAt < 400) return;
    lastReceiveChimeAt = now;
    playNotes([{ freq: 1174.66, at: 0, dur: 0.16 }], 0.07);
}

/** Softer low tap: your message left. */
export function playChatSentChime(): void {
    playNotes([{ freq: 587.33, at: 0, dur: 0.12 }], 0.05);
}
