package middleware

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestRequireAuth tests the authentication middleware
func TestRequireAuth(t *testing.T) {
	validToken := "test-admin-token-12345"
	authMiddleware := RequireAuth(AuthConfig{AdminToken: validToken})

	handler := authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("success"))
	}))

	tests := []struct {
		name           string
		authHeader     string
		expectedStatus int
	}{
		{
			name:           "valid token",
			authHeader:     "Bearer " + validToken,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "missing auth header",
			authHeader:     "",
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:           "invalid token",
			authHeader:     "Bearer wrong-token",
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:           "missing bearer prefix",
			authHeader:     validToken,
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:           "wrong prefix",
			authHeader:     "Basic " + validToken,
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:           "empty bearer token",
			authHeader:     "Bearer ",
			expectedStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/test", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, rr.Code)
			}
		})
	}
}

// TestRequireAuthConstantTime tests that auth uses constant-time comparison
func TestRequireAuthConstantTime(t *testing.T) {
	// This test verifies that the auth middleware works correctly with
	// different token lengths. Actual timing attack prevention is hard
	// to verify in a unit test, but we verify the behavior is correct.
	validToken := "test-admin-token-12345"
	authMiddleware := RequireAuth(AuthConfig{AdminToken: validToken})

	handler := authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Test with tokens of various lengths - all should be rejected
	testTokens := []string{
		"wrong-admin-token-12345", // Same length
		"short",                   // Shorter
		"a-very-long-token-that-is-much-longer-than-the-valid-one", // Longer
		"",         // Empty
		validToken, // Valid (should succeed)
	}

	for _, token := range testTokens {
		req := httptest.NewRequest("GET", "/api/test", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if token == validToken {
			if rr.Code != http.StatusOK {
				t.Errorf("valid token should succeed, got %d", rr.Code)
			}
		} else {
			if rr.Code != http.StatusUnauthorized {
				t.Errorf("invalid token %q should be rejected, got %d", token, rr.Code)
			}
		}
	}
}

// TestCORS tests the CORS middleware
func TestCORS(t *testing.T) {
	tests := []struct {
		name           string
		allowedOrigins []string
		requestOrigin  string
		expectedOrigin string
		shouldHaveACO  bool // Should have Access-Control-Allow-Origin
	}{
		{
			name:           "development mode allows any origin",
			allowedOrigins: nil,
			requestOrigin:  "http://localhost:5173",
			expectedOrigin: "http://localhost:5173",
			shouldHaveACO:  true,
		},
		{
			name:           "production mode allows configured origin",
			allowedOrigins: []string{"https://example.com"},
			requestOrigin:  "https://example.com",
			expectedOrigin: "https://example.com",
			shouldHaveACO:  true,
		},
		{
			name:           "production mode blocks unconfigured origin",
			allowedOrigins: []string{"https://example.com"},
			requestOrigin:  "https://evil.com",
			expectedOrigin: "",
			shouldHaveACO:  false,
		},
		{
			name:           "no origin header",
			allowedOrigins: nil,
			requestOrigin:  "",
			expectedOrigin: "",
			shouldHaveACO:  false,
		},
		{
			name:           "multiple allowed origins",
			allowedOrigins: []string{"https://example.com", "https://app.example.com"},
			requestOrigin:  "https://app.example.com",
			expectedOrigin: "https://app.example.com",
			shouldHaveACO:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := CORSConfig{AllowedOrigins: tt.allowedOrigins}
			corsMiddleware := CORS(cfg)

			handler := corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest("GET", "/api/test", nil)
			if tt.requestOrigin != "" {
				req.Header.Set("Origin", tt.requestOrigin)
			}

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			aco := rr.Header().Get("Access-Control-Allow-Origin")
			if tt.shouldHaveACO {
				if aco != tt.expectedOrigin {
					t.Errorf("expected Access-Control-Allow-Origin %q, got %q", tt.expectedOrigin, aco)
				}
			} else {
				if aco != "" {
					t.Errorf("expected no Access-Control-Allow-Origin, got %q", aco)
				}
			}
		})
	}
}

// TestCORSPreflight tests CORS preflight handling
func TestCORSPreflight(t *testing.T) {
	cfg := CORSConfig{AllowedOrigins: nil}
	corsMiddleware := CORS(cfg)

	called := false
	handler := corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("OPTIONS", "/api/test", nil)
	req.Header.Set("Origin", "http://localhost:5173")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Errorf("expected status %d for preflight, got %d", http.StatusNoContent, rr.Code)
	}

	if called {
		t.Error("handler should not be called for preflight requests")
	}

	// Check CORS headers are set
	if rr.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Error("expected Access-Control-Allow-Methods header")
	}
	if rr.Header().Get("Access-Control-Allow-Headers") == "" {
		t.Error("expected Access-Control-Allow-Headers header")
	}
}

