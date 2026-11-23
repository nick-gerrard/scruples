package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxBodyBytes     = 1 << 20
	maxBatchOps      = 200
	defaultListLimit = 500
	maxListLimit     = 1000
)

// Card is the JSON representation of a dilemma card.
type Card struct {
	ID           int64   `json:"id"`
	Circumstance string  `json:"circumstance"`
	Question     string  `json:"question"`
	Source       string  `json:"source"`
	Author       string  `json:"author"`
	CreatedAt    string  `json:"created_at"`
	ArchivedAt   *string `json:"archived_at"`
}

type dryRunCardResponse struct {
	Card
	DryRun bool `json:"dry_run"`
}

type listResponse struct {
	Cards []Card `json:"cards"`
	Count int    `json:"count"`
}

type createRequest struct {
	Circumstance string `json:"circumstance"`
	Question     string `json:"question"`
	Source       string `json:"source"`
	Author       string `json:"author"`
}

type patchRequest struct {
	Circumstance *string `json:"circumstance"`
	Question     *string `json:"question"`
	Source       *string `json:"source"`
}

type batchOp struct {
	Op           string  `json:"op"`
	ID           int64   `json:"id"`
	Circumstance *string `json:"circumstance"`
	Question     *string `json:"question"`
	Source       *string `json:"source"`
	Author       *string `json:"author"`
}

type batchRequest struct {
	Ops []batchOp `json:"ops"`
}

type batchResponse struct {
	DryRun  bool   `json:"dry_run,omitempty"`
	Results []Card `json:"results"`
}

type auditEntry struct {
	Method     string
	Path       string
	CardID     *int64
	DryRun     bool
	BeforeJSON *string
	AfterJSON  *string
}

type querier interface {
	QueryRow(query string, args ...any) *sql.Row
	Exec(query string, args ...any) (sql.Result, error)
}

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func badRequest(msg string) error { return &httpError{status: http.StatusBadRequest, msg: msg} }
func notFound(msg string) error   { return &httpError{status: http.StatusNotFound, msg: msg} }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("api: encode response: %v", err)
	}
}

func writeCardResult(w http.ResponseWriter, status int, card Card, dry bool) {
	if dry {
		writeJSON(w, status, dryRunCardResponse{Card: card, DryRun: true})
		return
	}
	writeJSON(w, status, card)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func handleErr(w http.ResponseWriter, err error) {
	var he *httpError
	if errors.As(err, &he) {
		writeError(w, he.status, he.msg)
		return
	}
	log.Printf("api: %v", err)
	writeError(w, http.StatusInternalServerError, "internal server error")
}

func requireBearer(apiKey string, h http.HandlerFunc) http.HandlerFunc {
	keyHash := sha256.Sum256([]byte(apiKey))
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		tokenHash := sha256.Sum256([]byte(token))
		if !ok || subtle.ConstantTimeCompare(tokenHash[:], keyHash[:]) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		h(w, r)
	}
}

func isDryRun(r *http.Request) bool {
	v := r.URL.Query().Get("dry_run")
	return v == "1" || strings.EqualFold(v, "true")
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return &httpError{status: http.StatusRequestEntityTooLarge, msg: "request body too large"}
		}
		return badRequest("invalid JSON body")
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return badRequest("unexpected trailing data after JSON body")
	}
	return nil
}

func validateCardFields(circumstance, question, source string) error {
	if circumstance == "" {
		return badRequest("circumstance must not be empty")
	}
	if utf8.RuneCountInString(circumstance) > 2000 {
		return badRequest("circumstance must be 2000 characters or fewer")
	}
	if question == "" {
		return badRequest("question must not be empty")
	}
	if utf8.RuneCountInString(question) > 300 {
		return badRequest("question must be 300 characters or fewer")
	}
	if source != "human" && source != "ai" {
		return badRequest("source must be 'human' or 'ai'")
	}
	return nil
}

func loadCard(q querier, id int64) (Card, error) {
	var c Card
	var archived sql.NullString
	err := q.QueryRow(`
		SELECT c.id, c.circumstance, c.question, c.source, u.username, c.created_at, c.archived_at
		FROM cards c
		JOIN users u ON u.id = c.author_id
		WHERE c.id = ?`, id,
	).Scan(&c.ID, &c.Circumstance, &c.Question, &c.Source, &c.Author, &c.CreatedAt, &archived)
	if errors.Is(err, sql.ErrNoRows) {
		return Card{}, notFound("card not found")
	}
	if err != nil {
		return Card{}, err
	}
	if archived.Valid {
		s := archived.String
		c.ArchivedAt = &s
	}
	return c, nil
}

func marshalPtr(v any) (*string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	s := string(b)
	return &s, nil
}

