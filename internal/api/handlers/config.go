package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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
	"chromatic/internal/webrtc"
)

var allowedLogoMIMETypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
}

// ConfigHandler handles application configuration endpoints
type ConfigHandler struct {
	db  *database.DB
	cfg *config.Config
	sfu *webrtc.SFU
}

// NewConfigHandler creates a new ConfigHandler
func NewConfigHandler(db *database.DB, cfg *config.Config, sfu *webrtc.SFU) *ConfigHandler {
	h := &ConfigHandler{db: db, cfg: cfg, sfu: sfu}
	h.syncRuntimeTURNFromDB()
	return h
}

// ConfigResponse is the response structure for config endpoints
type ConfigResponse struct {
	DefaultWatermarkText     *string `json:"defaultWatermarkText,omitempty"`
	DefaultWatermarkLogoPath *string `json:"defaultWatermarkLogoPath,omitempty"`
	DefaultWatermarkLogoURL  *string `json:"defaultWatermarkLogoUrl,omitempty"`
	TurnExternalURL          *string `json:"turnExternalUrl,omitempty"`
	TurnExternalUsername     *string `json:"turnExternalUsername,omitempty"`
	HasTurnCredential        bool    `json:"hasTurnCredential"`
	TurnMode                 string  `json:"turnMode"`
	TurnCloudflareConfigured bool    `json:"turnCloudflareConfigured"`
	// Informational fields (read-only)
	PublicURL  string `json:"publicUrl"`
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

	// TURN fields come from the same effective-settings calculation that the
	// SFU runtime and the setup status use, so the displayed config matches
	// the tested config and the running config.
	turn := effectiveTURNSettingsFor(h.db, h.cfg)
	if len(turn.URLs) > 0 {
		joined := strings.Join(turn.URLs, ",")
		response.TurnExternalURL = &joined
	}
	if turn.Username != "" {
		response.TurnExternalUsername = &turn.Username
	}
	response.HasTurnCredential = turn.HasCredential
	response.TurnMode = h.cfg.TurnMode
	response.TurnCloudflareConfigured = h.cfg.HasCloudflareTURN()

	// Generate logo URL if path exists
	if watermarkLogoPath != nil && *watermarkLogoPath != "" {
		logoURL := "/api/config/logo"
		response.DefaultWatermarkLogoURL = &logoURL
	}

	// Add informational fields
	response.PublicURL = h.cfg.PublicURL
	response.WHIPFormat = h.cfg.PublicURL + "/whip/{stream_key_token}"

	respondJSON(w, response)
}

// UpdateConfigRequest is the request structure for updating config
type UpdateConfigRequest struct {
	DefaultWatermarkText   *string `json:"defaultWatermarkText"`
	TurnExternalURL        *string `json:"turnExternalUrl"`
	TurnExternalUsername   *string `json:"turnExternalUsername"`
	TurnExternalCredential *string `json:"turnExternalCredential"`
}