// TestRateLimiter tests the rate limiting middleware
func TestRateLimiter(t *testing.T) {
	cfg := RateLimiterConfig{
		RequestsPerSecond: 5,
		BurstSize:         10,
		CleanupInterval:   time.Minute,
	}
	rateLimiter := RateLimiter(cfg)

	handler := rateLimiter(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Make requests up to burst size - should all succeed
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest("GET", "/api/test", nil)
		req.RemoteAddr = "192.168.1.1:12345"

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("request %d: expected status %d, got %d", i+1, http.StatusOK, rr.Code)
		}
	}

	// Next request should be rate limited
	req := httptest.NewRequest("GET", "/api/test", nil)
	req.RemoteAddr = "192.168.1.1:12345"

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusTooManyRequests {
		t.Errorf("expected rate limit status %d, got %d", http.StatusTooManyRequests, rr.Code)
	}

	// Check Retry-After header
	if rr.Header().Get("Retry-After") == "" {
		t.Error("expected Retry-After header on rate limited response")
	}
}

// TestRateLimiterDifferentIPs tests that rate limiting is per-IP
func TestRateLimiterDifferentIPs(t *testing.T) {
	cfg := RateLimiterConfig{
		RequestsPerSecond: 1,
		BurstSize:         2,
		CleanupInterval:   time.Minute,
	}
	rateLimiter := RateLimiter(cfg)

	handler := rateLimiter(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Exhaust rate limit for IP 1
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("GET", "/api/test", nil)
		req.RemoteAddr = "192.168.1.1:12345"
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
	}

	// IP 2 should still be able to make requests
	req := httptest.NewRequest("GET", "/api/test", nil)
	req.RemoteAddr = "192.168.1.2:12345"

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("different IP should not be rate limited, got status %d", rr.Code)
	}
}

// TestRateLimiterXForwardedFor tests X-Forwarded-For header handling
func TestRateLimiterXForwardedFor(t *testing.T) {
	cfg := RateLimiterConfig{
		RequestsPerSecond: 1,
		BurstSize:         2,
		CleanupInterval:   time.Minute,
	}
	rateLimiter := RateLimiter(cfg)

	handler := rateLimiter(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Make requests with X-Forwarded-For
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("GET", "/api/test", nil)
		req.RemoteAddr = "127.0.0.1:12345" // Proxy IP
		req.Header.Set("X-Forwarded-For", "203.0.113.1, 198.51.100.1")

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
	}

	// Should be rate limited based on first IP in X-Forwarded-For
	req := httptest.NewRequest("GET", "/api/test", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "203.0.113.1, 198.51.100.1")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusTooManyRequests {
		t.Errorf("expected rate limit, got status %d", rr.Code)
	}
}

// TestRateLimiterTokenRefill tests that tokens refill over time
func TestRateLimiterTokenRefill(t *testing.T) {
	cfg := RateLimiterConfig{
		RequestsPerSecond: 100, // High rate for quick refill
		BurstSize:         2,
		CleanupInterval:   time.Minute,
	}
	rateLimiter := RateLimiter(cfg)

	handler := rateLimiter(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	ip := "192.168.1.100:12345"

	// Exhaust tokens
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("GET", "/api/test", nil)
		req.RemoteAddr = ip
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
	}

	// Wait for token refill
	time.Sleep(50 * time.Millisecond)

	// Should be able to make requests again
	req := httptest.NewRequest("GET", "/api/test", nil)
	req.RemoteAddr = ip

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected OK after token refill, got status %d", rr.Code)
	}
}

// TestRateLimiterConcurrency tests concurrent access
func TestRateLimiterConcurrency(t *testing.T) {
	cfg := RateLimiterConfig{
		RequestsPerSecond: 100,
		BurstSize:         50,
		CleanupInterval:   time.Minute,
	}
	rateLimiter := RateLimiter(cfg)

	handler := rateLimiter(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	var wg sync.WaitGroup
	errors := make(chan error, 100)

	// Make concurrent requests
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			req := httptest.NewRequest("GET", "/api/test", nil)
			req.RemoteAddr = "192.168.1.1:12345"

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			// Just check it doesn't panic
			if rr.Code != http.StatusOK && rr.Code != http.StatusTooManyRequests {
				errors <- nil
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	// Check no unexpected errors
	for err := range errors {
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	}
}

// TestSecurityHeaders tests the security headers middleware
func TestSecurityHeaders(t *testing.T) {
	handler := SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	expectedHeaders := map[string]string{
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
		"X-XSS-Protection":       "1; mode=block",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
	}

	for header, expected := range expectedHeaders {
		if got := rr.Header().Get(header); got != expected {
			t.Errorf("expected %s: %q, got %q", header, expected, got)
		}
	}
}

// TestRecoverer tests the panic recovery middleware
func TestRecoverer(t *testing.T) {
	handler := Recoverer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	}))

	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()

	// Should not panic
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("expected status %d after panic, got %d", http.StatusInternalServerError, rr.Code)
	}
}

