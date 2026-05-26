package syncstore

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// Store is the stateful sync layer over notedb. All writes go through ApplyBatch
// in a single transaction; reads (OpsSince) stream the changelog for relay.
type Store struct {
	db *sql.DB
}

func New(db *sql.DB) *Store { return &Store{db: db} }

// RejectedOp identifies a permanently refused op (spec §7.2).
type RejectedOp struct {
	SiteID string `json:"site_id"`
	OpSeq  int64  `json:"op_seq"`
	Reason string `json:"reason"`
}

// ApplyResult reports the outcome of an ApplyBatch for the requesting device.
// AcceptedThrough is the contiguous high-water (spec §4.1), computed and
// persisted inside the apply transaction. Rejected is returned verbatim to the
// client; ChangedPages feeds the pipeline bridge (Phase 2).
type ApplyResult struct {
	AcceptedThrough int64
	Rejected        []RejectedOp
	ChangedPages    []TablePK // pages whose live render input may have changed
}

// validateOp returns "" if op is structurally acceptable, else a permanent
// rejection reason (spec §5.3 step 1/3, §7.2). It does not check value types —
// those are caught when the winning op is materialized.
func validateOp(op Op) string {
	known, ok := knownCols[op.Table]
	if !ok {
		return "unknown table"
	}
	if !IsULID(op.PK) {
		return "pk is not a ULID"
	}
	if !IsULID(op.SiteID) {
		return "site_id is not a ULID"
	}
	if op.OpSeq <= 0 {
		return "op_seq must be > 0"
	}
	for _, c := range known {
		if _, present := op.Cols[c]; !present {
			return "missing column: " + c
		}
	}
	return ""
}

// ApplyBatch validates, dedups, merges, and records each op in one transaction,
// then computes and persists the requesting device's contiguous accepted_through.
// siteID is the authenticated device; accepted_through is computed for it.
// Render/OCR are intentionally NOT here — the bridge runs after commit so it
// cannot stall /sync/v1 against the single-writer notedb.
func (s *Store) ApplyBatch(ctx context.Context, siteID string, ops []Op) (ApplyResult, error) {
	var res ApplyResult
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UnixMilli()
	rejectedSeqs := make(map[int64]bool) // op_seqs permanently rejected for siteID this call
	reject := func(op Op, reason string) {
		res.Rejected = append(res.Rejected, RejectedOp{op.SiteID, op.OpSeq, reason})
		if op.SiteID == siteID {
			rejectedSeqs[op.OpSeq] = true
		}
	}

	for _, op := range ops {
		if reason := validateOp(op); reason != "" {
			reject(op, reason)
			continue
		}

		// Dedup: already in the changelog → already settled, no re-apply (spec §7.3).
		var dummy int
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM sync_ops WHERE site_id = ? AND op_seq = ?`,
			op.SiteID, op.OpSeq).Scan(&dummy)
		if err == nil {
			continue
		} else if err != sql.ErrNoRows {
			return res, fmt.Errorf("dedup check: %w", err)
		}

		n := Normalize(op)
		changed, pagePK, mErr := mergeRow(ctx, tx, n)
		if mErr != nil {
			// Malformed value type in a known column → permanent rejection.
			reject(op, mErr.Error())
			continue
		}

		// Assign the next global seq and append the op verbatim (relay payload).
		// A losing op (mirror unchanged) is still recorded for relay completeness.
		var seq int64
		if err := tx.QueryRowContext(ctx,
			`UPDATE sync_seq SET last_seq = last_seq + 1 WHERE id = 1 RETURNING last_seq`).
			Scan(&seq); err != nil {
			return res, fmt.Errorf("bump seq: %w", err)
		}
		payload, err := json.Marshal(op) // original op (preserves any forward-compat cols)
		if err != nil {
			return res, fmt.Errorf("marshal payload: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sync_ops (seq, site_id, op_seq, table_name, pk, wall_ts, payload, applied_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			seq, op.SiteID, op.OpSeq, op.Table, op.PK, op.WallTS, string(payload), now); err != nil {
			return res, fmt.Errorf("insert sync_ops: %w", err)
		}

		if changed && pagePK != "" {
			res.ChangedPages = append(res.ChangedPages, TablePK{Table: "page", PK: pagePK})
		}
	}

	accepted, err := advanceAccepted(ctx, tx, siteID, rejectedSeqs, now)
	if err != nil {
		return res, err
	}
	res.AcceptedThrough = accepted

	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("commit: %w", err)
	}
	return res, nil
}

// advanceAccepted computes and persists siteID's contiguous accepted_through
// (spec §4.1): the greatest N such that every op_seq 1..N is durably settled —
// present in sync_ops (applied or deduped from any call) OR permanently rejected
// this call. Walking from the persisted high-water means a rejected op is counted
// once (here), advanced past, and never revisited — so a poison op neither wedges
// the water nor is silently lost.
func advanceAccepted(ctx context.Context, tx *sql.Tx, siteID string, rejectedSeqs map[int64]bool, now int64) (int64, error) {
	var h int64
	switch err := tx.QueryRowContext(ctx,
		`SELECT acked_op_seq FROM sync_cursors WHERE site_id = ?`, siteID).Scan(&h); err {
	case nil, sql.ErrNoRows:
		// h is 0 on ErrNoRows (Scan leaves it zero)
	default:
		return 0, fmt.Errorf("read acked_op_seq: %w", err)
	}

	for {
		next := h + 1
		if rejectedSeqs[next] {
			h = next
			continue
		}
		var dummy int
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM sync_ops WHERE site_id = ? AND op_seq = ?`, siteID, next).Scan(&dummy)
		if err == nil {
			h = next
			continue
		}
		if err == sql.ErrNoRows {
			break
		}
		return 0, fmt.Errorf("accepted walk: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sync_cursors (site_id, acked_op_seq, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(site_id) DO UPDATE SET acked_op_seq = excluded.acked_op_seq, updated_at = excluded.updated_at`,
		siteID, h, now); err != nil {
		return 0, fmt.Errorf("persist acked_op_seq: %w", err)
	}
	return h, nil
}

