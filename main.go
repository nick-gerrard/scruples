package main

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"

	"scruples/internal/api"
	"scruples/internal/auth"
	"scruples/internal/cards"
	"scruples/internal/db"
	"scruples/internal/game"
	"scruples/internal/web"
)

//go:embed static
var staticFiles embed.FS

// staticHandler serves the embedded static/ directory at /static/ with cache headers
// (long for ?v= URLs). Directory listings are disabled.
func staticHandler() http.Handler {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("static files: %v", err)
	}
	files := http.StripPrefix("/static/", http.FileServerFS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		// Versioned URLs (?v=) never change; unversioned ones (manifest icons,
		// favicon redirect) must be able to pick up new artwork.
		if r.URL.Query().Has("v") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=86400")
		}
		files.ServeHTTP(w, r)
	})
}

func buildMux(app *web.App, apiKey string) *http.ServeMux {
	mux := http.NewServeMux()
	auth.Routes(mux, app)
	cards.Routes(mux, app)
	game.Routes(mux, app)
	if apiKey != "" {
		api.Routes(mux, app.DB, apiKey)
	}

	mux.Handle("GET /static/", staticHandler())
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/static/brand/favicon-32.png", http.StatusMovedPermanently)
	})

	mux.HandleFunc("GET /{$}", app.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		app.Render(w, r, "home.html", nil)
	}))
	return mux
}

func main() {
	addr := os.Getenv("SCRUPLES_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	dbPath := os.Getenv("SCRUPLES_DB")
	if dbPath == "" {
		dbPath = "scruples.db"
	}
	adminPassword := os.Getenv("SCRUPLES_ADMIN_PASSWORD")

	database, err := db.Open(dbPath)
	if err != nil {
		log.Fatalf("opening database: %v", err)
	}

	resetAdmin := os.Getenv("SCRUPLES_RESET_ADMIN_PASSWORD") == "1"
	if err := auth.SeedAdmin(database, adminPassword, resetAdmin); err != nil {
		log.Fatalf("seeding admin user: %v", err)
	}

	uploadsDir := os.Getenv("SCRUPLES_UPLOADS")
	if uploadsDir == "" {
		uploadsDir = "uploads"
	}

	app := web.NewApp(database)
	app.UploadsDir = uploadsDir
	mux := buildMux(app, os.Getenv("SCRUPLES_API_KEY"))

	log.Printf("Scruples listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
