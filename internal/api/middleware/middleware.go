package middleware

import (
	"crypto/subtle"
	"fmt"
	"log"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// SessionValidator is a function that validates session IDs from cookies
type SessionValidator func(sessionID string) bool

// AuthConfig holds authentication configuration
type AuthConfig struct {
	AdminToken      string
	SessionCookie   string           // Name of the session cookie
	ValidateSession SessionValidator // Function to validate session IDs
}

// RequireAuth returns middleware that validates authentication
// Checks both httpOnly session cookies and Bearer token headers
// Cookie-based auth is preferred as it's not vulnerable to XSS
func RequireAuth(cfg AuthConfig) func(http.Handler) http.Handler {
	adminTokenBytes := []byte(cfg.AdminToken)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// First, try to authenticate via session cookie (preferred, secure)
			if cfg.SessionCookie != "" && cfg.ValidateSession != nil {
				cookie, err := r.Cookie(cfg.SessionCookie)
				if err == nil && cookie.Value != "" {
					if cfg.ValidateSession(cookie.Value) {
						next.ServeHTTP(w, r)
						return
					}
				}
			}

			// Fall back to Bearer token authentication (for backwards compatibility)
			auth := r.Header.Get("Authorization")
			if auth == "" {
				http.Error(w, "Authorization required", http.StatusUnauthorized)
				return
			}

			// Extract bearer token
			token := strings.TrimPrefix(auth, "Bearer ")
			if token == auth {
				// No "Bearer " prefix found
				http.Error(w, "Invalid authorization format", http.StatusUnauthorized)
				return
			}

			// Use constant-time comparison to prevent timing attacks
			if subtle.ConstantTimeCompare([]byte(token), adminTokenBytes) != 1 {
				http.Error(w, "Invalid token", http.StatusUnauthorized)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// CORSConfig holds CORS configuration
type CORSConfig struct {
	AllowedOrigins []string // Origins to allow; empty in dev mode allows all
	ProductionMode bool     // When true, requires explicit origin configuration
}

// OriginValidator validates origins for CORS and WebSocket
type OriginValidator struct {
	allowedOriginMap map[string]bool
	productionMode   bool
}

// NewOriginValidator creates a new origin validator
func NewOriginValidator(allowedOrigins []string, productionMode bool) *OriginValidator {
	allowedOriginMap := make(map[string]bool)
	for _, origin := range allowedOrigins {
		allowedOriginMap[origin] = true
	}
	return &OriginValidator{
		allowedOriginMap: allowedOriginMap,
		productionMode:   productionMode,
	}
}

// IsAllowed checks if an origin is allowed
// Returns true if:
// - Origin matches configured allowed origins, OR
// - Not in production mode and no origins configured (dev mode)
func (v *OriginValidator) IsAllowed(origin string) bool {
	if origin == "" {
		// No origin header (same-origin request or non-browser client)
		return true
	}

	// Check if origin is in allowed list
	if v.allowedOriginMap[origin] {
		return true
	}

	// In development mode with no configured origins, allow all
	if !v.productionMode && len(v.allowedOriginMap) == 0 {
		return true
	}

	// Origin not allowed
	return false
}

// CORS adds CORS headers for cross-origin requests
func CORS(cfg CORSConfig) func(http.Handler) http.Handler {
	validator := NewOriginValidator(cfg.AllowedOrigins, cfg.ProductionMode)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			if origin != "" {
				// Check if origin is allowed
				if validator.IsAllowed(origin) {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Set("Access-Control-Allow-Credentials", "true")
				}
				// If origin not allowed, don't set the header (browser will block)
			}

			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Accept, Authorization, Content-Type, X-CSRF-Token, X-Join-Token")
			w.Header().Set("Access-Control-Max-Age", "86400")

			// Handle preflight
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// RateLimiterConfig holds rate limiter configuration
type RateLimiterConfig struct {
	RequestsPerSecond int           // Max requests per second per IP
	BurstSize         int           // Max burst size
	CleanupInterval   time.Duration // How often to clean up old entries
	TrustedProxies    []string      // IP addresses of trusted reverse proxies
}

// ipRateLimiter tracks rate limiting state per IP
type ipRateLimiter struct {
	lastRequest time.Time
	tokens      float64
}

type trustedProxySet struct {
	exact map[string]bool
	cidrs []*net.IPNet
}

func buildTrustedProxySet(trusted []string) trustedProxySet {
	set := trustedProxySet{
		exact: make(map[string]bool),
	}
	for _, proxy := range trusted {
		proxy = strings.TrimSpace(proxy)
		if proxy == "" {
			continue
		}
		if strings.Contains(proxy, "/") {
			if _, cidr, err := net.ParseCIDR(proxy); err == nil {
				set.cidrs = append(set.cidrs, cidr)
				continue
			}
		}
		set.exact[proxy] = true
	}
	return set
}

func (t trustedProxySet) IsTrusted(ip string) bool {
	if t.exact[ip] {
		return true
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, cidr := range t.cidrs {
		if cidr.Contains(parsed) {
			return true
		}
	}
	return false
}

// RateLimiter middleware limits requests per IP
func RateLimiter(cfg RateLimiterConfig) func(http.Handler) http.Handler {
	if cfg.RequestsPerSecond <= 0 {
		cfg.RequestsPerSecond = 10
	}
	if cfg.BurstSize <= 0 {
		cfg.BurstSize = 20
	}
	if cfg.CleanupInterval <= 0 {
		cfg.CleanupInterval = 5 * time.Minute
	}

	trustedProxySet := buildTrustedProxySet(cfg.TrustedProxies)

	var mu sync.Mutex
	limiters := make(map[string]*ipRateLimiter)

	// Cleanup goroutine
	go func() {
		ticker := time.NewTicker(cfg.CleanupInterval)
		for range ticker.C {
			mu.Lock()
			now := time.Now()
			for ip, limiter := range limiters {
				// Remove entries that haven't been accessed in the cleanup interval
				if now.Sub(limiter.lastRequest) > cfg.CleanupInterval {
					delete(limiters, ip)
				}
			}
			mu.Unlock()
		}
	}()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Get client IP (only trust X-Forwarded-For from trusted proxies)
			ip := getClientIP(r, trustedProxySet)

			mu.Lock()
			limiter, exists := limiters[ip]
			if !exists {
				limiter = &ipRateLimiter{
					lastRequest: time.Now(),
					tokens:      float64(cfg.BurstSize),
				}
				limiters[ip] = limiter
			}

			// Token bucket algorithm
			now := time.Now()
			elapsed := now.Sub(limiter.lastRequest).Seconds()
			limiter.lastRequest = now

			// Add tokens based on time elapsed
			limiter.tokens += elapsed * float64(cfg.RequestsPerSecond)
			if limiter.tokens > float64(cfg.BurstSize) {
				limiter.tokens = float64(cfg.BurstSize)
			}

			// Check if we have tokens available
			if limiter.tokens < 1 {
				mu.Unlock()
				w.Header().Set("Retry-After", "1")
				http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
				return
			}

			// Consume a token
			limiter.tokens--
			mu.Unlock()

			next.ServeHTTP(w, r)
		})
	}
}

// getClientIP extracts the client IP from the request
// Only trusts X-Forwarded-For and X-Real-IP headers if the direct connection
// comes from a trusted proxy. This prevents IP spoofing attacks.
func getClientIP(r *http.Request, trustedProxies trustedProxySet) string {
	directIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		directIP = r.RemoteAddr
	}

	// If no trusted proxies configured, always use direct IP (secure default)
	if len(trustedProxies.exact) == 0 && len(trustedProxies.cidrs) == 0 {
		return directIP
	}

	// Only trust forwarded headers if request came from a trusted proxy
	if !trustedProxies.IsTrusted(directIP) {
		return directIP
	}

	// Request came from trusted proxy - trust the forwarded headers
	// Check X-Forwarded-For header first
	forwarded := r.Header.Get("X-Forwarded-For")
	if forwarded != "" {
		// Take the first IP in the list (original client IP)
		ips := strings.Split(forwarded, ",")
		clientIP := strings.TrimSpace(ips[0])
		if clientIP != "" {
			return clientIP
		}
	}

	// Check X-Real-IP header
	realIP := r.Header.Get("X-Real-IP")
	if realIP != "" {
		return strings.TrimSpace(realIP)
	}

	// Fall back to direct IP
	return directIP
}

// RequestLogger logs incoming requests
func RequestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Wrap response writer to capture status code
		wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(wrapped, r)

		duration := time.Since(start)

		// Skip logging for static files and health checks in production
		path := r.URL.Path
		if !strings.HasPrefix(path, "/api") && !strings.HasPrefix(path, "/ws") && !strings.HasPrefix(path, "/whip") {
			return
		}

		log.Printf("%s %s %d %v", r.Method, path, wrapped.statusCode, duration)
	})
}

