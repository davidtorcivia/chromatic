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
 * Maximum on-screen chord length (px) between spline input points. Fast
 * flicks produce sparse 30Hz samples; longer chords are subdivided by
 * linear interpolation before smoothing so the spline taper stays dense
 * and no segment reads as a straight rod.
 */
export const TRAIL_MAX_SEG_PX = 24;

/**
 * Body-pass styling. The body is stroked under 'source-over' (not
 * additive), so overlapping round caps between adjacent sub-strokes
 * merely saturate the participant color instead of clipping to white —
 * which is what allows the high alpha here.
 */
export const TRAIL_BODY_MAX_ALPHA = 0.95;
/** Fraction of body alpha that survives at the very tail (before life fade). */
export const TRAIL_TAIL_ALPHA_RATIO = 0.18;

/**
 * Glow-pass styling: one single stroke of the whole spline under
 * 'lighter'. A single stroke never overlaps itself, so this is safe at
 * any alpha; it stays low because it is a halo, not the body.
 */
export const TRAIL_GLOW_WIDTH_RATIO = 2.6; // x TRAIL_HEAD_WIDTH
export const TRAIL_GLOW_ALPHA = 0.22;

/**
 * Hot-core pass: a thin near-white single stroke over just the head
 * portion of the trail (pos >= TRAIL_CORE_MIN_POS) for the classic
 * laser look. Also a single stroke under 'lighter'.
 */
export const TRAIL_CORE_WIDTH_RATIO = 0.4; // x TRAIL_HEAD_WIDTH
export const TRAIL_CORE_ALPHA = 0.6;
export const TRAIL_CORE_MIN_POS = 0.82;

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
 * Subdivide any chord longer than maxSegPx (measured in screen pixels)
 * by linear interpolation, so fast flicks don't hand the spline sparse
 * points whose taper/age would otherwise jump across a long segment.
 * Timestamps are interpolated linearly. Returns the input array
 * unchanged (same reference, no allocation) when nothing needs
 * splitting; otherwise returns a new array.
 */
export function splitLongSegments(
    points: TrailPoint[],
    width: number,
    height: number,
    maxSegPx: number = TRAIL_MAX_SEG_PX
): TrailPoint[] {
    if (points.length < 2 || maxSegPx <= 0) return points;
    const maxSq = maxSegPx * maxSegPx;

    let needsSplit = false;
    for (let i = 1; i < points.length; i++) {
        const dx = (points[i].x - points[i - 1].x) * width;
        const dy = (points[i].y - points[i - 1].y) * height;
        if (dx * dx + dy * dy > maxSq) {
            needsSplit = true;
            break;
        }
    }
    if (!needsSplit) return points;

    const out: TrailPoint[] = [points[0]];
    for (let i = 1; i < points.length; i++) {
        const a = points[i - 1];
        const b = points[i];
        const dx = (b.x - a.x) * width;
        const dy = (b.y - a.y) * height;
        const distSq = dx * dx + dy * dy;
        if (distSq > maxSq) {
            const pieces = Math.ceil(Math.sqrt(distSq) / maxSegPx);
            for (let k = 1; k < pieces; k++) {
                const f = k / pieces;
                out.push({
                    x: a.x + (b.x - a.x) * f,
                    y: a.y + (b.y - a.y) * f,
                    t: a.t + (b.t - a.t) * f
                });
            }
        }
        out.push(b);
    }
    return out;
}

/**
 * One drawable segment of the smoothed trail: a cubic bezier from
 * (x0,y0) to (x1,y1), all in normalized video coords. Segments chain
 * end-to-start so the full set traces one continuous path that passes
 * exactly through every input point.
 */
export interface SplineSegment {
    x0: number;
    y0: number;
    c1x: number;
    c1y: number;
    c2x: number;
    c2y: number;
    x1: number;
    y1: number;
    /** Timestamp of the segment's end point (non-decreasing along the chain). */
    t: number;
    /** Position along the trail: near 0 at the tail, exactly 1 at the head. */
    pos: number;
}

