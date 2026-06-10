// Pure helpers for the laser pointer overlay: trail fading, cursor
// smoothing, and release-ripple animation. Kept free of DOM/canvas
// dependencies so they can be unit tested.

export interface TrailPoint {
    x: number; // normalized 0-1 video coords
    y: number;
    t: number; // timestamp (ms)
}

/** One position sample inside a batched cursor message (normalized 0-1). */
export interface BatchPoint {
    x: number;
    y: number;
}

/** How long a trail point remains visible before fully fading out. */
export const TRAIL_FADE_MS = 2000;

/**
 * Hard cap on stored trail points per cursor (safety backstop).
 * Adaptive thinning (adaptiveMinDistPx + decimateOlderHalf) keeps live
 * trails near TRAIL_TARGET_POINTS, well under this.
 */
export const TRAIL_MAX_POINTS = 128;

/**
 * Soft target for live trail length. Appending past this triggers
 * older-half decimation, and the minimum recording distance scales up
 * as a trail approaches/exceeds it (adaptiveMinDistPx), so sustained
 * fast movement (e.g. spinning circles) settles at a bounded,
 * constant-cost streak instead of an ever-denser one.
 */
export const TRAIL_TARGET_POINTS = 90;

/** Release ripple animation parameters. */
export const RIPPLE_DURATION_MS = 600;
export const RIPPLE_START_RADIUS = 6;
export const RIPPLE_END_RADIUS = 48;

/** Minimum on-screen distance (px) between recorded trail points. */
export const TRAIL_MIN_DIST_PX = 2;

/**
 * Maximum points per batched cursor network message. Coalesced pointer
 * events can produce far more samples per ~33ms send tick than this on
 * high-rate input devices; denser batches are evenly subsampled because
 * ~20 points per 33ms window is already well past visual saturation.
 * Must match the server-side cap in handleCursor.
 */
export const MAX_BATCH_POINTS = 20;

/** Interval (ms) between batched cursor sends while pointing (~30Hz). */
export const CURSOR_SEND_INTERVAL_MS = 33;

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
 * Upper bound for the adaptive minimum recording distance. Matches
 * TRAIL_MAX_SEG_PX so that even maximally thinned trails render smooth:
 * splitLongSegments only has to subdivide chords that decimation (not
 * thinning) widened past this.
 */
export const TRAIL_ADAPTIVE_MAX_DIST_PX = TRAIL_MAX_SEG_PX;

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
 * Number of width/alpha steps the body taper is quantized into. Each
 * bucket is stroked as ONE path instead of one stroke per segment, so
 * the body pass costs at most this many stroke calls per trail per
 * frame. Banding at 8 steps along a fading streak is imperceptible.
 */
export const TRAIL_BODY_BUCKETS = 8;

/**
 * Glow-pass styling: two layered wide strokes of the whole spline under
 * 'lighter' (an inner brighter halo plus a wider faint one) approximate
 * the soft glow that shadowBlur used to provide at a fraction of the
 * cost — shadowBlur scales with path length and dominated frame time on
 * long trails. A single stroke never overlaps itself, so additive
 * compositing cannot white-clip here.
 */
export const TRAIL_GLOW_WIDTH_RATIO = 2.6; // x TRAIL_HEAD_WIDTH
export const TRAIL_GLOW_ALPHA = 0.22;
export const TRAIL_GLOW_OUTER_WIDTH_RATIO = 4.6; // x TRAIL_HEAD_WIDTH
export const TRAIL_GLOW_OUTER_ALPHA = 0.1;

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
 * the time constant (smaller = snappier). The default is tuned for the
 * remote cursor dot: batched messages already carry the dense trail
 * geometry, so the dot only needs enough smoothing to glide between
 * ~30Hz targets without judder — a short tau keeps it close to live.
 */
export function smoothingFactor(dtMs: number, tauMs = 25): number {
    if (dtMs <= 0 || tauMs <= 0) return dtMs > 0 ? 1 : 0;
    return 1 - Math.exp(-dtMs / tauMs);
}

/**
 * Evenly subsample a batch down to at most `max` points, always keeping
 * the first and last samples (the last is the authoritative cursor
 * position; the first anchors continuity with the previous batch).
 * Returns the input array unchanged (same reference) when it already
 * fits.
 */
