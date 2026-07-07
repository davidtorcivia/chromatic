package handlers

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/url"

	"chromatic/internal/config"
	"chromatic/internal/database"
	"chromatic/internal/logger"
)

// SetupCheckStatus is the backend-computed state of a single setup check.
type SetupCheckStatus string

const (
	SetupCheckReady       SetupCheckStatus = "ready"
	SetupCheckNeedsAction SetupCheckStatus = "needs-action"
	SetupCheckWarning     SetupCheckStatus = "warning"
	SetupCheckOptional    SetupCheckStatus = "optional"
)

// SetupCheck is one item in the setup status report.
type SetupCheck struct {
	ID       string           `json:"id"`
	Title    string           `json:"title"`
	Status   SetupCheckStatus `json:"status"`
	Required bool             `json:"required"`
	Summary  string           `json:"summary"`
	Detail   string           `json:"detail,omitempty"`
	Action   string           `json:"action,omitempty"`
}

// SetupProgress is the rollup of required-check readiness.
type SetupProgress struct {
	Ready    int `json:"ready"`
	Required int `json:"required"`
	Total    int `json:"total"`
}

// SetupFacts carries the install facts Chromatic can compute from its own
// config and database, surfaced verbatim to the UI.
type SetupFacts struct {
	PublicURL                         string   `json:"publicUrl"`
	ProductionMode                    bool     `json:"productionMode"`
	AllowedOrigins                    []string `json:"allowedOrigins"`
	TurnMode                          string   `json:"turnMode"`
	TurnCloudflareConfigured          bool     `json:"turnCloudflareConfigured"`
	TurnStaticConfigured              bool     `json:"turnStaticConfigured"`
	HasTurnCredential                 bool     `json:"hasTurnCredential"`
	TurnLastTestedAt                  *string  `json:"turnLastTestedAt,omitempty"`
	TurnLastTestSuccess               bool     `json:"turnLastTestSuccess"`
	TurnLastTestMessage               string   `json:"turnLastTestMessage,omitempty"`
	TurnLastTestValidForCurrentConfig bool     `json:"turnLastTestValidForCurrentConfig"`
	StreamKeyCount                    int      `json:"streamKeyCount"`
	FirstStreamKeyID                  *string  `json:"firstStreamKeyId,omitempty"`
	RoomCount                         int      `json:"roomCount"`
	FirstRoomSlug                     *string  `json:"firstRoomSlug,omitempty"`
	DefaultWatermarkText              *string  `json:"defaultWatermarkText,omitempty"`
	DefaultWatermarkLogoURL           *string  `json:"defaultWatermarkLogoUrl,omitempty"`
}

// SetupStatusResponse is the full server-owned setup status.
type SetupStatusResponse struct {
	CompletedAt       *string       `json:"completedAt,omitempty"`
	DismissedAt       *string       `json:"dismissedAt,omitempty"`
	ReadyToComplete   bool          `json:"readyToComplete"`
	FirstRun          bool          `json:"firstRun"`
	RequiresAttention bool          `json:"requiresAttention"`
	Progress          SetupProgress `json:"progress"`
	Checks            []SetupCheck  `json:"checks"`
	Facts             SetupFacts    `json:"facts"`
}

// SetupHandler owns the server-backed setup status, completion, and dismissal.
type SetupHandler struct {
	db  *database.DB
	cfg *config.Config
}

// NewSetupHandler creates a new SetupHandler.
func NewSetupHandler(db *database.DB, cfg *config.Config) *SetupHandler {
	return &SetupHandler{db: db, cfg: cfg}
}

// Status returns the current server-computed setup status.
func (h *SetupHandler) Status(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, h.buildStatus())
}

// Complete marks setup complete. It recomputes status first: if any required
// check is not ready it returns 409 Conflict with the current status, otherwise
// it stamps setup_completed_at (clearing any prior dismissal) and returns the
// updated status.
func (h *SetupHandler) Complete(w http.ResponseWriter, r *http.Request) {
	status := h.buildStatus()
	if !status.ReadyToComplete {
		w.WriteHeader(http.StatusConflict)
		respondJSON(w, status)
		return
	}

	if _, err := h.db.Exec(`INSERT OR IGNORE INTO config (id) VALUES (1)`); err != nil {
		logger.Error("Failed to ensure config exists", "error", err)
		http.Error(w, "Failed to complete setup", http.StatusInternalServerError)
		return
	}
	if _, err := h.db.Exec(`UPDATE config SET setup_completed_at = CURRENT_TIMESTAMP, setup_dismissed_at = NULL WHERE id = 1`); err != nil {
		logger.Error("Failed to mark setup complete", "error", err)
		http.Error(w, "Failed to complete setup", http.StatusInternalServerError)
		return
	}

	respondJSON(w, h.buildStatus())
}