/**
 * Centripetal Catmull-Rom spline through the input points, converted
 * to cubic bezier segments. The curve interpolates every point with G1
 * (tangent-continuous) joints, and the centripetal parameterization
 * (alpha = 0.5) is the variant that provably never forms loops or cusps
 * within a segment — slow strokes with closely spaced points stay
 * knot-free. Degenerate (zero-length) chords are skipped. Returns []
 * for fewer than 2 points.
 */
export function buildSplineSegments(points: TrailPoint[]): SplineSegment[] {
    const n = points.length;
    if (n < 2) return [];

    // Below this squared normalized distance two points are "the same".
    const EPS_SQ = 1e-16;
    const segs: SplineSegment[] = [];
    const total = n - 1;

    for (let i = 0; i < n - 1; i++) {
        const p1 = points[i];
        const p2 = points[i + 1];
        const dxc = p2.x - p1.x;
        const dyc = p2.y - p1.y;
        const chordSq = dxc * dxc + dyc * dyc;
        if (chordSq < EPS_SQ) continue; // degenerate chord: nothing to draw

        // Endpoint phantom neighbors: duplicate the boundary point.
        const p0 = i > 0 ? points[i - 1] : p1;
        const p3 = i + 2 < n ? points[i + 2] : p2;

        // Centripetal knot spacing: d = |chord| ^ 0.5
        const d1 = Math.sqrt(Math.hypot(p1.x - p0.x, p1.y - p0.y));
        const d2 = Math.sqrt(Math.sqrt(chordSq));
        const d3 = Math.sqrt(Math.hypot(p3.x - p2.x, p3.y - p2.y));
        const EPS_D = 1e-6;

        let c1x: number, c1y: number, c2x: number, c2y: number;

        if (d1 < EPS_D) {
            // No previous neighbor (tail end): straight lead-in.
            c1x = p1.x + dxc / 3;
            c1y = p1.y + dyc / 3;
        } else {
            const a = d1 * d1;
            const b = d2 * d2;
            const m = 2 * a + 3 * d1 * d2 + b;
            const den = 3 * d1 * (d1 + d2);
            c1x = (a * p2.x - b * p0.x + m * p1.x) / den;
            c1y = (a * p2.y - b * p0.y + m * p1.y) / den;
        }

        if (d3 < EPS_D) {
            // No next neighbor (head end): straight lead-out.
            c2x = p2.x - dxc / 3;
            c2y = p2.y - dyc / 3;
        } else {
            const a = d3 * d3;
            const b = d2 * d2;
            const m = 2 * a + 3 * d3 * d2 + b;
            const den = 3 * d3 * (d3 + d2);
            c2x = (a * p1.x - b * p3.x + m * p2.x) / den;
            c2y = (a * p1.y - b * p3.y + m * p2.y) / den;
        }

        segs.push({
            x0: p1.x,
            y0: p1.y,
            c1x,
            c1y,
            c2x,
            c2y,
            x1: p2.x,
            y1: p2.y,
            t: p2.t,
            pos: (i + 1) / total
        });
    }

    return segs;
}

/**
 * Body-pass stroke style for one trail segment: width and alpha taper
 * with both position along the trail (pos: 0 tail -> 1 head) and
 * remaining life (1 fresh -> 0 expired). Width eases with sqrt so the
 * body keeps presence and only the tip pinches; alpha runs high
 * (~TRAIL_BODY_MAX_ALPHA at a fresh head) because the body pass is
 * composited with 'source-over', which cannot white-clip.
 */
export function trailStyle(pos: number, life: number): { width: number; alpha: number } {
    const p = clamp01(pos);
    const l = clamp01(life);
    return {
        width: TRAIL_HEAD_WIDTH * Math.sqrt(p * l),
        alpha: TRAIL_BODY_MAX_ALPHA * l * (TRAIL_TAIL_ALPHA_RATIO + (1 - TRAIL_TAIL_ALPHA_RATIO) * p)
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
