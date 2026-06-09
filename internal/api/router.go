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
	wsHandler := handlers.NewWebSocketHandler(db, hub, sfu, cfg.AllowedOrigins, cfg.ProductionMode, tokenSecret)
	authHandler := handlers.NewAuthHandler(db, cfg.AdminToken, cfg.ProductionMode)

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

	// Health check
	mux.HandleFunc("GET /health", handlers.HealthCheck)

	// Prometheus metrics endpoint (requires admin session cookie or bearer
	// token — Prometheus can scrape with an Authorization header)
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

	// Waiting Room (admin)
	adminMux.HandleFunc("GET /api/rooms/{slug}/waiting", roomHandler.ListWaiting)
	adminMux.HandleFunc("POST /api/rooms/{slug}/admit/{id}", roomHandler.AdmitParticipant)
	adminMux.HandleFunc("POST /api/rooms/{slug}/admit-all", roomHandler.AdmitAll)

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
	staticDir := http.Dir("./static")
	fileServer := http.FileServer(staticDir)
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path != "" {
			if file, err := staticDir.Open(path); err == nil {
				defer file.Close()
				if info, err := file.Stat(); err == nil && !info.IsDir() {
					fileServer.ServeHTTP(w, r)
					return
				}
			}
		}
		http.ServeFile(w, r, filepath.Join("static", "index.html"))
	}))

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
	handler = middleware.SecurityHeaders(handler)

	// Request logging
	handler = middleware.RequestLogger(handler)

	// Panic recovery
	handler = middleware.Recoverer(handler)

	return handler
}
