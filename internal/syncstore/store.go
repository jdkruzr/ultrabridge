package syncstore

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jdkruzr/rhizome/server-go/hlc"
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
	// Durable HLC: read this site's op_ts clock so we can drag it past the greatest incoming op_ts
	// (clock.ReceiveEvent below). Device ops keep their OWN op_ts (their LWW key) — they are NOT
	// re-stamped here; advancing UB's clock only guarantees that UB's NEXT authored op (e.g. OCR text
	// for these strokes) sorts strictly after everything it has observed, even under device clock skew.
	var lastHlc int64
	if err := tx.QueryRowContext(ctx, `SELECT last_hlc FROM sync_site WHERE id = 1`).Scan(&lastHlc); err != nil {
		return res, fmt.Errorf("read last_hlc: %w", err)
	}

	// Read the device's persisted accepted_through BEFORE this batch's inserts.
	// On a missing cursor row — a genuinely new device, or one whose row was
	// pruned (device management) and is now re-registering — seed from the
	// pre-batch MAX(op_seq) in the changelog. Walking from 0 would wedge below a
	// pruned device's real high-water at the first op_seq hole: compaction
	// reclaims sync_ops rows and historic rejections leave gaps, and the client
	// has already discarded acked outbox ops so it can never refill them.
	// Everything at or below the pre-batch MAX was settled (applied, since
	// compacted, or rejected). Reading it pre-insert keeps the gap-capping
	// semantics for THIS batch: an op the device skips in its first contact must
	// still cap the water so the device resends it.
	var acked int64
	switch err := tx.QueryRowContext(ctx,
		`SELECT acked_op_seq FROM sync_cursors WHERE site_id = ?`, siteID).Scan(&acked); err {
	case nil:
	case sql.ErrNoRows:
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(op_seq), 0) FROM sync_ops WHERE site_id = ?`, siteID).Scan(&acked); err != nil {
			return res, fmt.Errorf("reseed acked_op_seq: %w", err)
		}
	default:
		return res, fmt.Errorf("read acked_op_seq: %w", err)
	}

	var maxIncoming int64

	rejectedSeqs := make(map[int64]bool) // op_seqs permanently rejected for siteID this call
	reject := func(op Op, reason string) {
		res.Rejected = append(res.Rejected, RejectedOp{op.SiteID, op.OpSeq, reason})
		if op.SiteID == siteID {
			rejectedSeqs[op.OpSeq] = true
		}
	}

	for _, op := range ops {
		if op.WallTS > maxIncoming {
			maxIncoming = op.WallTS // observe every op's op_ts, even one we go on to reject
		}
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
		if err := appendOp(ctx, tx, op, now); err != nil {
			return res, err
		}

		if changed && pagePK != "" {
			res.ChangedPages = append(res.ChangedPages, TablePK{Table: "page", PK: pagePK})
		}
	}

	accepted, err := advanceAccepted(ctx, tx, siteID, acked, rejectedSeqs, now)
	if err != nil {
		return res, err
	}
	res.AcceptedThrough = accepted

	// Advance and persist the durable HLC past the greatest op_ts we just observed, so a later
	// AuthorOps issues an op_ts strictly greater (HLC ReceiveEvent — spec/hlc.md). Same single-writer
	// read-modify-write idiom as last_op_seq; the notedb is MaxOpenConns=1 so this can't race.
	clock := hlc.New(lastHlc, func() int64 { return now })
	clock.ReceiveEvent(maxIncoming)
	if _, err := tx.ExecContext(ctx,
		`UPDATE sync_site SET last_hlc = ? WHERE id = 1`, clock.Last()); err != nil {
		return res, fmt.Errorf("persist last_hlc: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("commit: %w", err)
	}
	return res, nil
}

// appendOp assigns the next global seq and appends op verbatim to the changelog,
// stamping applied_at = now. It is the changelog half shared by ApplyBatch
// (device-pushed ops) and AuthorOps (server-authored ops): identical relay
// payload and seq allocation, so both kinds of op flow through OpsSince the same
// way. The op is marshaled as-is to preserve any forward-compat columns.
func appendOp(ctx context.Context, tx *sql.Tx, op Op, now int64) error {
	var seq int64
	if err := tx.QueryRowContext(ctx,
		`UPDATE sync_seq SET last_seq = last_seq + 1 WHERE id = 1 RETURNING last_seq`).
		Scan(&seq); err != nil {
		return fmt.Errorf("bump seq: %w", err)
	}
	payload, err := json.Marshal(op)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sync_ops (seq, site_id, op_seq, table_name, pk, wall_ts, payload, applied_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		seq, op.SiteID, op.OpSeq, op.Table, op.PK, op.WallTS, string(payload), now); err != nil {
		return fmt.Errorf("insert sync_ops: %w", err)
	}
	return nil
}

// SiteID returns UB's own authoring ULID (seeded at Migrate, stable across
// restarts). Server-originated ops carry this as their site_id.
func (s *Store) SiteID(ctx context.Context) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT site_id FROM sync_site WHERE id = 1`).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("read site_id: %w", err)
	}
	return id, nil
}