// Dismiss records that the admin dismissed the setup banner without completing
// it, then returns the updated status.
func (h *SetupHandler) Dismiss(w http.ResponseWriter, r *http.Request) {
	if _, err := h.db.Exec(`INSERT OR IGNORE INTO config (id) VALUES (1)`); err != nil {
		logger.Error("Failed to ensure config exists", "error", err)
		http.Error(w, "Failed to dismiss setup", http.StatusInternalServerError)
		return
	}
	if _, err := h.db.Exec(`UPDATE config SET setup_dismissed_at = CURRENT_TIMESTAMP WHERE id = 1`); err != nil {
		logger.Error("Failed to dismiss setup", "error", err)
		http.Error(w, "Failed to dismiss setup", http.StatusInternalServerError)
		return
	}

	respondJSON(w, h.buildStatus())
}

// buildStatus computes the full SetupStatusResponse from the live config and DB.
func (h *SetupHandler) buildStatus() SetupStatusResponse {
	cfg := h.cfg
	facts := SetupFacts{
		PublicURL:                cfg.PublicURL,
		ProductionMode:           cfg.ProductionMode,
		AllowedOrigins:           append([]string(nil), cfg.AllowedOrigins...),
		TurnMode:                 cfg.TurnMode,
		TurnCloudflareConfigured: cfg.HasCloudflareTURN(),
	}

	turn := effectiveTURNSettingsFor(h.db, cfg)
	facts.TurnStaticConfigured = len(turn.URLs) > 0
	facts.HasTurnCredential = turn.HasCredential

	var checks []SetupCheck

	// 1. public-url (required)
	checks = append(checks, publicURLCheck(cfg.PublicURL))

	// 2. security (required)
	checks = append(checks, securityCheck(cfg))

	// 3. turn-config (required)
	tcCheck := turnConfigCheck(cfg, turn)
	checks = append(checks, tcCheck)

	// 4. turn-connectivity (required) — also populates last-test facts.
	connCheck := h.turnConnectivityCheck(&facts)
	checks = append(checks, connCheck)

	// 5. stream-key (required)
	keyCheck := h.streamKeyCheck(&facts)
	checks = append(checks, keyCheck)

	// 6. room (required)
	roomCheck := h.roomCheck(&facts)
	checks = append(checks, roomCheck)

	// 7. branding (optional)
	brandCheck := h.brandingCheck(&facts)
	checks = append(checks, brandCheck)

	// Progress rollup over required checks only.
	progress := SetupProgress{Total: len(checks)}
	for _, c := range checks {
		if c.Required {
			progress.Required++
			if c.Status == SetupCheckReady {
				progress.Ready++
			}
		}
	}

	readyToComplete := progress.Required > 0 && progress.Ready == progress.Required

	var completedAt, dismissedAt sql.NullString
	_ = h.db.QueryRow(`SELECT setup_completed_at, setup_dismissed_at FROM config WHERE id = 1`).Scan(&completedAt, &dismissedAt)

	resp := SetupStatusResponse{
		ReadyToComplete: readyToComplete,
		Progress:        progress,
		Checks:          checks,
		Facts:           facts,
	}
	if completedAt.Valid {
		s := completedAt.String
		resp.CompletedAt = &s
	}
	if dismissedAt.Valid {
		s := dismissedAt.String
		resp.DismissedAt = &s
	}
	resp.FirstRun = resp.CompletedAt == nil && resp.DismissedAt == nil && facts.StreamKeyCount == 0 && facts.RoomCount == 0
	resp.RequiresAttention = resp.CompletedAt != nil && !readyToComplete

	return resp
}

