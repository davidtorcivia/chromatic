// Pure helpers for the laser pointer overlay: midpoint-quadratic trail
// slices, age-bucketed trail fading, cursor smoothing, wire-format
// batching, and release-ripple animation. Kept free of DOM/canvas
// dependencies so they can be unit tested.
//
// Trail architecture (age-bucketed immutable path redraw): each new
// cursor point contributes ONE quadratic slice (midpointSlice) to the
// cursor's CURRENT age bucket. A new bucket opens every TRAIL_BUCKET_MS;
// once a bucket stops receiving slices it is immutable — its geometry
// lives in a cached Path2D that is never rebuilt. Every RAF tick the
// trail canvas is cleared and each live bucket is re-stroked at an
// alpha computed purely from its age (fadeForAge): full alpha through a
// short hold, then a smooth ease to exactly 0 at TRAIL_FADE_MS, when
// the bucket is dropped. This gives an exact wall-clock fade duration,
// a continuous-to-zero disappearance (no 8-bit destination-out
// quantization stall, no residue ghost), and constant per-frame cost:
// at most ceil(TRAIL_FADE_MS / TRAIL_BUCKET_MS) buckets per cursor,
// two strokes each. Adjacent slices share endpoints and tangents —
// including across bucket boundaries — so the chained curve is
// continuous at any speed with no visible joint.

/** One position sample inside a batched cursor message (normalized 0-1). */
export interface BatchPoint {
    x: number;
    y: number;
}

/**
 * One trail slice: a quadratic bezier from (x0,y0) through control
 * point (cx,cy) to (x1,y1). Produced by midpointSlice in whatever
 * coordinate space the inputs were in (the overlay uses canvas px).
 */
export interface QuadSlice {
    x0: number;
    y0: number;
    cx: number;
    cy: number;
    x1: number;
    y1: number;
}

// --- Tunables ---------------------------------------------------------------

/**
 * Exact wall-clock lifetime (ms) of a trail bucket. fadeForAge reaches
 * 0 precisely here (a hard cutoff, not an asymptote) and the overlay
 * drops the bucket, so a trail is COMPLETELY gone TRAIL_FADE_MS after
 * its last point was drawn — no residue, no ghost.
 */
export const TRAIL_FADE_MS = 3000;

/**
 * Width (ms) of one age bucket. Smaller buckets give a finer-grained
 * fade gradient along the trail at the cost of more Path2D strokes per
 * frame; the cap is ceil(TRAIL_FADE_MS / TRAIL_BUCKET_MS) live buckets
 * per cursor (~17 at the defaults). Adjacent buckets differ by one age
 * step, so their alphas are nearly identical and the per-bucket fade
 * reads as one smooth gradient.
 */
export const TRAIL_BUCKET_MS = 120;

/**
 * How long (ms) a bucket holds full alpha before easing toward 0. This
 * is what makes the streak feel "bright then gone" instead of starting
 * to dim the instant it is drawn. Must be >= TRAIL_BUCKET_MS so the
 * still-open (in-progress) bucket always strokes at full alpha.
 */
export const TRAIL_HOLD_MS = 300;

/**
 * Maximum on-screen chord length (px) of a single stamped slice. A
 * fast flick can put consecutive samples far apart; longer slices are
 * subdivided by sampling the quadratic (flattenSlice) so no stamp
 * degenerates into one long straight rod. Kept fairly small so fast
 * strokes read as a smooth curve rather than coarse polyline segments.
 */
export const SLICE_MAX_CHORD_PX = 18;

/**
 * Trail input smoothing: each stamped trail point is an exponential blend
 * this fraction of the way from the previous stamped point toward the raw
 * sample (1 = no smoothing, snappy; lower = smoother but the trail lags
 * slightly behind the live dot). Tames hand/sensor jitter so slow strokes
 * read as a clean curve. Applies to the TRAIL only — the cursor dot still
 * tracks the true pointer with zero lag.
 */
export const TRAIL_SMOOTHING = 0.6;

/**
 * Minimum on-screen distance (px) a new point must move from the last
 * stamped one to produce a slice. Sub-pixel jitter would otherwise
 * pile round caps onto one spot (visible under the additive glow).
 */
export const MIN_STAMP_DIST_PX = 1.5;

/** Trail body-pass stroke width (px). */
export const TRAIL_BODY_WIDTH = 4;

/**
 * Body-pass alpha (multiplied by the bucket's fadeForAge). Stroked
 * under 'source-over', so self-crossings (spinning circles) repaint
 * the same vivid color instead of accumulating toward white.
 */
export const TRAIL_BODY_ALPHA = 0.92;

/**
 * Glow pass: one wide low-alpha stroke of the same bucket path,
 * composited under 'lighter'. A single stroke() rasterizes the whole
 * path once (in-bucket overlap cannot double-blend), and the glow is
 * the pure participant color, so same-color accumulation across
 * buckets and crossings clamps toward the saturated hue — never white.
 */
export const TRAIL_GLOW_WIDTH_RATIO = 3; // x TRAIL_BODY_WIDTH
export const TRAIL_GLOW_ALPHA = 0.2;

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

/** Release ripple animation parameters. */
export const RIPPLE_DURATION_MS = 600;
export const RIPPLE_START_RADIUS = 6;
export const RIPPLE_END_RADIUS = 48;

function clamp01(v: number): number {
    return v < 0 ? 0 : v > 1 ? 1 : v;
}

// --- Trail slices -----------------------------------------------------------