// AuthorOps makes UB a first-class authoring site: it stamps each op with UB's
// site_id, the next monotonic op_seq, and an op_ts from UB's durable HLC
// (clock.LocalEvent — strictly greater than any op_ts UB has observed, so a
// server-authored op always sorts after the device ops that triggered it, even
// under device clock skew), then merges each into the mirror and
// appends it to the changelog in ONE transaction. Because they land in sync_ops
// with UB's site_id, the existing relay (OpsSince excludes only the requesting
// site) carries them to every device on its next pull — the wire needs no change.
//
// Callers pass full-row upserts (Table, PK, Cols); SiteID/OpSeq/WallTS are filled
// here and must not be pre-set. Each op is validated to the same bar as a device
// op (ULID pk, known table, all known columns present) so a programming error
// surfaces loudly rather than emitting a malformed op onto the wire. The op_seq
// counter is persisted in the same tx as the inserts, so a crash can neither
// double-allocate nor skip a sequence number. Returns the live pages whose render
// input changed (for the caller to re-render / re-index).
func (s *Store) AuthorOps(ctx context.Context, ops []Op) ([]TablePK, error) {
	if len(ops) == 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	var siteID string
	var lastOpSeq, lastHlc int64
	if err := tx.QueryRowContext(ctx,
		`SELECT site_id, last_op_seq, last_hlc FROM sync_site WHERE id = 1`).Scan(&siteID, &lastOpSeq, &lastHlc); err != nil {
		return nil, fmt.Errorf("load authoring site: %w", err)
	}

	now := time.Now().UnixMilli()
	clock := hlc.New(lastHlc, func() int64 { return now })
	seq := lastOpSeq
	var changedPages []TablePK
	for _, op := range ops {
		seq++
		op.SiteID = siteID
		op.OpSeq = seq
		op.WallTS = clock.LocalEvent() // op_ts strictly > anything UB has observed (HLC)
		if reason := validateOp(op); reason != "" {
			return nil, fmt.Errorf("author op %s/%s invalid: %s", op.Table, op.PK, reason)
		}
		n := Normalize(op)
		changed, pagePK, mErr := mergeRow(ctx, tx, n)
		if mErr != nil {
			return nil, fmt.Errorf("author merge %s/%s: %w", op.Table, op.PK, mErr)
		}
		if err := appendOp(ctx, tx, op, now); err != nil {
			return nil, err
		}
		if changed && pagePK != "" {
			changedPages = append(changedPages, TablePK{Table: "page", PK: pagePK})
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE sync_site SET last_op_seq = ?, last_hlc = ? WHERE id = 1`, seq, clock.Last()); err != nil {
		return nil, fmt.Errorf("persist op_seq/last_hlc: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return changedPages, nil
}

// --- authoring column helpers: mirror scan values → wire `cols` values ---
// Authored ops are built in Go but must look exactly like a decoded device op,
// where JSON numbers arrive as float64 (the col* accessors type-switch on it).
// So numeric columns become float64, nullable columns become a value or nil.

// wireNum renders a required numeric column; a NULL mirror value (shouldn't occur
// for device-sourced rows) coalesces to 0 rather than failing validation.
func wireNum(n sql.NullInt64) any {
	if n.Valid {
		return float64(n.Int64)
	}
	return float64(0)
}

// wireNullNum renders a nullable numeric column as float64 or nil.
func wireNullNum(n sql.NullInt64) any {
	if n.Valid {
		return float64(n.Int64)
	}
	return nil
}

// wireNullStr renders a nullable string column as string or nil.
func wireNullStr(s sql.NullString) any {
	if s.Valid {
		return s.String
	}
	return nil
}

// advanceAccepted computes and persists siteID's contiguous accepted_through
// (spec §4.1): the greatest N such that every op_seq 1..N is durably settled —
// present in sync_ops (applied or deduped from any call) OR permanently rejected
// this call. acked is the walk's starting point, read by ApplyBatch BEFORE the
// batch's inserts: the persisted high-water, or the pre-batch changelog MAX when
// the cursor row is missing (see the reseed comment there). Walking from the
// high-water means a rejected op is counted once (here), advanced past, and
// never revisited — so a poison op neither wedges the water nor is silently lost.
func advanceAccepted(ctx context.Context, tx *sql.Tx, siteID string, acked int64, rejectedSeqs map[int64]bool, now int64) (int64, error) {
	h := acked

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
	case "text_box":
		var pid string
		pid, err = upsertTextBox(ctx, tx, n)
		pagePK = pid
	case "page_text_from_server":
		// Server recognized text is NOT page render input — leave pagePK empty so it
		// never enters ChangedPages. If it did, authoring page text from the bridge
		// would re-enqueue the page and loop OCR→author→render forever.
		err = upsertPageText(ctx, tx, n)
	case "page_text_from_client":
		// Device recognized text is search/RAG input. Its pk is the page id, so report
		// that page as changed after materializing the row.
		err = upsertPageText(ctx, tx, n)
		pagePK = n.PK
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
	aspect, err := colNullInt(n, "aspect_long_axis") // null = legacy notebook (3:4 default)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO fn_notebook (id, name, sort_order, created_at, deleted_at, folder_id, aspect_long_axis, lww_wall_ts, lww_op_seq, lww_site_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name, sort_order=excluded.sort_order,
		   created_at=excluded.created_at, deleted_at=excluded.deleted_at, folder_id=excluded.folder_id,
		   aspect_long_axis=excluded.aspect_long_axis,
		   lww_wall_ts=excluded.lww_wall_ts, lww_op_seq=excluded.lww_op_seq, lww_site_id=excluded.lww_site_id`,
		n.PK, name, sort, created, del, folderID, aspect, n.WallTS, n.OpSeq, n.SiteID)
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

// upsertTextBox materializes a text_box op into fn_text_box, modeled on
// upsertStroke. Returns the row's page_id as the affected page pk (drives
// re-render/re-index). color is read with colInt exactly like stroke.color — it
// arrives as an unsigned ARGB int64 on the wire and is stored verbatim; the
// renderer reinterprets it, the same path strokes use today. text/font_name are
// strings; deleted_at is the nullable tombstone column.
func upsertTextBox(ctx context.Context, tx *sql.Tx, n Op) (pageID string, err error) {
	pageID, err = colString(n, "page_id")
	if err != nil {
		return "", err
	}
	x, err := colInt(n, "x")
	if err != nil {
		return "", err
	}
	y, err := colInt(n, "y")
	if err != nil {
		return "", err
	}
	width, err := colInt(n, "width")
	if err != nil {
		return "", err
	}
	height, err := colInt(n, "height")
	if err != nil {
		return "", err
	}
	text, err := colString(n, "text")
	if err != nil {
		return "", err
	}
	fontName, err := colString(n, "font_name")
	if err != nil {
		return "", err
	}
	fontSize, err := colInt(n, "font_size")
	if err != nil {
		return "", err
	}
	color, err := colInt(n, "color")
	if err != nil {
		return "", err
	}
	weight, err := colInt(n, "weight")
	if err != nil {
		return "", err
	}
	borderWidth, err := colInt(n, "border_width")
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
		`INSERT INTO fn_text_box (id, page_id, x, y, width, height, text, font_name, font_size, color, weight, border_width, z, created_at, deleted_at, lww_wall_ts, lww_op_seq, lww_site_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET page_id=excluded.page_id, x=excluded.x, y=excluded.y,
		   width=excluded.width, height=excluded.height, text=excluded.text, font_name=excluded.font_name,
		   font_size=excluded.font_size, color=excluded.color, weight=excluded.weight,
		   border_width=excluded.border_width, z=excluded.z, created_at=excluded.created_at, deleted_at=excluded.deleted_at,
		   lww_wall_ts=excluded.lww_wall_ts, lww_op_seq=excluded.lww_op_seq, lww_site_id=excluded.lww_site_id`,
		n.PK, pageID, x, y, width, height, text, fontName, fontSize, color, weight, borderWidth, z, created, del, n.WallTS, n.OpSeq, n.SiteID)
	return pageID, err
}

// upsertPageText materializes a page_text_from_server / page_text_from_client op into
// the matching fn_ table (named by n.Table — both have the identical 5-column shape).
// pk == the page ULID, so this is a 1:1 per-page row that re-OCR re-authors in place.
// model is the nullable recognizer column; deleted_at is the nullable tombstone.
func upsertPageText(ctx context.Context, tx *sql.Tx, n Op) error {
	text, err := colString(n, "text")
	if err != nil {
		return err
	}
	ocrAt, err := colInt(n, "ocr_at")
	if err != nil {
		return err
	}
	model, err := colNullString(n, "model")
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
	_, err = tx.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO fn_%s (id, text, ocr_at, model, created_at, deleted_at, lww_wall_ts, lww_op_seq, lww_site_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET text=excluded.text, ocr_at=excluded.ocr_at, model=excluded.model,
		   created_at=excluded.created_at, deleted_at=excluded.deleted_at,
		   lww_wall_ts=excluded.lww_wall_ts, lww_op_seq=excluded.lww_op_seq, lww_site_id=excluded.lww_site_id`, n.Table),
		n.PK, text, ocrAt, model, created, del, n.WallTS, n.OpSeq, n.SiteID)
	return err
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
// deviceName is the optional label from the request envelope: a non-empty name
// refreshes the stored one (so client renames propagate), while empty preserves
// it — an old client that never sends the field must not erase a known name.
func (s *Store) RecordCursor(ctx context.Context, siteID string, lastPullSeq int64, deviceName string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sync_cursors (site_id, last_pull_seq, updated_at, device_name) VALUES (?, ?, ?, ?)
		 ON CONFLICT(site_id) DO UPDATE SET
			last_pull_seq = excluded.last_pull_seq,
			updated_at    = excluded.updated_at,
			device_name   = CASE WHEN excluded.device_name <> '' THEN excluded.device_name
			                     ELSE sync_cursors.device_name END`,
		siteID, lastPullSeq, time.Now().UnixMilli(), deviceName)
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