// mergeRow applies the LWW rule to one mirror row. Returns whether the row was
// written (incoming won) and, for stroke/page ops, the affected page pk. A type
// coercion failure on a winning op is a permanent rejection (returned as error).
func mergeRow(ctx context.Context, tx *sql.Tx, n Op) (changed bool, pagePK string, err error) {
	table := "fn_" + n.Table
	var sw, ss sql.NullInt64
	var siteID sql.NullString
	row := tx.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT lww_wall_ts, lww_op_seq, lww_site_id FROM %s WHERE id = ?`, table),
		n.PK)
	switch scanErr := row.Scan(&sw, &ss, &siteID); scanErr {
	case nil:
		stored := Op{WallTS: sw.Int64, OpSeq: ss.Int64, SiteID: siteID.String}
		if !Less(stored, n) {
			return false, "", nil // incoming does not win; mirror unchanged
		}
	case sql.ErrNoRows:
		// no existing row → incoming wins
	default:
		return false, "", fmt.Errorf("load mirror row: %w", scanErr)
	}

	switch n.Table {
	case "folder":
		err = upsertFolder(ctx, tx, n)
	case "notebook":
		err = upsertNotebook(ctx, tx, n)
	case "page":
		err = upsertPage(ctx, tx, n)
		pagePK = n.PK
	case "stroke":
		var pid string
		pid, err = upsertStroke(ctx, tx, n)
		pagePK = pid
	}
	if err != nil {
		return false, "", err
	}
	return true, pagePK, nil
}

func upsertNotebook(ctx context.Context, tx *sql.Tx, n Op) error {
	name, err := colString(n, "name")
	if err != nil {
		return err
	}
	sort, err := colInt(n, "sort_order")
	if err != nil {
		return err
	}
	created, err := colInt(n, "created_at")
	if err != nil {
		return err
	}
	del, err := colNullInt(n, "deleted_at")
	if err != nil {
		return err
	}
	folderID, err := colNullString(n, "folder_id") // null = root (no folder)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO fn_notebook (id, name, sort_order, created_at, deleted_at, folder_id, lww_wall_ts, lww_op_seq, lww_site_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name, sort_order=excluded.sort_order,
		   created_at=excluded.created_at, deleted_at=excluded.deleted_at, folder_id=excluded.folder_id,
		   lww_wall_ts=excluded.lww_wall_ts, lww_op_seq=excluded.lww_op_seq, lww_site_id=excluded.lww_site_id`,
		n.PK, name, sort, created, del, folderID, n.WallTS, n.OpSeq, n.SiteID)
	return err
}

