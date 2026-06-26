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

import { base } from '$app/paths';
import { getAudioContext } from './context';
import type { AudioMode, DenoiserEngine } from './audio-mode';
import { createDenoiser, type Denoiser } from './denoiser';

const GATE_PROCESSOR_NAME = 'chromatic-soft-gate';
// Served as a same-origin static file (built by scripts/build-worklets.mjs).
// It used to be injected as a blob: URL, but a strict CSP (script-src 'self')
// blocks blob: worklet scripts, which aborted the entire mic cleanup chain.
const GATE_WORKLET_URL = `${base}/audio/gate-worklet.js`;

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
            await ctx.audioWorklet.addModule(GATE_WORKLET_URL);
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
