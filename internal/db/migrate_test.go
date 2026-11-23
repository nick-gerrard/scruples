package db

import (
	"database/sql"
	"path/filepath"
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
}
