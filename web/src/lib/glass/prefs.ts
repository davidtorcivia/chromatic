/**
 * The single "should glass effects render" predicate: OS media queries
 * (Chromium honors prefers-reduced-transparency; Safari never shipped it)
 * plus the in-app "Reduce transparency" toggle, which sets a class on
 * <html>. Every glass surface must consult the SAME predicate or the
 * toggle and the OS setting drift apart per-effect.
 */
export function glassDisabled(): boolean {
	if (typeof window === "undefined") return true;
	if (window.matchMedia("(prefers-reduced-transparency: reduce)").matches) return true;
	if (window.matchMedia("(prefers-contrast: more)").matches) return true;
	return document.documentElement.classList.contains("reduce-transparency");
}
