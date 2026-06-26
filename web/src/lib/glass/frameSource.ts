/**
 * Shared downsampled video frames for the glass renderers and scopes.
 *
 * Every consumer needs the same small frame each tick; producing it once
 * and handing out an ImageBitmap keeps the (potentially 4K) video from
 * being copied repeatedly. Preferred path: createImageBitmap(video,
 * {resize}) — fully async and GPU-friendly in all engines. Fallback:
 * draw onto an OffscreenCanvas first (some engines reject the direct
 * video path); Firefox in particular does video→canvas drawImage on the
 * main thread, which is exactly what the preferred path avoids.
 *
 * Consumers run one frame behind the video — invisible through frost.
 * Returns null when unsupported; consumers fall back to uploading the
 * video element directly.
 */

import { IS_GECKO } from "$lib/platform";
import { activeReviewToolCount, underPressure } from "$lib/perf/loadMonitor";

const TARGET_WIDTH = 1024;
// ~30fps; frost and scopes don't need 60. Gecko pays more per handoff
// (bitmap creation + texture upload), so it gets 20fps — invisible
// through frost, meaningful on its main thread.
const MIN_INTERVAL_MS = IS_GECKO ? 50 : 33;
const PRESSURE_INTERVAL_MS = IS_GECKO ? 100 : 66;
const STACKED_TOOLS_INTERVAL_MS = IS_GECKO ? 120 : 80;

type Mode = "direct" | "canvas" | "unsupported";
let mode: Mode | null = null;

let canvas: OffscreenCanvas | null = null;
let ctx: OffscreenCanvasRenderingContext2D | null = null;
let latest: ImageBitmap | null = null;
let lastVideo: HTMLVideoElement | null = null;
let pending = false;
let lastDrawAt = 0;

/** Drop the cached frame (leaving a room / stream teardown): without
 *  this the last frame of the previous source could briefly drive the
 *  glass and scopes of the next one, and the bitmap leaks tab-wide. */
export function releaseFrames(): void {
	latest?.close();
	latest = null;
	lastVideo = null;
}

function targetHeight(video: HTMLVideoElement): number {
	return Math.max(1, Math.round((TARGET_WIDTH * video.videoHeight) / video.videoWidth));
}

function captureInterval(): number {
	if (activeReviewToolCount() >= 3) return STACKED_TOOLS_INTERVAL_MS;
	if (underPressure() || activeReviewToolCount() >= 2) return PRESSURE_INTERVAL_MS;
	return MIN_INTERVAL_MS;
}

function captureViaCanvas(video: HTMLVideoElement, h: number): void {
	if (typeof OffscreenCanvas === "undefined") {
		mode = "unsupported";
		pending = false;
		return;
	}
	mode = "canvas";
	if (!canvas || canvas.width !== TARGET_WIDTH || canvas.height !== h) {
		canvas = new OffscreenCanvas(TARGET_WIDTH, h);
		ctx = canvas.getContext("2d", { alpha: false });
	}
	if (!ctx) {
		mode = "unsupported";
		pending = false;
		return;
	}
	ctx.drawImage(video, 0, 0, TARGET_WIDTH, h);
	createImageBitmap(canvas)
		.then((bmp) => {
			latest?.close();
			latest = bmp;
		})
		.catch(() => {
			// Keep the previous frame; try again next tick.
		})
		.finally(() => {
			pending = false;
		});
}

/**
 * Latest downsampled frame, or null when unsupported / not ready yet.
 * Kicks off the next capture as a side effect; never blocks the caller.
 */
export function getFrameBitmap(video: HTMLVideoElement): ImageBitmap | null {
	if (mode === "unsupported" || typeof createImageBitmap === "undefined") return null;
	if (video !== lastVideo) {
		releaseFrames();
		lastVideo = video;
	}
	const vw = video.videoWidth;
	if (!vw || !video.videoHeight) return latest;

	const now = performance.now();
	if (!pending && now - lastDrawAt >= captureInterval()) {
		lastDrawAt = now;
		pending = true;
		const h = targetHeight(video);
		if (mode === "canvas") {
			captureViaCanvas(video, h);
		} else {
			createImageBitmap(video, {
				resizeWidth: TARGET_WIDTH,
				resizeHeight: h,
				resizeQuality: "low",
			})
				.then((bmp) => {
					mode = "direct";
					latest?.close();
					latest = bmp;
					pending = false;
				})
				.catch(() => {
					// Engine rejects the direct video path; fall back once.
					captureViaCanvas(video, h);
				});
		}
	}
	return latest;
}
