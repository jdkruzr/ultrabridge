package syncstore

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"time"
)

// inventory.go exposes the mirror as a browsable note inventory for the UI:
// live folders, notebooks (with page counts), and a notebook's ordered pages.
// These are live projections of the fn_* tables — there is NO separate inventory
// table (the mirror IS the inventory). All reads filter soft-deletes
// (deleted_at IS NULL) and are single-statement to stay cheap against the
// single-writer notedb (WAL, MaxOpenConns=1). Keyed identifiers are ULIDs; a
// page is addressed for rendering as forestnote://{notebook_id}/{page_id}.

// FolderRow is one live folder. ParentFolderID is "" for a top-level folder.
// CreatedAt is the synced fn_folder.created_at; ModifiedAt is the folder row's
// own last-write wall clock (lww_wall_ts) — folders don't roll up child edits.
type FolderRow struct {
	ID             string
	Name           string
	ParentFolderID string
	SortOrder      int64
	CreatedAt      int64 // ms UTC, 0 = unset
	ModifiedAt     int64 // ms UTC
}

// NotebookRow is one live notebook with its live page count. FolderID is "" when
// the notebook is unfiled (sits at the root, not inside any folder). CreatedAt is
// the synced created_at; ModifiedAt is a derived "last activity" — MAX(lww_wall_ts)
// over the notebook row, its live pages, and those pages' live strokes — so
// drawing on a page bumps the notebook's modified time even though only the
// stroke row changed.
type NotebookRow struct {
	ID         string
	Name       string
	FolderID   string
	SortOrder  int64
	PageCount  int
	CreatedAt  int64 // ms UTC, 0 = unset
	ModifiedAt int64 // ms UTC, derived
}

// notebookModifiedExpr is the SQLite scalar MAX(...) (NOT the aggregate — there
// is no GROUP BY) that derives a notebook's "last activity" wall clock. The row
// term needs no COALESCE (lww_wall_ts is NOT NULL); the subquery terms do (an
// empty page/stroke set yields NULL). Indexed by idx_fn_page_nb / idx_fn_stroke_pg.
const notebookModifiedExpr = `MAX(
		n.lww_wall_ts,
		COALESCE((SELECT MAX(p.lww_wall_ts) FROM fn_page p
		            WHERE p.notebook_id = n.id AND p.deleted_at IS NULL), 0),
		COALESCE((SELECT MAX(s.lww_wall_ts) FROM fn_stroke s
		            JOIN fn_page p2 ON p2.id = s.page_id
		           WHERE p2.notebook_id = n.id AND p2.deleted_at IS NULL
		             AND s.deleted_at IS NULL), 0))`

// notebookPageCountExpr counts a notebook's live pages.
const notebookPageCountExpr = `(SELECT COUNT(*) FROM fn_page p
		WHERE p.notebook_id = n.id AND p.deleted_at IS NULL)`

// PageRef identifies a live page within a notebook, in display order.
type PageRef struct {
	ID        string
	SortOrder int64
}

