package main

import (
	"log"
	"net/http"
	"os"

	"scruples/internal/auth"
	"scruples/internal/cards"
	"scruples/internal/db"
	"scruples/internal/game"
	"scruples/internal/web"
)

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
	if adminPassword == "" {
		adminPassword = "changeme-admin"
		log.Printf("WARNING: SCRUPLES_ADMIN_PASSWORD not set; using default admin password %q", adminPassword)
	}

	database, err := db.Open(dbPath)
	if err != nil {
		log.Fatalf("opening database: %v", err)
	}

	resetAdmin := os.Getenv("SCRUPLES_RESET_ADMIN_PASSWORD") == "1"
	if err := auth.SeedAdmin(database, adminPassword, resetAdmin); err != nil {
		log.Fatalf("seeding admin user: %v", err)
	}

	app := web.NewApp(database)

	mux := http.NewServeMux()
	auth.Routes(mux, app)
	cards.Routes(mux, app)
	game.Routes(mux, app)

	mux.HandleFunc("GET /{$}", app.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		app.Render(w, r, "home.html", nil)
	}))

	log.Printf("Scruples listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
