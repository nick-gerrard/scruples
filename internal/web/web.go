package web

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"
)

// funcs are helpers available in all templates.
var funcs = template.FuncMap{
	"hasPrefix": strings.HasPrefix,
}

// User is the logged-in user attached to a request.
type User struct {
	ID       int64
	Username string
	IsAdmin  bool
}

// App holds shared application dependencies.
type App struct {
	DB *sql.DB
}

// NewApp constructs an App around the given database.
func NewApp(db *sql.DB) *App {
	return &App{DB: db}
}

const sessionCookie = "scruples_session"

// CurrentUser returns the logged-in user for the request, or nil.
func (a *App) CurrentUser(r *http.Request) *User {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	var u User
	err = a.DB.QueryRow(`
		SELECT u.id, u.username, u.is_admin
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token = ? AND s.expires_at > datetime('now')`,
		c.Value,
	).Scan(&u.ID, &u.Username, &u.IsAdmin)
	if err != nil {
		return nil
	}
	return &u
}

// RequireUser wraps a handler, rejecting requests without a valid session.
// htmx requests get a 401; normal requests are redirected to /login.
func (a *App) RequireUser(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.CurrentUser(r) == nil {
			if r.Header.Get("HX-Request") != "" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		h(w, r)
	}
}

// Render renders a full page: layout.html plus the given page file,
// executing the "layout" template.
func (a *App) Render(w http.ResponseWriter, r *http.Request, page string, data any) {
	tmpl, err := template.New("layout.html").Funcs(funcs).ParseFiles("templates/layout.html", "templates/"+page)
	if err != nil {
		log.Printf("render %s: parse: %v", page, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	payload := struct {
		User *User
		Data any
		Path string
	}{
		User: a.CurrentUser(r),
		Data: data,
		Path: r.URL.Path,
	}
	if err := tmpl.ExecuteTemplate(w, "layout", payload); err != nil {
		log.Printf("render %s: execute: %v", page, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

// RenderFragment renders a named template from a single page file with no
// layout, for htmx partial responses.
func (a *App) RenderFragment(w http.ResponseWriter, file string, name string, data any) {
	tmpl, err := template.New(file).Funcs(funcs).ParseFiles("templates/" + file)
	if err != nil {
		log.Printf("fragment %s#%s: parse: %v", file, name, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("fragment %s#%s: execute: %v", file, name, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

const sessionDuration = 30 * 24 * time.Hour

// CreateSession creates a session row for the user and sets the session cookie.
func (a *App) CreateSession(w http.ResponseWriter, userID int64) error {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	token := hex.EncodeToString(buf)
	expires := time.Now().UTC().Add(sessionDuration).Format("2006-01-02 15:04:05")
	if _, err := a.DB.Exec(
		`INSERT INTO sessions (token, user_id, expires_at) VALUES (?, ?, ?)`,
		token, userID, expires,
	); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionDuration / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// DestroySession deletes the request's session row and expires the cookie.
func (a *App) DestroySession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if _, err := a.DB.Exec(`DELETE FROM sessions WHERE token = ?`, c.Value); err != nil {
			log.Printf("destroy session: %v", err)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}