// Update updates the application configuration.
//
// Pointer semantics are preserved: omitted JSON fields leave DB values
// unchanged; provided empty strings write empty strings. Every write happens in
// a single transaction so a failure rolls the whole update back, and any change
// to a TURN field clears the stored reachability test (it was run against the
// previous configuration).
func (h *ConfigHandler) Update(w http.ResponseWriter, r *http.Request) {
	var req UpdateConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Validate TURN URLs when provided. Empty string means "clear the DB static
	// URL override" and is allowed; non-empty values must each parse as a
	// turn:/turns: URL with a non-empty host.
	if req.TurnExternalURL != nil {
		for _, entry := range splitTURNURLList(*req.TurnExternalURL) {
			if _, _, err := parseTURNURL(entry); err != nil {
				http.Error(w, fmt.Sprintf("Invalid TURN URL: %s", err), http.StatusBadRequest)
				return
			}
		}
	}

	// Ensure config row exists (upsert pattern)
	if _, err := h.db.Exec(`INSERT OR IGNORE INTO config (id) VALUES (1)`); err != nil {
		logger.Error("Failed to ensure config exists", "error", err)
		http.Error(w, "Failed to update configuration", http.StatusInternalServerError)
		return
	}

	tx, err := h.db.Begin()
	if err != nil {
		logger.Error("Failed to begin config transaction", "error", err)
		http.Error(w, "Failed to update configuration", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback() // no-op after Commit

	if req.DefaultWatermarkText != nil {
		if _, err := tx.Exec(`UPDATE config SET default_watermark_text = ? WHERE id = 1`, *req.DefaultWatermarkText); err != nil {
			logger.Error("Failed to update watermark text", "error", err)
			http.Error(w, "Failed to update watermark settings", http.StatusInternalServerError)
			return
		}
	}

	if req.TurnExternalURL != nil {
		if _, err := tx.Exec(`UPDATE config SET turn_external_url = ? WHERE id = 1`, *req.TurnExternalURL); err != nil {
			logger.Error("Failed to update TURN URL", "error", err)
			http.Error(w, "Failed to update TURN settings", http.StatusInternalServerError)
			return
		}
	}

	if req.TurnExternalUsername != nil {
		if _, err := tx.Exec(`UPDATE config SET turn_external_username = ? WHERE id = 1`, *req.TurnExternalUsername); err != nil {
			logger.Error("Failed to update TURN username", "error", err)
			http.Error(w, "Failed to update TURN settings", http.StatusInternalServerError)
			return
		}
	}

	if req.TurnExternalCredential != nil {
		if _, err := tx.Exec(`UPDATE config SET turn_external_credential = ? WHERE id = 1`, *req.TurnExternalCredential); err != nil {
			logger.Error("Failed to update TURN credential", "error", err)
			http.Error(w, "Failed to update TURN settings", http.StatusInternalServerError)
			return
		}
	}

	// Any TURN field change invalidates the last stored reachability test —
	// it was run against the previous effective configuration.
	if req.TurnExternalURL != nil || req.TurnExternalUsername != nil || req.TurnExternalCredential != nil {
		if _, err := tx.Exec(`
			UPDATE config
			SET turn_last_tested_at = NULL,
			    turn_last_test_success = 0,
			    turn_last_test_message = NULL,
			    turn_last_test_signature = NULL
			WHERE id = 1
		`); err != nil {
			logger.Error("Failed to clear stale TURN test", "error", err)
			http.Error(w, "Failed to update TURN settings", http.StatusInternalServerError)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		logger.Error("Failed to commit config update", "error", err)
		http.Error(w, "Failed to update configuration", http.StatusInternalServerError)
		return
	}

	// Return updated config
	h.syncRuntimeTURNFromDB()
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

	file, _, err := r.FormFile("logo")
	if err != nil {
		http.Error(w, "Missing logo file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Validate file type from content, not from the user-supplied filename.
	mimeType, err := detectFileContentType(file)
	if err != nil {
		http.Error(w, "Failed to read file", http.StatusBadRequest)
		return
	}

	if !allowedLogoMIMETypes[mimeType] {
		http.Error(w, "Invalid file type. Use PNG, JPEG, or WebP.", http.StatusBadRequest)
		return
	}
	if err := validateImageDimensions(file, mimeType, maxImagePixels); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Create logos directory if it doesn't exist
	if err := os.MkdirAll(h.cfg.LogoPath, 0755); err != nil {
		logger.Error("Failed to create logo directory", "path", h.cfg.LogoPath, "error", err)
		http.Error(w, "Failed to create logo directory", http.StatusInternalServerError)
		return
	}

	// Save logo with fixed name (overwrite previous)
	ext := getExtensionForMIME(mimeType)
	logoPath := filepath.Join(h.cfg.LogoPath, "default_watermark"+ext)

	// Read all file content
	if _, err := file.Seek(0, 0); err != nil {
		http.Error(w, "Failed to read file", http.StatusInternalServerError)
		return
	}
	fileContent, err := io.ReadAll(file)
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

	// Containment check: the logo path comes from the database and must
	// resolve inside the configured logo root.
	if !isPathWithin(h.cfg.LogoPath, *logoPath) {
		logger.Warn("Blocked logo outside logo root", "path", *logoPath)
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	// Check if file exists
	if _, err := os.Stat(*logoPath); os.IsNotExist(err) {
		http.Error(w, "Logo file not found", http.StatusNotFound)
		return
	}

	mimeType, err := detectPathContentType(*logoPath)
	if err != nil || !allowedLogoMIMETypes[mimeType] {
		logger.Warn("Blocked invalid logo content", "path", *logoPath, "mime_type", mimeType, "error", err)
		http.Error(w, "Invalid logo file", http.StatusUnsupportedMediaType)
		return
	}

	w.Header().Set("Content-Type", mimeType)
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

	// Containment check (mirrors GetLogo / DeleteFile): the path comes from the
	// DB and must resolve inside the configured logo root before we unlink it.
	// Without this, a logo_path that ever pointed outside the root (a buggy
	// write, a future code path, or a tampered DB) would let DeleteLogo remove
	// an arbitrary file.
	if !isPathWithin(h.cfg.LogoPath, *logoPath) {
		logger.Warn("Blocked logo delete outside logo root", "path", *logoPath)
		http.Error(w, "Forbidden", http.StatusForbidden)
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
	Server    string `json:"server"`
	Reachable bool   `json:"reachable"`
	Latency   int64  `json:"latency,omitempty"` // milliseconds
	Error     string `json:"error,omitempty"`
	Protocol  string `json:"protocol,omitempty"`
	TestType  string `json:"testType"` // "self-hosted" or "external"
}

// TURNTestResponse represents the full TURN test response
type TURNTestResponse struct {
	Success bool             `json:"success"`
	Results []TURNTestResult `json:"results"`
	Message string           `json:"message,omitempty"`
}

// TestTURN tests reachability of the configured TURN servers from this Chromatic
// server. It is a server-side socket test, not an authenticated TURN allocation
// or a browser NAT-traversal proof; the UI labels it as such.
func (h *ConfigHandler) TestTURN(w http.ResponseWriter, r *http.Request) {
	results := []TURNTestResult{}

	// Test self-hosted TURN server (Coturn)
	if h.cfg.TurnMode != config.TurnModeExternal && h.cfg.TurnRealm != "" {
		turnHost := h.cfg.TurnRealm
		if !strings.Contains(turnHost, ":") {
			turnHost = turnHost + ":3478"
		}

		results = append(results, testTURNServer(turnHost, "udp", "self-hosted"))
		results = append(results, testTURNServer(turnHost, "tcp", "self-hosted"))
	}

	// Test external/static TURN from the same effective settings the runtime
	// and /api/config expose, so the tested configuration matches the visible
	// configuration.
	if h.cfg.TurnMode != config.TurnModeSelfHosted {
		for _, external := range effectiveTURNSettingsFor(h.db, h.cfg).URLs {
			host, protocol, err := parseTURNURL(external)
			if err != nil {
				results = append(results, TURNTestResult{
					Server:    external,
					Reachable: false,
					Error:     fmt.Sprintf("Invalid TURN URL: %v", err),
					TestType:  "external",
				})
				continue
			}
			results = append(results, testTURNServer(host, protocol, "external"))
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
		response.Message = "At least one TURN endpoint is reachable from this server"
	} else {
		response.Message = "No TURN endpoints are reachable from this server"
	}

	// Persist the result with the current effective TURN signature so the setup
	// status can tell whether a stored test is still valid for this config.
	if _, err := h.db.Exec(`INSERT OR IGNORE INTO config (id) VALUES (1)`); err != nil {
		logger.Error("Failed to ensure config exists", "error", err)
	} else if _, err := h.db.Exec(`
		UPDATE config
		SET turn_last_tested_at = CURRENT_TIMESTAMP,
		    turn_last_test_success = ?,
		    turn_last_test_message = ?,
		    turn_last_test_signature = ?
		WHERE id = 1
	`, success, response.Message, currentTURNSignatureFor(h.db, h.cfg)); err != nil {
		logger.Error("Failed to persist TURN test result", "error", err)
	}

	respondJSON(w, response)
}

// syncRuntimeTURNFromDB pushes the effective external TURN settings (DB override
// with per-field environment fallback) into the SFU runtime. Per-field fallback
// fixes the partial-save clobber: saving only a username no longer erases the
// environment URL that the displayed config still shows.
func (h *ConfigHandler) syncRuntimeTURNFromDB() {
	if h.sfu == nil {
		return
	}
	turn := effectiveTURNSettingsFor(h.db, h.cfg)
	h.sfu.SetExternalTURNConfig(strings.Join(turn.URLs, ","), turn.Username, turn.Credential)
}

// effectiveTURNSettings is the resolved external/static TURN configuration after
// merging DB overrides with environment fallbacks.
type effectiveTURNSettings struct {
	URLs          []string
	Username      string
	Credential    string
	HasCredential bool
	// Source is "database" when any DB override is present, "environment" when
	// the values come from the process environment, or "none".
	Source string
}

// effectiveTURNSettingsFor resolves the effective external/static TURN settings.
//
// Each field falls back to the environment independently: a DB override for the
// username leaves the environment URL intact, and a DB override for the URL
// leaves the environment credential intact. A NULL DB column means "no
// override"; an empty-string DB column means "explicitly cleared" and is used
// as-is. This matches what Get displays and what the SFU runtime receives.
func effectiveTURNSettingsFor(db *database.DB, cfg *config.Config) effectiveTURNSettings {
	var dbURL, dbUser, dbCred *string
	// A missing config row is fine — every column stays nil and we fall back to
	// the environment.
	_ = db.QueryRow(`
		SELECT turn_external_url, turn_external_username, turn_external_credential
		FROM config WHERE id = 1
	`).Scan(&dbURL, &dbUser, &dbCred)

	envURL := ""
	if len(cfg.TurnExternalURLs) > 0 {
		envURL = strings.Join(cfg.TurnExternalURLs, ",")
	} else {
		envURL = cfg.TurnExternalURL
	}
	envUser := cfg.TurnExternalUser
	envCredential := cfg.TurnExternalPass

	s := effectiveTURNSettings{}
	if dbURL != nil {
		s.URLs = splitTURNURLList(*dbURL)
	} else {
		s.URLs = splitTURNURLList(envURL)
	}
	if dbUser != nil {
		s.Username = *dbUser
	} else {
		s.Username = envUser
	}
	if dbCred != nil {
		s.Credential = *dbCred
	} else {
		s.Credential = envCredential
	}
	s.HasCredential = s.Credential != ""

	switch {
	case dbURL != nil || dbUser != nil || dbCred != nil:
		s.Source = "database"
	case envURL != "" || envUser != "" || envCredential != "":
		s.Source = "environment"
	default:
		s.Source = "none"
	}
	return s
}

// currentTURNSignatureFor returns a stable SHA-256 fingerprint of the inputs
// that determine whether a stored TURN reachability test is still valid. The
// credential itself is never stored — only its hash.
func currentTURNSignatureFor(db *database.DB, cfg *config.Config) string {
	turn := effectiveTURNSettingsFor(db, cfg)
	credentialHash := sha256.Sum256([]byte(turn.Credential))

	h := sha256.New()
	fmt.Fprintln(h, cfg.TurnMode)
	fmt.Fprintln(h, cfg.TurnRealm)
	fmt.Fprintln(h, cfg.HasCloudflareTURN())
	fmt.Fprintln(h, strings.Join(turn.URLs, ","))
	fmt.Fprintln(h, turn.Username)
	fmt.Fprintln(h, hex.EncodeToString(credentialHash[:]))
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:])
}

func splitTURNURLList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		value := strings.TrimSpace(p)
		if value == "" {
			continue
		}
		result = append(result, value)
	}
	return result
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

// parseTURNURL parses a TURN URL and returns host:port and protocol.
//
// The URL must use the turn: or turns: scheme with a non-empty host. The
// optional ?transport= query selects udp (default for turn:), tcp (default for
// turns:), or tls; any other value is rejected.
func parseTURNURL(turnURL string) (host string, protocol string, err error) {
	raw := strings.TrimSpace(turnURL)
	if raw == "" {
		return "", "", fmt.Errorf("empty TURN URL")
	}

	isTLS := strings.HasPrefix(raw, "turns:")
	isTURN := strings.HasPrefix(raw, "turn:")
	if !isTURN && !isTLS {
		return "", "", fmt.Errorf("TURN URL must use the turn: or turns: scheme: %q", turnURL)
	}

	rest := strings.TrimPrefix(raw, "turn:")
	rest = strings.TrimPrefix(rest, "turns:")
	rest = strings.TrimPrefix(rest, "//")

	protocol = "udp" // default
	if isTLS {
		protocol = "tcp"
	}
	if i := strings.Index(rest, "?"); i >= 0 {
		query, qErr := url.ParseQuery(rest[i+1:])
		rest = rest[:i]
		if qErr == nil {
			if transport := query.Get("transport"); transport != "" {
				protocol = strings.ToLower(transport)
			}
		}
	}
	switch protocol {
	case "udp", "tcp", "tls":
	default:
		return "", "", fmt.Errorf("unsupported transport %q in TURN URL %q", protocol, turnURL)
	}

	host = rest
	if host == "" {
		return "", "", fmt.Errorf("TURN URL missing host: %q", turnURL)
	}
	if !strings.Contains(host, ":") {
		// Add default port based on protocol
		if protocol == "tcp" || protocol == "tls" {
			host = host + ":443"
		} else {
			host = host + ":3478"
		}
	}

	hostOnly := host
	if i := strings.LastIndex(host, ":"); i >= 0 {
		hostOnly = host[:i]
	}
	if hostOnly == "" {
		return "", "", fmt.Errorf("TURN URL missing host: %q", turnURL)
	}

	return host, protocol, nil
}
