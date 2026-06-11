/**
 * Cooperative priority for the room's render work.
 *
 * The main thread can't be preempted, so priority is implemented as
 * yielding: a heartbeat watches for dropped frames (rAF gaps), and while
 * the frame budget is strained, lower-priority consumers back off in
 * order. The ladder, highest first:
 *
 *   1. video playback   — browser-managed; wins whenever we yield
 *   2. laser pointer    — never throttled (input fidelity)
 *   3. glass UI         — never degraded (degrading it IS visible lag)
 *   4. loupe content    — halves its content rate (position untouched)
 *   5. scopes           — degrades hardest (~10fps analysis)
 *
 * Only genuine stalls count: a single missed 60Hz frame is normal life
 * (animations, GC) and must not trip the ladder, or the room throttles
 * itself into the very lag it's guarding against. Gaps after idle or a
 * hidden tab are ignored entirely.
 */

const LONG_FRAME_MS = 40; // ~2.5 dropped frames at 60Hz
const IDLE_GAP_MS = 800; // rAF was simply paused; not load
const LATCH_MS = 250;

let busyUntil = 0;
let lastNow = 0;
let raf = 0;

function loop(now: number) {
	const gap = lastNow ? now - lastNow : 0;
	if (gap > LONG_FRAME_MS && gap < IDLE_GAP_MS) busyUntil = now + LATCH_MS;
	lastNow = now;
	raf = requestAnimationFrame(loop);
}

export function startLoadMonitor(): void {
	if (typeof window === "undefined" || raf) return;
	lastNow = 0;
	raf = requestAnimationFrame(loop);
}

export function stopLoadMonitor(): void {
	if (raf) cancelAnimationFrame(raf);
	raf = 0;
}

/** True while recent frames have been running long. */
export function underPressure(): boolean {
	return performance.now() < busyUntil;
}

/* The executable ladder: per-tier backoff multipliers applied to a
 * consumer's base interval while under pressure. Defining them here keeps
 * the documented ordering and the shipped behavior the same artifact. */
const TIER_BACKOFF: Record<"loupe" | "scopes", number> = {
	loupe: 2,
	scopes: 2.4,
};

/** A consumer's effective frame interval given the current load. */
export function degradedInterval(tier: keyof typeof TIER_BACKOFF, baseMs: number): number {
	return underPressure() ? baseMs * TIER_BACKOFF[tier] : baseMs;
}
