package websocket

import "testing"

func TestSummarizeUserAgent(t *testing.T) {
	tests := []struct {
		name string
		ua   string
		want string
	}{
		{
			// The two engines from the 2026-08-02 cascade must be
			// distinguishable, which is the whole point of this field.
			name: "chrome on macos",
			ua:   "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/121.0.6167.85 Safari/537.36",
			want: "Chrome/121 macOS",
		},
		{
			name: "firefox on windows",
			ua:   "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:124.0) Gecko/20100101 Firefox/124.0",
			want: "Firefox/124 Windows",
		},
		{
			// Edge embeds "Chrome/" and must not be reported as Chrome.
			name: "edge is not chrome",
			ua:   "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/121.0.0.0 Safari/537.36 Edg/121.0.2277.83",
			want: "Edge/121 Windows",
		},
		{
			// Opera likewise.
			name: "opera is not chrome",
			ua:   "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36 OPR/106.0.0.0",
			want: "Opera/106 Windows",
		},
		{
			// Safari carries its real version in Version/, not Safari/.
			name: "safari on macos",
			ua:   "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.3 Safari/605.1.15",
			want: "Safari/17 macOS",
		},
		{
			name: "safari on ios",
			ua:   "Mozilla/5.0 (iPhone; CPU iPhone OS 17_3 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.3 Mobile/15E148 Safari/604.1",
			want: "Safari/17 iOS",
		},
		{
			// Android UAs contain "Linux" too; Android must win.
			name: "chrome on android",
			ua:   "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/121.0.0.0 Mobile Safari/537.36",
			want: "Chrome/121 Android",
		},
		{
			name: "firefox on linux",
			ua:   "Mozilla/5.0 (X11; Linux x86_64; rv:124.0) Gecko/20100101 Firefox/124.0",
			want: "Firefox/124 Linux",
		},
		{
			name: "empty stays empty",
			ua:   "",
			want: "",
		},
		{
			name: "unrecognized falls through to the raw string",
			ua:   "curl/8.5.0",
			want: "curl/8.5.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SummarizeUserAgent(tt.ua); got != tt.want {
				t.Errorf("SummarizeUserAgent(%q) = %q, want %q", tt.ua, got, tt.want)
			}
		})
	}
}

// A hostile or absurd header must not be able to bloat every join log line.
func TestSummarizeUserAgentBoundsUnrecognizedInput(t *testing.T) {
	long := make([]byte, 4096)
	for i := range long {
		long[i] = 'x'
	}
	got := SummarizeUserAgent(string(long))
	if len(got) > 60 {
		t.Errorf("unrecognized user agent not bounded: got %d chars, want <= 60", len(got))
	}
}
