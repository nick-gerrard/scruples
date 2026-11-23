package game

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"scruples/internal/db"
	"scruples/internal/web"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "scruples.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func createUser(t *testing.T, d *sql.DB, username string) *web.User {
	t.Helper()
	res, err := d.Exec(
		`INSERT INTO users (username, password_hash) VALUES (?, 'test-hash')`,
		username,
	)
	if err != nil {
		t.Fatalf("insert user %q: %v", username, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id for %q: %v", username, err)
	}
	return &web.User{ID: id, Username: username}
}

func createCard(t *testing.T, d *sql.DB, authorID int64, circumstance, question string) int64 {
	t.Helper()
	res, err := d.Exec(
		`INSERT INTO cards (author_id, circumstance, question) VALUES (?, ?, ?)`,
		authorID, circumstance, question,
	)
	if err != nil {
		t.Fatalf("insert card: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("card last insert id: %v", err)
	}
	return id
}

func TestDoStart(t *testing.T) {
	d := openTestDB(t)
	alice := createUser(t, d, "alice")
	bob := createUser(t, d, "bob")

	if err := doStart(d, alice, "classic", 7, 60); err != nil {
		t.Fatalf("doStart: %v", err)
	}

	g, err := activeGame(d)
	if err != nil {
		t.Fatalf("activeGame: %v", err)
	}
	if g == nil {
		t.Fatal("expected active game, got nil")
	}
	if g.Mode != "classic" || g.PointGoal != 7 || g.CreatedBy != alice.ID || g.DiscussionSeconds != 60 {
		t.Fatalf("unexpected game: %+v", g)
	}

	players, err := loadPlayers(d, g.ID)
	if err != nil {
		t.Fatalf("loadPlayers: %v", err)
	}
	if len(players) != 1 || players[0].ID != alice.ID || players[0].TurnOrder != 1 {
		t.Fatalf("unexpected players: %+v", players)
	}

	// Second start while active must be a no-op.
	if err := doStart(d, bob, "freeplay", 0, 30); err != nil {
		t.Fatalf("second doStart: %v", err)
	}
	var count int
	if err := d.QueryRow(`SELECT COUNT(*) FROM games`).Scan(&count); err != nil {
		t.Fatalf("count games: %v", err)
	}
	if count != 1 {
		t.Fatalf("games count = %d, want 1", count)
	}
	g2, err := activeGame(d)
	if err != nil {
		t.Fatalf("activeGame after second start: %v", err)
	}
	if g2.ID != g.ID || g2.Mode != "classic" {
		t.Fatalf("active game changed after second doStart: %+v", g2)
	}
}

func TestDoJoin(t *testing.T) {
	d := openTestDB(t)
	alice := createUser(t, d, "alice")
	bob := createUser(t, d, "bob")
	carol := createUser(t, d, "carol")

	if err := doStart(d, alice, "classic", 5, 60); err != nil {
		t.Fatalf("doStart: %v", err)
	}
	if err := doJoin(d, bob); err != nil {
		t.Fatalf("doJoin bob: %v", err)
	}
	if err := doJoin(d, carol); err != nil {
		t.Fatalf("doJoin carol: %v", err)
	}
	// Re-joining is a no-op.
	if err := doJoin(d, bob); err != nil {
		t.Fatalf("re-doJoin bob: %v", err)
	}

	g, err := activeGame(d)
	if err != nil {
		t.Fatalf("activeGame: %v", err)
	}
	players, err := loadPlayers(d, g.ID)
	if err != nil {
		t.Fatalf("loadPlayers: %v", err)
	}
	want := []struct {
		id    int64
		order int
	}{
		{alice.ID, 1},
		{bob.ID, 2},
		{carol.ID, 3},
	}
	if len(players) != len(want) {
		t.Fatalf("len(players) = %d, want %d", len(players), len(want))
	}
	for i, w := range want {
		if players[i].ID != w.id || players[i].TurnOrder != w.order {
			t.Errorf("player[%d] = (id=%d, order=%d), want (id=%d, order=%d)",
				i, players[i].ID, players[i].TurnOrder, w.id, w.order)
		}
	}
}

func TestDoDraw(t *testing.T) {
	d := openTestDB(t)
	alice := createUser(t, d, "alice")
	bob := createUser(t, d, "bob")

	if err := doStart(d, alice, "classic", 5, 60); err != nil {
		t.Fatalf("doStart: %v", err)
	}
	if err := doJoin(d, bob); err != nil {
		t.Fatalf("doJoin: %v", err)
	}

	// Empty deck returns a message and does not create a round.
	msg, err := doDraw(d, alice)
	if err != nil {
		t.Fatalf("doDraw empty: %v", err)
	}
	if msg == "" {
		t.Fatal("expected empty-deck message, got empty string")
	}
	_, rd, err := loadCurrent(d)
	if err != nil {
		t.Fatalf("loadCurrent: %v", err)
	}
	if rd != nil {
		t.Fatalf("expected no round on empty deck, got %+v", rd)
	}

	// With both an alice-authored card and a bob-authored card, alice draws bob's card.
	_ = createCard(t, d, alice.ID, "Alice's scene", "Would you?")
	bobCardID := createCard(t, d, bob.ID, "Bob's scene", "Would you?")

	msg, err = doDraw(d, alice)
	if err != nil {
		t.Fatalf("doDraw with cards: %v", err)
	}
	if msg != "" {
		t.Fatalf("unexpected message: %q", msg)
	}
	_, rd, err = loadCurrent(d)
	if err != nil {
		t.Fatalf("loadCurrent after draw: %v", err)
	}
	if rd == nil {
		t.Fatal("expected a round after draw, got nil")
	}
	if rd.CardID != bobCardID {
		t.Fatalf("drawn card_id = %d, want %d (not authored by drawer)", rd.CardID, bobCardID)
	}
	if rd.DrawerID != alice.ID || rd.Status != "voting" {
		t.Fatalf("unexpected round: %+v", rd)
	}
}

func TestMaybeReveal(t *testing.T) {
	t.Run("classic", func(t *testing.T) {
		d := openTestDB(t)
		alice := createUser(t, d, "alice")
		bob := createUser(t, d, "bob")
		carol := createUser(t, d, "carol")
		createCard(t, d, bob.ID, "A dilemma", "Do it?")

		if err := doStart(d, alice, "classic", 5, 60); err != nil {
			t.Fatalf("doStart: %v", err)
		}
		if err := doJoin(d, bob); err != nil {
			t.Fatalf("doJoin bob: %v", err)
		}
		if err := doJoin(d, carol); err != nil {
			t.Fatalf("doJoin carol: %v", err)
		}
		if _, err := doDraw(d, alice); err != nil {
			t.Fatalf("doDraw: %v", err)
		}

		g, rd, err := loadCurrent(d)
		if err != nil {
			t.Fatalf("loadCurrent: %v", err)
		}

		// Bob votes, but drawer hasn't answered and Carol hasn't voted.
		if _, err := d.Exec(`INSERT INTO votes (round_id, user_id, phase, vote) VALUES (?, ?, 'initial', 'yes')`, rd.ID, bob.ID); err != nil {
			t.Fatalf("insert bob vote: %v", err)
		}
		if err := maybeReveal(d, g, rd.ID); err != nil {
			t.Fatalf("maybeReveal 1: %v", err)
		}
		if got, _ := roundByID(d, rd.ID); got.Status != "voting" {
			t.Fatalf("status after 1 vote = %q, want voting", got.Status)
		}

		// Drawer answers, still waiting on Carol.
		if _, err := d.Exec(`UPDATE rounds SET drawer_answer = 'yes' WHERE id = ?`, rd.ID); err != nil {
			t.Fatalf("set drawer_answer: %v", err)
		}
		if err := maybeReveal(d, g, rd.ID); err != nil {
			t.Fatalf("maybeReveal 2: %v", err)
		}
		if got, _ := roundByID(d, rd.ID); got.Status != "voting" {
			t.Fatalf("status after drawer answer = %q, want voting", got.Status)
		}

		// Carol votes -> all eligible voters and drawer are in -> revealed.
		if _, err := d.Exec(`INSERT INTO votes (round_id, user_id, phase, vote) VALUES (?, ?, 'initial', 'no')`, rd.ID, carol.ID); err != nil {
			t.Fatalf("insert carol vote: %v", err)
		}
		if err := maybeReveal(d, g, rd.ID); err != nil {
			t.Fatalf("maybeReveal 3: %v", err)
		}
		if got, _ := roundByID(d, rd.ID); got.Status != "revealed" {
			t.Fatalf("status after all votes = %q, want revealed", got.Status)
		}
	})

	t.Run("freeplay", func(t *testing.T) {
		d := openTestDB(t)
		alice := createUser(t, d, "alice")
		bob := createUser(t, d, "bob")
		createCard(t, d, alice.ID, "A dilemma", "Do it?")

		if err := doStart(d, alice, "freeplay", 0, 60); err != nil {
			t.Fatalf("doStart: %v", err)
		}
		if err := doJoin(d, bob); err != nil {
			t.Fatalf("doJoin: %v", err)
		}
		if _, err := doDraw(d, alice); err != nil {
			t.Fatalf("doDraw: %v", err)
		}
		g, rd, err := loadCurrent(d)
		if err != nil {
			t.Fatalf("loadCurrent: %v", err)
		}

		if _, err := d.Exec(`INSERT INTO votes (round_id, user_id, phase, vote) VALUES (?, ?, 'initial', 'yes')`, rd.ID, alice.ID); err != nil {
			t.Fatalf("insert alice vote: %v", err)
		}
		if err := maybeReveal(d, g, rd.ID); err != nil {
			t.Fatalf("maybeReveal 1: %v", err)
		}
		if got, _ := roundByID(d, rd.ID); got.Status != "voting" {
			t.Fatalf("status after 1 vote = %q, want voting", got.Status)
		}

		if _, err := d.Exec(`INSERT INTO votes (round_id, user_id, phase, vote) VALUES (?, ?, 'initial', 'no')`, rd.ID, bob.ID); err != nil {
			t.Fatalf("insert bob vote: %v", err)
		}
		if err := maybeReveal(d, g, rd.ID); err != nil {
			t.Fatalf("maybeReveal 2: %v", err)
		}
		if got, _ := roundByID(d, rd.ID); got.Status != "revealed" {
			t.Fatalf("status after all votes = %q, want revealed", got.Status)
		}
	})
}

func TestDoNextClassicScoringAndFinish(t *testing.T) {
	d := openTestDB(t)
	alice := createUser(t, d, "alice")
	bob := createUser(t, d, "bob")
	carol := createUser(t, d, "carol")

	createCard(t, d, alice.ID, "Scene 1", "Q1?")
	createCard(t, d, bob.ID, "Scene 2", "Q2?")
	createCard(t, d, carol.ID, "Scene 3", "Q3?")

	if err := doStart(d, alice, "classic", 2, 0); err != nil {
		t.Fatalf("doStart: %v", err)
	}
	if err := doJoin(d, bob); err != nil {
		t.Fatalf("doJoin bob: %v", err)
	}
	if err := doJoin(d, carol); err != nil {
		t.Fatalf("doJoin carol: %v", err)
	}

	playRound := func(drawer *web.User, answer string, votes map[int64]string) {
		t.Helper()
		if msg, err := doDraw(d, drawer); err != nil || msg != "" {
			t.Fatalf("doDraw(%s): msg=%q err=%v", drawer.Username, msg, err)
		}
		g, rd, err := loadCurrent(d)
		if err != nil {
			t.Fatalf("loadCurrent: %v", err)
		}
		if _, err := d.Exec(`UPDATE rounds SET drawer_answer = ? WHERE id = ?`, answer, rd.ID); err != nil {
			t.Fatalf("set drawer_answer: %v", err)
		}
		for uid, v := range votes {
			if _, err := d.Exec(`INSERT INTO votes (round_id, user_id, phase, vote) VALUES (?, ?, 'initial', ?)`, rd.ID, uid, v); err != nil {
				t.Fatalf("insert vote: %v", err)
			}
		}
		if err := maybeReveal(d, g, rd.ID); err != nil {
			t.Fatalf("maybeReveal: %v", err)
		}
		if err := doNext(d, drawer); err != nil {
			t.Fatalf("doNext: %v", err)
		}
	}

	// Round 1: Alice draws -> answers yes; Bob guesses yes (+1), Carol guesses no (0).
	playRound(alice, "yes", map[int64]string{bob.ID: "yes", carol.ID: "no"})

	g, rd, err := loadCurrent(d)
	if err != nil {
		t.Fatalf("loadCurrent after round 1: %v", err)
	}
	if g == nil || g.Status != "active" {
		t.Fatalf("expected active game after round 1, got %+v", g)
	}
	if rd == nil || rd.Status != "complete" || rd.FinalAnswer != "yes" {
		t.Fatalf("unexpected round 1 state: %+v", rd)
	}
	players, err := loadPlayers(d, g.ID)
	if err != nil {
		t.Fatalf("loadPlayers after round 1: %v", err)
	}
	pts := map[int64]int{}
	for _, p := range players {
		pts[p.ID] = p.Points
	}
	if pts[alice.ID] != 0 || pts[bob.ID] != 1 || pts[carol.ID] != 0 {
		t.Fatalf("points after round 1 = %v, want alice=0 bob=1 carol=0", pts)
	}

	// Round 2: Bob draws -> answers no; Alice guesses no (+1), Carol guesses yes (0).
	playRound(bob, "no", map[int64]string{alice.ID: "no", carol.ID: "yes"})

	if gActive, err := activeGame(d); err != nil || gActive == nil {
		t.Fatalf("expected active game after round 2, got g=%+v err=%v", gActive, err)
	}

	// Round 3: Carol draws -> answers yes; Bob guesses yes (+1 -> 2, hits point_goal), Alice guesses no.
	playRound(carol, "yes", map[int64]string{bob.ID: "yes", alice.ID: "no"})

	if gActive, err := activeGame(d); err != nil || gActive != nil {
		t.Fatalf("expected no active game after hitting point_goal, got g=%+v err=%v", gActive, err)
	}
	finished, err := latestFinishedGame(d)
	if err != nil || finished == nil {
		t.Fatalf("expected finished game, got g=%+v err=%v", finished, err)
	}
	if finished.ID != g.ID || finished.Status != "finished" {
		t.Fatalf("unexpected finished game: %+v", finished)
	}

	finalPlayers, err := loadPlayers(d, finished.ID)
	if err != nil {
		t.Fatalf("loadPlayers finished: %v", err)
	}
	for _, p := range finalPlayers {
		switch p.ID {
		case alice.ID:
			if p.Points != 1 || p.IsWinner {
				t.Errorf("alice = (points=%d, winner=%v), want (1, false)", p.Points, p.IsWinner)
			}
		case bob.ID:
			if p.Points != 2 || !p.IsWinner {
				t.Errorf("bob = (points=%d, winner=%v), want (2, true)", p.Points, p.IsWinner)
			}
		case carol.ID:
			if p.Points != 0 || p.IsWinner {
				t.Errorf("carol = (points=%d, winner=%v), want (0, false)", p.Points, p.IsWinner)
			}
		}
	}
}

func TestPickCardExcludesArchived(t *testing.T) {
	d := openTestDB(t)
	alice := createUser(t, d, "alice")
	bob := createUser(t, d, "bob")

	if err := doStart(d, alice, "classic", 5, 60); err != nil {
		t.Fatalf("doStart: %v", err)
	}
	if err := doJoin(d, bob); err != nil {
		t.Fatalf("doJoin: %v", err)
	}
	g, err := activeGame(d)
	if err != nil {
		t.Fatalf("activeGame: %v", err)
	}

	// Create 4 cards: 3 by Bob (preferred for Alice) and 1 by Alice.
	// Archive all of Bob's cards so only Alice's card remains active.
	bob1 := createCard(t, d, bob.ID, "Bob 1", "Q1?")
	bob2 := createCard(t, d, bob.ID, "Bob 2", "Q2?")
	bob3 := createCard(t, d, bob.ID, "Bob 3", "Q3?")
	onlyActive := createCard(t, d, alice.ID, "Alice active", "Q4?")

	for _, id := range []int64{bob1, bob2, bob3} {
		if _, err := d.Exec(`UPDATE cards SET archived_at = datetime('now') WHERE id = ?`, id); err != nil {
			t.Fatalf("archive card %d: %v", id, err)
		}
	}

	// Even when the only active card is authored by the drawer (forcing fallback)
	// and even after it has been used in a round (forcing deepest fallback),
	// pickCard must never return an archived card.
	for i := range 10 {
		got, err := pickCard(d, g.ID, alice.ID, "classic")
		if err != nil {
			t.Fatalf("pickCard classic #%d: %v", i, err)
		}
		if got != onlyActive {
			t.Fatalf("pickCard classic #%d = %d, want %d", i, got, onlyActive)
		}
	}

	// Mark the only active card as used in this game to exercise the final fallbacks.
	if _, err := d.Exec(
		`INSERT INTO rounds (game_id, card_id, drawer_id, status) VALUES (?, ?, ?, 'complete')`,
		g.ID, onlyActive, alice.ID,
	); err != nil {
		t.Fatalf("insert round: %v", err)
	}
	for _, mode := range []string{"classic", "freeplay"} {
		for i := range 10 {
			got, err := pickCard(d, g.ID, alice.ID, mode)
			if err != nil {
				t.Fatalf("pickCard fallback %s #%d: %v", mode, i, err)
			}
			if got != onlyActive {
				t.Fatalf("pickCard fallback %s #%d = %d, want %d", mode, i, got, onlyActive)
			}
		}
	}

	// Archiving the last card makes the deck empty.
	if _, err := d.Exec(`UPDATE cards SET archived_at = datetime('now') WHERE id = ?`, onlyActive); err != nil {
		t.Fatalf("archive last card: %v", err)
	}
	got, err := pickCard(d, g.ID, alice.ID, "classic")
	if err != nil {
		t.Fatalf("pickCard all archived: %v", err)
	}
	if got != 0 {
		t.Fatalf("pickCard all archived = %d, want 0", got)
	}
}

func freezeClock(t *testing.T, start time.Time) *time.Time {
	t.Helper()
	cur := start.UTC()
	orig := nowFunc
	nowFunc = func() time.Time { return cur }
	t.Cleanup(func() { nowFunc = orig })
	return &cur
}

func createAdmin(t *testing.T, d *sql.DB, username string) *web.User {
	t.Helper()
	res, err := d.Exec(
		`INSERT INTO users (username, password_hash, is_admin) VALUES (?, 'test-hash', 1)`,
		username,
	)
	if err != nil {
		t.Fatalf("insert admin %q: %v", username, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id for %q: %v", username, err)
	}
	return &web.User{ID: id, Username: username, IsAdmin: true}
}

func sessionCookieFor(t *testing.T, app *web.App, userID int64) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	rec := httptest.NewRecorder()
	if err := app.CreateSession(rec, req, userID); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 session cookie, got %d", len(cookies))
	}
	return cookies[0]
}

