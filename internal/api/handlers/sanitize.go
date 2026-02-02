package handlers

import (
	"path/filepath"
	"regexp"
	"strings"
)

// htmlTagsRegex matches HTML tags for stripping.
var htmlTagsRegex = regexp.MustCompile(`<[^>]*>`)

// sanitizeText removes HTML tags and trims whitespace.
// Use for chat content and other fields rendered as text in the UI.
func sanitizeText(input string) string {
	// Strip HTML tags.
	stripped := htmlTagsRegex.ReplaceAllString(input, "")
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
