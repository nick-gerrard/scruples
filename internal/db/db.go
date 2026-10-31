package db

import (
	"database/sql"

	_ "modernc.org/sqlite"
)

const schema = `
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
`

// Open opens (creating if necessary) the SQLite database at path, enables
// foreign keys and WAL mode, and ensures the schema exists.
func Open(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
