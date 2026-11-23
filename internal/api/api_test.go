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

func TestCreateDefaultsAndValidation(t *testing.T) {
	database, mux := newTestAPI(t)
	auth := "Bearer " + testAPIKey

	// Default source=ai and author=Claude.
	rec := doAPI(t, mux, http.MethodPost, "/api/cards",
		`{"circumstance":"You find a wallet.","question":"Do you return it?"}`, auth)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/cards status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	var created Card
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if created.ID <= 0 || created.Source != "ai" || created.Author != "Claude" || created.ArchivedAt != nil {
		t.Fatalf("unexpected created card: %+v", created)
	}

	// Verify audit row recorded.
	var auditCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM api_audit WHERE card_id = ? AND dry_run = 0`, created.ID).Scan(&auditCount); err != nil {
		t.Fatalf("query audit: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("audit count = %d, want 1", auditCount)
	}

	// Validation: empty fields and length limits.
	badBodies := []string{
		`{"circumstance":"","question":"Q?"}`,
		`{"circumstance":"C","question":""}`,
		fmt.Sprintf(`{"circumstance":%q,"question":"Q?"}`, strings.Repeat("a", 2001)),
		fmt.Sprintf(`{"circumstance":"C","question":%q}`, strings.Repeat("q", 301)),
		`{"circumstance":"C","question":"Q?","source":"robot"}`,
		`{"circumstance":"C","question":"Q?","author":"nobody"}`,
	}
	for i, body := range badBodies {
		rec := doAPI(t, mux, http.MethodPost, "/api/cards", body, auth)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("bad body #%d status = %d, want 400 (body=%s)", i, rec.Code, rec.Body.String())
		}
	}
}

func TestListFiltersAndGet(t *testing.T) {
	database, mux := newTestAPI(t)
	auth := "Bearer " + testAPIKey

	if _, err := database.Exec(`INSERT INTO users (username, password_hash) VALUES ('alice', 'hash')`); err != nil {
		t.Fatalf("insert alice: %v", err)
	}

	// Create 3 cards:
	// 1. ai / Claude / active ("wallet")
	// 2. human / alice / active ("umbrella")
	// 3. ai / Claude / archived ("wallet archived")
	c1Rec := doAPI(t, mux, http.MethodPost, "/api/cards",
		`{"circumstance":"Lost wallet on bus.","question":"Keep the cash?"}`, auth)
	var c1 Card
	_ = json.Unmarshal(c1Rec.Body.Bytes(), &c1)

	c2Rec := doAPI(t, mux, http.MethodPost, "/api/cards",
		`{"circumstance":"Borrowed an umbrella.","question":"Return it broken?","source":"human","author":"alice"}`, auth)
	var c2 Card
	_ = json.Unmarshal(c2Rec.Body.Bytes(), &c2)

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
		got := map[int64]bool{}
		for _, c := range resp.Cards {
			got[c.ID] = true
		}
		for _, id := range wantIDs {
			if !got[id] {
				t.Errorf("GET /api/cards%s missing card %d (got %+v)", query, id, resp.Cards)
			}
		}
	}

	checkList("", c1.ID, c2.ID)
	checkList("?archived=0", c1.ID, c2.ID)
	checkList("?archived=1", c3.ID)
	checkList("?archived=all", c1.ID, c2.ID, c3.ID)
	checkList("?source=human", c2.ID)
	checkList("?source=ai", c1.ID)
	checkList("?source=ai&archived=all", c1.ID, c3.ID)
	checkList("?author=alice", c2.ID)
	checkList("?author=Claude", c1.ID)
	checkList("?q=WALLET", c1.ID)
	checkList("?q=wallet&archived=all", c1.ID, c3.ID)
	checkList("?q=broken", c2.ID)

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

func TestBatchAllOrNothing(t *testing.T) {
	database, mux := newTestAPI(t)
	auth := "Bearer " + testAPIKey

	// Seed one existing card.
	rec := doAPI(t, mux, http.MethodPost, "/api/cards",
		`{"circumstance":"Seed.","question":"Seed?"}`, auth)
	var seed Card
	_ = json.Unmarshal(rec.Body.Bytes(), &seed)

	// Bad batch: op 0 creates a card, op 1 patches a nonexistent ID -> entire batch rolls back.
	badBatch := fmt.Sprintf(`{"ops":[
		{"op":"create","circumstance":"Should roll back","question":"Roll back?"},
		{"op":"patch","id":99999,"question":"Nope?"}
	]}`)
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

func TestDryRunRollsBackButAudits(t *testing.T) {
	database, mux := newTestAPI(t)
	auth := "Bearer " + testAPIKey

	// Dry-run create: returns 201 preview, cards table stays empty, audit row survives.
	rec := doAPI(t, mux, http.MethodPost, "/api/cards?dry_run=1",
		`{"circumstance":"Dry scene.","question":"Dry question?"}`, auth)
	if rec.Code != http.StatusCreated {
		t.Fatalf("dry create status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	var preview Card
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
		t.Fatalf("unmarshal preview: %v", err)
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

	// Real create, then dry-run patch & batch.
	realRec := doAPI(t, mux, http.MethodPost, "/api/cards",
		`{"circumstance":"Real scene.","question":"Real question?"}`, auth)
	var realCard Card
	_ = json.Unmarshal(realRec.Body.Bytes(), &realCard)

	dryPatch := doAPI(t, mux, http.MethodPatch, fmt.Sprintf("/api/cards/%d?dry_run=1", realCard.ID),
		`{"question":"Would change?"}`, auth)
	if dryPatch.Code != http.StatusOK {
		t.Fatalf("dry patch status = %d", dryPatch.Code)
	}
	var patchedPreview Card
	_ = json.Unmarshal(dryPatch.Body.Bytes(), &patchedPreview)
	if patchedPreview.Question != "Would change?" {
		t.Fatalf("dry patch preview question = %q, want 'Would change?'", patchedPreview.Question)
	}

	// DB must still have original question.
	var storedQ string
	if err := database.QueryRow(`SELECT question FROM cards WHERE id = ?`, realCard.ID).Scan(&storedQ); err != nil {
		t.Fatalf("query stored question: %v", err)
	}
	if storedQ != "Real question?" {
		t.Fatalf("stored question = %q, want 'Real question?'", storedQ)
	}

	// Audit row for dry patch must have both before_json and after_json and dry_run=1.
	if err := database.QueryRow(
		`SELECT dry_run, before_json, after_json FROM api_audit ORDER BY id DESC LIMIT 1`,
	).Scan(&dryRun, &beforeJSON, &afterJSON); err != nil {
		t.Fatalf("query dry patch audit: %v", err)
	}
	if dryRun != 1 || !beforeJSON.Valid || !afterJSON.Valid {
		t.Fatalf("unexpected dry patch audit: dry=%d before=%v after=%v", dryRun, beforeJSON, afterJSON)
	}
	if !strings.Contains(beforeJSON.String, "Real question?") || !strings.Contains(afterJSON.String, "Would change?") {
		t.Fatalf("audit before/after mismatch: before=%s after=%s", beforeJSON.String, afterJSON.String)
	}
}