// publicURLCheck implements the public-url required check.
//
// Ready when PUBLIC_URL is a local dev URL (localhost/127.0.0.1/[::1]) or a
// non-local HTTPS URL. Needs action when empty, unparsable, missing a scheme,
// or non-local HTTP.
func publicURLCheck(publicURL string) SetupCheck {
	c := SetupCheck{ID: "public-url", Title: "Public URL", Required: true, Status: SetupCheckNeedsAction}

	if publicURL == "" {
		c.Summary = "PUBLIC_URL is not set"
		c.Detail = "Set PUBLIC_URL in your environment and restart Chromatic."
		return c
	}

	parsed, err := url.Parse(publicURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		c.Summary = "Public URL is unparsable"
		c.Detail = publicURL
		c.Action = "Set PUBLIC_URL to a full http(s):// URL and restart Chromatic."
		return c
	}

	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		c.Summary = "Public URL scheme is not http(s)"
		c.Detail = publicURL
		c.Action = "Set PUBLIC_URL to an http(s):// URL and restart Chromatic."
		return c
	}

	if isLocalDevURL(publicURL) {
		c.Status = SetupCheckReady
		c.Summary = "Local development URL"
		c.Detail = publicURL
		return c
	}

	if parsed.Scheme == "https" {
		c.Status = SetupCheckReady
		c.Summary = "HTTPS public URL configured"
		c.Detail = publicURL
		return c
	}

	// Non-local HTTP.
	c.Summary = "Public URL is not HTTPS"
	c.Detail = publicURL
	c.Action = "Set PUBLIC_URL to an https:// URL and restart Chromatic."
	return c
}

// securityCheck implements the security required check.
func securityCheck(cfg *config.Config) SetupCheck {
	c := SetupCheck{ID: "security", Title: "Security origins", Required: true, Status: SetupCheckNeedsAction}

	origin := ""
	if parsed, err := url.Parse(cfg.PublicURL); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		origin = parsed.Scheme + "://" + parsed.Host
	}
	local := isLocalDevURL(cfg.PublicURL)

	if local && !cfg.ProductionMode {
		c.Status = SetupCheckReady
		c.Summary = "Local development"
		c.Detail = "Production mode is off; local origin is allowed."
		return c
	}

	if !cfg.ProductionMode {
		// Non-local URL without production mode.
		c.Summary = "Production mode is off for a non-local URL"
		c.Detail = origin
		c.Action = "Enable PRODUCTION_MODE and set ALLOWED_ORIGINS to include " + origin + "."
		return c
	}

	if len(cfg.AllowedOrigins) == 0 {
		c.Summary = "ALLOWED_ORIGINS is empty"
		c.Detail = origin
		c.Action = "Set ALLOWED_ORIGINS to include " + origin + " and restart Chromatic."
		return c
	}

	if origin != "" && !containsString(cfg.AllowedOrigins, origin) {
		c.Summary = "Public origin missing from ALLOWED_ORIGINS"
		c.Detail = origin
		c.Action = "Set ALLOWED_ORIGINS to include " + origin + " and restart Chromatic."
		return c
	}

	c.Status = SetupCheckReady
	c.Summary = "Public origin allowed"
	c.Detail = origin
	return c
}

// turnConfigCheck implements the turn-config required check.
func turnConfigCheck(cfg *config.Config, turn effectiveTURNSettings) SetupCheck {
	c := SetupCheck{ID: "turn-config", Title: "TURN configuration", Required: true, Status: SetupCheckNeedsAction, Summary: "TURN configuration missing"}

	mode := cfg.TurnMode
	selfHostedOK := (mode == config.TurnModeSelfHosted || mode == config.TurnModeHybrid) &&
		cfg.TurnRealm != "" && cfg.TurnSecret != ""

	externalOK := false
	externalSummary := ""
	if mode == config.TurnModeExternal || mode == config.TurnModeHybrid {
		switch {
		case cfg.HasCloudflareTURN():
			externalOK = true
			externalSummary = "Cloudflare TURN configured"
		case len(turn.URLs) > 0 && turn.Username != "" && turn.HasCredential:
			externalOK = true
			externalSummary = "Static external TURN configured"
		}
	}

	switch mode {
	case config.TurnModeSelfHosted:
		if selfHostedOK {
			c.Status = SetupCheckReady
			c.Summary = "Self-hosted TURN configured"
		}
	case config.TurnModeExternal, config.TurnModeHybrid:
		if externalOK {
			c.Status = SetupCheckReady
			c.Summary = externalSummary
		} else if mode == config.TurnModeHybrid && selfHostedOK {
			c.Status = SetupCheckReady
			c.Summary = "Self-hosted TURN configured"
		}
	}

	return c
}

