package web

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"scruples/internal/db"
)

func TestSessionCookieSecureFlag(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	res, err := database.Exec(`INSERT INTO users (username, password_hash) VALUES ('alice', 'hash')`)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	uid, _ := res.LastInsertId()
	app := NewApp(database)

	tests := []struct {
		name       string
		setupReq   func(*http.Request)
		wantSecure bool
	}{
		{
			name:       "plain http",
			setupReq:   func(*http.Request) {},
			wantSecure: false,
		},
		{
			name: "x-forwarded-proto http",
			setupReq: func(r *http.Request) {
				r.Header.Set("X-Forwarded-Proto", "http")
			},
			wantSecure: false,
		},
		{
			name: "x-forwarded-proto https",
			setupReq: func(r *http.Request) {
				r.Header.Set("X-Forwarded-Proto", "https")
			},
			wantSecure: true,
		},
		{
			name: "direct tls",
			setupReq: func(r *http.Request) {
				r.TLS = &tls.ConnectionState{}
			},
			wantSecure: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/login", nil)
			tc.setupReq(req)
			rec := httptest.NewRecorder()
			if err := app.CreateSession(rec, req, uid); err != nil {
				t.Fatalf("CreateSession: %v", err)
			}
			cookies := rec.Result().Cookies()
			if len(cookies) != 1 {
				t.Fatalf("CreateSession cookies = %d, want 1", len(cookies))
			}
			if cookies[0].Secure != tc.wantSecure {
				t.Errorf("CreateSession Secure = %v, want %v", cookies[0].Secure, tc.wantSecure)
			}

			// Verify DestroySession applies the same Secure flag when clearing.
			logoutReq := httptest.NewRequest(http.MethodPost, "/logout", nil)
			tc.setupReq(logoutReq)
			logoutReq.AddCookie(cookies[0])
			logoutRec := httptest.NewRecorder()
			app.DestroySession(logoutRec, logoutReq)
			cleared := logoutRec.Result().Cookies()
			if len(cleared) != 1 {
				t.Fatalf("DestroySession cookies = %d, want 1", len(cleared))
			}
			if cleared[0].Secure != tc.wantSecure {
				t.Errorf("DestroySession Secure = %v, want %v", cleared[0].Secure, tc.wantSecure)
			}
		})
	}
}
