import { describe, it, expect } from "vitest";
import {
    pruneTrail,
    trailAlpha,
    smoothingFactor,
    easeOutCubic,
    rippleAt,
    shouldAppendPoint,
    adaptiveMinDistPx,
    decimateOlderHalf,
    splitLongSegments,
    buildSplineSegments,
    bucketTrailSegments,
    trailStyle,
    subsampleBatch,
    timestampBatch,
    MAX_BATCH_POINTS,
    TRAIL_FADE_MS,
    TRAIL_MAX_POINTS,
    TRAIL_TARGET_POINTS,
    TRAIL_MIN_DIST_PX,
    TRAIL_ADAPTIVE_MAX_DIST_PX,
    TRAIL_MAX_SEG_PX,
    TRAIL_HEAD_WIDTH,
    TRAIL_BODY_MAX_ALPHA,
    TRAIL_TAIL_ALPHA_RATIO,
    TRAIL_BODY_BUCKETS,
    RIPPLE_DURATION_MS,
    RIPPLE_START_RADIUS,
    RIPPLE_END_RADIUS,
    type TrailPoint,
    type BatchPoint,
    type SplineSegment
} from "./laser";

function pt(t: number): TrailPoint {
    return { x: 0.5, y: 0.5, t };
}

describe("pruneTrail", () => {
    it("removes only expired points from the head", () => {
        const now = 10_000;
        const points = [pt(now - 3000), pt(now - 2500), pt(now - 1000), pt(now)];
        pruneTrail(points, now, 2000);
        expect(points).toHaveLength(2);
        expect(points[0].t).toBe(now - 1000);
    });

    it("keeps a point exactly at max age", () => {
        const now = 10_000;
        const points = [pt(now - TRAIL_FADE_MS), pt(now)];
        pruneTrail(points, now);
        expect(points).toHaveLength(2);
    });

    it("returns the same (mutated) array", () => {
        const points = [pt(0)];
        const result = pruneTrail(points, 5000);
        expect(result).toBe(points);
        expect(points).toHaveLength(0);
    });

    it("handles an empty trail", () => {
        expect(pruneTrail([], 1234)).toEqual([]);
    });

    it("enforces the max point cap by dropping oldest entries", () => {
        const now = 1_000_000;
        const points: TrailPoint[] = [];
        for (let i = 0; i < TRAIL_MAX_POINTS + 10; i++) {
            points.push(pt(now - 100 + i)); // all fresh
        }
        pruneTrail(points, now);
        expect(points).toHaveLength(TRAIL_MAX_POINTS);
        // Newest points survive
        expect(points[points.length - 1].t).toBe(now - 100 + TRAIL_MAX_POINTS + 9);
    });
});

