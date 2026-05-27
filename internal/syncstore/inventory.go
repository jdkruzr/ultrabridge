package syncstore

import (
	"context"
	"database/sql"
	"fmt"
)

// inventory.go exposes the mirror as a browsable note inventory for the UI:
// live folders, notebooks (with page counts), and a notebook's ordered pages.
// These are live projections of the fn_* tables — there is NO separate inventory
// table (the mirror IS the inventory). All reads filter soft-deletes
// (deleted_at IS NULL) and are single-statement to stay cheap against the
// single-writer notedb (WAL, MaxOpenConns=1). Keyed identifiers are ULIDs; a
// page is addressed for rendering as forestnote://{notebook_id}/{page_id}.

// FolderRow is one live folder. ParentFolderID is "" for a top-level folder.
type FolderRow struct {
	ID             string
	Name           string
	ParentFolderID string
	SortOrder      int64
}

// NotebookRow is one live notebook with its live page count. FolderID is "" when
// the notebook is unfiled (sits at the root, not inside any folder).
type NotebookRow struct {
	ID        string
	Name      string
	FolderID  string
	SortOrder int64
	PageCount int
}

// PageRef identifies a live page within a notebook, in display order.
type PageRef struct {
	ID        string
	SortOrder int64
}

// ListFolders returns all live folders. The caller assembles the tree (folders
// link to their parent via ParentFolderID); this read stays flat and cheap.
func (s *Store) ListFolders(ctx context.Context) ([]FolderRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, COALESCE(name, ''), COALESCE(parent_folder_id, ''), COALESCE(sort_order, 0)
		   FROM fn_folder WHERE deleted_at IS NULL ORDER BY sort_order, name`)
	if err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}
	defer rows.Close()
	var out []FolderRow
	for rows.Next() {
		var f FolderRow
		if err := rows.Scan(&f.ID, &f.Name, &f.ParentFolderID, &f.SortOrder); err != nil {
			return nil, fmt.Errorf("scan folder: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ListNotebooks returns all live notebooks, each carrying a count of its live
// pages (so the Files tab can show "N pages" without a second round trip).
func (s *Store) ListNotebooks(ctx context.Context) ([]NotebookRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT n.id, COALESCE(n.name, ''), COALESCE(n.folder_id, ''), COALESCE(n.sort_order, 0),
		        (SELECT COUNT(*) FROM fn_page p WHERE p.notebook_id = n.id AND p.deleted_at IS NULL)
		   FROM fn_notebook n WHERE n.deleted_at IS NULL ORDER BY n.sort_order, n.name`)
	if err != nil {
		return nil, fmt.Errorf("list notebooks: %w", err)
	}
	defer rows.Close()
	var out []NotebookRow
	for rows.Next() {
		var n NotebookRow
		if err := rows.Scan(&n.ID, &n.Name, &n.FolderID, &n.SortOrder, &n.PageCount); err != nil {
			return nil, fmt.Errorf("scan notebook: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// NotebookPages returns a notebook's live pages in display order. The id tie-break
// keeps ordering stable when sort_order values collide.
func (s *Store) NotebookPages(ctx context.Context, notebookID string) ([]PageRef, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, COALESCE(sort_order, 0) FROM fn_page
		   WHERE notebook_id = ? AND deleted_at IS NULL ORDER BY sort_order, id`,
		notebookID)
	if err != nil {
		return nil, fmt.Errorf("notebook pages: %w", err)
	}
	defer rows.Close()
	var out []PageRef
	for rows.Next() {
		var p PageRef
		if err := rows.Scan(&p.ID, &p.SortOrder); err != nil {
			return nil, fmt.Errorf("scan page ref: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// NotebookMeta returns one live notebook's metadata (for the viewer header).
// Returns sql.ErrNoRows if the notebook is missing or soft-deleted.
func (s *Store) NotebookMeta(ctx context.Context, notebookID string) (NotebookRow, error) {
	var n NotebookRow
	err := s.db.QueryRowContext(ctx,
		`SELECT id, COALESCE(name, ''), COALESCE(folder_id, ''), COALESCE(sort_order, 0)
		   FROM fn_notebook WHERE id = ? AND deleted_at IS NULL`,
		notebookID).Scan(&n.ID, &n.Name, &n.FolderID, &n.SortOrder)
	if err != nil {
		if err == sql.ErrNoRows {
			return NotebookRow{}, err
		}
		return NotebookRow{}, fmt.Errorf("notebook meta: %w", err)
	}
	return n, nil
}