// turnConnectivityCheck implements the turn-connectivity required check and
// populates the last-test facts on the report.
func (h *SetupHandler) turnConnectivityCheck(facts *SetupFacts) SetupCheck {
	c := SetupCheck{ID: "turn-connectivity", Title: "TURN reachability", Required: true, Status: SetupCheckNeedsAction}

	var testedAt, lastMsg, lastSig sql.NullString
	var lastSuccess sql.NullBool
	_ = h.db.QueryRow(`
		SELECT turn_last_tested_at, turn_last_test_success, turn_last_test_message, turn_last_test_signature
		FROM config WHERE id = 1
	`).Scan(&testedAt, &lastSuccess, &lastMsg, &lastSig)

	currentSig := currentTURNSignatureFor(h.db, h.cfg)
	validForCurrent := testedAt.Valid && lastSig.Valid && lastSig.String == currentSig

	if testedAt.Valid {
		s := testedAt.String
		facts.TurnLastTestedAt = &s
	}
	facts.TurnLastTestSuccess = lastSuccess.Bool
	facts.TurnLastTestMessage = lastMsg.String
	facts.TurnLastTestValidForCurrentConfig = validForCurrent

	// This is a server-side reachability test only. Do not imply browser NAT
	// traversal or authenticated allocation was proven.
	switch {
	case validForCurrent && lastSuccess.Bool:
		c.Status = SetupCheckReady
		c.Summary = "Server reachability test passed"
	case validForCurrent && !lastSuccess.Bool:
		c.Status = SetupCheckNeedsAction
		c.Summary = "TURN reachability test failed"
		c.Detail = lastMsg.String
		c.Action = "Re-run the TURN reachability test after fixing the server."
	default:
		c.Status = SetupCheckNeedsAction
		c.Summary = "Run the TURN reachability test"
		c.Action = "Run the TURN reachability test to confirm the server can reach TURN."
	}

	return c
}

// streamKeyCheck implements the stream-key required check.
func (h *SetupHandler) streamKeyCheck(facts *SetupFacts) SetupCheck {
	c := SetupCheck{ID: "stream-key", Title: "Stream key", Required: true, Status: SetupCheckNeedsAction}

	var count int
	_ = h.db.QueryRow(`SELECT COUNT(*) FROM stream_keys`).Scan(&count)
	facts.StreamKeyCount = count

	var firstID sql.NullString
	_ = h.db.QueryRow(`SELECT id FROM stream_keys ORDER BY created_at DESC LIMIT 1`).Scan(&firstID)

	if count > 0 {
		c.Status = SetupCheckReady
		c.Summary = fmt.Sprintf("%d stream key(s) configured", count)
		if firstID.Valid {
			s := firstID.String
			facts.FirstStreamKeyID = &s
		}
	} else {
		c.Summary = "No stream key yet"
		c.Action = "Create a stream key for OBS."
	}

	return c
}

// roomCheck implements the room required check.
func (h *SetupHandler) roomCheck(facts *SetupFacts) SetupCheck {
	c := SetupCheck{ID: "room", Title: "First room", Required: true, Status: SetupCheckNeedsAction}

	var count int
	_ = h.db.QueryRow(`SELECT COUNT(*) FROM rooms`).Scan(&count)
	facts.RoomCount = count

	var firstSlug sql.NullString
	_ = h.db.QueryRow(`SELECT slug FROM rooms ORDER BY created_at DESC LIMIT 1`).Scan(&firstSlug)

	if count > 0 {
		c.Status = SetupCheckReady
		c.Summary = fmt.Sprintf("%d room(s) configured", count)
		if firstSlug.Valid {
			s := firstSlug.String
			facts.FirstRoomSlug = &s
		}
	} else {
		c.Summary = "No room yet"
		c.Action = "Create the first viewer room."
	}

	return c
}

// brandingCheck implements the optional branding check.
func (h *SetupHandler) brandingCheck(facts *SetupFacts) SetupCheck {
	c := SetupCheck{ID: "branding", Title: "Default branding", Required: false, Status: SetupCheckOptional, Summary: "Default branding can be set later."}

	var wmText, wmLogoPath sql.NullString
	_ = h.db.QueryRow(`SELECT default_watermark_text, default_watermark_logo_path FROM config WHERE id = 1`).Scan(&wmText, &wmLogoPath)

	if wmText.Valid && wmText.String != "" {
		s := wmText.String
		facts.DefaultWatermarkText = &s
	}
	if wmLogoPath.Valid && wmLogoPath.String != "" {
		c.Status = SetupCheckReady
		c.Summary = "Default branding set"
		logoURL := "/api/config/logo"
		facts.DefaultWatermarkLogoURL = &logoURL
	} else if wmText.Valid && wmText.String != "" {
		c.Status = SetupCheckReady
		c.Summary = "Default branding set"
	}

	return c
}

// isLocalDevURL reports whether raw is a localhost/loopback URL.
func isLocalDevURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// containsString reports whether items contains target. (A tiny local helper so
// config.go and setup.go do not share a name and each stays self-contained.)
func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