// responseWriter wraps http.ResponseWriter to capture status code
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// Recoverer recovers from panics and returns a 500 error
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				log.Printf("Panic recovered: %v\n%s", err, debug.Stack())
				http.Error(w, "Internal server error", http.StatusInternalServerError)
			}
		}()

		next.ServeHTTP(w, r)
	})
}

// EndpointRateLimiterConfig holds configuration for endpoint-specific rate limiting
type EndpointRateLimiterConfig struct {
	RequestsPerWindow int                          // Max requests per window
	WindowDuration    time.Duration                // Time window (e.g., 1 minute, 1 hour)
	TrustedProxies    []string                     // IP addresses of trusted reverse proxies
	KeyExtractor      func(r *http.Request) string // Custom key extraction (e.g., include room slug)
}

// endpointRateLimiter tracks rate limiting state per key
type endpointRateLimiter struct {
	requests    int
	windowStart time.Time
}

// EndpointRateLimiter creates a rate limiter for specific endpoints with time-window based limits
func EndpointRateLimiter(cfg EndpointRateLimiterConfig) func(http.Handler) http.Handler {
	if cfg.RequestsPerWindow <= 0 {
		cfg.RequestsPerWindow = 10
	}
	if cfg.WindowDuration <= 0 {
		cfg.WindowDuration = time.Minute
	}

	trustedProxySet := buildTrustedProxySet(cfg.TrustedProxies)

	var mu sync.Mutex
	limiters := make(map[string]*endpointRateLimiter)

	// Cleanup goroutine - run every window duration
	go func() {
		ticker := time.NewTicker(cfg.WindowDuration)
		for range ticker.C {
			mu.Lock()
			now := time.Now()
			for key, limiter := range limiters {
				// Remove entries older than 2 windows
				if now.Sub(limiter.windowStart) > cfg.WindowDuration*2 {
					delete(limiters, key)
				}
			}
			mu.Unlock()
		}
	}()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Get the rate limit key
			var key string
			if cfg.KeyExtractor != nil {
				key = cfg.KeyExtractor(r)
			} else {
				key = getClientIP(r, trustedProxySet)
			}

			mu.Lock()
			now := time.Now()
			limiter, exists := limiters[key]

			if !exists || now.Sub(limiter.windowStart) > cfg.WindowDuration {
				// New window
				limiter = &endpointRateLimiter{
					requests:    1,
					windowStart: now,
				}
				limiters[key] = limiter
				mu.Unlock()
				next.ServeHTTP(w, r)
				return
			}

			// Within existing window
			if limiter.requests >= cfg.RequestsPerWindow {
				mu.Unlock()
				// Calculate seconds until window resets
				retryAfter := int(cfg.WindowDuration.Seconds() - now.Sub(limiter.windowStart).Seconds())
				if retryAfter < 1 {
					retryAfter = 1
				}
				w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
				http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
				return
			}

			limiter.requests++
			mu.Unlock()
			next.ServeHTTP(w, r)
		})
	}
}

