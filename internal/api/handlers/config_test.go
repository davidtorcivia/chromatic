package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"chromatic/internal/config"
	"chromatic/internal/database"
)

func setupConfigTest(t *testing.T) (*ConfigHandler, func()) {
	db, dbCleanup := database.NewTestDB(t)

	// Create config table
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS config (
			id INTEGER PRIMARY KEY,
			default_watermark_text TEXT,
			default_watermark_logo_path TEXT,
			turn_external_url TEXT,
			turn_external_username TEXT,
			turn_external_credential TEXT
		)
	`)
	if err != nil {
		t.Fatalf("failed to create config table: %v", err)
	}

	// Create temp dir for logos
	tempDir, err := os.MkdirTemp("", "chromatic-config-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	cfg := &config.Config{
		PublicURL: "http://localhost:3000",
		LogoPath:  tempDir,
	}

	handler := NewConfigHandler(db, cfg, nil)

	cleanup := func() {
		dbCleanup()
		os.RemoveAll(tempDir)
	}

	return handler, cleanup
}

func TestConfigHandler_Get_Empty(t *testing.T) {
	handler, cleanup := setupConfigTest(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/api/config", nil)
	rr := httptest.NewRecorder()

	handler.Get(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, rr.Code)
	}

	var resp ConfigResponse
	json.NewDecoder(rr.Body).Decode(&resp)

	if resp.PublicURL != "http://localhost:3000" {
		t.Errorf("expected publicUrl 'http://localhost:3000', got %s", resp.PublicURL)
	}

	if resp.WHIPFormat != "http://localhost:3000/whip/{stream_key_token}" {
		t.Errorf("unexpected WHIP format: %s", resp.WHIPFormat)
	}
}

func TestConfigHandler_Get_UsesEnvTURNFallback(t *testing.T) {
	handler, cleanup := setupConfigTest(t)
	defer cleanup()

	handler.cfg.TurnExternalURL = "turn:external.example.com:3478?transport=udp"
	handler.cfg.TurnExternalUser = "fallback-user"
	handler.cfg.TurnExternalPass = "fallback-pass"

	req := httptest.NewRequest("GET", "/api/config", nil)
	rr := httptest.NewRecorder()
	handler.Get(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rr.Code)
	}

	var resp ConfigResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.TurnExternalURL == nil || *resp.TurnExternalURL != handler.cfg.TurnExternalURL {
		t.Fatalf("expected fallback TURN URL %q, got %v", handler.cfg.TurnExternalURL, resp.TurnExternalURL)
	}
	if resp.TurnExternalUsername == nil || *resp.TurnExternalUsername != handler.cfg.TurnExternalUser {
		t.Fatalf("expected fallback TURN username %q, got %v", handler.cfg.TurnExternalUser, resp.TurnExternalUsername)
	}
	if !resp.HasTurnCredential {
		t.Fatal("expected fallback TURN credential flag to be true")
	}
}

func TestConfigHandler_Update(t *testing.T) {
	handler, cleanup := setupConfigTest(t)
	defer cleanup()

	// Update watermark text
	watermarkText := "Test Watermark {{ name }}"
	body := map[string]interface{}{
		"defaultWatermarkText": watermarkText,
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest("PATCH", "/api/config", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.Update(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d: %s", http.StatusOK, rr.Code, rr.Body.String())
	}

	var resp ConfigResponse
	json.NewDecoder(rr.Body).Decode(&resp)

	if resp.DefaultWatermarkText == nil || *resp.DefaultWatermarkText != watermarkText {
		t.Errorf("expected watermark text %s, got %v", watermarkText, resp.DefaultWatermarkText)
	}
}

func TestConfigHandler_Update_TurnSettings(t *testing.T) {
	handler, cleanup := setupConfigTest(t)
	defer cleanup()

	turnURL := "turn:test.example.com:3478"
	turnUsername := "testuser"
	turnCredential := "testpass"

	body := map[string]interface{}{
		"turnExternalUrl":        turnURL,
		"turnExternalUsername":   turnUsername,
		"turnExternalCredential": turnCredential,
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest("PATCH", "/api/config", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.Update(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, rr.Code)
	}

	var resp ConfigResponse
	json.NewDecoder(rr.Body).Decode(&resp)

	if resp.TurnExternalURL == nil || *resp.TurnExternalURL != turnURL {
		t.Errorf("expected TURN URL %s, got %v", turnURL, resp.TurnExternalURL)
	}

	if resp.TurnExternalUsername == nil || *resp.TurnExternalUsername != turnUsername {
		t.Errorf("expected TURN username %s, got %v", turnUsername, resp.TurnExternalUsername)
	}

	if !resp.HasTurnCredential {
		t.Error("expected HasTurnCredential to be true")
	}
}

func TestConfigHandler_Update_InvalidBody(t *testing.T) {
	handler, cleanup := setupConfigTest(t)
	defer cleanup()

	req := httptest.NewRequest("PATCH", "/api/config", bytes.NewReader([]byte("invalid json")))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.Update(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, rr.Code)
	}
}

func TestConfigHandler_GetLogo_NotConfigured(t *testing.T) {
	handler, cleanup := setupConfigTest(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/api/config/logo", nil)
	rr := httptest.NewRecorder()

	handler.GetLogo(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected status %d, got %d", http.StatusNotFound, rr.Code)
	}
}

func TestConfigHandler_DeleteLogo_NotConfigured(t *testing.T) {
	handler, cleanup := setupConfigTest(t)
	defer cleanup()

	req := httptest.NewRequest("DELETE", "/api/config/logo", nil)
	rr := httptest.NewRecorder()

	handler.DeleteLogo(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected status %d, got %d", http.StatusNotFound, rr.Code)
	}
}

func TestConfigHandler_UploadLogo_MissingFile(t *testing.T) {
	handler, cleanup := setupConfigTest(t)
	defer cleanup()

	// Create empty multipart form
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	writer.Close()

	req := httptest.NewRequest("POST", "/api/config/logo", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()

	handler.UploadLogo(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d: %s", http.StatusBadRequest, rr.Code, rr.Body.String())
	}
}

func TestConfigHandler_UploadLogo_InvalidType(t *testing.T) {
	handler, cleanup := setupConfigTest(t)
	defer cleanup()

	// Create multipart form with text file
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, _ := writer.CreateFormFile("logo", "test.txt")
	part.Write([]byte("this is a text file not an image"))
	writer.Close()

	req := httptest.NewRequest("POST", "/api/config/logo", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()

	handler.UploadLogo(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d: %s", http.StatusBadRequest, rr.Code, rr.Body.String())
	}
}

func TestConfigHandler_UploadLogo_ValidPNG(t *testing.T) {
	handler, cleanup := setupConfigTest(t)
	defer cleanup()

	// Create minimal valid PNG (1x1 transparent pixel)
	pngData := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, // PNG signature
		0x00, 0x00, 0x00, 0x0D, // IHDR chunk length
		0x49, 0x48, 0x44, 0x52, // "IHDR"
		0x00, 0x00, 0x00, 0x01, // width = 1
		0x00, 0x00, 0x00, 0x01, // height = 1
		0x08, 0x06, // bit depth = 8, color type = 6 (RGBA)
		0x00, 0x00, 0x00, // compression, filter, interlace
		0x1F, 0x15, 0xC4, 0x89, // CRC
		0x00, 0x00, 0x00, 0x0A, // IDAT chunk length
		0x49, 0x44, 0x41, 0x54, // "IDAT"
		0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05, 0x00, 0x01, // compressed data
		0x0D, 0x0A, 0x2D, 0xB4, // CRC
		0x00, 0x00, 0x00, 0x00, // IEND chunk length
		0x49, 0x45, 0x4E, 0x44, // "IEND"
		0xAE, 0x42, 0x60, 0x82, // CRC
	}

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, _ := writer.CreateFormFile("logo", "watermark.html")
	part.Write(pngData)
	writer.Close()

	req := httptest.NewRequest("POST", "/api/config/logo", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()

	handler.UploadLogo(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d: %s", http.StatusOK, rr.Code, rr.Body.String())
	}

	var resp map[string]string
	json.NewDecoder(rr.Body).Decode(&resp)

	if resp["logoUrl"] != "/api/config/logo" {
		t.Errorf("expected logoUrl '/api/config/logo', got %s", resp["logoUrl"])
	}
	if filepath.Ext(resp["path"]) != ".png" {
		t.Errorf("expected stored logo extension '.png' from detected MIME, got %q", filepath.Ext(resp["path"]))
	}

	// Verify logo can be retrieved
	getReq := httptest.NewRequest("GET", "/api/config/logo", nil)
	getRR := httptest.NewRecorder()
	handler.GetLogo(getRR, getReq)

	if getRR.Code != http.StatusOK {
		t.Errorf("expected status %d for GetLogo, got %d", http.StatusOK, getRR.Code)
	}
	if getRR.Header().Get("Content-Type") != "image/png" {
		t.Errorf("expected logo Content-Type image/png, got %q", getRR.Header().Get("Content-Type"))
	}
}

func TestConfigHandler_DeleteLogo_AfterUpload(t *testing.T) {
	handler, cleanup := setupConfigTest(t)
	defer cleanup()

	// First upload a logo
	pngData := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D,
		0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01,
		0x00, 0x00, 0x00, 0x01,
		0x08, 0x06,
		0x00, 0x00, 0x00,
		0x1F, 0x15, 0xC4, 0x89,
		0x00, 0x00, 0x00, 0x0A,
		0x49, 0x44, 0x41, 0x54,
		0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05, 0x00, 0x01,
		0x0D, 0x0A, 0x2D, 0xB4,
		0x00, 0x00, 0x00, 0x00,
		0x49, 0x45, 0x4E, 0x44,
		0xAE, 0x42, 0x60, 0x82,
	}

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, _ := writer.CreateFormFile("logo", "test.png")
	part.Write(pngData)
	writer.Close()

	uploadReq := httptest.NewRequest("POST", "/api/config/logo", &buf)
	uploadReq.Header.Set("Content-Type", writer.FormDataContentType())
	uploadRR := httptest.NewRecorder()
	handler.UploadLogo(uploadRR, uploadReq)

	// Now delete it
	deleteReq := httptest.NewRequest("DELETE", "/api/config/logo", nil)
	deleteRR := httptest.NewRecorder()
	handler.DeleteLogo(deleteRR, deleteReq)

	if deleteRR.Code != http.StatusNoContent {
		t.Errorf("expected status %d, got %d", http.StatusNoContent, deleteRR.Code)
	}

	// Verify logo is gone
	getReq := httptest.NewRequest("GET", "/api/config/logo", nil)
	getRR := httptest.NewRecorder()
	handler.GetLogo(getRR, getReq)

	if getRR.Code != http.StatusNotFound {
		t.Errorf("expected status %d after delete, got %d", http.StatusNotFound, getRR.Code)
	}
}

// Note: parseTURNURL is tested indirectly through TestTURN handler tests

func TestConfigHandler_TestTURN_NoServers(t *testing.T) {
	handler, cleanup := setupConfigTest(t)
	defer cleanup()

	req := httptest.NewRequest("POST", "/api/config/test-turn", nil)
	rr := httptest.NewRecorder()

	handler.TestTURN(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, rr.Code)
	}

	var resp TURNTestResponse
	json.NewDecoder(rr.Body).Decode(&resp)

	if resp.Message != "No TURN servers configured" {
		t.Errorf("expected message 'No TURN servers configured', got %s", resp.Message)
	}
}

func TestConfigHandler_UploadLogo_FileTooLarge(t *testing.T) {
	handler, cleanup := setupConfigTest(t)
	defer cleanup()

	// Create a buffer larger than 1MB
	largeData := make([]byte, 2*1024*1024) // 2MB
	// Add PNG header to make it look like a valid image
	copy(largeData[:8], []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A})

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, _ := writer.CreateFormFile("logo", "large.png")
	part.Write(largeData)
	writer.Close()

	req := httptest.NewRequest("POST", "/api/config/logo", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()

	handler.UploadLogo(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected status %d for too large file, got %d", http.StatusBadRequest, rr.Code)
	}
}

func TestConfigHandler_UploadLogo_JPEG(t *testing.T) {
	handler, cleanup := setupConfigTest(t)
	defer cleanup()

	// Minimal JPEG (1x1 red pixel)
	jpegData := []byte{
		0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0x4A, 0x46, 0x49, 0x46, 0x00, 0x01,
		0x01, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0xFF, 0xDB, 0x00, 0x43,
		0x00, 0x08, 0x06, 0x06, 0x07, 0x06, 0x05, 0x08, 0x07, 0x07, 0x07, 0x09,
		0x09, 0x08, 0x0A, 0x0C, 0x14, 0x0D, 0x0C, 0x0B, 0x0B, 0x0C, 0x19, 0x12,
		0x13, 0x0F, 0x14, 0x1D, 0x1A, 0x1F, 0x1E, 0x1D, 0x1A, 0x1C, 0x1C, 0x20,
		0x24, 0x2E, 0x27, 0x20, 0x22, 0x2C, 0x23, 0x1C, 0x1C, 0x28, 0x37, 0x29,
		0x2C, 0x30, 0x31, 0x34, 0x34, 0x34, 0x1F, 0x27, 0x39, 0x3D, 0x38, 0x32,
		0x3C, 0x2E, 0x33, 0x34, 0x32, 0xFF, 0xC0, 0x00, 0x0B, 0x08, 0x00, 0x01,
		0x00, 0x01, 0x01, 0x01, 0x11, 0x00, 0xFF, 0xC4, 0x00, 0x1F, 0x00, 0x00,
		0x01, 0x05, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0A, 0x0B, 0xFF, 0xC4, 0x00, 0xB5, 0x10, 0x00, 0x02, 0x01, 0x03,
		0x03, 0x02, 0x04, 0x03, 0x05, 0x05, 0x04, 0x04, 0x00, 0x00, 0x01, 0x7D,
		0xFF, 0xD9,
	}

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, _ := writer.CreateFormFile("logo", "test.jpg")
	part.Write(jpegData)
	writer.Close()

	req := httptest.NewRequest("POST", "/api/config/logo", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()

	handler.UploadLogo(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d: %s", http.StatusOK, rr.Code, rr.Body.String())
	}
}

func TestConfigHandler_GetLogo_FileNotFound(t *testing.T) {
	handler, cleanup := setupConfigTest(t)
	defer cleanup()

	// Manually set a logo path (inside the logo root) that doesn't exist
	missingPath := filepath.Join(handler.cfg.LogoPath, "missing-logo.png")
	handler.db.Exec(`INSERT OR IGNORE INTO config (id) VALUES (1)`)
	handler.db.Exec(`UPDATE config SET default_watermark_logo_path = ? WHERE id = 1`, missingPath)

	req := httptest.NewRequest("GET", "/api/config/logo", nil)
	rr := httptest.NewRecorder()

	handler.GetLogo(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected status %d for missing file, got %d", http.StatusNotFound, rr.Code)
	}
}

func TestConfigHandler_GetLogo_PathOutsideLogoRoot(t *testing.T) {
	handler, cleanup := setupConfigTest(t)
	defer cleanup()

	// A logo path outside the configured logo root must be rejected even if
	// the file exists (e.g. tampered database)
	handler.db.Exec(`INSERT OR IGNORE INTO config (id) VALUES (1)`)
	handler.db.Exec(`UPDATE config SET default_watermark_logo_path = '/etc/passwd' WHERE id = 1`)

	req := httptest.NewRequest("GET", "/api/config/logo", nil)
	rr := httptest.NewRecorder()

	handler.GetLogo(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("expected status %d for path outside logo root, got %d", http.StatusForbidden, rr.Code)
	}
}

func TestParseTURNURL_TURNSSchemeDefaultsToTCP443(t *testing.T) {
	host, protocol, err := parseTURNURL("turns:turn.cloudflare.com")
	if err != nil {
		t.Fatalf("parseTURNURL returned error: %v", err)
	}

	if protocol != "tcp" {
		t.Fatalf("expected protocol tcp, got %s", protocol)
	}
	if host != "turn.cloudflare.com:443" {
		t.Fatalf("expected host turn.cloudflare.com:443, got %s", host)
	}
}

func init() {
	// Initialize logger for tests
	io.Discard.Write([]byte{})
}