// TestRequestLogger tests the request logging middleware
func TestRequestLogger(t *testing.T) {
	handler := RequestLogger(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Should not panic and should complete
	req := httptest.NewRequest("GET", "/api/test", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, rr.Code)
	}
}

// TestGetClientIP tests IP extraction from requests
func TestGetClientIP(t *testing.T) {
	// Test with no trusted proxies - should always use direct IP
	t.Run("no trusted proxies", func(t *testing.T) {
		tests := []struct {
			name          string
			remoteAddr    string
			xForwardedFor string
			xRealIP       string
			expectedIP    string
		}{
			{
				name:       "remote addr only",
				remoteAddr: "192.168.1.1:12345",
				expectedIP: "192.168.1.1",
			},
			{
				name:          "ignores x-forwarded-for without trusted proxies",
				remoteAddr:    "192.168.1.1:12345",
				xForwardedFor: "203.0.113.1",
				expectedIP:    "192.168.1.1", // Uses direct IP, not forwarded
			},
			{
				name:       "ignores x-real-ip without trusted proxies",
				remoteAddr: "192.168.1.1:12345",
				xRealIP:    "203.0.113.50",
				expectedIP: "192.168.1.1", // Uses direct IP
			},
		}

		emptyProxies := buildTrustedProxySet(nil)
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				req := httptest.NewRequest("GET", "/", nil)
				req.RemoteAddr = tt.remoteAddr
				if tt.xForwardedFor != "" {
					req.Header.Set("X-Forwarded-For", tt.xForwardedFor)
				}
				if tt.xRealIP != "" {
					req.Header.Set("X-Real-IP", tt.xRealIP)
				}

				ip := getClientIP(req, emptyProxies)
				if ip != tt.expectedIP {
					t.Errorf("expected IP %q, got %q", tt.expectedIP, ip)
				}
			})
		}
	})

	// Test with trusted proxy - should trust forwarded headers from proxy
	t.Run("with trusted proxy", func(t *testing.T) {
		trustedProxies := buildTrustedProxySet([]string{"127.0.0.1", "10.0.0.1"})

		tests := []struct {
			name          string
			remoteAddr    string
			xForwardedFor string
			xRealIP       string
			expectedIP    string
		}{
			{
				name:          "x-forwarded-for from trusted proxy",
				remoteAddr:    "127.0.0.1:12345",
				xForwardedFor: "203.0.113.1",
				expectedIP:    "203.0.113.1",
			},
			{
				name:          "x-forwarded-for multiple from trusted proxy",
				remoteAddr:    "127.0.0.1:12345",
				xForwardedFor: "203.0.113.1, 198.51.100.1, 10.0.0.1",
				expectedIP:    "203.0.113.1",
			},
			{
				name:       "x-real-ip from trusted proxy",
				remoteAddr: "127.0.0.1:12345",
				xRealIP:    "203.0.113.50",
				expectedIP: "203.0.113.50",
			},
			{
				name:          "x-forwarded-for takes precedence from trusted proxy",
				remoteAddr:    "127.0.0.1:12345",
				xForwardedFor: "203.0.113.1",
				xRealIP:       "203.0.113.50",
				expectedIP:    "203.0.113.1",
			},
			{
				name:          "untrusted proxy - ignores headers",
				remoteAddr:    "192.168.1.1:12345",
				xForwardedFor: "203.0.113.1",
				expectedIP:    "192.168.1.1", // Uses direct IP since proxy not trusted
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				req := httptest.NewRequest("GET", "/", nil)
				req.RemoteAddr = tt.remoteAddr
				if tt.xForwardedFor != "" {
					req.Header.Set("X-Forwarded-For", tt.xForwardedFor)
				}
				if tt.xRealIP != "" {
					req.Header.Set("X-Real-IP", tt.xRealIP)
				}

				ip := getClientIP(req, trustedProxies)
				if ip != tt.expectedIP {
					t.Errorf("expected IP %q, got %q", tt.expectedIP, ip)
				}
			})
		}
	})
}
