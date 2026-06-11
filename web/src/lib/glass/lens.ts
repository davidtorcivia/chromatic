/**
 * Refractive "liquid glass" lens — Chromium-only progressive enhancement.
 *
 * Chromium is the only engine that applies SVG filters in backdrop-filter
 * (WebKit bug 245510, open since 2022; Gecko same). Safari/iOS get the
 * CSS-only glass material defined in app.css; this action upgrades the
 * same surfaces with true refraction where the platform allows it.
 *
 * Optics (validated headlessly against Chromium): displacement must run
 * BEFORE the blur or the bend gets smeared into invisibility, and a
 * subtle whole-surface magnification (sampling toward the center) is
 * what reads as "glass" — a rim-only bend is imperceptible over video.
 * Chain: displace(map) → blur → saturate, then brightness via a CSS
 * function appended after url() (mixed lists verified working).
 */

import { IS_CHROMIUM } from "$lib/platform";
import { glassDisabled } from "./prefs";

interface LensOptions {
	/** Gaussian blur stdDeviation (≈ CSS blur px), applied after displacement. */
	blur?: number;
	/** Saturation multiplier (1 = unchanged). */
	saturate?: number;
	/** Brightness multiplier (1 = unchanged). */
	brightness?: number;
	/** feDisplacementMap scale: the maximum encodable shift is scale/2 px. */
	scale?: number;
	/** Whole-surface magnification factor (0.05 = sample 5% toward center). */
	zoom?: number;
	/** Extra outward bend at the rim, in px. */
	rim?: number;
	/** Width of the bending rim in px, measured inward from the edge. */
	bezel?: number;
	/** Falloff exponent for the rim bend (higher = tighter to the edge). */
	exp?: number;
	/** Corner radius of the lens profile; should match the border-radius. */
	radius?: number;
}

const DEFAULTS: Required<LensOptions> = {
	blur: 8,
	saturate: 1.6,
	brightness: 1.06,
	scale: 52,
	zoom: 0.05,
	rim: 24,
	bezel: 24,
	exp: 1.2,
	radius: 20,
};

let lensCounter = 0;
let defsRoot: SVGDefsElement | null = null;

/**
 * True only on Chromium engines. @supports can't be used here: Safari
 * parses `backdrop-filter: url(...)` but never applies it, so the query
 * false-positives. navigator.userAgentData ships only in Chromium.
 */
export function supportsBackdropLens(): boolean {
	if (typeof window === "undefined") return false;
	if (!IS_CHROMIUM) return false;
	return !glassDisabled();
}

function ensureDefs(): SVGDefsElement {
	if (defsRoot && defsRoot.isConnected) return defsRoot;
	const SVG_NS = "http://www.w3.org/2000/svg";
	const svg = document.createElementNS(SVG_NS, "svg");
	svg.setAttribute("aria-hidden", "true");
	svg.style.cssText = "position:fixed;width:0;height:0;pointer-events:none";
	defsRoot = document.createElementNS(SVG_NS, "defs");
	svg.appendChild(defsRoot);
	document.body.appendChild(svg);
	return defsRoot;
}

/** Signed distance to a w×h rounded rect centered in its own box (<0 inside). */
function roundedRectSDF(x: number, y: number, w: number, h: number, r: number): number {
	const qx = Math.abs(x - w / 2) - (w / 2 - r);
	const qy = Math.abs(y - h / 2) - (h / 2 - r);
	const ax = Math.max(qx, 0);
	const ay = Math.max(qy, 0);
	return Math.hypot(ax, ay) + Math.min(Math.max(qx, qy), 0) - r;
}

/**
 * Displacement map for feDisplacementMap: R encodes x-shift, G encodes
 * y-shift, 128 = neutral, full range = ±scale/2 px. Two components:
 * a whole-surface magnification (sample toward center) and an outward
 * rim bend with convex falloff.
 */
function buildDisplacementMap(w: number, h: number, opts: Required<LensOptions>): string {
	const canvas = document.createElement("canvas");
	canvas.width = w;
	canvas.height = h;
	const ctx = canvas.getContext("2d")!;
	const img = ctx.createImageData(w, h);
	const data = img.data;
	const r = Math.min(opts.radius, w / 2, h / 2);
	const bz = Math.min(opts.bezel, w / 2, h / 2);
	const half = opts.scale / 2;

	for (let y = 0; y < h; y++) {
		for (let x = 0; x < w; x++) {
			const px = x + 0.5;
			const py = y + 0.5;
			const d = roundedRectSDF(px, py, w, h, r);
			let dx = 0;
			let dy = 0;
			if (d <= 0) {
				dx = -(px - w / 2) * opts.zoom;
				dy = -(py - h / 2) * opts.zoom;
				const t = Math.min(-d / bz, 1);
				const mag = Math.pow(1 - t, opts.exp) * opts.rim;
				if (mag > 0.01) {
					// Numeric SDF gradient = outward normal
					const gx = roundedRectSDF(px + 1, py, w, h, r) - roundedRectSDF(px - 1, py, w, h, r);
					const gy = roundedRectSDF(px, py + 1, w, h, r) - roundedRectSDF(px, py - 1, w, h, r);
					const len = Math.hypot(gx, gy) || 1;
					dx += (gx / len) * mag;
					dy += (gy / len) * mag;
				}
			}
			const i = (y * w + x) * 4;
			data[i] = Math.round(128 + Math.max(-1, Math.min(1, dx / half)) * 127);
			data[i + 1] = Math.round(128 + Math.max(-1, Math.min(1, dy / half)) * 127);
			data[i + 2] = 128;
			data[i + 3] = 255;
		}
	}
	ctx.putImageData(img, 0, 0);
	return canvas.toDataURL("image/png");
}

