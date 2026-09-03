package config

import (
	"os"
	"testing"
)

// TestLoad tests configuration loading from environment
func TestLoad(t *testing.T) {
	// Save original env and restore after tests
	originalEnv := map[string]string{
		"PUBLIC_URL":                os.Getenv("PUBLIC_URL"),
		"ADMIN_TOKEN":               os.Getenv("ADMIN_TOKEN"),
		"TURN_SECRET":               os.Getenv("TURN_SECRET"),
		"TURN_REALM":                os.Getenv("TURN_REALM"),
		"TURN_MODE":                 os.Getenv("TURN_MODE"),
		"TURN_EXTERNAL_URL":         os.Getenv("TURN_EXTERNAL_URL"),
		"TURN_EXTERNAL_URLS":        os.Getenv("TURN_EXTERNAL_URLS"),
		"TURN_EXTERNAL_USER":        os.Getenv("TURN_EXTERNAL_USER"),
		"TURN_EXTERNAL_PASS":        os.Getenv("TURN_EXTERNAL_PASS"),
		"TURN_CLOUDFLARE_KEY_ID":    os.Getenv("TURN_CLOUDFLARE_KEY_ID"),
		"TURN_CLOUDFLARE_API_TOKEN": os.Getenv("TURN_CLOUDFLARE_API_TOKEN"),
		"PORT":                      os.Getenv("PORT"),
		"DATABASE_PATH":             os.Getenv("DATABASE_PATH"),
	}
	defer func() {
		for k, v := range originalEnv {
			if v == "" {
				os.Unsetenv(k)
			} else {
				os.Setenv(k, v)
			}
		}
	}()

	t.Run("valid configuration", func(t *testing.T) {
		os.Setenv("PUBLIC_URL", "https://example.com")
		os.Setenv("ADMIN_TOKEN", "test-token")
		os.Setenv("TURN_SECRET", "turn-secret")
		os.Setenv("TURN_REALM", "turn.example.com")
		os.Setenv("PORT", "8080")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if cfg.PublicURL != "https://example.com" {
			t.Errorf("expected PublicURL 'https://example.com', got %q", cfg.PublicURL)
		}
		if cfg.AdminToken != "test-token" {
			t.Errorf("expected AdminToken 'test-token', got %q", cfg.AdminToken)
		}
		if cfg.Port != 8080 {
			t.Errorf("expected Port 8080, got %d", cfg.Port)
		}
	})

	t.Run("missing PUBLIC_URL", func(t *testing.T) {
		os.Unsetenv("PUBLIC_URL")
		os.Setenv("ADMIN_TOKEN", "test-token")
		os.Setenv("TURN_SECRET", "turn-secret")
		os.Setenv("TURN_REALM", "turn.example.com")

		_, err := Load()
		if err == nil {
			t.Error("expected error for missing PUBLIC_URL")
		}
	})

	t.Run("missing ADMIN_TOKEN", func(t *testing.T) {
		os.Setenv("PUBLIC_URL", "https://example.com")
		os.Unsetenv("ADMIN_TOKEN")
		os.Setenv("TURN_SECRET", "turn-secret")
		os.Setenv("TURN_REALM", "turn.example.com")

		_, err := Load()
		if err == nil {
			t.Error("expected error for missing ADMIN_TOKEN")
		}
	})

	t.Run("default values", func(t *testing.T) {
		os.Setenv("PUBLIC_URL", "https://example.com")
		os.Setenv("ADMIN_TOKEN", "test-token")
		os.Setenv("TURN_SECRET", "turn-secret")
		os.Setenv("TURN_REALM", "turn.example.com")
		os.Unsetenv("PORT")
		os.Unsetenv("DATABASE_PATH")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if cfg.Port != 3000 {
			t.Errorf("expected default Port 3000, got %d", cfg.Port)
		}
		if cfg.DatabasePath != "/data/chromatic.db" {
			t.Errorf("expected default DatabasePath, got %q", cfg.DatabasePath)
		}
	})

	t.Run("external mode with cloudflare does not require self-hosted TURN vars", func(t *testing.T) {
		os.Setenv("PUBLIC_URL", "https://example.com")
		os.Setenv("ADMIN_TOKEN", "test-token")
		os.Setenv("TURN_MODE", "external")
		os.Unsetenv("TURN_SECRET")
		os.Unsetenv("TURN_REALM")
		os.Unsetenv("TURN_EXTERNAL_URL")
		os.Unsetenv("TURN_EXTERNAL_URLS")
		os.Unsetenv("TURN_EXTERNAL_USER")
		os.Unsetenv("TURN_EXTERNAL_PASS")
		os.Setenv("TURN_CLOUDFLARE_KEY_ID", "cf-key-id")
		os.Setenv("TURN_CLOUDFLARE_API_TOKEN", "cf-api-token")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if cfg.TurnMode != TurnModeExternal {
			t.Fatalf("expected %q mode, got %q", TurnModeExternal, cfg.TurnMode)
		}
		if len(cfg.TurnExternalURLs) == 0 {
			t.Fatal("expected default Cloudflare TURN URL list")
		}
	})

	t.Run("external mode requires external credentials or cloudflare", func(t *testing.T) {
		os.Setenv("PUBLIC_URL", "https://example.com")
		os.Setenv("ADMIN_TOKEN", "test-token")
		os.Setenv("TURN_MODE", "external")
		os.Unsetenv("TURN_SECRET")
		os.Unsetenv("TURN_REALM")
		os.Unsetenv("TURN_EXTERNAL_URL")
		os.Unsetenv("TURN_EXTERNAL_URLS")
		os.Unsetenv("TURN_EXTERNAL_USER")
		os.Unsetenv("TURN_EXTERNAL_PASS")
		os.Unsetenv("TURN_CLOUDFLARE_KEY_ID")
		os.Unsetenv("TURN_CLOUDFLARE_API_TOKEN")

		_, err := Load()
		if err == nil {
			t.Fatal("expected validation error in external mode without TURN provider")
		}
	})

	t.Run("invalid turn mode", func(t *testing.T) {
		os.Setenv("PUBLIC_URL", "https://example.com")
		os.Setenv("ADMIN_TOKEN", "test-token")
		os.Setenv("TURN_MODE", "unsupported")
		os.Setenv("TURN_SECRET", "turn-secret")
		os.Setenv("TURN_REALM", "turn.example.com")

		_, err := Load()
		if err == nil {
			t.Fatal("expected error for invalid TURN_MODE")
		}
	})
}