/**
 * The midpoint-quadratic slice for a new point: a quadratic bezier
 * from mid(prev2, prev1) through prev1 (as control point) to
 * mid(prev1, next). Consecutive slices share both the endpoint and the
 * tangent there — slice N ends at mid(prev1, next) heading along
 * (next - prev1)/2, and slice N+1 starts at the same point with the
 * same direction — so stamping each slice exactly once still yields a
 * globally smooth (G1-continuous) curve.
 *
 * For the first slice of a stroke pass prev2 === prev1 (the slice then
 * starts at prev1 itself); for the closing cap pass next === prev1
 * (the slice ends exactly at the final point).
 */
export function midpointSlice(
    prev2: BatchPoint,
    prev1: BatchPoint,
    next: BatchPoint
): QuadSlice {
    return {
        x0: (prev2.x + prev1.x) / 2,
        y0: (prev2.y + prev1.y) / 2,
        cx: prev1.x,
        cy: prev1.y,
        x1: (prev1.x + next.x) / 2,
        y1: (prev1.y + next.y) / 2
    };
}

/** Point on a quadratic slice at parameter t in [0, 1]. */
export function quadPoint(s: QuadSlice, t: number): BatchPoint {
    const u = 1 - t;
    const a = u * u;
    const b = 2 * u * t;
    const c = t * t;
    return {
        x: a * s.x0 + b * s.cx + c * s.x1,
        y: a * s.y0 + b * s.cy + c * s.y1
    };
}

/** Straight-line distance between the two endpoints of a slice. */
export function sliceChord(s: QuadSlice): number {
    return Math.hypot(s.x1 - s.x0, s.y1 - s.y0);
}

/**
 * Subdivide a slice whose chord exceeds maxChordPx (a fast flick) by
 * sampling the quadratic at evenly spaced parameters. Returns the
 * sample points AFTER the start point (t = 1/n .. 1, so the caller
 * does moveTo(start) then lineTo through them), or null when the slice
 * is short enough to stroke directly as one quadraticCurveTo.
 */
export function flattenSlice(
    s: QuadSlice,
    maxChordPx: number = SLICE_MAX_CHORD_PX
): BatchPoint[] | null {
    if (maxChordPx <= 0) return null;
    const chord = sliceChord(s);
    if (chord <= maxChordPx) return null;
    const pieces = Math.ceil(chord / maxChordPx);
    const out: BatchPoint[] = new Array(pieces);
    for (let i = 1; i <= pieces; i++) {
        out[i - 1] = quadPoint(s, i / pieces);
    }
    return out;
}

// --- Age-bucketed fade --------------------------------------------------------

/**
 * Trail alpha multiplier for a bucket of the given age (ms since the
 * bucket opened). Perceptual curve: hold full alpha through holdMs,
 * then ease to EXACTLY 0 at fadeMs with a smoothstep falloff
 * (1 - u²(3 - 2u)). The curve is C1-continuous at both ends — flat
 * leaving the hold and flat arriving at 0 — so the streak stays vivid
 * briefly, dims smoothly, and vanishes without a visible pop. At the
 * defaults the body is still clearly visible around 1.2-1.5s
 * (~0.46 / ~0.21 of full alpha) and is gone at 2s.
 */
export function fadeForAge(
    ageMs: number,
    fadeMs: number = TRAIL_FADE_MS,
    holdMs: number = TRAIL_HOLD_MS
): number {
    if (ageMs >= fadeMs) return 0;
    if (ageMs <= holdMs || fadeMs <= holdMs) return 1;
    const u = (ageMs - holdMs) / (fadeMs - holdMs);
    return 1 - u * u * (3 - 2 * u);
}

/** Minimal shape of an age bucket as the bookkeeping helpers see it. */
export interface AgedBucket {
    /** performance.now()-style timestamp of the bucket's first slice. */
    openedAt: number;
}

/**
 * Whether a cursor's current bucket is too old to receive new slices:
 * the next slice must open a fresh bucket. Buckets close by AGE (not by
 * slice count) so the fade gradient along the trail is uniform in time.
 */
export function bucketExpired(
    openedAt: number,
    now: number,
    bucketMs: number = TRAIL_BUCKET_MS
): boolean {
    return now - openedAt >= bucketMs;
}

/**
 * Drop buckets that have fully faded (age >= fadeMs), compacting the
 * array in place (returned for convenience). Keeps memory bounded at
 * ceil(fadeMs / bucketMs) live buckets per cursor and keeps per-frame
 * stroke cost constant. Order (oldest first) is preserved.
 */
export function pruneBuckets<T extends AgedBucket>(
    buckets: T[],
    now: number,
    fadeMs: number = TRAIL_FADE_MS
): T[] {
    let keep = 0;
    for (const b of buckets) {
        if (now - b.openedAt < fadeMs) buckets[keep++] = b;
    }
    buckets.length = keep;
    return buckets;
}

// --- Cursor dot smoothing ---------------------------------------------------

/**
 * Frame-rate independent exponential smoothing factor: the fraction of
 * the remaining distance to the target to cover this frame. tauMs is
 * the time constant (smaller = snappier). The default is tuned for the
 * remote cursor dot: batched messages already carry the dense trail
 * geometry (stamped on arrival), so the dot only needs enough
 * smoothing to glide between ~30Hz targets without judder — a short
 * tau keeps it close to live.
 */
export function smoothingFactor(dtMs: number, tauMs = 25): number {
    if (dtMs <= 0 || tauMs <= 0) return dtMs > 0 ? 1 : 0;
    return 1 - Math.exp(-dtMs / tauMs);
}

// --- Wire format ------------------------------------------------------------

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

// --- Release ripples --------------------------------------------------------

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
