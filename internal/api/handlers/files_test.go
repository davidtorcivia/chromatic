package handlers

import (
	"bytes"
	"encoding/json"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chromatic/internal/config"
	"chromatic/internal/database"
)

var testTokenSecret = DeriveTokenSecret("test-admin-token")

func setupFileTest(t *testing.T) (*FileHandler, *RoomHandler, *database.DB, func()) {
	db, dbCleanup := database.NewTestDB(t)

	// Create temp directories for file storage
	uploadPath, err := os.MkdirTemp("", "chromatic-test-uploads")
	if err != nil {
		t.Fatalf("failed to create temp upload dir: %v", err)
	}

	cfg := &config.Config{
		UploadPath: uploadPath,
	}

	fileHandler := NewFileHandler(db, cfg, testTokenSecret)
	roomHandler := NewRoomHandler(db, cfg, testTokenSecret)

	cleanup := func() {
		dbCleanup()
		os.RemoveAll(uploadPath)
	}

	return fileHandler, roomHandler, db, cleanup
}

func createJoinToken(t *testing.T, participantID, roomSlug string) string {
	t.Helper()

	tokenManager := NewTokenManager(testTokenSecret)
	token, err := tokenManager.GenerateToken(participantID, roomSlug, "Test User", time.Hour)
	if err != nil {
		t.Fatalf("failed to create join token: %v", err)
	}
	return token
}

// createTestRoom creates a room and returns its ID
func createTestRoom(t *testing.T, handler *RoomHandler, db *database.DB, slug string) string {
	createBody := map[string]interface{}{
		"slug":          slug,
		"name":          "Test Room",
		"watermarkMode": "none",
	}
	bodyBytes, _ := json.Marshal(createBody)
	req := httptest.NewRequest("POST", "/api/rooms", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.Create(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("failed to create test room: %s", rr.Body.String())
	}

	// Get the room ID
	var roomID string
	err := db.QueryRow("SELECT id FROM rooms WHERE slug = ?", slug).Scan(&roomID)
	if err != nil {
		t.Fatalf("failed to get room ID: %v", err)
	}
	return roomID
}

// createTestParticipant creates a participant record for FK constraint satisfaction
func createTestParticipant(t *testing.T, db *database.DB, roomID, participantID string) {
	_, err := db.Exec(`
		INSERT INTO participants (id, room_id, name, role, color, is_admitted)
		VALUES (?, ?, 'Test User', 'viewer', '#FF0000', TRUE)
	`, participantID, roomID)
	if err != nil {
		t.Fatalf("failed to create test participant: %v", err)
	}
}

func createMultipartRequest(filename string, content []byte, contentType string) (*bytes.Buffer, string) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	part, _ := writer.CreateFormFile("file", filename)
	part.Write(content)
	writer.Close()

	return body, writer.FormDataContentType()
}

