package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"scruples/internal/db"
)

const testAPIKey = "test-secret-api-key"

func newTestAPI(t *testing.T) (*sql.DB, *http.ServeMux) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	mux := http.NewServeMux()
	Routes(mux, database, testAPIKey)
	return database, mux
}

func doAPI(t *testing.T, mux *http.ServeMux, method, path, body, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	var buf *bytes.Buffer
	if body != "" {
		buf = bytes.NewBufferString(body)
	} else {
		buf = &bytes.Buffer{}
	}
	req := httptest.NewRequest(method, path, buf)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestRoutesEmptyKeyRegistersNothing(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "emptykey.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	mux := http.NewServeMux()
	Routes(mux, database, "")

	rec := doAPI(t, mux, http.MethodGet, "/api/cards", "", "Bearer ")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /api/cards with empty apiKey = %d, want 404", rec.Code)
	}
}

func TestAuthUnauthorized(t *testing.T) {
	_, mux := newTestAPI(t)

	cases := []struct {
		name string
		auth string
	}{
		{"missing", ""},
		{"wrong scheme", "Basic " + testAPIKey},
		{"wrong key", "Bearer wrong-key"},
		{"prefix mismatch", "Bearer " + testAPIKey + "x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doAPI(t, mux, http.MethodGet, "/api/cards", "", tc.auth)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			var resp map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode JSON: %v (body=%q)", err, rec.Body.String())
			}
			if resp["error"] != "unauthorized" {
				t.Fatalf("error = %q, want unauthorized", resp["error"])
			}
		})
	}
}