// TestGetEnv tests the getEnv helper
func TestGetEnv(t *testing.T) {
	os.Setenv("TEST_VAR", "test-value")
	defer os.Unsetenv("TEST_VAR")

	tests := []struct {
		name     string
		key      string
		default_ string
		expected string
	}{
		{
			name:     "existing var",
			key:      "TEST_VAR",
			default_: "default",
			expected: "test-value",
		},
		{
			name:     "non-existing var",
			key:      "NON_EXISTING",
			default_: "default",
			expected: "default",
		},
		{
			name:     "empty default",
			key:      "NON_EXISTING",
			default_: "",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getEnv(tt.key, tt.default_)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

// TestGetEnvInt tests the getEnvInt helper
func TestGetEnvInt(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		default_ int
		expected int
	}{
		{
			name:     "valid int",
			envValue: "8080",
			default_: 3000,
			expected: 8080,
		},
		{
			name:     "invalid int",
			envValue: "not-a-number",
			default_: 3000,
			expected: 3000,
		},
		{
			name:     "empty value",
			envValue: "",
			default_: 3000,
			expected: 3000,
		},
		{
			name:     "negative int",
			envValue: "-1",
			default_: 3000,
			expected: -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Setenv("TEST_INT", tt.envValue)
			defer os.Unsetenv("TEST_INT")

			if tt.envValue == "" {
				os.Unsetenv("TEST_INT")
			}

			result := getEnvInt("TEST_INT", tt.default_)
			if result != tt.expected {
				t.Errorf("expected %d, got %d", tt.expected, result)
			}
		})
	}
}

// TestGetEnvList tests the getEnvList helper
func TestGetEnvList(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		default_ []string
		expected []string
	}{
		{
			name:     "single value",
			envValue: "https://example.com",
			default_: nil,
			expected: []string{"https://example.com"},
		},
		{
			name:     "multiple values",
			envValue: "https://example.com,https://app.example.com",
			default_: nil,
			expected: []string{"https://example.com", "https://app.example.com"},
		},
		{
			name:     "values with spaces",
			envValue: "https://example.com, https://app.example.com , https://other.com",
			default_: nil,
			expected: []string{"https://example.com", "https://app.example.com", "https://other.com"},
		},
		{
			name:     "empty value",
			envValue: "",
			default_: []string{"default"},
			expected: []string{"default"},
		},
		{
			name:     "nil default",
			envValue: "",
			default_: nil,
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Setenv("TEST_LIST", tt.envValue)
			defer os.Unsetenv("TEST_LIST")

			if tt.envValue == "" {
				os.Unsetenv("TEST_LIST")
			}

			result := getEnvList("TEST_LIST", tt.default_)

			if len(result) != len(tt.expected) {
				t.Errorf("expected length %d, got %d", len(tt.expected), len(result))
				return
			}

			for i := range result {
				if result[i] != tt.expected[i] {
					t.Errorf("at index %d: expected %q, got %q", i, tt.expected[i], result[i])
				}
			}
		})
	}
}

