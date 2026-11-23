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
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"scruples/internal/db"
	"scruples/internal/web"
)

func createTestUserWithSession(t *testing.T, app *web.App, username string) (int64, *http.Cookie) {
	t.Helper()
	res, err := app.DB.Exec(`INSERT INTO users (username, password_hash) VALUES (?, 'hash')`, username)
	if err != nil {
		t.Fatalf("insert user %s: %v", username, err)
	}
	uid, _ := res.LastInsertId()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	if err := app.CreateSession(rec, req, uid); err != nil {
		t.Fatalf("CreateSession(%s): %v", username, err)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 session cookie for %s, got %d", username, len(cookies))
	}
	return uid, cookies[0]
}

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

	uid, cookie := createTestUserWithSession(t, app, "alice")
	return app, mux, uid, cookie
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

// injectEXIFOrientation inserts a valid EXIF APP1 segment with the given
// orientation right after the JPEG SOI marker (0xFF 0xD8).
func injectEXIFOrientation(t *testing.T, jpegData []byte, orientation uint16, bigEndian bool) []byte {
	t.Helper()
	if len(jpegData) < 2 || jpegData[0] != 0xFF || jpegData[1] != 0xD8 {
		t.Fatal("input is not a JPEG")
	}

	var order binary.ByteOrder = binary.LittleEndian
	byteOrderTag := "II"
	if bigEndian {
		order = binary.BigEndian
		byteOrderTag = "MM"
	}

	// Build minimal TIFF with 1 IFD0 entry (tag 0x0112 Orientation).
	var tiff bytes.Buffer
	tiff.WriteString(byteOrderTag)
	_ = binary.Write(&tiff, order, uint16(42)) // TIFF magic
	_ = binary.Write(&tiff, order, uint32(8))  // IFD0 offset
	_ = binary.Write(&tiff, order, uint16(1))  // 1 entry
	_ = binary.Write(&tiff, order, uint16(0x0112))
	_ = binary.Write(&tiff, order, uint16(3)) // SHORT
	_ = binary.Write(&tiff, order, uint32(1)) // count = 1
	_ = binary.Write(&tiff, order, orientation)
	_ = binary.Write(&tiff, order, uint16(0)) // padding
	_ = binary.Write(&tiff, order, uint32(0)) // next IFD = 0

	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	var out bytes.Buffer
	out.Write(jpegData[:2]) // SOI
	out.Write([]byte{0xFF, 0xE1})
	_ = binary.Write(&out, binary.BigEndian, uint16(len(payload)+2))
	out.Write(payload)
	out.Write(jpegData[2:])
	return out.Bytes()
}

func countFilesInDir(t *testing.T, dir string) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%s): %v", dir, err)
	}
	return count
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

	// 2. Oversized dimensions (4097x4097 PNG header, exceeds 4096x4096 cap).
	hugeDim := buildFakeHugePNGHeader(4097, 4097)
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
	if n := countFilesInDir(t, app.UploadsDir); n != 0 {
		t.Fatalf("expected 0 files in uploads dir after rejected uploads, got %d", n)
	}
}

func TestAvatarUploadSemaphoreBusyReturns503(t *testing.T) {
	_, mux, _, cookie := setupAvatarTest(t)

	origTimeout := avatarSemTimeout
	avatarSemTimeout = 50 * time.Millisecond
	t.Cleanup(func() { avatarSemTimeout = origTimeout })

	avatarProcessSem <- struct{}{}
	t.Cleanup(func() { <-avatarProcessSem })

	body, ct := makeMultipartUpload(t, "avatar", "pic.png", encodeTestPNG(t, 64, 64))
	req := httptest.NewRequest(http.MethodPost, "/account/avatar", body)
	req.Header.Set("Content-Type", ct)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("busy upload status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "try again") {
		t.Fatalf("503 response body missing 'try again': %q", rec.Body.String())
	}
}

