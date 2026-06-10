import { describe, it, expect } from "vitest";
import {
    pruneTrail,
    trailAlpha,
    smoothingFactor,
    easeOutCubic,
    rippleAt,
    shouldAppendPoint,
    buildTrailSlices,
    trailStyle,
    TRAIL_FADE_MS,
    TRAIL_MAX_POINTS,
    TRAIL_MIN_DIST_PX,
    TRAIL_HEAD_WIDTH,
    TRAIL_MAX_ALPHA,
    RIPPLE_DURATION_MS,
    RIPPLE_START_RADIUS,
    RIPPLE_END_RADIUS,
    type TrailPoint
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

describe("buildTrailSlices", () => {
    function p(x: number, y: number, t: number): TrailPoint {
        return { x, y, t };
    }

    it("returns [] for fewer than 2 points", () => {
        expect(buildTrailSlices([])).toEqual([]);
        expect(buildTrailSlices([p(0.1, 0.2, 100)])).toEqual([]);
    });

    it("produces a single straight slice for 2 points", () => {
        const slices = buildTrailSlices([p(0, 0, 100), p(0.4, 0.2, 200)]);
        expect(slices).toHaveLength(1);
        const s = slices[0];
        expect(s.x0).toBe(0);
        expect(s.y0).toBe(0);
        expect(s.x1).toBe(0.4);
        expect(s.y1).toBe(0.2);
        // Degenerate quadratic: control point on the segment midpoint
        expect(s.cx).toBeCloseTo(0.2);
        expect(s.cy).toBeCloseTo(0.1);
        expect(s.t).toBe(200);
        expect(s.pos).toBe(1);
    });

    it("starts at the oldest point and ends exactly at the head", () => {
        const pts = [p(0, 0, 0), p(0.2, 0.1, 10), p(0.5, 0.5, 20), p(0.9, 0.4, 30)];
        const slices = buildTrailSlices(pts);
        expect(slices[0].x0).toBe(0);
        expect(slices[0].y0).toBe(0);
        const last = slices[slices.length - 1];
        expect(last.x1).toBe(0.9);
        expect(last.y1).toBe(0.4);
    });

    it("chains continuously: each slice starts where the previous ended", () => {
        const pts = [p(0, 0, 0), p(0.2, 0.1, 10), p(0.5, 0.5, 20), p(0.9, 0.4, 30), p(1, 1, 40)];
        const slices = buildTrailSlices(pts);
        for (let i = 1; i < slices.length; i++) {
            expect(slices[i].x0).toBeCloseTo(slices[i - 1].x1, 12);
            expect(slices[i].y0).toBeCloseTo(slices[i - 1].y1, 12);
        }
    });

    it("uses recorded points as control points and midpoints as joints", () => {
        const pts = [p(0, 0, 0), p(0.4, 0.2, 10), p(0.8, 0.6, 20)];
        const slices = buildTrailSlices(pts);
        // tail stub + 1 middle + head stub
        expect(slices).toHaveLength(3);
        const mid = slices[1];
        // Middle slice runs midpoint -> midpoint, curving through p1
        expect(mid.x0).toBeCloseTo((0 + 0.4) / 2);
        expect(mid.y0).toBeCloseTo((0 + 0.2) / 2);
        expect(mid.cx).toBe(0.4);
        expect(mid.cy).toBe(0.2);
        expect(mid.x1).toBeCloseTo((0.4 + 0.8) / 2);
        expect(mid.y1).toBeCloseTo((0.2 + 0.6) / 2);
    });

    it("has monotonically increasing pos ending at 1 and non-decreasing t", () => {
        const pts = [p(0, 0, 0), p(0.1, 0, 10), p(0.2, 0, 20), p(0.3, 0, 30), p(0.4, 0, 40)];
        const slices = buildTrailSlices(pts);
        for (let i = 1; i < slices.length; i++) {
            expect(slices[i].pos).toBeGreaterThan(slices[i - 1].pos);
            expect(slices[i].t).toBeGreaterThanOrEqual(slices[i - 1].t);
        }
        expect(slices[0].pos).toBeGreaterThan(0);
        expect(slices[slices.length - 1].pos).toBe(1);
    });
});

describe("trailStyle", () => {
    it("is full width and capped alpha at a fresh head", () => {
        const v = trailStyle(1, 1);
        expect(v.width).toBe(TRAIL_HEAD_WIDTH);
        expect(v.alpha).toBe(TRAIL_MAX_ALPHA);
    });

    it("pinches to zero at the tail", () => {
        const v = trailStyle(0, 1);
        expect(v.width).toBe(0);
        expect(v.alpha).toBe(0);
    });

    it("is invisible once life expires", () => {
        const v = trailStyle(1, 0);
        expect(v.width).toBe(0);
        expect(v.alpha).toBe(0);
    });

    it("never exceeds the additive-safe alpha cap", () => {
        for (const pos of [0, 0.25, 0.5, 0.75, 1]) {
            for (const life of [0, 0.5, 1]) {
                expect(trailStyle(pos, life).alpha).toBeLessThanOrEqual(TRAIL_MAX_ALPHA);
            }
        }
        // Clamps out-of-range inputs too
        expect(trailStyle(5, 5).alpha).toBe(TRAIL_MAX_ALPHA);
        expect(trailStyle(-1, 1).alpha).toBe(0);
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
