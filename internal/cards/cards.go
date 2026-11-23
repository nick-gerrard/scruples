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
	Source       string
	CreatedAt    string
	Archived     bool
	CanEdit      bool
	CanDelete    bool
}

type deckData struct {
	Cards      []CardView
	Count      int
	MyCount    int
	TotalCount int
	IsAdmin    bool
	Filter     string
}

type newCardData struct {
	Error        string
	Circumstance string
	Question     string
}

type editCardData struct {
	ID           int64
	Error        string
	Circumstance string
	Question     string
}

func validateCard(circumstance, question string) string {
	if circumstance == "" {
		return "Set the scene — the circumstance can't be empty."
	}
	if utf8.RuneCountInString(circumstance) > 2000 {
		return "The circumstance is too long — keep it under 2000 characters."
	}
	if question == "" {
		return "Every dilemma needs its yes/no question."
	}
	if utf8.RuneCountInString(question) > 300 {
		return "The question is too long — keep it under 300 characters."
	}
	return ""
}

func canModify(user *web.User, authorID int64, source string) bool {
	if user.IsAdmin {
		return true
	}
	return user.ID == authorID && source != "ai"
}

// Routes registers the card routes on the mux.
func Routes(mux *http.ServeMux, app *web.App) {
	mux.HandleFunc("GET /cards", app.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		user := app.CurrentUser(r)

		var myCount, totalCount int
		if err := app.DB.QueryRow(
			`SELECT COUNT(*) FROM cards WHERE author_id = ? AND archived_at IS NULL`, user.ID,
		).Scan(&myCount); err != nil {
			log.Printf("cards: count own: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if err := app.DB.QueryRow(
			`SELECT COUNT(*) FROM cards WHERE archived_at IS NULL`,
		).Scan(&totalCount); err != nil {
			log.Printf("cards: count total: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		filter := "mine"
		query := `
			SELECT c.id, c.circumstance, c.question, c.author_id, u.username, c.source, c.created_at, c.archived_at
			FROM cards c
			JOIN users u ON u.id = c.author_id`
		var args []any

		if user.IsAdmin {
			filter = r.URL.Query().Get("filter")
			switch filter {
			case "mine":
				query += ` WHERE c.author_id = ? AND c.archived_at IS NULL`
				args = append(args, user.ID)
			case "human":
				query += ` WHERE c.source = 'human' AND c.archived_at IS NULL`
			case "ai":
				query += ` WHERE c.source = 'ai' AND c.archived_at IS NULL`
			case "archived":
				query += ` WHERE c.archived_at IS NOT NULL`
			default:
				filter = "all"
				query += ` WHERE c.archived_at IS NULL`
			}
		} else {
			query += ` WHERE c.author_id = ? AND c.archived_at IS NULL`
			args = append(args, user.ID)
		}
		query += ` ORDER BY c.id DESC`

		rows, err := app.DB.Query(query, args...)
		if err != nil {
			log.Printf("cards: query deck: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var cards []CardView
		for rows.Next() {
			var c CardView
			var archivedAt sql.NullString
			if err := rows.Scan(
				&c.ID, &c.Circumstance, &c.Question, &c.AuthorID, &c.Author, &c.Source, &c.CreatedAt, &archivedAt,
			); err != nil {
				log.Printf("cards: scan card: %v", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			// created_at is "YYYY-MM-DD HH:MM:SS"; show just the date.
			if len(c.CreatedAt) > 10 {
				c.CreatedAt = c.CreatedAt[:10]
			}
			c.Archived = archivedAt.Valid
			c.CanEdit = !c.Archived && canModify(user, c.AuthorID, c.Source)
			c.CanDelete = !c.Archived && canModify(user, c.AuthorID, c.Source)
			cards = append(cards, c)
		}
		if err := rows.Err(); err != nil {
			log.Printf("cards: iterate deck: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		app.Render(w, r, "cards.html", deckData{
			Cards:      cards,
			Count:      len(cards),
			MyCount:    myCount,
			TotalCount: totalCount,
			IsAdmin:    user.IsAdmin,
			Filter:     filter,
		})
	}))

	mux.HandleFunc("GET /cards/new", app.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		app.Render(w, r, "card_new.html", newCardData{})
	}))

	mux.HandleFunc("POST /cards", app.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		user := app.CurrentUser(r)
		circumstance := strings.TrimSpace(r.FormValue("circumstance"))
		question := strings.TrimSpace(r.FormValue("question"))

		if msg := validateCard(circumstance, question); msg != "" {
			app.Render(w, r, "card_new.html", newCardData{
				Error:        msg,
				Circumstance: circumstance,
				Question:     question,
			})
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

	mux.HandleFunc("GET /cards/{id}/edit", app.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		user := app.CurrentUser(r)
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		var authorID int64
		var circumstance, question, source string
		var archivedAt sql.NullString
		err = app.DB.QueryRow(
			`SELECT author_id, circumstance, question, source, archived_at FROM cards WHERE id = ?`,
			id,
		).Scan(&authorID, &circumstance, &question, &source, &archivedAt)
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			log.Printf("cards: load edit card %d: %v", id, err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		if !canModify(user, authorID, source) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if !user.IsAdmin && archivedAt.Valid {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		app.Render(w, r, "card_edit.html", editCardData{
			ID:           id,
			Circumstance: circumstance,
			Question:     question,
		})
	}))

	mux.HandleFunc("POST /cards/{id}", app.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		user := app.CurrentUser(r)
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		circumstance := strings.TrimSpace(r.FormValue("circumstance"))
		question := strings.TrimSpace(r.FormValue("question"))

		tx, err := app.DB.Begin()
		if err != nil {
			log.Printf("cards: begin edit %d: %v", id, err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		var authorID int64
		var source string
		var archivedAt sql.NullString
		err = tx.QueryRow(
			`SELECT author_id, source, archived_at FROM cards WHERE id = ?`,
			id,
		).Scan(&authorID, &source, &archivedAt)
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			log.Printf("cards: load card %d for edit: %v", id, err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		if !canModify(user, authorID, source) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if !user.IsAdmin && archivedAt.Valid {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		if msg := validateCard(circumstance, question); msg != "" {
			// Release the tx before app.Render queries app.DB (MaxOpenConns(1)).
			_ = tx.Rollback()
			w.WriteHeader(http.StatusBadRequest)
			app.Render(w, r, "card_edit.html", editCardData{
				ID:           id,
				Error:        msg,
				Circumstance: circumstance,
				Question:     question,
			})
			return
		}

		if _, err := tx.Exec(
			`UPDATE cards SET circumstance = ?, question = ? WHERE id = ?`,
			circumstance, question, id,
		); err != nil {
			log.Printf("cards: update card %d: %v", id, err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if err := tx.Commit(); err != nil {
			log.Printf("cards: commit edit %d: %v", id, err)
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

		tx, err := app.DB.Begin()
		if err != nil {
			log.Printf("cards: begin archive %d: %v", id, err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		var authorID int64
		var source string
		err = tx.QueryRow(`SELECT author_id, source FROM cards WHERE id = ? AND archived_at IS NULL`, id).Scan(&authorID, &source)
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			log.Printf("cards: load card %d: %v", id, err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		if !canModify(user, authorID, source) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		if _, err := tx.Exec(`UPDATE cards SET archived_at = datetime('now') WHERE id = ? AND archived_at IS NULL`, id); err != nil {
			log.Printf("cards: archive card %d: %v", id, err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if err := tx.Commit(); err != nil {
			log.Printf("cards: commit archive %d: %v", id, err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
	}))

	mux.HandleFunc("POST /cards/{id}/restore", app.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		user := app.CurrentUser(r)
		if !user.IsAdmin {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		tx, err := app.DB.Begin()
		if err != nil {
			log.Printf("cards: begin restore %d: %v", id, err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		var exists int
		err = tx.QueryRow(`SELECT 1 FROM cards WHERE id = ?`, id).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			log.Printf("cards: load card %d for restore: %v", id, err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		if _, err := tx.Exec(`UPDATE cards SET archived_at = NULL WHERE id = ?`, id); err != nil {
			log.Printf("cards: restore card %d: %v", id, err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if err := tx.Commit(); err != nil {
			log.Printf("cards: commit restore %d: %v", id, err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		if r.Header.Get("HX-Request") != "" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/cards?filter=archived", http.StatusSeeOther)
	}))
}
