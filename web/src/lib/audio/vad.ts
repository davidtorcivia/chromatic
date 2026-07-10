// Voice-activity detection over the per-participant AnalyserNodes: polls
// frequency data at ~15Hz and reports the set of currently-speaking
// participants whenever it changes.
//
// An interval, not a rAF loop: this isn't display-synchronized work, and a
// rAF loop would wake the main thread on every compositor frame (120Hz on
// ProMotion) just to skip most wakeups.

export interface VADOptions {
    /** Live view of the analysers to poll (participantId → analyser). */
    getAnalysers: () => Map<string, { analyser: AnalyserNode }>;
    /** Called only when the speaking set actually changed. */
    onChange: (speaking: Set<string>) => void;
    /** Poll cadence. ~66ms ≈ 15Hz is plenty for speaker detection. */
    intervalMs?: number;
    /** Average-level threshold above which a participant counts as speaking. */
    thresholdDb?: number;
}

export function createVADMonitor(opts: VADOptions) {
    const intervalMs = opts.intervalMs ?? 66;
    const thresholdDb = opts.thresholdDb ?? -50;
    let timer: ReturnType<typeof setInterval> | null = null;
    let speaking = new Set<string>();
    // One buffer reused across checks — no allocation per poll.
    let buffer: Uint8Array<ArrayBuffer> | null = null;

    const check = () => {
        const next = new Set<string>();
        for (const [pid, { analyser }] of opts.getAnalysers()) {
            if (!buffer || buffer.length !== analyser.frequencyBinCount) {
                buffer = new Uint8Array(analyser.frequencyBinCount) as Uint8Array<ArrayBuffer>;
            }
            analyser.getByteFrequencyData(buffer);
            let sum = 0;
            for (let i = 0; i < buffer.length; i++) sum += buffer[i];
            const avg = sum / buffer.length;
            const db = 20 * Math.log10(avg / 255);
            if (db > thresholdDb) next.add(pid);
        }

        let changed = next.size !== speaking.size;
        if (!changed) {
            for (const pid of next) {
                if (!speaking.has(pid)) {
                    changed = true;
                    break;
                }
            }
        }
        if (changed) {
            speaking = next;
            opts.onChange(next);
        }
    };

    return {
        get running() {
            return timer !== null;
        },
        start() {
            if (timer) return;
            timer = setInterval(check, intervalMs);
        },
        stop() {
            if (timer) {
                clearInterval(timer);
                timer = null;
            }
            speaking = new Set();
        },
        /** Exposed for tests: run one poll synchronously. */
        tick: check,
    };
}
