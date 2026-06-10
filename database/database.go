package database

import (
	"database/sql"
	"fmt"
	"log"

	_ "modernc.org/sqlite"
)

var DB *sql.DB

func Init(path string) error {
	var err error
	DB, err = sql.Open("sqlite", path)
	if err != nil {
		return err
	}

	DB.Exec("PRAGMA journal_mode=WAL")
	DB.Exec("PRAGMA busy_timeout=5000")
	DB.SetMaxOpenConns(1)

	return migrate()
}

func migrate() error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS users (
			username TEXT PRIMARY KEY,
			password TEXT NOT NULL,
			active INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			token TEXT PRIMARY KEY,
			expires_at TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS catalog_cache (
			id INTEGER PRIMARY KEY,
			data TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS transcode_jobs (
			file_id TEXT PRIMARY KEY,
			file_name TEXT NOT NULL,
			file_path TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			progress REAL NOT NULL DEFAULT 0,
			error TEXT DEFAULT '',
			duration REAL NOT NULL DEFAULT 0,
			video_codec TEXT DEFAULT '',
			audio_codec TEXT DEFAULT '',
			dest_dir TEXT DEFAULT '',
			updated_at TEXT NOT NULL
		)`,
	}

	for _, m := range migrations {
		if _, err := DB.Exec(m); err != nil {
			return err
		}
	}

	if err := migrateColumns(); err != nil {
		return err
	}

	return nil
}

func hasColumn(table, col string) bool {
	rows, err := DB.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false
	}
	defer rows.Close()
	var cid, notnull, pk int
	var name, ctype string
	var dflt sql.NullString
	for rows.Next() {
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false
		}
		if name == col {
			return true
		}
	}
	return false
}

func migrateColumns() error {
	cols := []struct {
		table string
		col   string
		typ   string
		def   string
	}{
		{"users", "exp_date", "TEXT", "''"},
		{"users", "max_connections", "INTEGER", "1"},
		{"users", "plain_password", "TEXT", "''"},
	}
	for _, c := range cols {
		if !hasColumn(c.table, c.col) {
			_, err := DB.Exec(fmt.Sprintf(
				"ALTER TABLE %s ADD COLUMN %s %s DEFAULT %s",
				c.table, c.col, c.typ, c.def,
			))
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func Close() error {
	if DB != nil {
		return DB.Close()
	}
	return nil
}

func init() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
}
