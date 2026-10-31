package game

import (
	"database/sql"
	"errors"
	"sort"

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
	ID        int64
	Mode      string
	PointGoal int
	Status    string
	CreatedBy int64
}

type roundRow struct {
	ID           int64
	CardID       int64
	DrawerID     int64
	DrawerAnswer string // "" until answered
	Status       string
	FinalAnswer  string // "" until finalized
	ChallengerID int64  // 0 until challenged
	CreatedAt    string
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
	err := row.Scan(&g.ID, &g.Mode, &goal, &g.Status, &g.CreatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	g.PointGoal = int(goal.Int64)
	return g, nil
}

func activeGame(q querier) (*gameRow, error) {
	return scanGame(q.QueryRow(
		`SELECT id, mode, point_goal, status, created_by
		 FROM games WHERE status = 'active' ORDER BY id DESC LIMIT 1`))
}

func latestFinishedGame(q querier) (*gameRow, error) {
	return scanGame(q.QueryRow(
		`SELECT id, mode, point_goal, status, created_by
		 FROM games WHERE status = 'finished' ORDER BY id DESC LIMIT 1`))
}

func scanRound(row *sql.Row) (*roundRow, error) {
	rd := &roundRow{}
	var ans, fin sql.NullString
	var chal sql.NullInt64
	err := row.Scan(&rd.ID, &rd.CardID, &rd.DrawerID, &ans, &rd.Status, &fin, &chal, &rd.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rd.DrawerAnswer = ans.String
	rd.FinalAnswer = fin.String
	rd.ChallengerID = chal.Int64
	return rd, nil
}

func latestRound(q querier, gameID int64) (*roundRow, error) {
	return scanRound(q.QueryRow(
		`SELECT id, card_id, drawer_id, drawer_answer, status, final_answer, challenger_id, created_at
		 FROM rounds WHERE game_id = ? ORDER BY id DESC LIMIT 1`, gameID))
}

func roundByID(q querier, id int64) (*roundRow, error) {
	return scanRound(q.QueryRow(
		`SELECT id, card_id, drawer_id, drawer_answer, status, final_answer, challenger_id, created_at
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
	Name    string
	Vote    string
	Correct bool
}

type RoundView struct {
	ID                 int64
	Card               CardView
	DrawerName         string
	Status             string
	VotesIn            int
	VotesTotal         int
	WaitingOn          []string
	Revealed           []VoteView
	YesCount           int
	NoCount            int
	DrawerAnswer       string
	FinalAnswer        string
	ChallengerName     string
	ChallengeIn        int
	ChallengeTotal     int
	ChallengeWaitingOn []string
}

type MeView struct {
	ID            int64
	IsAdmin       bool
	Joined        bool
	Eligible      bool
	IsDrawer      bool
	InitialVote   string
	ChallengeVote string
	AnswerSet     bool
	CanDraw       bool
	CanChallenge  bool
	CanEnd        bool
}

type GameView struct {
	NoGame         bool
	GameOver       bool
	Error          string
	ID             int64
	Mode           string
	PointGoal      int
	Status         string
	Classic        bool
	Me             MeView
	Players        []PlayerView
	Round          *RoundView
	NextDrawerName string
	Winners        []string
}

// buildView assembles everything the templates need. Hidden information
// (votes, the drawer's answer) is only included once the round status
// permits revealing it.
func buildView(db *sql.DB, me *web.User, errMsg string) (*GameView, error) {
	v := &GameView{Error: errMsg}
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
		return v, nil
	}

	v.Me.CanEnd = me.ID == g.CreatedBy || me.IsAdmin

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
		v.Me.CanDraw = v.Me.Joined && (!v.Classic || me.ID == nextID)
		return v, nil
	}

	rv := &RoundView{ID: rd.ID, Status: rd.Status, DrawerName: names[rd.DrawerID]}
	if err := db.QueryRow(`SELECT circumstance, question FROM cards WHERE id = ?`, rd.CardID).
		Scan(&rv.Card.Circumstance, &rv.Card.Question); err != nil {
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
			rv.Revealed = append(rv.Revealed, VoteView{
				Name: p.Name, Vote: vote,
				Correct: v.Classic && vote == cmp,
			})
			if vote == "yes" {
				rv.YesCount++
			} else {
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
