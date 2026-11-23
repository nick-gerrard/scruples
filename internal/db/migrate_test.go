package db

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrationsFreshAndIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")

	d1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}

	applied, err := appliedVersions(d1)
	if err != nil {
		t.Fatalf("appliedVersions: %v", err)
	}
	if len(applied) != len(migrations) {
		t.Fatalf("applied count = %d, want %d (%v)", len(applied), len(migrations), applied)
	}
	for _, m := range migrations {
		if !applied[m.Version] {
			t.Errorf("migration %d (%s) not recorded", m.Version, m.Name)
		}
	}

	// Verify cards_active index exists.
	var idxName string
	if err := d1.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'cards_active'`).Scan(&idxName); err != nil {
		t.Fatalf("cards_active index missing: %v", err)
	}

	if err := d1.Close(); err != nil {
		t.Fatalf("close first DB: %v", err)
	}

	// Second Open must be idempotent.
	d2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	t.Cleanup(func() { _ = d2.Close() })

	var count int
	if err := d2.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if count != len(migrations) {
		t.Fatalf("schema_migrations count after reopen = %d, want %d", count, len(migrations))
	}
}

func TestMigrationsPreMigrationDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	// Simulate an existing pre-migration DB using only the original baseline schema.
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("sql.Open raw: %v", err)
	}
	if _, err := raw.Exec(migrations[0].SQL); err != nil {
		raw.Close()
		t.Fatalf("exec legacy schema: %v", err)
	}
	res, err := raw.Exec(`INSERT INTO users (username, password_hash) VALUES ('alice', 'hash')`)
	if err != nil {
		raw.Close()
		t.Fatalf("insert user: %v", err)
	}
	uid, _ := res.LastInsertId()
	if _, err := raw.Exec(
		`INSERT INTO cards (author_id, circumstance, question) VALUES (?, 'Old scene', 'Old question?')`,
		uid,
	); err != nil {
		raw.Close()
		t.Fatalf("insert legacy card: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw DB: %v", err)
	}

	// Now open via db.Open, which must apply all migrations while preserving existing data.
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy DB: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	var circumstance, question, source string
	var archivedAt sql.NullString
	if err := d.QueryRow(
		`SELECT circumstance, question, source, archived_at FROM cards`,
	).Scan(&circumstance, &question, &source, &archivedAt); err != nil {
		t.Fatalf("select migrated card: %v", err)
	}
	if circumstance != "Old scene" || question != "Old question?" {
		t.Fatalf("card text = (%q, %q), want ('Old scene', 'Old question?')", circumstance, question)
	}
	if source != "human" {
		t.Fatalf("source = %q, want 'human'", source)
	}
	if archivedAt.Valid {
		t.Fatalf("archived_at = %q, want NULL", archivedAt.String)
	}

	// Reject invalid source values via CHECK constraint.
	if _, err := d.Exec(
		`INSERT INTO cards (author_id, circumstance, question, source) VALUES (?, 'Bad', 'Bad?', 'bot')`,
		uid,
	); err == nil {
		t.Fatal("expected CHECK constraint error for invalid source, got nil")
	}

	// Verify Claude bot user was seeded by migration 3.
	var pwHash string
	var isAdmin, isBot int
	if err := d.QueryRow(
		`SELECT password_hash, is_admin, is_bot FROM users WHERE username = 'Claude'`,
	).Scan(&pwHash, &isAdmin, &isBot); err != nil {
		t.Fatalf("Claude bot user missing: %v", err)
	}
	if pwHash != "!" || isAdmin != 0 || isBot != 1 {
		t.Fatalf("Claude user = (hash=%q, admin=%d, bot=%d), want ('!', 0, 1)", pwHash, isAdmin, isBot)
	}
}

func TestMigration3FailsIfNonBotClaudeExists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude_conflict.db")

	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("sql.Open raw: %v", err)
	}
	if _, err := raw.Exec(migrations[0].SQL); err != nil {
		raw.Close()
		t.Fatalf("exec baseline: %v", err)
	}
	// Insert a pre-existing human user named "claude" (lowercase to test NOCASE).
	if _, err := raw.Exec(`INSERT INTO users (username, password_hash) VALUES ('claude', 'human-hash')`); err != nil {
		raw.Close()
		t.Fatalf("insert human claude: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw DB: %v", err)
	}

	d, err := Open(path)
	if err == nil {
		d.Close()
		t.Fatal("expected migration 3 to fail when a non-bot 'claude' user exists, got nil")
	}
	if !strings.Contains(err.Error(), "Claude") || !strings.Contains(err.Error(), "non-bot") {
		t.Fatalf("expected clear error mentioning Claude and non-bot, got: %v", err)
	}
}

func TestMigration4Discussion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "discuss.db")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	var name string
	if err := d.QueryRow(`SELECT name FROM schema_migrations WHERE version = 4`).Scan(&name); err != nil {
		t.Fatalf("migration 4 not recorded: %v", err)
	}
	if name != "discussion" {
		t.Fatalf("migration 4 name = %q, want discussion", name)
	}

	res, err := d.Exec(`INSERT INTO users (username, password_hash) VALUES ('alice', 'hash')`)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	uid, _ := res.LastInsertId()

	res, err = d.Exec(`INSERT INTO cards (author_id, circumstance, question) VALUES (?, 'Scene', 'Question?')`, uid)
	if err != nil {
		t.Fatalf("insert card: %v", err)
	}
	cid, _ := res.LastInsertId()

	res, err = d.Exec(`INSERT INTO games (mode, point_goal, created_by) VALUES ('classic', 5, ?)`, uid)
	if err != nil {
		t.Fatalf("insert game: %v", err)
	}
	gid, _ := res.LastInsertId()

	var ds int
	if err := d.QueryRow(`SELECT discussion_seconds FROM games WHERE id = ?`, gid).Scan(&ds); err != nil {
		t.Fatalf("select discussion_seconds: %v", err)
	}
	if ds != 60 {
		t.Fatalf("default discussion_seconds = %d, want 60", ds)
	}

	res, err = d.Exec(`INSERT INTO rounds (game_id, card_id, drawer_id) VALUES (?, ?, ?)`, gid, cid, uid)
	if err != nil {
		t.Fatalf("insert round: %v", err)
	}
	rid, _ := res.LastInsertId()

	var compAt, discEnd sql.NullString
	if err := d.QueryRow(`SELECT completed_at, discussion_ends_at FROM rounds WHERE id = ?`, rid).Scan(&compAt, &discEnd); err != nil {
		t.Fatalf("select round discussion columns: %v", err)
	}
	if compAt.Valid || discEnd.Valid {
		t.Fatalf("expected NULL completed_at/discussion_ends_at, got (%v, %v)", compAt, discEnd)
	}
}
