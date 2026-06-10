import { describe, it, expect } from "vitest";
import {
    midpointSlice,
    quadPoint,
    sliceChord,
    flattenSlice,
    fadeForAge,
    bucketExpired,
    pruneBuckets,
    smoothingFactor,
    easeOutCubic,
    rippleAt,
    subsampleBatch,
    MAX_BATCH_POINTS,
    TRAIL_FADE_MS,
    TRAIL_BUCKET_MS,
    TRAIL_HOLD_MS,
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

describe("fadeForAge", () => {
    it("holds FULL alpha through the hold window (no instant dimming)", () => {
        expect(fadeForAge(0)).toBe(1);
        expect(fadeForAge(TRAIL_HOLD_MS / 2)).toBe(1);
        expect(fadeForAge(TRAIL_HOLD_MS)).toBe(1);
        // Negative ages (clock skew) clamp to full alpha, never overshoot.
        expect(fadeForAge(-50)).toBe(1);
    });

    it("reaches EXACTLY 0 at TRAIL_FADE_MS and stays 0 after (hard cutoff, no asymptote)", () => {
        expect(fadeForAge(TRAIL_FADE_MS)).toBe(0);
        expect(fadeForAge(TRAIL_FADE_MS + 1)).toBe(0);
        expect(fadeForAge(TRAIL_FADE_MS * 10)).toBe(0);
    });

    it("decreases strictly monotonically between hold and fade end", () => {
        let prev = fadeForAge(TRAIL_HOLD_MS);
        for (let age = TRAIL_HOLD_MS + 10; age <= TRAIL_FADE_MS; age += 10) {
            const a = fadeForAge(age);
            expect(a).toBeLessThan(prev);
            expect(a).toBeGreaterThanOrEqual(0);
            prev = a;
        }
    });

    it("keeps the streak clearly visible at 1.2-1.5s and nearly gone by 1.9s (the tuned feel)", () => {
        expect(fadeForAge(1200)).toBeGreaterThan(0.35);
        expect(fadeForAge(1500)).toBeGreaterThan(0.15);
        expect(fadeForAge(1900)).toBeLessThan(0.05);
    });

    it("is continuous at both boundaries (no visible pop entering or leaving the fade)", () => {
        // Just past the hold: still essentially 1.
        expect(fadeForAge(TRAIL_HOLD_MS + 1)).toBeGreaterThan(0.999);
        // Just before the end: essentially 0 (smoothstep lands flat).
        expect(fadeForAge(TRAIL_FADE_MS - 1)).toBeLessThan(0.001);
    });

    it("never changes by more than ~2% per 60Hz frame (continuous fade, no stepping)", () => {
        const frameMs = 1000 / 60;
        let maxDelta = 0;
        for (let age = 0; age < TRAIL_FADE_MS; age += frameMs) {
            const delta = fadeForAge(age) - fadeForAge(age + frameMs);
            maxDelta = Math.max(maxDelta, delta);
        }
        // Max slope of 1 - smoothstep is 1.5/(fade - hold) per ms.
        expect(maxDelta).toBeLessThan(0.02);
    });

    it("keeps the open bucket at full alpha (hold covers a whole bucket width)", () => {
        // The component relies on this: an in-progress bucket is at most
        // TRAIL_BUCKET_MS old when it rotates, so it always strokes at 1.
        expect(TRAIL_HOLD_MS).toBeGreaterThanOrEqual(TRAIL_BUCKET_MS);
        expect(fadeForAge(TRAIL_BUCKET_MS)).toBe(1);
    });

    it("respects custom fade/hold parameters", () => {
        expect(fadeForAge(100, 1000, 100)).toBe(1);
        expect(fadeForAge(550, 1000, 100)).toBeCloseTo(0.5, 12); // smoothstep midpoint
        expect(fadeForAge(1000, 1000, 100)).toBe(0);
    });

    it("handles degenerate parameters (fade <= hold collapses to a step)", () => {
        expect(fadeForAge(50, 100, 100)).toBe(1);
        expect(fadeForAge(99, 100, 200)).toBe(1);
        expect(fadeForAge(100, 100, 200)).toBe(0);
    });
});

describe("bucketExpired", () => {
    it("keeps a bucket open strictly within the bucket window", () => {
        expect(bucketExpired(1000, 1000)).toBe(false);
        expect(bucketExpired(1000, 1000 + TRAIL_BUCKET_MS - 1)).toBe(false);
    });

    it("closes the bucket once a full bucket width has elapsed", () => {
        expect(bucketExpired(1000, 1000 + TRAIL_BUCKET_MS)).toBe(true);
        expect(bucketExpired(1000, 1000 + TRAIL_BUCKET_MS * 5)).toBe(true);
    });

    it("respects a custom bucket width", () => {
        expect(bucketExpired(0, 49, 50)).toBe(false);
        expect(bucketExpired(0, 50, 50)).toBe(true);
    });
});

describe("pruneBuckets", () => {
    function buckets(...openedAts: number[]) {
        return openedAts.map((openedAt) => ({ openedAt }));
    }

    it("drops buckets at or past the fade lifetime, keeps younger ones in order", () => {
        const now = 10_000;
        const list = buckets(
            now - TRAIL_FADE_MS - 1, // gone
            now - TRAIL_FADE_MS, // exactly at cutoff: gone (alpha is 0)
            now - TRAIL_FADE_MS + 1, // still live
            now - 500,
            now
        );
        const out = pruneBuckets(list, now);
        expect(out.map((b) => b.openedAt)).toEqual([
            now - TRAIL_FADE_MS + 1,
            now - 500,
            now
        ]);
    });

    it("compacts in place and returns the same array reference", () => {
        const list = buckets(0, 5000);
        const out = pruneBuckets(list, 5000);
        expect(out).toBe(list);
        expect(list).toHaveLength(1);
        expect(list[0].openedAt).toBe(5000);
    });

    it("agrees with fadeForAge: every kept bucket has alpha > 0, every dropped one 0", () => {
        const now = 50_000;
        const ages = [0, 100, 1999, 2000, 2001, 7000];
        const list = buckets(...ages.map((a) => now - a));
        const kept = new Set(pruneBuckets([...list], now).map((b) => b.openedAt));
        for (const b of list) {
            if (kept.has(b.openedAt)) {
                expect(fadeForAge(now - b.openedAt)).toBeGreaterThan(0);
            } else {
                expect(fadeForAge(now - b.openedAt)).toBe(0);
            }
        }
    });

    it("bounds live buckets per cursor at ceil(fade/bucket) for a continuous stroke", () => {
        // Simulate a long stroke: one bucket opens every TRAIL_BUCKET_MS
        // for 10 seconds, pruned each step like the render loop does.
        const list: { openedAt: number }[] = [];
        let maxLive = 0;
        for (let now = 0; now <= 10_000; now += TRAIL_BUCKET_MS) {
            list.push({ openedAt: now });
            pruneBuckets(list, now);
            maxLive = Math.max(maxLive, list.length);
        }
        expect(maxLive).toBeLessThanOrEqual(Math.ceil(TRAIL_FADE_MS / TRAIL_BUCKET_MS));
    });

    it("handles empty input and a custom lifetime", () => {
        expect(pruneBuckets([], 123)).toEqual([]);
        const list = buckets(0, 400, 800);
        pruneBuckets(list, 900, 500);
        expect(list.map((b) => b.openedAt)).toEqual([800]);
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
