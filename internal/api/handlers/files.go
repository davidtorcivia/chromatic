package handlers

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"chromatic/internal/config"
	"chromatic/internal/database"
	"chromatic/internal/logger"
	"chromatic/internal/metrics"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	thumbnailMaxWidth  = 200
	thumbnailMaxHeight = 200
	thumbnailQuality   = 80
	maxImagePixels     = 40_000_000
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
	db           *database.DB
	cfg          *config.Config
	tokenManager *TokenManager
	// Admin session cookie validation — lets the admin room-settings page
	// view files/thumbnails without a participant join token.
	validateSession SessionValidator
}

// NewFileHandler creates a new FileHandler
func NewFileHandler(db *database.DB, cfg *config.Config, tokenSecret []byte) *FileHandler {
	return &FileHandler{
		db:           db,
		cfg:          cfg,
		tokenManager: NewTokenManager(tokenSecret),
	}
}

// SetSessionValidator wires the admin session cookie validator (same one the
// room and websocket handlers use).
func (h *FileHandler) SetSessionValidator(validator SessionValidator) {
	h.validateSession = validator
}

// hasAdminSession reports whether the request carries a valid admin session cookie.
func (h *FileHandler) hasAdminSession(r *http.Request) bool {
	if h.validateSession == nil {
		return false
	}
	c, err := r.Cookie(SessionCookieName)
	return err == nil && c.Value != "" && h.validateSession(c.Value)
}

// isPathWithin reports whether target resolves to a location inside baseDir.
// Used to ensure paths read from the database cannot escape the storage root.
func isPathWithin(baseDir, target string) bool {
	absBase, err := filepath.Abs(filepath.Clean(baseDir))
	if err != nil {
		return false
	}
	absTarget, err := filepath.Abs(filepath.Clean(target))
	if err != nil {
		return false
	}
	if absTarget == absBase {
		return true
	}
	return strings.HasPrefix(absTarget, absBase+string(filepath.Separator))
}

