package game

import (
	"log"
	"net/http"
	"strconv"
	"strings"

	"scruples/internal/web"
)

// pageData mirrors the payload shape web.App.Render passes to templates, so
// the "game-state" fragment can be rendered standalone with the same fields.
type pageData struct {
	User *web.User
	Data any
	Path string
}

func serverErr(w http.ResponseWriter, err error) {
	log.Printf("game: %v", err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

// respond re-renders the current game state: the "game-state" fragment for
// htmx requests, otherwise a redirect back to /game.
func respond(app *web.App, w http.ResponseWriter, r *http.Request, errMsg string) {
	if r.Header.Get("HX-Request") == "" {
		http.Redirect(w, r, "/game", http.StatusSeeOther)
		return
	}
	u := app.CurrentUser(r)
	view, err := buildView(app.DB, u, errMsg)
	if err != nil {
		serverErr(w, err)
		return
	}
	app.RenderFragment(w, "game.html", "game-state", pageData{User: u, Data: view, Path: "/game"})
}

// Routes registers the game and hall-of-fame routes on the mux.
func Routes(mux *http.ServeMux, app *web.App) {
	mux.HandleFunc("GET /game", app.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		view, err := buildView(app.DB, app.CurrentUser(r), "")
		if err != nil {
			serverErr(w, err)
			return
		}
		app.Render(w, r, "game.html", view)
	}))

	mux.HandleFunc("GET /game/state", app.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		u := app.CurrentUser(r)
		view, err := buildView(app.DB, u, "")
		if err != nil {
			serverErr(w, err)
			return
		}
		app.RenderFragment(w, "game.html", "game-state", pageData{User: u, Data: view, Path: "/game"})
	}))

	mux.HandleFunc("POST /game/start", app.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		u := app.CurrentUser(r)
		mode := r.FormValue("mode")
		if mode != "classic" && mode != "freeplay" {
			respond(app, w, r, "Pick a game mode.")
			return
		}
		pointGoal := 5
		if mode == "classic" {
			if pg := strings.TrimSpace(r.FormValue("point_goal")); pg != "" {
				n, err := strconv.Atoi(pg)
				if err != nil || n < 1 || n > 100 {
					respond(app, w, r, "Point goal must be a number between 1 and 100.")
					return
				}
				pointGoal = n
			}
		}
		if err := doStart(app.DB, u, mode, pointGoal); err != nil {
			serverErr(w, err)
			return
		}
		respond(app, w, r, "")
	}))

	mux.HandleFunc("POST /game/join", app.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		if err := doJoin(app.DB, app.CurrentUser(r)); err != nil {
			serverErr(w, err)
			return
		}
		respond(app, w, r, "")
	}))

	mux.HandleFunc("POST /game/draw", app.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		msg, err := doDraw(app.DB, app.CurrentUser(r))
		if err != nil {
			serverErr(w, err)
			return
		}
		respond(app, w, r, msg)
	}))

	mux.HandleFunc("POST /game/answer", app.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		u := app.CurrentUser(r)
		answer := r.FormValue("answer")
		if answer != "yes" && answer != "no" {
			http.Error(w, "bad answer", http.StatusBadRequest)
			return
		}
		g, rd, err := loadCurrent(app.DB)
		if err != nil {
			serverErr(w, err)
			return
		}
		if g == nil || rd == nil || g.Mode != "classic" || rd.Status != "voting" || rd.DrawerID != u.ID {
			respond(app, w, r, "")
			return
		}
		if _, err := app.DB.Exec(
			`UPDATE rounds SET drawer_answer = ?
			 WHERE id = ? AND status = 'voting' AND drawer_answer IS NULL`,
			answer, rd.ID); err != nil {
			serverErr(w, err)
			return
		}
		if err := maybeReveal(app.DB, g, rd.ID); err != nil {
			serverErr(w, err)
			return
		}
		respond(app, w, r, "")
	}))

	mux.HandleFunc("POST /game/vote", app.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		u := app.CurrentUser(r)
		vote := r.FormValue("vote")
		if vote != "yes" && vote != "no" {
			http.Error(w, "bad vote", http.StatusBadRequest)
			return
		}
		g, rd, err := loadCurrent(app.DB)
		if err != nil {
			serverErr(w, err)
			return
		}
		if g == nil || rd == nil || rd.Status != "voting" {
			respond(app, w, r, "")
			return
		}
		if g.Mode == "classic" && rd.DrawerID == u.ID {
			respond(app, w, r, "")
			return
		}
		ok, err := isEligible(app.DB, g.ID, u.ID, rd.CreatedAt)
		if err != nil {
			serverErr(w, err)
			return
		}
		if !ok {
			respond(app, w, r, "")
			return
		}
		if _, err := app.DB.Exec(
			`INSERT INTO votes (round_id, user_id, phase, vote) VALUES (?, ?, 'initial', ?)
			 ON CONFLICT DO NOTHING`,
			rd.ID, u.ID, vote); err != nil {
			serverErr(w, err)
			return
		}
		if err := maybeReveal(app.DB, g, rd.ID); err != nil {
			serverErr(w, err)
			return
		}
		respond(app, w, r, "")
	}))

	mux.HandleFunc("POST /game/challenge", app.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		u := app.CurrentUser(r)
		g, rd, err := loadCurrent(app.DB)
		if err != nil {
			serverErr(w, err)
			return
		}
		if g == nil || rd == nil || g.Mode != "classic" || rd.Status != "revealed" || rd.DrawerID == u.ID {
			respond(app, w, r, "")
			return
		}
		joined, err := isPlayer(app.DB, g.ID, u.ID)
		if err != nil {
			serverErr(w, err)
			return
		}
		if !joined {
			respond(app, w, r, "")
			return
		}
		if _, err := app.DB.Exec(
			`UPDATE rounds SET challenger_id = ?, status = 'challenge'
			 WHERE id = ? AND status = 'revealed'`,
			u.ID, rd.ID); err != nil {
			serverErr(w, err)
			return
		}
		// A challenge with nobody eligible to re-vote resolves immediately.
		if err := maybeResolveChallenge(app.DB, g); err != nil {
			serverErr(w, err)
			return
		}
		respond(app, w, r, "")
	}))

	mux.HandleFunc("POST /game/challenge/vote", app.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		u := app.CurrentUser(r)
		vote := r.FormValue("vote")
		if vote != "yes" && vote != "no" {
			http.Error(w, "bad vote", http.StatusBadRequest)
			return
		}
		g, rd, err := loadCurrent(app.DB)
		if err != nil {
			serverErr(w, err)
			return
		}
		if g == nil || rd == nil || g.Mode != "classic" || rd.Status != "challenge" || rd.DrawerID == u.ID {
			respond(app, w, r, "")
			return
		}
		ok, err := isEligible(app.DB, g.ID, u.ID, rd.CreatedAt)
		if err != nil {
			serverErr(w, err)
			return
		}
		if !ok {
			respond(app, w, r, "")
			return
		}
		if _, err := app.DB.Exec(
			`INSERT INTO votes (round_id, user_id, phase, vote) VALUES (?, ?, 'challenge', ?)
			 ON CONFLICT DO NOTHING`,
			rd.ID, u.ID, vote); err != nil {
			serverErr(w, err)
			return
		}
		if err := maybeResolveChallenge(app.DB, g); err != nil {
			serverErr(w, err)
			return
		}
		respond(app, w, r, "")
	}))

	mux.HandleFunc("POST /game/next", app.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		if err := doNext(app.DB, app.CurrentUser(r)); err != nil {
			serverErr(w, err)
			return
		}
		respond(app, w, r, "")
	}))

	mux.HandleFunc("POST /game/end", app.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		if err := doEnd(app.DB, app.CurrentUser(r)); err != nil {
			serverErr(w, err)
			return
		}
		respond(app, w, r, "")
	}))

	mux.HandleFunc("GET /winners", app.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		hall, err := buildHall(app.DB)
		if err != nil {
			serverErr(w, err)
			return
		}
		app.Render(w, r, "winners.html", hall)
	}))
}
