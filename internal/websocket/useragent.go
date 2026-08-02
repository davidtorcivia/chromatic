package websocket

import "strings"

// SummarizeUserAgent reduces a User-Agent header to a short "Engine/major OS"
// string for logging, e.g. "Chrome/121 macOS" or "Firefox/124 Windows".
//
// Deliberately coarse. The goal is answering "which browser was this
// participant on?" when a WebRTC bug turns out to be engine-specific — the
// 2026-08-02 renegotiation cascade hinged on Chrome rejecting an SDP that
// Firefox accepted, and nothing in the log could distinguish them. Full UA
// strings are long, noisy, and worse for that purpose than the two facts that
// matter, so we keep only those and cap the fallback.
func SummarizeUserAgent(ua string) string {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return ""
	}

	browser := browserFromUA(ua)
	os := osFromUA(ua)
	switch {
	case browser != "" && os != "":
		return browser + " " + os
	case browser != "":
		return browser
	case os != "":
		return os
	}
	// Unrecognized: keep a bounded slice so a hostile header can't bloat logs.
	if len(ua) > 60 {
		return ua[:60]
	}
	return ua
}

// Order matters: Edge and Opera embed "Chrome", and Chrome embeds "Safari".
// Checking the most specific token first keeps them from collapsing together.
func browserFromUA(ua string) string {
	for _, c := range []struct{ token, name string }{
		{"Edg/", "Edge"},
		{"OPR/", "Opera"},
		{"Firefox/", "Firefox"},
		{"Chrome/", "Chrome"},
		{"Version/", "Safari"}, // Safari carries its version in Version/, not Safari/
	} {
		if v, ok := versionAfter(ua, c.token); ok {
			// Only treat Version/ as Safari when it really is Safari.
			if c.name == "Safari" && !strings.Contains(ua, "Safari/") {
				continue
			}
			return c.name + "/" + v
		}
	}
	return ""
}

// versionAfter returns the major version following token, e.g. "121" for
// "Chrome/121.0.6167.85".
func versionAfter(ua, token string) (string, bool) {
	i := strings.Index(ua, token)
	if i < 0 {
		return "", false
	}
	rest := ua[i+len(token):]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return "", false
	}
	return rest[:end], true
}

func osFromUA(ua string) string {
	switch {
	// Android must precede Linux: Android UAs contain both.
	case strings.Contains(ua, "Android"):
		return "Android"
	case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"):
		return "iOS"
	case strings.Contains(ua, "Mac OS X"):
		return "macOS"
	case strings.Contains(ua, "Windows"):
		return "Windows"
	case strings.Contains(ua, "CrOS"):
		return "ChromeOS"
	case strings.Contains(ua, "Linux"):
		return "Linux"
	}
	return ""
}
