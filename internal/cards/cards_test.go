package cards

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
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

func TestDeckPrivacyAndAdminFilters(t *testing.T) {
	t.Chdir("../..")

	database, err := db.Open(filepath.Join(t.TempDir(), "privacy.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	app := web.NewApp(database)
	mux := http.NewServeMux()
	Routes(mux, app)

	insertUser := func(name string, isAdmin int) int64 {
		t.Helper()
		res, err := database.Exec(`INSERT INTO users (username, password_hash, is_admin) VALUES (?, 'hash', ?)`, name, isAdmin)
		if err != nil {
			t.Fatalf("insert user %s: %v", name, err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	aliceID := insertUser("alice", 0)
	bobID := insertUser("bob", 0)
	adminID := insertUser("boss", 1)

	var claudeID int64
	if err := database.QueryRow(`SELECT id FROM users WHERE username = 'Claude'`).Scan(&claudeID); err != nil {
		t.Fatalf("select Claude: %v", err)
	}

	insertCard := func(authorID int64, circ, q, source string, archived bool) int64 {
		t.Helper()
		var arch any
		if archived {
			arch = "2026-01-01 00:00:00"
		}
		res, err := database.Exec(
			`INSERT INTO cards (author_id, circumstance, question, source, archived_at) VALUES (?, ?, ?, ?, ?)`,
			authorID, circ, q, source, arch,
		)
		if err != nil {
			t.Fatalf("insert card: %v", err)
		}
		id, _ := res.LastInsertId()
		return id
	}

	_ = insertCard(aliceID, "ALICE_ACTIVE_SCENE", "Alice Q?", "human", false)
	aliceArchivedID := insertCard(aliceID, "ALICE_ARCHIVED_SCENE", "Alice old Q?", "human", true)
	bobCardID := insertCard(bobID, "BOB_SECRET_SCENE", "Bob Q?", "human", false)
	aiCardID := insertCard(claudeID, "CLAUDE_AI_SCENE", "Claude Q?", "ai", false)

	aliceCookie := sessionCookieFor(t, app, aliceID)
	adminCookie := sessionCookieFor(t, app, adminID)

	// Non-admin GET /cards (even when passing admin filter query params) shows ONLY own active cards.
	for _, path := range []string{"/cards", "/cards?filter=all", "/cards?filter=ai", "/cards?filter=archived"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(aliceCookie)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("alice GET %s status = %d, want 200", path, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "You've written 1 card. The deck has 3.") {
			t.Errorf("alice GET %s missing privacy header count; body=%s", path, body)
		}
		if !strings.Contains(body, "ALICE_ACTIVE_SCENE") {
			t.Errorf("alice GET %s missing own active card", path)
		}
		for _, forbidden := range []string{"BOB_SECRET_SCENE", "CLAUDE_AI_SCENE", "ALICE_ARCHIVED_SCENE"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("alice GET %s leaked %s", path, forbidden)
			}
		}
	}

	// Non-admin cannot fetch/edit another user's card or an AI card (403) or archived card (404), nor restore (403).
	for _, tc := range []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, fmt.Sprintf("/cards/%d/edit", bobCardID), http.StatusForbidden},
		{http.MethodPost, fmt.Sprintf("/cards/%d", bobCardID), http.StatusForbidden},
		{http.MethodGet, fmt.Sprintf("/cards/%d/edit", aiCardID), http.StatusForbidden},
		{http.MethodPost, fmt.Sprintf("/cards/%d", aiCardID), http.StatusForbidden},
		{http.MethodGet, fmt.Sprintf("/cards/%d/edit", aliceArchivedID), http.StatusNotFound},
		{http.MethodPost, fmt.Sprintf("/cards/%d", aliceArchivedID), http.StatusNotFound},
		{http.MethodPost, fmt.Sprintf("/cards/%d/restore", aliceArchivedID), http.StatusForbidden},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.AddCookie(aliceCookie)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("alice %s %s status = %d, want %d", tc.method, tc.path, rec.Code, tc.want)
		}
		if strings.Contains(rec.Body.String(), "BOB_SECRET_SCENE") || strings.Contains(rec.Body.String(), "CLAUDE_AI_SCENE") {
			t.Errorf("alice %s %s leaked card text", tc.method, tc.path)
		}
	}

	// Admin sees all active cards by default and can filter.
	checkAdminFilter := func(query string, wantPresent, wantAbsent []string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/cards"+query, nil)
		req.AddCookie(adminCookie)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("admin GET /cards%s status = %d", query, rec.Code)
		}
		body := rec.Body.String()
		for _, s := range wantPresent {
			if !strings.Contains(body, s) {
				t.Errorf("admin GET /cards%s missing %q", query, s)
			}
		}
		for _, s := range wantAbsent {
			if strings.Contains(body, s) {
				t.Errorf("admin GET /cards%s unexpectedly contained %q", query, s)
			}
		}
	}

	checkAdminFilter("",
		[]string{"ALICE_ACTIVE_SCENE", "BOB_SECRET_SCENE", "CLAUDE_AI_SCENE", "by alice", "by bob", "by Claude"},
		[]string{"ALICE_ARCHIVED_SCENE"})
	checkAdminFilter("?filter=mine",
		[]string{"No matching cards."},
		[]string{"ALICE_ACTIVE_SCENE", "BOB_SECRET_SCENE", "CLAUDE_AI_SCENE"})
	checkAdminFilter("?filter=human",
		[]string{"ALICE_ACTIVE_SCENE", "BOB_SECRET_SCENE"},
		[]string{"CLAUDE_AI_SCENE", "ALICE_ARCHIVED_SCENE"})
	checkAdminFilter("?filter=ai",
		[]string{"CLAUDE_AI_SCENE"},
		[]string{"ALICE_ACTIVE_SCENE", "BOB_SECRET_SCENE", "ALICE_ARCHIVED_SCENE"})
	checkAdminFilter("?filter=archived",
		[]string{"ALICE_ARCHIVED_SCENE"},
		[]string{"ALICE_ACTIVE_SCENE", "BOB_SECRET_SCENE", "CLAUDE_AI_SCENE"})

	// Admin can restore an archived card.
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/cards/%d/restore", aliceArchivedID), nil)
	req.Header.Set("HX-Request", "true")
	req.AddCookie(adminCookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin restore status = %d, want 200", rec.Code)
	}
	var arch sql.NullString
	if err := database.QueryRow(`SELECT archived_at FROM cards WHERE id = ?`, aliceArchivedID).Scan(&arch); err != nil {
		t.Fatalf("query restored card: %v", err)
	}
	if arch.Valid {
		t.Fatalf("expected archived_at NULL after restore, got %q", arch.String)
	}
}

