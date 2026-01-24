package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"chromatic/internal/config"
	"chromatic/internal/database"
)

// Allowed MIME types and max file size
var allowedMIMETypes = map[string]bool{
	"image/jpeg":      true,
	"image/png":       true,
	"image/gif":       true,
	"image/webp":      true,
	"audio/mpeg":      true,
	"audio/wav":       true,
	"audio/ogg":       true,
	"application/pdf": true,
}

const maxFileSize = 5 * 1024 * 1024 // 5 MB

// FileHandler handles file upload and download
type FileHandler struct {
	db  *database.DB
	cfg *config.Config
}

// NewFileHandler creates a new FileHandler
func NewFileHandler(db *database.DB, cfg *config.Config) *FileHandler {
	return &FileHandler{db: db, cfg: cfg}
}

// Upload handles file uploads
func (h *FileHandler) Upload(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")

	// Get room ID
	var roomID string
	err := h.db.QueryRow("SELECT id FROM rooms WHERE slug = ?", slug).Scan(&roomID)
	if err != nil {
		http.Error(w, "Room not found", http.StatusNotFound)
		return
	}

	// Parse multipart form
	r.Body = http.MaxBytesReader(w, r.Body, maxFileSize+1024)
	if err := r.ParseMultipartForm(maxFileSize); err != nil {
		http.Error(w, "File too large (max 5MB)", http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Missing file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Check file size
	if header.Size > maxFileSize {
		http.Error(w, "File too large (max 5MB)", http.StatusBadRequest)
		return
	}

	// Detect MIME type
	buffer := make([]byte, 512)
	_, err = file.Read(buffer)
	if err != nil {
		http.Error(w, "Failed to read file", http.StatusBadRequest)
		return
	}
	mimeType := http.DetectContentType(buffer)

	// Reset file position
	file.Seek(0, io.SeekStart)

	// Validate MIME type
	if !allowedMIMETypes[mimeType] {
		http.Error(w, "File type not allowed", http.StatusBadRequest)
		return
	}

	// Generate file ID and path
	fileID := generateFileID()
	ext := filepath.Ext(header.Filename)
	if ext == "" {
		ext = getExtensionForMIME(mimeType)
	}
	storedName := fileID + ext
	storedPath := filepath.Join(h.cfg.UploadPath, roomID, storedName)

	// Create directory
	if err := os.MkdirAll(filepath.Dir(storedPath), 0755); err != nil {
		http.Error(w, "Failed to create directory", http.StatusInternalServerError)
		return
	}

	// Save file
	dst, err := os.Create(storedPath)
	if err != nil {
		http.Error(w, "Failed to save file", http.StatusInternalServerError)
		return
	}
	defer dst.Close()

	if _, err := io.Copy(dst, file); err != nil {
		os.Remove(storedPath)
		http.Error(w, "Failed to save file", http.StatusInternalServerError)
		return
	}

	// Get uploader ID from auth context (simplified for now)
	uploaderID := r.Header.Get("X-Participant-ID")
	if uploaderID == "" {
		uploaderID = "unknown"
	}

	// Insert into database
	_, err = h.db.Exec(`
		INSERT INTO files (id, room_id, uploader_id, original_name, stored_path, mime_type, size_bytes)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, fileID, roomID, uploaderID, header.Filename, storedPath, mimeType, header.Size)

	if err != nil {
		os.Remove(storedPath)
		http.Error(w, "Failed to save file metadata", http.StatusInternalServerError)
		return
	}

	// TODO: Generate thumbnail for images

	response := map[string]interface{}{
		"id":           fileID,
		"originalName": header.Filename,
		"mimeType":     mimeType,
		"sizeBytes":    header.Size,
		"url":          fmt.Sprintf("/api/files/%s", fileID),
	}

	if strings.HasPrefix(mimeType, "image/") {
		response["thumbnailUrl"] = fmt.Sprintf("/api/files/%s/thumbnail", fileID)
	}

	w.WriteHeader(http.StatusCreated)
	respondJSON(w, response)
}

// Download handles file downloads
func (h *FileHandler) Download(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var storedPath, mimeType, originalName string
	err := h.db.QueryRow(`
		SELECT stored_path, mime_type, original_name FROM files WHERE id = ?
	`, id).Scan(&storedPath, &mimeType, &originalName)

	if err != nil {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	// Check file exists
	if _, err := os.Stat(storedPath); os.IsNotExist(err) {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="%s"`, originalName))

	http.ServeFile(w, r, storedPath)
}

// Thumbnail serves image thumbnails
func (h *FileHandler) Thumbnail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var storedPath, mimeType string
	err := h.db.QueryRow(`
		SELECT stored_path, mime_type FROM files WHERE id = ?
	`, id).Scan(&storedPath, &mimeType)

	if err != nil {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	// For now, serve the original image
	// TODO: Implement proper thumbnail generation
	if !strings.HasPrefix(mimeType, "image/") {
		http.Error(w, "Not an image", http.StatusBadRequest)
		return
	}

	http.ServeFile(w, r, storedPath)
}

func generateFileID() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

func getExtensionForMIME(mimeType string) string {
	switch mimeType {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "audio/mpeg":
		return ".mp3"
	case "audio/wav":
		return ".wav"
	case "audio/ogg":
		return ".ogg"
	case "application/pdf":
		return ".pdf"
	default:
		return ".bin"
	}
}
