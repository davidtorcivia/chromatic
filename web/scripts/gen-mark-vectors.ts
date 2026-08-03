/**
 * Regenerates testdata/mark_vectors.json from the TypeScript mark engine, which
 * is the reference implementation for the geometry. internal/watermark asserts
 * the same file, so the two cannot drift apart silently.
 *
 * Run from web/:  npx vite-node scripts/gen-mark-vectors.ts
 * Only run this when the geometry is deliberately changed: regenerating to make
 * a failing test pass is how the two implementations quietly diverge.
 */
import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";

import {
    DRIFT_PERIOD_MS,
    ROTATION_RADIANS,
    TILE_BASE_HEIGHT,
    TILE_BASE_WIDTH,
    driftOffset,
    opacityScale,
    seedHash,
    tileSize,
    type MarkSpec
} from "../src/lib/video/mark";

const seeds = [
    "",
    "session-0001",
    "0123456789abcdef0123456789abcdef",
    // Non-ASCII on purpose: a charCodeAt-based hash passes every hex seed and
    // fails only here.
    "séance-室-42"
];

const times = [0, 1, 7_500, 22_500, 45_000, 135_000, 1_700_000_000_000, -1_000];

const scales = [0.25, 0.5, 1, 1.5, 3, 4.2];
const dprs = [1, 1.5, 2, 3];

const spec = (scale: number): MarkSpec => ({
    token: "",
    lines: [],
    seed: "",
    opacity: 1,
    scale
});

const vectors = {
    comment:
        "Shared geometry vectors for the mark engine. Asserted by " +
        "web/src/lib/video/mark.test.ts and internal/watermark/mark_test.go. " +
        "Hashes and tile sizes are exact; drift and opacity are floats and " +
        "compare within 1e-9, because sin() is not bit-identical across " +
        "runtimes and the contract is geometry, not float bits.",
    constants: {
        tileBaseWidth: TILE_BASE_WIDTH,
        tileBaseHeight: TILE_BASE_HEIGHT,
        rotationRadians: ROTATION_RADIANS,
        driftPeriodMs: DRIFT_PERIOD_MS
    },
    seedHashes: seeds.map((seed) => ({ seed, hash: seedHash(seed) })),
    tileSizes: scales.flatMap((scale) =>
        dprs.map((dpr) => {
            const { width, height } = tileSize(spec(scale), dpr);
            return { scale, dpr, width, height };
        })
    ),
    drift: seeds.flatMap((seed) =>
        times.map((tMs) => {
            const { dx, dy } = driftOffset(seed, tMs);
            return { seed, tMs, dx, dy };
        })
    ),
    opacity: seeds.flatMap((seed) =>
        times.map((tMs) => ({ seed, tMs, scale: opacityScale(seed, tMs) }))
    )
};

const out = resolve(dirname(fileURLToPath(import.meta.url)), "../../testdata/mark_vectors.json");
writeFileSync(out, JSON.stringify(vectors, null, 2) + "\n");
console.log(`wrote ${out}`);
