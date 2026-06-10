// Pure helpers for the laser pointer overlay: midpoint-quadratic trail
// slices, dt-scaled decay-canvas fading, cursor smoothing, wire-format
// batching, and release-ripple animation. Kept free of DOM/canvas
// dependencies so they can be unit tested.
//
// Trail architecture (decay canvas): the overlay keeps a persistent
// trail canvas that is never fully redrawn. Each new cursor point
// stamps ONE quadratic slice onto it (midpointSlice), and every RAF
// tick the whole layer is faded toward transparent with a
// destination-out fill (fadeAlpha). Adjacent slices share endpoints
// and tangents, so the stamped curve is inherently continuous at any
// speed, and per-frame cost is one fillRect plus a handful of stamps —
// independent of how long the user has been drawing.

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
 * Per-frame fade strength of the trail layer at a 60fps reference
 * frame (see fadeAlpha for the dt-scaled version). 0.05 leaves
 * 0.95^n of the trail after n frames: visually gone (< 1/255) in
 * ~1.8s of wall-clock time regardless of display refresh rate.
 */
export const TRAIL_FADE_BASE_ALPHA = 0.05;

/** Reference frame duration (ms) that TRAIL_FADE_BASE_ALPHA is tuned for. */
export const FADE_REFERENCE_FRAME_MS = 1000 / 60;

/**
 * Nominal wall-clock fade duration (ms). destination-out fading is
 * asymptotic, so this is the point where the trail is visually gone
 * (sub-1/255 residue at TRAIL_FADE_BASE_ALPHA), not a hard cutoff.
 */
export const TRAIL_FADE_MS = 2000;

/**
 * Extra grace (ms) past TRAIL_FADE_MS before the overlay does its one
 * full clearRect of the trail layer. 8-bit alpha can leave faint
 * ghosts that destination-out never quite removes; the explicit clear
 * wipes them and lets the RAF loop park (idle = zero work).
 */
export const TRAIL_CLEAR_GRACE_MS = 500;

/**
 * Maximum on-screen chord length (px) of a single stamped slice. A
 * fast flick can put consecutive samples far apart; longer slices are
 * subdivided by sampling the quadratic (flattenSlice) so no stamp
 * degenerates into one long straight rod.
 */
export const SLICE_MAX_CHORD_PX = 40;

/**
 * Minimum on-screen distance (px) a new point must move from the last
 * stamped one to produce a slice. Sub-pixel jitter would otherwise
 * pile round caps onto one spot (visible under the additive glow).
 */
export const MIN_STAMP_DIST_PX = 1.5;

/** Trail body-pass stroke width (px). */
export const TRAIL_BODY_WIDTH = 4;

/**
 * Body-pass alpha. Stamped under 'source-over', so self-crossings
 * (spinning circles) repaint the same vivid color instead of
 * accumulating toward white.
 */
export const TRAIL_BODY_ALPHA = 0.92;

/**
 * Glow pass: one wide low-alpha stroke of the same slice, stamped
 * under 'lighter'. Each slice is stamped exactly once, and the glow is
 * the pure participant color, so same-color accumulation at crossings
 * and shared caps clamps toward the saturated hue — never white.
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

// --- Decay-canvas fade ------------------------------------------------------

/**
 * destination-out fill alpha for one frame of trail fading, scaled by
 * the actual frame duration so the trail fades at the same wall-clock
 * rate on any display: A = 1 - (1 - base)^(dt / 16.67ms). At 60fps
 * this is exactly `base`; at 120fps each frame fades half as hard;
 * after a long stall one big fill catches up.
 */
export function fadeAlpha(
    dtMs: number,
    base: number = TRAIL_FADE_BASE_ALPHA,
    referenceFrameMs: number = FADE_REFERENCE_FRAME_MS
): number {
    if (dtMs <= 0 || base <= 0 || referenceFrameMs <= 0) return 0;
    if (base >= 1) return 1;
    return 1 - Math.pow(1 - base, dtMs / referenceFrameMs);
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
