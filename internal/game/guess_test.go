package game

import (
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"scruples/internal/web"
)

// guessFixture is a freeplay game (alice host, bob, carol) with one open round
// on a card written by bob. dave wrote a card but never joined.
type guessFixture struct {
	d                       *sql.DB
	alice, bob, carol, dave *web.User
	roundID                 int64
}

func newGuessFixture(t *testing.T, guess bool) *guessFixture {
	t.Helper()
	d := openTestDB(t)
	f := &guessFixture{d: d}
	f.alice = createUser(t, d, "alice")
	f.bob = createUser(t, d, "bob")
	f.carol = createUser(t, d, "carol")
	f.dave = createUser(t, d, "dave")
	card := createCard(t, d, f.bob.ID, "Scene", "Question?")
	createCard(t, d, f.carol.ID, "Scene C", "Question C?")
	createCard(t, d, f.dave.ID, "Scene D", "Question D?")

	if err := doStartGame(d, f.alice, "freeplay", 0, 60, true, guess); err != nil {
		t.Fatalf("doStartGame: %v", err)
	}
	for _, u := range []*web.User{f.bob, f.carol} {
		if err := doJoin(d, u); err != nil {
			t.Fatalf("doJoin: %v", err)
		}
	}
	g, err := activeGame(d)
	if err != nil {
		t.Fatalf("activeGame: %v", err)
	}
	res, err := d.Exec(`INSERT INTO rounds (game_id, card_id, drawer_id) VALUES (?, ?, ?)`, g.ID, card, f.alice.ID)
	if err != nil {
		t.Fatalf("insert round: %v", err)
	}
	f.roundID, _ = res.LastInsertId()
	return f
}

func (f *guessFixture) guessOf(t *testing.T, u *web.User) int64 {
	t.Helper()
	var id int64
	err := f.d.QueryRow(`SELECT guessed_author_id FROM author_guesses WHERE round_id = ? AND user_id = ?`, f.roundID, u.ID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0
	}
	if err != nil {
		t.Fatalf("select guess: %v", err)
	}
	return id
}

func (f *guessFixture) complete(t *testing.T) {
	t.Helper()
	if _, err := f.d.Exec(
		`UPDATE rounds SET status = 'complete', completed_at = '2026-11-23 20:59:00', discussion_ends_at = '2026-11-23 21:30:00' WHERE id = ?`,
		f.roundID); err != nil {
		t.Fatalf("complete round: %v", err)
	}
}

func TestDoGuessHappyPathAndChange(t *testing.T) {
	f := newGuessFixture(t, true)
	if err := doGuess(f.d, f.alice, f.bob.ID); err != nil {
		t.Fatalf("doGuess: %v", err)
	}
	if got := f.guessOf(t, f.alice); got != f.bob.ID {
		t.Fatalf("guess = %d, want bob %d", got, f.bob.ID)
	}
	if err := doGuess(f.d, f.alice, f.carol.ID); err != nil {
		t.Fatalf("change guess: %v", err)
	}
	if got := f.guessOf(t, f.alice); got != f.carol.ID {
		t.Fatalf("changed guess = %d, want carol %d", got, f.carol.ID)
	}
	var n int
	if err := f.d.QueryRow(`SELECT COUNT(*) FROM author_guesses WHERE round_id = ?`, f.roundID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("guess rows = %d (err %v), want 1", n, err)
	}
	// Guessing someone who has no cards (alice herself aside) is a non-candidate;
	// dave has an active card so he is valid even though he is not playing.
	if err := doGuess(f.d, f.carol, f.dave.ID); err != nil {
		t.Fatalf("guess dave: %v", err)
	}
}

