// Pure helpers for the laser pointer overlay: trail fading, cursor
// smoothing, and release-ripple animation. Kept free of DOM/canvas
// dependencies so they can be unit tested.

export interface TrailPoint {
    x: number; // normalized 0-1 video coords
    y: number;
    t: number; // timestamp (ms)
}

/** How long a trail point remains visible before fully fading out. */
export const TRAIL_FADE_MS = 2000;

/** Hard cap on stored trail points per cursor (safety bound). */
export const TRAIL_MAX_POINTS = 256;

/** Release ripple animation parameters. */
export const RIPPLE_DURATION_MS = 600;
export const RIPPLE_START_RADIUS = 6;
export const RIPPLE_END_RADIUS = 48;

function clamp01(v: number): number {
    return v < 0 ? 0 : v > 1 ? 1 : v;
}

/**
 * Remove expired points from the head of a trail (points are ordered
 * oldest -> newest). Mutates the array in place and returns it.
 * Also enforces TRAIL_MAX_POINTS by dropping the oldest entries.
 */
export function pruneTrail(
    points: TrailPoint[],
    now: number,
    maxAgeMs: number = TRAIL_FADE_MS
): TrailPoint[] {
    let drop = 0;
    while (drop < points.length && now - points[drop].t > maxAgeMs) {
        drop++;
    }
    const overflow = points.length - drop - TRAIL_MAX_POINTS;
    if (overflow > 0) drop += overflow;
    if (drop > 0) points.splice(0, drop);
    return points;
}

/**
 * Opacity for a trail point of a given age: 1 when fresh, fading
 * linearly to 0 at maxAgeMs.
 */
export function trailAlpha(ageMs: number, maxAgeMs: number = TRAIL_FADE_MS): number {
    if (maxAgeMs <= 0) return 0;
    return clamp01(1 - ageMs / maxAgeMs);
}

/**
 * Frame-rate independent exponential smoothing factor: the fraction of
 * the remaining distance to the target to cover this frame. tauMs is
 * the time constant (smaller = snappier).
 */
export function smoothingFactor(dtMs: number, tauMs = 55): number {
    if (dtMs <= 0 || tauMs <= 0) return dtMs > 0 ? 1 : 0;
    return 1 - Math.exp(-dtMs / tauMs);
}

export function easeOutCubic(t: number): number {
    const u = 1 - clamp01(t);
    return 1 - u * u * u;
}

export interface RippleVisual {
    radius: number;
    alpha: number;
    done: boolean;
}

/**
 * Visual state of a release ripple at a given age: radius grows with an
 * ease-out curve from RIPPLE_START_RADIUS to RIPPLE_END_RADIUS while
 * opacity fades linearly to 0.
 */
export function rippleAt(
    ageMs: number,
    durationMs: number = RIPPLE_DURATION_MS
): RippleVisual {
    const t = durationMs <= 0 ? 1 : clamp01(ageMs / durationMs);
    const eased = easeOutCubic(t);
    return {
        radius: RIPPLE_START_RADIUS + (RIPPLE_END_RADIUS - RIPPLE_START_RADIUS) * eased,
        alpha: 1 - t,
        done: t >= 1
    };
}