func TestAvatarEXIFOrientation(t *testing.T) {
	app, mux, uid, cookie := setupAvatarTest(t)

	// Desired upright 100x100 quadrant colors:
	//   TL = Red, TR = Green, BL = Blue, BR = Yellow.
	red := color.RGBA{R: 230, G: 20, B: 20, A: 255}
	green := color.RGBA{R: 20, G: 220, B: 20, A: 255}
	blue := color.RGBA{R: 20, G: 20, B: 230, A: 255}
	yellow := color.RGBA{R: 235, G: 230, B: 20, A: 255}

	makeUpright := func() *image.RGBA {
		img := image.NewRGBA(image.Rect(0, 0, 100, 100))
		for y := 0; y < 100; y++ {
			for x := 0; x < 100; x++ {
				switch {
				case x < 50 && y < 50:
					img.Set(x, y, red)
				case x >= 50 && y < 50:
					img.Set(x, y, green)
				case x < 50 && y >= 50:
					img.Set(x, y, blue)
				default:
					img.Set(x, y, yellow)
				}
			}
		}
		return img
	}

	// Build stored image by applying the inverse of each orientation so that
	// applying the orientation tag restores the upright quadrant layout.
	cases := []struct {
		name        string
		orientation uint16
		inverse     int
		bigEndian   bool
	}{
		{name: "orientation 3 (180 deg, LE)", orientation: 3, inverse: 3, bigEndian: false},
		{name: "orientation 6 (90 CW, BE)", orientation: 6, inverse: 8, bigEndian: true},
		{name: "orientation 8 (270 CW, LE)", orientation: 8, inverse: 6, bigEndian: false},
	}

	assertClose := func(t *testing.T, label string, got color.Color, want color.RGBA) {
		t.Helper()
		r, g, b, _ := got.RGBA()
		gr, gg, gb := int(r>>8), int(g>>8), int(b>>8)
		abs := func(n int) int {
			if n < 0 {
				return -n
			}
			return n
		}
		if abs(gr-int(want.R)) > 35 || abs(gg-int(want.G)) > 35 || abs(gb-int(want.B)) > 35 {
			t.Errorf("%s pixel = RGB(%d,%d,%d), want ~RGB(%d,%d,%d)", label, gr, gg, gb, want.R, want.G, want.B)
		}
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stored := applyOrientation(makeUpright(), tc.inverse)
			var raw bytes.Buffer
			if err := jpeg.Encode(&raw, stored, &jpeg.Options{Quality: 95}); err != nil {
				t.Fatalf("jpeg.Encode: %v", err)
			}
			withEXIF := injectEXIFOrientation(t, raw.Bytes(), tc.orientation, tc.bigEndian)

			body, ct := makeMultipartUpload(t, "avatar", "photo.jpg", withEXIF)
			req := httptest.NewRequest(http.MethodPost, "/account/avatar", body)
			req.Header.Set("Content-Type", ct)
			req.AddCookie(cookie)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("upload status = %d, want 200", rec.Code)
			}

			var relPath string
			if err := app.DB.QueryRow(`SELECT avatar_path FROM users WHERE id = ?`, uid).Scan(&relPath); err != nil {
				t.Fatalf("query avatar_path: %v", err)
			}
			f, err := os.Open(filepath.Join(app.UploadsDir, filepath.FromSlash(relPath)))
			if err != nil {
				t.Fatalf("open result: %v", err)
			}
			defer f.Close()
			out, _, err := image.Decode(f)
			if err != nil {
				t.Fatalf("decode result: %v", err)
			}

			// Sample centers of the 4 quadrants of the 256x256 output.
			assertClose(t, "top-left", out.At(64, 64), red)
			assertClose(t, "top-right", out.At(192, 64), green)
			assertClose(t, "bottom-left", out.At(64, 192), blue)
			assertClose(t, "bottom-right", out.At(192, 192), yellow)
		})
	}
}

