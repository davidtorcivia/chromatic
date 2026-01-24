package handlers

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"chromatic/internal/config"
	"chromatic/internal/database"
	"chromatic/internal/logger"
)

// ConfigHandler handles application configuration endpoints
type ConfigHandler struct {
	db  *database.DB
	cfg *config.Config
}

// NewConfigHandler creates a new ConfigHandler
func NewConfigHandler(db *database.DB, cfg *config.Config) *ConfigHandler {
	return &ConfigHandler{db: db, cfg: cfg}
}

// ConfigResponse is the response structure for config endpoints
type ConfigResponse struct {
	DefaultWatermarkText     *string `json:"defaultWatermarkText,omitempty"`
	DefaultWatermarkLogoPath *string `json:"defaultWatermarkLogoPath,omitempty"`
	DefaultWatermarkLogoURL  *string `json:"defaultWatermarkLogoUrl,omitempty"`
	TurnExternalURL          *string `json:"turnExternalUrl,omitempty"`
	TurnExternalUsername     *string `json:"turnExternalUsername,omitempty"`
	HasTurnCredential        bool    `json:"hasTurnCredential"`
	// Informational fields (read-only)
	PublicURL string `json:"publicUrl"`
	WHIPFormat string `json:"whipFormat"`
}

// Get retrieves the current application configuration
func (h *ConfigHandler) Get(w http.ResponseWriter, r *http.Request) {
	var response ConfigResponse

	// Query config from database
	var watermarkText, watermarkLogoPath, turnURL, turnUsername, turnCredential *string
	err := h.db.QueryRow(`
		SELECT default_watermark_text, default_watermark_logo_path,
		       turn_external_url, turn_external_username, turn_external_credential
		FROM config WHERE id = 1
	`).Scan(&watermarkText, &watermarkLogoPath, &turnURL, &turnUsername, &turnCredential)

	if err != nil {
		// If no config exists, return empty config (it will be created on first update)
		logger.Debug("No config found, returning defaults", "error", err)
	}

	response.DefaultWatermarkText = watermarkText
	response.DefaultWatermarkLogoPath = watermarkLogoPath
	response.TurnExternalURL = turnURL
	response.TurnExternalUsername = turnUsername
	response.HasTurnCredential = turnCredential != nil && *turnCredential != ""

	// Generate logo URL if path exists
	if watermarkLogoPath != nil && *watermarkLogoPath != "" {
		logoURL := "/api/config/logo"
		response.DefaultWatermarkLogoURL = &logoURL
	}

	// Add informational fields
	response.PublicURL = h.cfg.PublicURL
	response.WHIPFormat = h.cfg.PublicURL + "/whip/{stream_key_token}"

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// UpdateConfigRequest is the request structure for updating config
type UpdateConfigRequest struct {
	DefaultWatermarkText   *string `json:"defaultWatermarkText"`
	TurnExternalURL        *string `json:"turnExternalUrl"`
	TurnExternalUsername   *string `json:"turnExternalUsername"`
	TurnExternalCredential *string `json:"turnExternalCredential"`
}

// Update updates the application configuration
func (h *ConfigHandler) Update(w http.ResponseWriter, r *http.Request) {
	var req UpdateConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Ensure config row exists (upsert pattern)
	_, err := h.db.Exec(`INSERT OR IGNORE INTO config (id) VALUES (1)`)
	if err != nil {
		logger.Error("Failed to ensure config exists", "error", err)
		http.Error(w, "Failed to update configuration", http.StatusInternalServerError)
		return
	}

	// Update only the fields that were provided
	if req.DefaultWatermarkText != nil {
		_, err = h.db.Exec(`UPDATE config SET default_watermark_text = ? WHERE id = 1`, *req.DefaultWatermarkText)
		if err != nil {
			logger.Error("Failed to update watermark text", "error", err)
			http.Error(w, "Failed to update watermark settings", http.StatusInternalServerError)
			return
		}
	}

	if req.TurnExternalURL != nil {
		_, err = h.db.Exec(`UPDATE config SET turn_external_url = ? WHERE id = 1`, *req.TurnExternalURL)
		if err != nil {
			logger.Error("Failed to update TURN URL", "error", err)
			http.Error(w, "Failed to update TURN settings", http.StatusInternalServerError)
			return
		}
	}

	if req.TurnExternalUsername != nil {
		_, err = h.db.Exec(`UPDATE config SET turn_external_username = ? WHERE id = 1`, *req.TurnExternalUsername)
		if err != nil {
			logger.Error("Failed to update TURN username", "error", err)
			http.Error(w, "Failed to update TURN settings", http.StatusInternalServerError)
			return
		}
	}

	if req.TurnExternalCredential != nil {
		_, err = h.db.Exec(`UPDATE config SET turn_external_credential = ? WHERE id = 1`, *req.TurnExternalCredential)
		if err != nil {
			logger.Error("Failed to update TURN credential", "error", err)
			http.Error(w, "Failed to update TURN settings", http.StatusInternalServerError)
			return
		}
	}

	// Return updated config
	h.Get(w, r)
}

// UploadLogo handles logo file uploads
func (h *ConfigHandler) UploadLogo(w http.ResponseWriter, r *http.Request) {
	// Parse multipart form (max 1MB for logos)
	const maxLogoSize = 1 * 1024 * 1024
	r.Body = http.MaxBytesReader(w, r.Body, maxLogoSize+1024)
	if err := r.ParseMultipartForm(maxLogoSize); err != nil {
		http.Error(w, "File too large (max 1MB)", http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("logo")
	if err != nil {
		http.Error(w, "Missing logo file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Validate file type
	buffer := make([]byte, 512)
	_, err = file.Read(buffer)
	if err != nil {
		http.Error(w, "Failed to read file", http.StatusBadRequest)
		return
	}
	mimeType := http.DetectContentType(buffer)

	allowedTypes := map[string]bool{
		"image/png":  true,
		"image/jpeg": true,
		"image/webp": true,
	}
	if !allowedTypes[mimeType] {
		http.Error(w, "Invalid file type. Use PNG, JPEG, or WebP.", http.StatusBadRequest)
		return
	}

	// Reset file position
	file.Seek(0, 0)

	// Create logos directory if it doesn't exist
	if err := os.MkdirAll(h.cfg.LogoPath, 0755); err != nil {
		logger.Error("Failed to create logo directory", "path", h.cfg.LogoPath, "error", err)
		http.Error(w, "Failed to create logo directory", http.StatusInternalServerError)
		return
	}

	// Save logo with fixed name (overwrite previous)
	ext := filepath.Ext(header.Filename)
	if ext == "" {
		switch mimeType {
		case "image/png":
			ext = ".png"
		case "image/jpeg":
			ext = ".jpg"
		case "image/webp":
			ext = ".webp"
		}
	}
	logoPath := filepath.Join(h.cfg.LogoPath, "default_watermark"+ext)

	// Read all file content
	file.Seek(0, 0)
	fileContent := make([]byte, header.Size)
	_, err = file.Read(fileContent)
	if err != nil {
		http.Error(w, "Failed to read file", http.StatusInternalServerError)
		return
	}

	// Write to disk
	if err := os.WriteFile(logoPath, fileContent, 0644); err != nil {
		logger.Error("Failed to save logo file", "path", logoPath, "error", err)
		http.Error(w, "Failed to save logo file", http.StatusInternalServerError)
		return
	}

	// Ensure config row exists
	_, err = h.db.Exec(`INSERT OR IGNORE INTO config (id) VALUES (1)`)
	if err != nil {
		logger.Error("Failed to ensure config exists", "error", err)
		http.Error(w, "Failed to save logo configuration", http.StatusInternalServerError)
		return
	}

	// Update database with logo path
	_, err = h.db.Exec(`UPDATE config SET default_watermark_logo_path = ? WHERE id = 1`, logoPath)
	if err != nil {
		logger.Error("Failed to update logo path in database", "path", logoPath, "error", err)
		http.Error(w, "Failed to save logo configuration", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"logoUrl": "/api/config/logo",
		"path":    logoPath,
	})
}

// GetLogo serves the default watermark logo
func (h *ConfigHandler) GetLogo(w http.ResponseWriter, r *http.Request) {
	var logoPath *string
	err := h.db.QueryRow(`SELECT default_watermark_logo_path FROM config WHERE id = 1`).Scan(&logoPath)
	if err != nil || logoPath == nil || *logoPath == "" {
		http.Error(w, "No logo configured", http.StatusNotFound)
		return
	}

	// Check if file exists
	if _, err := os.Stat(*logoPath); os.IsNotExist(err) {
		http.Error(w, "Logo file not found", http.StatusNotFound)
		return
	}

	http.ServeFile(w, r, *logoPath)
}

// DeleteLogo removes the default watermark logo
func (h *ConfigHandler) DeleteLogo(w http.ResponseWriter, r *http.Request) {
	var logoPath *string
	err := h.db.QueryRow(`SELECT default_watermark_logo_path FROM config WHERE id = 1`).Scan(&logoPath)
	if err != nil || logoPath == nil || *logoPath == "" {
		http.Error(w, "No logo configured", http.StatusNotFound)
		return
	}

	// Delete the file
	if err := os.Remove(*logoPath); err != nil && !os.IsNotExist(err) {
		logger.Warn("Failed to delete logo file", "path", *logoPath, "error", err)
		// Continue anyway to clear database reference
	}

	// Clear database reference
	_, err = h.db.Exec(`UPDATE config SET default_watermark_logo_path = NULL WHERE id = 1`)
	if err != nil {
		logger.Error("Failed to clear logo path in database", "error", err)
		http.Error(w, "Failed to remove logo configuration", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// TURNTestResult represents the result of a TURN server test
type TURNTestResult struct {
	Server      string `json:"server"`
	Reachable   bool   `json:"reachable"`
	Latency     int64  `json:"latency,omitempty"` // milliseconds
	Error       string `json:"error,omitempty"`
	Protocol    string `json:"protocol,omitempty"`
	TestType    string `json:"testType"` // "self-hosted" or "external"
}

// TURNTestResponse represents the full TURN test response
type TURNTestResponse struct {
	Success bool             `json:"success"`
	Results []TURNTestResult `json:"results"`
	Message string           `json:"message,omitempty"`
}

// TestTURN tests connectivity to configured TURN servers
func (h *ConfigHandler) TestTURN(w http.ResponseWriter, r *http.Request) {
	var results []TURNTestResult

	// Test self-hosted TURN server (Coturn)
	if h.cfg.TurnRealm != "" {
		turnHost := h.cfg.TurnRealm
		if !strings.Contains(turnHost, ":") {
			turnHost = turnHost + ":3478"
		}

		result := testTURNServer(turnHost, "udp", "self-hosted")
		results = append(results, result)

		// Also test TCP
		tcpResult := testTURNServer(turnHost, "tcp", "self-hosted")
		results = append(results, tcpResult)
	}

	// Test external TURN server if configured
	if h.cfg.TurnExternalURL != "" {
		host, protocol, err := parseTURNURL(h.cfg.TurnExternalURL)
		if err != nil {
			results = append(results, TURNTestResult{
				Server:    h.cfg.TurnExternalURL,
				Reachable: false,
				Error:     fmt.Sprintf("Invalid TURN URL: %v", err),
				TestType:  "external",
			})
		} else {
			result := testTURNServer(host, protocol, "external")
			results = append(results, result)
		}
	}

	// Also check database for configured TURN
	var turnURL *string
	h.db.QueryRow(`SELECT turn_external_url FROM config WHERE id = 1`).Scan(&turnURL)
	if turnURL != nil && *turnURL != "" && *turnURL != h.cfg.TurnExternalURL {
		host, protocol, err := parseTURNURL(*turnURL)
		if err != nil {
			results = append(results, TURNTestResult{
				Server:    *turnURL,
				Reachable: false,
				Error:     fmt.Sprintf("Invalid TURN URL: %v", err),
				TestType:  "external (database)",
			})
		} else {
			result := testTURNServer(host, protocol, "external (database)")
			results = append(results, result)
		}
	}

	// Calculate overall success
	success := false
	for _, r := range results {
		if r.Reachable {
			success = true
			break
		}
	}

	response := TURNTestResponse{
		Success: success,
		Results: results,
	}

	if len(results) == 0 {
		response.Message = "No TURN servers configured"
	} else if success {
		response.Message = "At least one TURN server is reachable"
	} else {
		response.Message = "No TURN servers are reachable"
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// testTURNServer tests connectivity to a TURN server
func testTURNServer(host, protocol, testType string) TURNTestResult {
	result := TURNTestResult{
		Server:   host,
		Protocol: protocol,
		TestType: testType,
	}

	// Determine network type
	network := "tcp"
	if protocol == "udp" {
		network = "udp"
	}

	start := time.Now()
	timeout := 5 * time.Second

	conn, err := net.DialTimeout(network, host, timeout)
	if err != nil {
		result.Reachable = false
		result.Error = err.Error()
		logger.Warn("TURN server test failed", "host", host, "protocol", protocol, "error", err)
		return result
	}
	defer conn.Close()

	result.Reachable = true
	result.Latency = time.Since(start).Milliseconds()
	logger.Info("TURN server test succeeded", "host", host, "protocol", protocol, "latency_ms", result.Latency)

	return result
}

// parseTURNURL parses a TURN URL and returns host:port and protocol
func parseTURNURL(turnURL string) (host string, protocol string, err error) {
	// Handle turn: and turns: schemes
	turnURL = strings.TrimPrefix(turnURL, "turn:")
	turnURL = strings.TrimPrefix(turnURL, "turns:")
	turnURL = strings.TrimPrefix(turnURL, "//")

	// Check for transport parameter
	protocol = "udp" // default
	if strings.Contains(turnURL, "?") {
		parts := strings.SplitN(turnURL, "?", 2)
		turnURL = parts[0]
		query, err := url.ParseQuery(parts[1])
		if err == nil {
			if transport := query.Get("transport"); transport != "" {
				protocol = strings.ToLower(transport)
			}
		}
	}

	// Parse host:port
	host = turnURL
	if !strings.Contains(host, ":") {
		// Add default port based on protocol
		if protocol == "tcp" || protocol == "tls" {
			host = host + ":443"
		} else {
			host = host + ":3478"
		}
	}

	return host, protocol, nil
}
