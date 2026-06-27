/**
 * The single "should glass effects render" predicate: OS media queries
 * (Chromium honors prefers-reduced-transparency; Safari never shipped it)
 * plus the in-app "Reduce transparency" toggle, which sets a class on
 * <html>. Every glass surface must consult the SAME predicate or the
 * toggle and the OS setting drift apart per-effect.
 *
 * The WebGL "liquid glass" renderer runs on Firefox too. It is per-pixel
 * scoped — the shader returns alpha=0 for every pixel outside a bar/pill's
 * rounded-rect, so the canvas composites transparently everywhere the UI
 * isn't (the loupe overlay uses the identical technique and renders clean
 * on Gecko). What actually washed the whole frame on Firefox was CSS
 * `backdrop-filter` under a transformed ancestor (a Gecko bug that bleeds
 * the blur across the whole stacking context); that is fixed by keeping
 * `--glass-backdrop: none`, NOT by disabling the GPU renderer here.
 */
export function glassDisabled(): boolean {
	if (typeof window === "undefined") return true;
	if (window.matchMedia("(prefers-reduced-transparency: reduce)").matches) return true;
	if (window.matchMedia("(prefers-contrast: more)").matches) return true;
	return document.documentElement.classList.contains("reduce-transparency");
}
