package handlers

import (
	"os"
	"path/filepath"
	"testing"
)

// TestIsPathWithin_LogoContainment is the regression guard for the DeleteLogo
// path-traversal fix. DeleteLogo (and GetLogo, and the file handlers) all gate
// os.Remove / ServeFile on isPathWithin(root, dbPath) before touching a path
// that was read from the database — so a logo_path (or file stored_path) that
// ever resolves outside its storage root is refused instead of deleting/serving
// an arbitrary file. This test pins the containment contract.
func TestIsPathWithin_LogoContainment(t *testing.T) {
	root, err := os.MkdirTemp("", "chromatic-contain-*")
	if err != nil {
		t.Fatalf("mkdirtemp: %v", err)
	}
	defer os.RemoveAll(root)

	inside := filepath.Join(root, "default_watermark.png")
	outsideParent := filepath.Dir(root)
	outsideFile := filepath.Join(outsideParent, "etc-passwd-"+filepath.Base(root))

	cases := []struct {
		name   string
		target string
		want   bool
	}{
		{"file directly in root", inside, true},
		{"nested file in root", filepath.Join(root, "sub", "logo.png"), true},
		{"root itself", root, true},
		{"sibling outside root", filepath.Join(outsideParent, "other-dir", "x.png"), false},
		{"parent traversal via ..", filepath.Join(root, "..", "secret.png"), false},
		{"absolute unrelated path", outsideFile, false},
		{"traversal prefix-collision (rootx not under root)", filepath.Join(root+"x", "y.png"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isPathWithin(root, c.target); got != c.want {
				t.Errorf("isPathWithin(%q, %q) = %v, want %v", root, c.target, got, c.want)
			}
		})
	}
}
