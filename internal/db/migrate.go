package db

import (
	"database/sql"
	"errors"
	"fmt"
)

type migration struct {
	Version int
	Name    string
	SQL     string
	Check   func(*sql.Tx) error
}

var migrations = []migration{
	{
		Version: 1,
		Name:    "baseline",
		SQL: `
CREATE TABLE IF NOT EXISTS users (
  id INTEGER PRIMARY KEY,
  username TEXT NOT NULL UNIQUE COLLATE NOCASE,
  password_hash TEXT NOT NULL,
  is_admin INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE IF NOT EXISTS sessions (
  token TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS cards (
  id INTEGER PRIMARY KEY,
  author_id INTEGER NOT NULL REFERENCES users(id),
  circumstance TEXT NOT NULL,
  question TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE IF NOT EXISTS games (
  id INTEGER PRIMARY KEY,
  mode TEXT NOT NULL CHECK (mode IN ('classic','freeplay')),
  point_goal INTEGER,
  status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','finished')),
  created_by INTEGER NOT NULL REFERENCES users(id),
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  finished_at TEXT
);
CREATE TABLE IF NOT EXISTS game_players (
  game_id INTEGER NOT NULL REFERENCES games(id) ON DELETE CASCADE,
  user_id INTEGER NOT NULL REFERENCES users(id),
  points INTEGER NOT NULL DEFAULT 0,
  turn_order INTEGER NOT NULL,
  is_winner INTEGER NOT NULL DEFAULT 0,
  joined_at TEXT NOT NULL DEFAULT (datetime('now')),
  PRIMARY KEY (game_id, user_id)
);
CREATE TABLE IF NOT EXISTS rounds (
  id INTEGER PRIMARY KEY,
  game_id INTEGER NOT NULL REFERENCES games(id) ON DELETE CASCADE,
  card_id INTEGER NOT NULL REFERENCES cards(id),
  drawer_id INTEGER NOT NULL REFERENCES users(id),
  drawer_answer TEXT CHECK (drawer_answer IN ('yes','no')),
  status TEXT NOT NULL DEFAULT 'voting' CHECK (status IN ('voting','revealed','challenge','complete')),
  final_answer TEXT CHECK (final_answer IN ('yes','no')),
  challenger_id INTEGER REFERENCES users(id),
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE IF NOT EXISTS votes (
  round_id INTEGER NOT NULL REFERENCES rounds(id) ON DELETE CASCADE,
  user_id INTEGER NOT NULL REFERENCES users(id),
  phase TEXT NOT NULL CHECK (phase IN ('initial','challenge')),
  vote TEXT NOT NULL CHECK (vote IN ('yes','no')),
  PRIMARY KEY (round_id, user_id, phase)
);
`,
	},
	{
		Version: 2,
		Name:    "cards_source_archive",
		SQL: `
ALTER TABLE cards ADD COLUMN source TEXT NOT NULL DEFAULT 'human' CHECK (source IN ('human','ai'));
ALTER TABLE cards ADD COLUMN archived_at TEXT;
CREATE INDEX IF NOT EXISTS cards_active ON cards(archived_at);
`,
	},
	{
		Version: 3,
		Name:    "api_claude_audit",
		Check: func(tx *sql.Tx) error {
			var exists int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM users WHERE username = 'Claude' COLLATE NOCASE`).Scan(&exists); err != nil {
				return err
			}
			if exists > 0 {
				return errors.New("cannot seed bot user 'Claude': a non-bot user with that username already exists")
			}
			return nil
		},
		SQL: `
ALTER TABLE users ADD COLUMN is_bot INTEGER NOT NULL DEFAULT 0;
INSERT INTO users (username, password_hash, is_admin, is_bot) VALUES ('Claude', '!', 0, 1);
CREATE TABLE IF NOT EXISTS api_audit (
  id INTEGER PRIMARY KEY,
  at TEXT NOT NULL DEFAULT (datetime('now')),
  method TEXT NOT NULL,
  path TEXT NOT NULL,
  card_id INTEGER,
  dry_run INTEGER NOT NULL DEFAULT 0,
  before_json TEXT,
  after_json TEXT
);
`,
	},
	{
		Version: 4,
		Name:    "discussion",
		SQL: `
ALTER TABLE games ADD COLUMN discussion_seconds INTEGER NOT NULL DEFAULT 60;
ALTER TABLE rounds ADD COLUMN completed_at TEXT;
ALTER TABLE rounds ADD COLUMN discussion_ends_at TEXT;
`,
	},
}

func appliedVersions(db *sql.DB) (map[int]bool, error) {
	rows, err := db.Query(`SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	applied := make(map[int]bool)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := appliedVersions(db)
	if err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}

	for _, m := range migrations {
		if applied[m.Version] {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %d (%s): %w", m.Version, m.Name, err)
		}
		if m.Check != nil {
			if err := m.Check(tx); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("apply migration %d (%s): %w", m.Version, m.Name, err)
			}
		}
		if _, err := tx.Exec(m.SQL); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %d (%s): %w", m.Version, m.Name, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO schema_migrations (version, name) VALUES (?, ?)`,
			m.Version, m.Name,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %d (%s): %w", m.Version, m.Name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d (%s): %w", m.Version, m.Name, err)
		}
	}
	return nil
}