func insertAudit(q querier, a auditEntry) error {
	dry := 0
	if a.DryRun {
		dry = 1
	}
	_, err := q.Exec(`
		INSERT INTO api_audit (method, path, card_id, dry_run, before_json, after_json)
		VALUES (?, ?, ?, ?, ?, ?)`,
		a.Method, a.Path, a.CardID, dry, a.BeforeJSON, a.AfterJSON,
	)
	return err
}

func applyCreate(tx *sql.Tx, req createRequest) (Card, auditEntry, error) {
	circumstance := strings.TrimSpace(req.Circumstance)
	question := strings.TrimSpace(req.Question)
	source := strings.TrimSpace(req.Source)
	if source == "" {
		source = "ai"
	}
	author := strings.TrimSpace(req.Author)
	if author == "" {
		author = "Claude"
	}
	if err := validateCardFields(circumstance, question, source); err != nil {
		return Card{}, auditEntry{}, err
	}

	var authorID int64
	var isBot bool
	err := tx.QueryRow(`SELECT id, is_bot FROM users WHERE username = ?`, author).Scan(&authorID, &isBot)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !isBot) {
		return Card{}, auditEntry{}, badRequest("author must be a bot account")
	}
	if err != nil {
		return Card{}, auditEntry{}, err
	}

	res, err := tx.Exec(
		`INSERT INTO cards (author_id, circumstance, question, source) VALUES (?, ?, ?, ?)`,
		authorID, circumstance, question, source,
	)
	if err != nil {
		return Card{}, auditEntry{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Card{}, auditEntry{}, err
	}
	card, err := loadCard(tx, id)
	if err != nil {
		return Card{}, auditEntry{}, err
	}
	afterJSON, err := marshalPtr(card)
	if err != nil {
		return Card{}, auditEntry{}, err
	}
	cardID := card.ID
	return card, auditEntry{CardID: &cardID, AfterJSON: afterJSON}, nil
}

func applyPatch(tx *sql.Tx, id int64, req patchRequest) (Card, auditEntry, error) {
	if req.Circumstance == nil && req.Question == nil && req.Source == nil {
		return Card{}, auditEntry{}, badRequest("at least one of circumstance, question, or source is required")
	}
	before, err := loadCard(tx, id)
	if err != nil {
		return Card{}, auditEntry{}, err
	}
	circumstance := before.Circumstance
	question := before.Question
	source := before.Source
	if req.Circumstance != nil {
		circumstance = strings.TrimSpace(*req.Circumstance)
	}
	if req.Question != nil {
		question = strings.TrimSpace(*req.Question)
	}
	if req.Source != nil {
		source = strings.TrimSpace(*req.Source)
	}
	if err := validateCardFields(circumstance, question, source); err != nil {
		return Card{}, auditEntry{}, err
	}
	if _, err := tx.Exec(
		`UPDATE cards SET circumstance = ?, question = ?, source = ? WHERE id = ?`,
		circumstance, question, source, id,
	); err != nil {
		return Card{}, auditEntry{}, err
	}
	after, err := loadCard(tx, id)
	if err != nil {
		return Card{}, auditEntry{}, err
	}
	beforeJSON, err := marshalPtr(before)
	if err != nil {
		return Card{}, auditEntry{}, err
	}
	afterJSON, err := marshalPtr(after)
	if err != nil {
		return Card{}, auditEntry{}, err
	}
	cardID := after.ID
	return after, auditEntry{CardID: &cardID, BeforeJSON: beforeJSON, AfterJSON: afterJSON}, nil
}

func applyArchive(tx *sql.Tx, id int64) (Card, auditEntry, error) {
	before, err := loadCard(tx, id)
	if err != nil {
		return Card{}, auditEntry{}, err
	}
	if _, err := tx.Exec(
		`UPDATE cards SET archived_at = COALESCE(archived_at, datetime('now')) WHERE id = ?`,
		id,
	); err != nil {
		return Card{}, auditEntry{}, err
	}
	after, err := loadCard(tx, id)
	if err != nil {
		return Card{}, auditEntry{}, err
	}
	beforeJSON, err := marshalPtr(before)
	if err != nil {
		return Card{}, auditEntry{}, err
	}
	afterJSON, err := marshalPtr(after)
	if err != nil {
		return Card{}, auditEntry{}, err
	}
	cardID := after.ID
	return after, auditEntry{CardID: &cardID, BeforeJSON: beforeJSON, AfterJSON: afterJSON}, nil
}

