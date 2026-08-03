package handlers

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"chromatic/internal/database"
)

// createTestFrameJPEG builds a uniform mid-grey JPEG. Uniform is the point: any
// non-grey pixel in a decoded result is stamp ink, which is what these tests
// assert on.
func createTestFrameJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.RGBA{R: 128, G: 128, B: 128, A: 255}}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 92}); err != nil {
		t.Fatalf("encode test frame: %v", err)
	}
	return buf.Bytes()
}

// isUniformGrey reports whether every pixel of the JPEG is within tol of the
// mid-grey the test source was built from. JPEG ringing means an exact compare
// is not possible even for an untouched image.
func isUniformGrey(t *testing.T, data []byte, tol int) bool {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			for _, c := range []int{int(r >> 8), int(g >> 8), int(bl >> 8)} {
				if c < 128-tol || c > 128+tol {
					return false
				}
			}
		}
	}
	return true
}

func enableRoomWatermark(t *testing.T, db *database.DB, roomID string) {
	t.Helper()
	if _, err := db.Exec(`
		UPDATE rooms SET watermark_mode = 'text', watermark_opacity = 0.35, watermark_scale = 1.0
		WHERE id = ?
	`, roomID); err != nil {
		t.Fatalf("enable watermark: %v", err)
	}
}

func postFrameGrab(t *testing.T, h *FileHandler, slug, token string, frame []byte) *httptest.ResponseRecorder {
	t.Helper()
	body, contentType := createMultipartRequest("frame-"+slug+"-120000.jpg", frame, "image/jpeg")
	req := httptest.NewRequest("POST", "/api/rooms/"+slug+"/grab", body)
	req.SetPathValue("slug", slug)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Join-Token", token)
	rr := httptest.NewRecorder()
	h.GrabFrame(rr, req)
	return rr
}

func uploadedFileID(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload failed: %d %s", rr.Code, rr.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	return resp["id"].(string)
}

func storedPathOf(t *testing.T, db *database.DB, fileID string) string {
	t.Helper()
	var p string
	if err := db.QueryRow("SELECT stored_path FROM files WHERE id = ?", fileID).Scan(&p); err != nil {
		t.Fatalf("read stored path: %v", err)
	}
	return p
}

func downloadFile(t *testing.T, h *FileHandler, fileID, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/files/"+fileID, nil)
	req.SetPathValue("id", fileID)
	if token != "" {
		req.Header.Set("X-Join-Token", token)
	}
	rr := httptest.NewRecorder()
	h.Download(rr, req)
	return rr
}

// A grab into a watermarked room is stamped on the way in, recorded with
// origin='frame-grab', and keeps a clean thumbnail.
func TestGrabFrame_StampsAndRecordsOrigin(t *testing.T) {
	fileHandler, roomHandler, db, cleanup := setupFileTest(t)
	defer cleanup()

	roomID := createTestRoom(t, roomHandler, db, "grab-stamp")
	enableRoomWatermark(t, db, roomID)
	createTestParticipant(t, db, roomID, "grabber-1")
	token := createJoinToken(t, "grabber-1", "grab-stamp")

	frame := createTestFrameJPEG(t, 640, 360)
	fileID := uploadedFileID(t, postFrameGrab(t, fileHandler, "grab-stamp", token, frame))

	var origin string
	var thumbnailPath *string
	if err := db.QueryRow("SELECT origin, thumbnail_path FROM files WHERE id = ?", fileID).
		Scan(&origin, &thumbnailPath); err != nil {
		t.Fatalf("read file row: %v", err)
	}
	if origin != fileOriginFrameGrab {
		t.Errorf("origin = %q, want %q", origin, fileOriginFrameGrab)
	}

	stored, err := os.ReadFile(storedPathOf(t, db, fileID))
	if err != nil {
		t.Fatalf("read stored frame: %v", err)
	}
	if isUniformGrey(t, stored, 6) {
		t.Error("stored grab is still uniform grey; the capture stamp was not burned in")
	}

	// Thumbnails stay unmarked by design: identity text on a 200px preview is
	// unreadable, and it is not a meaningful leak surface.
	if thumbnailPath == nil || *thumbnailPath == "" {
		t.Fatal("expected a thumbnail for a grabbed frame")
	}
	thumb, err := os.ReadFile(*thumbnailPath)
	if err != nil {
		t.Fatalf("read thumbnail: %v", err)
	}
	if !isUniformGrey(t, thumb, 6) {
		t.Error("thumbnail carries stamp ink; it must be generated from the clean frame")
	}
}

