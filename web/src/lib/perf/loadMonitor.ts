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
 *   4. loupe content    — slows content uploads (position untouched)
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

export type ReviewQualityMode = "performance" | "balanced" | "fidelity";
type ToolTier = "loupe" | "scopes";
type ReviewTool = "laser" | ToolTier;

let busyUntil = 0;
let lastNow = 0;
let raf = 0;
let longFrameCount = 0;
let lastLongFrameMs = 0;
let worstLongFrameMs = 0;
let qualityMode: ReviewQualityMode = "balanced";
const activeReviewTools = new Map<ReviewTool, number>();

function loop(now: number) {
	const gap = lastNow ? now - lastNow : 0;
	if (gap > LONG_FRAME_MS && gap < IDLE_GAP_MS) {
		busyUntil = now + LATCH_MS;
		longFrameCount++;
		lastLongFrameMs = gap;
		worstLongFrameMs = Math.max(worstLongFrameMs, gap);
	}
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

export function setReviewQualityMode(mode: ReviewQualityMode): void {
	qualityMode = mode;
}

export function getReviewQualityMode(): ReviewQualityMode {
	return qualityMode;
}

export function setReviewToolActive(tool: ReviewTool, active: boolean): void {
	const count = activeReviewTools.get(tool) ?? 0;
	if (active) {
		activeReviewTools.set(tool, count + 1);
	} else if (count <= 1) {
		activeReviewTools.delete(tool);
	} else {
		activeReviewTools.set(tool, count - 1);
	}
}

export function activeReviewToolCount(): number {
	return activeReviewTools.size;
}

export function loadSnapshot() {
	return {
		underPressure: underPressure(),
		activeReviewToolCount: activeReviewToolCount(),
		qualityMode,
		longFrameCount,
		lastLongFrameMs: lastLongFrameMs || null,
		worstLongFrameMs: worstLongFrameMs || null,
	};
}

/* The executable ladder: per-tier backoff multipliers applied to a
 * consumer's base interval while under pressure. Defining them here keeps
 * the documented ordering and the shipped behavior the same artifact. */
const TIER_BACKOFF: Record<ToolTier, number> = {
	loupe: 2,
	scopes: 2.4,
};

const COLOAD_BACKOFF: Record<ToolTier, number> = {
	loupe: 1.4,
	scopes: 1.8,
};

const MODE_BACKOFF: Record<ReviewQualityMode, Record<ToolTier, number>> = {
	performance: { loupe: 1.6, scopes: 2.2 },
	balanced: { loupe: 1, scopes: 1 },
	fidelity: { loupe: 0.85, scopes: 0.85 },
};

/** A consumer's effective frame interval given the current load. */
export function degradedInterval(tier: ToolTier, baseMs: number): number {
	let multiplier = MODE_BACKOFF[qualityMode][tier];
	if (underPressure()) {
		multiplier *= TIER_BACKOFF[tier];
	}
	if (activeReviewToolCount() >= 2) {
		multiplier *= COLOAD_BACKOFF[tier];
	}
	if (activeReviewToolCount() >= 3) {
		multiplier *= COLOAD_BACKOFF[tier];
	}
	return baseMs * multiplier;
}
