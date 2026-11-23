package db

import (
	"database/sql"

	_ "modernc.org/sqlite"
)

// Open opens (creating if necessary) the SQLite database at path, enables
// foreign keys and WAL mode, and applies any pending schema migrations.
func Open(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
