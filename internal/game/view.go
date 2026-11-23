package game

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"scruples/internal/web"
)

// querier is satisfied by both *sql.DB and *sql.Tx.
type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
	Exec(query string, args ...any) (sql.Result, error)
}

// ---- raw rows ----

type gameRow struct {
	ID                int64
	Mode              string
	PointGoal         int
	Status            string
	CreatedBy         int64
	DiscussionSeconds int
	IncludeAI         bool
	GuessAuthor       bool
}

type roundRow struct {
	ID               int64
	CardID           int64
	DrawerID         int64
	DrawerAnswer     string // "" until answered
	Status           string
	FinalAnswer      string // "" until finalized
	ChallengerID     int64  // 0 until challenged
	CreatedAt        string
	CompletedAt      string // "" until complete
	DiscussionEndsAt string // "" until complete
}

type playerRow struct {
	ID        int64
	Name      string
	Points    int
	TurnOrder int
	IsWinner  bool
	JoinedAt  string
}

func scanGame(row *sql.Row) (*gameRow, error) {
	g := &gameRow{}
	var goal sql.NullInt64
	var includeAI, guessAuthor int
	err := row.Scan(&g.ID, &g.Mode, &goal, &g.Status, &g.CreatedBy, &g.DiscussionSeconds, &includeAI, &guessAuthor)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	g.PointGoal = int(goal.Int64)
	g.IncludeAI = includeAI != 0
	g.GuessAuthor = guessAuthor != 0
	return g, nil
}

func activeGame(q querier) (*gameRow, error) {
	return scanGame(q.QueryRow(
		`SELECT id, mode, point_goal, status, created_by, discussion_seconds, include_ai, guess_author
		 FROM games WHERE status = 'active' ORDER BY id DESC LIMIT 1`))
}

func latestFinishedGame(q querier) (*gameRow, error) {
	return scanGame(q.QueryRow(
		`SELECT id, mode, point_goal, status, created_by, discussion_seconds, include_ai, guess_author
		 FROM games WHERE status = 'finished' ORDER BY id DESC LIMIT 1`))
}