func TestFileHandler_Upload(t *testing.T) {
	fileHandler, roomHandler, db, cleanup := setupFileTest(t)
	defer cleanup()

	roomID := createTestRoom(t, roomHandler, db, "upload-test")

	// Create participants for each test case that uses a participant ID
	createTestParticipant(t, db, roomID, "user-123")
	createTestParticipant(t, db, roomID, "user-456")
	createTestParticipant(t, db, roomID, "user-789")
	createTestParticipant(t, db, roomID, "unknown") // For uploads without explicit participant ID

	tokens := map[string]string{
		"user-123": createJoinToken(t, "user-123", "upload-test"),
		"user-456": createJoinToken(t, "user-456", "upload-test"),
		"user-789": createJoinToken(t, "user-789", "upload-test"),
		"unknown":  createJoinToken(t, "unknown", "upload-test"),
	}

	tests := []struct {
		name           string
		filename       string
		content        []byte
		contentType    string
		participantID  string
		token          string
		expectedStatus int
		checkResponse  func(t *testing.T, body []byte)
	}{
		{
			name:           "upload valid PNG",
			filename:       "test.php",
			content:        createTestPNG(),
			contentType:    "image/png",
			participantID:  "user-123",
			token:          tokens["user-123"],
			expectedStatus: http.StatusCreated,
			checkResponse: func(t *testing.T, body []byte) {
				var resp map[string]interface{}
				json.Unmarshal(body, &resp)
				if resp["originalName"] != "test.php" {
					t.Errorf("expected originalName 'test.php', got %v", resp["originalName"])
				}
				if resp["mimeType"] != "image/png" {
					t.Errorf("expected mimeType 'image/png', got %v", resp["mimeType"])
				}
				if resp["url"] == nil {
					t.Error("expected url in response")
				}
				if resp["thumbnailUrl"] == nil {
					t.Error("expected thumbnailUrl for image")
				}
				var storedPath string
				if err := db.QueryRow("SELECT stored_path FROM files WHERE id = ?", resp["id"]).Scan(&storedPath); err != nil {
					t.Fatalf("failed to read stored path: %v", err)
				}
				if filepath.Ext(storedPath) != ".png" {
					t.Fatalf("stored extension = %q, want .png for detected MIME", filepath.Ext(storedPath))
				}
			},
		},
		{
			name:           "upload valid JPEG",
			filename:       "photo.jpg",
			content:        createTestJPEG(),
			contentType:    "image/jpeg",
			participantID:  "user-456",
			token:          tokens["user-456"],
			expectedStatus: http.StatusCreated,
		},
		{
			name:           "upload PDF",
			filename:       "document.pdf",
			content:        createTestPDF(),
			contentType:    "application/pdf",
			participantID:  "user-789",
			token:          tokens["user-789"],
			expectedStatus: http.StatusCreated,
			checkResponse: func(t *testing.T, body []byte) {
				var resp map[string]interface{}
				json.Unmarshal(body, &resp)
				// PDFs shouldn't have thumbnails
				if resp["thumbnailUrl"] != nil {
					t.Error("PDF should not have thumbnailUrl")
				}
			},
		},
		{
			name:           "upload WAV normalized from sniffed audio/wave",
			filename:       "sample.wav",
			content:        createTestWAV(),
			contentType:    "audio/wav",
			participantID:  "user-123",
			token:          tokens["user-123"],
			expectedStatus: http.StatusCreated,
			checkResponse: func(t *testing.T, body []byte) {
				var resp map[string]interface{}
				json.Unmarshal(body, &resp)
				if resp["mimeType"] != "audio/wav" {
					t.Errorf("expected normalized mimeType 'audio/wav', got %v", resp["mimeType"])
				}
				var storedPath string
				if err := db.QueryRow("SELECT stored_path FROM files WHERE id = ?", resp["id"]).Scan(&storedPath); err != nil {
					t.Fatalf("failed to read stored path: %v", err)
				}
				if filepath.Ext(storedPath) != ".wav" {
					t.Fatalf("stored extension = %q, want .wav", filepath.Ext(storedPath))
				}
			},
		},
		{
			name:           "upload OGG normalized from sniffed application/ogg",
			filename:       "sample.ogg",
			content:        createTestOGG(),
			contentType:    "audio/ogg",
			participantID:  "user-456",
			token:          tokens["user-456"],
			expectedStatus: http.StatusCreated,
			checkResponse: func(t *testing.T, body []byte) {
				var resp map[string]interface{}
				json.Unmarshal(body, &resp)
				if resp["mimeType"] != "audio/ogg" {
					t.Errorf("expected normalized mimeType 'audio/ogg', got %v", resp["mimeType"])
				}
				var storedPath string
				if err := db.QueryRow("SELECT stored_path FROM files WHERE id = ?", resp["id"]).Scan(&storedPath); err != nil {
					t.Fatalf("failed to read stored path: %v", err)
				}
				if filepath.Ext(storedPath) != ".ogg" {
					t.Fatalf("stored extension = %q, want .ogg", filepath.Ext(storedPath))
				}
			},
		},
		{
			name:           "upload without participant ID",
			filename:       "test.png",
			content:        createTestPNG(),
			contentType:    "image/png",
			participantID:  "",
			token:          tokens["unknown"],
			expectedStatus: http.StatusCreated,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, contentType := createMultipartRequest(tt.filename, tt.content, tt.contentType)

			req := httptest.NewRequest("POST", "/api/rooms/upload-test/files", body)
			req.SetPathValue("slug", "upload-test")
			req.Header.Set("Content-Type", contentType)
			req.Header.Set("X-Join-Token", tt.token)
			if tt.participantID != "" {
				req.Header.Set("X-Participant-ID", tt.participantID)
			}

			rr := httptest.NewRecorder()
			fileHandler.Upload(rr, req)

			if rr.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d: %s", tt.expectedStatus, rr.Code, rr.Body.String())
			}

			if tt.checkResponse != nil && rr.Code == tt.expectedStatus {
				tt.checkResponse(t, rr.Body.Bytes())
			}
		})
	}
}

