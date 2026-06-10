package api

import (
	"net/http"
	"path/filepath"
	"strings"

	"chromatic/internal/api/handlers"
	"chromatic/internal/api/middleware"
	"chromatic/internal/config"
	"chromatic/internal/database"
	"chromatic/internal/metrics"
	"chromatic/internal/webrtc"
	"chromatic/internal/websocket"
)

// spaHandler serves the SvelteKit build output with an index.html fallback
// for client-side routes. Cache-Control headers prevent browsers from serving
// stale HTML that references old hashed assets after a deploy:
//   - /_app/immutable/* files are content-hashed, so they can be cached forever
//   - everything else (index.html fallback, page HTML, favicons) is no-cache,
//     forcing revalidation; ETag/Last-Modified keep that cheap (304s)
func spaHandler(staticRoot string) http.Handler {
	staticDir := http.Dir(staticRoot)
	fileServer := http.FileServer(staticDir)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path != "" {
			if file, err := staticDir.Open(path); err == nil {
				defer file.Close()
				if info, err := file.Stat(); err == nil && !info.IsDir() {
					if strings.HasPrefix(r.URL.Path, "/_app/immutable/") {
						w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
					} else {
						w.Header().Set("Cache-Control", "no-cache")
					}
					fileServer.ServeHTTP(w, r)
					return
				}
			}
		}
		// SPA fallback: index.html must never be cached as immutable, even
		// when the request path looked like a hashed asset.
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, filepath.Join(staticRoot, "index.html"))
	})
}

