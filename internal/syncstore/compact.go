package syncstore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jdkruzr/rhizome/server-go/compaction"
	"github.com/jdkruzr/rhizome/server-go/registry"
	rzsync "github.com/jdkruzr/rhizome/server-go/syncstore"
)

// fnTombstoneCols maps each ForestNote table to its tombstone column (every table soft-deletes via
// deleted_at), derived from the RhizomeSync registry. It is handed to compaction.Sweep so a delete op
// becomes reclaimable once every active device has pulled past it. Deriving it from the registry (not
// a local literal) keeps it in lockstep with the same declaration the schema hash and Less come from.
var fnTombstoneCols = func() map[string]string {
	out := map[string]string{}
	for _, t := range registry.ForestNote().Tables {
		if t.Tombstone != "" {
			out[t.Name] = t.Tombstone
		}
	}
	return out
}()

// TombstoneCols returns a copy of the table→tombstone-column map for the ForestNote schema. The
// source adapter passes it to Compact; a copy keeps the package-internal map immutable to callers.
func TombstoneCols() map[string]string {
	out := make(map[string]string, len(fnTombstoneCols))
	for k, v := range fnTombstoneCols {
		out[k] = v
	}
	return out
}

// Compact reclaims the durable sync_ops relay log in ONE transaction, applying the RhizomeSync
// library's pure compaction.Sweep decision — the SAME algorithm the in-memory reference store runs,
// so UB's reclamation can never drift from the library's. It loads the log in seq order, asks Sweep
// which entries to keep, and DELETEs the dropped seqs. Two invariants, both from spec/compaction.md:
//
//   - seq is NEVER renumbered (rule 3): surviving rows keep their original seq; holes are fine. The
//     global high-water sync_seq.last_seq stands, so cursors stay meaningful.
//   - sync_cursors / sync_seq are NEVER touched: a device's pull cursor and the high-water are
//     untouched, so a fresh cursor=0 pull still reconstructs exact current state from the surviving
//     LWW winners (the mirror itself is also untouched — compaction reclaims only the relay LOG).
//
// watermark gates tombstone purge (see ComputeWatermark); pass 0 to collapse superseded versions
// only and keep every tombstone. tombstoneCols is TombstoneCols().
//
// Single-tx safety: the notedb is MaxOpenConns=1, so no ApplyBatch/AuthorOps can interleave with this
// transaction; any op appended after it commits carries a seq greater than everything loaded here and
// is therefore untouched. Divergence from the in-memory reference (documented, accepted — plan §D4):
// UB retains no `seen` set beyond sync_ops, so a purged op re-delivered later re-appends at a NEW seq.
// That is harmless — the mirror already holds the materialized (winning/tombstoned) row, so the
// re-append converges by LWW and the next sweep reclaims it again.
func (s *Store) Compact(ctx context.Context, tombstoneCols map[string]string, watermark int64) (compaction.Stats, error) {
	var stats compaction.Stats
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return stats, fmt.Errorf("compact begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT seq, site_id, op_seq, table_name, pk, wall_ts, payload FROM sync_ops ORDER BY seq`)
	if err != nil {
		return stats, fmt.Errorf("compact load: %w", err)
	}
	var entries []compaction.Entry
	for rows.Next() {
		var (
			seq, opSeq, wallTS         int64
			siteID, table, pk, payload string
		)
		if err := rows.Scan(&seq, &siteID, &opSeq, &table, &pk, &wallTS, &payload); err != nil {
			rows.Close()
			return stats, fmt.Errorf("compact scan: %w", err)
		}
		// Cols come from the stored payload (Sweep needs them only for tombstone detection); the LWW
		// key fields come from the indexed columns, which are authoritative and immune to the legacy
		// wall_ts→op_ts payload-key rename — a pre-cutover payload would otherwise decode op_ts=0 and
		// lose every conflict during the sweep.
		var op rzsync.Op
		if err := json.Unmarshal([]byte(payload), &op); err != nil {
			rows.Close()
			return stats, fmt.Errorf("compact unmarshal seq %d: %w", seq, err)
		}
		op.Table, op.PK, op.SiteID, op.OpSeq, op.OpTs = table, pk, siteID, opSeq, wallTS
		entries = append(entries, compaction.Entry{Seq: seq, Op: op})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return stats, fmt.Errorf("compact iterate: %w", err)
	}
	rows.Close() // must fully drain + close before issuing DELETEs on the same single-conn tx

	kept, st := compaction.Sweep(entries, tombstoneCols, watermark)
	stats = st
	if st.CollapsedSuperseded == 0 && st.PurgedTombstones == 0 {
		return stats, nil // nothing reclaimed → skip the delete + commit churn
	}

	keptSeqs := make(map[int64]struct{}, len(kept))
	for _, e := range kept {
		keptSeqs[e.Seq] = struct{}{}
	}
	for _, e := range entries {
		if _, ok := keptSeqs[e.Seq]; ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sync_ops WHERE seq = ?`, e.Seq); err != nil {
			return stats, fmt.Errorf("compact delete seq %d: %w", e.Seq, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return stats, fmt.Errorf("compact commit: %w", err)
	}
	return stats, nil
}

// ComputeWatermark derives the tombstone-purge watermark from sync_cursors via the RhizomeSync
// library's compaction.Watermark: the minimum last_pull_seq over every device NOT evicted as stale. A
// tombstone at seq <= watermark is safe to purge because every active device has already pulled past
// it; a device unseen for STRICTLY longer than staleHorizonMs is evicted from the min (and, if it ever
// returns, re-pulls correctly from cursor=0), so one dead device can't pin the watermark forever. The
// evicted site ids are returned for the caller to log — eviction is never silent. With no active
// device the watermark is 0, so nothing is purged (safe: there is no reader to protect).
//
// nowMs is injected (not read from the clock here) so the staleness boundary is deterministic in
// tests. The watermark is read-only: it inspects sync_cursors and mutates nothing.
func (s *Store) ComputeWatermark(ctx context.Context, nowMs, staleHorizonMs int64) (int64, []string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT site_id, last_pull_seq, updated_at FROM sync_cursors`)
	if err != nil {
		return 0, nil, fmt.Errorf("watermark load cursors: %w", err)
	}
	defer rows.Close()
	sites := map[string]compaction.Site{}
	for rows.Next() {
		var id string
		var lastPull, updated int64
		if err := rows.Scan(&id, &lastPull, &updated); err != nil {
			return 0, nil, fmt.Errorf("watermark scan: %w", err)
		}
		sites[id] = compaction.Site{LastPullSeq: lastPull, LastSeenUnixMs: updated}
	}
	if err := rows.Err(); err != nil {
		return 0, nil, fmt.Errorf("watermark iterate: %w", err)
	}
	watermark, evicted := compaction.Watermark(sites, nowMs, staleHorizonMs)
	return watermark, evicted, nil
}
