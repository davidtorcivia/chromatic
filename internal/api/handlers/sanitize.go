package handlers

import (
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// htmlTagsRegex matches HTML tags for stripping.
var htmlTagsRegex = regexp.MustCompile(`<[^>]*>`)

// isBidiOverride reports whether r is a Unicode bidirectional override or
// isolate character (U+202A–U+202E, U+2066–U+2069). These can visually
// reorder text and are used for name-spoofing attacks.
func isBidiOverride(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}

// sanitizeText removes HTML tags, control characters (except \n and \t),
// Unicode bidi-override characters, and trims whitespace.
// Use for chat content and other fields rendered as text in the UI.
func sanitizeText(input string) string {
	// Strip HTML tags.
	stripped := htmlTagsRegex.ReplaceAllString(input, "")
	// Strip control characters (except newline and tab) and bidi overrides.
	stripped = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || isBidiOverride(r) {
			return -1
		}
		return r
	}, stripped)
	// Trim whitespace.
	stripped = strings.TrimSpace(stripped)
	return stripped
}

// sanitizeFilename normalizes a filename for safe header usage.
func sanitizeFilename(name string) string {
	base := filepath.Base(name)
	base = strings.ReplaceAll(base, `"`, "")
	base = strings.ReplaceAll(base, "\n", "")
	base = strings.ReplaceAll(base, "\r", "")
	base = strings.TrimSpace(base)
	if base == "" {
		return "file"
	}
	return base
}