func upsertFolder(ctx context.Context, tx *sql.Tx, n Op) error {
	name, err := colString(n, "name")
	if err != nil {
		return err
	}
	sort, err := colInt(n, "sort_order")
	if err != nil {
		return err
	}
	created, err := colInt(n, "created_at")
	if err != nil {
		return err
	}
	del, err := colNullInt(n, "deleted_at")
	if err != nil {
		return err
	}
	parent, err := colNullString(n, "parent_folder_id") // null = root-level folder
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO fn_folder (id, name, sort_order, created_at, deleted_at, parent_folder_id, lww_wall_ts, lww_op_seq, lww_site_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name, sort_order=excluded.sort_order,
		   created_at=excluded.created_at, deleted_at=excluded.deleted_at, parent_folder_id=excluded.parent_folder_id,
		   lww_wall_ts=excluded.lww_wall_ts, lww_op_seq=excluded.lww_op_seq, lww_site_id=excluded.lww_site_id`,
		n.PK, name, sort, created, del, parent, n.WallTS, n.OpSeq, n.SiteID)
	return err
}

func upsertPage(ctx context.Context, tx *sql.Tx, n Op) error {
	nb, err := colString(n, "notebook_id")
	if err != nil {
		return err
	}
	sort, err := colInt(n, "sort_order")
	if err != nil {
		return err
	}
	created, err := colInt(n, "created_at")
	if err != nil {
		return err
	}
	del, err := colNullInt(n, "deleted_at")
	if err != nil {
		return err
	}
	template, err := colNullString(n, "template") // null = inherit Settings.defaultTemplate
	if err != nil {
		return err
	}
	pitch, err := colNullInt(n, "template_pitch_mm") // null = inherit Settings.defaultPitchMm
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO fn_page (id, notebook_id, sort_order, created_at, deleted_at, template, template_pitch_mm, lww_wall_ts, lww_op_seq, lww_site_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET notebook_id=excluded.notebook_id, sort_order=excluded.sort_order,
		   created_at=excluded.created_at, deleted_at=excluded.deleted_at,
		   template=excluded.template, template_pitch_mm=excluded.template_pitch_mm,
		   lww_wall_ts=excluded.lww_wall_ts, lww_op_seq=excluded.lww_op_seq, lww_site_id=excluded.lww_site_id`,
		n.PK, nb, sort, created, del, template, pitch, n.WallTS, n.OpSeq, n.SiteID)
	return err
}