// ListFolders returns all live folders. The caller assembles the tree (folders
// link to their parent via ParentFolderID); this read stays flat and cheap.
func (s *Store) ListFolders(ctx context.Context) ([]FolderRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, COALESCE(name, ''), COALESCE(parent_folder_id, ''), COALESCE(sort_order, 0),
		        COALESCE(created_at, 0), lww_wall_ts
		   FROM fn_folder WHERE deleted_at IS NULL ORDER BY sort_order, name`)
	if err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}
	defer rows.Close()
	var out []FolderRow
	for rows.Next() {
		var f FolderRow
		if err := rows.Scan(&f.ID, &f.Name, &f.ParentFolderID, &f.SortOrder, &f.CreatedAt, &f.ModifiedAt); err != nil {
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
		        `+notebookPageCountExpr+`, COALESCE(n.created_at, 0), `+notebookModifiedExpr+`
		   FROM fn_notebook n WHERE n.deleted_at IS NULL ORDER BY n.sort_order, n.name`)
	if err != nil {
		return nil, fmt.Errorf("list notebooks: %w", err)
	}
	defer rows.Close()
	var out []NotebookRow
	for rows.Next() {
		var n NotebookRow
		if err := rows.Scan(&n.ID, &n.Name, &n.FolderID, &n.SortOrder, &n.PageCount, &n.CreatedAt, &n.ModifiedAt); err != nil {
			return nil, fmt.Errorf("scan notebook: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ListFolderContents returns the direct children of folderID (empty string =
// root): subfolders whose parent is exactly folderID, and notebooks whose
// folder is exactly folderID. This backs breadcrumb-style navigation (one
// level at a time) rather than assembling the whole tree per request.
func (s *Store) ListFolderContents(ctx context.Context, folderID string) ([]FolderRow, []NotebookRow, error) {
	frows, err := s.db.QueryContext(ctx,
		`SELECT id, COALESCE(name, ''), COALESCE(parent_folder_id, ''), COALESCE(sort_order, 0),
		        COALESCE(created_at, 0), lww_wall_ts
		   FROM fn_folder
		  WHERE deleted_at IS NULL AND COALESCE(parent_folder_id, '') = ?
		  ORDER BY sort_order, name`, folderID)
	if err != nil {
		return nil, nil, fmt.Errorf("folder contents (folders): %w", err)
	}
	defer frows.Close()
	var folders []FolderRow
	for frows.Next() {
		var f FolderRow
		if err := frows.Scan(&f.ID, &f.Name, &f.ParentFolderID, &f.SortOrder, &f.CreatedAt, &f.ModifiedAt); err != nil {
			return nil, nil, fmt.Errorf("scan folder: %w", err)
		}
		folders = append(folders, f)
	}
	if err := frows.Err(); err != nil {
		return nil, nil, err
	}

	nrows, err := s.db.QueryContext(ctx,
		`SELECT n.id, COALESCE(n.name, ''), COALESCE(n.folder_id, ''), COALESCE(n.sort_order, 0),
		        `+notebookPageCountExpr+`, COALESCE(n.created_at, 0), `+notebookModifiedExpr+`
		   FROM fn_notebook n
		  WHERE n.deleted_at IS NULL AND COALESCE(n.folder_id, '') = ?
		  ORDER BY n.sort_order, n.name`, folderID)
	if err != nil {
		return nil, nil, fmt.Errorf("folder contents (notebooks): %w", err)
	}
	defer nrows.Close()
	var notebooks []NotebookRow
	for nrows.Next() {
		var n NotebookRow
		if err := nrows.Scan(&n.ID, &n.Name, &n.FolderID, &n.SortOrder, &n.PageCount, &n.CreatedAt, &n.ModifiedAt); err != nil {
			return nil, nil, fmt.Errorf("scan notebook: %w", err)
		}
		notebooks = append(notebooks, n)
	}
	return folders, notebooks, nrows.Err()
}

// FolderPath returns the ancestor chain root→folderID (inclusive) for breadcrumb
// rendering. Returns nil for the root (empty folderID). The depth guard prevents
// an infinite loop if a corrupt mirror ever produces a parent cycle.
func (s *Store) FolderPath(ctx context.Context, folderID string) ([]FolderRow, error) {
	var chain []FolderRow
	seen := make(map[string]bool)
	for id := folderID; id != "" && !seen[id]; {
		seen[id] = true
		if len(chain) > 64 {
			break // corrupt-mirror cycle guard
		}
		var f FolderRow
		err := s.db.QueryRowContext(ctx,
			`SELECT id, COALESCE(name, ''), COALESCE(parent_folder_id, ''), COALESCE(sort_order, 0),
			        COALESCE(created_at, 0), lww_wall_ts
			   FROM fn_folder WHERE id = ? AND deleted_at IS NULL`, id).
			Scan(&f.ID, &f.Name, &f.ParentFolderID, &f.SortOrder, &f.CreatedAt, &f.ModifiedAt)
		if err == sql.ErrNoRows {
			break // a deleted/missing ancestor truncates the chain
		}
		if err != nil {
			return nil, fmt.Errorf("folder path: %w", err)
		}
		chain = append(chain, f)
		id = f.ParentFolderID
	}
	// Reverse to root→leaf order.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain, nil
}

// LiveNotebookPageIDs returns the live page IDs of a notebook (used to re-enqueue
// a notebook's pages for reprocessing).
func (s *Store) LiveNotebookPageIDs(ctx context.Context, notebookID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM fn_page WHERE notebook_id = ? AND deleted_at IS NULL ORDER BY sort_order, id`,
		notebookID)
	if err != nil {
		return nil, fmt.Errorf("live notebook pages: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan page id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SoftDeleteNotebook deletes a notebook plus its live pages and those pages'
// live strokes by AUTHORING tombstone ops — full-row upserts with deleted_at set
// to now — through the server-authoring path (Store.AuthorOps). Because the
// tombstones are recorded in the changelog under UB's own site_id, the relay
// carries them to the user's devices on their next pull: the delete is a real
// two-way push, not just a UB-local mirror edit. LWW still arbitrates the outcome
// (forestnote-sync-protocol.md §5): a UB delete reliably beats OLDER device ops,
// but a device edit with a NEWER wall_ts wins and resurrects the row (conformance
// vector ub-delete-then-device-edit). The notebook + its pages + their strokes
// are all tombstoned so the content — not just the notebook header — leaves the
// device.
//
// Returns the IDs of the pages that were live at delete time, so the caller can
// de-index each forestnote://{nb}/{page} from search + embeddings. The reads that
// build the tombstone set are not in AuthorOps's transaction, so a stroke a device
// adds between read and author is not tombstoned (it re-deletes on the next user
// action) — acceptable for a user-triggered delete on the single-writer notedb.
func (s *Store) SoftDeleteNotebook(ctx context.Context, notebookID string) ([]string, error) {
	now := time.Now().UnixMilli()

	// Notebook full row → tombstone op. A missing row means nothing to delete.
	var nbName sql.NullString
	var nbSort, nbCreated, nbAspect sql.NullInt64
	var nbFolder sql.NullString
	switch err := s.db.QueryRowContext(ctx,
		`SELECT name, sort_order, created_at, folder_id, aspect_long_axis FROM fn_notebook WHERE id = ?`, notebookID).
		Scan(&nbName, &nbSort, &nbCreated, &nbFolder, &nbAspect); err {
	case nil:
	case sql.ErrNoRows:
		return nil, nil
	default:
		return nil, fmt.Errorf("read notebook: %w", err)
	}
	ops := []Op{{
		Table: "notebook", PK: notebookID,
		Cols: map[string]any{
			"name":             nbName.String, // NOT NULL in practice; "" if ever null
			"sort_order":       wireNum(nbSort),
			"created_at":       wireNum(nbCreated),
			"deleted_at":       float64(now),
			"folder_id":        wireNullStr(nbFolder),
			"aspect_long_axis": wireNullNum(nbAspect),
		},
	}}

	// Live pages → tombstones; collect their IDs for the caller's de-index set.
	prows, err := s.db.QueryContext(ctx,
		`SELECT id, notebook_id, sort_order, created_at, template, template_pitch_mm
		   FROM fn_page WHERE notebook_id = ? AND deleted_at IS NULL`, notebookID)
	if err != nil {
		return nil, fmt.Errorf("read live pages: %w", err)
	}
	var pageIDs []string
	for prows.Next() {
		var id string
		var pnb sql.NullString
		var psort, pcreated, ppitch sql.NullInt64
		var ptmpl sql.NullString
		if err := prows.Scan(&id, &pnb, &psort, &pcreated, &ptmpl, &ppitch); err != nil {
			prows.Close()
			return nil, fmt.Errorf("scan page: %w", err)
		}
		pageIDs = append(pageIDs, id)
		ops = append(ops, Op{
			Table: "page", PK: id,
			Cols: map[string]any{
				"notebook_id":       pnb.String,
				"sort_order":        wireNum(psort),
				"created_at":        wireNum(pcreated),
				"deleted_at":        float64(now),
				"template":          wireNullStr(ptmpl),
				"template_pitch_mm": wireNullNum(ppitch),
			},
		})
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pages: %w", err)
	}

	// Live strokes of those live pages → tombstones (full row; points re-emitted).
	srows, err := s.db.QueryContext(ctx,
		`SELECT id, page_id, color, pen_width_min, pen_width_max, points, z, created_at
		   FROM fn_stroke
		  WHERE deleted_at IS NULL
		    AND page_id IN (SELECT id FROM fn_page WHERE notebook_id = ? AND deleted_at IS NULL)`,
		notebookID)
	if err != nil {
		return nil, fmt.Errorf("read live strokes: %w", err)
	}
	for srows.Next() {
		var id string
		var spage sql.NullString
		var scolor, swmin, swmax, sz, screated sql.NullInt64
		var spts []byte
		if err := srows.Scan(&id, &spage, &scolor, &swmin, &swmax, &spts, &sz, &screated); err != nil {
			srows.Close()
			return nil, fmt.Errorf("scan stroke: %w", err)
		}
		ops = append(ops, Op{
			Table: "stroke", PK: id,
			Cols: map[string]any{
				"page_id":       spage.String,
				"color":         wireNum(scolor),
				"pen_width_min": wireNum(swmin),
				"pen_width_max": wireNum(swmax),
				"points":        base64.StdEncoding.EncodeToString(spts),
				"z":             wireNum(sz),
				"created_at":    wireNum(screated),
				"deleted_at":    float64(now),
			},
		})
	}
	srows.Close()
	if err := srows.Err(); err != nil {
		return nil, fmt.Errorf("iterate strokes: %w", err)
	}

	// Each deleted page's recognized-text row → tombstone, so the page's OCR text
	// leaves the device alongside its content (not just the page header).
	for _, id := range pageIDs {
		ops = append(ops, Op{
			Table: "page_text_from_server", PK: id,
			Cols: pageTextCols("", 0, now, "", &now),
		})
	}

	if _, err := s.AuthorOps(ctx, ops); err != nil {
		return nil, fmt.Errorf("author notebook delete: %w", err)
	}
	return pageIDs, nil
}

// TextBoxRef identifies a live text box for discovery (e.g. by an MCP agent that
// wants to edit one): its ULID, the page it lives on, and its current text.
type TextBoxRef struct {
	ID     string
	PageID string
	Text   string
	Z      int64
}

// ListNotebookTextBoxes returns every live text box on a notebook's live pages,
// ordered by page then z. Backs text-box discovery before an edit.
func (s *Store) ListNotebookTextBoxes(ctx context.Context, notebookID string) ([]TextBoxRef, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT t.id, t.page_id, COALESCE(t.text, ''), COALESCE(t.z, 0)
		   FROM fn_text_box t
		   JOIN fn_page p ON p.id = t.page_id
		  WHERE p.notebook_id = ? AND p.deleted_at IS NULL AND t.deleted_at IS NULL
		  ORDER BY p.sort_order, p.id, t.z, t.id`, notebookID)
	if err != nil {
		return nil, fmt.Errorf("list notebook text boxes: %w", err)
	}
	defer rows.Close()
	var out []TextBoxRef
	for rows.Next() {
		var r TextBoxRef
		if err := rows.Scan(&r.ID, &r.PageID, &r.Text, &r.Z); err != nil {
			return nil, fmt.Errorf("scan text box ref: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// EditTextBoxText authors a server-side edit of one text box's text. It reads the
// box's current full row and re-authors it as a text_box upsert with the new text
// (every other column preserved) through AuthorOps — so the edit is recorded in
// the changelog under UB's site_id and relayed to the user's devices on their next
// pull, then resolved by the same LWW rule as a device edit. Returns the box's
// page_id so the caller can re-render/re-index that page. Errors if the box is
// missing or already deleted (a tombstoned box is not editable).
func (s *Store) EditTextBoxText(ctx context.Context, boxID, newText string) (pageID string, err error) {
	var pid, text, fontName string
	var x, y, w, h, fontSize, color, weight, border, z, created int64
	var del sql.NullInt64
	switch e := s.db.QueryRowContext(ctx,
		`SELECT page_id, x, y, width, height, text, font_name, font_size, color, weight, border_width, z, created_at, deleted_at
		   FROM fn_text_box WHERE id = ?`, boxID).
		Scan(&pid, &x, &y, &w, &h, &text, &fontName, &fontSize, &color, &weight, &border, &z, &created, &del); e {
	case nil:
	case sql.ErrNoRows:
		return "", fmt.Errorf("text box not found: %s", boxID)
	default:
		return "", fmt.Errorf("read text box: %w", e)
	}
	if del.Valid {
		return "", fmt.Errorf("text box is deleted: %s", boxID)
	}
	op := Op{Table: "text_box", PK: boxID, Cols: map[string]any{
		"page_id": pid, "x": float64(x), "y": float64(y), "width": float64(w), "height": float64(h),
		"text": newText, "font_name": fontName, "font_size": float64(fontSize), "color": float64(color),
		"weight": float64(weight), "border_width": float64(border), "z": float64(z),
		"created_at": float64(created), "deleted_at": nil,
	}}
	if _, err := s.AuthorOps(ctx, []Op{op}); err != nil {
		return "", fmt.Errorf("author text box edit: %w", err)
	}
	return pid, nil
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
		`SELECT n.id, COALESCE(n.name, ''), COALESCE(n.folder_id, ''), COALESCE(n.sort_order, 0),
		        `+notebookPageCountExpr+`, COALESCE(n.created_at, 0), `+notebookModifiedExpr+`
		   FROM fn_notebook n WHERE n.id = ? AND n.deleted_at IS NULL`,
		notebookID).Scan(&n.ID, &n.Name, &n.FolderID, &n.SortOrder, &n.PageCount, &n.CreatedAt, &n.ModifiedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return NotebookRow{}, err
		}
		return NotebookRow{}, fmt.Errorf("notebook meta: %w", err)
	}
	return n, nil
}
