// Package middleware provides the HTTP middleware chain: admin auth, rate
// limiting, CORS, security headers, and admin audit logging.
package middleware

import (
	"bufio"
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
			// An empty configured admin token must fail every request:
			// constant-time comparison treats two empty strings as equal, so
			// a "Bearer " header with nothing after it would otherwise
			// authenticate. Config rejects an empty ADMIN_TOKEN, but this is
			// a trust boundary and does not depend on that.
			if len(adminTokenBytes) == 0 {
				http.Error(w, "Server misconfigured", http.StatusInternalServerError)
				return
			}

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
			if token == auth || token == "" {
				// No "Bearer " prefix, or nothing after it
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

// EnforceTrustedOrigins rejects unsafe browser requests from origins that are
// not explicitly allowed. CORS controls whether a browser can read a response;
// this middleware prevents the state change itself, which matters for
// cookie-authenticated admin routes. Requests without an Origin header are
// allowed so CLI clients, Prometheus, OBS, and same-origin legacy user agents
// keep working.
func EnforceTrustedOrigins(cfg CORSConfig) func(http.Handler) http.Handler {
	validator := NewOriginValidator(cfg.AllowedOrigins, cfg.ProductionMode)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}

			origin := r.Header.Get("Origin")
			if origin != "" && !validator.IsAllowed(origin) {
				http.Error(w, "Origin not allowed", http.StatusForbidden)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// LimitJSONBody caps JSON request bodies before handlers decode them. File and
// logo uploads keep their multipart-specific limits in the upload handlers.
func LimitJSONBody(maxBytes int64) func(http.Handler) http.Handler {
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body == nil {
				next.ServeHTTP(w, r)
				return
			}

			contentType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
			isJSON := contentType == "application/json" || strings.HasSuffix(contentType, "+json")
			if !isJSON {
				next.ServeHTTP(w, r)
				return
			}

			if r.ContentLength > maxBytes {
				http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
				return
			}

			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
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
//
// X-Forwarded-For is a comma-separated chain where each trusted proxy appends
// the address it received the request from to the RIGHT. The leftmost entry is
// therefore fully client-controlled and trivially spoofable — trusting it lets
// an attacker rotate the leftmost value to defeat every IP-based rate limiter
// (login brute force, room join, file upload). Instead we walk the chain
// right-to-left and return the first address that is NOT a trusted proxy, i.e.
// the origin the last trusted proxy actually saw.
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

	// Request came from trusted proxy - trust the forwarded headers.
	// Check X-Forwarded-For header first.
	forwarded := r.Header.Get("X-Forwarded-For")
	if forwarded != "" {
		// Walk right-to-left: the last proxy (the one we trust) appended the
		// immediate client to the right, so the rightmost untrusted hop is the
		// real client. Skip any further trusted proxies (multi-hop through our
		// own infra). Taking the leftmost entry instead would let a client
		// forge it and bypass every IP rate limit.
		ips := strings.Split(forwarded, ",")
		for i := len(ips) - 1; i >= 0; i-- {
			clientIP := strings.TrimSpace(ips[i])
			if clientIP == "" {
				continue
			}
			if !trustedProxies.IsTrusted(clientIP) {
				return clientIP
			}
		}
		// Every hop was trusted (e.g. health check from internal infra). Fall
		// through to X-Real-IP / direct IP rather than trusting the spoofable
		// leftmost entry.
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

// Hijack implements http.Hijacker so WebSocket upgrades work through the logging middleware.
func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hijacker, ok := rw.ResponseWriter.(http.Hijacker); ok {
		return hijacker.Hijack()
	}
	return nil, nil, fmt.Errorf("upstream ResponseWriter does not implement http.Hijacker")
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

// LoginRateLimiter creates a rate limiter for admin login attempts
// (brute-force protection): 5 attempts per minute per IP
func LoginRateLimiter(trustedProxies []string) func(http.Handler) http.Handler {
	return EndpointRateLimiter(EndpointRateLimiterConfig{
		RequestsPerWindow: 5,           // 5 attempts
		WindowDuration:    time.Minute, // per minute
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

// WHIPRateLimiter rate-limits the WHIP ingest endpoint per IP. The WHIP route
// authenticates with a raw stream key in the URL and each POST creates a
// PeerConnection + hits the DB (validateKey), so it is both a stream-key
// brute-force vector and a cheap CPU/DB amplification attack. A legitimate OBS
// instance makes very few requests per session (one POST, a handful of trickle
// PATCHes, one DELETE); 20/minute per IP is generous for real publishers and
// throttles guessing.
func WHIPRateLimiter(trustedProxies []string) func(http.Handler) http.Handler {
	return EndpointRateLimiter(EndpointRateLimiterConfig{
		RequestsPerWindow: 20,
		WindowDuration:    time.Minute,
		TrustedProxies:    trustedProxies,
	})
}

// SecurityHeaders adds security-related HTTP headers.
// In production mode, connect-src is restricted to same-origin only.
func SecurityHeaders(productionMode bool) func(http.Handler) http.Handler {
	// Build the CSP once.
	// Notes:
	// - In production, connect-src 'self' covers same-origin WebSocket in
	//   all modern browsers; ws:/wss: wildcards would allow exfiltration to
	//   any WebSocket origin. WebRTC (turn:) is not governed by connect-src.
	//   In development we additionally allow ws:/wss: so the Vite dev server
	//   and cross-port local WebSocket connections keep working.
	// - script-src keeps 'unsafe-inline' because SvelteKit's hydration
	//   inline script requires it; removing it would break the app.
	// - Fonts are self-hosted, so no CDN origins are allowlisted.
	connectSrc := "'self'"
	if !productionMode {
		connectSrc = "'self' ws: wss:"
	}
	csp := "default-src 'self'; " +
		"script-src 'self' 'unsafe-inline'; " +
		"style-src 'self' 'unsafe-inline'; " +
		"font-src 'self'; " +
		"img-src 'self' data: blob:; " +
		"media-src 'self' blob:; " +
		"connect-src " + connectSrc + "; " +
		"frame-ancestors 'none';"

	return func(next http.Handler) http.Handler {
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
			w.Header().Set("Content-Security-Policy", csp)
			// Permissions Policy (limit browser features)
			w.Header().Set("Permissions-Policy", "geolocation=(), payment=(), usb=()")
			// HTTP Strict Transport Security: once a browser has seen this over
			// HTTPS it refuses to talk to the host over plaintext, defeating
			// SSL-strip / downgrade MITM against the login token, room passwords
			// and session cookie. Production only — over plain HTTP browsers
			// ignore it, and asserting it in dev (localhost, self-signed) would
			// pin developers to HTTPS. Host-scoped: no includeSubDomains/preload,
			// so deploying Chromatic at review.example.com never forces sibling
			// subdomains (a client's HTTP blog, staging box) onto HTTPS. Operators
			// on a dedicated apex can harden further at their edge/proxy.
			if productionMode {
				w.Header().Set("Strict-Transport-Security", "max-age=31536000")
			}
			next.ServeHTTP(w, r)
		})
	}
}
