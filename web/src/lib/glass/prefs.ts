import { IS_GECKO } from "$lib/platform";

/**
 * The single "should glass effects render" predicate: OS media queries
 * (Chromium honors prefers-reduced-transparency; Safari never shipped it)
 * plus the in-app "Reduce transparency" toggle, which sets a class on
 * <html>. Every glass surface must consult the SAME predicate or the
 * toggle and the OS setting drift apart per-effect.
 *
 * Firefox: the WebGL "liquid glass" renderer washes a grey/tinted veil
 * across the WHOLE frame there (the same whole-frame bleed that forced us
 * off CSS backdrop-filter — Gecko composites the effect over the entire
 * canvas, not just the bar footprints). For a color-review app that's a
 * non-starter, so we render no GPU glass on Gecko; the bars fall back to
 * translucent CSS glass (tints only their own footprint, image untouched
 * everywhere else). Chromium keeps the full frosted renderer.
 */
export function glassDisabled(): boolean {
	if (typeof window === "undefined") return true;
	if (IS_GECKO) return true;
	if (window.matchMedia("(prefers-reduced-transparency: reduce)").matches) return true;
	if (window.matchMedia("(prefers-contrast: more)").matches) return true;
	return document.documentElement.classList.contains("reduce-transparency");
}
