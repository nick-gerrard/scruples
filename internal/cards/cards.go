package cards

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"scruples/internal/web"
)

// CardView is a card as displayed on the deck page.
type CardView struct {
	ID           int64
	Circumstance string
	Question     string
	Author       string
	AuthorID     int64
	CreatedAt    string
	CanDelete    bool
}

type deckData struct {
	Cards []CardView
	Count int
}

type newCardData struct {
	Error        string
	Circumstance string
	Question     string
}

// Routes registers the card routes on the mux.
func Routes(mux *http.ServeMux, app *web.App) {
	mux.HandleFunc("GET /cards", app.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		user := app.CurrentUser(r)

		rows, err := app.DB.Query(`
			SELECT c.id, c.circumstance, c.question, c.author_id, u.username, c.created_at
			FROM cards c
			JOIN users u ON u.id = c.author_id
			ORDER BY c.id DESC`)
		if err != nil {
			log.Printf("cards: query deck: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var cards []CardView
		for rows.Next() {
			var c CardView
			if err := rows.Scan(&c.ID, &c.Circumstance, &c.Question, &c.AuthorID, &c.Author, &c.CreatedAt); err != nil {
				log.Printf("cards: scan card: %v", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			// created_at is "YYYY-MM-DD HH:MM:SS"; show just the date.
			if len(c.CreatedAt) > 10 {
				c.CreatedAt = c.CreatedAt[:10]
			}
			c.CanDelete = user.IsAdmin || user.ID == c.AuthorID
			cards = append(cards, c)
		}
		if err := rows.Err(); err != nil {
			log.Printf("cards: iterate deck: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		app.Render(w, r, "cards.html", deckData{Cards: cards, Count: len(cards)})
	}))

	mux.HandleFunc("GET /cards/new", app.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		app.Render(w, r, "card_new.html", newCardData{})
	}))

	mux.HandleFunc("POST /cards", app.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		user := app.CurrentUser(r)
		circumstance := strings.TrimSpace(r.FormValue("circumstance"))
		question := strings.TrimSpace(r.FormValue("question"))

		fail := func(msg string) {
			app.Render(w, r, "card_new.html", newCardData{
				Error:        msg,
				Circumstance: circumstance,
				Question:     question,
			})
		}

		if circumstance == "" {
			fail("Set the scene — the circumstance can't be empty.")
			return
		}
		if utf8.RuneCountInString(circumstance) > 2000 {
			fail("The circumstance is too long — keep it under 2000 characters.")
			return
		}
		if question == "" {
			fail("Every dilemma needs its yes/no question.")
			return
		}
		if utf8.RuneCountInString(question) > 300 {
			fail("The question is too long — keep it under 300 characters.")
			return
		}

		if _, err := app.DB.Exec(
			`INSERT INTO cards (author_id, circumstance, question) VALUES (?, ?, ?)`,
			user.ID, circumstance, question,
		); err != nil {
			log.Printf("cards: insert card: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		http.Redirect(w, r, "/cards", http.StatusSeeOther)
	}))

	mux.HandleFunc("DELETE /cards/{id}", app.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		user := app.CurrentUser(r)
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		var authorID int64
		err = app.DB.QueryRow(`SELECT author_id FROM cards WHERE id = ?`, id).Scan(&authorID)
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			log.Printf("cards: load card %d: %v", id, err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		if !user.IsAdmin && user.ID != authorID {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		if _, err := app.DB.Exec(`DELETE FROM cards WHERE id = ?`, id); err != nil {
			log.Printf("cards: delete card %d: %v", id, err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
	}))
}