export function subsampleBatch<T>(points: T[], max: number = MAX_BATCH_POINTS): T[] {
    if (max <= 0) return [];
    if (points.length <= max) return points;
    if (max === 1) return [points[points.length - 1]];
    const out: T[] = new Array(max);
    const step = (points.length - 1) / (max - 1);
    for (let i = 0; i < max; i++) {
        out[i] = points[Math.round(i * step)];
    }
    return out;
}

/**
 * Convert a received batch of positions into trail points whose
 * timestamps are spread linearly across (startT, endT]. The samples in
 * a batch were captured over the preceding send window but arrive in
 * one message; interpolating their timestamps keeps the age-based
 * fade/taper smooth along the batch instead of aging it as one clump.
 * The last point always gets exactly endT. startT is clamped to endT.
 */
export function timestampBatch(
    points: BatchPoint[],
    startT: number,
    endT: number
): TrailPoint[] {
    const span = Math.max(0, endT - startT);
    const n = points.length;
    const out: TrailPoint[] = new Array(n);
    for (let i = 0; i < n; i++) {
        out[i] = {
            x: points[i].x,
            y: points[i].y,
            t: endT - span + (span * (i + 1)) / n
        };
    }
    return out;
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
 * Minimum recording distance scaled by current trail density. At or
 * below the target point count it is the base distance; above it the
 * distance ramps quadratically with the overflow ratio (clamped to
 * `max`), so a trail that is filling up demands progressively more
 * spacing from new points. Sustained fast movement therefore
 * self-limits at a roughly constant point count instead of densifying
 * forever.
 */
export function adaptiveMinDistPx(
    trailLength: number,
    base: number = TRAIL_MIN_DIST_PX,
    target: number = TRAIL_TARGET_POINTS,
    max: number = TRAIL_ADAPTIVE_MAX_DIST_PX
): number {
    if (target <= 0 || trailLength <= target) return Math.min(base, max);
    const ratio = trailLength / target;
    const d = base * ratio * ratio;
    return d > max ? max : d;
}

/**
 * Drop every other point in the older (tail) half of a trail, in
 * place, keeping the tail endpoint and the entire newer half. Returns
 * the same array. Called when a trail grows past TRAIL_TARGET_POINTS:
 * the oldest, most-faded portion loses density nobody can see while
 * the bright head keeps its full geometry, and the trail length drops
 * by ~25% in O(n) with no allocation.
 */
export function decimateOlderHalf(points: TrailPoint[]): TrailPoint[] {
    const n = points.length;
    const half = n >> 1;
    if (half < 2) return points;
    let w = 0;
    for (let i = 0; i < half; i += 2) {
        points[w++] = points[i];
    }
    for (let i = half; i < n; i++) {
        points[w++] = points[i];
    }
    points.length = w;
    return points;
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
 * One contiguous run of spline segments that the body pass strokes as
 * a single path with a single width/alpha. `pos` and `t` come from the
 * run's middle segment and feed trailStyle/trailAlpha for the bucket.
 */
export interface TrailBucket {
    /** First segment index in the run (inclusive). */
    start: number;
    /** One past the last segment index (exclusive). */
    end: number;
    pos: number;
    t: number;
}

/**
 * Split the segment chain into at most `numBuckets` contiguous,
 * near-equal runs for the body pass. Quantizing the taper into ~8
 * steps replaces one stroke call per segment (potentially hundreds per
 * frame) with one per bucket; the steps are imperceptible along a
 * fading streak. With fewer segments than buckets each segment gets
 * its own bucket, which reproduces the exact per-segment taper.
 */
export function bucketTrailSegments(
    segs: SplineSegment[],
    numBuckets: number = TRAIL_BODY_BUCKETS
): TrailBucket[] {
    const n = segs.length;
    if (n === 0 || numBuckets <= 0) return [];
    const count = Math.min(numBuckets, n);
    const out: TrailBucket[] = new Array(count);
    for (let b = 0; b < count; b++) {
        const start = Math.floor((b * n) / count);
        const end = Math.floor(((b + 1) * n) / count);
        const mid = (start + end - 1) >> 1;
        out[b] = { start, end, pos: segs[mid].pos, t: segs[mid].t };
    }
    return out;
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
