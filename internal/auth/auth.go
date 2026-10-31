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

// SeedAdmin makes sure the admin user "nick" exists. The given password is only
// used when the user is first created, so a password changed on the account page
// survives restarts; pass reset=true to force it back to the given password
// (lockout recovery).
func SeedAdmin(db *sql.DB, password string, reset bool) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	onConflict := `UPDATE SET is_admin = 1`
	if reset {
		onConflict = `UPDATE SET password_hash = excluded.password_hash, is_admin = 1`
	}
	_, err = db.Exec(`
		INSERT INTO users (username, password_hash, is_admin)
		VALUES ('nick', ?, 1)
		ON CONFLICT(username) DO `+onConflict,
		string(hash),
	)
	return err
}

var usernameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

type formData struct {
	Error    string
	Username string
}

type accountData struct {
	Error   string
	Success string
}

const minPasswordLen = 6

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
		if len(password) < minPasswordLen {
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

	mux.HandleFunc("GET /account", app.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		app.Render(w, r, "account.html", accountData{})
	}))

	mux.HandleFunc("POST /account/password", app.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		user := app.CurrentUser(r)
		current := r.FormValue("current_password")
		next := r.FormValue("new_password")

		fail := func(msg string) {
			app.Render(w, r, "account.html", accountData{Error: msg})
		}

		var hash string
		if err := app.DB.QueryRow(`SELECT password_hash FROM users WHERE id = ?`, user.ID).Scan(&hash); err != nil {
			log.Printf("change password: load user: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(current)) != nil {
			fail("Your current password isn't right.")
			return
		}
		if len(next) < minPasswordLen {
			fail("New password must be at least 6 characters.")
			return
		}
		if next != r.FormValue("confirm_password") {
			fail("The new passwords don't match.")
			return
		}

		newHash, err := bcrypt.GenerateFromPassword([]byte(next), bcrypt.DefaultCost)
		if err != nil {
			log.Printf("change password: bcrypt: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if _, err := app.DB.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, string(newHash), user.ID); err != nil {
			log.Printf("change password: update: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		// Anyone else signed in with the old password gets logged out.
		if err := app.DestroyOtherSessions(r, user.ID); err != nil {
			log.Printf("change password: destroy other sessions: %v", err)
		}
		app.Render(w, r, "account.html", accountData{Success: "Password changed. Other devices have been signed out."})
	}))

	mux.HandleFunc("POST /logout", func(w http.ResponseWriter, r *http.Request) {
		app.DestroySession(w, r)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}
