import { describe, it, expect } from "vitest";

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
} from "./mark";
import vectors from "../../../../testdata/mark_vectors.json";

// The same fixture is asserted by internal/watermark/mark_test.go. If these two
// ever disagree, a leaked frame cannot be located by the Go side, so the fixture
// is regenerated (web/scripts/gen-mark-vectors.ts) only when the geometry is
// deliberately changed.
//
// Hashes and tile sizes compare exactly. Drift and opacity are floats: sin() is
// not bit-identical across runtimes, and the determinism contract is geometry,
// not float bits. 1e-9 of a tile pitch is far below a pixel.
const EPSILON = 1e-9;

const specWithScale = (scale: number): MarkSpec => ({
    token: "",
    lines: [],
    seed: "",
    opacity: 1,
    scale
});

describe("mark engine geometry", () => {
    it("matches the shared constants", () => {
        expect(TILE_BASE_WIDTH).toBe(vectors.constants.tileBaseWidth);
        expect(TILE_BASE_HEIGHT).toBe(vectors.constants.tileBaseHeight);
        expect(ROTATION_RADIANS).toBe(vectors.constants.rotationRadians);
        expect(DRIFT_PERIOD_MS).toBe(vectors.constants.driftPeriodMs);
    });

    it("hashes seeds exactly, including non-ASCII", () => {
        for (const { seed, hash } of vectors.seedHashes) {
            expect(seedHash(seed), `seed ${JSON.stringify(seed)}`).toBe(hash);
        }
    });

    it("derives tile size from scale and dpr only", () => {
        for (const { scale, dpr, width, height } of vectors.tileSizes) {
            const size = tileSize(specWithScale(scale), dpr);
            expect(size.width, `scale ${scale} dpr ${dpr}`).toBe(width);
            expect(size.height, `scale ${scale} dpr ${dpr}`).toBe(height);
        }
    });

    it("reproduces drift offsets", () => {
        for (const { seed, tMs, dx, dy } of vectors.drift) {
            const offset = driftOffset(seed, tMs);
            expect(offset.dx, `dx ${seed}@${tMs}`).toBeCloseTo(dx, 9);
            expect(offset.dy, `dy ${seed}@${tMs}`).toBeCloseTo(dy, 9);
        }
    });

    it("reproduces opacity modulation", () => {
        for (const { seed, tMs, scale } of vectors.opacity) {
            expect(opacityScale(seed, tMs), `${seed}@${tMs}`).toBeCloseTo(scale, 9);
        }
    });
});

describe("mark engine invariants", () => {
    it("keeps drift within one tile pitch", () => {
        for (let t = 0; t < DRIFT_PERIOD_MS * 3; t += 137) {
            const { dx, dy } = driftOffset("session-0001", t);
            expect(Math.abs(dx)).toBeLessThanOrEqual(1 + EPSILON);
            expect(Math.abs(dy)).toBeLessThanOrEqual(1 + EPSILON);
        }
    });

    it("travels at least a full tile pitch across the period", () => {
        // Amplitude below one pitch would leave a band the mark never covers,
        // which is exactly what temporal averaging looks for.
        let minDx = Infinity;
        let maxDx = -Infinity;
        for (let t = 0; t < DRIFT_PERIOD_MS * 3; t += 97) {
            const { dx } = driftOffset("session-0001", t);
            minDx = Math.min(minDx, dx);
            maxDx = Math.max(maxDx, dx);
        }
        expect(maxDx - minDx).toBeGreaterThan(1.9);
    });

    it("repeats after three periods, not one", () => {
        const seed = "session-0001";
        const at = (t: number) => driftOffset(seed, t);
        expect(at(1_000).dx).toBeCloseTo(at(1_000 + DRIFT_PERIOD_MS * 3).dx, 9);
        expect(at(1_000).dy).toBeCloseTo(at(1_000 + DRIFT_PERIOD_MS * 3).dy, 9);
        // The y component is what breaks the single-period repeat.
        expect(at(1_000).dy).not.toBeCloseTo(at(1_000 + DRIFT_PERIOD_MS).dy, 3);
    });

    it("gives different sessions different paths", () => {
        const a = driftOffset("session-0001", 12_345);
        const b = driftOffset("session-0002", 12_345);
        expect(a.dx).not.toBeCloseTo(b.dx, 3);
    });

    it("survives epoch-scale timestamps", () => {
        // Real callers pass server epoch milliseconds. The reduction before the
        // sine is what keeps this agreeing with Go.
        const now = 1_762_000_000_000;
        const { dx, dy } = driftOffset("session-0001", now);
        expect(Number.isFinite(dx)).toBe(true);
        expect(Number.isFinite(dy)).toBe(true);
        expect(driftOffset("session-0001", now + DRIFT_PERIOD_MS * 3).dx).toBeCloseTo(dx, 9);
    });

    it("keeps opacity modulation subtle", () => {
        for (let t = 0; t < DRIFT_PERIOD_MS; t += 53) {
            const s = opacityScale("session-0001", t);
            expect(s).toBeGreaterThanOrEqual(0.8 - EPSILON);
            expect(s).toBeLessThanOrEqual(1 + EPSILON);
        }
    });

    it("clamps scale and dpr to the configured bounds", () => {
        // Room config allows 0.25-3.0 and the compositor caps dpr at 2; a spec
        // outside those must not produce a tile the Go side cannot reproduce.
        expect(tileSize(specWithScale(99), 1)).toEqual(tileSize(specWithScale(3), 1));
        expect(tileSize(specWithScale(0), 1)).toEqual(tileSize(specWithScale(0.25), 1));
        expect(tileSize(specWithScale(1), 8)).toEqual(tileSize(specWithScale(1), 2));
        expect(tileSize(specWithScale(NaN), 1)).toEqual(tileSize(specWithScale(0.25), 1));
    });
});