func TestDiscussionWindowAndDrawRefusal(t *testing.T) {
	d := openTestDB(t)
	now := freezeClock(t, time.Date(2026, 11, 23, 18, 0, 0, 0, time.UTC))

	alice := createUser(t, d, "alice")
	bob := createUser(t, d, "bob")
	carol := createUser(t, d, "carol")
	createCard(t, d, alice.ID, "S1", "Q1?")
	createCard(t, d, bob.ID, "S2", "Q2?")
	createCard(t, d, carol.ID, "S3", "Q3?")

	if err := doStart(d, alice, "classic", 5, 60); err != nil {
		t.Fatalf("doStart: %v", err)
	}
	if err := doJoin(d, bob); err != nil {
		t.Fatalf("doJoin bob: %v", err)
	}
	if err := doJoin(d, carol); err != nil {
		t.Fatalf("doJoin carol: %v", err)
	}

	// Complete Round 1 via doNext.
	if msg, err := doDraw(d, alice); err != nil || msg != "" {
		t.Fatalf("doDraw 1: msg=%q err=%v", msg, err)
	}
	g, rd1, err := loadCurrent(d)
	if err != nil {
		t.Fatalf("loadCurrent: %v", err)
	}
	if _, err := d.Exec(`UPDATE rounds SET drawer_answer = 'yes' WHERE id = ?`, rd1.ID); err != nil {
		t.Fatalf("answer: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO votes (round_id, user_id, phase, vote) VALUES (?, ?, 'initial', 'yes'), (?, ?, 'initial', 'no')`, rd1.ID, bob.ID, rd1.ID, carol.ID); err != nil {
		t.Fatalf("votes: %v", err)
	}
	if err := maybeReveal(d, g, rd1.ID); err != nil {
		t.Fatalf("maybeReveal: %v", err)
	}
	if err := doNext(d, alice); err != nil {
		t.Fatalf("doNext: %v", err)
	}

	_, rd1Done, err := loadCurrent(d)
	if err != nil {
		t.Fatalf("loadCurrent after doNext: %v", err)
	}
	if rd1Done.CompletedAt != "2026-11-23 18:00:00" || rd1Done.DiscussionEndsAt != "2026-11-23 18:01:00" {
		t.Fatalf("unexpected timestamps: completed=%q ends=%q", rd1Done.CompletedAt, rd1Done.DiscussionEndsAt)
	}

	// Draw is refused during discussion window.
	msg, err := doDraw(d, bob)
	if err != nil {
		t.Fatalf("doDraw during discussion: %v", err)
	}
	if msg == "" {
		t.Fatal("expected refusal message during discussion")
	}
	var roundCount int
	if err := d.QueryRow(`SELECT COUNT(*) FROM rounds WHERE game_id = ?`, g.ID).Scan(&roundCount); err != nil {
		t.Fatalf("count rounds: %v", err)
	}
	if roundCount != 1 {
		t.Fatalf("roundCount during discussion = %d, want 1", roundCount)
	}

	view, err := buildView(d, bob, "")
	if err != nil {
		t.Fatalf("buildView during discussion: %v", err)
	}
	if view.Discussion == nil {
		t.Fatal("expected Discussion view while window is open")
	}
	if view.Me.CanDraw {
		t.Fatal("expected CanDraw=false during discussion")
	}

	// Advance clock past discussion_ends_at -> draw allowed.
	*now = now.Add(60 * time.Second)
	viewAfter, err := buildView(d, bob, "")
	if err != nil {
		t.Fatalf("buildView after discussion: %v", err)
	}
	if viewAfter.Discussion != nil {
		t.Fatalf("expected Discussion=nil after expiry, got %+v", viewAfter.Discussion)
	}
	if !viewAfter.Me.CanDraw {
		t.Fatal("expected CanDraw=true for next drawer after discussion ends")
	}

	if msg, err := doDraw(d, bob); err != nil || msg != "" {
		t.Fatalf("doDraw after discussion: msg=%q err=%v", msg, err)
	}

	// Round 2 completed via maybeResolveChallenge also opens discussion window.
	g, rd2, err := loadCurrent(d)
	if err != nil {
		t.Fatalf("loadCurrent round 2: %v", err)
	}
	if rd2.ID == rd1.ID {
		t.Fatal("expected new round after discussion ended")
	}
	if _, err := d.Exec(`UPDATE rounds SET drawer_answer = 'yes' WHERE id = ?`, rd2.ID); err != nil {
		t.Fatalf("answer 2: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO votes (round_id, user_id, phase, vote) VALUES (?, ?, 'initial', 'no'), (?, ?, 'initial', 'no')`, rd2.ID, alice.ID, rd2.ID, carol.ID); err != nil {
		t.Fatalf("votes 2: %v", err)
	}
	if err := maybeReveal(d, g, rd2.ID); err != nil {
		t.Fatalf("maybeReveal 2: %v", err)
	}
	if _, err := d.Exec(`UPDATE rounds SET status = 'challenge', challenger_id = ? WHERE id = ?`, carol.ID, rd2.ID); err != nil {
		t.Fatalf("challenge 2: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO votes (round_id, user_id, phase, vote) VALUES (?, ?, 'challenge', 'no'), (?, ?, 'challenge', 'no')`, rd2.ID, alice.ID, rd2.ID, carol.ID); err != nil {
		t.Fatalf("challenge votes 2: %v", err)
	}
	if err := maybeResolveChallenge(d, g); err != nil {
		t.Fatalf("maybeResolveChallenge: %v", err)
	}

	_, rd2Done, err := loadCurrent(d)
	if err != nil {
		t.Fatalf("loadCurrent after challenge: %v", err)
	}
	if rd2Done.CompletedAt != "2026-11-23 18:01:00" || rd2Done.DiscussionEndsAt != "2026-11-23 18:02:00" {
		t.Fatalf("unexpected challenge timestamps: completed=%q ends=%q", rd2Done.CompletedAt, rd2Done.DiscussionEndsAt)
	}

	// Refused during challenge round discussion; allowed immediately after host skip.
	if msg, err := doDraw(d, carol); err != nil || msg == "" {
		t.Fatalf("expected draw refusal after challenge, got msg=%q err=%v", msg, err)
	}
	if err := doSkipDiscussion(d, alice); err != nil {
		t.Fatalf("doSkipDiscussion: %v", err)
	}
	if msg, err := doDraw(d, carol); err != nil || msg != "" {
		t.Fatalf("doDraw after skip: msg=%q err=%v", msg, err)
	}
}

func TestDiscussionZeroSkipsPhase(t *testing.T) {
	d := openTestDB(t)
	_ = freezeClock(t, time.Date(2026, 11, 23, 19, 0, 0, 0, time.UTC))

	alice := createUser(t, d, "alice")
	bob := createUser(t, d, "bob")
	createCard(t, d, alice.ID, "S1", "Q1?")
	createCard(t, d, bob.ID, "S2", "Q2?")

	if err := doStart(d, alice, "freeplay", 0, 0); err != nil {
		t.Fatalf("doStart: %v", err)
	}
	if err := doJoin(d, bob); err != nil {
		t.Fatalf("doJoin: %v", err)
	}
	if msg, err := doDraw(d, alice); err != nil || msg != "" {
		t.Fatalf("doDraw 1: msg=%q err=%v", msg, err)
	}
	g, rd, err := loadCurrent(d)
	if err != nil {
		t.Fatalf("loadCurrent: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO votes (round_id, user_id, phase, vote) VALUES (?, ?, 'initial', 'yes'), (?, ?, 'initial', 'no')`, rd.ID, alice.ID, rd.ID, bob.ID); err != nil {
		t.Fatalf("votes: %v", err)
	}
	if err := maybeReveal(d, g, rd.ID); err != nil {
		t.Fatalf("maybeReveal: %v", err)
	}
	if err := doNext(d, alice); err != nil {
		t.Fatalf("doNext: %v", err)
	}

	view, err := buildView(d, alice, "")
	if err != nil {
		t.Fatalf("buildView: %v", err)
	}
	if view.Discussion != nil {
		t.Fatalf("expected no discussion phase when discussion_seconds=0, got %+v", view.Discussion)
	}
	if !view.Me.CanDraw {
		t.Fatal("expected CanDraw=true immediately when discussion_seconds=0")
	}
	if msg, err := doDraw(d, bob); err != nil || msg != "" {
		t.Fatalf("expected immediate draw with discussion_seconds=0, got msg=%q err=%v", msg, err)
	}
}

func TestDiscussionPermissionsHostAndAdmin(t *testing.T) {
	d := openTestDB(t)
	_ = freezeClock(t, time.Date(2026, 11, 23, 20, 0, 0, 0, time.UTC))

	host := createUser(t, d, "host")
	guest := createUser(t, d, "guest")
	admin := createAdmin(t, d, "admin")
	createCard(t, d, guest.ID, "S1", "Q1?")

	if err := doStart(d, host, "classic", 5, 60); err != nil {
		t.Fatalf("doStart: %v", err)
	}
	if err := doJoin(d, guest); err != nil {
		t.Fatalf("doJoin: %v", err)
	}
	if _, err := doDraw(d, host); err != nil {
		t.Fatalf("doDraw: %v", err)
	}
	g, rd, err := loadCurrent(d)
	if err != nil {
		t.Fatalf("loadCurrent: %v", err)
	}
	if _, err := d.Exec(`UPDATE rounds SET drawer_answer = 'yes' WHERE id = ?`, rd.ID); err != nil {
		t.Fatalf("answer: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO votes (round_id, user_id, phase, vote) VALUES (?, ?, 'initial', 'yes')`, rd.ID, guest.ID); err != nil {
		t.Fatalf("vote: %v", err)
	}
	if err := maybeReveal(d, g, rd.ID); err != nil {
		t.Fatalf("maybeReveal: %v", err)
	}
	if err := doNext(d, host); err != nil {
		t.Fatalf("doNext: %v", err)
	}

	// Non-host guest cannot extend, change duration, or skip.
	if err := doExtendDiscussion(d, guest); err != nil {
		t.Fatalf("guest extend: %v", err)
	}
	if err := doSetDiscussionSeconds(d, guest, 120); err != nil {
		t.Fatalf("guest set duration: %v", err)
	}
	if err := doSkipDiscussion(d, guest); err != nil {
		t.Fatalf("guest skip: %v", err)
	}
	gAfter, rdAfter, err := loadCurrent(d)
	if err != nil {
		t.Fatalf("loadCurrent after guest attempts: %v", err)
	}
	if gAfter.DiscussionSeconds != 60 {
		t.Fatalf("guest changed discussion_seconds to %d, want 60", gAfter.DiscussionSeconds)
	}
	if rdAfter.DiscussionEndsAt != "2026-11-23 20:01:00" {
		t.Fatalf("guest changed discussion_ends_at to %q, want 2026-11-23 20:01:00", rdAfter.DiscussionEndsAt)
	}

	// Host can extend (+30s) and change duration.
	if err := doExtendDiscussion(d, host); err != nil {
		t.Fatalf("host extend: %v", err)
	}
	if err := doSetDiscussionSeconds(d, host, 90); err != nil {
		t.Fatalf("host set duration: %v", err)
	}
	gAfter, rdAfter, err = loadCurrent(d)
	if err != nil {
		t.Fatalf("loadCurrent after host actions: %v", err)
	}
	if gAfter.DiscussionSeconds != 90 {
		t.Fatalf("discussion_seconds = %d, want 90", gAfter.DiscussionSeconds)
	}
	if rdAfter.DiscussionEndsAt != "2026-11-23 20:01:30" {
		t.Fatalf("discussion_ends_at after +30s = %q, want 2026-11-23 20:01:30", rdAfter.DiscussionEndsAt)
	}

	// Admin (non-creator) can also extend, change duration, and skip.
	if err := doExtendDiscussion(d, admin); err != nil {
		t.Fatalf("admin extend: %v", err)
	}
	if err := doSetDiscussionSeconds(d, admin, 120); err != nil {
		t.Fatalf("admin set duration: %v", err)
	}
	gAfter, rdAfter, err = loadCurrent(d)
	if err != nil {
		t.Fatalf("loadCurrent after admin extend/duration: %v", err)
	}
	if gAfter.DiscussionSeconds != 120 || rdAfter.DiscussionEndsAt != "2026-11-23 20:02:00" {
		t.Fatalf("admin actions failed: ds=%d ends=%q", gAfter.DiscussionSeconds, rdAfter.DiscussionEndsAt)
	}
	if err := doSkipDiscussion(d, admin); err != nil {
		t.Fatalf("admin skip: %v", err)
	}
	_, rdSkipped, err := loadCurrent(d)
	if err != nil {
		t.Fatalf("loadCurrent after admin skip: %v", err)
	}
	if rdSkipped.DiscussionEndsAt != "2026-11-23 20:00:00" {
		t.Fatalf("discussion_ends_at after admin skip = %q, want 2026-11-23 20:00:00", rdSkipped.DiscussionEndsAt)
	}
}

func TestGameState204AndRoutes(t *testing.T) {
	t.Chdir("../..")
	d := openTestDB(t)
	now := freezeClock(t, time.Date(2026, 11, 23, 21, 0, 0, 0, time.UTC))

	app := web.NewApp(d)
	mux := http.NewServeMux()
	Routes(mux, app)

	alice := createUser(t, d, "alice")
	bob := createUser(t, d, "bob")
	createCard(t, d, bob.ID, "Scene A", "Question A?")
	createCard(t, d, alice.ID, "Scene B", "Question B?")

	aliceCookie := sessionCookieFor(t, app, alice.ID)
	bobCookie := sessionCookieFor(t, app, bob.ID)

	doReq := func(method, target string, cookie *http.Cookie, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		var body *strings.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		} else {
			body = strings.NewReader("")
		}
		req := httptest.NewRequest(method, target, body)
		req.Header.Set("HX-Request", "true")
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// Full page GET /game renders cleanly with bottom sheet outside #game-state.
	reqFull := httptest.NewRequest(http.MethodGet, "/game", nil)
	reqFull.AddCookie(aliceCookie)
	recFull := httptest.NewRecorder()
	mux.ServeHTTP(recFull, reqFull)
	if recFull.Code != http.StatusOK {
		t.Fatalf("GET /game status = %d, want 200", recFull.Code)
	}
	fullBody := recFull.Body.String()
	if !strings.Contains(fullBody, `id="history-sheet"`) || !strings.Contains(fullBody, `id="game-state-v"`) {
		t.Fatalf("GET /game missing history-sheet or game-state-v")
	}

	// Start game via POST /game/start with discussion_seconds=30.
	rec := doReq(http.MethodPost, "/game/start", aliceCookie, url.Values{
		"mode":               {"classic"},
		"point_goal":         {"5"},
		"discussion_seconds": {"30"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /game/start status = %d, want 200", rec.Code)
	}
	v1, err := buildView(d, alice, "")
	if err != nil {
		t.Fatalf("buildView 1: %v", err)
	}
	if v1.DiscussionSeconds != 30 {
		t.Fatalf("DiscussionSeconds = %d, want 30", v1.DiscussionSeconds)
	}

	// Poll with matching version -> 204 No Content.
	rec204 := doReq(http.MethodGet, "/game/state?v="+v1.StateVersion, aliceCookie, nil)
	if rec204.Code != http.StatusNoContent {
		t.Fatalf("GET /game/state unchanged status = %d, want 204", rec204.Code)
	}
	if rec204.Body.Len() != 0 {
		t.Fatalf("GET /game/state 204 body not empty: %q", rec204.Body.String())
	}

	// Bob joins -> state changes -> GET /game/state with old version returns 200.
	recJoin := doReq(http.MethodPost, "/game/join", bobCookie, nil)
	if recJoin.Code != http.StatusOK {
		t.Fatalf("POST /game/join status = %d, want 200", recJoin.Code)
	}
	rec200 := doReq(http.MethodGet, "/game/state?v="+v1.StateVersion, aliceCookie, nil)
	if rec200.Code != http.StatusOK {
		t.Fatalf("GET /game/state after change status = %d, want 200", rec200.Code)
	}

	// Play a round into discussion.
	doReq(http.MethodPost, "/game/draw", aliceCookie, nil)
	doReq(http.MethodPost, "/game/answer", aliceCookie, url.Values{"answer": {"yes"}})
	doReq(http.MethodPost, "/game/vote", bobCookie, url.Values{"vote": {"yes"}})
	recDisc := doReq(http.MethodPost, "/game/next", aliceCookie, nil)
	if recDisc.Code != http.StatusOK || !strings.Contains(recDisc.Body.String(), "data-discussion-ring") {
		t.Fatalf("expected discussion screen after /game/next, code=%d", recDisc.Code)
	}

	// Version stays unchanged as seconds tick inside the discussion window -> 204.
	vDisc, err := buildView(d, alice, "")
	if err != nil {
		t.Fatalf("buildView disc: %v", err)
	}
	*now = now.Add(10 * time.Second)
	recTick := doReq(http.MethodGet, "/game/state?v="+vDisc.StateVersion, aliceCookie, nil)
	if recTick.Code != http.StatusNoContent {
		t.Fatalf("GET /game/state during discussion tick status = %d, want 204", recTick.Code)
	}

	// Host extends -> version changes -> 200.
	recExt := doReq(http.MethodPost, "/game/discussion/extend", aliceCookie, nil)
	if recExt.Code != http.StatusOK {
		t.Fatalf("POST /game/discussion/extend status = %d, want 200", recExt.Code)
	}
	recAfterExt := doReq(http.MethodGet, "/game/state?v="+vDisc.StateVersion, aliceCookie, nil)
	if recAfterExt.Code != http.StatusOK {
		t.Fatalf("GET /game/state after extend status = %d, want 200", recAfterExt.Code)
	}

	// Invalid duration gets 400.
	recBadDur := doReq(http.MethodPost, "/game/discussion/duration", aliceCookie, url.Values{"discussion_seconds": {"45"}})
	if recBadDur.Code != http.StatusBadRequest {
		t.Fatalf("POST /game/discussion/duration bad value status = %d, want 400", recBadDur.Code)
	}

	// Valid duration updates game.
	recGoodDur := doReq(http.MethodPost, "/game/discussion/duration", aliceCookie, url.Values{"discussion_seconds": {"120"}})
	if recGoodDur.Code != http.StatusOK {
		t.Fatalf("POST /game/discussion/duration status = %d, want 200", recGoodDur.Code)
	}

	// Host skips -> discussion ends -> GET /game/rounds renders history sheet.
	recSkip := doReq(http.MethodPost, "/game/discussion/skip", aliceCookie, nil)
	if recSkip.Code != http.StatusOK || strings.Contains(recSkip.Body.String(), "data-discussion-ring") {
		t.Fatalf("expected discussion ended after skip, code=%d", recSkip.Code)
	}
	recHist := doReq(http.MethodGet, "/game/rounds", bobCookie, nil)
	if recHist.Code != http.StatusOK || !strings.Contains(recHist.Body.String(), "Question A?") {
		t.Fatalf("GET /game/rounds status=%d missing card question", recHist.Code)
	}
	recList := doReq(http.MethodGet, "/game/rounds?tab=list", bobCookie, nil)
	if recList.Code != http.StatusOK || !strings.Contains(recList.Body.String(), "Rounds (1)") {
		t.Fatalf("GET /game/rounds?tab=list status=%d unexpected body", recList.Code)
	}
}

func TestLoadCompletedRoundsHistory(t *testing.T) {
	d := openTestDB(t)
	_ = freezeClock(t, time.Date(2026, 11, 23, 22, 0, 0, 0, time.UTC))

	alice := createUser(t, d, "alice")
	bob := createUser(t, d, "bob")
	carol := createUser(t, d, "carol")

	c1 := createCard(t, d, bob.ID, "Circumstance 1", "Question 1?")
	c2 := createCard(t, d, carol.ID, "Circumstance 2", "Question 2?")
	c3 := createCard(t, d, alice.ID, "Circumstance 3", "Question 3?")
	_ = c1
	_ = c2
	_ = c3

	if err := doStart(d, alice, "classic", 10, 0); err != nil {
		t.Fatalf("doStart: %v", err)
	}
	if err := doJoin(d, bob); err != nil {
		t.Fatalf("doJoin bob: %v", err)
	}
	if err := doJoin(d, carol); err != nil {
		t.Fatalf("doJoin carol: %v", err)
	}

	// Round 1: Alice draws -> yes; Bob=yes (+1), Carol=no (0). Accepted.
	if _, err := doDraw(d, alice); err != nil {
		t.Fatalf("draw 1: %v", err)
	}
	g, r1, _ := loadCurrent(d)
	_, _ = d.Exec(`UPDATE rounds SET drawer_answer = 'yes' WHERE id = ?`, r1.ID)
	_, _ = d.Exec(`INSERT INTO votes (round_id, user_id, phase, vote) VALUES (?, ?, 'initial', 'yes'), (?, ?, 'initial', 'no')`, r1.ID, bob.ID, r1.ID, carol.ID)
	_ = maybeReveal(d, g, r1.ID)
	_ = doNext(d, alice)

	// Round 2: Bob draws -> yes; Alice=no, Carol=no. Challenged by Carol -> overturned to no!
	if _, err := doDraw(d, bob); err != nil {
		t.Fatalf("draw 2: %v", err)
	}
	g, r2, _ := loadCurrent(d)
	_, _ = d.Exec(`UPDATE rounds SET drawer_answer = 'yes' WHERE id = ?`, r2.ID)
	_, _ = d.Exec(`INSERT INTO votes (round_id, user_id, phase, vote) VALUES (?, ?, 'initial', 'no'), (?, ?, 'initial', 'no')`, r2.ID, alice.ID, r2.ID, carol.ID)
	_ = maybeReveal(d, g, r2.ID)
	_, _ = d.Exec(`UPDATE rounds SET status = 'challenge', challenger_id = ? WHERE id = ?`, carol.ID, r2.ID)
	_, _ = d.Exec(`INSERT INTO votes (round_id, user_id, phase, vote) VALUES (?, ?, 'challenge', 'no'), (?, ?, 'challenge', 'no')`, r2.ID, alice.ID, r2.ID, carol.ID)
	_ = maybeResolveChallenge(d, g)

	// Round 3: still voting -> must NOT appear in completed history.
	if _, err := doDraw(d, carol); err != nil {
		t.Fatalf("draw 3: %v", err)
	}

	history, err := loadCompletedRounds(d, g)
	if err != nil {
		t.Fatalf("loadCompletedRounds: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("len(history) = %d, want 2", len(history))
	}

	if history[0].ID != r1.ID || history[0].Number != 1 || history[0].DrawerName != "alice" {
		t.Fatalf("round 1 mismatch: %+v", history[0])
	}
	if history[0].DrawerAnswer != "yes" || history[0].FinalAnswer != "yes" || history[0].Challenged {
		t.Fatalf("round 1 answers mismatch: %+v", history[0])
	}
	if len(history[0].YesVotes) != 1 || history[0].YesVotes[0].Name != "bob" || !history[0].YesVotes[0].Correct {
		t.Fatalf("round 1 yes votes = %+v", history[0].YesVotes)
	}
	if len(history[0].NoVotes) != 1 || history[0].NoVotes[0].Name != "carol" || history[0].NoVotes[0].Correct {
		t.Fatalf("round 1 no votes = %+v", history[0].NoVotes)
	}

	if history[1].ID != r2.ID || history[1].Number != 2 || history[1].DrawerName != "bob" {
		t.Fatalf("round 2 mismatch: %+v", history[1])
	}
	if !history[1].Challenged || history[1].ChallengerName != "carol" || !history[1].ChallengeOverturned {
		t.Fatalf("round 2 challenge mismatch: %+v", history[1])
	}
	if history[1].DrawerAnswer != "yes" || history[1].FinalAnswer != "no" {
		t.Fatalf("round 2 answers = (%q, %q), want (yes, no)", history[1].DrawerAnswer, history[1].FinalAnswer)
	}
	if history[1].ChallengeFlip != 2 || history[1].ChallengeAgree != 0 {
		t.Fatalf("round 2 challenge counts = (flip=%d, agree=%d), want (2, 0)", history[1].ChallengeFlip, history[1].ChallengeAgree)
	}
	if len(history[1].NoVotes) != 2 || !history[1].NoVotes[0].Correct || !history[1].NoVotes[1].Correct {
		t.Fatalf("round 2 no votes should both be correct after flip: %+v", history[1].NoVotes)
	}
}
