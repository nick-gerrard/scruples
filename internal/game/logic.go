package game

import (
	"database/sql"
	"errors"
	"time"

	"scruples/internal/web"
)

// TimerOption is an allowed discussion window duration.
type TimerOption struct {
	Seconds int
	Label   string
}

var discussionOptions = []TimerOption{
	{Seconds: 0, Label: "Off"},
	{Seconds: 30, Label: "30s"},
	{Seconds: 60, Label: "60s"},
	{Seconds: 90, Label: "90s"},
	{Seconds: 120, Label: "2m"},
	{Seconds: 180, Label: "3m"},
}

func validDiscussionSeconds(n int) bool {
	for _, opt := range discussionOptions {
		if opt.Seconds == n {
			return true
		}
	}
	return false
}

// nowFunc is the clock used for discussion windows; overridable in tests.
var nowFunc = func() time.Time { return time.Now().UTC() }

func formatTime(t time.Time) string {
	return t.UTC().Format(time.DateTime)
}

func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.DateTime, s); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

func isHost(g *gameRow, u *web.User) bool {
	return u.ID == g.CreatedBy || u.IsAdmin
}

func discussionOpen(rd *roundRow, now time.Time) bool {
	if rd == nil || rd.Status != "complete" || rd.DiscussionEndsAt == "" {
		return false
	}
	endsAt, ok := parseTime(rd.DiscussionEndsAt)
	if !ok {
		return false
	}
	return now.UTC().Before(endsAt)
}

// doStart creates a new game if none is active. The creator auto-joins with
// turn_order 1. Silently a no-op if a game is already running (race-safe:
// the check and insert share one transaction, and the pool has one conn).
func doStart(db *sql.DB, u *web.User, mode string, pointGoal, discussionSeconds int, includeAI bool) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var active int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM games WHERE status = 'active'`).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		return nil
	}
	var goal any
	if mode == "classic" {
		goal = pointGoal
	}
	incAI := 0
	if includeAI {
		incAI = 1
	}
	res, err := tx.Exec(
		`INSERT INTO games (mode, point_goal, created_by, discussion_seconds, include_ai) VALUES (?, ?, ?, ?, ?)`,
		mode, goal, u.ID, discussionSeconds, incAI)
	if err != nil {
		return err
	}
	gid, err := res.LastInsertId()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO game_players (game_id, user_id, turn_order) VALUES (?, ?, 1)`,
		gid, u.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// doToggleAI flips include_ai on the active game. Only the game creator or an