describe("trailAlpha", () => {
    it("is 1 for a fresh point and 0 at/after expiry", () => {
        expect(trailAlpha(0)).toBe(1);
        expect(trailAlpha(TRAIL_FADE_MS)).toBe(0);
        expect(trailAlpha(TRAIL_FADE_MS * 2)).toBe(0);
    });

    it("fades linearly", () => {
        expect(trailAlpha(TRAIL_FADE_MS / 2)).toBeCloseTo(0.5);
        expect(trailAlpha(TRAIL_FADE_MS / 4)).toBeCloseTo(0.75);
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

describe("shouldAppendPoint", () => {
    const W = 1000;
    const H = 500;

    it("always records when there is no previous point", () => {
        expect(shouldAppendPoint(undefined, 0.5, 0.5, W, H)).toBe(true);
    });

    it("skips points closer than the minimum pixel distance", () => {
        const last: TrailPoint = { x: 0.5, y: 0.5, t: 0 };
        // 0.001 * 1000px = 1px < 2px
        expect(shouldAppendPoint(last, 0.501, 0.5, W, H)).toBe(false);
        // Identical point
        expect(shouldAppendPoint(last, 0.5, 0.5, W, H)).toBe(false);
    });

    it("records points at or beyond the minimum distance", () => {
        const last: TrailPoint = { x: 0.5, y: 0.5, t: 0 };
        // Exactly 2px horizontally
        expect(shouldAppendPoint(last, 0.5 + TRAIL_MIN_DIST_PX / W, 0.5, W, H)).toBe(true);
        // 5px vertically (0.01 * 500)
        expect(shouldAppendPoint(last, 0.5, 0.51, W, H)).toBe(true);
    });

    it("measures distance in pixel space, not normalized space", () => {
        const last: TrailPoint = { x: 0.5, y: 0.5, t: 0 };
        // Same normalized delta: 1px on a 1000px-wide video (skip)
        // but 4px on a 4000px-wide video (record)
        expect(shouldAppendPoint(last, 0.501, 0.5, 1000, H)).toBe(false);
        expect(shouldAppendPoint(last, 0.501, 0.5, 4000, H)).toBe(true);
    });

    it("respects a custom minimum distance", () => {
        const last: TrailPoint = { x: 0.5, y: 0.5, t: 0 };
        expect(shouldAppendPoint(last, 0.503, 0.5, W, H, 10)).toBe(false);
        expect(shouldAppendPoint(last, 0.503, 0.5, W, H, 3)).toBe(true);
    });
});

describe("adaptiveMinDistPx", () => {
    it("returns the base distance at or below the target count", () => {
        expect(adaptiveMinDistPx(0)).toBe(TRAIL_MIN_DIST_PX);
        expect(adaptiveMinDistPx(1)).toBe(TRAIL_MIN_DIST_PX);
        expect(adaptiveMinDistPx(TRAIL_TARGET_POINTS)).toBe(TRAIL_MIN_DIST_PX);
    });

    it("grows once the trail exceeds the target", () => {
        const over = adaptiveMinDistPx(TRAIL_TARGET_POINTS + 1);
        expect(over).toBeGreaterThan(TRAIL_MIN_DIST_PX);
        expect(adaptiveMinDistPx(TRAIL_TARGET_POINTS * 2)).toBeGreaterThan(over);
    });

    it("ramps quadratically with the overflow ratio", () => {
        expect(adaptiveMinDistPx(180, 2, 90, 1000)).toBeCloseTo(2 * 4); // 2x over -> 4x base
        expect(adaptiveMinDistPx(270, 2, 90, 1000)).toBeCloseTo(2 * 9); // 3x over -> 9x base
    });

    it("is monotonically non-decreasing in trail length", () => {
        let prev = 0;
        for (let len = 0; len <= TRAIL_MAX_POINTS * 3; len++) {
            const d = adaptiveMinDistPx(len);
            expect(d).toBeGreaterThanOrEqual(prev);
            prev = d;
        }
    });

    it("clamps to the maximum distance", () => {
        expect(adaptiveMinDistPx(10_000)).toBe(TRAIL_ADAPTIVE_MAX_DIST_PX);
        expect(adaptiveMinDistPx(100, 2, 90, 3)).toBeLessThanOrEqual(3);
    });

    it("never exceeds the max even when the base does", () => {
        expect(adaptiveMinDistPx(10, 50, 90, 24)).toBe(24);
    });
});

describe("decimateOlderHalf", () => {
    function pts(n: number): TrailPoint[] {
        return Array.from({ length: n }, (_, i) => ({ x: i, y: i, t: i }));
    }

    it("returns the same (mutated) array", () => {
        const points = pts(10);
        expect(decimateOlderHalf(points)).toBe(points);
    });

    it("leaves trails too short to halve untouched", () => {
        for (const n of [0, 1, 2, 3]) {
            const points = pts(n);
            decimateOlderHalf(points);
            expect(points).toHaveLength(n);
        }
    });

    it("drops every other point in the older half only", () => {
        const points = pts(10); // older half = indices 0..4
        decimateOlderHalf(points);
        // keeps 0, 2, 4 from the older half plus all of 5..9
        expect(points.map((p) => p.t)).toEqual([0, 2, 4, 5, 6, 7, 8, 9]);
    });

    it("always keeps the tail point and the entire newer half", () => {
        const points = pts(91);
        const newerHalf = points.slice(45).map((p) => p.t);
        decimateOlderHalf(points);
        expect(points[0].t).toBe(0);
        expect(points.slice(-newerHalf.length).map((p) => p.t)).toEqual(newerHalf);
    });

    it("preserves chronological order", () => {
        const points = pts(50);
        decimateOlderHalf(points);
        for (let i = 1; i < points.length; i++) {
            expect(points[i].t).toBeGreaterThan(points[i - 1].t);
        }
    });

    it("reduces an over-target trail by roughly a quarter", () => {
        const points = pts(TRAIL_TARGET_POINTS + 1);
        decimateOlderHalf(points);
        expect(points.length).toBeLessThanOrEqual(TRAIL_TARGET_POINTS - 20);
        expect(points.length).toBeGreaterThanOrEqual(TRAIL_TARGET_POINTS / 2);
    });
});

describe("adaptive thinning integration (sustained fast circles)", () => {
    it("settles at a bounded trail length, never an ever-denser one", () => {
        const W = 1280;
        const H = 720;
        const trail: TrailPoint[] = [];
        // Mirrors the component's appendTrailPoint.
        const append = (x: number, y: number, t: number) => {
            const last = trail[trail.length - 1];
            if (shouldAppendPoint(last, x, y, W, H, adaptiveMinDistPx(trail.length))) {
                trail.push({ x, y, t });
                if (trail.length > TRAIL_TARGET_POINTS) decimateOlderHalf(trail);
            }
        };
        // Spin 3 revolutions/sec on a 200px-radius circle, sampled at
        // 240Hz for 4 seconds (well past the 2s fade window).
        let maxLen = 0;
        for (let i = 0; i < 960; i++) {
            const t = (i * 1000) / 240;
            const ang = (2 * Math.PI * 3 * t) / 1000;
            append(0.5 + (200 / W) * Math.cos(ang), 0.5 + (200 / H) * Math.sin(ang), t);
            pruneTrail(trail, t);
            maxLen = Math.max(maxLen, trail.length);
        }
        // Bounded: never exceeds the soft target (+1 transient) or hard cap
        expect(maxLen).toBeLessThanOrEqual(TRAIL_TARGET_POINTS + 1);
        expect(maxLen).toBeLessThanOrEqual(TRAIL_MAX_POINTS);
        // Still a real streak at steady state, not thinned to nothing
        expect(trail.length).toBeGreaterThan(20);
    });
});

describe("splitLongSegments", () => {
    function p(x: number, y: number, t: number): TrailPoint {
        return { x, y, t };
    }
    const W = 1000;
    const H = 500;

    it("returns the same array reference when no chord is too long", () => {
        // 10px apart on a 1000px-wide video, well under the 24px default
        const pts = [p(0.5, 0.5, 0), p(0.51, 0.5, 10), p(0.52, 0.5, 20)];
        expect(splitLongSegments(pts, W, H)).toBe(pts);
    });

    it("handles trivial inputs", () => {
        const single = [p(0.5, 0.5, 0)];
        expect(splitLongSegments([], W, H)).toEqual([]);
        expect(splitLongSegments(single, W, H)).toBe(single);
    });

    it("splits a long chord so every piece is at most maxSegPx", () => {
        // 100px horizontal jump -> needs ceil(100/24) = 5 pieces
        const pts = [p(0.5, 0.5, 0), p(0.6, 0.5, 100)];
        const out = splitLongSegments(pts, W, H);
        expect(out).not.toBe(pts);
        expect(out).toHaveLength(6); // 2 originals + 4 inserted
        for (let i = 1; i < out.length; i++) {
            const dx = (out[i].x - out[i - 1].x) * W;
            const dy = (out[i].y - out[i - 1].y) * H;
            expect(Math.hypot(dx, dy)).toBeLessThanOrEqual(TRAIL_MAX_SEG_PX + 1e-9);
        }
    });

    it("preserves original points and interpolates position and time linearly", () => {
        const pts = [p(0.1, 0.2, 0), p(0.2, 0.2, 100)];
        const out = splitLongSegments(pts, W, H, 25); // 100px chord -> 4 pieces
        expect(out[0]).toEqual(pts[0]);
        expect(out[out.length - 1]).toEqual(pts[1]);
        expect(out).toHaveLength(5);
        expect(out[1].x).toBeCloseTo(0.125);
        expect(out[1].y).toBeCloseTo(0.2);
        expect(out[1].t).toBeCloseTo(25);
        expect(out[2].t).toBeCloseTo(50);
    });

    it("measures chords in pixel space", () => {
        // Same normalized delta: ~23px on a 2300px-wide video (no split),
        // ~48px on a 4800px-wide video (split)
        const pts = [p(0.5, 0.5, 0), p(0.51, 0.5, 10)];
        expect(splitLongSegments(pts, 2300, H)).toBe(pts);
        expect(splitLongSegments(pts, 4800, H).length).toBeGreaterThan(2);
    });
});

describe("buildSplineSegments", () => {
    function p(x: number, y: number, t: number): TrailPoint {
        return { x, y, t };
    }

    it("returns [] for fewer than 2 points", () => {
        expect(buildSplineSegments([])).toEqual([]);
        expect(buildSplineSegments([p(0.1, 0.2, 100)])).toEqual([]);
    });

    it("produces a single straight segment for 2 points", () => {
        const segs = buildSplineSegments([p(0, 0, 100), p(0.4, 0.2, 200)]);
        expect(segs).toHaveLength(1);
        const s = segs[0];
        expect(s.x0).toBe(0);
        expect(s.y0).toBe(0);
        expect(s.x1).toBe(0.4);
        expect(s.y1).toBe(0.2);
        // Control points sit on the chord (collinear) at 1/3 and 2/3
        expect(s.c1x).toBeCloseTo(0.4 / 3);
        expect(s.c1y).toBeCloseTo(0.2 / 3);
        expect(s.c2x).toBeCloseTo((0.4 * 2) / 3);
        expect(s.c2y).toBeCloseTo((0.2 * 2) / 3);
        expect(s.t).toBe(200);
        expect(s.pos).toBe(1);
    });

    it("interpolates every input point: segment i runs points[i] -> points[i+1]", () => {
        const pts = [p(0, 0, 0), p(0.2, 0.1, 10), p(0.5, 0.5, 20), p(0.9, 0.4, 30)];
        const segs = buildSplineSegments(pts);
        expect(segs).toHaveLength(3);
        for (let i = 0; i < segs.length; i++) {
            expect(segs[i].x0).toBe(pts[i].x);
            expect(segs[i].y0).toBe(pts[i].y);
            expect(segs[i].x1).toBe(pts[i + 1].x);
            expect(segs[i].y1).toBe(pts[i + 1].y);
        }
    });

    it("is tangent-continuous at interior joints (no angles)", () => {
        const pts = [p(0, 0, 0), p(0.2, 0.1, 10), p(0.5, 0.5, 20), p(0.9, 0.4, 30), p(1, 0.8, 40)];
        const segs = buildSplineSegments(pts);
        for (let i = 1; i < segs.length; i++) {
            // Incoming direction at the joint: end point minus its second
            // control point; outgoing: first control point minus start point.
            const inX = segs[i - 1].x1 - segs[i - 1].c2x;
            const inY = segs[i - 1].y1 - segs[i - 1].c2y;
            const outX = segs[i].c1x - segs[i].x0;
            const outY = segs[i].c1y - segs[i].y0;
            const cross = inX * outY - inY * outX;
            const dot = inX * outX + inY * outY;
            const scale = Math.hypot(inX, inY) * Math.hypot(outX, outY);
            expect(scale).toBeGreaterThan(0);
            expect(Math.abs(cross) / scale).toBeLessThan(1e-9); // parallel
            expect(dot).toBeGreaterThan(0); // same direction
        }
    });

    it("keeps collinear points on the straight line (no wobble)", () => {
        const pts = [p(0.1, 0.3, 0), p(0.2, 0.3, 10), p(0.35, 0.3, 20), p(0.6, 0.3, 30)];
        const segs = buildSplineSegments(pts);
        for (const s of segs) {
            expect(s.c1y).toBeCloseTo(0.3, 12);
            expect(s.c2y).toBeCloseTo(0.3, 12);
            // Control points stay between the segment endpoints (no overshoot)
            expect(s.c1x).toBeGreaterThanOrEqual(s.x0);
            expect(s.c1x).toBeLessThanOrEqual(s.x1);
            expect(s.c2x).toBeGreaterThanOrEqual(s.x0);
            expect(s.c2x).toBeLessThanOrEqual(s.x1);
        }
    });

    it("skips degenerate zero-length chords without producing NaN", () => {
        const pts = [p(0.1, 0.1, 0), p(0.1, 0.1, 10), p(0.5, 0.5, 20)];
        const segs = buildSplineSegments(pts);
        expect(segs).toHaveLength(1);
        for (const s of segs) {
            for (const v of [s.x0, s.y0, s.c1x, s.c1y, s.c2x, s.c2y, s.x1, s.y1]) {
                expect(Number.isFinite(v)).toBe(true);
            }
        }
    });

    it("has monotonically increasing pos ending at 1 and non-decreasing t", () => {
        const pts = [p(0, 0, 0), p(0.1, 0.02, 10), p(0.2, 0.05, 20), p(0.3, 0.01, 30), p(0.4, 0, 40)];
        const segs = buildSplineSegments(pts);
        for (let i = 1; i < segs.length; i++) {
            expect(segs[i].pos).toBeGreaterThan(segs[i - 1].pos);
            expect(segs[i].t).toBeGreaterThanOrEqual(segs[i - 1].t);
        }
        expect(segs[0].pos).toBeGreaterThan(0);
        expect(segs[segs.length - 1].pos).toBe(1);
    });

    it("composes with splitLongSegments for fast flicks", () => {
        // One huge jump: split first, then spline through the dense points
        const pts = [p(0.1, 0.1, 0), p(0.9, 0.8, 33)];
        const dense = splitLongSegments(pts, 1920, 1080);
        const segs = buildSplineSegments(dense);
        expect(segs.length).toBeGreaterThan(10);
        expect(segs[0].x0).toBe(0.1);
        const last = segs[segs.length - 1];
        expect(last.x1).toBe(0.9);
        expect(last.y1).toBe(0.8);
        expect(last.pos).toBe(1);
    });
});

describe("bucketTrailSegments", () => {
    function seg(pos: number, t: number): SplineSegment {
        return { x0: 0, y0: 0, c1x: 0, c1y: 0, c2x: 0, c2y: 0, x1: 0, y1: 0, t, pos };
    }

    function segs(n: number): SplineSegment[] {
        return Array.from({ length: n }, (_, i) => seg((i + 1) / n, i * 10));
    }

    it("returns [] for empty input or a non-positive bucket count", () => {
        expect(bucketTrailSegments([])).toEqual([]);
        expect(bucketTrailSegments(segs(5), 0)).toEqual([]);
    });

    it("never produces more than the requested bucket count", () => {
        expect(bucketTrailSegments(segs(3)).length).toBe(3);
        expect(bucketTrailSegments(segs(TRAIL_BODY_BUCKETS)).length).toBe(TRAIL_BODY_BUCKETS);
        expect(bucketTrailSegments(segs(300)).length).toBe(TRAIL_BODY_BUCKETS);
        expect(bucketTrailSegments(segs(50), 4).length).toBe(4);
    });

    it("gives each segment its own bucket when there are few segments", () => {
        const s = segs(3);
        const buckets = bucketTrailSegments(s);
        expect(buckets).toHaveLength(3);
        for (let i = 0; i < 3; i++) {
            expect(buckets[i].start).toBe(i);
            expect(buckets[i].end).toBe(i + 1);
            // Exact per-segment taper is reproduced
            expect(buckets[i].pos).toBe(s[i].pos);
            expect(buckets[i].t).toBe(s[i].t);
        }
    });

    it("covers all segments contiguously with no gaps or overlaps", () => {
        for (const n of [8, 9, 17, 100, 257]) {
            const buckets = bucketTrailSegments(segs(n));
            expect(buckets[0].start).toBe(0);
            for (let i = 1; i < buckets.length; i++) {
                expect(buckets[i].start).toBe(buckets[i - 1].end);
            }
            expect(buckets[buckets.length - 1].end).toBe(n);
        }
    });

    it("splits into near-equal runs", () => {
        const buckets = bucketTrailSegments(segs(100));
        for (const b of buckets) {
            const size = b.end - b.start;
            expect(size).toBeGreaterThanOrEqual(12);
            expect(size).toBeLessThanOrEqual(13);
        }
    });

    it("takes pos/t from a segment inside the bucket, increasing toward the head", () => {
        const s = segs(64);
        const buckets = bucketTrailSegments(s);
        for (let i = 0; i < buckets.length; i++) {
            const b = buckets[i];
            expect(b.pos).toBeGreaterThanOrEqual(s[b.start].pos);
            expect(b.pos).toBeLessThanOrEqual(s[b.end - 1].pos);
            expect(b.t).toBeGreaterThanOrEqual(s[b.start].t);
            expect(b.t).toBeLessThanOrEqual(s[b.end - 1].t);
            if (i > 0) {
                expect(b.pos).toBeGreaterThan(buckets[i - 1].pos);
                expect(b.t).toBeGreaterThan(buckets[i - 1].t);
            }
        }
    });

    it("composes with buildSplineSegments output", () => {
        const pts: TrailPoint[] = Array.from({ length: 40 }, (_, i) => ({
            x: 0.1 + i * 0.02,
            y: 0.5 + 0.1 * Math.sin(i / 3),
            t: i * 16
        }));
        const spline = buildSplineSegments(pts);
        const buckets = bucketTrailSegments(spline);
        expect(buckets.length).toBe(TRAIL_BODY_BUCKETS);
        expect(buckets[buckets.length - 1].end).toBe(spline.length);
    });
});

describe("trailStyle", () => {
    it("is full width and vivid (near-opaque) at a fresh head", () => {
        const v = trailStyle(1, 1);
        expect(v.width).toBe(TRAIL_HEAD_WIDTH);
        expect(v.alpha).toBe(TRAIL_BODY_MAX_ALPHA);
        expect(v.alpha).toBeGreaterThanOrEqual(0.85);
    });

    it("pinches width to zero at the tail and tapers alpha down", () => {
        const v = trailStyle(0, 1);
        expect(v.width).toBe(0);
        expect(v.alpha).toBeCloseTo(TRAIL_BODY_MAX_ALPHA * TRAIL_TAIL_ALPHA_RATIO);
        expect(v.alpha).toBeLessThan(0.25);
    });

    it("is invisible once life expires", () => {
        const v = trailStyle(1, 0);
        expect(v.width).toBe(0);
        expect(v.alpha).toBe(0);
    });

    it("never exceeds the head alpha and clamps out-of-range inputs", () => {
        for (const pos of [0, 0.25, 0.5, 0.75, 1]) {
            for (const life of [0, 0.5, 1]) {
                expect(trailStyle(pos, life).alpha).toBeLessThanOrEqual(TRAIL_BODY_MAX_ALPHA);
            }
        }
        expect(trailStyle(5, 5).alpha).toBe(TRAIL_BODY_MAX_ALPHA);
        expect(trailStyle(5, 5).width).toBe(TRAIL_HEAD_WIDTH);
        expect(trailStyle(-1, 1).width).toBe(0);
    });

    it("tapers monotonically with position and life", () => {
        expect(trailStyle(0.8, 1).width).toBeGreaterThan(trailStyle(0.4, 1).width);
        expect(trailStyle(0.8, 1).alpha).toBeGreaterThan(trailStyle(0.4, 1).alpha);
        expect(trailStyle(1, 0.9).width).toBeGreaterThan(trailStyle(1, 0.3).width);
        expect(trailStyle(1, 0.9).alpha).toBeGreaterThan(trailStyle(1, 0.3).alpha);
    });

    it("eases width (sqrt) so the body keeps presence", () => {
        // At half strength the width is sqrt(0.5) of head, > linear 0.5
        expect(trailStyle(0.5, 1).width).toBeCloseTo(TRAIL_HEAD_WIDTH * Math.SQRT1_2);
        expect(trailStyle(0.5, 1).width).toBeGreaterThan(TRAIL_HEAD_WIDTH * 0.5);
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
        const indices = out.map((p) => p.x);
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

describe("timestampBatch", () => {
    const pts: BatchPoint[] = [
        { x: 0.1, y: 0.2 },
        { x: 0.3, y: 0.4 },
        { x: 0.5, y: 0.6 },
        { x: 0.7, y: 0.8 }
    ];

    it("preserves coordinates and assigns the end time to the last point", () => {
        const out = timestampBatch(pts, 1000, 1033);
        expect(out).toHaveLength(4);
        for (let i = 0; i < pts.length; i++) {
            expect(out[i].x).toBe(pts[i].x);
            expect(out[i].y).toBe(pts[i].y);
        }
        expect(out[out.length - 1].t).toBe(1033);
    });

    it("spreads timestamps linearly across (startT, endT]", () => {
        const out = timestampBatch(pts, 1000, 1040);
        expect(out.map((p) => p.t)).toEqual([1010, 1020, 1030, 1040]);
    });

    it("is strictly increasing and stays after startT", () => {
        const out = timestampBatch(pts, 500, 533);
        expect(out[0].t).toBeGreaterThan(500);
        for (let i = 1; i < out.length; i++) {
            expect(out[i].t).toBeGreaterThan(out[i - 1].t);
        }
    });

    it("gives a single point exactly the end time", () => {
        const out = timestampBatch([{ x: 0.5, y: 0.5 }], 100, 133);
        expect(out).toHaveLength(1);
        expect(out[0].t).toBe(133);
    });

    it("clamps an inverted window: never timestamps past endT", () => {
        const out = timestampBatch(pts, 2000, 1000);
        for (const p of out) {
            expect(p.t).toBe(1000);
        }
    });

    it("handles an empty batch", () => {
        expect(timestampBatch([], 0, 100)).toEqual([]);
    });

    it("produces trail points compatible with pruneTrail ordering", () => {
        const now = 10_000;
        const out = timestampBatch(pts, now - 33, now);
        pruneTrail(out, now);
        expect(out).toHaveLength(4); // all fresh, nothing pruned
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
