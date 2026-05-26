package notedb

import (
	"context"
	"database/sql"
	"fmt"
)

// migrate creates all tables and indexes if they do not exist. Idempotent.
func migrate(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS notes (
			path         TEXT NOT NULL PRIMARY KEY,
			rel_path     TEXT NOT NULL,
			file_type    TEXT NOT NULL,
			size_bytes   INTEGER,
			mtime        INTEGER,
			sha256       TEXT,
			backup_path  TEXT,
			backed_up_at INTEGER,
			created_at   INTEGER NOT NULL,
			updated_at   INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS jobs (
			id           INTEGER PRIMARY KEY,
			note_path    TEXT NOT NULL UNIQUE REFERENCES notes(path),
			status       TEXT NOT NULL,
			skip_reason  TEXT,
			ocr_source   TEXT,
			api_model    TEXT,
			attempts     INTEGER NOT NULL DEFAULT 0,
			last_error   TEXT,
			queued_at    INTEGER,
			started_at   INTEGER,
			finished_at  INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS idx_jobs_note_path ON jobs(note_path)`,
		`CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status)`,
		`CREATE TABLE IF NOT EXISTS note_content (
			id          INTEGER PRIMARY KEY,
			note_path   TEXT NOT NULL,
			page        INTEGER NOT NULL,
			title_text  TEXT,
			body_text   TEXT,
			keywords    TEXT,
			source      TEXT,
			model       TEXT,
			indexed_at  INTEGER NOT NULL,
			UNIQUE(note_path, page)
		)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS note_fts USING fts5(
			note_path, page UNINDEXED, title_text, body_text, keywords,
			content="note_content",
			content_rowid="id"
		)`,
		`CREATE TRIGGER IF NOT EXISTS note_content_ai
			AFTER INSERT ON note_content BEGIN
				INSERT INTO note_fts(rowid, note_path, page, title_text, body_text, keywords)
				VALUES (new.id, new.note_path, new.page, new.title_text, new.body_text, new.keywords);
			END`,
		`CREATE TRIGGER IF NOT EXISTS note_content_ad
			AFTER DELETE ON note_content BEGIN
				INSERT INTO note_fts(note_fts, rowid, note_path, page, title_text, body_text, keywords)
				VALUES ('delete', old.id, old.note_path, old.page, old.title_text, old.body_text, old.keywords);
			END`,
		`CREATE TRIGGER IF NOT EXISTS note_content_au
			AFTER UPDATE ON note_content BEGIN
				INSERT INTO note_fts(note_fts, rowid, note_path, page, title_text, body_text, keywords)
				VALUES ('delete', old.id, old.note_path, old.page, old.title_text, old.body_text, old.keywords);
				INSERT INTO note_fts(rowid, note_path, page, title_text, body_text, keywords)
				VALUES (new.id, new.note_path, new.page, new.title_text, new.body_text, new.keywords);
			END`,
		`CREATE TABLE IF NOT EXISTS boox_notes (
			path TEXT PRIMARY KEY,
			note_id TEXT NOT NULL DEFAULT '',
			title TEXT NOT NULL DEFAULT '',
			device_model TEXT NOT NULL DEFAULT '',
			note_type TEXT NOT NULL DEFAULT '',
			folder TEXT NOT NULL DEFAULT '',
			page_count INTEGER NOT NULL DEFAULT 0,
			file_hash TEXT NOT NULL DEFAULT '',
			version INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS boox_jobs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			note_path TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			skip_reason TEXT NOT NULL DEFAULT '',
			ocr_source TEXT NOT NULL DEFAULT '',
			api_model TEXT NOT NULL DEFAULT '',
			attempts INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			queued_at INTEGER NOT NULL DEFAULT 0,
			started_at INTEGER NOT NULL DEFAULT 0,
			finished_at INTEGER NOT NULL DEFAULT 0,
			requeue_after INTEGER,
			FOREIGN KEY (note_path) REFERENCES boox_notes(path)
		)`,
		`CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS sources (
			id          INTEGER PRIMARY KEY,
			type        TEXT NOT NULL,
			name        TEXT NOT NULL,
			enabled     INTEGER NOT NULL DEFAULT 1,
			config_json TEXT NOT NULL DEFAULT '{}',
			created_at  INTEGER NOT NULL,
			updated_at  INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS note_embeddings (
			note_path  TEXT NOT NULL,
			page       INTEGER NOT NULL,
			chunk      INTEGER NOT NULL DEFAULT 0,
			embedding  BLOB NOT NULL,
			model      TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			UNIQUE(note_path, page, chunk)
		)`,
		`CREATE TABLE IF NOT EXISTS chat_sessions (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			title      TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS chat_messages (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id INTEGER NOT NULL REFERENCES chat_sessions(id),
			role       TEXT NOT NULL,
			content    TEXT NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_chat_messages_session ON chat_messages(session_id)`,
	}
	for i, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migration statement %d: %w", i, err)
		}
	}

	// Add requeue_after column to jobs table (idempotent — check first, then ALTER)
	var count int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('jobs') WHERE name='requeue_after'`).Scan(&count)
	if count == 0 {
		if _, err := db.ExecContext(ctx, `ALTER TABLE jobs ADD COLUMN requeue_after INTEGER`); err != nil {
			return fmt.Errorf("add requeue_after column: %w", err)
		}
	}

	// note_embeddings chunk support: pre-chunking the table was UNIQUE(note_path,
	// page) with one vector per page. Long pages now embed as multiple chunks, so
	// widen the key to (note_path,page,chunk). SQLite can't drop a table-level
	// UNIQUE via ALTER, so rebuild the table (existing rows become chunk 0). The
	// CREATE above already produces the new shape on a fresh DB, so this only
	// fires for pre-existing old-shape tables.
	var hasChunk int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('note_embeddings') WHERE name='chunk'`).Scan(&hasChunk)
	if hasChunk == 0 {
		rebuild := []string{
			`ALTER TABLE note_embeddings RENAME TO note_embeddings_old`,
			`CREATE TABLE note_embeddings (
				note_path  TEXT NOT NULL,
				page       INTEGER NOT NULL,
				chunk      INTEGER NOT NULL DEFAULT 0,
				embedding  BLOB NOT NULL,
				model      TEXT NOT NULL,
				created_at INTEGER NOT NULL,
				UNIQUE(note_path, page, chunk)
			)`,
			`INSERT INTO note_embeddings (note_path, page, chunk, embedding, model, created_at)
				SELECT note_path, page, 0, embedding, model, created_at FROM note_embeddings_old`,
			`DROP TABLE note_embeddings_old`,
		}
		for i, stmt := range rebuild {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("note_embeddings chunk migration %d: %w", i, err)
			}
		}
	}

	// Backfill notes.created_at: older rows stored scan-time (wall clock)
	// instead of file mtime. Fix by setting created_at = mtime * 1000
	// where created_at is clearly later than the file's modification time.
	_, _ = db.ExecContext(ctx, `UPDATE notes SET created_at = mtime * 1000 WHERE created_at > mtime * 1000`)

	return nil
}
