package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"chromatic/internal/config"
	"chromatic/internal/database"
	"chromatic/internal/logger"
	"chromatic/internal/metrics"

	"golang.org/x/image/draw"
)

const (
	thumbnailMaxWidth  = 200
	thumbnailMaxHeight = 200
	thumbnailQuality   = 80
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

	// Generate thumbnail for images
	var thumbnailPath string
	if strings.HasPrefix(mimeType, "image/") && mimeType != "image/gif" {
		thumbnailPath = filepath.Join(h.cfg.UploadPath, roomID, "thumbnails", fileID+".jpg")
		if err := generateThumbnail(storedPath, thumbnailPath); err != nil {
			logger.Warn("Failed to generate thumbnail", "file_id", fileID, "error", err)
			// Don't fail the upload, just skip thumbnail
		} else {
			// Update database with thumbnail path
			h.db.Exec("UPDATE files SET thumbnail_path = ? WHERE id = ?", thumbnailPath, fileID)
		}
	}

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

	// Track file upload
	metrics.Get().TotalFilesUploaded.Add(1)

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
	var thumbnailPath *string
	err := h.db.QueryRow(`
		SELECT stored_path, mime_type, thumbnail_path FROM files WHERE id = ?
	`, id).Scan(&storedPath, &mimeType, &thumbnailPath)

	if err != nil {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	if !strings.HasPrefix(mimeType, "image/") {
		http.Error(w, "Not an image", http.StatusBadRequest)
		return
	}

	// If we have a thumbnail, serve it
	if thumbnailPath != nil && *thumbnailPath != "" {
		if _, err := os.Stat(*thumbnailPath); err == nil {
			w.Header().Set("Content-Type", "image/jpeg")
			http.ServeFile(w, r, *thumbnailPath)
			return
		}
	}

	// Fall back to original if no thumbnail
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

// generateThumbnail creates a resized thumbnail from an image file
func generateThumbnail(srcPath, dstPath string) error {
	// Open source file
	srcFile, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("failed to open source: %w", err)
	}
	defer srcFile.Close()

	// Decode image
	srcImage, format, err := image.Decode(srcFile)
	if err != nil {
		return fmt.Errorf("failed to decode image: %w", err)
	}

	logger.Debug("Generating thumbnail", "format", format, "source", srcPath)

	// Calculate new dimensions maintaining aspect ratio
	bounds := srcImage.Bounds()
	srcWidth := bounds.Dx()
	srcHeight := bounds.Dy()

	// Calculate scale to fit within max dimensions
	scaleW := float64(thumbnailMaxWidth) / float64(srcWidth)
	scaleH := float64(thumbnailMaxHeight) / float64(srcHeight)
	scale := scaleW
	if scaleH < scaleW {
		scale = scaleH
	}

	// Don't upscale small images
	if scale >= 1.0 {
		scale = 1.0
	}

	newWidth := int(float64(srcWidth) * scale)
	newHeight := int(float64(srcHeight) * scale)

	// Create destination image
	dstImage := image.NewRGBA(image.Rect(0, 0, newWidth, newHeight))

	// Use high-quality resampling
	draw.CatmullRom.Scale(dstImage, dstImage.Bounds(), srcImage, bounds, draw.Over, nil)

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
		return fmt.Errorf("failed to create thumbnail directory: %w", err)
	}

	// Create destination file
	dstFile, err := os.Create(dstPath)
	if err != nil {
		return fmt.Errorf("failed to create thumbnail file: %w", err)
	}
	defer dstFile.Close()

	// Encode as JPEG
	if err := jpeg.Encode(dstFile, dstImage, &jpeg.Options{Quality: thumbnailQuality}); err != nil {
		os.Remove(dstPath)
		return fmt.Errorf("failed to encode thumbnail: %w", err)
	}

	logger.Debug("Generated thumbnail", "path", dstPath, "width", newWidth, "height", newHeight)
	return nil
}

// Register image decoders (required for image.Decode to work)
func init() {
	// JPEG decoder is automatically registered via import
	// PNG decoder is automatically registered via import
	_ = png.Decode // Trigger registration
	_ = jpeg.Decode
}