function buildFilter(id: string, opts: Required<LensOptions>): SVGFilterElement {
	const SVG_NS = "http://www.w3.org/2000/svg";
	const filter = document.createElementNS(SVG_NS, "filter");
	filter.setAttribute("id", id);
	filter.setAttribute("filterUnits", "userSpaceOnUse");
	filter.setAttribute("color-interpolation-filters", "sRGB");

	const feImage = document.createElementNS(SVG_NS, "feImage");
	feImage.setAttribute("result", "map");
	feImage.setAttribute("preserveAspectRatio", "none");

	const feDisp = document.createElementNS(SVG_NS, "feDisplacementMap");
	feDisp.setAttribute("in", "SourceGraphic");
	feDisp.setAttribute("in2", "map");
	feDisp.setAttribute("scale", String(opts.scale));
	feDisp.setAttribute("xChannelSelector", "R");
	feDisp.setAttribute("yChannelSelector", "G");
	feDisp.setAttribute("result", "displaced");

	const feBlur = document.createElementNS(SVG_NS, "feGaussianBlur");
	feBlur.setAttribute("in", "displaced");
	feBlur.setAttribute("stdDeviation", String(opts.blur));
	feBlur.setAttribute("result", "blurred");

	const feSat = document.createElementNS(SVG_NS, "feColorMatrix");
	feSat.setAttribute("in", "blurred");
	feSat.setAttribute("type", "saturate");
	feSat.setAttribute("values", String(opts.saturate));

	filter.append(feImage, feDisp, feBlur, feSat);
	return filter;
}

/**
 * Svelte action: `use:liquidLens` / `use:liquidLens={{ blur: 12 }}`.
 * No-op outside Chromium, so Safari keeps the stylesheet material.
 */
export function liquidLens(node: HTMLElement, options: LensOptions = {}) {
	if (!supportsBackdropLens()) return {};

	const opts = { ...DEFAULTS, ...options };
	const id = `liquid-lens-${++lensCounter}`;
	const filter = buildFilter(id, opts);
	ensureDefs().appendChild(filter);
	const feImage = filter.querySelector("feImage")!;

	let lastW = 0;
	let lastH = 0;
	let raf = 0;

	function update() {
		raf = 0;
		const w = Math.round(node.offsetWidth);
		const h = Math.round(node.offsetHeight);
		if (!w || !h || (w === lastW && h === lastH)) return;
		lastW = w;
		lastH = h;
		const href = buildDisplacementMap(w, h, opts);
		feImage.setAttribute("href", href);
		for (const el of [feImage, filter]) {
			el.setAttribute("x", "0");
			el.setAttribute("y", "0");
			el.setAttribute("width", String(w));
			el.setAttribute("height", String(h));
		}
		node.style.backdropFilter = `url(#${id}) brightness(${opts.brightness})`;
	}

	// Map regeneration is an O(w*h) pixel loop plus a synchronous PNG
	// encode — debounce resize storms instead of regenerating per frame
	// (the stale map stretches via preserveAspectRatio in the interim).
	let regenTimer: ReturnType<typeof setTimeout> | null = null;
	const ro = new ResizeObserver(() => {
		if (regenTimer) clearTimeout(regenTimer);
		regenTimer = setTimeout(update, 120);
	});
	ro.observe(node);
	update();

	// The in-app reduce-transparency toggle must strip the lens from
	// surfaces that are ALREADY mounted (the settings popover itself is
	// open while the user flips the switch).
	const prefsObserver = new MutationObserver(() => {
		if (glassDisabled()) {
			node.style.backdropFilter = "";
		} else if (lastW && lastH) {
			node.style.backdropFilter = `url(#${id}) brightness(${opts.brightness})`;
		}
	});
	prefsObserver.observe(document.documentElement, {
		attributes: true,
		attributeFilter: ["class"],
	});

	return {
		destroy() {
			ro.disconnect();
			prefsObserver.disconnect();
			if (regenTimer) clearTimeout(regenTimer);
			if (raf) cancelAnimationFrame(raf);
			filter.remove();
			node.style.backdropFilter = "";
		},
	};
}