// TestListenAddr tests the ListenAddr method
func TestListenAddr(t *testing.T) {
	cfg := &Config{Port: 8080}
	if cfg.ListenAddr() != ":8080" {
		t.Errorf("expected ':8080', got %q", cfg.ListenAddr())
	}

	cfg.Port = 3000
	if cfg.ListenAddr() != ":3000" {
		t.Errorf("expected ':3000', got %q", cfg.ListenAddr())
	}
}

func TestGetEnvBool_CaseInsensitive(t *testing.T) {
	cases := map[string]bool{
		"true": true, "True": true, "TRUE": true, "1": true, "yes": true, "Yes": true, " on ": true,
		"false": false, "False": false, "0": false, "no": false, "NO": false, "off": false,
	}
	for raw, want := range cases {
		t.Setenv("TEST_BOOL", raw)
		if got := getEnvBool("TEST_BOOL", !want); got != want {
			t.Errorf("getEnvBool(%q) = %v, want %v", raw, got, want)
		}
	}
	t.Setenv("TEST_BOOL", "maybe")
	if got := getEnvBool("TEST_BOOL", true); !got {
		t.Error("unrecognized value should fall back to the default")
	}
}

func TestLoad_RejectsMalformedProductionMode(t *testing.T) {
	t.Setenv("PUBLIC_URL", "https://example.com")
	t.Setenv("ADMIN_TOKEN", "x")
	t.Setenv("TURN_MODE", "external")
	t.Setenv("TURN_EXTERNAL_URLS", "turn:turn.example.com:3478")
	t.Setenv("TURN_EXTERNAL_USER", "u")
	t.Setenv("TURN_EXTERNAL_PASS", "p")
	t.Setenv("ALLOWED_ORIGINS", "https://example.com")

	// A typo must fail startup rather than silently running in dev mode
	// (query-string join tokens accepted, Secure cookie flag off).
	t.Setenv("PRODUCTION_MODE", "ture")
	if _, err := Load(); err == nil {
		t.Fatal("expected Load to reject PRODUCTION_MODE=ture")
	}

	t.Setenv("PRODUCTION_MODE", "True")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with PRODUCTION_MODE=True: %v", err)
	}
	if !cfg.ProductionMode {
		t.Fatal("PRODUCTION_MODE=True must enable production mode")
	}
}