func TestEditCardAndValidation(t *testing.T) {
	t.Chdir("../..")

	database, err := db.Open(filepath.Join(t.TempDir(), "edit.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	app := web.NewApp(database)
	mux := http.NewServeMux()
	Routes(mux, app)

	res, _ := database.Exec(`INSERT INTO users (username, password_hash, is_admin) VALUES ('alice', 'hash', 0)`)
	aliceID, _ := res.LastInsertId()
	res, _ = database.Exec(`INSERT INTO users (username, password_hash, is_admin) VALUES ('boss', 'hash', 1)`)
	adminID, _ := res.LastInsertId()
	var claudeID int64
	_ = database.QueryRow(`SELECT id FROM users WHERE username = 'Claude'`).Scan(&claudeID)

	res, _ = database.Exec(`INSERT INTO cards (author_id, circumstance, question) VALUES (?, 'Orig scene', 'Orig Q?')`, aliceID)
	aliceCardID, _ := res.LastInsertId()
	res, _ = database.Exec(`INSERT INTO cards (author_id, circumstance, question, source) VALUES (?, 'AI scene', 'AI Q?', 'ai')`, claudeID)
	aiCardID, _ := res.LastInsertId()

	aliceCookie := sessionCookieFor(t, app, aliceID)
	adminCookie := sessionCookieFor(t, app, adminID)

	// Author can view edit form.
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/cards/%d/edit", aliceCardID), nil)
	req.AddCookie(aliceCookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Orig scene") {
		t.Fatalf("GET edit status=%d, expected 200 with original text", rec.Code)
	}

	// Validation errors return 400 and leave the card unchanged.
	badForms := []url.Values{
		{"circumstance": {""}, "question": {"Valid?"}},
		{"circumstance": {"Valid"}, "question": {""}},
		{"circumstance": {strings.Repeat("x", 2001)}, "question": {"Valid?"}},
		{"circumstance": {"Valid"}, "question": {strings.Repeat("y", 301)}},
	}
	for i, form := range badForms {
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/cards/%d", aliceCardID), strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(aliceCookie)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("bad edit #%d status = %d, want 400", i, rec.Code)
		}
	}
	var circ, q string
	_ = database.QueryRow(`SELECT circumstance, question FROM cards WHERE id = ?`, aliceCardID).Scan(&circ, &q)
	if circ != "Orig scene" || q != "Orig Q?" {
		t.Fatalf("invalid edits mutated card: (%q, %q)", circ, q)
	}

	// Valid edit by author succeeds.
	form := url.Values{"circumstance": {"Updated scene"}, "question": {"Updated Q?"}}
	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/cards/%d", aliceCardID), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(aliceCookie)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("valid author edit status = %d, want 303", rec.Code)
	}
	_ = database.QueryRow(`SELECT circumstance, question FROM cards WHERE id = ?`, aliceCardID).Scan(&circ, &q)
	if circ != "Updated scene" || q != "Updated Q?" {
		t.Fatalf("card after author edit = (%q, %q)", circ, q)
	}

	// Admin can edit AI card.
	aiForm := url.Values{"circumstance": {"Edited AI scene"}, "question": {"Edited AI Q?"}}
	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/cards/%d", aiCardID), strings.NewReader(aiForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(adminCookie)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("admin AI edit status = %d, want 303", rec.Code)
	}
	_ = database.QueryRow(`SELECT circumstance, question FROM cards WHERE id = ?`, aiCardID).Scan(&circ, &q)
	if circ != "Edited AI scene" || q != "Edited AI Q?" {
		t.Fatalf("AI card after admin edit = (%q, %q)", circ, q)
	}
}
