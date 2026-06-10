import { describe, it, expect } from "vitest";
import {
    pruneTrail,
    trailAlpha,
    smoothingFactor,
    easeOutCubic,
    rippleAt,
    TRAIL_FADE_MS,
    TRAIL_MAX_POINTS,
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