// The stamp follows the room's watermark setting. A room with watermarking off
// gets clean grabs.
func TestGrabFrame_UnstampedWhenWatermarkOff(t *testing.T) {
	fileHandler, roomHandler, db, cleanup := setupFileTest(t)
	defer cleanup()

	roomID := createTestRoom(t, roomHandler, db, "grab-nomark")
	createTestParticipant(t, db, roomID, "grabber-2")
	token := createJoinToken(t, "grabber-2", "grab-nomark")

	frame := createTestFrameJPEG(t, 320, 180)
	fileID := uploadedFileID(t, postFrameGrab(t, fileHandler, "grab-nomark", token, frame))

	stored, err := os.ReadFile(storedPathOf(t, db, fileID))
	if err != nil {
		t.Fatalf("read stored frame: %v", err)
	}
	if !isUniformGrey(t, stored, 6) {
		t.Error("grab was stamped in a room with watermarking off")
	}

	rr := downloadFile(t, fileHandler, fileID, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("download: %d %s", rr.Code, rr.Body.String())
	}
	if !isUniformGrey(t, rr.Body.Bytes(), 6) {
		t.Error("download was marked in a room with watermarking off")
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "" {
		t.Errorf("Cache-Control = %q on an unmarked serve, want empty", cc)
	}
}

// The grab route only takes the player's JPEG capture.
func TestGrabFrame_RejectsNonJPEG(t *testing.T) {
	fileHandler, roomHandler, db, cleanup := setupFileTest(t)
	defer cleanup()

	roomID := createTestRoom(t, roomHandler, db, "grab-mime")
	createTestParticipant(t, db, roomID, "grabber-3")
	token := createJoinToken(t, "grabber-3", "grab-mime")

	rr := postFrameGrab(t, fileHandler, "grab-mime", token, createTestPNG())
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d for a PNG on the grab route", rr.Code, http.StatusBadRequest)
	}
}

// Each download is marked for its own requester, not for the grabber.
func TestDownload_MarksFrameGrabPerRequester(t *testing.T) {
	fileHandler, roomHandler, db, cleanup := setupFileTest(t)
	defer cleanup()

	roomID := createTestRoom(t, roomHandler, db, "grab-serve")
	enableRoomWatermark(t, db, roomID)
	createTestParticipant(t, db, roomID, "alice-000")
	if _, err := db.Exec(`
		INSERT INTO participants (id, room_id, name, role, color, is_admitted)
		VALUES ('bob-00000', ?, 'Bob Reviewer', 'viewer', '#00FF00', TRUE)
	`, roomID); err != nil {
		t.Fatalf("create bob: %v", err)
	}
	aliceToken := createJoinToken(t, "alice-000", "grab-serve")
	bobToken := createJoinToken(t, "bob-00000", "grab-serve")

	fileID := uploadedFileID(t, postFrameGrab(t, fileHandler, "grab-serve", aliceToken,
		createTestFrameJPEG(t, 640, 360)))
	stored, err := os.ReadFile(storedPathOf(t, db, fileID))
	if err != nil {
		t.Fatalf("read stored frame: %v", err)
	}

	aliceRR := downloadFile(t, fileHandler, fileID, aliceToken)
	bobRR := downloadFile(t, fileHandler, fileID, bobToken)
	for name, rr := range map[string]*httptest.ResponseRecorder{"alice": aliceRR, "bob": bobRR} {
		if rr.Code != http.StatusOK {
			t.Fatalf("%s download: %d %s", name, rr.Code, rr.Body.String())
		}
		if cc := rr.Header().Get("Cache-Control"); cc != "private, no-store" {
			t.Errorf("%s Cache-Control = %q, want %q", name, cc, "private, no-store")
		}
		if rr.Header().Get("Content-Type") != "image/jpeg" {
			t.Errorf("%s Content-Type = %q", name, rr.Header().Get("Content-Type"))
		}
		if bytes.Equal(rr.Body.Bytes(), stored) {
			t.Errorf("%s got the stored bytes back; the serve-time mark is missing", name)
		}
	}

	// Different requesters must not get the same body, or the serve-time mark
	// attributes the leak to whoever grabbed the frame.
	if bytes.Equal(aliceRR.Body.Bytes(), bobRR.Body.Bytes()) {
		t.Error("alice and bob received identical bodies; the mark is not per requester")
	}
}

