// Bundles AudioWorklet entries into self-contained classic scripts under
// static/audio/. Worklets can't use ES imports or fetch WASM, so we inline
// everything (including @jitsi/rnnoise-wasm's base64 WASM) ahead of time and
// serve the result as a static asset loaded via audioWorklet.addModule().
//
// Run via `npm run build:worklets` (also wired into prebuild).

import { build } from 'esbuild';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const root = resolve(here, '..');

const entries = [
    {
        in: resolve(root, 'src/lib/audio/rnnoise/worklet-entry.js'),
        out: resolve(root, 'static/audio/rnnoise-worklet.js')
    }
];

for (const entry of entries) {
    await build({
        entryPoints: [entry.in],
        outfile: entry.out,
        bundle: true,
        format: 'iife', // classic script for AudioWorkletGlobalScope
        platform: 'browser',
        target: 'es2021',
        minify: true,
        legalComments: 'none',
        // Emscripten glue has dead Node-only branches; keep those requires out
        // of the browser bundle (never executed in a worklet).
        external: ['fs', 'path', 'crypto', 'module', 'url', 'worker_threads'],
        logLevel: 'info'
    });
    console.log(`built ${entry.out}`);
}
