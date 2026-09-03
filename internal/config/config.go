// Package config loads and validates Chromatic's environment configuration.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	TurnModeSelfHosted = "self-hosted"
	TurnModeExternal   = "external"
	TurnModeHybrid     = "hybrid"
)

var defaultCloudflareTurnURLs = []string{
	"turn:turn.cloudflare.com:3478?transport=udp",
	"turn:turn.cloudflare.com:3478?transport=tcp",
	"turns:turn.cloudflare.com:443?transport=tcp",
}

// Config holds all application configuration
type Config struct {
	// Server
	Port      int
	PublicURL string

	// Database
	DatabasePath string

	// Storage
	UploadPath string
	LogoPath   string
	// LogPath, when set, mirrors every log line to a rotating file. Point it at
	// a persistent volume: container stdout dies with the container, so without
	// this a redeploy erases the history any incident investigation depends on.
	// Empty disables file logging (stdout only).
	LogPath string

	// Authentication
	AdminToken string

	// Security
	AllowedOrigins []string // Required in production mode
	TrustedProxies []string // IP addresses/CIDRs of trusted reverse proxies
	ProductionMode bool     // When true, enforces strict security (requires ALLOWED_ORIGINS)

	// Limits
	MaxParticipantsPerRoom int // Maximum non-ended participants per room

	// TURN
	TurnMode                     string
	TurnSecret                   string
	TurnRealm                    string
	TurnExternalURL              string // Legacy single-URL field (backwards compatibility)
	TurnExternalURLs             []string
	TurnExternalUser             string
	TurnExternalPass             string
	TurnCloudflareKeyID          string
	TurnCloudflareAPIToken       string
	TurnCloudflareCredentialTTL  int
	TurnCloudflareCredentialSkew int

	// ICELoopbackOnly restricts ICE gathering to loopback addresses and
	// disables mDNS. Test-only (set by test helpers, never loaded from the
	// environment): it keeps the ad-hoc `go test` binaries from listening on
	// real interfaces, which would otherwise trigger an OS firewall prompt
	// for every freshly built test executable on Windows/macOS.
	ICELoopbackOnly bool
}

// Load loads configuration from environment variables
func Load() (*Config, error) {
	cfg := &Config{
		Port:                   getEnvInt("PORT", 3000),
		PublicURL:              getEnvRequired("PUBLIC_URL"),
		DatabasePath:           getEnv("DATABASE_PATH", "/data/chromatic.db"),
		UploadPath:             getEnv("UPLOAD_PATH", "/data/files"),
		LogoPath:               getEnv("LOGO_PATH", "/data/logos"),
		LogPath:                getEnv("LOG_PATH", "/data/logs/chromatic.log"),
		AdminToken:             getEnvRequired("ADMIN_TOKEN"),
		AllowedOrigins:         getEnvList("ALLOWED_ORIGINS", nil),
		TrustedProxies:         getEnvList("TRUSTED_PROXIES", nil),
		ProductionMode:         getEnvBool("PRODUCTION_MODE", false),
		MaxParticipantsPerRoom: getEnvInt("MAX_PARTICIPANTS_PER_ROOM", 20),
		TurnMode:               normalizeTurnMode(getEnv("TURN_MODE", TurnModeHybrid)),
		TurnSecret:             getEnvRequired("TURN_SECRET"),
		TurnRealm:              getEnvRequired("TURN_REALM"),
		TurnExternalURL:        getEnv("TURN_EXTERNAL_URL", ""),
		TurnExternalURLs:       getEnvList("TURN_EXTERNAL_URLS", nil),
		TurnExternalUser:       getEnv("TURN_EXTERNAL_USER", ""),
		TurnExternalPass:       getEnv("TURN_EXTERNAL_PASS", ""),
		TurnCloudflareKeyID:    getEnv("TURN_CLOUDFLARE_KEY_ID", ""),
		TurnCloudflareAPIToken: getEnv("TURN_CLOUDFLARE_API_TOKEN",
			"",
		),
		TurnCloudflareCredentialTTL:  getEnvInt("TURN_CLOUDFLARE_CREDENTIAL_TTL", 3600),
		TurnCloudflareCredentialSkew: getEnvInt("TURN_CLOUDFLARE_CREDENTIAL_SKEW", 60),
	}

	// Support legacy TURN_EXTERNAL_URL when TURN_EXTERNAL_URLS is not set.
	if len(cfg.TurnExternalURLs) == 0 && cfg.TurnExternalURL != "" {
		cfg.TurnExternalURLs = []string{cfg.TurnExternalURL}
	}

	// If Cloudflare is configured and external URLs are omitted, use sane defaults.
	if cfg.HasCloudflareTURN() && len(cfg.TurnExternalURLs) == 0 {
		cfg.TurnExternalURLs = append([]string(nil), defaultCloudflareTurnURLs...)
	}

	// Keep legacy field populated from the first URL for compatibility with older code paths.
	if cfg.TurnExternalURL == "" && len(cfg.TurnExternalURLs) > 0 {
		cfg.TurnExternalURL = cfg.TurnExternalURLs[0]
	}

	// Validate required fields
	if v := os.Getenv("PRODUCTION_MODE"); v != "" {
		if _, ok := parseBool(v); !ok {
			return nil, fmt.Errorf("PRODUCTION_MODE must be true/false (got %q)", v)
		}
	}
	if cfg.PublicURL == "" {
		return nil, fmt.Errorf("PUBLIC_URL is required")
	}
	if cfg.AdminToken == "" {
		return nil, fmt.Errorf("ADMIN_TOKEN is required")
	}

	// In production mode, ALLOWED_ORIGINS is required
	if cfg.ProductionMode && len(cfg.AllowedOrigins) == 0 {
		return nil, fmt.Errorf("ALLOWED_ORIGINS is required in production mode")
	}

	if cfg.TurnMode != TurnModeSelfHosted && cfg.TurnMode != TurnModeExternal && cfg.TurnMode != TurnModeHybrid {
		return nil, fmt.Errorf("TURN_MODE must be one of: %s, %s, %s", TurnModeSelfHosted, TurnModeExternal, TurnModeHybrid)
	}

	if cfg.TurnMode != TurnModeExternal {
		if cfg.TurnSecret == "" {
			return nil, fmt.Errorf("TURN_SECRET is required unless TURN_MODE=external")
		}
		if cfg.TurnRealm == "" {
			return nil, fmt.Errorf("TURN_REALM is required unless TURN_MODE=external")
		}
	}

	if cfg.TurnMode == TurnModeExternal {
		hasExternalStatic := len(cfg.TurnExternalURLs) > 0 && cfg.TurnExternalUser != "" && cfg.TurnExternalPass != ""
		if !hasExternalStatic && !cfg.HasCloudflareTURN() {
			return nil, fmt.Errorf("TURN_MODE=external requires either Cloudflare TURN credentials or TURN_EXTERNAL_URLS + TURN_EXTERNAL_USER/PASS")
		}
	}

	if cfg.MaxParticipantsPerRoom <= 0 {
		return nil, fmt.Errorf("MAX_PARTICIPANTS_PER_ROOM must be > 0")
	}

	if cfg.TurnCloudflareCredentialTTL < 60 {
		return nil, fmt.Errorf("TURN_CLOUDFLARE_CREDENTIAL_TTL must be >= 60 seconds")
	}
	if cfg.TurnCloudflareCredentialSkew < 0 {
		return nil, fmt.Errorf("TURN_CLOUDFLARE_CREDENTIAL_SKEW must be >= 0 seconds")
	}
	if cfg.TurnCloudflareCredentialSkew >= cfg.TurnCloudflareCredentialTTL {
		return nil, fmt.Errorf("TURN_CLOUDFLARE_CREDENTIAL_SKEW must be smaller than TURN_CLOUDFLARE_CREDENTIAL_TTL")
	}

	return cfg, nil
}

