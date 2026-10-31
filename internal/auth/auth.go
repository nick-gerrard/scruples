package auth

import (
	"database/sql"
	"log"
	"net/http"
	"regexp"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"scruples/internal/web"
)

// SeedAdmin creates or updates the admin user "nick" with the given password.
// The password from the environment always wins over an existing hash.
func SeedAdmin(db *sql.DB, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = db.Exec(`
		INSERT INTO users (username, password_hash, is_admin)
		VALUES ('nick', ?, 1)
		ON CONFLICT(username) DO UPDATE SET password_hash = excluded.password_hash, is_admin = 1`,
		string(hash),
	)
	return err
}

var usernameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

type formData struct {
	Error    string
	Username string
}

// Routes registers the auth routes on the mux.
func Routes(mux *http.ServeMux, app *web.App) {
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		if app.CurrentUser(r) != nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		app.Render(w, r, "login.html", formData{})
	})

	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		username := strings.TrimSpace(r.FormValue("username"))
		password := r.FormValue("password")

		var id int64
		var hash string
		err := app.DB.QueryRow(
			`SELECT id, password_hash FROM users WHERE username = ?`, username,
		).Scan(&id, &hash)
		if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
			app.Render(w, r, "login.html", formData{
				Error:    "Wrong username or password.",
				Username: username,
			})
			return
		}
		if err := app.CreateSession(w, id); err != nil {
			log.Printf("login: create session: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})

	mux.HandleFunc("GET /signup", func(w http.ResponseWriter, r *http.Request) {
		if app.CurrentUser(r) != nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		app.Render(w, r, "signup.html", formData{})
	})

	mux.HandleFunc("POST /signup", func(w http.ResponseWriter, r *http.Request) {
		username := strings.TrimSpace(r.FormValue("username"))
		password := r.FormValue("password")

		fail := func(msg string) {
			app.Render(w, r, "signup.html", formData{Error: msg, Username: username})
		}

		if len(username) < 1 || len(username) > 30 || !usernameRe.MatchString(username) {
			fail("Username must be 1-30 characters: letters, numbers, _ or - only.")
			return
		}
		if len(password) < 6 {
			fail("Password must be at least 6 characters.")
			return
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			log.Printf("signup: bcrypt: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		res, err := app.DB.Exec(
			`INSERT INTO users (username, password_hash) VALUES (?, ?)`,
			username, string(hash),
		)
		if err != nil {
			if strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
				fail("That username is taken — try another.")
				return
			}
			log.Printf("signup: insert user: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		id, err := res.LastInsertId()
		if err != nil {
			log.Printf("signup: last insert id: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if err := app.CreateSession(w, id); err != nil {
			log.Printf("signup: create session: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})

	mux.HandleFunc("POST /logout", func(w http.ResponseWriter, r *http.Request) {
		app.DestroySession(w, r)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}