func TestAvatarUploadInvalidInputsAndAuth(t *testing.T) {
	app, mux, uid, cookie := setupAvatarTest(t)

	// 1. Non-image bytes named .png -> 400 and no file left behind.
	body, ct := makeMultipartUpload(t, "avatar", "fake.png", []byte("not actually a png file at all"))
	req := httptest.NewRequest(http.MethodPost, "/account/avatar", body)
	req.Header.Set("Content-Type", ct)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-image .png status = %d, want 400", rec.Code)
	}
	if n := countFilesInDir(t, app.UploadsDir); n != 0 {
		t.Fatalf("non-image upload left %d file(s) behind", n)
	}

	// 2. Missing avatar field -> 400 and no file left behind.
	var emptyBody bytes.Buffer
	mw := multipart.NewWriter(&emptyBody)
	_ = mw.WriteField("other", "value")
	_ = mw.Close()
	req2 := httptest.NewRequest(http.MethodPost, "/account/avatar", &emptyBody)
	req2.Header.Set("Content-Type", mw.FormDataContentType())
	req2.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("missing avatar field status = %d, want 400", rec2.Code)
	}
	if n := countFilesInDir(t, app.UploadsDir); n != 0 {
		t.Fatalf("missing avatar field left %d file(s) behind", n)
	}

	// 3. Unauthenticated POST /account/avatar -> 303 redirect (browser) and 401 (htmx), no write.
	body3, ct3 := makeMultipartUpload(t, "avatar", "pic.png", encodeTestPNG(t, 64, 64))
	req3 := httptest.NewRequest(http.MethodPost, "/account/avatar", body3)
	req3.Header.Set("Content-Type", ct3)
	rec3 := httptest.NewRecorder()
	mux.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusSeeOther || rec3.Header().Get("Location") != "/login" {
		t.Fatalf("unauthenticated upload = (%d, %q), want (303, /login)", rec3.Code, rec3.Header().Get("Location"))
	}

	body4, ct4 := makeMultipartUpload(t, "avatar", "pic.png", encodeTestPNG(t, 64, 64))
	req4 := httptest.NewRequest(http.MethodPost, "/account/avatar", body4)
	req4.Header.Set("Content-Type", ct4)
	req4.Header.Set("HX-Request", "true")
	rec4 := httptest.NewRecorder()
	mux.ServeHTTP(rec4, req4)
	if rec4.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated htmx upload status = %d, want 401", rec4.Code)
	}

	var avatarPath sql.NullString
	if err := app.DB.QueryRow(`SELECT avatar_path FROM users WHERE id = ?`, uid).Scan(&avatarPath); err != nil {
		t.Fatalf("query avatar_path: %v", err)
	}
	if avatarPath.Valid {
		t.Fatalf("avatar_path = %q, want NULL", avatarPath.String)
	}
	if n := countFilesInDir(t, app.UploadsDir); n != 0 {
		t.Fatalf("unauthenticated upload wrote %d file(s)", n)
	}
}

func TestAvatarUserIsolation(t *testing.T) {
	app, mux, uidA, cookieA := setupAvatarTest(t)
	uidB, cookieB := createTestUserWithSession(t, app, "bob")

	// User B uploads an avatar.
	bodyB, ctB := makeMultipartUpload(t, "avatar", "bob.png", encodeTestPNG(t, 96, 96))
	reqB := httptest.NewRequest(http.MethodPost, "/account/avatar", bodyB)
	reqB.Header.Set("Content-Type", ctB)
	reqB.AddCookie(cookieB)
	recB := httptest.NewRecorder()
	mux.ServeHTTP(recB, reqB)
	if recB.Code != http.StatusOK {
		t.Fatalf("user B upload status = %d, want 200", recB.Code)
	}

	var pathB string
	if err := app.DB.QueryRow(`SELECT avatar_path FROM users WHERE id = ?`, uidB).Scan(&pathB); err != nil {
		t.Fatalf("query user B avatar_path: %v", err)
	}
	fullB := filepath.Join(app.UploadsDir, filepath.FromSlash(pathB))

	// User A uploads and deletes their own avatar.
	bodyA, ctA := makeMultipartUpload(t, "avatar", "alice.png", encodeTestPNG(t, 96, 96))
	reqA := httptest.NewRequest(http.MethodPost, "/account/avatar", bodyA)
	reqA.Header.Set("Content-Type", ctA)
	reqA.AddCookie(cookieA)
	recA := httptest.NewRecorder()
	mux.ServeHTTP(recA, reqA)
	if recA.Code != http.StatusOK {
		t.Fatalf("user A upload status = %d, want 200", recA.Code)
	}

	delA := httptest.NewRequest(http.MethodPost, "/account/avatar/delete", nil)
	delA.AddCookie(cookieA)
	recDelA := httptest.NewRecorder()
	mux.ServeHTTP(recDelA, delA)
	if recDelA.Code != http.StatusOK {
		t.Fatalf("user A delete status = %d, want 200", recDelA.Code)
	}

	// Verify user A is cleared while user B's DB row and file are untouched.
	var pathA sql.NullString
	if err := app.DB.QueryRow(`SELECT avatar_path FROM users WHERE id = ?`, uidA).Scan(&pathA); err != nil {
		t.Fatalf("query user A avatar_path: %v", err)
	}
	if pathA.Valid {
		t.Fatalf("user A avatar_path = %q, want NULL", pathA.String)
	}

	var pathBAfter string
	if err := app.DB.QueryRow(`SELECT avatar_path FROM users WHERE id = ?`, uidB).Scan(&pathBAfter); err != nil {
		t.Fatalf("query user B avatar_path after: %v", err)
	}
	if pathBAfter != pathB {
		t.Fatalf("user B avatar_path changed from %q to %q", pathB, pathBAfter)
	}
	if _, err := os.Stat(fullB); err != nil {
		t.Fatalf("user B avatar file missing after user A actions: %v", err)
	}
}

