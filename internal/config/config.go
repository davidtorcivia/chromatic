package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

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

	// Authentication
	AdminToken string

	// Security
	AllowedOrigins []string // Required in production mode
	TrustedProxies []string // IP addresses/CIDRs of trusted reverse proxies
	ProductionMode bool     // When true, enforces strict security (requires ALLOWED_ORIGINS)

	// TURN
	TurnSecret       string
	TurnRealm        string
	TurnExternalURL  string
	TurnExternalUser string
	TurnExternalPass string

	// Timeouts
	OBSReconnectTimeout time.Duration
	ClientPingInterval  time.Duration
}

// Load loads configuration from environment variables
func Load() (*Config, error) {
	cfg := &Config{
		Port:                getEnvInt("PORT", 3000),
		PublicURL:           getEnvRequired("PUBLIC_URL"),
		DatabasePath:        getEnv("DATABASE_PATH", "/data/chromatic.db"),
		UploadPath:          getEnv("UPLOAD_PATH", "/data/files"),
		LogoPath:            getEnv("LOGO_PATH", "/data/logos"),
		AdminToken:          getEnvRequired("ADMIN_TOKEN"),
		AllowedOrigins:      getEnvList("ALLOWED_ORIGINS", nil),
		TrustedProxies:      getEnvList("TRUSTED_PROXIES", nil),
		ProductionMode:      getEnvBool("PRODUCTION_MODE", false),
		TurnSecret:          getEnvRequired("TURN_SECRET"),
		TurnRealm:           getEnvRequired("TURN_REALM"),
		TurnExternalURL:     getEnv("TURN_EXTERNAL_URL", ""),
		TurnExternalUser:    getEnv("TURN_EXTERNAL_USER", ""),
		TurnExternalPass:    getEnv("TURN_EXTERNAL_PASS", ""),
		OBSReconnectTimeout: getEnvDuration("OBS_RECONNECT_TIMEOUT", 5*time.Minute),
		ClientPingInterval:  getEnvDuration("CLIENT_PING_INTERVAL", 60*time.Second),
	}

	// Validate required fields
	if cfg.PublicURL == "" {
		return nil, fmt.Errorf("PUBLIC_URL is required")
	}
	if cfg.AdminToken == "" {
		return nil, fmt.Errorf("ADMIN_TOKEN is required")
	}
	if cfg.TurnSecret == "" {
		return nil, fmt.Errorf("TURN_SECRET is required")
	}
	if cfg.TurnRealm == "" {
		return nil, fmt.Errorf("TURN_REALM is required")
	}

	// In production mode, ALLOWED_ORIGINS is required
	if cfg.ProductionMode && len(cfg.AllowedOrigins) == 0 {
		return nil, fmt.Errorf("ALLOWED_ORIGINS is required in production mode")
	}

	return cfg, nil
}

// ListenAddr returns the address to listen on
func (c *Config) ListenAddr() string {
	return fmt.Sprintf(":%d", c.Port)
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

// getEnvDuration gets a duration environment variable with a default value
func getEnvDuration(key string, defaultValue time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if d, err := time.ParseDuration(value); err == nil {
			return d
		}
	}
	return defaultValue
}

// getEnvBool gets a boolean environment variable with a default value
func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		// Accept "true", "1", "yes" as true
		return value == "true" || value == "1" || value == "yes"
	}
	return defaultValue
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
	for _, part := range splitString(s, sep) {
		trimmed := trimSpace(part)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// splitString splits a string by separator (simple implementation to avoid strings import)
func splitString(s, sep string) []string {
	if len(sep) == 0 {
		return []string{s}
	}
	var result []string
	for {
		i := indexOf(s, sep)
		if i < 0 {
			result = append(result, s)
			break
		}
		result = append(result, s[:i])
		s = s[i+len(sep):]
	}
	return result
}

// indexOf returns the index of sep in s, or -1 if not found
func indexOf(s, sep string) int {
	for i := 0; i <= len(s)-len(sep); i++ {
		if s[i:i+len(sep)] == sep {
			return i
		}
	}
	return -1
}

// trimSpace removes leading and trailing whitespace
func trimSpace(s string) string {
	start := 0
	for start < len(s) && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}
