package syncstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/jdkruzr/rhizome/server-go/bounded"
)

// ExchangeBounded commits ONLY after the exact encoded response is known to fit.
// No network or pipeline callback holds the writer. Legacy ApplyBatch/OpsSince
// retain their existing public behavior and use the same merge implementation.
func (s *Store) ExchangeBounded(ctx context.Context, req bounded.Request, limits bounded.Limits) ([]byte, []TablePK, error) {
	if err := req.ValidateRows(limits); err != nil {
		return nil, nil, err
	}
	ops := make([]Op, len(req.Ops))
	for i, raw := range req.Ops {
		if err := json.Unmarshal(raw, &ops[i]); err != nil {
			return nil, nil, bounded.Fail(400, "invalid_op")
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	// Acquire writer before reads to avoid WAL read-snapshot upgrade races.
	if _, err := tx.ExecContext(ctx, `UPDATE sync_seq SET last_seq=last_seq WHERE id=1`); err != nil {
		return nil, nil, err
	}
	res, err := s.applyBatchTx(ctx, tx, req.SiteID, ops)
	if err != nil {
		return nil, nil, err
	}
	rejected, err := json.Marshal(res.Rejected)
	if err != nil {
		return nil, nil, err
	}
	page, err := bounded.NewPage(req.Cursor, res.AcceptedThrough, rejected, limits)
	if err != nil {
		return nil, nil, err
	}
	after := req.Cursor
	for {
		// A bounded scalar probe prevents materializing an oversized historical
		// payload (or even loading the payload of the count-limit lookahead).
		cap := limits.MaxRowBytes
		if page.CountFull() {
			cap = 0
		}
		var seq, opSeq, size int64
		var site string
		var payload sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT seq,site_id,op_seq,length(CAST(payload AS BLOB)),
		 CASE WHEN length(CAST(payload AS BLOB))<=? THEN payload ELSE NULL END
		 FROM sync_ops WHERE seq>? AND site_id<>? ORDER BY seq LIMIT 1`, cap, after, req.SiteID).
			Scan(&seq, &site, &opSeq, &size, &payload)
		if err == sql.ErrNoRows {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		if page.CountFull() {
			page.Response.HasMore = true
			break
		}
		var raw []byte
		if payload.Valid {
			var op Op
			if err := json.Unmarshal([]byte(payload.String), &op); err != nil {
				return nil, nil, err
			}
			raw, err = json.Marshal(withV5Defaults(op))
			if err != nil {
				return nil, nil, err
			}
			size = int64(len(raw))
		}
		added, err := page.Add(seq, site, opSeq, size, raw)
		if err != nil {
			return nil, nil, err
		}
		if !added {
			break
		}
		after = seq
	}
	body, err := page.Encode()
	if err != nil {
		return nil, nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sync_cursors SET last_pull_seq=?,updated_at=?,
	 device_name=CASE WHEN ?<>'' THEN ? ELSE device_name END WHERE site_id=?`,
		page.Response.Cursor, time.Now().UnixMilli(), req.DeviceName, req.DeviceName, req.SiteID); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return body, res.ChangedPages, nil
}

// The same grace set used by AcceptsSchemaHash; reader hashes are not guessed.
func AcceptedSchemaHashes() []string { return []string{SchemaHash(), schemaHashV4} }