func TestCreateDefaultsValidationAndBotAuthors(t *testing.T) {
	database, mux := newTestAPI(t)
	auth := "Bearer " + testAPIKey

	if _, err := database.Exec(`INSERT INTO users (username, password_hash, is_bot) VALUES ('alice', 'hash', 0)`); err != nil {
		t.Fatalf("insert alice: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO users (username, password_hash, is_bot) VALUES ('HelperBot', '!', 1)`); err != nil {
		t.Fatalf("insert HelperBot: %v", err)
	}

	// Default source=ai and author=Claude.
	rec := doAPI(t, mux, http.MethodPost, "/api/cards",
		`{"circumstance":"You find a wallet.","question":"Do you return it?"}`, auth)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/cards status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if _, hasDry := raw["dry_run"]; hasDry {
		t.Fatalf("non-dry-run response should not contain dry_run, got %v", raw)
	}
	var created Card
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if created.ID <= 0 || created.Source != "ai" || created.Author != "Claude" || created.ArchivedAt != nil {
		t.Fatalf("unexpected created card: %+v", created)
	}

	// Another bot user is also allowed as author.
	botRec := doAPI(t, mux, http.MethodPost, "/api/cards",
		`{"circumstance":"Bot scene.","question":"Bot Q?","author":"HelperBot"}`, auth)
	if botRec.Code != http.StatusCreated {
		t.Fatalf("bot author status = %d, want 201 (body=%s)", botRec.Code, botRec.Body.String())
	}

	// Human user or unknown user must be rejected with 400 "author must be a bot account".
	for _, author := range []string{"alice", "nobody"} {
		r := doAPI(t, mux, http.MethodPost, "/api/cards",
			fmt.Sprintf(`{"circumstance":"C","question":"Q?","author":%q}`, author), auth)
		if r.Code != http.StatusBadRequest {
			t.Fatalf("author=%q status = %d, want 400", author, r.Code)
		}
		var errResp map[string]string
		_ = json.Unmarshal(r.Body.Bytes(), &errResp)
		if errResp["error"] != "author must be a bot account" {
			t.Errorf("author=%q error = %q, want 'author must be a bot account'", author, errResp["error"])
		}
	}

	// Verify audit row recorded.
	var auditCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM api_audit WHERE card_id = ? AND dry_run = 0`, created.ID).Scan(&auditCount); err != nil {
		t.Fatalf("query audit: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("audit count = %d, want 1", auditCount)
	}

	// Validation: empty fields, length limits, unknown fields, trailing data.
	badBodies := []string{
		`{"circumstance":"","question":"Q?"}`,
		`{"circumstance":"C","question":""}`,
		fmt.Sprintf(`{"circumstance":%q,"question":"Q?"}`, strings.Repeat("a", 2001)),
		fmt.Sprintf(`{"circumstance":"C","question":%q}`, strings.Repeat("q", 301)),
		`{"circumstance":"C","question":"Q?","source":"robot"}`,
		`{"circumstance":"C","question":"Q?","unknown_field":1}`,
		`{"circumstance":"C","question":"Q?"} {"extra":true}`,
	}
	for i, body := range badBodies {
		rec := doAPI(t, mux, http.MethodPost, "/api/cards", body, auth)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("bad body #%d status = %d, want 400 (body=%s)", i, rec.Code, rec.Body.String())
		}
	}

	// Oversized request body (> 1 MiB) is rejected.
	huge := `{"circumstance":"` + strings.Repeat("a", (1<<20)+10) + `","question":"Q?"}`
	hugeRec := doAPI(t, mux, http.MethodPost, "/api/cards", huge, auth)
	if hugeRec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d, want 413", hugeRec.Code)
	}
}

func TestListFiltersPaginationAndGet(t *testing.T) {
	database, mux := newTestAPI(t)
	auth := "Bearer " + testAPIKey

	res, err := database.Exec(`INSERT INTO users (username, password_hash) VALUES ('alice', 'hash')`)
	if err != nil {
		t.Fatalf("insert alice: %v", err)
	}
	aliceID, _ := res.LastInsertId()

	// Create 3 cards:
	// 1. ai / Claude / active ("wallet")
	// 2. human / alice / active ("umbrella")
	// 3. ai / Claude / archived ("wallet archived")
	c1Rec := doAPI(t, mux, http.MethodPost, "/api/cards",
		`{"circumstance":"Lost wallet on bus.","question":"Keep the cash?"}`, auth)
	var c1 Card
	_ = json.Unmarshal(c1Rec.Body.Bytes(), &c1)

	res, err = database.Exec(
		`INSERT INTO cards (author_id, circumstance, question, source) VALUES (?, 'Borrowed an umbrella.', 'Return it broken?', 'human')`,
		aliceID,
	)
	if err != nil {
		t.Fatalf("insert c2: %v", err)
	}
	c2ID, _ := res.LastInsertId()

	c3Rec := doAPI(t, mux, http.MethodPost, "/api/cards",
		`{"circumstance":"Found an old wallet.","question":"Sell it?"}`, auth)
	var c3 Card
	_ = json.Unmarshal(c3Rec.Body.Bytes(), &c3)

	archRec := doAPI(t, mux, http.MethodPost, fmt.Sprintf("/api/cards/%d/archive", c3.ID), "", auth)
	if archRec.Code != http.StatusOK {
		t.Fatalf("archive c3 status = %d", archRec.Code)
	}

	checkList := func(query string, wantIDs ...int64) {
		t.Helper()
		rec := doAPI(t, mux, http.MethodGet, "/api/cards"+query, "", auth)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/cards%s status = %d (body=%s)", query, rec.Code, rec.Body.String())
		}
		var resp listResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal %s: %v", query, err)
		}
		if resp.Count != len(resp.Cards) || resp.Count != len(wantIDs) {
			t.Fatalf("GET /api/cards%s count=%d cards=%d, want %d (%v)", query, resp.Count, len(resp.Cards), len(wantIDs), resp.Cards)
		}
		for i, wantID := range wantIDs {
			if resp.Cards[i].ID != wantID {
				t.Errorf("GET /api/cards%s card[%d].ID = %d, want %d", query, i, resp.Cards[i].ID, wantID)
			}
		}
	}

	// Results are ordered by ID DESC: c3 > c2 > c1.
	checkList("", c2ID, c1.ID)
	checkList("?archived=0", c2ID, c1.ID)
	checkList("?archived=1", c3.ID)
	checkList("?archived=all", c3.ID, c2ID, c1.ID)
	checkList("?source=human", c2ID)
	checkList("?source=ai", c1.ID)
	checkList("?source=ai&archived=all", c3.ID, c1.ID)
	checkList("?author=alice", c2ID)
	checkList("?author=Claude", c1.ID)
	checkList("?q=WALLET", c1.ID)
	checkList("?q=wallet&archived=all", c3.ID, c1.ID)
	checkList("?q=broken", c2ID)

	// Pagination: limit & offset.
	checkList("?archived=all&limit=2", c3.ID, c2ID)
	checkList("?archived=all&limit=2&offset=1", c2ID, c1.ID)
	checkList("?archived=all&limit=2&offset=2", c1.ID)
	checkList("?archived=all&limit=5000", c3.ID, c2ID, c1.ID)

	// Invalid limit / offset.
	for _, bad := range []string{"?limit=0", "?limit=-5", "?limit=abc", "?offset=-1", "?offset=xyz"} {
		r := doAPI(t, mux, http.MethodGet, "/api/cards"+bad, "", auth)
		if r.Code != http.StatusBadRequest {
			t.Errorf("GET /api/cards%s status = %d, want 400", bad, r.Code)
		}
	}

	// GET /api/cards/{id}
	getRec := doAPI(t, mux, http.MethodGet, fmt.Sprintf("/api/cards/%d", c3.ID), "", auth)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET /api/cards/%d status = %d, want 200", c3.ID, getRec.Code)
	}
	var got3 Card
	_ = json.Unmarshal(getRec.Body.Bytes(), &got3)
	if got3.ID != c3.ID || got3.ArchivedAt == nil {
		t.Fatalf("unexpected GET card: %+v", got3)
	}

	notFoundRec := doAPI(t, mux, http.MethodGet, "/api/cards/99999", "", auth)
	if notFoundRec.Code != http.StatusNotFound {
		t.Fatalf("GET /api/cards/99999 status = %d, want 404", notFoundRec.Code)
	}
}

func TestPatchArchiveRestore(t *testing.T) {
	database, mux := newTestAPI(t)
	auth := "Bearer " + testAPIKey

	rec := doAPI(t, mux, http.MethodPost, "/api/cards",
		`{"circumstance":"Original scene.","question":"Original question?"}`, auth)
	var card Card
	_ = json.Unmarshal(rec.Body.Bytes(), &card)

	// Empty patch (no recognised fields) -> 400.
	emptyPatch := doAPI(t, mux, http.MethodPatch, fmt.Sprintf("/api/cards/%d", card.ID), `{}`, auth)
	if emptyPatch.Code != http.StatusBadRequest {
		t.Fatalf("empty PATCH status = %d, want 400", emptyPatch.Code)
	}

	// Unknown field in patch -> 400.
	unkPatch := doAPI(t, mux, http.MethodPatch, fmt.Sprintf("/api/cards/%d", card.ID), `{"bogus":"x"}`, auth)
	if unkPatch.Code != http.StatusBadRequest {
		t.Fatalf("unknown-field PATCH status = %d, want 400", unkPatch.Code)
	}

	// Patch question and source only.
	patchRec := doAPI(t, mux, http.MethodPatch, fmt.Sprintf("/api/cards/%d", card.ID),
		`{"question":"Updated question?","source":"human"}`, auth)
	if patchRec.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d (body=%s)", patchRec.Code, patchRec.Body.String())
	}
	var patched Card
	_ = json.Unmarshal(patchRec.Body.Bytes(), &patched)
	if patched.Circumstance != "Original scene." || patched.Question != "Updated question?" || patched.Source != "human" {
		t.Fatalf("unexpected patched card: %+v", patched)
	}

	// Archive.
	archRec := doAPI(t, mux, http.MethodPost, fmt.Sprintf("/api/cards/%d/archive", card.ID), "", auth)
	if archRec.Code != http.StatusOK {
		t.Fatalf("archive status = %d", archRec.Code)
	}
	var archived Card
	_ = json.Unmarshal(archRec.Body.Bytes(), &archived)
	if archived.ArchivedAt == nil {
		t.Fatal("expected archived_at to be non-nil after archive")
	}

	// Restore.
	restRec := doAPI(t, mux, http.MethodPost, fmt.Sprintf("/api/cards/%d/restore", card.ID), "", auth)
	if restRec.Code != http.StatusOK {
		t.Fatalf("restore status = %d", restRec.Code)
	}
	var restored Card
	_ = json.Unmarshal(restRec.Body.Bytes(), &restored)
	if restored.ArchivedAt != nil {
		t.Fatalf("expected archived_at nil after restore, got %v", *restored.ArchivedAt)
	}

	// Audit should have 4 entries: create, patch, archive, restore.
	var audits int
	if err := database.QueryRow(`SELECT COUNT(*) FROM api_audit WHERE card_id = ?`, card.ID).Scan(&audits); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if audits != 4 {
		t.Fatalf("audit count = %d, want 4", audits)
	}
}

func TestBatchAllOrNothingAndLimit(t *testing.T) {
	database, mux := newTestAPI(t)
	auth := "Bearer " + testAPIKey

	// Seed one existing card.
	rec := doAPI(t, mux, http.MethodPost, "/api/cards",
		`{"circumstance":"Seed.","question":"Seed?"}`, auth)
	var seed Card
	_ = json.Unmarshal(rec.Body.Bytes(), &seed)

	// More than 200 ops -> 400.
	ops := make([]string, 201)
	for i := range ops {
		ops[i] = `{"op":"create","circumstance":"C","question":"Q?"}`
	}
	tooMany := `{"ops":[` + strings.Join(ops, ",") + `]}`
	tooManyRec := doAPI(t, mux, http.MethodPost, "/api/cards/batch", tooMany, auth)
	if tooManyRec.Code != http.StatusBadRequest {
		t.Fatalf("201-op batch status = %d, want 400", tooManyRec.Code)
	}

	// Bad batch: op 0 creates a card, op 1 patches a nonexistent ID -> entire batch rolls back.
	badBatch := `{"ops":[
		{"op":"create","circumstance":"Should roll back","question":"Roll back?"},
		{"op":"patch","id":99999,"question":"Nope?"}
	]}`
	badRec := doAPI(t, mux, http.MethodPost, "/api/cards/batch", badBatch, auth)
	if badRec.Code != http.StatusNotFound {
		t.Fatalf("bad batch status = %d, want 404 (body=%s)", badRec.Code, badRec.Body.String())
	}
	var cardCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM cards`).Scan(&cardCount); err != nil {
		t.Fatalf("count cards: %v", err)
	}
	if cardCount != 1 {
		t.Fatalf("cards count after failed batch = %d, want 1 (rollback failed)", cardCount)
	}

	// Valid batch: create + patch seed + archive seed + restore seed.
	goodBatch := fmt.Sprintf(`{"ops":[
		{"op":"create","circumstance":"Batch created","question":"Batch Q?"},
		{"op":"patch","id":%d,"question":"Seed updated?"},
		{"op":"archive","id":%d},
		{"op":"restore","id":%d}
	]}`, seed.ID, seed.ID, seed.ID)
	goodRec := doAPI(t, mux, http.MethodPost, "/api/cards/batch", goodBatch, auth)
	if goodRec.Code != http.StatusOK {
		t.Fatalf("good batch status = %d, want 200 (body=%s)", goodRec.Code, goodRec.Body.String())
	}
	var raw map[string]any
	_ = json.Unmarshal(goodRec.Body.Bytes(), &raw)
	if _, hasDry := raw["dry_run"]; hasDry {
		t.Fatalf("non-dry batch response must not have dry_run, got %v", raw)
	}
	var resp batchResponse
	if err := json.Unmarshal(goodRec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal batch response: %v", err)
	}
	if len(resp.Results) != 4 {
		t.Fatalf("batch results len = %d, want 4", len(resp.Results))
	}
	if resp.Results[0].Author != "Claude" || resp.Results[0].Source != "ai" {
		t.Fatalf("batch create result = %+v", resp.Results[0])
	}
	if resp.Results[1].Question != "Seed updated?" {
		t.Fatalf("batch patch result = %+v", resp.Results[1])
	}
	if resp.Results[2].ArchivedAt == nil {
		t.Fatal("batch archive result should have archived_at")
	}
	if resp.Results[3].ArchivedAt != nil {
		t.Fatal("batch restore result should have nil archived_at")
	}

	if err := database.QueryRow(`SELECT COUNT(*) FROM cards`).Scan(&cardCount); err != nil {
		t.Fatalf("count cards after good batch: %v", err)
	}
	if cardCount != 2 {
		t.Fatalf("cards count after good batch = %d, want 2", cardCount)
	}
}