func applyRestore(tx *sql.Tx, id int64) (Card, auditEntry, error) {
	before, err := loadCard(tx, id)
	if err != nil {
		return Card{}, auditEntry{}, err
	}
	if _, err := tx.Exec(`UPDATE cards SET archived_at = NULL WHERE id = ?`, id); err != nil {
		return Card{}, auditEntry{}, err
	}
	after, err := loadCard(tx, id)
	if err != nil {
		return Card{}, auditEntry{}, err
	}
	beforeJSON, err := marshalPtr(before)
	if err != nil {
		return Card{}, auditEntry{}, err
	}
	afterJSON, err := marshalPtr(after)
	if err != nil {
		return Card{}, auditEntry{}, err
	}
	cardID := after.ID
	return after, auditEntry{CardID: &cardID, BeforeJSON: beforeJSON, AfterJSON: afterJSON}, nil
}

func runWrite(db *sql.DB, r *http.Request, fn func(tx *sql.Tx) ([]auditEntry, error)) error {
	dry := isDryRun(r)
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	rolledBack := false
	defer func() {
		if !rolledBack {
			_ = tx.Rollback()
		}
	}()

	audits, err := fn(tx)
	if err != nil {
		return err
	}
	for i := range audits {
		audits[i].Method = r.Method
		audits[i].Path = r.URL.Path
		audits[i].DryRun = dry
	}

	if dry {
		if err := tx.Rollback(); err != nil {
			return err
		}
		rolledBack = true
		for _, a := range audits {
			if err := insertAudit(db, a); err != nil {
				return err
			}
		}
		return nil
	}

	for _, a := range audits {
		if err := insertAudit(tx, a); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	rolledBack = true
	return nil
}

func parseID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, notFound("card not found")
	}
	return id, nil
}

