package auth

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/image/draw"

	"scruples/internal/web"
)

const (
	maxAvatarUploadBytes = 5 << 20 // 5 MB
	maxAvatarDimension   = 6000
	avatarOutputSize     = 256
	avatarJPEGQuality    = 85
)

var (
	errUnsupportedFormat = errors.New("unsupported image format")
	errImageTooLarge     = errors.New("image dimensions exceed limit")
)

func avatarRoutes(mux *http.ServeMux, app *web.App) {
	mux.HandleFunc("POST /account/avatar", app.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		handleAvatarUpload(w, r, app)
	}))

	mux.HandleFunc("POST /account/avatar/delete", app.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		handleAvatarDelete(w, r, app)
	}))

	mux.HandleFunc("GET /avatar/{id}", func(w http.ResponseWriter, r *http.Request) {
		handleAvatarServe(w, r, app)
	})
}

func handleAvatarUpload(w http.ResponseWriter, r *http.Request, app *web.App) {
	user := app.CurrentUser(r)

	fail := func(status int, msg string) {
		w.WriteHeader(status)
		app.Render(w, r, "account.html", accountData{AvatarError: msg})
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxAvatarUploadBytes)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			fail(http.StatusRequestEntityTooLarge, "Image must be 5 MB or smaller.")
			return
		}
		fail(http.StatusBadRequest, "Could not read upload. Please try again.")
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}

	file, _, err := r.FormFile("avatar")
	if err != nil {
		fail(http.StatusBadRequest, "Please choose an image to upload.")
		return
	}
	defer file.Close()

	jpegBytes, err := processAvatarImage(file)
	if err != nil {
		switch {
		case errors.Is(err, errImageTooLarge):
			fail(http.StatusBadRequest, "Image dimensions must be 6000×6000 pixels or smaller.")
		case errors.Is(err, errUnsupportedFormat):
			fail(http.StatusBadRequest, "Please upload a JPEG, PNG, or GIF image.")
		default:
			fail(http.StatusBadRequest, "That file doesn't look like a valid JPEG, PNG, or GIF image.")
		}
		return
	}

	randBuf := make([]byte, 8)
	if _, err := rand.Read(randBuf); err != nil {
		log.Printf("avatar upload: random bytes: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	relPath := fmt.Sprintf("avatars/%d-%s.jpg", user.ID, hex.EncodeToString(randBuf))
	fullPath := filepath.Join(app.UploadsPath(), filepath.FromSlash(relPath))

	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		log.Printf("avatar upload: mkdir: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if err := os.WriteFile(fullPath, jpegBytes, 0o644); err != nil {
		log.Printf("avatar upload: write file: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	tx, err := app.DB.Begin()
	if err != nil {
		_ = os.Remove(fullPath)
		log.Printf("avatar upload: begin tx: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	var oldPath sql.NullString
	if err := tx.QueryRow(`SELECT avatar_path FROM users WHERE id = ?`, user.ID).Scan(&oldPath); err != nil {
		_ = tx.Rollback()
		_ = os.Remove(fullPath)
		log.Printf("avatar upload: select old path: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if _, err := tx.Exec(`UPDATE users SET avatar_path = ? WHERE id = ?`, relPath, user.ID); err != nil {
		_ = tx.Rollback()
		_ = os.Remove(fullPath)
		log.Printf("avatar upload: update user: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		_ = os.Remove(fullPath)
		log.Printf("avatar upload: commit: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if oldPath.Valid && oldPath.String != "" && oldPath.String != relPath && isSafeAvatarPath(oldPath.String) {
		oldFull := filepath.Join(app.UploadsPath(), filepath.FromSlash(oldPath.String))
		if err := os.Remove(oldFull); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("avatar upload: remove old file %s: %v", oldPath.String, err)
		}
	}

	app.Render(w, r, "account.html", accountData{AvatarSuccess: "Profile picture updated."})
}

func handleAvatarDelete(w http.ResponseWriter, r *http.Request, app *web.App) {
	user := app.CurrentUser(r)

	tx, err := app.DB.Begin()
	if err != nil {
		log.Printf("avatar delete: begin tx: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	var oldPath sql.NullString
	if err := tx.QueryRow(`SELECT avatar_path FROM users WHERE id = ?`, user.ID).Scan(&oldPath); err != nil {
		_ = tx.Rollback()
		log.Printf("avatar delete: select old path: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if _, err := tx.Exec(`UPDATE users SET avatar_path = NULL WHERE id = ?`, user.ID); err != nil {
		_ = tx.Rollback()
		log.Printf("avatar delete: update user: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		log.Printf("avatar delete: commit: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if oldPath.Valid && oldPath.String != "" && isSafeAvatarPath(oldPath.String) {
		oldFull := filepath.Join(app.UploadsPath(), filepath.FromSlash(oldPath.String))
		if err := os.Remove(oldFull); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("avatar delete: remove file %s: %v", oldPath.String, err)
		}
	}

	app.Render(w, r, "account.html", accountData{AvatarSuccess: "Profile picture removed."})
}

func handleAvatarServe(w http.ResponseWriter, r *http.Request, app *web.App) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	var username string
	var avatarPath sql.NullString
	err = app.DB.QueryRow(`SELECT username, avatar_path FROM users WHERE id = ?`, id).Scan(&username, &avatarPath)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("avatar serve: query user %d: %v", id, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if err == nil && avatarPath.Valid && isSafeAvatarPath(avatarPath.String) {
		fullPath := filepath.Join(app.UploadsPath(), filepath.FromSlash(avatarPath.String))
		data, readErr := os.ReadFile(fullPath)
		if readErr == nil {
			serveCachedBytes(w, r, "image/jpeg", data)
			return
		}
		if !errors.Is(readErr, os.ErrNotExist) {
			log.Printf("avatar serve: read file %s: %v", avatarPath.String, readErr)
		}
	}

	svg := web.GenerateAvatarSVG(id, username)
	serveCachedBytes(w, r, "image/svg+xml; charset=utf-8", svg)
}

func serveCachedBytes(w http.ResponseWriter, r *http.Request, contentType string, data []byte) {
	sum := sha256.Sum256(data)
	etag := fmt.Sprintf(`"%x"`, sum[:16])

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("ETag", etag)
	w.Header().Set("X-Content-Type-Options", "nosniff")

	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(data)
}

func processAvatarImage(r io.ReadSeeker) ([]byte, error) {
	cfg, format, err := image.DecodeConfig(r)
	if err != nil {
		return nil, err
	}
	switch format {
	case "jpeg", "png", "gif":
	default:
		return nil, errUnsupportedFormat
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxAvatarDimension || cfg.Height > maxAvatarDimension {
		return nil, errImageTooLarge
	}

	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	src, _, err := image.Decode(r)
	if err != nil {
		return nil, err
	}

	bounds := src.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	side := min(w, h)
	cropX := bounds.Min.X + (w-side)/2
	cropY := bounds.Min.Y + (h-side)/2
	cropRect := image.Rect(cropX, cropY, cropX+side, cropY+side)

	dst := image.NewRGBA(image.Rect(0, 0, avatarOutputSize, avatarOutputSize))
	// Fill with dark slate so transparent PNG/GIF areas blend cleanly when converted to JPEG.
	draw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.RGBA{R: 15, G: 23, B: 42, A: 255}}, image.Point{}, draw.Src)
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, cropRect, draw.Over, nil)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: avatarJPEGQuality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func isSafeAvatarPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	cleaned := path.Clean(p)
	return strings.HasPrefix(cleaned, "avatars/") && cleaned == p && !strings.Contains(cleaned, "..")
}
