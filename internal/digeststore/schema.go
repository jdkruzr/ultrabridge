package digeststore

import (
	"context"
	"database/sql"
	"fmt"
)

// Migrate creates the digests and digest_tags tables idempotently. Called from
// main in SPC server mode (precedent: fileids.Migrate / staging.Migrate), not by
// notedb.Open, keeping digest storage gated to UB-as-SPC server mode.
func Migrate(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS digests (
			id                        INTEGER PRIMARY KEY AUTOINCREMENT,
			file_id                   INTEGER NOT NULL DEFAULT 0,
			user_id                   INTEGER NOT NULL DEFAULT 0,
			name                      TEXT NOT NULL DEFAULT '',
			unique_identifier         TEXT NOT NULL DEFAULT '',
			parent_unique_identifier  TEXT NOT NULL DEFAULT '',
			content                   TEXT NOT NULL DEFAULT '',
			source_path               TEXT NOT NULL DEFAULT '',
			data_source               TEXT NOT NULL DEFAULT '',
			source_type               INTEGER NOT NULL DEFAULT 0,
			is_group                  TEXT NOT NULL DEFAULT 'N',
			description               TEXT NOT NULL DEFAULT '',
			tags                      TEXT NOT NULL DEFAULT '',
			md5_hash                  TEXT NOT NULL DEFAULT '',
			metadata                  TEXT NOT NULL DEFAULT '',
			comment_str               TEXT NOT NULL DEFAULT '',
			comment_handwrite_name    TEXT NOT NULL DEFAULT '',
			handwrite_inner_name      TEXT NOT NULL DEFAULT '',
			handwrite_md5             TEXT NOT NULL DEFAULT '',
			creation_time             INTEGER NOT NULL DEFAULT 0,
			last_modified_time        INTEGER NOT NULL DEFAULT 0,
			author                    TEXT NOT NULL DEFAULT '',
			is_deleted                TEXT NOT NULL DEFAULT 'N',
			created_at                INTEGER NOT NULL DEFAULT 0,
			updated_at                INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_digests_user ON digests(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_digests_uid ON digests(unique_identifier)`,
		`CREATE INDEX IF NOT EXISTS idx_digests_parent ON digests(parent_unique_identifier)`,
		`CREATE INDEX IF NOT EXISTS idx_digests_deleted ON digests(is_deleted)`,
		`CREATE TABLE IF NOT EXISTS digest_tags (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id    INTEGER NOT NULL DEFAULT 0,
			name       TEXT NOT NULL,
			created_at INTEGER NOT NULL DEFAULT 0,
			UNIQUE(user_id, name)
		)`,
	}
	for i, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("digeststore migration statement %d: %w", i, err)
		}
	}
	return nil
}