func upsertStroke(ctx context.Context, tx *sql.Tx, n Op) (pageID string, err error) {
	pageID, err = colString(n, "page_id")
	if err != nil {
		return "", err
	}
	color, err := colInt(n, "color")
	if err != nil {
		return "", err
	}
	wmin, err := colInt(n, "pen_width_min")
	if err != nil {
		return "", err
	}
	wmax, err := colInt(n, "pen_width_max")
	if err != nil {
		return "", err
	}
	pts, err := colBytes(n, "points")
	if err != nil {
		return "", err
	}
	z, err := colInt(n, "z")
	if err != nil {
		return "", err
	}
	created, err := colInt(n, "created_at")
	if err != nil {
		return "", err
	}
	del, err := colNullInt(n, "deleted_at")
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO fn_stroke (id, page_id, color, pen_width_min, pen_width_max, points, z, created_at, deleted_at, lww_wall_ts, lww_op_seq, lww_site_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET page_id=excluded.page_id, color=excluded.color,
		   pen_width_min=excluded.pen_width_min, pen_width_max=excluded.pen_width_max,
		   points=excluded.points, z=excluded.z, created_at=excluded.created_at, deleted_at=excluded.deleted_at,
		   lww_wall_ts=excluded.lww_wall_ts, lww_op_seq=excluded.lww_op_seq, lww_site_id=excluded.lww_site_id`,
		n.PK, pageID, color, wmin, wmax, pts, z, created, del, n.WallTS, n.OpSeq, n.SiteID)
	return pageID, err
}

// OpsSince returns changelog ops with seq > cursor authored by some OTHER site,
// in ascending seq, capped at limit. newCursor is the seq of the last returned
// op (or cursor if none); hasMore is true iff more lie beyond the cap (spec §4.1).
func (s *Store) OpsSince(ctx context.Context, cursor int64, excludeSite string, limit int) (ops []Op, newCursor int64, hasMore bool, err error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, payload FROM sync_ops WHERE seq > ? AND site_id <> ? ORDER BY seq LIMIT ?`,
		cursor, excludeSite, limit+1) // +1 to detect has_more
	if err != nil {
		return nil, cursor, false, fmt.Errorf("ops since: %w", err)
	}
	defer rows.Close()

	newCursor = cursor
	for rows.Next() {
		var seq int64
		var payload string
		if err := rows.Scan(&seq, &payload); err != nil {
			return nil, cursor, false, fmt.Errorf("scan op: %w", err)
		}
		if len(ops) == limit { // the +1 row: there's more, don't return it
			hasMore = true
			break
		}
		var op Op
		if err := json.Unmarshal([]byte(payload), &op); err != nil {
			return nil, cursor, false, fmt.Errorf("unmarshal op seq %d: %w", seq, err)
		}
		ops = append(ops, op)
		newCursor = seq
	}
	if err := rows.Err(); err != nil {
		return nil, cursor, false, fmt.Errorf("iterate ops: %w", err)
	}
	return ops, newCursor, hasMore, nil
}

// LastSeq returns the current global high-water (spec §7.4 cursor reconciliation).
func (s *Store) LastSeq(ctx context.Context) (int64, error) {
	var seq int64
	err := s.db.QueryRowContext(ctx, `SELECT last_seq FROM sync_seq WHERE id = 1`).Scan(&seq)
	return seq, err
}

// RecordCursor stores a device's last pull high-water (best-effort bookkeeping /
// observability; the wire is client-driven so this is not load-bearing).
func (s *Store) RecordCursor(ctx context.Context, siteID string, lastPullSeq int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sync_cursors (site_id, last_pull_seq, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(site_id) DO UPDATE SET last_pull_seq=excluded.last_pull_seq, updated_at=excluded.updated_at`,
		siteID, lastPullSeq, time.Now().UnixMilli())
	return err
}

// --- column coercion (JSON numbers decode as float64) ---

func colInt(n Op, key string) (int64, error) {
	switch v := n.Cols[key].(type) {
	case float64:
		return int64(v), nil
	default:
		return 0, fmt.Errorf("column %q must be a number", key)
	}
}

func colNullInt(n Op, key string) (sql.NullInt64, error) {
	switch v := n.Cols[key].(type) {
	case nil:
		return sql.NullInt64{}, nil
	case float64:
		return sql.NullInt64{Int64: int64(v), Valid: true}, nil
	default:
		return sql.NullInt64{}, fmt.Errorf("column %q must be a number or null", key)
	}
}

func colString(n Op, key string) (string, error) {
	if s, ok := n.Cols[key].(string); ok {
		return s, nil
	}
	return "", fmt.Errorf("column %q must be a string", key)
}

func colNullString(n Op, key string) (sql.NullString, error) {
	switch v := n.Cols[key].(type) {
	case nil:
		return sql.NullString{}, nil
	case string:
		return sql.NullString{String: v, Valid: true}, nil
	default:
		return sql.NullString{}, fmt.Errorf("column %q must be a string or null", key)
	}
}

func colBytes(n Op, key string) ([]byte, error) {
	s, ok := n.Cols[key].(string)
	if !ok {
		return nil, fmt.Errorf("column %q must be a base64 string", key)
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("column %q is not valid base64: %w", key, err)
	}
	return b, nil
}