func scanRound(row *sql.Row) (*roundRow, error) {
	rd := &roundRow{}
	var ans, fin, comp, discEnd sql.NullString
	var chal sql.NullInt64
	err := row.Scan(
		&rd.ID, &rd.CardID, &rd.DrawerID, &ans, &rd.Status, &fin, &chal,
		&rd.CreatedAt, &comp, &discEnd,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rd.DrawerAnswer = ans.String
	rd.FinalAnswer = fin.String
	rd.ChallengerID = chal.Int64
	rd.CompletedAt = comp.String
	rd.DiscussionEndsAt = discEnd.String
	return rd, nil
}

func latestRound(q querier, gameID int64) (*roundRow, error) {
	return scanRound(q.QueryRow(
		`SELECT id, card_id, drawer_id, drawer_answer, status, final_answer, challenger_id, created_at, completed_at, discussion_ends_at
		 FROM rounds WHERE game_id = ? ORDER BY id DESC LIMIT 1`, gameID))
}

func roundByID(q querier, id int64) (*roundRow, error) {
	return scanRound(q.QueryRow(
		`SELECT id, card_id, drawer_id, drawer_answer, status, final_answer, challenger_id, created_at, completed_at, discussion_ends_at
		 FROM rounds WHERE id = ?`, id))
}

// loadCurrent returns the active game (or nil) and its latest round (or nil).
func loadCurrent(q querier) (*gameRow, *roundRow, error) {
	g, err := activeGame(q)
	if err != nil || g == nil {
		return g, nil, err
	}
	rd, err := latestRound(q, g.ID)
	return g, rd, err
}

func loadPlayers(q querier, gameID int64) ([]playerRow, error) {
	rows, err := q.Query(
		`SELECT gp.user_id, u.username, gp.points, gp.turn_order, gp.is_winner, gp.joined_at
		 FROM game_players gp JOIN users u ON u.id = gp.user_id
		 WHERE gp.game_id = ? ORDER BY gp.turn_order`, gameID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []playerRow
	for rows.Next() {
		var p playerRow
		if err := rows.Scan(&p.ID, &p.Name, &p.Points, &p.TurnOrder, &p.IsWinner, &p.JoinedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// loadVotes returns initial and challenge votes as userID -> "yes"/"no".
func loadVotes(q querier, roundID int64) (map[int64]string, map[int64]string, error) {
	initial := map[int64]string{}
	challenge := map[int64]string{}
	rows, err := q.Query(`SELECT user_id, phase, vote FROM votes WHERE round_id = ?`, roundID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var uid int64
		var phase, vote string
		if err := rows.Scan(&uid, &phase, &vote); err != nil {
			return nil, nil, err
		}
		if phase == "initial" {
			initial[uid] = vote
		} else {
			challenge[uid] = vote
		}
	}
	return initial, challenge, rows.Err()
}

func isPlayer(q querier, gameID, userID int64) (bool, error) {
	var n int
	err := q.QueryRow(
		`SELECT COUNT(*) FROM game_players WHERE game_id = ? AND user_id = ?`,
		gameID, userID).Scan(&n)
	return n > 0, err
}

func isEligible(q querier, gameID, userID int64, roundCreated string) (bool, error) {
	var n int
	err := q.QueryRow(
		`SELECT COUNT(*) FROM game_players
		 WHERE game_id = ? AND user_id = ? AND joined_at <= ?`,
		gameID, userID, roundCreated).Scan(&n)
	return n > 0, err
}

// nextDrawerIDFor picks the next drawer: lowest turn_order if there was no
// previous round, otherwise the next-higher turn_order after the last round's
// drawer, wrapping around. players must be sorted by turn_order.
func nextDrawerIDFor(players []playerRow, last *roundRow) int64 {
	if len(players) == 0 {
		return 0
	}
	if last == nil {
		return players[0].ID
	}
	lastOrder := -1
	for _, p := range players {
		if p.ID == last.DrawerID {
			lastOrder = p.TurnOrder
		}
	}
	for _, p := range players {
		if p.TurnOrder > lastOrder {
			return p.ID
		}
	}
	return players[0].ID
}

// ---- view model ----

type CardView struct {
	Circumstance string
	Question     string
	Source       string
}

type PlayerView struct {
	ID       int64
	Name     string
	Points   int
	IsNext   bool
	IsWinner bool
	IsMe     bool
}

type VoteView struct {
	UserID  int64
	Name    string
	Vote    string
	Correct bool
}

type RoundView struct {
	ID                 int64
	Classic            bool
	Card               CardView
	DrawerID           int64
	DrawerName         string
	Status             string
	VotesIn            int
	VotesTotal         int
	WaitingOn          []string
	Revealed           []VoteView
	YesVotes           []VoteView
	NoVotes            []VoteView
	YesCount           int
	NoCount            int
	DrawerAnswer       string
	FinalAnswer        string
	ChallengerName     string
	ChallengeIn        int
	ChallengeTotal     int
	ChallengeWaitingOn []string
	Guess              *GuessPanel // nil unless "guess who wrote it" is on
}

// GuessPanel is the per-viewer "who wrote it?" state of an open round. It
// never carries the card's author except as the viewer's own IsAuthor flag.
type GuessPanel struct {
	IsAuthor   bool
	CanGuess   bool
	MyGuessID  int64
	Candidates []guessCandidate
	Guessed    int
}

// GuessResult is one player's guess in a completed round.
type GuessResult struct {
	UserID      int64
	Name        string
	GuessedID   int64
	GuessedName string
	Correct     bool
}

// RoundSummaryView represents a completed round, used for both the live
// discussion screen (with timer fields populated) and the round history sheet.
type RoundSummaryView struct {
	ID                  int64
	Number              int
	Classic             bool
	Card                CardView
	DrawerID            int64
	DrawerName          string
	DrawerAnswer        string
	FinalAnswer         string
	YesVotes            []VoteView
	NoVotes             []VoteView
	YesCount            int
	NoCount             int
	Challenged          bool
	ChallengerID        int64
	ChallengerName      string
	ChallengeOverturned bool
	ChallengeAgree      int
	ChallengeFlip       int
	GuessShown          bool
	AuthorID            int64
	AuthorName          string
	Guesses             []GuessResult
	CompletedAt         string
	DiscussionEndsAt    string
	DiscussionEndsUnix  int64
	DiscussionTotalSecs int
	DiscussionRemText   string
	ServerNowUnix       int64
}

type HistorySheetView struct {
	Classic      bool
	Rounds       []RoundSummaryView
	RoundsDesc   []RoundSummaryView
	Tab          string // "detail" or "list"
	Selected     *RoundSummaryView
	PrevRoundID  int64
	PrevRoundNum int
	NextRoundID  int64
	NextRoundNum int
}

type MeView struct {
	ID             int64
	IsAdmin        bool
	IsHost         bool
	Joined         bool
	Eligible       bool
	IsDrawer       bool
	InitialVote    string
	ChallengeVote  string
	AnswerSet      bool
	CanDraw        bool
	CanChallenge   bool
	CanEnd         bool
	CanToggleAI    bool
	CanToggleGuess bool
}

type GameView struct {
	NoGame            bool
	GameOver          bool
	Error             string
	ID                int64
	Mode              string
	PointGoal         int
	Status            string
	Classic           bool
	IncludeAI         bool
	GuessAuthor       bool
	BestGuessers      []string
	BestGuessCount    int
	DiscussionSeconds int
	DiscussionOptions []TimerOption
	Me                MeView
	Players           []PlayerView
	Round             *RoundView
	Discussion        *RoundSummaryView
	CompletedRounds   int
	LatestCompletedID int64
	NextDrawerName    string
	Winners           []string
	StateVersion      string
}

func formatCountdown(secs int) string {
	if secs < 0 {
		secs = 0
	}
	return fmt.Sprintf("%d:%02d", secs/60, secs%60)
}

// loadCompletedRounds returns every completed round of game g in chronological
// order (Round 1, Round 2, ...) along with its card, votes, and challenge outcome.
func loadCompletedRounds(q querier, g *gameRow) ([]RoundSummaryView, error) {
	rows, err := q.Query(
		`SELECT r.id, c.circumstance, c.question, c.source,
		        r.drawer_id, du.username,
		        r.drawer_answer, r.final_answer,
		        r.challenger_id, cu.username,
		        r.completed_at, r.discussion_ends_at
		 FROM rounds r
		 JOIN cards c ON c.id = r.card_id
		 JOIN users du ON du.id = r.drawer_id
		 LEFT JOIN users cu ON cu.id = r.challenger_id
		 WHERE r.game_id = ? AND r.status = 'complete'
		 ORDER BY r.id ASC`, g.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RoundSummaryView
	byID := map[int64]int{}
	classic := g.Mode == "classic"
	for rows.Next() {
		var s RoundSummaryView
		var drawerAns, finalAns, chalName, compAt, discEnd sql.NullString
		var chalID sql.NullInt64
		if err := rows.Scan(
			&s.ID, &s.Card.Circumstance, &s.Card.Question, &s.Card.Source,
			&s.DrawerID, &s.DrawerName,
			&drawerAns, &finalAns,
			&chalID, &chalName,
			&compAt, &discEnd,
		); err != nil {
			return nil, err
		}
		s.Number = len(out) + 1
		s.Classic = classic
		s.DrawerAnswer = drawerAns.String
		s.FinalAnswer = finalAns.String
		if chalID.Valid && chalID.Int64 != 0 {
			s.Challenged = true
			s.ChallengerID = chalID.Int64
			s.ChallengerName = chalName.String
			if s.DrawerAnswer != "" && s.FinalAnswer != "" && s.FinalAnswer != s.DrawerAnswer {
				s.ChallengeOverturned = true
			}
		}
		s.CompletedAt = compAt.String
		s.DiscussionEndsAt = discEnd.String
		byID[s.ID] = len(out)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	vrows, err := q.Query(
		`SELECT v.round_id, v.user_id, u.username, v.phase, v.vote
		 FROM votes v
		 JOIN rounds r ON r.id = v.round_id
		 JOIN users u ON u.id = v.user_id
		 LEFT JOIN game_players gp ON gp.game_id = r.game_id AND gp.user_id = v.user_id
		 WHERE r.game_id = ? AND r.status = 'complete'
		 ORDER BY r.id ASC, COALESCE(gp.turn_order, 999999) ASC, u.id ASC`, g.ID)
	if err != nil {
		return nil, err
	}
	defer vrows.Close()

	for vrows.Next() {
		var roundID, userID int64
		var name, phase, vote string
		if err := vrows.Scan(&roundID, &userID, &name, &phase, &vote); err != nil {
			return nil, err
		}
		idx, ok := byID[roundID]
		if !ok {
			continue
		}
		s := &out[idx]
		if phase == "initial" {
			cmp := s.FinalAnswer
			if cmp == "" {
				cmp = s.DrawerAnswer
			}
			vv := VoteView{
				UserID:  userID,
				Name:    name,
				Vote:    vote,
				Correct: s.Classic && vote == cmp,
			}
			if vote == "yes" {
				s.YesVotes = append(s.YesVotes, vv)
				s.YesCount++
			} else {
				s.NoVotes = append(s.NoVotes, vv)
				s.NoCount++
			}
		} else if phase == "challenge" && s.Challenged {
			if vote == s.DrawerAnswer {
				s.ChallengeAgree++
			} else {
				s.ChallengeFlip++
			}
		}
	}
	if err := vrows.Err(); err != nil {
		return nil, err
	}
	vrows.Close() // single-connection pool: free it before the next query
	ptrs := make([]*RoundSummaryView, len(out))
	for i := range out {
		ptrs[i] = &out[i]
	}
	if err := fillAuthorReveal(q, g, ptrs); err != nil {
		return nil, err
	}
	return out, nil
}

// fillAuthorReveal adds the card author and each player's author guess to
// completed rounds. Only completed rounds are ever passed in, so the author is
// never exposed earlier. Guesses sort correct-first, then by guesser name.
func fillAuthorReveal(q querier, g *gameRow, rounds []*RoundSummaryView) error {
	if len(rounds) == 0 {
		return nil
	}
	byID := map[int64]*RoundSummaryView{}
	for _, s := range rounds {
		byID[s.ID] = s
	}
	// A single round (buildRoundSummary) filters to it; otherwise load the game's.
	roundCond, roundArgs := "", []any{g.ID}
	if len(rounds) == 1 {
		roundCond, roundArgs = " AND r.id = ?", append(roundArgs, rounds[0].ID)
	}
	arows, err := q.Query(
		`SELECT r.id, c.author_id, u.username
		 FROM rounds r JOIN cards c ON c.id = r.card_id JOIN users u ON u.id = c.author_id
		 WHERE r.game_id = ? AND r.status = 'complete'`+roundCond, roundArgs...)
	if err != nil {
		return err
	}
	defer arows.Close()
	for arows.Next() {
		var rid, aid int64
		var name string
		if err := arows.Scan(&rid, &aid, &name); err != nil {
			return err
		}
		if s := byID[rid]; s != nil {
			s.AuthorID, s.AuthorName = aid, name
		}
	}
	if err := arows.Err(); err != nil {
		return err
	}
	arows.Close()

	grows, err := q.Query(
		`SELECT ag.round_id, ag.user_id, gu.username, ag.guessed_author_id, au.username
		 FROM author_guesses ag
		 JOIN rounds r ON r.id = ag.round_id
		 JOIN users gu ON gu.id = ag.user_id
		 JOIN users au ON au.id = ag.guessed_author_id
		 WHERE r.game_id = ? AND r.status = 'complete'`+roundCond+`
		 ORDER BY ag.round_id, gu.username COLLATE NOCASE`, roundArgs...)
	if err != nil {
		return err
	}
	defer grows.Close()
	for grows.Next() {
		var rid int64
		var gr GuessResult
		if err := grows.Scan(&rid, &gr.UserID, &gr.Name, &gr.GuessedID, &gr.GuessedName); err != nil {
			return err
		}
		if s := byID[rid]; s != nil {
			gr.Correct = gr.GuessedID == s.AuthorID
			s.Guesses = append(s.Guesses, gr)
		}
	}
	if err := grows.Err(); err != nil {
		return err
	}
	for _, s := range rounds {
		sort.SliceStable(s.Guesses, func(i, j int) bool { return s.Guesses[i].Correct && !s.Guesses[j].Correct })
		s.GuessShown = g.GuessAuthor || len(s.Guesses) > 0
	}
	return nil
}

// loadBestGuessers returns the players with the most correct author guesses
// across the game's completed rounds (ties included), and that count. Empty
// when nobody guessed right.
func loadBestGuessers(q querier, gameID int64) ([]string, int, error) {
	rows, err := q.Query(
		`SELECT u.username, COUNT(*) AS n
		 FROM author_guesses ag
		 JOIN rounds r ON r.id = ag.round_id
		 JOIN cards c ON c.id = r.card_id
		 JOIN users u ON u.id = ag.user_id
		 WHERE r.game_id = ? AND r.status = 'complete' AND ag.guessed_author_id = c.author_id
		 GROUP BY u.id
		 ORDER BY n DESC, u.username COLLATE NOCASE`, gameID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var names []string
	best := 0
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, 0, err
		}
		if len(names) > 0 && n < best {
			break
		}
		best = n
		names = append(names, name)
	}
	return names, best, rows.Err()
}

func buildHistorySheet(db *sql.DB, tab string, selectedRoundID int64) (*HistorySheetView, error) {
	g, err := activeGame(db)
	if err != nil {
		return nil, err
	}
	if g == nil {
		g, err = latestFinishedGame(db)
		if err != nil || g == nil {
			return &HistorySheetView{Tab: "detail"}, err
		}
	}
	rounds, err := loadCompletedRounds(db, g)
	if err != nil {
		return nil, err
	}
	h := &HistorySheetView{
		Classic: g.Mode == "classic",
		Rounds:  rounds,
		Tab:     "detail",
	}
	if tab == "list" {
		h.Tab = "list"
	}
	for i := len(rounds) - 1; i >= 0; i-- {
		h.RoundsDesc = append(h.RoundsDesc, rounds[i])
	}
	if len(rounds) == 0 {
		return h, nil
	}
	selIdx := len(rounds) - 1
	if selectedRoundID > 0 {
		for i := range rounds {
			if rounds[i].ID == selectedRoundID {
				selIdx = i
				break
			}
		}
	}
	h.Selected = &rounds[selIdx]
	if selIdx > 0 {
		h.PrevRoundID = rounds[selIdx-1].ID
		h.PrevRoundNum = rounds[selIdx-1].Number
	}
	if selIdx+1 < len(rounds) {
		h.NextRoundID = rounds[selIdx+1].ID
		h.NextRoundNum = rounds[selIdx+1].Number
	}
	return h, nil
}

// buildRoundSummary loads a single completed round for the discussion screen.
func buildRoundSummary(q querier, g *gameRow, rd *roundRow, roundNum int, now time.Time) (*RoundSummaryView, error) {
	s := &RoundSummaryView{
		ID:               rd.ID,
		Number:           roundNum,
		Classic:          g.Mode == "classic",
		DrawerID:         rd.DrawerID,
		DrawerAnswer:     rd.DrawerAnswer,
		FinalAnswer:      rd.FinalAnswer,
		CompletedAt:      rd.CompletedAt,
		DiscussionEndsAt: rd.DiscussionEndsAt,
	}
	if err := q.QueryRow(`SELECT circumstance, question, source FROM cards WHERE id = ?`, rd.CardID).
		Scan(&s.Card.Circumstance, &s.Card.Question, &s.Card.Source); err != nil {
		return nil, err
	}
	if err := q.QueryRow(`SELECT username FROM users WHERE id = ?`, rd.DrawerID).Scan(&s.DrawerName); err != nil {
		return nil, err
	}
	if rd.ChallengerID != 0 {
		s.Challenged = true
		s.ChallengerID = rd.ChallengerID
		if err := q.QueryRow(`SELECT username FROM users WHERE id = ?`, rd.ChallengerID).Scan(&s.ChallengerName); err != nil {
			return nil, err
		}
		if s.DrawerAnswer != "" && s.FinalAnswer != "" && s.FinalAnswer != s.DrawerAnswer {
			s.ChallengeOverturned = true
		}
	}

	vrows, err := q.Query(
		`SELECT v.user_id, u.username, v.phase, v.vote
		 FROM votes v
		 JOIN users u ON u.id = v.user_id
		 LEFT JOIN game_players gp ON gp.game_id = ? AND gp.user_id = v.user_id
		 WHERE v.round_id = ?
		 ORDER BY COALESCE(gp.turn_order, 999999) ASC, u.id ASC`, g.ID, rd.ID)
	if err != nil {
		return nil, err
	}
	defer vrows.Close()

	cmp := s.FinalAnswer
	if cmp == "" {
		cmp = s.DrawerAnswer
	}
	for vrows.Next() {
		var uid int64
		var name, phase, vote string
		if err := vrows.Scan(&uid, &name, &phase, &vote); err != nil {
			return nil, err
		}
		if phase == "initial" {
			vv := VoteView{
				UserID:  uid,
				Name:    name,
				Vote:    vote,
				Correct: s.Classic && vote == cmp,
			}
			if vote == "yes" {
				s.YesVotes = append(s.YesVotes, vv)
				s.YesCount++
			} else {
				s.NoVotes = append(s.NoVotes, vv)
				s.NoCount++
			}
		} else if phase == "challenge" && s.Challenged {
			if vote == s.DrawerAnswer {
				s.ChallengeAgree++
			} else {
				s.ChallengeFlip++
			}
		}
	}
	if err := vrows.Err(); err != nil {
		return nil, err
	}
	vrows.Close() // single-connection pool: free it before the next query
	if err := fillAuthorReveal(q, g, []*RoundSummaryView{s}); err != nil {
		return nil, err
	}

	if endsAt, ok := parseTime(rd.DiscussionEndsAt); ok {
		s.DiscussionEndsUnix = endsAt.Unix()
		s.ServerNowUnix = now.UTC().Unix()
		rem := int(math.Ceil(endsAt.Sub(now.UTC()).Seconds()))
		if rem < 0 {
			rem = 0
		}
		s.DiscussionRemText = formatCountdown(rem)
		total := g.DiscussionSeconds
		if compAt, ok := parseTime(rd.CompletedAt); ok && endsAt.After(compAt) {
			total = int(math.Round(endsAt.Sub(compAt).Seconds()))
		}
		if total < 1 {
			total = 1
		}
		s.DiscussionTotalSecs = total
	}
	return s, nil
}

// computeStateVersion hashes every piece of view state that affects the
// #game-state fragment so GET /game/state can return 204 when unchanged.
// Wall-clock display fields (ServerNowUnix, DiscussionRemText) are intentionally
// excluded so ticking countdowns do not trigger re-renders.
func computeStateVersion(v *GameView) string {
	h := sha256.New()
	fmt.Fprintf(h, "ng=%t;go=%t;err=%s;id=%d;m=%s;pg=%d;st=%s;ai=%t;ds=%d;cr=%d;lc=%d;nd=%s|",
		v.NoGame, v.GameOver, v.Error, v.ID, v.Mode, v.PointGoal, v.Status, v.IncludeAI,
		v.DiscussionSeconds, v.CompletedRounds, v.LatestCompletedID, v.NextDrawerName)
	fmt.Fprintf(h, "me=%d,%t,%t,%t,%t,%t,%s,%s,%t,%t,%t,%t,%t|",
		v.Me.ID, v.Me.IsAdmin, v.Me.IsHost, v.Me.Joined, v.Me.Eligible, v.Me.IsDrawer,
		v.Me.InitialVote, v.Me.ChallengeVote, v.Me.AnswerSet,
		v.Me.CanDraw, v.Me.CanChallenge, v.Me.CanEnd, v.Me.CanToggleAI)
	for _, p := range v.Players {
		fmt.Fprintf(h, "p=%d,%s,%d,%t,%t,%t;", p.ID, p.Name, p.Points, p.IsNext, p.IsWinner, p.IsMe)
	}
	for _, w := range v.Winners {
		fmt.Fprintf(h, "w=%s;", w)
	}
	fmt.Fprintf(h, "ga=%t,%t,%d;", v.GuessAuthor, v.Me.CanToggleGuess, v.BestGuessCount)
	for _, b := range v.BestGuessers {
		fmt.Fprintf(h, "bg=%s;", b)
	}
	if r := v.Round; r != nil {
		fmt.Fprintf(h, "|r=%d,%s,%s,%d,%s,%s,%s,%d/%d,%d/%d,%s,%d/%d",
			r.ID, r.Status, r.Card.Source, r.DrawerID, r.DrawerName, r.DrawerAnswer, r.FinalAnswer,
			r.VotesIn, r.VotesTotal, r.YesCount, r.NoCount,
			r.ChallengerName, r.ChallengeIn, r.ChallengeTotal)
		for _, wn := range r.WaitingOn {
			fmt.Fprintf(h, ",wo=%s", wn)
		}
		for _, rv := range r.Revealed {
			fmt.Fprintf(h, ",rv=%d:%s:%t", rv.UserID, rv.Vote, rv.Correct)
		}
		for _, cw := range r.ChallengeWaitingOn {
			fmt.Fprintf(h, ",cw=%s", cw)
		}
		if gp := r.Guess; gp != nil {
			fmt.Fprintf(h, ",gp=%t,%t,%d,%d", gp.IsAuthor, gp.CanGuess, gp.MyGuessID, gp.Guessed)
			for _, c := range gp.Candidates {
				fmt.Fprintf(h, ",gc=%d:%s:%s", c.ID, c.Name, c.AvatarVer)
			}
		}
	}
	if d := v.Discussion; d != nil {
		fmt.Fprintf(h, "|disc=%d,%d,%s,%s,%s,%s,%d,%t,%s,%t,%d,%d",
			d.ID, d.Number, d.Card.Source, d.DrawerAnswer, d.FinalAnswer,
			d.DiscussionEndsAt, d.DiscussionTotalSecs,
			d.Challenged, d.ChallengerName, d.ChallengeOverturned,
			d.ChallengeAgree, d.ChallengeFlip)
		for _, yv := range d.YesVotes {
			fmt.Fprintf(h, ",y=%d:%s:%t", yv.UserID, yv.Name, yv.Correct)
		}
		for _, nv := range d.NoVotes {
			fmt.Fprintf(h, ",n=%d:%s:%t", nv.UserID, nv.Name, nv.Correct)
		}
		fmt.Fprintf(h, ",au=%t:%d:%s", d.GuessShown, d.AuthorID, d.AuthorName)
		for _, gr := range d.Guesses {
			fmt.Fprintf(h, ",g=%d:%d:%t", gr.UserID, gr.GuessedID, gr.Correct)
		}
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:8])
}

// buildView assembles everything the templates need. Hidden information
// (votes, the drawer's answer) is only included once the round status
// permits revealing it.
func buildView(db *sql.DB, me *web.User, errMsg string) (*GameView, error) {
	v, err := buildViewInner(db, me, errMsg)
	if err != nil {
		return nil, err
	}
	v.StateVersion = computeStateVersion(v)
	return v, nil
}

func buildViewInner(db *sql.DB, me *web.User, errMsg string) (*GameView, error) {
	v := &GameView{
		Error:             errMsg,
		DiscussionOptions: discussionOptions,
	}
	v.Me.ID = me.ID
	v.Me.IsAdmin = me.IsAdmin

	g, err := activeGame(db)
	if err != nil {
		return nil, err
	}
	if g == nil {
		g, err = latestFinishedGame(db)
		if err != nil {
			return nil, err
		}
		if g == nil {
			v.NoGame = true
			return v, nil
		}
		v.GameOver = true
	}
	v.ID, v.Mode, v.PointGoal, v.Status = g.ID, g.Mode, g.PointGoal, g.Status
	v.Classic = g.Mode == "classic"
	v.IncludeAI = g.IncludeAI
	v.GuessAuthor = g.GuessAuthor
	v.DiscussionSeconds = g.DiscussionSeconds

	if err := db.QueryRow(
		`SELECT COUNT(*), COALESCE(MAX(id), 0) FROM rounds WHERE game_id = ? AND status = 'complete'`,
		g.ID,
	).Scan(&v.CompletedRounds, &v.LatestCompletedID); err != nil {
		return nil, err
	}

	players, err := loadPlayers(db, g.ID)
	if err != nil {
		return nil, err
	}
	var myRow *playerRow
	for i := range players {
		if players[i].ID == me.ID {
			myRow = &players[i]
		}
	}
	v.Me.Joined = myRow != nil

	if v.GameOver {
		if v.Classic {
			sort.SliceStable(players, func(i, j int) bool { return players[i].Points > players[j].Points })
		}
		for _, p := range players {
			v.Players = append(v.Players, PlayerView{
				ID: p.ID, Name: p.Name, Points: p.Points,
				IsWinner: p.IsWinner, IsMe: p.ID == me.ID,
			})
			if p.IsWinner {
				v.Winners = append(v.Winners, p.Name)
			}
		}
		if v.BestGuessers, v.BestGuessCount, err = loadBestGuessers(db, g.ID); err != nil {
			return nil, err
		}
		return v, nil
	}

	v.Me.IsHost = isHost(g, me)
	v.Me.CanEnd = v.Me.IsHost
	v.Me.CanToggleAI = v.Me.IsHost
	v.Me.CanToggleGuess = v.Me.IsHost

	rd, err := latestRound(db, g.ID)
	if err != nil {
		return nil, err
	}
	open := rd != nil && rd.Status != "complete"

	var nextID int64
	if v.Classic {
		if open {
			nextID = rd.DrawerID
		} else {
			nextID = nextDrawerIDFor(players, rd)
		}
	}
	names := map[int64]string{}
	for _, p := range players {
		names[p.ID] = p.Name
		v.Players = append(v.Players, PlayerView{
			ID: p.ID, Name: p.Name, Points: p.Points,
			IsNext: v.Classic && p.ID == nextID,
			IsMe:   p.ID == me.ID,
		})
	}
	v.NextDrawerName = names[nextID]

	if !open {
		now := nowFunc().UTC()
		if discussionOpen(rd, now) {
			disc, err := buildRoundSummary(db, g, rd, v.CompletedRounds, now)
			if err != nil {
				return nil, err
			}
			v.Discussion = disc
			v.Me.CanDraw = false
			return v, nil
		}
		v.Me.CanDraw = v.Me.Joined && (!v.Classic || me.ID == nextID)
		return v, nil
	}

	rv := &RoundView{ID: rd.ID, Classic: v.Classic, Status: rd.Status, DrawerID: rd.DrawerID, DrawerName: names[rd.DrawerID]}
	if err := db.QueryRow(`SELECT circumstance, question, source FROM cards WHERE id = ?`, rd.CardID).
		Scan(&rv.Card.Circumstance, &rv.Card.Question, &rv.Card.Source); err != nil {
		return nil, err
	}

	// Players eligible for this round (joined before it was drawn).
	var eligible []playerRow
	for _, p := range players {
		if p.JoinedAt <= rd.CreatedAt {
			eligible = append(eligible, p)
		}
	}
	// Expected voters: everyone eligible; in classic, minus the drawer.
	var voters []playerRow
	for _, p := range eligible {
		if v.Classic && p.ID == rd.DrawerID {
			continue
		}
		voters = append(voters, p)
	}

	initial, challenge, err := loadVotes(db, rd.ID)
	if err != nil {
		return nil, err
	}

	v.Me.IsDrawer = me.ID == rd.DrawerID
	if myRow != nil {
		v.Me.Eligible = myRow.JoinedAt <= rd.CreatedAt
	}
	v.Me.InitialVote = initial[me.ID]
	v.Me.ChallengeVote = challenge[me.ID]
	v.Me.AnswerSet = rd.DrawerAnswer != ""

	for _, p := range voters {
		if _, ok := initial[p.ID]; ok {
			rv.VotesIn++
		} else {
			rv.WaitingOn = append(rv.WaitingOn, p.Name)
		}
	}
	rv.VotesTotal = len(voters)
	if v.Classic {
		// Count the drawer's secret answer as one of the "in" slots.
		rv.VotesTotal++
		if rd.DrawerAnswer != "" {
			rv.VotesIn++
		} else {
			rv.WaitingOn = append(rv.WaitingOn, rv.DrawerName)
		}
	}

	if rd.Status != "voting" {
		// Reveal-time data only.
		rv.DrawerAnswer = rd.DrawerAnswer
		rv.FinalAnswer = rd.FinalAnswer
		cmp := rd.FinalAnswer
		if cmp == "" {
			cmp = rd.DrawerAnswer
		}
		for _, p := range voters {
			vote, ok := initial[p.ID]
			if !ok {
				continue
			}
			vv := VoteView{
				UserID:  p.ID,
				Name:    p.Name,
				Vote:    vote,
				Correct: v.Classic && vote == cmp,
			}
			rv.Revealed = append(rv.Revealed, vv)
			if vote == "yes" {
				rv.YesVotes = append(rv.YesVotes, vv)
				rv.YesCount++
			} else {
				rv.NoVotes = append(rv.NoVotes, vv)
				rv.NoCount++
			}
		}
		rv.WaitingOn = nil
	}

	if rd.Status == "challenge" {
		rv.ChallengerName = names[rd.ChallengerID]
		rv.ChallengeTotal = len(voters)
		for _, p := range voters {
			if _, ok := challenge[p.ID]; ok {
				rv.ChallengeIn++
			} else {
				rv.ChallengeWaitingOn = append(rv.ChallengeWaitingOn, p.Name)
			}
		}
	}

	if g.GuessAuthor {
		gp := &GuessPanel{}
		var authorID int64
		if err := db.QueryRow(
			`SELECT c.author_id, (SELECT COUNT(*) FROM author_guesses WHERE round_id = ?)
			 FROM cards c WHERE c.id = ?`, rd.ID, rd.CardID,
		).Scan(&authorID, &gp.Guessed); err != nil {
			return nil, err
		}
		if v.Me.Eligible {
			gp.IsAuthor = authorID == me.ID
			gp.CanGuess = !gp.IsAuthor
		}
		if gp.CanGuess {
			if err := db.QueryRow(
				`SELECT guessed_author_id FROM author_guesses WHERE round_id = ? AND user_id = ?`, rd.ID, me.ID,
			).Scan(&gp.MyGuessID); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			if gp.Candidates, err = guessCandidates(db, g.ID, g.IncludeAI, me.ID); err != nil {
				return nil, err
			}
		}
		rv.Guess = gp
	}

	v.Me.CanChallenge = v.Classic && rd.Status == "revealed" && v.Me.Joined && !v.Me.IsDrawer
	v.Round = rv
	return v, nil
}

// ---- winners page ----

type Leader struct {
	Name string
	Wins int
}

type PastWinner struct {
	Name   string
	Points int
}

type PastGame struct {
	Date      string
	PointGoal int
	Winners   []PastWinner
}

type HallView struct {
	Leaders []Leader
	Games   []PastGame
}

func buildHall(db *sql.DB) (*HallView, error) {
	h := &HallView{}

	rows, err := db.Query(
		`SELECT u.username, COUNT(*) AS wins
		 FROM game_players gp
		 JOIN games g ON g.id = gp.game_id
		 JOIN users u ON u.id = gp.user_id
		 WHERE g.status = 'finished' AND g.mode = 'classic' AND gp.is_winner = 1
		 GROUP BY u.id
		 ORDER BY wins DESC, u.username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var l Leader
		if err := rows.Scan(&l.Name, &l.Wins); err != nil {
			return nil, err
		}
		h.Leaders = append(h.Leaders, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	grows, err := db.Query(
		`SELECT g.id, COALESCE(g.point_goal, 0), COALESCE(g.finished_at, g.created_at), u.username, gp.points
		 FROM games g
		 LEFT JOIN game_players gp ON gp.game_id = g.id AND gp.is_winner = 1
		 LEFT JOIN users u ON u.id = gp.user_id
		 WHERE g.status = 'finished' AND g.mode = 'classic'
		 ORDER BY g.id DESC, u.username`)
	if err != nil {
		return nil, err
	}
	defer grows.Close()
	idx := -1
	var curID int64 = -1
	for grows.Next() {
		var gid int64
		var goal int
		var date string
		var name sql.NullString
		var pts sql.NullInt64
		if err := grows.Scan(&gid, &goal, &date, &name, &pts); err != nil {
			return nil, err
		}
		if gid != curID {
			if len(date) > 10 {
				date = date[:10]
			}
			h.Games = append(h.Games, PastGame{Date: date, PointGoal: goal})
			idx = len(h.Games) - 1
			curID = gid
		}
		if name.Valid {
			h.Games[idx].Winners = append(h.Games[idx].Winners, PastWinner{
				Name: name.String, Points: int(pts.Int64),
			})
		}
	}
	return h, grows.Err()
}
