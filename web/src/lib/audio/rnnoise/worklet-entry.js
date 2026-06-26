// @ts-nocheck — runs in AudioWorkletGlobalScope (registerProcessor,
// AudioWorkletProcessor, sampleRate are worklet globals, not in the DOM lib).
// This file is an esbuild entry, bundled separately; nothing imports it.
//
// RNNoise AudioWorklet processor (talkback denoiser).
//
// This file is the ENTRY for an esbuild bundle, not imported directly by the
// app. esbuild inlines @jitsi/rnnoise-wasm's sync build (WASM embedded as
// base64, synchronously compiled — the only form usable inside an
// AudioWorkletGlobalScope) and emits a self-contained classic script to
// web/static/audio/rnnoise-worklet.js, loaded via audioWorklet.addModule().
// See scripts/build-worklets.mjs.
//
// RNNoise works on 480-sample (10 ms @48k) mono frames; the render quantum is
// 128 samples, so we ring-buffer input up to 480, denoise, and drain 128/quantum
// back out. Net added latency ~one frame (~13 ms). Samples are scaled to int16
// range (×32768) for RNNoise and back (÷32768) on the way out — the model is
// trained on that range, not [-1, 1].

import createRNNWasmModuleSync from '@jitsi/rnnoise-wasm/dist/rnnoise-sync';

const FRAME = 480;
const SHIFT = 32768;
// Output FIFO: a few frames of headroom so input(128)/output(480) pacing never
// overruns in steady state.
const RING = FRAME * 4;

class RnnoiseWorkletProcessor extends AudioWorkletProcessor {
    constructor() {
        super();
        this._ready = false;
        try {
            const m = createRNNWasmModuleSync();
            this._m = m;
            this._state = m._rnnoise_create(0); // 0 = built-in default model
            this._ptr = m._malloc(FRAME * 4); // Float32 scratch (in == out)
            this._heapIdx = this._ptr >> 2;
            this._in = new Float32Array(FRAME);
            this._inFill = 0;
            this._ring = new Float32Array(RING);
            this._wr = 0;
            this._rd = 0;
            this._avail = 0;
            this._started = false;
            this._ready = true;
            this.port.postMessage({ type: 'ready' });
        } catch (err) {
            // Constructor runs on the audio thread; report failure so the main
            // thread can fall back to native noise suppression.
            this.port.postMessage({
                type: 'error',
                message: String((err && err.message) || err)
            });
        }
    }

    _pushDenoisedFrame() {
        const ring = this._ring;
        for (let j = 0; j < FRAME; j++) {
            ring[this._wr] = this._in[j];
            this._wr = (this._wr + 1) % RING;
        }
        this._avail += FRAME;
        if (this._avail > RING) {
            // Overrun guard (should not happen in steady state): drop oldest.
            const drop = this._avail - RING;
            this._rd = (this._rd + drop) % RING;
            this._avail = RING;
        }
    }

    process(inputs, outputs) {
        const input = inputs[0];
        const output = outputs[0];
        if (!output || !output[0]) return true;
        const outCh = output[0];
        const n = outCh.length;

        // Not initialized, or no input this quantum: pass the input straight
        // through (or silence) so audio is never dropped.
        if (!this._ready || !input || !input[0]) {
            if (input && input[0]) {
                outCh.set(input[0]);
            } else {
                outCh.fill(0);
            }
            for (let c = 1; c < output.length; c++) {
                if (output[c]) output[c].set(outCh);
            }
            return true;
        }

        const inCh = input[0];
        const m = this._m;
        for (let i = 0; i < n; i++) {
            this._in[this._inFill++] = inCh[i];
            if (this._inFill === FRAME) {
                this._inFill = 0;
                // Re-read HEAPF32 each frame in case the heap grew/moved.
                const heap = m.HEAPF32;
                const base = this._heapIdx;
                for (let j = 0; j < FRAME; j++) heap[base + j] = this._in[j] * SHIFT;
                m._rnnoise_process_frame(this._state, this._ptr, this._ptr);
                for (let j = 0; j < FRAME; j++) this._in[j] = heap[base + j] / SHIFT;
                this._pushDenoisedFrame();
                if (!this._started && this._avail >= n) this._started = true;
            }
        }

        if (this._started && this._avail >= n) {
            const ring = this._ring;
            for (let i = 0; i < n; i++) {
                outCh[i] = ring[this._rd];
                this._rd = (this._rd + 1) % RING;
            }
            this._avail -= n;
        } else {
            // Startup priming (≈ first frame): emit silence until buffered.
            outCh.fill(0);
        }
        for (let c = 1; c < output.length; c++) {
            if (output[c]) output[c].set(outCh);
        }
        return true;
    }
}

registerProcessor('chromatic-rnnoise', RnnoiseWorkletProcessor);
