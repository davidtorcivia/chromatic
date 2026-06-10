import { describe, it, expect } from "vitest";
import {
    midpointSlice,
    quadPoint,
    sliceChord,
    flattenSlice,
    fadeAlpha,
    smoothingFactor,
    easeOutCubic,
    rippleAt,
    subsampleBatch,
    MAX_BATCH_POINTS,
    TRAIL_FADE_BASE_ALPHA,
    FADE_REFERENCE_FRAME_MS,
    TRAIL_FADE_MS,
    SLICE_MAX_CHORD_PX,
    RIPPLE_DURATION_MS,
    RIPPLE_START_RADIUS,
    RIPPLE_END_RADIUS,
    type BatchPoint,
    type QuadSlice
} from "./laser";

function p(x: number, y: number): BatchPoint {
    return { x, y };
}

describe("midpointSlice", () => {
    it("runs from mid(prev2, prev1) through prev1 to mid(prev1, next)", () => {
        const s = midpointSlice(p(0, 0), p(10, 4), p(20, 20));
        expect(s.x0).toBe(5);
        expect(s.y0).toBe(2);
        expect(s.cx).toBe(10);
        expect(s.cy).toBe(4);
        expect(s.x1).toBe(15);
        expect(s.y1).toBe(12);
    });

    it("starts exactly at prev1 when prev2 === prev1 (first slice of a stroke)", () => {
        const s = midpointSlice(p(10, 4), p(10, 4), p(20, 20));
        expect(s.x0).toBe(10);
        expect(s.y0).toBe(4);
        expect(s.x1).toBe(15);
        expect(s.y1).toBe(12);
    });

    it("ends exactly at prev1 when next === prev1 (closing cap of a stroke)", () => {
        const s = midpointSlice(p(0, 0), p(10, 4), p(10, 4));
        expect(s.x0).toBe(5);
        expect(s.y0).toBe(2);
        expect(s.x1).toBe(10);
        expect(s.y1).toBe(4);
    });

    it("interpolates the control point: the curve passes near prev1 at t=0.5", () => {
        const s = midpointSlice(p(0, 0), p(10, 0), p(20, 10));
        const mid = quadPoint(s, 0.5);
        // B(0.5) = (start + 2*ctrl + end) / 4; with midpoint endpoints it
        // stays within the prev2..next hull, close to prev1.
        expect(mid.x).toBeCloseTo((5 + 2 * 10 + 15) / 4);
        expect(mid.y).toBeCloseTo((0 + 0 + 5) / 4);
    });

    it("chains G1-continuously: consecutive slices share endpoint AND tangent", () => {
        // Points along an arbitrary wiggle.
        const pts = [p(0, 0), p(10, 5), p(18, 20), p(30, 22), p(45, 10)];
        for (let i = 2; i < pts.length - 1; i++) {
            const a = midpointSlice(pts[i - 2], pts[i - 1], pts[i]);
            const b = midpointSlice(pts[i - 1], pts[i], pts[i + 1]);
            // Shared endpoint
            expect(a.x1).toBeCloseTo(b.x0, 12);
            expect(a.y1).toBeCloseTo(b.y0, 12);
            // Equal tangent vectors at the joint: end-minus-control of a
            // equals control-minus-start of b (both are (p[i]-p[i-1])/2).
            expect(a.x1 - a.cx).toBeCloseTo(b.cx - b.x0, 12);
            expect(a.y1 - a.cy).toBeCloseTo(b.cy - b.y0, 12);
        }
    });

    it("keeps collinear points on the straight line (no wobble)", () => {
        const s = midpointSlice(p(0, 3), p(10, 3), p(25, 3));
        for (const t of [0, 0.25, 0.5, 0.75, 1]) {
            expect(quadPoint(s, t).y).toBeCloseTo(3, 12);
        }
    });
});