func TestFileHandler_Upload_InvalidMIME(t *testing.T) {
	fileHandler, roomHandler, db, cleanup := setupFileTest(t)
	defer cleanup()

	roomID := createTestRoom(t, roomHandler, db, "mime-test")
	createTestParticipant(t, db, roomID, "user-123")
	token := createJoinToken(t, "user-123", "mime-test")

	// Create a file with disallowed MIME type (executable)
	content := []byte("MZ\x00\x00") // DOS executable header
	body, contentType := createMultipartRequest("test.exe", content, "application/x-msdownload")

	req := httptest.NewRequest("POST", "/api/rooms/mime-test/files", body)
	req.SetPathValue("slug", "mime-test")
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Join-Token", token)

	rr := httptest.NewRecorder()
	fileHandler.Upload(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected status %d for disallowed MIME type, got %d", http.StatusBadRequest, rr.Code)
	}

	if !strings.Contains(rr.Body.String(), "not allowed") {
		t.Errorf("expected 'not allowed' error message, got: %s", rr.Body.String())
	}
}

func TestFileHandler_Upload_TooLarge(t *testing.T) {
	fileHandler, roomHandler, db, cleanup := setupFileTest(t)
	defer cleanup()

	roomID := createTestRoom(t, roomHandler, db, "size-test")
	createTestParticipant(t, db, roomID, "user-123")
	token := createJoinToken(t, "user-123", "size-test")

	// Create a file larger than 5MB
	content := make([]byte, 6*1024*1024)
	body, contentType := createMultipartRequest("large.bin", content, "application/octet-stream")

	req := httptest.NewRequest("POST", "/api/rooms/size-test/files", body)
	req.SetPathValue("slug", "size-test")
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Join-Token", token)

	rr := httptest.NewRecorder()
	fileHandler.Upload(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected status %d for oversized file, got %d", http.StatusBadRequest, rr.Code)
	}
}

func TestFileHandler_Upload_Unauthorized(t *testing.T) {
	fileHandler, _, _, cleanup := setupFileTest(t)
	defer cleanup()

	body, contentType := createMultipartRequest("test.png", createTestPNG(), "image/png")

	req := httptest.NewRequest("POST", "/api/rooms/nonexistent/files", body)
	req.SetPathValue("slug", "nonexistent")
	req.Header.Set("Content-Type", contentType)

	rr := httptest.NewRecorder()
	fileHandler.Upload(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected status %d for missing token, got %d", http.StatusUnauthorized, rr.Code)
	}
}

