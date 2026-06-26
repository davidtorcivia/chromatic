// Pluggable noise-reduction engine for the talkback mic graph.
//
// A Denoiser wraps an AudioNode that sits between the high-pass filter and the
// soft gate in mic-chain.ts: source -> highpass -> [denoiser] -> gate -> dest.
// RNNoise is the ultra-low-latency neural denoiser. If it fails to
// load/initialize, createDenoiser returns null and the caller keeps the
// browser's native noise suppression so talkback is never left un-denoised.

import { base } from '$app/paths';
import type { DenoiserEngine } from './audio-mode';

export interface Denoiser {
    /** Graph node to splice in: connect source -> node -> next. */
    readonly node: AudioNode;
    /** Tears down the engine's nodes/worklet. Must be idempotent. */
    dispose(): void;
}

// Whether an engine has a working in-app implementation. Drives the decision to
// disable the browser's native noiseSuppression (we only turn it off when our
// own denoiser will actually run).
export function isDenoiserImplemented(engine: DenoiserEngine): boolean {
    return engine === 'rnnoise';
}

// Build a denoiser for the given engine on the supplied context. Returns null
// when the engine is unimplemented or fails to initialize; callers must treat
// null as "no in-app denoise" and fall back to native noise suppression.
export async function createDenoiser(
    ctx: AudioContext,
    engine: DenoiserEngine
): Promise<Denoiser | null> {
    if (engine === 'rnnoise') return createRnnoise(ctx);
    return null;
}

// ---- RNNoise -------------------------------------------------------------

const RNNOISE_PROCESSOR = 'chromatic-rnnoise';
const RNNOISE_WORKLET_URL = `${base}/audio/rnnoise-worklet.js`;
// Confirm the WASM actually initialized before trusting the engine. If the
// worklet doesn't report 'ready' in time we treat it as unavailable so the
// caller falls back to native NS — better safe than shipping a dead node.
const RNNOISE_READY_TIMEOUT_MS = 1500;

// addModule is per-context; load the worklet exactly once per AudioContext.
const moduleLoaded = new WeakSet<AudioContext>();

async function createRnnoise(ctx: AudioContext): Promise<Denoiser | null> {
    try {
        if (!moduleLoaded.has(ctx)) {
            await ctx.audioWorklet.addModule(RNNOISE_WORKLET_URL);
            moduleLoaded.add(ctx);
        }

        const node = new AudioWorkletNode(ctx, RNNOISE_PROCESSOR, {
            numberOfInputs: 1,
            numberOfOutputs: 1,
            channelCount: 1,
            channelCountMode: 'explicit',
            channelInterpretation: 'speakers',
            outputChannelCount: [1]
        });

        const ready = await new Promise<boolean>((resolve) => {
            const timer = setTimeout(() => resolve(false), RNNOISE_READY_TIMEOUT_MS);
            node.port.onmessage = (e: MessageEvent) => {
                const type = (e.data as { type?: string } | null)?.type;
                if (type === 'ready') {
                    clearTimeout(timer);
                    resolve(true);
                } else if (type === 'error') {
                    clearTimeout(timer);
                    console.warn('RNNoise worklet failed to initialize:', (e.data as { message?: string }).message);
                    resolve(false);
                }
            };
        });
        node.port.onmessage = null;

        if (!ready) {
            try {
                node.disconnect();
            } catch {
                // not connected yet
            }
            return null;
        }

        return {
            node,
            dispose() {
                try {
                    node.port.onmessage = null;
                    node.disconnect();
                } catch {
                    // already disconnected
                }
            }
        };
    } catch (err) {
        console.warn('RNNoise denoiser unavailable; falling back to native noise suppression', err);
        return null;
    }
}