// NewRouter creates the HTTP router with all routes configured
func NewRouter(cfg *config.Config, db *database.DB, sfu *webrtc.SFU, hub *websocket.Hub) http.Handler {
	mux := http.NewServeMux()

	// Join tokens are signed with a secret derived from the admin token,
	// never the admin token itself.
	tokenSecret := handlers.DeriveTokenSecret(cfg.AdminToken)

	// Create handlers
	roomHandler := handlers.NewRoomHandler(db, cfg, tokenSecret)
	roomHandler.SetSFU(sfu)
	roomHandler.SetHub(hub)

	streamKeyHandler := handlers.NewStreamKeyHandler(db)
	fileHandler := handlers.NewFileHandler(db, cfg, tokenSecret)
	configHandler := handlers.NewConfigHandler(db, cfg, sfu)
	authHandler := handlers.NewAuthHandler(db, cfg.AdminToken, cfg.ProductionMode)
	wsHandler := handlers.NewWebSocketHandler(db, hub, sfu, cfg.AllowedOrigins, cfg.ProductionMode, tokenSecret, authHandler.ValidateSession)

	// Join requests carrying a valid admin session cookie are granted the
	// admin role (dashboard "Join as host") — same validator the WebSocket
	// handler uses.
	roomHandler.SetSessionValidator(authHandler.ValidateSession)
	fileHandler.SetSessionValidator(authHandler.ValidateSession)

	// WS admin:waiting-approve/deny reuse the exact admit/deny logic the REST
	// endpoints run (DB update + SSE notification + admin broadcast).
	wsHandler.SetWaitingActions(roomHandler)

	// Wire up room live callback to initiate WebRTC subscriptions
	roomHandler.SetOnRoomLive(wsHandler.InitiateSubscriptionsForRoom)

	// Create WHIP handler
	whipHandler := webrtc.NewWHIPHandler(
		sfu,
		streamKeyHandler.ValidateKey,
		func(token string) error {
			// Called when stream starts
			return roomHandler.OnStreamStart(token)
		},
		func(token string) {
			// Called when stream ends
			roomHandler.OnStreamEnd(token)
		},
	)

	// Auth configuration shared by admin routes and the metrics endpoint
	authConfig := middleware.AuthConfig{
		AdminToken:      cfg.AdminToken,
		SessionCookie:   handlers.SessionCookieName,
		ValidateSession: authHandler.ValidateSession,
	}

	// Health check (public — used by load balancers and Caddy)
	mux.HandleFunc("GET /health", handlers.HealthCheck)

	// Prometheus metrics — behind the same admin auth as /api. Prometheus can
	// scrape with `Authorization: Bearer <admin-token>` (or a proxy that
	// injects the cookie). Leaving /metrics unauthenticated leaks operational
	// counters.
	mux.Handle("GET /metrics", middleware.RequireAuth(authConfig)(http.HandlerFunc(metrics.Handler())))

	// Auth endpoints (no auth required) - login rate limited: 5 per minute per IP
	mux.Handle("POST /api/auth/login", middleware.LoginRateLimiter(cfg.TrustedProxies)(http.HandlerFunc(authHandler.Login)))
	mux.HandleFunc("POST /api/auth/logout", authHandler.Logout)

	// WHIP endpoints (no auth - stream key is in URL)
	mux.Handle("/whip/", whipHandler)

	// Admin API (requires auth)
	adminMux := http.NewServeMux()

	// Stream Keys
	adminMux.HandleFunc("GET /api/stream-keys", streamKeyHandler.List)
	adminMux.HandleFunc("POST /api/stream-keys", streamKeyHandler.Create)
	adminMux.HandleFunc("DELETE /api/stream-keys/{id}", streamKeyHandler.Delete)

	// Rooms
	adminMux.HandleFunc("GET /api/rooms", roomHandler.List)
	// Room creation rate limited: 10 per hour
	adminMux.Handle("POST /api/rooms", middleware.RoomCreationRateLimiter(cfg.TrustedProxies)(http.HandlerFunc(roomHandler.Create)))
	adminMux.HandleFunc("GET /api/rooms/{slug}", roomHandler.Get)
	adminMux.HandleFunc("PATCH /api/rooms/{slug}", roomHandler.Update)
	adminMux.HandleFunc("DELETE /api/rooms/{slug}", roomHandler.Delete)
	adminMux.HandleFunc("POST /api/rooms/{slug}/end", roomHandler.EndSession)
	// Open a scheduled room ahead of time (auto-admits the countdown lobby)
	adminMux.HandleFunc("POST /api/rooms/{slug}/open", roomHandler.OpenRoom)

	// File review/moderation (admin)
	adminMux.HandleFunc("GET /api/rooms/{slug}/files", fileHandler.ListRoomFiles)
	adminMux.HandleFunc("DELETE /api/files/{id}", fileHandler.DeleteFile)

	// Waiting Room (admin)
	adminMux.HandleFunc("GET /api/rooms/{slug}/waiting", roomHandler.ListWaiting)
	adminMux.HandleFunc("POST /api/rooms/{slug}/admit/{id}", roomHandler.AdmitParticipant)
	adminMux.HandleFunc("POST /api/rooms/{slug}/admit-all", roomHandler.AdmitAll)
	adminMux.HandleFunc("POST /api/rooms/{slug}/deny/{id}", roomHandler.DenyParticipant)

	// Config (admin)
	adminMux.HandleFunc("GET /api/config", configHandler.Get)
	adminMux.HandleFunc("PATCH /api/config", configHandler.Update)
	adminMux.HandleFunc("POST /api/config/logo", configHandler.UploadLogo)
	adminMux.HandleFunc("DELETE /api/config/logo", configHandler.DeleteLogo)
	adminMux.HandleFunc("POST /api/config/test-turn", configHandler.TestTURN)

	// Wrap admin routes with auth middleware
	mux.Handle("/api/", middleware.RequireAuth(authConfig)(adminMux))

	// File endpoints (session auth) - rate limited: 10 per minute
	mux.Handle("POST /api/rooms/{slug}/files", middleware.FileUploadRateLimiter(cfg.TrustedProxies)(http.HandlerFunc(fileHandler.Upload)))
	mux.HandleFunc("GET /api/files/{id}", fileHandler.Download)
	mux.HandleFunc("GET /api/files/{id}/thumbnail", fileHandler.Thumbnail)
	// Public watermark logo (viewers need access)
	mux.HandleFunc("GET /api/config/logo", configHandler.GetLogo)

	// WebSocket endpoint
	mux.HandleFunc("GET /ws/room/{slug}", wsHandler.HandleConnection)

	// Join API (password validation, etc.) - rate limited: 5 per minute per room per IP
	mux.Handle("POST /api/rooms/{slug}/join", middleware.RoomJoinRateLimiter(cfg.TrustedProxies)(http.HandlerFunc(roomHandler.Join)))
	mux.HandleFunc("GET /api/rooms/{slug}/info", roomHandler.PublicInfo)
	mux.HandleFunc("GET /api/rooms/{slug}/status/{id}", roomHandler.CheckParticipantStatus)
	mux.HandleFunc("GET /api/rooms/{slug}/waiting/events/{id}", roomHandler.WaitingEvents)

	// Static files (SvelteKit build) with SPA fallback
	mux.Handle("/", spaHandler("static"))

	// Apply global middleware
	var handler http.Handler = mux

	// CORS configuration - requires ALLOWED_ORIGINS in production mode
	corsConfig := middleware.CORSConfig{
		AllowedOrigins: cfg.AllowedOrigins,
		ProductionMode: cfg.ProductionMode,
	}
	handler = middleware.CORS(corsConfig)(handler)

	// Rate limiting - protect against abuse
	rateLimitConfig := middleware.RateLimiterConfig{
		RequestsPerSecond: 20,
		BurstSize:         50,
		TrustedProxies:    cfg.TrustedProxies,
	}
	handler = middleware.RateLimiter(rateLimitConfig)(handler)

	// Security headers
	handler = middleware.SecurityHeaders(cfg.ProductionMode)(handler)

	// Request logging
	handler = middleware.RequestLogger(handler)

	// Panic recovery
	handler = middleware.Recoverer(handler)

	return handler
}
