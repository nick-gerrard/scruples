package auth

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"scruples/internal/db"
	"scruples/internal/web"
)

func setupAvatarTest(t *testing.T) (*web.App, *http.ServeMux, int64, *http.Cookie) {
	t.Helper()
	t.Chdir("../..")

	database, err := db.Open(filepath.Join(t.TempDir(), "avatar.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	app := web.NewApp(database)
	app.UploadsDir = t.TempDir()

	mux := http.NewServeMux()
	Routes(mux, app)

	res, err := database.Exec(`INSERT INTO users (username, password_hash) VALUES ('alice', 'hash')`)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	uid, _ := res.LastInsertId()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	if err := app.CreateSession(rec, req, uid); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 session cookie, got %d", len(cookies))
	}
	return app, mux, uid, cookies[0]
}

func makeMultipartUpload(t *testing.T, field, filename string, payload []byte) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile(field, filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write(payload); err != nil {
		t.Fatalf("write multipart payload: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return &body, mw.FormDataContentType()
}

func encodeTestPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{R: 99, G: 102, B: 241, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

func encodeTestJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 20, G: 184, B: 166, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	return buf.Bytes()
}

func encodeTestGIF(t *testing.T, w, h int) []byte {
	t.Helper()
	pal := color.Palette{color.RGBA{R: 244, G: 63, B: 94, A: 255}, color.RGBA{R: 251, G: 191, B: 36, A: 255}}
	frame1 := image.NewPaletted(image.Rect(0, 0, w, h), pal)
	frame2 := image.NewPaletted(image.Rect(0, 0, w, h), pal)
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, &gif.GIF{
		Image: []*image.Paletted{frame1, frame2},
		Delay: []int{10, 10},
	}); err != nil {
		t.Fatalf("gif.EncodeAll: %v", err)
	}
	return buf.Bytes()
}

// buildFakeHugePNGHeader builds a minimal PNG signature + IHDR chunk claiming
// width x height pixels without allocating a giant image in memory.
func buildFakeHugePNGHeader(width, height uint32) []byte {
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})

	var ihdr [13]byte
	binary.BigEndian.PutUint32(ihdr[0:4], width)
	binary.BigEndian.PutUint32(ihdr[4:8], height)
	ihdr[8] = 8 // bit depth
	ihdr[9] = 2 // RGB color type

	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(ihdr)))
	buf.Write(length[:])
	buf.WriteString("IHDR")
	buf.Write(ihdr[:])

	crc := crc32.NewIEEE()
	_, _ = crc.Write([]byte("IHDR"))
	_, _ = crc.Write(ihdr[:])
	var crcBuf [4]byte
	binary.BigEndian.PutUint32(crcBuf[:], crc.Sum32())
	buf.Write(crcBuf[:])
	return buf.Bytes()
}

func assertDiskAvatar256JPEG(t *testing.T, uploadsDir, relPath string) {
	t.Helper()
	full := filepath.Join(uploadsDir, filepath.FromSlash(relPath))
	f, err := os.Open(full)
	if err != nil {
		t.Fatalf("open uploaded avatar %s: %v", full, err)
	}
	defer f.Close()

	cfg, format, err := image.DecodeConfig(f)
	if err != nil {
		t.Fatalf("DecodeConfig(%s): %v", full, err)
	}
	if format != "jpeg" {
		t.Fatalf("uploaded avatar format = %q, want jpeg", format)
	}
	if cfg.Width != 256 || cfg.Height != 256 {
		t.Fatalf("uploaded avatar dimensions = %dx%d, want 256x256", cfg.Width, cfg.Height)
	}
}

