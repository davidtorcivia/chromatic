// @ts-nocheck — runs in AudioWorkletGlobalScope (registerProcessor,
// AudioWorkletProcessor, sampleRate are worklet globals, not in the DOM lib).
// esbuild entry → web/static/audio/gate-worklet.js (see scripts/build-worklets.mjs).
//
// Soft RMS gate that *attenuates* (never hard-mutes) the mic between phrases.
// Opens at -50 dBFS, closes at -56 dBFS (hysteresis so it doesn't chatter),
// attenuates by 12 dB when closed. ~5 ms attack so the first syllable passes,
// ~200 ms release so word endings don't pump.
//
// NOTE: this used to be injected as a blob: URL, but a strict CSP
// (script-src 'self') blocks blob: worklet scripts — which aborted the whole
// mic chain. Serving it as a same-origin static file keeps the CSP tight.

const GATE_PROCESSOR_NAME = 'chromatic-soft-gate';

class SoftGateProcessor extends AudioWorkletProcessor {
    constructor() {
        super();
        this.gain = 1;
        this.openThreshold = Math.pow(10, -50 / 20);
        this.closeThreshold = Math.pow(10, -56 / 20);
        this.closedGain = Math.pow(10, -12 / 20);
        this.attackCoef = Math.exp(-1 / (sampleRate * 0.005));
        this.releaseCoef = Math.exp(-1 / (sampleRate * 0.2));
        this.env = 0;
        this.open = false;
    }
    process(inputs, outputs) {
        const input = inputs[0];
        const output = outputs[0];
        if (!input || input.length === 0 || !input[0]) return true;
        const ch0 = input[0];
        for (let i = 0; i < ch0.length; i++) {
            const a = Math.abs(ch0[i]);
            this.env = a > this.env
                ? a + this.attackCoef * (this.env - a)
                : a + this.releaseCoef * (this.env - a);
        }
        if (this.open) {
            if (this.env < this.closeThreshold) this.open = false;
        } else if (this.env > this.openThreshold) {
            this.open = true;
        }
        const target = this.open ? 1 : this.closedGain;
        const coef = target > this.gain ? 0.3 : 0.05;
        let g = this.gain;
        for (let c = 0; c < input.length; c++) {
            const inCh = input[c];
            const outCh = output[c];
            if (!inCh || !outCh) continue;
            g = this.gain;
            for (let i = 0; i < inCh.length; i++) {
                g += (target - g) * coef;
                outCh[i] = inCh[i] * g;
            }
        }
        this.gain = g;
        return true;
    }
}
registerProcessor(GATE_PROCESSOR_NAME, SoftGateProcessor);