// An admin downloading through the session cookie still gets a marked frame.
func TestDownload_AdminSessionStillMarked(t *testing.T) {
	fileHandler, roomHandler, db, cleanup := setupFileTest(t)
	defer cleanup()

	fileHandler.SetSessionValidator(func(token string) bool { return token == "valid-admin" })

	roomID := createTestRoom(t, roomHandler, db, "grab-admin")
	enableRoomWatermark(t, db, roomID)
	createTestParticipant(t, db, roomID, "grabber-4")
	token := createJoinToken(t, "grabber-4", "grab-admin")

	fileID := uploadedFileID(t, postFrameGrab(t, fileHandler, "grab-admin", token,
		createTestFrameJPEG(t, 480, 270)))

	req := httptest.NewRequest("GET", "/api/files/"+fileID, nil)
	req.SetPathValue("id", fileID)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "valid-admin"})
	rr := httptest.NewRecorder()
	fileHandler.Download(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("admin download: %d %s", rr.Code, rr.Body.String())
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Errorf("Cache-Control = %q, want %q", cc, "private, no-store")
	}
	stored, err := os.ReadFile(storedPathOf(t, db, fileID))
	if err != nil {
		t.Fatalf("read stored frame: %v", err)
	}
	if bytes.Equal(rr.Body.Bytes(), stored) {
		t.Error("admin download was served unmarked")
	}
}

// Ordinary uploads are never stamped, whatever the room's watermark setting: a
// reference PDF or a client's brand deck is not a frame grab.
func TestDownload_OrdinaryUploadNeverMarked(t *testing.T) {
	fileHandler, roomHandler, db, cleanup := setupFileTest(t)
	defer cleanup()

	roomID := createTestRoom(t, roomHandler, db, "grab-upload")
	enableRoomWatermark(t, db, roomID)
	createTestParticipant(t, db, roomID, "uploader-1")
	token := createJoinToken(t, "uploader-1", "grab-upload")

	frame := createTestFrameJPEG(t, 640, 360)
	body, contentType := createMultipartRequest("reference.jpg", frame, "image/jpeg")
	req := httptest.NewRequest("POST", "/api/rooms/grab-upload/files", body)
	req.SetPathValue("slug", "grab-upload")
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Join-Token", token)
	rr := httptest.NewRecorder()
	fileHandler.Upload(rr, req)
	fileID := uploadedFileID(t, rr)

	var origin string
	if err := db.QueryRow("SELECT origin FROM files WHERE id = ?", fileID).Scan(&origin); err != nil {
		t.Fatalf("read origin: %v", err)
	}
	if origin != fileOriginUpload {
		t.Errorf("origin = %q, want %q", origin, fileOriginUpload)
	}

	dl := downloadFile(t, fileHandler, fileID, token)
	if dl.Code != http.StatusOK {
		t.Fatalf("download: %d %s", dl.Code, dl.Body.String())
	}
	if !isUniformGrey(t, dl.Body.Bytes(), 6) {
		t.Error("an ordinary upload was stamped")
	}
}