// RoomJoinRateLimiter creates a rate limiter for room join attempts (password protection)
// Keys by IP + room slug to limit password guessing per room
func RoomJoinRateLimiter(trustedProxies []string) func(http.Handler) http.Handler {
	trustedProxySet := buildTrustedProxySet(trustedProxies)

	return EndpointRateLimiter(EndpointRateLimiterConfig{
		RequestsPerWindow: 5,           // 5 attempts
		WindowDuration:    time.Minute, // per minute
		TrustedProxies:    trustedProxies,
		KeyExtractor: func(r *http.Request) string {
			ip := getClientIP(r, trustedProxySet)
			slug := r.PathValue("slug")
			return fmt.Sprintf("%s:%s", ip, slug)
		},
	})
}

// RoomCreationRateLimiter creates a rate limiter for room creation (admin protection)
// 10 rooms per hour per IP
func RoomCreationRateLimiter(trustedProxies []string) func(http.Handler) http.Handler {
	return EndpointRateLimiter(EndpointRateLimiterConfig{
		RequestsPerWindow: 10,        // 10 rooms
		WindowDuration:    time.Hour, // per hour
		TrustedProxies:    trustedProxies,
	})
}

// FileUploadRateLimiter creates a rate limiter for file uploads
// 10 uploads per minute per IP
func FileUploadRateLimiter(trustedProxies []string) func(http.Handler) http.Handler {
	return EndpointRateLimiter(EndpointRateLimiterConfig{
		RequestsPerWindow: 10,          // 10 uploads
		WindowDuration:    time.Minute, // per minute
		TrustedProxies:    trustedProxies,
	})
}

// SecurityHeaders adds security-related HTTP headers
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Prevent clickjacking
		w.Header().Set("X-Frame-Options", "DENY")

		// Prevent MIME type sniffing
		w.Header().Set("X-Content-Type-Options", "nosniff")

		// XSS protection (legacy but still useful)
		w.Header().Set("X-XSS-Protection", "1; mode=block")

		// Referrer policy
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")

		// Content Security Policy
		// Note: WebRTC requires 'connect-src' for ws:// and turn:// protocols
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self' 'unsafe-inline'; "+
				"style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data: blob:; "+
				"media-src 'self' blob:; "+
				"connect-src 'self' ws: wss:; "+
				"frame-ancestors 'none';")

		// Permissions Policy (limit browser features)
		w.Header().Set("Permissions-Policy", "geolocation=(), payment=(), usb=()")

		next.ServeHTTP(w, r)
	})
}
