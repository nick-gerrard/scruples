package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"scruples/internal/db"
	"scruples/internal/web"
)

func TestLoginSmoke(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	mux := buildMux(web.NewApp(database), "")

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /login status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), `action="/login"`) {
		t.Fatalf("GET /login body missing login form")
	}
}

func TestAPIDisabledWithoutKey(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	mux := buildMux(web.NewApp(database), "")

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/cards"},
		{http.MethodPost, "/api/cards"},
		{http.MethodPost, "/api/cards/batch"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer anything")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s status = %d, want %d when API key unset", tc.method, tc.path, rec.Code, http.StatusNotFound)
		}
	}
}

func TestBotUserCannotLogin(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	// Even if someone sets a valid bcrypt password on the Claude bot user, login must fail.
	hash, err := bcrypt.GenerateFromPassword([]byte("secret123"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	if _, err := database.Exec(`UPDATE users SET password_hash = ? WHERE username = 'Claude'`, string(hash)); err != nil {
		t.Fatalf("update bot password: %v", err)
	}

	mux := buildMux(web.NewApp(database), "")

	form := url.Values{
		"username": {"Claude"},
		"password": {"secret123"},
	}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("bot POST /login status = %d, want 200 re-rendered login page (no redirect)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Wrong username or password.") {
		t.Fatalf("expected login error message for bot user")
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatalf("bot login must not set session cookies, got %v", rec.Result().Cookies())
	}
	var sessions int
	if err := database.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 0 {
		t.Fatalf("sessions count = %d, want 0", sessions)
	}
}