func TestDryRunRollsBackAuditsAndFlagsResponse(t *testing.T) {
	database, mux := newTestAPI(t)
	auth := "Bearer " + testAPIKey

	// Dry-run create: returns 201 preview with dry_run:true, cards table stays empty, audit row survives.
	rec := doAPI(t, mux, http.MethodPost, "/api/cards?dry_run=1",
		`{"circumstance":"Dry scene.","question":"Dry question?"}`, auth)
	if rec.Code != http.StatusCreated {
		t.Fatalf("dry create status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	var preview dryRunCardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
		t.Fatalf("unmarshal preview: %v", err)
	}
	if !preview.DryRun {
		t.Fatalf("expected dry_run:true in single create response, body=%s", rec.Body.String())
	}
	if preview.Circumstance != "Dry scene." || preview.Author != "Claude" || preview.Source != "ai" {
		t.Fatalf("unexpected dry-run preview: %+v", preview)
	}

	var cardCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM cards`).Scan(&cardCount); err != nil {
		t.Fatalf("count cards: %v", err)
	}
	if cardCount != 0 {
		t.Fatalf("cards count after dry_run = %d, want 0", cardCount)
	}

	var dryRun int
	var beforeJSON, afterJSON sql.NullString
	if err := database.QueryRow(
		`SELECT dry_run, before_json, after_json FROM api_audit ORDER BY id DESC LIMIT 1`,
	).Scan(&dryRun, &beforeJSON, &afterJSON); err != nil {
		t.Fatalf("expected audit row after dry_run: %v", err)
	}
	if dryRun != 1 || beforeJSON.Valid || !afterJSON.Valid || !strings.Contains(afterJSON.String, "Dry scene.") {
		t.Fatalf("unexpected dry_run audit row: dry=%d before=%v after=%v", dryRun, beforeJSON, afterJSON)
	}

	// Real create, then dry-run patch, archive, restore, and batch.
	realRec := doAPI(t, mux, http.MethodPost, "/api/cards",
		`{"circumstance":"Real scene.","question":"Real question?"}`, auth)
	var realCard Card
	_ = json.Unmarshal(realRec.Body.Bytes(), &realCard)

	dryPatch := doAPI(t, mux, http.MethodPatch, fmt.Sprintf("/api/cards/%d?dry_run=1", realCard.ID),
		`{"question":"Would change?"}`, auth)
	if dryPatch.Code != http.StatusOK {
		t.Fatalf("dry patch status = %d", dryPatch.Code)
	}
	var patchedPreview dryRunCardResponse
	_ = json.Unmarshal(dryPatch.Body.Bytes(), &patchedPreview)
	if !patchedPreview.DryRun || patchedPreview.Question != "Would change?" {
		t.Fatalf("dry patch preview = %+v", patchedPreview)
	}

	dryArch := doAPI(t, mux, http.MethodPost, fmt.Sprintf("/api/cards/%d/archive?dry_run=1", realCard.ID), "", auth)
	var archPreview dryRunCardResponse
	_ = json.Unmarshal(dryArch.Body.Bytes(), &archPreview)
	if !archPreview.DryRun || archPreview.ArchivedAt == nil {
		t.Fatalf("dry archive preview = %+v", archPreview)
	}

	dryBatch := doAPI(t, mux, http.MethodPost, "/api/cards/batch?dry_run=1",
		`{"ops":[{"op":"create","circumstance":"Dry batch","question":"Dry batch Q?"}]}`, auth)
	if dryBatch.Code != http.StatusOK {
		t.Fatalf("dry batch status = %d (body=%s)", dryBatch.Code, dryBatch.Body.String())
	}
	var batchPreview batchResponse
	_ = json.Unmarshal(dryBatch.Body.Bytes(), &batchPreview)
	if !batchPreview.DryRun || len(batchPreview.Results) != 1 {
		t.Fatalf("dry batch preview = %+v", batchPreview)
	}

	// DB must still have only realCard with original question and unarchived.
	if err := database.QueryRow(`SELECT COUNT(*) FROM cards`).Scan(&cardCount); err != nil {
		t.Fatalf("count cards: %v", err)
	}
	if cardCount != 1 {
		t.Fatalf("cards count = %d, want 1", cardCount)
	}
	var storedQ string
	var storedArch sql.NullString
	if err := database.QueryRow(`SELECT question, archived_at FROM cards WHERE id = ?`, realCard.ID).Scan(&storedQ, &storedArch); err != nil {
		t.Fatalf("query stored card: %v", err)
	}
	if storedQ != "Real question?" || storedArch.Valid {
		t.Fatalf("stored card mutated by dry runs: q=%q arch=%v", storedQ, storedArch)
	}

	// Audit table must have 4 dry_run=1 rows (create, patch, archive, batch) + 1 dry_run=0 row.
	var dryAudits int
	if err := database.QueryRow(`SELECT COUNT(*) FROM api_audit WHERE dry_run = 1`).Scan(&dryAudits); err != nil {
		t.Fatalf("count dry audits: %v", err)
	}
	if dryAudits != 4 {
		t.Fatalf("dry audit count = %d, want 4", dryAudits)
	}
}