describe("quadPoint", () => {
    const s: QuadSlice = { x0: 0, y0: 0, cx: 10, cy: 20, x1: 40, y1: 0 };

    it("hits the endpoints at t=0 and t=1", () => {
        expect(quadPoint(s, 0)).toEqual({ x: 0, y: 0 });
        expect(quadPoint(s, 1)).toEqual({ x: 40, y: 0 });
    });

    it("evaluates the standard quadratic bezier at t=0.5", () => {
        const mid = quadPoint(s, 0.5);
        expect(mid.x).toBeCloseTo((0 + 2 * 10 + 40) / 4);
        expect(mid.y).toBeCloseTo((0 + 2 * 20 + 0) / 4);
    });

    it("stays inside the control hull", () => {
        for (let i = 0; i <= 10; i++) {
            const q = quadPoint(s, i / 10);
            expect(q.x).toBeGreaterThanOrEqual(0);
            expect(q.x).toBeLessThanOrEqual(40);
            expect(q.y).toBeGreaterThanOrEqual(0);
            expect(q.y).toBeLessThanOrEqual(20);
        }
    });
});

describe("sliceChord", () => {
    it("measures the straight-line distance between slice endpoints", () => {
        const s: QuadSlice = { x0: 0, y0: 0, cx: 99, cy: 99, x1: 3, y1: 4 };
        expect(sliceChord(s)).toBeCloseTo(5);
    });

    it("is zero for a degenerate slice", () => {
        const s: QuadSlice = { x0: 7, y0: 7, cx: 7, cy: 7, x1: 7, y1: 7 };
        expect(sliceChord(s)).toBe(0);
    });
});

describe("flattenSlice", () => {
    it("returns null when the chord is within the limit (stroke the quadratic directly)", () => {
        const short = midpointSlice(p(0, 0), p(10, 0), p(20, 0));
        expect(flattenSlice(short)).toBeNull();
        // Exactly at the limit: still null
        const exact: QuadSlice = {
            x0: 0,
            y0: 0,
            cx: SLICE_MAX_CHORD_PX / 2,
            cy: 0,
            x1: SLICE_MAX_CHORD_PX,
            y1: 0
        };
        expect(flattenSlice(exact)).toBeNull();
    });

    it("subdivides a fast flick into pieces sampled on the quadratic", () => {
        // 200px chord -> ceil(200/40) = 5 pieces
        const s = midpointSlice(p(0, 0), p(100, 30), p(400, 0));
        const chord = sliceChord(s);
        expect(chord).toBeGreaterThan(SLICE_MAX_CHORD_PX);
        const pts = flattenSlice(s);
        expect(pts).not.toBeNull();
        const expectedPieces = Math.ceil(chord / SLICE_MAX_CHORD_PX);
        expect(pts!).toHaveLength(expectedPieces);
        // Every sample lies exactly on the quadratic at evenly spaced t
        for (let i = 0; i < pts!.length; i++) {
            const q = quadPoint(s, (i + 1) / expectedPieces);
            expect(pts![i].x).toBeCloseTo(q.x, 12);
            expect(pts![i].y).toBeCloseTo(q.y, 12);
        }
        // The last sample is the slice end point (trail stays connected)
        expect(pts![pts!.length - 1].x).toBeCloseTo(s.x1, 12);
        expect(pts![pts!.length - 1].y).toBeCloseTo(s.y1, 12);
    });

    it("keeps each polyline piece at most maxChordPx for a straight flick", () => {
        const s: QuadSlice = { x0: 0, y0: 0, cx: 100, cy: 0, x1: 200, y1: 0 };
        const pts = flattenSlice(s, 40)!;
        let prev = { x: s.x0, y: s.y0 };
        for (const q of pts) {
            expect(Math.hypot(q.x - prev.x, q.y - prev.y)).toBeLessThanOrEqual(40 + 1e-9);
            prev = q;
        }
    });

    it("respects a custom chord limit", () => {
        const s: QuadSlice = { x0: 0, y0: 0, cx: 50, cy: 0, x1: 100, y1: 0 };
        expect(flattenSlice(s, 100)).toBeNull();
        expect(flattenSlice(s, 25)!).toHaveLength(4);
    });

    it("handles a non-positive limit by not subdividing", () => {
        const s: QuadSlice = { x0: 0, y0: 0, cx: 50, cy: 0, x1: 100, y1: 0 };
        expect(flattenSlice(s, 0)).toBeNull();
    });
});

