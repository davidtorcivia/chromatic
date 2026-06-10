package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// newTestStaticDir builds a fake SvelteKit output directory:
//
//	static/
//	  index.html
//	  favicon.png
//	  _app/immutable/chunks/app.abc123.js
func newTestStaticDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	immutableDir := filepath.Join(root, "_app", "immutable", "chunks")
	if err := os.MkdirAll(immutableDir, 0o755); err != nil {
		t.Fatalf("failed to create immutable dir: %v", err)
	}

	files := map[string]string{
		filepath.Join(root, "index.html"):            "<html>index</html>",
		filepath.Join(root, "favicon.png"):           "png-bytes",
		filepath.Join(immutableDir, "app.abc123.js"): "console.log('app')",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("failed to write %s: %v", path, err)
		}
	}

	return root
}

func TestSPAHandlerCacheControl(t *testing.T) {
	handler := spaHandler(newTestStaticDir(t))

	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantCache  string
		wantBody   string
	}{
		{
			name:       "immutable assets are cached forever",
			path:       "/_app/immutable/chunks/app.abc123.js",
			wantStatus: http.StatusOK,
			wantCache:  "public, max-age=31536000, immutable",
			wantBody:   "console.log('app')",
		},
		{
			name:       "root index is no-cache",
			path:       "/",
			wantStatus: http.StatusOK,
			wantCache:  "no-cache",
			wantBody:   "<html>index</html>",
		},
		{
			name:       "regular static file is no-cache",
			path:       "/favicon.png",
			wantStatus: http.StatusOK,
			wantCache:  "no-cache",
			wantBody:   "png-bytes",
		},
		{
			name:       "SPA fallback serves index.html with no-cache",
			path:       "/admin/rooms/some-room",
			wantStatus: http.StatusOK,
			wantCache:  "no-cache",
			wantBody:   "<html>index</html>",
		},
		{
			name:       "missing immutable asset falls back to index.html with no-cache",
			path:       "/_app/immutable/chunks/gone.js",
			wantStatus: http.StatusOK,
			wantCache:  "no-cache",
			wantBody:   "<html>index</html>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Header().Get("Cache-Control"); got != tt.wantCache {
				t.Errorf("Cache-Control = %q, want %q", got, tt.wantCache)
			}
			if got := rec.Body.String(); got != tt.wantBody {
				t.Errorf("body = %q, want %q", got, tt.wantBody)
			}
		})
	}
}