func TestDoGuessRejections(t *testing.T) {
	f := newGuessFixture(t, true)
	outsider := createUser(t, f.d, "erin")
	reject := func(name string, u *web.User, id int64) {
		t.Helper()
		if err := doGuess(f.d, u, id); !errors.Is(err, errGuessRejected) {
			t.Errorf("%s: err = %v, want errGuessRejected", name, err)
		}
		if got := f.guessOf(t, u); got != 0 {
			t.Errorf("%s: guess row written (%d)", name, got)
		}
	}
	reject("author guesses", f.bob, f.carol.ID)
	reject("own id", f.alice, f.alice.ID)
	reject("non-candidate (no cards)", f.alice, outsider.ID)
	reject("unknown id", f.alice, 9999)
	reject("not in game", outsider, f.bob.ID)

	// Archived-only author is not a candidate.
	if _, err := f.d.Exec(`UPDATE cards SET archived_at = datetime('now') WHERE author_id = ?`, f.dave.ID); err != nil {
		t.Fatal(err)
	}
	reject("archived author", f.alice, f.dave.ID)

	// The Claude bot is a candidate only while AI cards are in.
	var botID int64
	if err := f.d.QueryRow(`SELECT id FROM users WHERE username = 'Claude'`).Scan(&botID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.Exec(`INSERT INTO cards (author_id, circumstance, question, source) VALUES (?, 's', 'q?', 'ai')`, botID); err != nil {
		t.Fatal(err)
	}
	if err := doGuess(f.d, f.alice, botID); err != nil {
		t.Fatalf("bot with AI on: %v", err)
	}
	if err := doToggleAI(f.d, f.alice); err != nil {
		t.Fatal(err)
	}
	reject("bot with AI off", f.carol, botID)

	// Completed round.
	f.complete(t)
	reject("complete round", f.carol, f.bob.ID)
}

func TestGuessCandidatesKeepCardAuthor(t *testing.T) {
	f := newGuessFixture(t, true)
	// Bob's cards get archived after the draw: he must stay a candidate.
	if _, err := f.d.Exec(`UPDATE cards SET archived_at = datetime('now') WHERE author_id = ?`, f.bob.ID); err != nil {
		t.Fatal(err)
	}
	cands, err := guessCandidates(f.d, true, f.alice.ID, f.bob.ID)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range cands {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "bob,carol,dave" {
		t.Fatalf("candidates = %v, want bob,carol,dave (name order)", names)
	}
	if err := doGuess(f.d, f.alice, f.bob.ID); err != nil {
		t.Fatalf("guess archived author: %v", err)
	}
	f.complete(t)
	g, _ := activeGame(f.d)
	rd, _ := latestRound(f.d, g.ID)
	s, err := buildRoundSummary(f.d, g, rd, 1, time.Date(2026, 11, 23, 21, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Guesses) != 1 || !s.Guesses[0].Correct {
		t.Fatalf("guesses = %+v, want one correct", s.Guesses)
	}
	// Someone who isn't the card author and has no active cards is still out.
	if _, err := f.d.Exec(`UPDATE cards SET archived_at = datetime('now') WHERE author_id = ?`, f.dave.ID); err != nil {
		t.Fatal(err)
	}
	if err := doGuess(f.d, f.carol, f.dave.ID); !errors.Is(err, errGuessRejected) {
		t.Fatalf("err = %v, want rejection", err)
	}
}

func TestDoGuessSettingOff(t *testing.T) {
	f := newGuessFixture(t, false)
	if err := doGuess(f.d, f.alice, f.bob.ID); !errors.Is(err, errGuessRejected) {
		t.Fatalf("err = %v, want errGuessRejected", err)
	}
	if got := f.guessOf(t, f.alice); got != 0 {
		t.Fatalf("guess written with setting off")
	}
	// Host can toggle it on; a non-host cannot.
	if err := doToggleGuess(f.d, f.bob); err != nil {
		t.Fatal(err)
	}
	if g, _ := activeGame(f.d); g.GuessAuthor {
		t.Fatal("non-host toggled guess_author")
	}
	if err := doToggleGuess(f.d, f.alice); err != nil {
		t.Fatal(err)
	}
	if g, _ := activeGame(f.d); !g.GuessAuthor {
		t.Fatal("host toggle did not enable guess_author")
	}
}

func TestGuessAuthorHiddenUntilComplete(t *testing.T) {
	t.Chdir("../..")
	f := newGuessFixture(t, true)
	freezeClock(t, time.Date(2026, 11, 23, 21, 0, 0, 0, time.UTC))
	app := web.NewApp(f.d)
	mux := http.NewServeMux()
	Routes(mux, app)

	get := func(u *web.User, target string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("HX-Request", "true")
		req.AddCookie(sessionCookieFor(t, app, u.ID))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", target, rec.Code)
		}
		return rec.Body.String()
	}
	post := func(u *web.User, target string, form url.Values) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
		req.Header.Set("HX-Request", "true")
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(sessionCookieFor(t, app, u.ID))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %s = %d", target, rec.Code)
		}
	}

	carolHTML := get(f.carol, "/game/state")
	if !strings.Contains(carolHTML, "Who wrote this?") || strings.Contains(carolHTML, "Your card") {
		t.Fatalf("non-author should see picker and not 'Your card'")
	}
	if strings.Contains(carolHTML, "Written by") {
		t.Fatalf("author revealed before completion")
	}
	if strings.Contains(carolHTML, `author_id":"`+itoa(f.carol.ID)+`"`) {
		t.Fatalf("picker offers the guesser themself")
	}
	bobHTML := get(f.bob, "/game/state")
	if !strings.Contains(bobHTML, "Your card") || strings.Contains(bobHTML, "Who wrote this?") {
		t.Fatalf("author should see 'Your card' and no picker")
	}

	// State version changes when a guess is made.
	vBefore, err := buildView(f.d, f.alice, "")
	if err != nil {
		t.Fatal(err)
	}
	post(f.alice, "/game/guess", url.Values{"author_id": {itoa(f.carol.ID)}})
	vAfter, err := buildView(f.d, f.alice, "")
	if err != nil {
		t.Fatal(err)
	}
	if vBefore.StateVersion == vAfter.StateVersion {
		t.Fatal("state version unchanged after a guess")
	}
	// Another player's version changes too ("N guessed").
	if got := get(f.carol, "/game/state"); !strings.Contains(got, "1 guessed") {
		t.Fatalf("expected '1 guessed' for others")
	}
	post(f.carol, "/game/guess", url.Values{"author_id": {itoa(f.bob.ID)}})

	// After completion the author and guesses are revealed, correct first.
	f.complete(t)
	done := get(f.alice, "/game/state")
	if !strings.Contains(done, "Written by") {
		t.Fatalf("author not shown after completion")
	}
	if strings.Index(done, "✓ carol") < 0 || strings.Index(done, "✗ alice") < 0 || strings.Index(done, "✓ carol") > strings.Index(done, "✗ alice") {
		t.Fatalf("expected correct guesser carol listed before wrong guesser alice")
	}
	if !strings.Contains(get(f.alice, "/game/rounds"), "Written by") {
		t.Fatalf("history sheet missing author reveal")
	}

	// Game over: best guesser line.
	if err := doEnd(f.d, f.alice); err != nil {
		t.Fatal(err)
	}
	if over := get(f.alice, "/game/state"); !strings.Contains(over, "Best guesser") || !strings.Contains(over, "carol") {
		t.Fatalf("game over missing best guesser line")
	}
}

func TestGuessSettingOffNoPicker(t *testing.T) {
	t.Chdir("../..")
	f := newGuessFixture(t, false)
	app := web.NewApp(f.d)
	mux := http.NewServeMux()
	Routes(mux, app)
	req := httptest.NewRequest(http.MethodGet, "/game/state", nil)
	req.Header.Set("HX-Request", "true")
	req.AddCookie(sessionCookieFor(t, app, f.carol.ID))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, s := range []string{"Who wrote this?", "Your card", "guessed", "Written by"} {
		if strings.Contains(body, s) {
			t.Errorf("setting off but page contains %q", s)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
