package game

import (
	"database/sql"
	"path/filepath"
	"testing"

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

	if err := doStart(d, alice, "classic", 7); err != nil {
		t.Fatalf("doStart: %v", err)
	}

	g, err := activeGame(d)
	if err != nil {
		t.Fatalf("activeGame: %v", err)
	}
	if g == nil {
		t.Fatal("expected active game, got nil")
	}
	if g.Mode != "classic" || g.PointGoal != 7 || g.CreatedBy != alice.ID {
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
	if err := doStart(d, bob, "freeplay", 0); err != nil {
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

	if err := doStart(d, alice, "classic", 5); err != nil {
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

	if err := doStart(d, alice, "classic", 5); err != nil {
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

		if err := doStart(d, alice, "classic", 5); err != nil {
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

		if err := doStart(d, alice, "freeplay", 0); err != nil {
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

	if err := doStart(d, alice, "classic", 2); err != nil {
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

	if err := doStart(d, alice, "classic", 5); err != nil {
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