func TestAvatarUploadPNGJPEGAndGIF(t *testing.T) {
	app, mux, uid, cookie := setupAvatarTest(t)

	// 1. Upload PNG (non-square 400x200).
	body, ct := makeMultipartUpload(t, "avatar", "pic.png", encodeTestPNG(t, 400, 200))
	req := httptest.NewRequest(http.MethodPost, "/account/avatar", body)
	req.Header.Set("Content-Type", ct)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PNG upload status = %d, want 200", rec.Code)
	}

	var path1 string
	if err := app.DB.QueryRow(`SELECT avatar_path FROM users WHERE id = ?`, uid).Scan(&path1); err != nil {
		t.Fatalf("query avatar_path after PNG: %v", err)
	}
	if !strings.HasPrefix(path1, "avatars/"+strconv.FormatInt(uid, 10)+"-") || !strings.HasSuffix(path1, ".jpg") {
		t.Fatalf("unexpected avatar_path = %q", path1)
	}
	assertDiskAvatar256JPEG(t, app.UploadsDir, path1)

	// 2. Upload JPEG (non-square 120x300) — must replace old file on disk.
	body2, ct2 := makeMultipartUpload(t, "avatar", "pic.jpg", encodeTestJPEG(t, 120, 300))
	req2 := httptest.NewRequest(http.MethodPost, "/account/avatar", body2)
	req2.Header.Set("Content-Type", ct2)
	req2.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("JPEG upload status = %d, want 200", rec2.Code)
	}

	var path2 string
	if err := app.DB.QueryRow(`SELECT avatar_path FROM users WHERE id = ?`, uid).Scan(&path2); err != nil {
		t.Fatalf("query avatar_path after JPEG: %v", err)
	}
	if path2 == path1 {
		t.Fatalf("expected new avatar_path after second upload, still %q", path2)
	}
	assertDiskAvatar256JPEG(t, app.UploadsDir, path2)

	if _, err := os.Stat(filepath.Join(app.UploadsDir, filepath.FromSlash(path1))); !os.IsNotExist(err) {
		t.Fatalf("previous avatar file %s should have been deleted, stat err = %v", path1, err)
	}

	// 3. Upload animated GIF — first frame should produce a 256x256 JPEG.
	body3, ct3 := makeMultipartUpload(t, "avatar", "anim.gif", encodeTestGIF(t, 80, 100))
	req3 := httptest.NewRequest(http.MethodPost, "/account/avatar", body3)
	req3.Header.Set("Content-Type", ct3)
	req3.AddCookie(cookie)
	rec3 := httptest.NewRecorder()
	mux.ServeHTTP(rec3, req3)

	if rec3.Code != http.StatusOK {
		t.Fatalf("GIF upload status = %d, want 200", rec3.Code)
	}
	var path3 string
	if err := app.DB.QueryRow(`SELECT avatar_path FROM users WHERE id = ?`, uid).Scan(&path3); err != nil {
		t.Fatalf("query avatar_path after GIF: %v", err)
	}
	assertDiskAvatar256JPEG(t, app.UploadsDir, path3)
}

func TestAvatarUploadRejectsOversizedBytesAndDimensions(t *testing.T) {
	app, mux, uid, cookie := setupAvatarTest(t)

	// 1. Oversized byte count (> 5 MB).
	big := make([]byte, maxAvatarUploadBytes+1024)
	body, ct := makeMultipartUpload(t, "avatar", "huge.jpg", big)
	req := httptest.NewRequest(http.MethodPost, "/account/avatar", body)
	req.Header.Set("Content-Type", ct)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized bytes status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}

	// 2. Oversized dimensions (6001x6001 PNG header).
	hugeDim := buildFakeHugePNGHeader(6001, 6001)
	body2, ct2 := makeMultipartUpload(t, "avatar", "huge-dim.png", hugeDim)
	req2 := httptest.NewRequest(http.MethodPost, "/account/avatar", body2)
	req2.Header.Set("Content-Type", ct2)
	req2.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("oversized dimensions status = %d, want %d", rec2.Code, http.StatusBadRequest)
	}

	var avatarPath sql.NullString
	if err := app.DB.QueryRow(`SELECT avatar_path FROM users WHERE id = ?`, uid).Scan(&avatarPath); err != nil {
		t.Fatalf("query avatar_path: %v", err)
	}
	if avatarPath.Valid {
		t.Fatalf("avatar_path = %q, want NULL after rejected uploads", avatarPath.String)
	}
}