// Routes registers the JSON card API routes guarded by the Bearer API key.
// If apiKey is empty, nothing is registered.
func Routes(mux *http.ServeMux, db *sql.DB, apiKey string) {
	if apiKey == "" {
		return
	}
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return requireBearer(apiKey, h)
	}

	mux.HandleFunc("GET /api/cards", auth(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var conds []string
		var args []any

		if src := q.Get("source"); src != "" {
			if src != "human" && src != "ai" {
				writeError(w, http.StatusBadRequest, "source must be 'human' or 'ai'")
				return
			}
			conds = append(conds, "c.source = ?")
			args = append(args, src)
		}

		archived := q.Get("archived")
		if archived == "" {
			archived = "0"
		}
		switch archived {
		case "0":
			conds = append(conds, "c.archived_at IS NULL")
		case "1":
			conds = append(conds, "c.archived_at IS NOT NULL")
		case "all":
		default:
			writeError(w, http.StatusBadRequest, "archived must be '0', '1', or 'all'")
			return
		}

		if author := q.Get("author"); author != "" {
			conds = append(conds, "u.username = ?")
			args = append(args, author)
		}

		if sub := q.Get("q"); sub != "" {
			conds = append(conds, "(instr(lower(c.circumstance), lower(?)) > 0 OR instr(lower(c.question), lower(?)) > 0)")
			args = append(args, sub, sub)
		}

		limit := defaultListLimit
		if raw := q.Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 {
				writeError(w, http.StatusBadRequest, "limit must be a positive integer")
				return
			}
			if n > maxListLimit {
				n = maxListLimit
			}
			limit = n
		}

		offset := 0
		if raw := q.Get("offset"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 {
				writeError(w, http.StatusBadRequest, "offset must be a non-negative integer")
				return
			}
			offset = n
		}

		query := `
			SELECT c.id, c.circumstance, c.question, c.source, u.username, c.created_at, c.archived_at
			FROM cards c
			JOIN users u ON u.id = c.author_id`
		if len(conds) > 0 {
			query += " WHERE " + strings.Join(conds, " AND ")
		}
		query += " ORDER BY c.id DESC LIMIT ? OFFSET ?"
		args = append(args, limit, offset)

		rows, err := db.Query(query, args...)
		if err != nil {
			handleErr(w, err)
			return
		}
		defer rows.Close()

		cards := []Card{}
		for rows.Next() {
			var c Card
			var arch sql.NullString
			if err := rows.Scan(&c.ID, &c.Circumstance, &c.Question, &c.Source, &c.Author, &c.CreatedAt, &arch); err != nil {
				handleErr(w, err)
				return
			}
			if arch.Valid {
				s := arch.String
				c.ArchivedAt = &s
			}
			cards = append(cards, c)
		}
		if err := rows.Err(); err != nil {
			handleErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, listResponse{Cards: cards, Count: len(cards)})
	}))

	mux.HandleFunc("GET /api/cards/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		id, err := parseID(r)
		if err != nil {
			handleErr(w, err)
			return
		}
		card, err := loadCard(db, id)
		if err != nil {
			handleErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, card)
	}))

	mux.HandleFunc("POST /api/cards", auth(func(w http.ResponseWriter, r *http.Request) {
		var req createRequest
		if err := decodeJSON(w, r, &req); err != nil {
			handleErr(w, err)
			return
		}
		var created Card
		err := runWrite(db, r, func(tx *sql.Tx) ([]auditEntry, error) {
			c, audit, err := applyCreate(tx, req)
			if err != nil {
				return nil, err
			}
			created = c
			return []auditEntry{audit}, nil
		})
		if err != nil {
			handleErr(w, err)
			return
		}
		writeCardResult(w, http.StatusCreated, created, isDryRun(r))
	}))

	mux.HandleFunc("PATCH /api/cards/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		id, err := parseID(r)
		if err != nil {
			handleErr(w, err)
			return
		}
		var req patchRequest
		if err := decodeJSON(w, r, &req); err != nil {
			handleErr(w, err)
			return
		}
		var updated Card
		err = runWrite(db, r, func(tx *sql.Tx) ([]auditEntry, error) {
			c, audit, err := applyPatch(tx, id, req)
			if err != nil {
				return nil, err
			}
			updated = c
			return []auditEntry{audit}, nil
		})
		if err != nil {
			handleErr(w, err)
			return
		}
		writeCardResult(w, http.StatusOK, updated, isDryRun(r))
	}))

	mux.HandleFunc("POST /api/cards/{id}/archive", auth(func(w http.ResponseWriter, r *http.Request) {
		id, err := parseID(r)
		if err != nil {
			handleErr(w, err)
			return
		}
		var updated Card
		err = runWrite(db, r, func(tx *sql.Tx) ([]auditEntry, error) {
			c, audit, err := applyArchive(tx, id)
			if err != nil {
				return nil, err
			}
			updated = c
			return []auditEntry{audit}, nil
		})
		if err != nil {
			handleErr(w, err)
			return
		}
		writeCardResult(w, http.StatusOK, updated, isDryRun(r))
	}))

	mux.HandleFunc("POST /api/cards/{id}/restore", auth(func(w http.ResponseWriter, r *http.Request) {
		id, err := parseID(r)
		if err != nil {
			handleErr(w, err)
			return
		}
		var updated Card
		err = runWrite(db, r, func(tx *sql.Tx) ([]auditEntry, error) {
			c, audit, err := applyRestore(tx, id)
			if err != nil {
				return nil, err
			}
			updated = c
			return []auditEntry{audit}, nil
		})
		if err != nil {
			handleErr(w, err)
			return
		}
		writeCardResult(w, http.StatusOK, updated, isDryRun(r))
	}))

	mux.HandleFunc("POST /api/cards/batch", auth(func(w http.ResponseWriter, r *http.Request) {
		var req batchRequest
		if err := decodeJSON(w, r, &req); err != nil {
			handleErr(w, err)
			return
		}
		if len(req.Ops) > maxBatchOps {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("batch exceeds maximum of %d ops", maxBatchOps))
			return
		}
		results := make([]Card, 0, len(req.Ops))
		err := runWrite(db, r, func(tx *sql.Tx) ([]auditEntry, error) {
			audits := make([]auditEntry, 0, len(req.Ops))
			for i, op := range req.Ops {
				var (
					c     Card
					audit auditEntry
					err   error
				)
				switch op.Op {
				case "create":
					cr := createRequest{}
					if op.Circumstance != nil {
						cr.Circumstance = *op.Circumstance
					}
					if op.Question != nil {
						cr.Question = *op.Question
					}
					if op.Source != nil {
						cr.Source = *op.Source
					}
					if op.Author != nil {
						cr.Author = *op.Author
					}
					c, audit, err = applyCreate(tx, cr)
				case "patch":
					if op.ID <= 0 {
						err = badRequest("id is required")
					} else {
						c, audit, err = applyPatch(tx, op.ID, patchRequest{
							Circumstance: op.Circumstance,
							Question:     op.Question,
							Source:       op.Source,
						})
					}
				case "archive":
					if op.ID <= 0 {
						err = badRequest("id is required")
					} else {
						c, audit, err = applyArchive(tx, op.ID)
					}
				case "restore":
					if op.ID <= 0 {
						err = badRequest("id is required")
					} else {
						c, audit, err = applyRestore(tx, op.ID)
					}
				default:
					err = badRequest(fmt.Sprintf("unknown op %q", op.Op))
				}
				if err != nil {
					var he *httpError
					if errors.As(err, &he) {
						return nil, &httpError{status: he.status, msg: fmt.Sprintf("op %d: %s", i, he.msg)}
					}
					return nil, err
				}
				results = append(results, c)
				audits = append(audits, audit)
			}
			return audits, nil
		})
		if err != nil {
			handleErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, batchResponse{DryRun: isDryRun(r), Results: results})
	}))
}
