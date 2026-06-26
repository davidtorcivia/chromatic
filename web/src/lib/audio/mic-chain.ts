// Light microphone cleanup chain: a high-pass filter that cuts
// low-frequency rumble (HVAC, desk thumps, mic handling) plus a soft RMS
// gate that *attenuates* — never hard-mutes — the signal between phrases.
// It runs on top of the browser's own echoCancellation / noiseSuppression /
// autoGainControl constraints and is deliberately gentle so speech onsets
// are never clipped.
//
// Failure is always graceful: any error (no AudioWorklet support, suspended
// AudioContext with no user gesture yet) returns null and the caller sends
// the raw capture track exactly as before.

import { getAudioContext } from './context';
import type { AudioMode, DenoiserEngine } from './audio-mode';
import { createDenoiser, type Denoiser } from './denoiser';

const GATE_PROCESSOR_NAME = 'chromatic-soft-gate';

// Opens at -50 dBFS, closes at -56 dBFS (hysteresis so it doesn't chatter),
// attenuates by 12 dB when closed. ~5 ms attack so the first syllable
// passes, ~200 ms release so word endings don't pump.
const GATE_WORKLET_SOURCE = `
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
registerProcessor('${GATE_PROCESSOR_NAME}', SoftGateProcessor);
`;

// addModule is per-context and must run exactly once for it.
const moduleLoaded = new WeakSet<AudioContext>();

export interface MicChainOptions {
    /** Talkback applies the cleanup chain; studio sends the capture untouched. */
    mode: AudioMode;
    /** Which noise-reduction engine to splice in (talkback only). */
    denoiser: DenoiserEngine;
}

export interface MicChain {
    /** Stream whose audio track should be sent in place of the raw capture. */
    stream: MediaStream;
    /** True when an in-app denoiser is actually running in this chain. When
     *  false in talkback, the caller keeps the browser's native noise
     *  suppression so speech is never left un-denoised. */
    denoiserActive: boolean;
    /** Tears down the graph nodes (does NOT stop the raw capture track). */
    dispose(): void;
}

export async function createMicChain(
    raw: MediaStream,
    opts: MicChainOptions
): Promise<MicChain | null> {
    // Studio / critical-listening: send the capture untouched (no HPF, gate, or
    // denoiser) so reference music / instruments stay pristine.
    if (opts.mode === 'studio') {
        return null;
    }
    try {
        const ctx = await getAudioContext();
        // A suspended context (no user gesture yet, e.g. right after F5)
        // would produce a silent processed track — being heard matters more
        // than noise reduction, so fall back to the raw capture.
        if (ctx.state !== 'running') {
            console.warn('AudioContext not running; sending raw microphone audio');
            return null;
        }
        if (!moduleLoaded.has(ctx)) {
            const url = URL.createObjectURL(
                new Blob([GATE_WORKLET_SOURCE], { type: 'application/javascript' })
            );
            try {
                await ctx.audioWorklet.addModule(url);
            } finally {
                URL.revokeObjectURL(url);
            }
            moduleLoaded.add(ctx);
        }

        const source = ctx.createMediaStreamSource(raw);
        const highpass = ctx.createBiquadFilter();
        highpass.type = 'highpass';
        highpass.frequency.value = 90;
        highpass.Q.value = 0.7;

        // Optional neural denoiser between the high-pass and the gate. If the
        // engine is unimplemented or fails to load, createDenoiser returns null
        // and we run HPF -> gate alone; the caller then keeps native NS.
        let denoiser: Denoiser | null = null;
        if (opts.denoiser !== 'off') {
            denoiser = await createDenoiser(ctx, opts.denoiser);
        }

        const gate = new AudioWorkletNode(ctx, GATE_PROCESSOR_NAME, {
            numberOfInputs: 1,
            numberOfOutputs: 1
        });
        const destination = ctx.createMediaStreamDestination();

        source.connect(highpass);
        let tail: AudioNode = highpass;
        if (denoiser) {
            tail.connect(denoiser.node);
            tail = denoiser.node;
        }
        tail.connect(gate);
        gate.connect(destination);

        return {
            stream: destination.stream,
            denoiserActive: denoiser !== null,
            dispose() {
                try {
                    source.disconnect();
                    highpass.disconnect();
                    denoiser?.dispose();
                    gate.disconnect();
                    // Disconnect the destination too: it holds the processed
                    // MediaStream the sender referenced, and without this the
                    // node lingers in the shared AudioContext across repeated
                    // mic-device switches (a slow graph leak on long sessions).
                    destination.disconnect();
                } catch {
                    // nodes already disconnected
                }
            }
        };
    } catch (err) {
        console.warn('Mic processing chain unavailable; sending raw microphone audio', err);
        return null;
    }
}