describe("fadeAlpha", () => {
    it("equals the base alpha at the 60fps reference frame", () => {
        expect(fadeAlpha(FADE_REFERENCE_FRAME_MS)).toBeCloseTo(TRAIL_FADE_BASE_ALPHA, 12);
    });

    it("returns 0 for zero or negative dt", () => {
        expect(fadeAlpha(0)).toBe(0);
        expect(fadeAlpha(-16)).toBe(0);
    });

    it("scales with dt so two 120Hz frames fade exactly like one 60Hz frame", () => {
        const half = fadeAlpha(FADE_REFERENCE_FRAME_MS / 2);
        const composed = 1 - (1 - half) * (1 - half);
        expect(composed).toBeCloseTo(fadeAlpha(FADE_REFERENCE_FRAME_MS), 12);
        expect(half).toBeLessThan(TRAIL_FADE_BASE_ALPHA);
        expect(half).toBeGreaterThan(TRAIL_FADE_BASE_ALPHA / 2 - 1e-9);
    });

    it("stays in (0, 1] and grows monotonically with dt", () => {
        let prev = 0;
        for (const dt of [1, 8, 16.67, 33, 100, 1000]) {
            const a = fadeAlpha(dt);
            expect(a).toBeGreaterThan(prev);
            expect(a).toBeLessThanOrEqual(1);
            prev = a;
        }
    });

    it("fades a trail below visibility within TRAIL_FADE_MS of wall-clock time", () => {
        // Simulate 60fps: residual alpha after TRAIL_FADE_MS of frames
        const frames = Math.floor(TRAIL_FADE_MS / FADE_REFERENCE_FRAME_MS);
        let residual = 1;
        for (let i = 0; i < frames; i++) {
            residual *= 1 - fadeAlpha(FADE_REFERENCE_FRAME_MS);
        }
        expect(residual).toBeLessThan(1 / 255);
    });

    it("reaches the same residual at any frame rate (wall-clock equivalence)", () => {
        // 1s of fading: 120 frames at 120Hz vs 60 frames at 60Hz
        const at = (frameMs: number, frames: number) => {
            let residual = 1;
            for (let i = 0; i < frames; i++) {
                residual *= 1 - fadeAlpha(frameMs);
            }
            return residual;
        };
        expect(at(1000 / 120, 120)).toBeCloseTo(at(1000 / 60, 60), 10);
    });

    it("handles degenerate base values", () => {
        expect(fadeAlpha(16, 0)).toBe(0);
        expect(fadeAlpha(16, 1)).toBe(1);
        expect(fadeAlpha(16, 2)).toBe(1);
    });
});

describe("smoothingFactor", () => {
    it("returns 0 for zero or negative dt", () => {
        expect(smoothingFactor(0)).toBe(0);
        expect(smoothingFactor(-16)).toBe(0);
    });

    it("stays within (0, 1) and increases with dt", () => {
        const a = smoothingFactor(8);
        const b = smoothingFactor(16);
        const c = smoothingFactor(200);
        expect(a).toBeGreaterThan(0);
        expect(b).toBeGreaterThan(a);
        expect(c).toBeGreaterThan(b);
        expect(c).toBeLessThanOrEqual(1);
    });

    it("is frame-rate independent (two half steps ~= one full step)", () => {
        const full = smoothingFactor(32);
        const half = smoothingFactor(16);
        const twoHalves = 1 - (1 - half) * (1 - half);
        expect(twoHalves).toBeCloseTo(full, 10);
    });
});