func (c *Config) HasCloudflareTURN() bool {
	return c.TurnCloudflareKeyID != "" && c.TurnCloudflareAPIToken != ""
}

// ListenAddr returns the address to listen on
func (c *Config) ListenAddr() string {
	return fmt.Sprintf(":%d", c.Port)
}

func normalizeTurnMode(mode string) string {
	normalized := strings.ToLower(strings.TrimSpace(mode))
	switch normalized {
	case "", TurnModeHybrid:
		return TurnModeHybrid
	case TurnModeSelfHosted, "selfhosted", "self_hosted":
		return TurnModeSelfHosted
	case TurnModeExternal:
		return TurnModeExternal
	default:
		return normalized
	}
}

// getEnv gets an environment variable with a default value
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// getEnvRequired gets a required environment variable (returns empty if not set, validation happens in Load)
func getEnvRequired(key string) string {
	return os.Getenv(key)
}

// getEnvInt gets an integer environment variable with a default value
func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if i, err := strconv.Atoi(value); err == nil {
			return i
		}
	}
	return defaultValue
}

// getEnvBool gets a boolean environment variable with a default value.
// Unrecognized values fall back to the default; Load rejects them for
// PRODUCTION_MODE, where a typo would silently downgrade security posture.
func getEnvBool(key string, defaultValue bool) bool {
	if b, ok := parseBool(os.Getenv(key)); ok {
		return b
	}
	return defaultValue
}

// parseBool accepts the common spellings case-insensitively ("True", "YES",
// "0") so hand-edited .env files and templating tools that emit capitalized
// booleans do not silently read as false.
func parseBool(value string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes", "on":
		return true, true
	case "false", "0", "no", "off":
		return false, true
	}
	return false, false
}

// getEnvList gets a comma-separated list environment variable
func getEnvList(key string, defaultValue []string) []string {
	if value := os.Getenv(key); value != "" {
		var result []string
		for _, item := range splitAndTrim(value, ",") {
			if item != "" {
				result = append(result, item)
			}
		}
		return result
	}
	return defaultValue
}

// splitAndTrim splits a string and trims whitespace from each element
func splitAndTrim(s, sep string) []string {
	var result []string
	for _, part := range strings.Split(s, sep) {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