// admin may toggle it; otherwise it is a silent no-op.
func doToggleAI(db *sql.DB, u *web.User) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	g, err := activeGame(tx)
	if err != nil || g == nil {
		return err
	}
	if u.ID != g.CreatedBy && !u.IsAdmin {
		return nil
	}
	next := 1
	if g.IncludeAI {
		next = 0
	}
	if _, err := tx.Exec(
		`UPDATE games SET include_ai = ? WHERE id = ? AND status = 'active'`,
		next, g.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// doJoin adds the user to the active game with the next turn_order.
func doJoin(db *sql.DB, u *web.User) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	g, err := activeGame(tx)
	if err != nil || g == nil {
		return err
	}
	joined, err := isPlayer(tx, g.ID, u.ID)
	if err != nil || joined {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO game_players (game_id, user_id, turn_order)
		 VALUES (?, ?, (SELECT COALESCE(MAX(turn_order), 0) + 1 FROM game_players WHERE game_id = ?))`,
		g.ID, u.ID, g.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// pickCard chooses a random card for a new round. Preference order:
// unused in this game and (classic) not by the drawer, then any unused,
// then (classic) any not by the drawer, then any card at all.
// Always excludes archived cards, and excludes AI cards when includeAI is false.
// Returns 0 if there are no eligible cards.
func pickCard(q querier, gameID, drawerID int64, mode string, includeAI bool) (int64, error) {
	type cand struct {
		sql  string
		args []any
	}
	baseWhere := "archived_at IS NULL"
	if !includeAI {
		baseWhere += " AND source != 'ai'"
	}
	var cands []cand
	if mode == "classic" {
		cands = append(cands, cand{
			`SELECT id FROM cards
			 WHERE ` + baseWhere + `
			   AND id NOT IN (SELECT card_id FROM rounds WHERE game_id = ?) AND author_id != ?
			 ORDER BY RANDOM() LIMIT 1`, []any{gameID, drawerID}})
	}
	cands = append(cands, cand{
		`SELECT id FROM cards
		 WHERE ` + baseWhere + `
		   AND id NOT IN (SELECT card_id FROM rounds WHERE game_id = ?)
		 ORDER BY RANDOM() LIMIT 1`, []any{gameID}})
	if mode == "classic" {
		cands = append(cands, cand{
			`SELECT id FROM cards WHERE ` + baseWhere + ` AND author_id != ? ORDER BY RANDOM() LIMIT 1`, []any{drawerID}})
	}
	cands = append(cands, cand{`SELECT id FROM cards WHERE ` + baseWhere + ` ORDER BY RANDOM() LIMIT 1`, nil})

	for _, c := range cands {
		var id int64
		err := q.QueryRow(c.sql, c.args...).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, err
		}
		return id, nil
	}
	return 0, nil
}

// doDraw opens a new round. Returns a user-facing message for soft failures
// (discussion still open, or an empty deck).
func doDraw(db *sql.DB, u *web.User) (string, error) {
	tx, err := db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	g, rd, err := loadCurrent(tx)
	if err != nil {
		return "", err
	}
	if g == nil || (rd != nil && rd.Status != "complete") {
		return "", nil
	}
	if discussionOpen(rd, nowFunc()) {
		return "Discussion is still open.", nil
	}
	players, err := loadPlayers(tx, g.ID)
	if err != nil {
		return "", err
	}
	joined := false
	for _, p := range players {
		if p.ID == u.ID {
			joined = true
		}
	}
	if !joined {
		return "", nil
	}
	if g.Mode == "classic" && nextDrawerIDFor(players, rd) != u.ID {
		return "", nil
	}
	cardID, err := pickCard(tx, g.ID, u.ID, g.Mode, g.IncludeAI)
	if err != nil {
		return "", err
	}
	if cardID == 0 {
		return "The deck is empty — head to the Deck and write some dilemmas first.", nil
	}
	if _, err := tx.Exec(
		`INSERT INTO rounds (game_id, card_id, drawer_id) VALUES (?, ?, ?)`,
		g.ID, cardID, u.ID); err != nil {
		return "", err
	}
	return "", tx.Commit()
}

// maybeReveal flips a voting round to revealed once the drawer has answered
// (classic) and every eligible expected voter has an initial vote in.
func maybeReveal(q querier, g *gameRow, roundID int64) error {
	rd, err := roundByID(q, roundID)
	if err != nil || rd == nil {
		return err
	}
	if rd.Status != "voting" {
		return nil
	}
	if g.Mode == "classic" && rd.DrawerAnswer == "" {
		return nil
	}
	// In classic the drawer does not vote; excl=0 matches nobody (ids start at 1).
	var excl int64
	if g.Mode == "classic" {
		excl = rd.DrawerID
	}
	var elig, voted int
	if err := q.QueryRow(
		`SELECT COUNT(*) FROM game_players
		 WHERE game_id = ? AND joined_at <= ? AND user_id != ?`,
		g.ID, rd.CreatedAt, excl).Scan(&elig); err != nil {
		return err
	}
	if err := q.QueryRow(
		`SELECT COUNT(*) FROM votes v
		 JOIN game_players gp ON gp.game_id = ? AND gp.user_id = v.user_id
		 WHERE v.round_id = ? AND v.phase = 'initial' AND gp.joined_at <= ? AND v.user_id != ?`,
		g.ID, rd.ID, rd.CreatedAt, excl).Scan(&voted); err != nil {
		return err
	}
	if voted >= elig {
		_, err = q.Exec(`UPDATE rounds SET status = 'revealed' WHERE id = ? AND status = 'voting'`, rd.ID)
	}
	return err
}

// awardAndFinish gives +1 to every eligible non-drawer whose initial vote
// matched the final answer, then finishes the game if anyone reached the goal.
// Classic games only. Must run inside the caller's transaction.
func awardAndFinish(tx *sql.Tx, g *gameRow, rd *roundRow, final string) error {
	if _, err := tx.Exec(
		`UPDATE game_players SET points = points + 1
		 WHERE game_id = ? AND joined_at <= ? AND user_id != ?
		   AND user_id IN (SELECT user_id FROM votes WHERE round_id = ? AND phase = 'initial' AND vote = ?)`,
		g.ID, rd.CreatedAt, rd.DrawerID, rd.ID, final); err != nil {
		return err
	}
	var max int
	if err := tx.QueryRow(
		`SELECT COALESCE(MAX(points), 0) FROM game_players WHERE game_id = ?`,
		g.ID).Scan(&max); err != nil {
		return err
	}
	if g.PointGoal > 0 && max >= g.PointGoal {
		if _, err := tx.Exec(
			`UPDATE games SET status = 'finished', finished_at = datetime('now')
			 WHERE id = ? AND status = 'active'`, g.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`UPDATE game_players SET is_winner = 1
			 WHERE game_id = ? AND points = (SELECT MAX(points) FROM game_players WHERE game_id = ?)`,
			g.ID, g.ID); err != nil {
			return err
		}
	}
	return nil
}

// doNext accepts a revealed round: classic locks in the drawer's answer as
// final and scores it; freeplay simply completes the round. Sets completed_at
// and discussion_ends_at according to the game's discussion_seconds.
func doNext(db *sql.DB, u *web.User) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	g, rd, err := loadCurrent(tx)
	if err != nil {
		return err
	}
	if g == nil || rd == nil || rd.Status != "revealed" {
		return nil
	}
	joined, err := isPlayer(tx, g.ID, u.ID)
	if err != nil || !joined {
		return err
	}
	now := nowFunc().UTC()
	completedAt := formatTime(now)
	endsAt := formatTime(now.Add(time.Duration(g.DiscussionSeconds) * time.Second))

	if g.Mode == "classic" {
		if rd.DrawerAnswer == "" {
			return nil
		}
		res, err := tx.Exec(
			`UPDATE rounds
			 SET status = 'complete', final_answer = drawer_answer, completed_at = ?, discussion_ends_at = ?
			 WHERE id = ? AND status = 'revealed'`,
			completedAt, endsAt, rd.ID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n > 0 {
			if err := awardAndFinish(tx, g, rd, rd.DrawerAnswer); err != nil {
				return err
			}
		}
	} else {
		if _, err := tx.Exec(
			`UPDATE rounds
			 SET status = 'complete', completed_at = ?, discussion_ends_at = ?
			 WHERE id = ? AND status = 'revealed'`,
			completedAt, endsAt, rd.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// maybeResolveChallenge resolves a challenge once every eligible non-drawer
// has re-voted: a strict majority contradicting the drawer flips the answer;
// a tie or majority agreeing lets it stand. Then scores initial votes and opens
// the discussion window.
func maybeResolveChallenge(db *sql.DB, g *gameRow) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rd, err := latestRound(tx, g.ID)
	if err != nil {
		return err
	}
	if rd == nil || rd.Status != "challenge" || rd.DrawerAnswer == "" {
		return nil
	}
	var discussionSeconds int
	if err := tx.QueryRow(`SELECT discussion_seconds FROM games WHERE id = ?`, g.ID).Scan(&discussionSeconds); err != nil {
		return err
	}
	var elig, voted int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM game_players
		 WHERE game_id = ? AND joined_at <= ? AND user_id != ?`,
		g.ID, rd.CreatedAt, rd.DrawerID).Scan(&elig); err != nil {
		return err
	}
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM votes v
		 JOIN game_players gp ON gp.game_id = ? AND gp.user_id = v.user_id
		 WHERE v.round_id = ? AND v.phase = 'challenge' AND gp.joined_at <= ? AND v.user_id != ?`,
		g.ID, rd.ID, rd.CreatedAt, rd.DrawerID).Scan(&voted); err != nil {
		return err
	}
	if voted < elig {
		return nil
	}
	flipped := "no"
	if rd.DrawerAnswer == "no" {
		flipped = "yes"
	}
	var agree, flip int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM votes WHERE round_id = ? AND phase = 'challenge' AND vote = ?`,
		rd.ID, rd.DrawerAnswer).Scan(&agree); err != nil {
		return err
	}
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM votes WHERE round_id = ? AND phase = 'challenge' AND vote = ?`,
		rd.ID, flipped).Scan(&flip); err != nil {
		return err
	}
	final := rd.DrawerAnswer
	if flip > agree {
		final = flipped
	}
	now := nowFunc().UTC()
	completedAt := formatTime(now)
	endsAt := formatTime(now.Add(time.Duration(discussionSeconds) * time.Second))
	res, err := tx.Exec(
		`UPDATE rounds
		 SET status = 'complete', final_answer = ?, completed_at = ?, discussion_ends_at = ?
		 WHERE id = ? AND status = 'challenge'`,
		final, completedAt, endsAt, rd.ID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return nil
	}
	if err := awardAndFinish(tx, g, rd, final); err != nil {
		return err
	}
	return tx.Commit()
}

// doSkipDiscussion ends the active discussion window immediately. Host or admin only.
func doSkipDiscussion(db *sql.DB, u *web.User) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	g, rd, err := loadCurrent(tx)
	if err != nil || g == nil || !isHost(g, u) {
		return err
	}
	now := nowFunc().UTC()
	if !discussionOpen(rd, now) {
		return nil
	}
	if _, err := tx.Exec(
		`UPDATE rounds SET discussion_ends_at = ? WHERE id = ? AND status = 'complete'`,
		formatTime(now), rd.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// doExtendDiscussion adds 30 seconds to an open discussion window. Host or admin only.
func doExtendDiscussion(db *sql.DB, u *web.User) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	g, rd, err := loadCurrent(tx)
	if err != nil || g == nil || !isHost(g, u) {
		return err
	}
	now := nowFunc().UTC()
	if !discussionOpen(rd, now) {
		return nil
	}
	endsAt, ok := parseTime(rd.DiscussionEndsAt)
	if !ok {
		return nil
	}
	newEnds := endsAt.Add(30 * time.Second)
	if _, err := tx.Exec(
		`UPDATE rounds SET discussion_ends_at = ? WHERE id = ? AND status = 'complete'`,
		formatTime(newEnds), rd.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// doSetDiscussionSeconds updates the active game's discussion_seconds setting. Host or admin only.
func doSetDiscussionSeconds(db *sql.DB, u *web.User, seconds int) error {
	if !validDiscussionSeconds(seconds) {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	g, err := activeGame(tx)
	if err != nil || g == nil || !isHost(g, u) {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE games SET discussion_seconds = ? WHERE id = ? AND status = 'active'`,
		seconds, g.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// doEnd finishes the active game early. Classic: current max-point players
// become winners, but only if at least one point was scored.
func doEnd(db *sql.DB, u *web.User) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	g, err := activeGame(tx)
	if err != nil || g == nil {
		return err
	}
	if !isHost(g, u) {
		return nil
	}
	if g.Mode == "classic" {
		var max int
		if err := tx.QueryRow(
			`SELECT COALESCE(MAX(points), 0) FROM game_players WHERE game_id = ?`,
			g.ID).Scan(&max); err != nil {
			return err
		}
		if max >= 1 {
			if _, err := tx.Exec(
				`UPDATE game_players SET is_winner = 1
				 WHERE game_id = ? AND points = (SELECT MAX(points) FROM game_players WHERE game_id = ?)`,
				g.ID, g.ID); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(
		`UPDATE games SET status = 'finished', finished_at = datetime('now')
		 WHERE id = ? AND status = 'active'`, g.ID); err != nil {
		return err
	}
	return tx.Commit()
}