func TestFileHandler_Download(t *testing.T) {
	fileHandler, roomHandler, db, cleanup := setupFileTest(t)
	defer cleanup()

	roomID := createTestRoom(t, roomHandler, db, "download-test")
	createTestParticipant(t, db, roomID, "unknown") // For default uploader
	token := createJoinToken(t, "unknown", "download-test")

	// First upload a file
	body, contentType := createMultipartRequest("download.png", createTestPNG(), "image/png")
	uploadReq := httptest.NewRequest("POST", "/api/rooms/download-test/files", body)
	uploadReq.SetPathValue("slug", "download-test")
	uploadReq.Header.Set("Content-Type", contentType)
	uploadReq.Header.Set("X-Join-Token", token)

	uploadRR := httptest.NewRecorder()
	fileHandler.Upload(uploadRR, uploadReq)

	if uploadRR.Code != http.StatusCreated {
		t.Fatalf("failed to upload test file: %s", uploadRR.Body.String())
	}

	var uploadResp map[string]interface{}
	json.Unmarshal(uploadRR.Body.Bytes(), &uploadResp)
	fileID := uploadResp["id"].(string)

	t.Run("download existing file", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/files/"+fileID, nil)
		req.SetPathValue("id", fileID)
		req.Header.Set("X-Join-Token", token)

		rr := httptest.NewRecorder()
		fileHandler.Download(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, rr.Code)
		}

		if rr.Header().Get("Content-Type") != "image/png" {
			t.Errorf("expected Content-Type 'image/png', got %s", rr.Header().Get("Content-Type"))
		}

		if rr.Header().Get("Content-Disposition") == "" {
			t.Error("expected Content-Disposition header")
		}
	})

	t.Run("download filename is formatted as a safe disposition parameter", func(t *testing.T) {
		maliciousName := "evil\"; filename=\"pwned.pdf"
		_, err := db.Exec("UPDATE files SET original_name = ? WHERE id = ?", maliciousName, fileID)
		if err != nil {
			t.Fatalf("failed to update original name: %v", err)
		}

		req := httptest.NewRequest("GET", "/api/files/"+fileID, nil)
		req.SetPathValue("id", fileID)
		req.Header.Set("X-Join-Token", token)

		rr := httptest.NewRecorder()
		fileHandler.Download(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected status %d, got %d", http.StatusOK, rr.Code)
		}
		disposition, params, err := mime.ParseMediaType(rr.Header().Get("Content-Disposition"))
		if err != nil {
			t.Fatalf("invalid Content-Disposition header: %v", err)
		}
		if disposition != "inline" {
			t.Fatalf("disposition = %q, want inline", disposition)
		}
		if params["filename"] != "evil; filename=pwned.pdf" {
			t.Fatalf("filename param = %q, want sanitized filename", params["filename"])
		}
	})

	t.Run("download nonexistent file", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/files/nonexistent", nil)
		req.SetPathValue("id", "nonexistent")
		req.Header.Set("X-Join-Token", token)

		rr := httptest.NewRecorder()
		fileHandler.Download(rr, req)

		if rr.Code != http.StatusNotFound {
			t.Errorf("expected status %d, got %d", http.StatusNotFound, rr.Code)
		}
	})
}

func TestFileHandler_Thumbnail(t *testing.T) {
	fileHandler, roomHandler, db, cleanup := setupFileTest(t)
	defer cleanup()

	roomID := createTestRoom(t, roomHandler, db, "thumb-test")
	createTestParticipant(t, db, roomID, "unknown") // For default uploader
	token := createJoinToken(t, "unknown", "thumb-test")

	// Upload an image (which should generate a thumbnail)
	body, contentType := createMultipartRequest("thumb.png", createTestPNG(), "image/png")
	uploadReq := httptest.NewRequest("POST", "/api/rooms/thumb-test/files", body)
	uploadReq.SetPathValue("slug", "thumb-test")
	uploadReq.Header.Set("Content-Type", contentType)
	uploadReq.Header.Set("X-Join-Token", token)

	uploadRR := httptest.NewRecorder()
	fileHandler.Upload(uploadRR, uploadReq)

	if uploadRR.Code != http.StatusCreated {
		t.Fatalf("failed to upload test file: %s", uploadRR.Body.String())
	}

	var uploadResp map[string]interface{}
	json.Unmarshal(uploadRR.Body.Bytes(), &uploadResp)
	fileID := uploadResp["id"].(string)

	t.Run("get thumbnail for image", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/files/"+fileID+"/thumbnail", nil)
		req.SetPathValue("id", fileID)
		req.Header.Set("X-Join-Token", token)

		rr := httptest.NewRecorder()
		fileHandler.Thumbnail(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, rr.Code)
		}
	})

	t.Run("thumbnail for nonexistent file", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/files/nonexistent/thumbnail", nil)
		req.SetPathValue("id", "nonexistent")
		req.Header.Set("X-Join-Token", token)

		rr := httptest.NewRecorder()
		fileHandler.Thumbnail(rr, req)

		if rr.Code != http.StatusNotFound {
			t.Errorf("expected status %d, got %d", http.StatusNotFound, rr.Code)
		}
	})
}

