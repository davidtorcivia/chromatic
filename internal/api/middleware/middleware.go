package middleware

import (
	"crypto/subtle"
	"log"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// RequireAuth returns middleware that validates the admin token
// Uses constant-time comparison to prevent timing attacks
func RequireAuth(adminToken string) func(http.Handler) http.Handler {
	adminTokenBytes := []byte(adminToken)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	AllowedOrigins []string // Empty means allow any (development mode)
}

// CORS adds CORS headers for cross-origin requests
func CORS(cfg CORSConfig) func(http.Handler) http.Handler {
	// Build a map for quick origin lookup
	allowedOriginMap := make(map[string]bool)
	for _, origin := range cfg.AllowedOrigins {
		allowedOriginMap[origin] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			if origin != "" {
				// Check if origin is allowed
				if len(cfg.AllowedOrigins) == 0 {
					// Development mode: allow any origin
					w.Header().Set("Access-Control-Allow-Origin", origin)
				} else if allowedOriginMap[origin] {
					// Production mode: only allow configured origins
					w.Header().Set("Access-Control-Allow-Origin", origin)
				}
				// If origin not allowed, don't set the header (browser will block)

				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}

			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Accept, Authorization, Content-Type, X-CSRF-Token")
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
}

// ipRateLimiter tracks rate limiting state per IP
type ipRateLimiter struct {
	lastRequest time.Time
	tokens      float64
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
			// Get client IP (handle X-Forwarded-For for proxies)
			ip := getClientIP(r)

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
func getClientIP(r *http.Request) string {
	// Check X-Forwarded-For header (for reverse proxies)
	forwarded := r.Header.Get("X-Forwarded-For")
	if forwarded != "" {
		// Take the first IP in the list
		ips := strings.Split(forwarded, ",")
		return strings.TrimSpace(ips[0])
	}

	// Check X-Real-IP header
	realIP := r.Header.Get("X-Real-IP")
	if realIP != "" {
		return realIP
	}

	// Fall back to RemoteAddr
	ip := r.RemoteAddr
	// Remove port if present
	if idx := strings.LastIndex(ip, ":"); idx != -1 {
		ip = ip[:idx]
	}
	return ip
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

		next.ServeHTTP(w, r)
	})
}
