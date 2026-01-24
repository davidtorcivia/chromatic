package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"chromatic/internal/api"
	"chromatic/internal/config"
	"chromatic/internal/database"
	"chromatic/internal/logger"
	"chromatic/internal/webrtc"
	"chromatic/internal/websocket"
)

func main() {
	// Load configuration first (we need production mode for logger)
	cfg, err := config.Load()
	if err != nil {
		// Can't use structured logger yet, fall back to standard
		slog.Error("Failed to load configuration", "error", err)
		os.Exit(1)
	}

	// Initialize structured logger
	logger.Initialize(cfg.ProductionMode)

	logger.Info("Starting Chromatic server",
		"port", cfg.Port,
		"public_url", cfg.PublicURL,
		"production_mode", cfg.ProductionMode,
	)

	// Initialize database
	db, err := database.New(cfg.DatabasePath)
	if err != nil {
		logger.Error("Failed to initialize database", "error", err, "path", cfg.DatabasePath)
		os.Exit(1)
	}
	defer db.Close()

	// Run migrations
	if err := db.Migrate(); err != nil {
		logger.Error("Failed to run migrations", "error", err)
		os.Exit(1)
	}
	logger.Info("Database initialized", "path", cfg.DatabasePath)

	// Initialize WebRTC SFU
	sfu, err := webrtc.NewSFU(cfg)
	if err != nil {
		logger.Error("Failed to initialize SFU", "error", err)
		os.Exit(1)
	}
	logger.Info("WebRTC SFU initialized")

	// Initialize WebSocket hub
	hub := websocket.NewHub()
	go hub.Run()
	logger.Info("WebSocket hub started")

	// Create HTTP router
	router := api.NewRouter(cfg, db, sfu, hub)

	// Create HTTP server
	server := &http.Server{
		Addr:         cfg.ListenAddr(),
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start server in goroutine
	go func() {
		logger.Info("HTTP server listening", "addr", cfg.ListenAddr())
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("Server error", "error", err)
			os.Exit(1)
		}
	}()

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit

	logger.Info("Shutdown signal received", "signal", sig.String())

	// Graceful shutdown with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Stop accepting new WebRTC connections
	sfu.Shutdown()
	logger.Info("WebRTC SFU stopped")

	// Close all WebSocket connections
	hub.Shutdown()
	logger.Info("WebSocket hub stopped")

	// Shutdown HTTP server
	if err := server.Shutdown(ctx); err != nil {
		logger.Error("Server forced to shutdown", "error", err)
	}

	logger.Info("Server stopped gracefully")
}
