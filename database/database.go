package database

import (
	"database/sql"
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
