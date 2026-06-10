// Short notification chimes for admin action prompts. Pure WebAudio (no
// asset files): each chime is a couple of sine notes with a soft envelope on
// the shared AudioContext. Failures are swallowed — a missed chime must
// never break the session (e.g. context still suspended before any gesture).

import { getAudioContext } from './context';

const CHIME_GAIN = 0.12; // gentle — admins may be mid-conversation

function playNotes(notes: { freq: number; at: number; dur: number }[]): void {
    void (async () => {
        try {
            const ctx = await getAudioContext();
            if (ctx.state !== 'running') return;
            const now = ctx.currentTime;
            for (const n of notes) {
                const osc = ctx.createOscillator();
                const gain = ctx.createGain();
                osc.type = 'sine';
                osc.frequency.value = n.freq;
                const start = now + n.at;
                const end = start + n.dur;
                gain.gain.setValueAtTime(0, start);
                gain.gain.linearRampToValueAtTime(CHIME_GAIN, start + 0.015);
                gain.gain.exponentialRampToValueAtTime(0.0001, end);
                osc.connect(gain);
                gain.connect(ctx.destination);
                osc.start(start);
                osc.stop(end + 0.05);
                osc.onended = () => {
                    osc.disconnect();
                    gain.disconnect();
                };
            }
        } catch {
            // No audio available — silently skip.
        }
    })();
}

/** Two quick ascending notes: someone is asking to share their screen. */
export function playShareRequestChime(): void {
    playNotes([
        { freq: 660, at: 0, dur: 0.18 },
        { freq: 880, at: 0.14, dur: 0.26 },
    ]);
}

/** Soft descending doorbell: someone joined the waiting room. */
export function playWaitingRoomChime(): void {
    playNotes([
        { freq: 784, at: 0, dur: 0.22 },
        { freq: 523, at: 0.18, dur: 0.34 },
    ]);
}