// getJoinToken extracts a join token from headers, the HttpOnly room cookie,
// bearer auth, or development-only query params. First-party browsers should
// rely on the cookie so signed tokens do not end up in URLs, logs or referrers.
func (h *FileHandler) getJoinToken(r *http.Request, roomSlug string) string {
	if token := r.Header.Get("X-Join-Token"); token != "" {
		return token
	}
	if c, err := r.Cookie(JoinTokenCookieName(roomSlug)); err == nil && c.Value != "" {
		return c.Value
	}
	if token := r.URL.Query().Get("token"); token != "" && (h.cfg == nil || !h.cfg.ProductionMode) {
		return token
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

// authorizeParticipant validates the join token and ensures the participant is admitted to the room.
func (h *FileHandler) authorizeParticipant(r *http.Request, roomSlug string) (string, error) {
	token := h.getJoinToken(r, roomSlug)
	if token == "" {
		return "", errors.New("missing join token")
	}

	payload, err := h.tokenManager.ValidateToken(token)
	if err != nil {
		return "", err
	}

	if payload.RoomSlug != roomSlug {
		return "", errors.New("token room mismatch")
	}

	var isAdmitted bool
	var roomStatus string
	ctx, cancel := database.WithTimeout(r.Context())
	err = h.db.QueryRowContext(ctx, `
		SELECT p.is_admitted, r.status
		FROM participants p
		JOIN rooms r ON r.id = p.room_id
		WHERE p.id = ? AND r.slug = ?
	`, payload.ParticipantID, roomSlug).Scan(&isAdmitted, &roomStatus)
	cancel()
	if err != nil {
		return "", err
	}

	if roomStatus == "ended" {
		return "", errors.New("room ended")
	}
	if !isAdmitted {
		return "", errors.New("participant not admitted")
	}

	return payload.ParticipantID, nil
}

// Upload handles ordinary file uploads (references, decks, audio notes).
func (h *FileHandler) Upload(w http.ResponseWriter, r *http.Request) {
	h.storeUpload(w, r, fileOriginUpload)
}

// GrabFrame handles frame captures from the session player. It is a separate
// route from Upload purely so origin='frame-grab' is decided by which endpoint
// the request arrived on: keying watermark behaviour off a client-supplied
// field would let a client opt its own capture out of the stamp.
func (h *FileHandler) GrabFrame(w http.ResponseWriter, r *http.Request) {
	h.storeUpload(w, r, fileOriginFrameGrab)
}

func (h *FileHandler) storeUpload(w http.ResponseWriter, r *http.Request, origin string) {
	slug := r.PathValue("slug")

	uploaderID, err := h.authorizeParticipant(r, slug)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Room id plus the watermark config, which decides whether a frame grab is
	// stamped on the way in.
	roomCtx, roomCancel := database.WithTimeout(r.Context())
	room, err := h.roomWatermark(roomCtx, slug)
	roomCancel()
	if err != nil {
		http.Error(w, "Room not found", http.StatusNotFound)
		return
	}
	roomID := room.ID

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

	// Detect MIME type from content, not from the client-supplied filename or
	// part header.
	mimeType, err := detectFileContentType(file)
	if err != nil {
		http.Error(w, "Failed to read file", http.StatusBadRequest)
		return
	}

	// Validate MIME type
	if !allowedMIMETypes[mimeType] {
		http.Error(w, "File type not allowed", http.StatusBadRequest)
		return
	}
	// The grab route only ever receives the player's JPEG capture. Narrowing it
	// keeps the stamped path to a single decoder.
	if origin == fileOriginFrameGrab && mimeType != "image/jpeg" {
		http.Error(w, "Frame grabs must be JPEG", http.StatusBadRequest)
		return
	}
	if err := validateImageDimensions(file, mimeType, maxImagePixels); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Generate file ID and path
	fileID, err := generateFileID()
	if err != nil {
		logger.Error("Failed to generate file ID", "error", err)
		http.Error(w, "Failed to save file", http.StatusInternalServerError)
		return
	}
	originalName := sanitizeFilename(header.Filename)
	ext := getExtensionForMIME(mimeType)
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

	if _, copyErr := io.Copy(dst, file); copyErr != nil {
		_ = dst.Close()
		_ = os.Remove(storedPath)
		http.Error(w, "Failed to save file", http.StatusInternalServerError)
		return
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(storedPath)
		http.Error(w, "Failed to save file", http.StatusInternalServerError)
		return
	}
	storedSize := header.Size

	// Thumbnails are generated from the clean frame, before any stamp. Identity
	// text tiled onto a 200px preview is unreadable mush, and thumbnails are
	// deliberately left unmarked (a 200px still is not a meaningful leak
	// surface for a 4K frame, and per-requester marking would mean regenerating
	// on every request).
	var thumbnailPath string
	if strings.HasPrefix(mimeType, "image/") && mimeType != "image/gif" {
		thumbnailPath = filepath.Join(h.cfg.UploadPath, roomID, "thumbnails", fileID+".jpg")
		if err := generateThumbnail(storedPath, thumbnailPath); err != nil {
			logger.Warn("Failed to generate thumbnail", "file_id", fileID, "error", err)
			// Don't fail the upload, just skip thumbnail
			thumbnailPath = ""
		}
	}

	// Provenance stamp: who captured this frame. Serve-time marking records who
	// downloaded it, but only the upload stamp survives a copy made outside
	// Chromatic. Fails the upload rather than storing a clean frame: a grab in a
	// watermarked room that silently skipped its stamp is the hole this closes.
	if origin == fileOriginFrameGrab && room.marksFrames() {
		name, _ := h.participantName(r.Context(), uploaderID)
		size, stampErr := stampStoredFrame(storedPath, frameStampSpec{
			Lines:   captureStampLines(name, uploaderID, time.Now().UTC()),
			Opacity: room.Opacity,
			Scale:   room.Scale,
		})
		if stampErr != nil {
			logger.Error("Failed to stamp grabbed frame", "file_id", fileID, "error", stampErr)
			_ = os.Remove(storedPath)
			if thumbnailPath != "" {
				_ = os.Remove(thumbnailPath)
			}
			http.Error(w, "Failed to save file", http.StatusInternalServerError)
			return
		}
		storedSize = size
	}

	// Insert into database
	insertCtx, insertCancel := database.WithTimeout(r.Context())
	_, err = h.db.ExecContext(insertCtx, `
		INSERT INTO files (id, room_id, uploader_id, original_name, stored_path, thumbnail_path, mime_type, size_bytes, origin)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, fileID, roomID, uploaderID, originalName, storedPath, nullableString(thumbnailPath), mimeType, storedSize, origin)
	insertCancel()

	if err != nil {
		_ = os.Remove(storedPath)
		if thumbnailPath != "" {
			_ = os.Remove(thumbnailPath)
		}
		http.Error(w, "Failed to save file metadata", http.StatusInternalServerError)
		return
	}

	response := map[string]interface{}{
		"id":           fileID,
		"originalName": originalName,
		"mimeType":     mimeType,
		"sizeBytes":    storedSize,
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

	var storedPath, mimeType, originalName, roomSlug, origin string
	var room roomWatermarkConfig
	dlCtx, dlCancel := database.WithTimeout(r.Context())
	err := h.db.QueryRowContext(dlCtx, `
		SELECT f.stored_path, f.mime_type, f.original_name, f.origin, r.slug,
		       COALESCE(r.watermark_mode, 'none'), COALESCE(r.watermark_opacity, 0.3),
		       COALESCE(r.watermark_scale, 1.0)
		FROM files f
		JOIN rooms r ON r.id = f.room_id
		WHERE f.id = ?
	`, id).Scan(&storedPath, &mimeType, &originalName, &origin, &roomSlug, &room.Mode, &room.Opacity, &room.Scale)
	dlCancel()

	if err != nil {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	// Keep the requester id: a marked serve is stamped for whoever is
	// downloading, not for whoever grabbed the frame. Marking only at upload
	// misattributes — Alice grabs, Bob downloads, Bob leaks, the frame says
	// Alice.
	requesterID, authErr := h.authorizeParticipant(r, roomSlug)
	if authErr != nil {
		if !h.hasAdminSession(r) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		requesterID = ""
	}

	// Containment check: the stored path comes from the database and must
	// resolve inside the configured upload root.
	if !isPathWithin(h.cfg.UploadPath, storedPath) {
		logger.Warn("Blocked file download outside upload root", "file_id", id, "path", storedPath)
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	// Check file exists
	if _, err := os.Stat(storedPath); os.IsNotExist(err) {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	// PDFs can contain embedded JavaScript that executes when rendered
	// inline in the browser — force them to download instead.
	disposition := "inline"
	if mimeType == "application/pdf" {
		disposition = "attachment"
	}

	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{
		"filename": sanitizeFilename(originalName),
	}))

	if origin == fileOriginFrameGrab && room.marksFrames() {
		h.serveMarkedFrame(w, r, id, storedPath, room, requesterID)
		return
	}

	http.ServeFile(w, r, storedPath)
}

// serveMarkedFrame stamps a grabbed frame for the requester and writes it
// directly. ServeFile is not usable here: the body differs per requester and
// per second, so range requests, ETag and Last-Modified would all describe a
// representation that does not exist.
func (h *FileHandler) serveMarkedFrame(
	w http.ResponseWriter, r *http.Request,
	fileID, storedPath string, room roomWatermarkConfig, requesterID string,
) {
	src, err := os.ReadFile(storedPath)
	if err != nil {
		logger.Error("Failed to read frame for marking", "file_id", fileID, "error", err)
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	name := "Admin"
	if requesterID != "" {
		name, _ = h.participantName(r.Context(), requesterID)
	}

	marked, err := stampJPEG(src, frameStampSpec{
		Lines:   downloadStampLines(name, requesterID, time.Now().UTC()),
		Opacity: room.Opacity,
		Scale:   room.Scale,
	})
	if err != nil {
		// Fail closed. Serving the clean frame because marking broke would hand
		// out exactly the unmarked full-resolution still this path exists to
		// prevent.
		logger.Error("Failed to mark frame for download", "file_id", fileID, "error", err)
		http.Error(w, "Failed to prepare file", http.StatusInternalServerError)
		return
	}

	// A shared cache handing Bob a copy marked for Alice would undo the whole
	// thing.
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(marked)))
	if _, err := w.Write(marked); err != nil {
		logger.Debug("Marked frame write failed", "file_id", fileID, "error", err)
	}
}

// ListRoomFiles returns every file uploaded to a room, newest first.
// Admin-only (registered under the auth-protected mux) — used by the room
// settings page to review and moderate attachments.
func (h *FileHandler) ListRoomFiles(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")

	var roomID string
	roomCtx, roomCancel := database.WithTimeout(r.Context())
	if err := h.db.QueryRowContext(roomCtx, "SELECT id FROM rooms WHERE slug = ?", slug).Scan(&roomID); err != nil {
		roomCancel()
		http.Error(w, "Room not found", http.StatusNotFound)
		return
	}
	roomCancel()

	listCtx, listCancel := database.WithTimeout(r.Context())
	rows, err := h.db.QueryContext(listCtx, `
		SELECT f.id, f.original_name, f.mime_type, f.size_bytes, f.created_at,
		       COALESCE(p.name, 'Unknown')
		FROM files f
		LEFT JOIN participants p ON p.id = f.uploader_id
		WHERE f.room_id = ?
		ORDER BY f.created_at DESC
	`, roomID)
	if err != nil {
		listCancel()
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	defer listCancel()

	files := []map[string]interface{}{}
	for rows.Next() {
		var (
			id, name, mime, uploader string
			size                     int64
			createdAt                time.Time
		)
		if err := rows.Scan(&id, &name, &mime, &size, &createdAt, &uploader); err != nil {
			continue
		}
		entry := map[string]interface{}{
			"id":           id,
			"originalName": name,
			"mimeType":     mime,
			"sizeBytes":    size,
			"uploaderName": uploader,
			"createdAt":    createdAt.UnixMilli(),
			"url":          fmt.Sprintf("/api/files/%s", id),
		}
		if strings.HasPrefix(mime, "image/") {
			entry["thumbnailUrl"] = fmt.Sprintf("/api/files/%s/thumbnail", id)
		}
		files = append(files, entry)
	}
	// Distinguish end-of-data from a mid-iteration error (e.g. query deadline)
	// so a timeout isn't served as a silently truncated file list.
	if err := rows.Err(); err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	respondJSON(w, map[string]interface{}{"files": files})
}

// DeleteFile removes an uploaded file: disk file, thumbnail, DB row, and any
// chat messages referencing it (so transcripts don't show dead attachments).
// Admin-only (registered under the auth-protected mux).
func (h *FileHandler) DeleteFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Reject anything that isn't a real file id. The id is interpolated into a
	// LIKE pattern below to remove the file's chat message; a value containing
	// LIKE metacharacters (%) would over-match and delete unrelated file
	// messages in the room. Validating the format up front (and escaping the
	// LIKE pattern defensively) makes that impossible.
	if !validFileID(id) {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	var storedPath, roomID string
	var thumbnailPath *string
	getCtx, getCancel := database.WithTimeout(r.Context())
	err := h.db.QueryRowContext(getCtx, `
		SELECT stored_path, thumbnail_path, room_id FROM files WHERE id = ?
	`, id).Scan(&storedPath, &thumbnailPath, &roomID)
	getCancel()
	if err != nil {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	// Containment checks mirror Download/Thumbnail: paths come from the DB
	// and must resolve inside the upload root before we unlink anything.
	if isPathWithin(h.cfg.UploadPath, storedPath) {
		if err := os.Remove(storedPath); err != nil && !os.IsNotExist(err) {
			logger.Warn("Failed to remove file from disk", "file_id", id, "error", err)
		}
	} else {
		logger.Warn("Blocked file delete outside upload root", "file_id", id, "path", storedPath)
	}
	if thumbnailPath != nil && *thumbnailPath != "" && isPathWithin(h.cfg.UploadPath, *thumbnailPath) {
		if err := os.Remove(*thumbnailPath); err != nil && !os.IsNotExist(err) {
			logger.Warn("Failed to remove thumbnail from disk", "file_id", id, "error", err)
		}
	}

	// Both deletes run in one transaction: losing the messages delete after the
	// files delete succeeded would leave a permanently broken attachment
	// reference in the transcript.
	delCtx, delCancel := database.WithTimeout(r.Context())
	defer delCancel()
	tx, err := h.db.BeginTx(delCtx, nil)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback() // no-op after Commit
	if _, err := tx.ExecContext(delCtx, "DELETE FROM files WHERE id = ?", id); err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	// File chat messages store the file reference as JSON in content; remove
	// them so reloaded transcripts don't render broken attachments. Escape LIKE
	// metacharacters in the id (defence in depth on top of validFileID) so the
	// pattern can only ever match this exact file's reference.
	if _, err := tx.ExecContext(delCtx, `DELETE FROM messages WHERE room_id = ? AND type = 'file' AND content LIKE ? ESCAPE '\'`,
		roomID, `%"`+escapeLikeForID(id)+`"%`); err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	logger.Info("File deleted by admin", "file_id", id, "room_id", roomID)
	w.WriteHeader(http.StatusNoContent)
}

// Thumbnail serves image thumbnails
func (h *FileHandler) Thumbnail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var storedPath, mimeType, roomSlug, origin string
	var thumbnailPath *string
	var room roomWatermarkConfig
	thCtx, thCancel := database.WithTimeout(r.Context())
	err := h.db.QueryRowContext(thCtx, `
		SELECT f.stored_path, f.mime_type, f.thumbnail_path, f.origin, r.slug,
		       COALESCE(r.watermark_mode, 'none'), COALESCE(r.watermark_opacity, 0.3),
		       COALESCE(r.watermark_scale, 1.0)
		FROM files f
		JOIN rooms r ON r.id = f.room_id
		WHERE f.id = ?
	`, id).Scan(&storedPath, &mimeType, &thumbnailPath, &origin, &roomSlug,
		&room.Mode, &room.Opacity, &room.Scale)
	thCancel()

	if err != nil {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	if _, err := h.authorizeParticipant(r, roomSlug); err != nil && !h.hasAdminSession(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if !strings.HasPrefix(mimeType, "image/") {
		http.Error(w, "Not an image", http.StatusBadRequest)
		return
	}

	// If we have a thumbnail, serve it (after containment check)
	if thumbnailPath != nil && *thumbnailPath != "" {
		if !isPathWithin(h.cfg.UploadPath, *thumbnailPath) {
			logger.Warn("Blocked thumbnail outside upload root", "file_id", id, "path", *thumbnailPath)
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if _, err := os.Stat(*thumbnailPath); err == nil {
			w.Header().Set("Content-Type", "image/jpeg")
			http.ServeFile(w, r, *thumbnailPath)
			return
		}
	}

	// Fall back to original if no thumbnail (after containment check)
	if !isPathWithin(h.cfg.UploadPath, storedPath) {
		logger.Warn("Blocked file download outside upload root", "file_id", id, "path", storedPath)
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	// Never let the fallback hand out a full-resolution grabbed frame. If
	// thumbnail generation failed at upload, or the file was removed from disk,
	// this branch would otherwise serve the whole picture from an endpoint named
	// "thumbnail" — the exact hole this path exists to close, one route over.
	if origin == fileOriginFrameGrab && room.marksFrames() {
		data, err := encodeThumbnail(storedPath)
		if err != nil {
			logger.Warn("Failed to build fallback thumbnail", "file_id", id, "error", err)
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		if _, err := w.Write(data); err != nil {
			logger.Debug("Fallback thumbnail write failed", "file_id", id, "error", err)
		}
		return
	}

	http.ServeFile(w, r, storedPath)
}

// File origins. Set by which route the upload arrived on, never by a client
// field, so watermark behaviour cannot be opted out of by the uploader.
const (
	fileOriginUpload    = "upload"
	fileOriginFrameGrab = "frame-grab"
)

// roomWatermarkConfig is the subset of room watermark settings the file
// pipeline needs.
type roomWatermarkConfig struct {
	ID      string
	Mode    string
	Opacity float64
	Scale   float64
}

// marksFrames reports whether grabbed frames in this room get stamped. Rooms
// with watermarking switched off keep clean grabs: the stamp follows the room's
// watermark setting rather than introducing a second, hidden policy.
func (c roomWatermarkConfig) marksFrames() bool {
	return c.Mode != "" && c.Mode != "none"
}

func (h *FileHandler) roomWatermark(ctx context.Context, slug string) (roomWatermarkConfig, error) {
	var c roomWatermarkConfig
	err := h.db.QueryRowContext(ctx, `
		SELECT id, COALESCE(watermark_mode, 'none'), COALESCE(watermark_opacity, 0.3),
		       COALESCE(watermark_scale, 1.0)
		FROM rooms WHERE slug = ?
	`, slug).Scan(&c.ID, &c.Mode, &c.Opacity, &c.Scale)
	return c, err
}

// participantName resolves a display name from the database rather than from
// the join token: mark content is server-owned, and a participant can be gone
// by the time an old grab is downloaded.
func (h *FileHandler) participantName(ctx context.Context, participantID string) (string, bool) {
	if participantID == "" {
		return "Unknown", false
	}
	var name string
	nameCtx, cancel := database.WithTimeout(ctx)
	err := h.db.QueryRowContext(nameCtx, "SELECT name FROM participants WHERE id = ?", participantID).Scan(&name)
	cancel()
	if err != nil || name == "" {
		return "Unknown", false
	}
	return name, true
}

// shortParticipantID is what ties a leaked frame back to an audit row. The
// participant id is 32 hex chars; the first 8 are plenty to disambiguate within
// a room and short enough to stay legible in a tiled mark.
func shortParticipantID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func captureStampLines(name, participantID string, t time.Time) []string {
	return []string{
		fmt.Sprintf("captured by %s (%s)", name, shortParticipantID(participantID)),
		t.Format("2006-01-02 15:04:05 UTC"),
	}
}

func downloadStampLines(name, participantID string, t time.Time) []string {
	who := name
	if participantID != "" {
		who = fmt.Sprintf("%s (%s)", name, shortParticipantID(participantID))
	}
	return []string{
		fmt.Sprintf("downloaded by %s", who),
		t.Format("2006-01-02 15:04:05 UTC"),
	}
}

// stampStoredFrame rewrites the stored JPEG with the stamp burned in and
// returns the new size on disk.
func stampStoredFrame(path string, spec frameStampSpec) (int64, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	marked, err := stampJPEG(src, spec)
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(path, marked, 0o600); err != nil {
		return 0, err
	}
	return int64(len(marked)), nil
}

// nullableString keeps an unset path as SQL NULL instead of an empty string,
// which is what the thumbnail readers expect.
func nullableString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func generateFileID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// fileIDRe matches the format generateFileID produces: 32 lowercase hex chars.
// Used to validate admin-supplied file ids before they reach a LIKE pattern so
// a path value containing LIKE metacharacters (%, _) cannot over-match and
// delete unrelated file chat messages.
var fileIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// validFileID reports whether id is a plausibly-real file id (32 hex chars).
func validFileID(id string) bool { return fileIDRe.MatchString(id) }

// escapeLikeForID escapes LIKE metacharacters in a file id destined for a LIKE
// pattern (defence in depth on top of validFileID). % and _ are escaped with a
// backslash; the caller must pair this with ESCAPE '\' on the LIKE clause.
func escapeLikeForID(id string) string {
	// strings.ReplaceAll would suffice, but build the escaped form explicitly to
	// keep the escape char handling obvious.
	r := make([]byte, 0, len(id)+4)
	for i := 0; i < len(id); i++ {
		switch id[i] {
		case '%', '_', '\\':
			r = append(r, '\\')
		}
		r = append(r, id[i])
	}
	return string(r)
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
	case "audio/wav", "audio/wave", "audio/x-wav":
		return ".wav"
	case "audio/ogg", "application/ogg":
		return ".ogg"
	case "application/pdf":
		return ".pdf"
	default:
		return ".bin"
	}
}

func validateImageDimensions(file io.ReadSeeker, mimeType string, maxPixels int64) error {
	if !strings.HasPrefix(mimeType, "image/") {
		return nil
	}

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("failed to inspect image")
	}
	cfg, _, err := image.DecodeConfig(file)
	if _, seekErr := file.Seek(0, io.SeekStart); seekErr != nil {
		return fmt.Errorf("failed to inspect image")
	}
	if err != nil {
		return fmt.Errorf("invalid image file")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return fmt.Errorf("invalid image dimensions")
	}
	pixels := int64(cfg.Width) * int64(cfg.Height)
	if pixels > maxPixels {
		return fmt.Errorf("image dimensions too large (max %d megapixels)", maxPixels/1_000_000)
	}
	return nil
}

func detectFileContentType(file io.ReadSeeker) (string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}

	buffer := make([]byte, 512)
	n, err := file.Read(buffer)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if n == 0 {
		return "", errors.New("empty file")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}

	return normalizeDetectedMIMEType(http.DetectContentType(buffer[:n])), nil
}

func normalizeDetectedMIMEType(mimeType string) string {
	switch mimeType {
	case "audio/wave", "audio/x-wav":
		return "audio/wav"
	case "application/ogg":
		return "audio/ogg"
	default:
		return mimeType
	}
}

func detectPathContentType(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	return detectFileContentType(file)
}

// generateThumbnail creates a resized thumbnail from an image file
func generateThumbnail(srcPath, dstPath string) error {
	data, err := encodeThumbnail(srcPath)
	if err != nil {
		return err
	}

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
		return fmt.Errorf("failed to create thumbnail directory: %w", err)
	}

	if err := os.WriteFile(dstPath, data, 0o600); err != nil {
		return fmt.Errorf("failed to write thumbnail: %w", err)
	}

	logger.Debug("Generated thumbnail", "path", dstPath, "bytes", len(data))
	return nil
}

// encodeThumbnail decodes an image and returns a JPEG-encoded thumbnail. Kept
// separate from generateThumbnail so the serve path can build one in memory
// without leaving a file behind.
func encodeThumbnail(srcPath string) ([]byte, error) {
	// Open source file
	srcFile, err := os.Open(srcPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open source: %w", err)
	}
	defer srcFile.Close()

	// Decode image
	srcImage, format, err := image.Decode(srcFile)
	if err != nil {
		return nil, fmt.Errorf("failed to decode image: %w", err)
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

	var out bytes.Buffer
	if err := jpeg.Encode(&out, dstImage, &jpeg.Options{Quality: thumbnailQuality}); err != nil {
		return nil, fmt.Errorf("failed to encode thumbnail: %w", err)
	}

	logger.Debug("Encoded thumbnail", "source", srcPath, "width", newWidth, "height", newHeight)
	return out.Bytes(), nil
}

// Register image decoders (required for image.Decode to work)
func init() {
	// JPEG decoder is automatically registered via import
	// PNG decoder is automatically registered via import
	_ = png.Decode // Trigger registration
	_ = jpeg.Decode
}
