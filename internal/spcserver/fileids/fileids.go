// Package fileids assigns the stable numeric Long IDs the Supernote device uses
// to address folders and files in the SPC file protocol. The device sends a
// parent id to list_folder and an id to query_v3, so UB must resolve id→path as
// well as path→id — a one-way hash can't do that. The mapping is persisted in a
// dedicated spc_file_ids table (keyed on absolute filesystem path) so ids are
// stable across restarts, mirroring how real SPC's f_user_file rows carry a Long
// id. The table is migrated by this package (called from main in server mode),
// not by notedb.Open, keeping it gated to UB-as-SPC server mode (precedent:
// mcpauth.Migrate).
package fileids

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
)

// Migrate creates the spc_file_ids table idempotently.
func Migrate(ctx context.Context, db *sql.DB) error {
	const stmt = `CREATE TABLE IF NOT EXISTS spc_file_ids (
		id        INTEGER PRIMARY KEY AUTOINCREMENT,
		path      TEXT NOT NULL UNIQUE,
		md5       TEXT NOT NULL DEFAULT '',
		md5_size  INTEGER NOT NULL DEFAULT 0,
		md5_mtime INTEGER NOT NULL DEFAULT 0
	)`
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("fileids migration: %w", err)
	}
	return nil
}

// Registry assigns and resolves path↔id over the spc_file_ids table.
type Registry struct {
	db   *sql.DB
	root string // cleaned absolute storage root the device browses
}

// New builds a Registry for the given storage root. The root path itself is
// registered lazily (on the first RootID/IDFor call), so a query for the root's
// id resolves like any other path.
func New(db *sql.DB, root string) *Registry {
	return &Registry{db: db, root: filepath.Clean(root)}
}

// Root returns the cleaned storage root.
func (r *Registry) Root() string { return r.root }

// RootID returns the id of the storage root, registering it on first call.
func (r *Registry) RootID(ctx context.Context) (int64, error) {
	return r.IDFor(ctx, r.root)
}

// IDFor returns the stable id for absPath, assigning one on first sighting.
// The path is cleaned first so non-normalized device paths (double slashes)
// resolve to the same id as their canonical form.
func (r *Registry) IDFor(ctx context.Context, absPath string) (int64, error) {
	p := filepath.Clean(absPath)
	if _, err := r.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO spc_file_ids(path) VALUES(?)`, p); err != nil {
		return 0, fmt.Errorf("fileids assign %q: %w", p, err)
	}
	var id int64
	if err := r.db.QueryRowContext(ctx,
		`SELECT id FROM spc_file_ids WHERE path = ?`, p).Scan(&id); err != nil {
		return 0, fmt.Errorf("fileids lookup %q: %w", p, err)
	}
	return id, nil
}

// UpdatePath repoints an existing id at a new absolute path, preserving the id
// (and the md5 cache on the same row, valid because a move doesn't change
// content). Used by move_v3 so a moved file keeps its stable device-facing id.
func (r *Registry) UpdatePath(ctx context.Context, id int64, newAbsPath string) error {
	p := filepath.Clean(newAbsPath)
	if _, err := r.db.ExecContext(ctx,
		`UPDATE spc_file_ids SET path = ? WHERE id = ?`, p, id); err != nil {
		return fmt.Errorf("fileids UpdatePath %d→%q: %w", id, p, err)
	}
	return nil
}

// PathFor resolves an id back to its absolute path. found is false (with a nil
// error) when no row matches — the device probes unknown ids and must get a
// clean "not found", not a hard error.
func (r *Registry) PathFor(ctx context.Context, id int64) (path string, found bool, err error) {
	err = r.db.QueryRowContext(ctx,
		`SELECT path FROM spc_file_ids WHERE id = ?`, id).Scan(&path)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("fileids PathFor %d: %w", id, err)
	}
	return path, true, nil
}