func TestIsSafeAvatarPath(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"avatars/1-0123456789abcdef.jpg", true},
		{"avatars/42-abc.jpg", true},
		{"", false},
		{"../etc/passwd", false},
		{"/etc/passwd", false},
		{"/avatars/1-abc.jpg", false},
		{`avatars\1-abc.jpg`, false},
		{`avatars\..\x`, false},
		{"avatars/../x", false},
		{"avatars/../../etc/passwd", false},
		{"avatars/sub/../1-abc.jpg", false},
		{"other/1-abc.jpg", false},
	}
	for _, tc := range tests {
		if got := isSafeAvatarPath(tc.in); got != tc.want {
			t.Errorf("isSafeAvatarPath(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestAvatarServeAndDeleteFallback(t *testing.T) {
	app, mux, uid, cookie := setupAvatarTest(t)
	uidStr := strconv.FormatInt(uid, 10)
	const immutableCC = "public, max-age=31536000, immutable"

	// Before upload: unversioned GET gets no-cache + ETag.
	req := httptest.NewRequest(http.MethodGet, "/avatar/"+uidStr, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /avatar/%s status = %d, want 200", uidStr, rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/svg+xml") {
		t.Fatalf("Content-Type before upload = %q, want image/svg+xml", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("unversioned Cache-Control = %q, want no-cache", cc)
	}
	svgETag := rec.Header().Get("ETag")
	if svgETag == "" {
		t.Fatal("missing ETag on SVG response")
	}

	// Matching ?v=gen gets immutable Cache-Control.
	genReq := httptest.NewRequest(http.MethodGet, "/avatar/"+uidStr+"?v=gen", nil)
	genRec := httptest.NewRecorder()
	mux.ServeHTTP(genRec, genReq)
	if cc := genRec.Header().Get("Cache-Control"); cc != immutableCC {
		t.Fatalf("GET /avatar/%s?v=gen Cache-Control = %q, want %q", uidStr, cc, immutableCC)
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
	vHash := web.AvatarVersion(savedPath)

	// After upload: stale ?v=gen gets no-cache, while matching ?v=<hash> gets immutable.
	staleReq := httptest.NewRequest(http.MethodGet, "/avatar/"+uidStr+"?v=gen", nil)
	staleRec := httptest.NewRecorder()
	mux.ServeHTTP(staleRec, staleReq)
	if staleRec.Code != http.StatusOK {
		t.Fatalf("GET /avatar/%s?v=gen after upload status = %d, want 200", uidStr, staleRec.Code)
	}
	if ct := staleRec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("Content-Type after upload = %q, want image/jpeg", ct)
	}
	if cc := staleRec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("stale ?v=gen Cache-Control after upload = %q, want no-cache", cc)
	}

	jpgReq := httptest.NewRequest(http.MethodGet, "/avatar/"+uidStr+"?v="+vHash, nil)
	jpgRec := httptest.NewRecorder()
	mux.ServeHTTP(jpgRec, jpgReq)
	if jpgRec.Code != http.StatusOK {
		t.Fatalf("GET /avatar/%s?v=%s status = %d, want 200", uidStr, vHash, jpgRec.Code)
	}
	if ct := jpgRec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("Content-Type = %q, want image/jpeg", ct)
	}
	if cc := jpgRec.Header().Get("Cache-Control"); cc != immutableCC {
		t.Fatalf("matching ?v=<hash> Cache-Control = %q, want %q", cc, immutableCC)
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

	// Stale ?v=<oldHash> now gets no-cache + SVG; ?v=gen gets immutable + SVG.
	fbReq := httptest.NewRequest(http.MethodGet, "/avatar/"+uidStr+"?v="+vHash, nil)
	fbRec := httptest.NewRecorder()
	mux.ServeHTTP(fbRec, fbReq)
	if fbRec.Code != http.StatusOK {
		t.Fatalf("GET /avatar/%s fallback status = %d, want 200", uidStr, fbRec.Code)
	}
	if ct := fbRec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/svg+xml") {
		t.Fatalf("Content-Type after delete = %q, want image/svg+xml", ct)
	}
	if cc := fbRec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("stale ?v=<oldHash> after delete Cache-Control = %q, want no-cache", cc)
	}

	genAfterReq := httptest.NewRequest(http.MethodGet, "/avatar/"+uidStr+"?v=gen", nil)
	genAfterRec := httptest.NewRecorder()
	mux.ServeHTTP(genAfterRec, genAfterReq)
	if cc := genAfterRec.Header().Get("Cache-Control"); cc != immutableCC {
		t.Fatalf("?v=gen after delete Cache-Control = %q, want %q", cc, immutableCC)
	}

	// Unknown user ID serves deterministic SVG derived from ID.
	unkReq := httptest.NewRequest(http.MethodGet, "/avatar/999999?v=gen", nil)
	unkRec := httptest.NewRecorder()
	mux.ServeHTTP(unkRec, unkReq)
	if unkRec.Code != http.StatusOK {
		t.Fatalf("GET /avatar/999999 status = %d, want 200", unkRec.Code)
	}
	if ct := unkRec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/svg+xml") {
		t.Fatalf("Content-Type for unknown id = %q, want image/svg+xml", ct)
	}
	if cc := unkRec.Header().Get("Cache-Control"); cc != immutableCC {
		t.Fatalf("unknown id ?v=gen Cache-Control = %q, want %q", cc, immutableCC)
	}
	if !bytes.Equal(unkRec.Body.Bytes(), web.GenerateAvatarSVG(999999, "")) {
		t.Fatal("unknown id SVG did not match web.GenerateAvatarSVG(999999, \"\")")
	}

	// Unauthenticated delete is redirected to /login.
	unauthReq := httptest.NewRequest(http.MethodPost, "/account/avatar/delete", nil)
	unauthRec := httptest.NewRecorder()
	mux.ServeHTTP(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusSeeOther || unauthRec.Header().Get("Location") != "/login" {
		t.Fatalf("unauthenticated delete = (%d, %q), want (303, /login)", unauthRec.Code, unauthRec.Header().Get("Location"))
	}
}

func TestAvatarServeBotUser(t *testing.T) {
	app, mux, _, _ := setupAvatarTest(t)

	var botID int64
	if err := app.DB.QueryRow(`SELECT id FROM users WHERE is_bot = 1 AND username = 'Claude'`).Scan(&botID); err != nil {
		t.Fatalf("find seeded Claude bot user: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/avatar/"+strconv.FormatInt(botID, 10)+"?v=gen", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /avatar/%d status = %d, want 200", botID, rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/svg+xml") {
		t.Fatalf("bot Content-Type = %q, want image/svg+xml", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Fatalf("bot Cache-Control = %q, want public, max-age=31536000, immutable", cc)
	}
	if !bytes.Equal(rec.Body.Bytes(), web.GenerateBotAvatarSVG()) {
		t.Fatal("bot user did not receive distinctive GenerateBotAvatarSVG()")
	}
}