// The thumbnail endpoint falls back to the stored file when no thumbnail
// exists. For a grabbed frame that fallback would serve the whole picture from
// a route named "thumbnail", so it must downscale instead.
func TestThumbnail_FrameGrabFallbackNeverServesFullFrame(t *testing.T) {
	fileHandler, roomHandler, db, cleanup := setupFileTest(t)
	defer cleanup()

	roomID := createTestRoom(t, roomHandler, db, "grab-thumb")
	enableRoomWatermark(t, db, roomID)
	createTestParticipant(t, db, roomID, "grabber-5")
	token := createJoinToken(t, "grabber-5", "grab-thumb")

	fileID := uploadedFileID(t, postFrameGrab(t, fileHandler, "grab-thumb", token,
		createTestFrameJPEG(t, 1280, 720)))

	// Simulate thumbnail generation having failed at upload time.
	var thumbPath *string
	if err := db.QueryRow("SELECT thumbnail_path FROM files WHERE id = ?", fileID).Scan(&thumbPath); err != nil {
		t.Fatalf("read thumbnail path: %v", err)
	}
	if thumbPath != nil {
		if err := os.Remove(*thumbPath); err != nil {
			t.Fatalf("remove thumbnail: %v", err)
		}
	}
	if _, err := db.Exec("UPDATE files SET thumbnail_path = NULL WHERE id = ?", fileID); err != nil {
		t.Fatalf("clear thumbnail path: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/files/"+fileID+"/thumbnail", nil)
	req.SetPathValue("id", fileID)
	req.Header.Set("X-Join-Token", token)
	rr := httptest.NewRecorder()
	fileHandler.Thumbnail(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("thumbnail: %d %s", rr.Code, rr.Body.String())
	}
	img, err := jpeg.Decode(bytes.NewReader(rr.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode fallback thumbnail: %v", err)
	}
	if img.Bounds().Dx() > 200 || img.Bounds().Dy() > 200 {
		t.Errorf("fallback served %v; the full frame is reachable from the thumbnail route", img.Bounds().Size())
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Errorf("Cache-Control = %q, want %q", cc, "private, no-store")
	}
}

// Fixed input plus fixed spec produces stable output, and the stamp never
// changes the frame's dimensions.
func TestStampImage_DeterministicAndSizePreserving(t *testing.T) {
	src := createTestFrameJPEG(t, 320, 180)
	spec := frameStampSpec{
		Lines:   captureStampLines("Test User", "0123456789abcdef", time.Unix(1_700_000_000, 0).UTC()),
		Opacity: 0.35,
		Scale:   1.0,
	}

	first, err := stampJPEG(src, spec)
	if err != nil {
		t.Fatalf("stamp: %v", err)
	}
	second, err := stampJPEG(src, spec)
	if err != nil {
		t.Fatalf("stamp again: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Error("stamping the same frame twice produced different bytes")
	}

	out, err := jpeg.Decode(bytes.NewReader(first))
	if err != nil {
		t.Fatalf("decode stamped: %v", err)
	}
	if out.Bounds().Dx() != 320 || out.Bounds().Dy() != 180 {
		t.Errorf("stamped size = %v, want 320x180", out.Bounds().Size())
	}
	if isUniformGrey(t, first, 6) {
		t.Error("stamp left the frame unchanged")
	}
}

// An empty spec is a no-op rather than a decode/encode that quietly alters the
// picture's dimensions.
func TestStampImage_NoLinesLeavesSizeIntact(t *testing.T) {
	src := createTestFrameJPEG(t, 200, 120)
	out, err := stampJPEG(src, frameStampSpec{Opacity: 0.35, Scale: 1})
	if err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if !isUniformGrey(t, out, 6) {
		t.Error("an empty spec drew something")
	}
	img, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if img.Bounds().Dx() != 200 || img.Bounds().Dy() != 120 {
		t.Errorf("size = %v, want 200x120", img.Bounds().Size())
	}
}

// A served frame carries two stamp layers: the capture stamp burned in at
// upload and the download stamp composited at serve. They share a rotation and
// a tile pitch, so at the same grid phase they land on each other and neither
// is readable — which destroys exactly the attribution the stamp exists for.
// This asserts the two layers ink mostly different pixels.
func TestStampLayersDoNotOverprint(t *testing.T) {
	base := image.NewRGBA(image.Rect(0, 0, 1280, 720))
	draw.Draw(base, base.Bounds(), &image.Uniform{C: color.RGBA{R: 128, G: 128, B: 128, A: 255}}, image.Point{}, draw.Src)

	spec := func(lines []string, phase float64) frameStampSpec {
		return frameStampSpec{Lines: lines, Opacity: 0.35, Scale: 1, Phase: phase}
	}
	captured, err := stampImage(base, spec(
		captureStampLines("Dana Reyes", "9f3c1a72b4d5e6f7", time.Unix(1_770_000_000, 0).UTC()),
		captureStampPhase))
	if err != nil {
		t.Fatalf("capture stamp: %v", err)
	}
	downloaded, err := stampImage(base, spec(
		downloadStampLines("Sam Okafor", "1122334455667788", time.Unix(1_770_000_600, 0).UTC()),
		downloadStampPhase))
	if err != nil {
		t.Fatalf("download stamp: %v", err)
	}

	inked := func(img image.Image, x, y int) bool {
		r, g, b, _ := img.At(x, y).RGBA()
		return abs8(int(r>>8)-128) > 2 || abs8(int(g>>8)-128) > 2 || abs8(int(b>>8)-128) > 2
	}

	var countA, countB, both int
	for y := base.Bounds().Min.Y; y < base.Bounds().Max.Y; y++ {
		for x := base.Bounds().Min.X; x < base.Bounds().Max.X; x++ {
			a, b := inked(captured, x, y), inked(downloaded, x, y)
			if a {
				countA++
			}
			if b {
				countB++
			}
			if a && b {
				both++
			}
		}
	}
	if countA == 0 || countB == 0 {
		t.Fatalf("a stamp layer drew nothing: capture=%d download=%d", countA, countB)
	}

	smaller := countA
	if countB < smaller {
		smaller = countB
	}
	overlap := float64(both) / float64(smaller)
	// Identical grids at the same phase overlap almost completely. Interleaved
	// layers still cross where the diagonals meet, so this is a ceiling on
	// collision, not a demand for zero.
	if overlap > 0.4 {
		t.Errorf("stamp layers overlap %.0f%% of the smaller layer; they overprint and neither reads", overlap*100)
	}
}

func abs8(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// The short-ID ties a leaked frame back to an audit row, so a long display name
// must not be what pushes it off the end of the line.
func TestStampLinesKeepShortIDForLongNames(t *testing.T) {
	long := "Bartholomew Fitzgerald-Montgomery III"
	id := "9f3c1a72b4d5e6f7"
	for _, line := range append(
		captureStampLines(long, id, time.Unix(1_770_000_000, 0).UTC()),
		downloadStampLines(long, id, time.Unix(1_770_000_000, 0).UTC())...,
	) {
		if strings.Contains(line, "captured by") || strings.Contains(line, "downloaded by") {
			if !strings.Contains(line, "(9f3c1a72)") {
				t.Errorf("line %q lost the participant short-ID", line)
			}
			if len([]rune(line)) > 60 {
				t.Errorf("line %q is %d runes; too long for the fixed tile box", line, len([]rune(line)))
			}
		}
	}
}

// Serve-time marking is one decode + composite + encode per download, on an
// authenticated endpoint with no per-file rate limit. Keep the cost visible.
func BenchmarkStampJPEG4K(b *testing.B) {
	img := image.NewRGBA(image.Rect(0, 0, 3840, 2160))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.RGBA{R: 90, G: 110, B: 130, A: 255}}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 92}); err != nil {
		b.Fatal(err)
	}
	spec := frameStampSpec{
		Lines:   downloadStampLines("Sam Okafor", "1122334455667788", time.Unix(1_770_000_000, 0).UTC()),
		Opacity: 0.35, Scale: 1, Phase: downloadStampPhase,
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := stampJPEG(buf.Bytes(), spec); err != nil {
			b.Fatal(err)
		}
	}
}
