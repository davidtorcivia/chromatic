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

/** Minimum on-screen distance (px) between recorded trail points. */
export const TRAIL_MIN_DIST_PX = 2;

/** Trail stroke width at the head (px); tapers to 0 at the tail. */
export const TRAIL_HEAD_WIDTH = 4;

/**
 * Per-slice alpha ceiling for the trail core. With round caps only two
 * adjacent slices ever overlap, so under additive 'lighter' compositing
 * the sum is bounded by ~1.0x the participant color — the streak keeps
 * its per-user hue instead of clipping every channel to white.
 */
export const TRAIL_MAX_ALPHA = 0.5;

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

/**
 * Whether a new point is far enough from the last recorded one to be
 * worth keeping. Distances are measured in screen pixels (points are
 * normalized 0-1, so the video content size scales them). Thinning
 * redundant points is what keeps the smoothed trail from degenerating
 * into a pile of overlapping sub-pixel segments.
 */
export function shouldAppendPoint(
    last: TrailPoint | undefined,
    x: number,
    y: number,
    width: number,
    height: number,
    minDistPx: number = TRAIL_MIN_DIST_PX
): boolean {
    if (!last) return true;
    const dx = (x - last.x) * width;
    const dy = (y - last.y) * height;
    return dx * dx + dy * dy >= minDistPx * minDistPx;
}

/**
 * One drawable slice of a smoothed trail: a quadratic curve from
 * (x0,y0) to (x1,y1) with control point (cx,cy), all in normalized
 * video coords. Slices chain end-to-start so the full set traces one
 * continuous path.
 */
export interface TrailSlice {
    x0: number;
    y0: number;
    cx: number;
    cy: number;
    x1: number;
    y1: number;
    /** Representative timestamp for the age fade (non-decreasing along the chain). */
    t: number;
    /** Position along the trail: near 0 at the tail, exactly 1 at the head. */
    pos: number;
}

/**
 * Midpoint smoothing: builds quadratic slices that pass through segment
 * midpoints with the recorded points as control points (the classic
 * moveTo(mid01), quadraticCurveTo(p1, mid12), ... technique), plus
 * straight stubs so the path still starts at the oldest point and ends
 * exactly at the head. Returns [] for fewer than 2 points.
 */
export function buildTrailSlices(points: TrailPoint[]): TrailSlice[] {
    const n = points.length;
    if (n < 2) return [];

    if (n === 2) {
        const [a, b] = points;
        return [
            {
                x0: a.x,
                y0: a.y,
                cx: (a.x + b.x) / 2,
                cy: (a.y + b.y) / 2,
                x1: b.x,
                y1: b.y,
                t: b.t,
                pos: 1
            }
        ];
    }

    // tail stub + (n-2) midpoint curves + head stub
    const total = n;
    const slices: TrailSlice[] = [];
    let idx = 0;
    const push = (
        x0: number,
        y0: number,
        cx: number,
        cy: number,
        x1: number,
        y1: number,
        t: number
    ) => {
        idx++;
        slices.push({ x0, y0, cx, cy, x1, y1, t, pos: idx / total });
    };

    // Tail stub: oldest point up to the first midpoint (a straight line,
    // expressed as a degenerate quadratic so every slice draws the same way).
    const p0 = points[0];
    const p1 = points[1];
    const m01x = (p0.x + p1.x) / 2;
    const m01y = (p0.y + p1.y) / 2;
    push(p0.x, p0.y, (p0.x + m01x) / 2, (p0.y + m01y) / 2, m01x, m01y, p0.t);

    // Middle: midpoint -> midpoint, curving through each recorded point.
    for (let i = 1; i <= n - 2; i++) {
        const a = points[i - 1];
        const b = points[i];
        const c = points[i + 1];
        push((a.x + b.x) / 2, (a.y + b.y) / 2, b.x, b.y, (b.x + c.x) / 2, (b.y + c.y) / 2, b.t);
    }

    // Head stub: last midpoint up to the live head point.
    const pm = points[n - 2];
    const ph = points[n - 1];
    const mx = (pm.x + ph.x) / 2;
    const my = (pm.y + ph.y) / 2;
    push(mx, my, (mx + ph.x) / 2, (my + ph.y) / 2, ph.x, ph.y, ph.t);

    return slices;
}

/**
 * Stroke style for one trail slice: width and alpha taper smoothly with
 * both position along the trail (pos: 0 tail -> 1 head) and remaining
 * life (1 fresh -> 0 expired). Width eases with sqrt so the body keeps
 * presence and only the tip pinches; alpha stays linear and is capped at
 * TRAIL_MAX_ALPHA so additive compositing cannot blow out to white.
 */
export function trailStyle(pos: number, life: number): { width: number; alpha: number } {
    const k = clamp01(pos) * clamp01(life);
    return {
        width: TRAIL_HEAD_WIDTH * Math.sqrt(k),
        alpha: TRAIL_MAX_ALPHA * k
    };
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
