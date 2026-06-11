/**
 * Engine detection, in exactly one place. Consumers import capabilities
 * instead of re-rolling UA regexes (the sniff was previously pasted into
 * four modules, with SSR guards that had already diverged).
 */

export const IS_GECKO =
	typeof navigator !== "undefined" && /Gecko\/\d/.test(navigator.userAgent);

/** navigator.userAgentData ships only in Chromium engines. Used where
 *  @supports lies (Safari parses backdrop-filter: url() but ignores it). */
export const IS_CHROMIUM = typeof navigator !== "undefined" && "userAgentData" in navigator;