func TestAvatarServeAndDeleteFallback(t *testing.T) {
	app, mux, uid, cookie := setupAvatarTest(t)
	uidStr := strconv.FormatInt(uid, 10)

	// Before upload: GET /avatar/{id} serves generated SVG with cache headers.
	req := httptest.NewRequest(http.MethodGet, "/avatar/"+uidStr, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /avatar/%s status = %d, want 200", uidStr, rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/svg+xml") {
		t.Fatalf("Content-Type before upload = %q, want image/svg+xml", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=300" {
		t.Fatalf("Cache-Control = %q, want public, max-age=300", cc)
	}
	svgETag := rec.Header().Get("ETag")
	if svgETag == "" {
		t.Fatal("missing ETag on SVG response")
	}

	// Conditional request with matching ETag returns 304.
	req304 := httptest.NewRequest(http.MethodGet, "/avatar/"+uidStr, nil)
	req304.Header.Set("If-None-Match", svgETag)
	rec304 := httptest.NewRecorder()
	mux.ServeHTTP(rec304, req304)
	if rec304.Code != http.StatusNotModified {
		t.Fatalf("GET /avatar/%s with If-None-Match status = %d, want 304", uidStr, rec304.Code)
	}

	// Upload a PNG.
	body, ct := makeMultipartUpload(t, "avatar", "pic.png", encodeTestPNG(t, 128, 128))
	upReq := httptest.NewRequest(http.MethodPost, "/account/avatar", body)
	upReq.Header.Set("Content-Type", ct)
	upReq.AddCookie(cookie)
	upRec := httptest.NewRecorder()
	mux.ServeHTTP(upRec, upReq)
	if upRec.Code != http.StatusOK {
		t.Fatalf("upload status = %d, want 200", upRec.Code)
	}

	var savedPath string
	if err := app.DB.QueryRow(`SELECT avatar_path FROM users WHERE id = ?`, uid).Scan(&savedPath); err != nil {
		t.Fatalf("query avatar_path: %v", err)
	}

	// After upload: GET /avatar/{id} serves JPEG.
	jpgReq := httptest.NewRequest(http.MethodGet, "/avatar/"+uidStr, nil)
	jpgRec := httptest.NewRecorder()
	mux.ServeHTTP(jpgRec, jpgReq)
	if jpgRec.Code != http.StatusOK {
		t.Fatalf("GET /avatar/%s after upload status = %d, want 200", uidStr, jpgRec.Code)
	}
	if ct := jpgRec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("Content-Type after upload = %q, want image/jpeg", ct)
	}
	if jpgRec.Header().Get("ETag") == "" || jpgRec.Header().Get("ETag") == svgETag {
		t.Fatalf("expected distinct non-empty ETag for uploaded JPEG, got %q", jpgRec.Header().Get("ETag"))
	}

	// Delete avatar: file is removed, DB is cleared, GET falls back to SVG.
	delReq := httptest.NewRequest(http.MethodPost, "/account/avatar/delete", nil)
	delReq.AddCookie(cookie)
	delRec := httptest.NewRecorder()
	mux.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusOK {
		t.Fatalf("DELETE avatar status = %d, want 200", delRec.Code)
	}

	if _, err := os.Stat(filepath.Join(app.UploadsDir, filepath.FromSlash(savedPath))); !os.IsNotExist(err) {
		t.Fatalf("deleted avatar file still exists: stat err = %v", err)
	}
	var nullPath sql.NullString
	if err := app.DB.QueryRow(`SELECT avatar_path FROM users WHERE id = ?`, uid).Scan(&nullPath); err != nil {
		t.Fatalf("query avatar_path after delete: %v", err)
	}
	if nullPath.Valid {
		t.Fatalf("avatar_path after delete = %q, want NULL", nullPath.String)
	}

	fbReq := httptest.NewRequest(http.MethodGet, "/avatar/"+uidStr, nil)
	fbRec := httptest.NewRecorder()
	mux.ServeHTTP(fbRec, fbReq)
	if fbRec.Code != http.StatusOK {
		t.Fatalf("GET /avatar/%s fallback status = %d, want 200", uidStr, fbRec.Code)
	}
	if ct := fbRec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/svg+xml") {
		t.Fatalf("Content-Type after delete = %q, want image/svg+xml", ct)
	}

	// Unknown user ID serves deterministic SVG derived from ID.
	unkReq := httptest.NewRequest(http.MethodGet, "/avatar/999999", nil)
	unkRec := httptest.NewRecorder()
	mux.ServeHTTP(unkRec, unkReq)
	if unkRec.Code != http.StatusOK {
		t.Fatalf("GET /avatar/999999 status = %d, want 200", unkRec.Code)
	}
	if ct := unkRec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/svg+xml") {
		t.Fatalf("Content-Type for unknown id = %q, want image/svg+xml", ct)
	}
	if !bytes.Equal(unkRec.Body.Bytes(), web.GenerateAvatarSVG(999999, "")) {
		t.Fatal("unknown id SVG did not match web.GenerateAvatarSVG(999999, \"\")")
	}

	// Unauthenticated upload/delete are redirected to /login.
	unauthReq := httptest.NewRequest(http.MethodPost, "/account/avatar/delete", nil)
	unauthRec := httptest.NewRecorder()
	mux.ServeHTTP(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusSeeOther || unauthRec.Header().Get("Location") != "/login" {
		t.Fatalf("unauthenticated delete = (%d, %q), want (303, /login)", unauthRec.Code, unauthRec.Header().Get("Location"))
	}
}
