package cards

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"scruples/internal/db"
	"scruples/internal/web"
)

func sessionCookieFor(t *testing.T, app *web.App, userID int64) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	if err := app.CreateSession(rec, req, userID); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("expected session cookie")
	}
	return cookies[0]
}

func TestDeleteCardArchives(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "cards.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	app := web.NewApp(database)
	mux := http.NewServeMux()
	Routes(mux, app)

	res, err := database.Exec(`INSERT INTO users (username, password_hash) VALUES ('alice', 'hash')`)
	if err != nil {
		t.Fatalf("insert alice: %v", err)
	}
	aliceID, _ := res.LastInsertId()

	res, err = database.Exec(`INSERT INTO users (username, password_hash) VALUES ('bob', 'hash')`)
	if err != nil {
		t.Fatalf("insert bob: %v", err)
	}
	bobID, _ := res.LastInsertId()

	res, err = database.Exec(
		`INSERT INTO cards (author_id, circumstance, question) VALUES (?, 'A dilemma', 'Do it?')`,
		aliceID,
	)
	if err != nil {
		t.Fatalf("insert card: %v", err)
	}
	cardID, _ := res.LastInsertId()

	// Reference the card from a round so a hard DELETE would fail on the FK constraint.
	res, err = database.Exec(`INSERT INTO games (mode, point_goal, created_by) VALUES ('classic', 5, ?)`, aliceID)
	if err != nil {
		t.Fatalf("insert game: %v", err)
	}
	gameID, _ := res.LastInsertId()
	if _, err := database.Exec(
		`INSERT INTO rounds (game_id, card_id, drawer_id) VALUES (?, ?, ?)`,
		gameID, cardID, aliceID,
	); err != nil {
		t.Fatalf("insert round: %v", err)
	}

	// Non-author non-admin cannot archive the card.
	req := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/cards/%d", cardID), nil)
	req.AddCookie(sessionCookieFor(t, app, bobID))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bob DELETE /cards/%d status = %d, want %d", cardID, rec.Code, http.StatusForbidden)
	}

	// Author archives the card -> 200 OK, row remains with archived_at set.
	aliceCookie := sessionCookieFor(t, app, aliceID)
	req = httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/cards/%d", cardID), nil)
	req.AddCookie(aliceCookie)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("alice DELETE /cards/%d status = %d, want %d (body: %s)", cardID, rec.Code, http.StatusOK, rec.Body.String())
	}

	var archivedAt sql.NullString
	if err := database.QueryRow(`SELECT archived_at FROM cards WHERE id = ?`, cardID).Scan(&archivedAt); err != nil {
		t.Fatalf("expected archived card row to remain in DB: %v", err)
	}
	if !archivedAt.Valid || archivedAt.String == "" {
		t.Fatalf("expected archived_at to be set, got %+v", archivedAt)
	}

	// Second DELETE on already-archived card returns 404.
	req = httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/cards/%d", cardID), nil)
	req.AddCookie(aliceCookie)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second DELETE /cards/%d status = %d, want %d", cardID, rec.Code, http.StatusNotFound)
	}
}