describe("easeOutCubic", () => {
    it("starts at 0, ends at 1, clamps outside [0,1]", () => {
        expect(easeOutCubic(0)).toBe(0);
        expect(easeOutCubic(1)).toBe(1);
        expect(easeOutCubic(-1)).toBe(0);
        expect(easeOutCubic(2)).toBe(1);
    });

    it("decelerates (front-loaded progress)", () => {
        expect(easeOutCubic(0.5)).toBeGreaterThan(0.5);
    });
});

describe("subsampleBatch", () => {
    function batch(n: number): BatchPoint[] {
        return Array.from({ length: n }, (_, i) => ({ x: i, y: i * 2 }));
    }

    it("returns the same array reference when within the cap", () => {
        const pts = batch(5);
        expect(subsampleBatch(pts, 5)).toBe(pts);
        expect(subsampleBatch(pts, 20)).toBe(pts);
        const empty: BatchPoint[] = [];
        expect(subsampleBatch(empty)).toBe(empty);
    });

    it("uses MAX_BATCH_POINTS as the default cap", () => {
        expect(subsampleBatch(batch(MAX_BATCH_POINTS)).length).toBe(MAX_BATCH_POINTS);
        expect(subsampleBatch(batch(MAX_BATCH_POINTS * 3)).length).toBe(MAX_BATCH_POINTS);
    });

    it("keeps the first and last points when subsampling", () => {
        const pts = batch(97);
        const out = subsampleBatch(pts, 20);
        expect(out).toHaveLength(20);
        expect(out[0]).toBe(pts[0]);
        expect(out[out.length - 1]).toBe(pts[pts.length - 1]);
    });

    it("samples evenly (monotonic source indices, roughly uniform spacing)", () => {
        const pts = batch(101);
        const out = subsampleBatch(pts, 11);
        const indices = out.map((q) => q.x);
        for (let i = 1; i < indices.length; i++) {
            expect(indices[i]).toBeGreaterThan(indices[i - 1]);
            expect(indices[i] - indices[i - 1]).toBeGreaterThanOrEqual(9);
            expect(indices[i] - indices[i - 1]).toBeLessThanOrEqual(11);
        }
    });

    it("handles degenerate caps", () => {
        const pts = batch(7);
        expect(subsampleBatch(pts, 0)).toEqual([]);
        const one = subsampleBatch(pts, 1);
        expect(one).toHaveLength(1);
        expect(one[0]).toBe(pts[6]); // the newest point wins
    });
});

describe("rippleAt", () => {
    it("starts small and opaque", () => {
        const v = rippleAt(0);
        expect(v.radius).toBe(RIPPLE_START_RADIUS);
        expect(v.alpha).toBe(1);
        expect(v.done).toBe(false);
    });

    it("ends at full radius, transparent and done", () => {
        const v = rippleAt(RIPPLE_DURATION_MS);
        expect(v.radius).toBe(RIPPLE_END_RADIUS);
        expect(v.alpha).toBe(0);
        expect(v.done).toBe(true);
    });

    it("grows faster early (ease-out) while alpha fades linearly", () => {
        const v = rippleAt(RIPPLE_DURATION_MS / 2);
        const linearRadius =
            RIPPLE_START_RADIUS + (RIPPLE_END_RADIUS - RIPPLE_START_RADIUS) * 0.5;
        expect(v.radius).toBeGreaterThan(linearRadius);
        expect(v.alpha).toBeCloseTo(0.5);
        expect(v.done).toBe(false);
    });

    it("clamps past the end", () => {
        const v = rippleAt(RIPPLE_DURATION_MS * 3);
        expect(v.radius).toBe(RIPPLE_END_RADIUS);
        expect(v.alpha).toBe(0);
        expect(v.done).toBe(true);
    });
});