func TestFileHandler_MIME_Detection(t *testing.T) {
	tests := []struct {
		mimeType  string
		extension string
	}{
		{"image/jpeg", ".jpg"},
		{"image/png", ".png"},
		{"image/gif", ".gif"},
		{"image/webp", ".webp"},
		{"audio/mpeg", ".mp3"},
		{"audio/wav", ".wav"},
		{"audio/wave", ".wav"},
		{"audio/x-wav", ".wav"},
		{"audio/ogg", ".ogg"},
		{"application/ogg", ".ogg"},
		{"application/pdf", ".pdf"},
		{"application/octet-stream", ".bin"},
	}

	for _, tt := range tests {
		t.Run(tt.mimeType, func(t *testing.T) {
			ext := getExtensionForMIME(tt.mimeType)
			if ext != tt.extension {
				t.Errorf("expected extension %s for MIME %s, got %s", tt.extension, tt.mimeType, ext)
			}
		})
	}
}

func TestNormalizeDetectedMIMEType(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"audio/wave", "audio/wav"},
		{"audio/x-wav", "audio/wav"},
		{"application/ogg", "audio/ogg"},
		{"audio/mpeg", "audio/mpeg"},
		{"image/png", "image/png"},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := normalizeDetectedMIMEType(tt.in); got != tt.want {
				t.Fatalf("normalizeDetectedMIMEType(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// Helper functions to create test files

func createTestPNG() []byte {
	// Minimal valid PNG (1x1 red pixel)
	return []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, // PNG signature
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52, // IHDR chunk
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53,
		0xde, 0x00, 0x00, 0x00, 0x0c, 0x49, 0x44, 0x41, // IDAT chunk
		0x54, 0x08, 0xd7, 0x63, 0xf8, 0xcf, 0xc0, 0x00,
		0x00, 0x00, 0x03, 0x00, 0x01, 0x00, 0x18, 0xdd,
		0x8d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, // IEND chunk
		0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
	}
}

func createTestJPEG() []byte {
	// Minimal valid JPEG (1x1 white pixel)
	return []byte{
		0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 0x4a, 0x46,
		0x49, 0x46, 0x00, 0x01, 0x01, 0x00, 0x00, 0x01,
		0x00, 0x01, 0x00, 0x00, 0xff, 0xdb, 0x00, 0x43,
		0x00, 0x08, 0x06, 0x06, 0x07, 0x06, 0x05, 0x08,
		0x07, 0x07, 0x07, 0x09, 0x09, 0x08, 0x0a, 0x0c,
		0x14, 0x0d, 0x0c, 0x0b, 0x0b, 0x0c, 0x19, 0x12,
		0x13, 0x0f, 0x14, 0x1d, 0x1a, 0x1f, 0x1e, 0x1d,
		0x1a, 0x1c, 0x1c, 0x20, 0x24, 0x2e, 0x27, 0x20,
		0x22, 0x2c, 0x23, 0x1c, 0x1c, 0x28, 0x37, 0x29,
		0x2c, 0x30, 0x31, 0x34, 0x34, 0x34, 0x1f, 0x27,
		0x39, 0x3d, 0x38, 0x32, 0x3c, 0x2e, 0x33, 0x34,
		0x32, 0xff, 0xc0, 0x00, 0x0b, 0x08, 0x00, 0x01,
		0x00, 0x01, 0x01, 0x01, 0x11, 0x00, 0xff, 0xc4,
		0x00, 0x1f, 0x00, 0x00, 0x01, 0x05, 0x01, 0x01,
		0x01, 0x01, 0x01, 0x01, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x01, 0x02, 0x03, 0x04,
		0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0xff,
		0xc4, 0x00, 0xb5, 0x10, 0x00, 0x02, 0x01, 0x03,
		0x03, 0x02, 0x04, 0x03, 0x05, 0x05, 0x04, 0x04,
		0x00, 0x00, 0x01, 0x7d, 0x01, 0x02, 0x03, 0x00,
		0x04, 0x11, 0x05, 0x12, 0x21, 0x31, 0x41, 0x06,
		0x13, 0x51, 0x61, 0x07, 0x22, 0x71, 0x14, 0x32,
		0x81, 0x91, 0xa1, 0x08, 0x23, 0x42, 0xb1, 0xc1,
		0x15, 0x52, 0xd1, 0xf0, 0x24, 0x33, 0x62, 0x72,
		0x82, 0x09, 0x0a, 0x16, 0x17, 0x18, 0x19, 0x1a,
		0x25, 0x26, 0x27, 0x28, 0x29, 0x2a, 0x34, 0x35,
		0x36, 0x37, 0x38, 0x39, 0x3a, 0x43, 0x44, 0x45,
		0x46, 0x47, 0x48, 0x49, 0x4a, 0x53, 0x54, 0x55,
		0x56, 0x57, 0x58, 0x59, 0x5a, 0x63, 0x64, 0x65,
		0x66, 0x67, 0x68, 0x69, 0x6a, 0x73, 0x74, 0x75,
		0x76, 0x77, 0x78, 0x79, 0x7a, 0x83, 0x84, 0x85,
		0x86, 0x87, 0x88, 0x89, 0x8a, 0x92, 0x93, 0x94,
		0x95, 0x96, 0x97, 0x98, 0x99, 0x9a, 0xa2, 0xa3,
		0xa4, 0xa5, 0xa6, 0xa7, 0xa8, 0xa9, 0xaa, 0xb2,
		0xb3, 0xb4, 0xb5, 0xb6, 0xb7, 0xb8, 0xb9, 0xba,
		0xc2, 0xc3, 0xc4, 0xc5, 0xc6, 0xc7, 0xc8, 0xc9,
		0xca, 0xd2, 0xd3, 0xd4, 0xd5, 0xd6, 0xd7, 0xd8,
		0xd9, 0xda, 0xe1, 0xe2, 0xe3, 0xe4, 0xe5, 0xe6,
		0xe7, 0xe8, 0xe9, 0xea, 0xf1, 0xf2, 0xf3, 0xf4,
		0xf5, 0xf6, 0xf7, 0xf8, 0xf9, 0xfa, 0xff, 0xda,
		0x00, 0x08, 0x01, 0x01, 0x00, 0x00, 0x3f, 0x00,
		0xfb, 0xd5, 0x00, 0x00, 0x00, 0x00, 0xff, 0xd9,
	}
}

func createTestPDF() []byte {
	// Minimal valid PDF
	return []byte("%PDF-1.4\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n3 0 obj<</Type/Page/MediaBox[0 0 612 792]/Parent 2 0 R>>endobj\nxref\n0 4\n0000000000 65535 f \n0000000009 00000 n \n0000000052 00000 n \n0000000101 00000 n \ntrailer<</Size 4/Root 1 0 R>>\nstartxref\n178\n%%EOF\n")
}

func createTestWAV() []byte {
	// Minimal RIFF/WAVE header with a short PCM data chunk.
	return []byte{
		'R', 'I', 'F', 'F', 0x2c, 0x00, 0x00, 0x00,
		'W', 'A', 'V', 'E',
		'f', 'm', 't', ' ', 0x10, 0x00, 0x00, 0x00,
		0x01, 0x00, // PCM
		0x01, 0x00, // mono
		0x40, 0x1f, 0x00, 0x00, // 8000 Hz
		0x80, 0x3e, 0x00, 0x00, // byte rate
		0x02, 0x00, // block align
		0x10, 0x00, // 16-bit
		'd', 'a', 't', 'a', 0x08, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
	}
}

func createTestOGG() []byte {
	// Ogg page capture pattern plus enough bytes for http.DetectContentType.
	data := make([]byte, 64)
	copy(data, []byte{'O', 'g', 'g', 'S', 0x00, 0x02})
	return data
}
