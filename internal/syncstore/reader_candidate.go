package syncstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/jdkruzr/rhizome/server-go/bounded"
	"github.com/sysop/ultrabridge/internal/readercontract"
	"github.com/sysop/ultrabridge/internal/readerstore"
)

// ExchangeReaderCandidate is opt-in, for the disposable integration harness.
// The host must Install reader storage and bind verifiedSite to credentials. No
// production route, migration or accepted hash calls/enables this method.
func (s *Store) ExchangeReaderCandidate(ctx context.Context, req bounded.Request, limits bounded.Limits, verifiedSite string) ([]byte, []TablePK, error) {
	if req.ProtocolVersion != 1 || req.SchemaHash != readercontract.CandidateCombined().SchemaHash() {
		return nil, nil, bounded.Fail(409, "reader_candidate_schema_required")
	}
	if !IsULID(verifiedSite) || req.SiteID != verifiedSite {
		return nil, nil, bounded.Fail(403, "site_binding_mismatch")
	}
	if req.Cursor < 0 {
		return nil, nil, bounded.Fail(400, "bad_cursor")
	}
	if err := limits.Validate(); err != nil {
		return nil, nil, err
	}
	if err := req.ValidateRows(bounded.Defaults()); err != nil {
		return nil, nil, err
	}
	tables := readercontract.Registry().ByName()
	var readerRaw [][]byte
	var writerOps []Op
	var entries []readerstore.RelayEntry
	total := 0
	for _, raw := range req.Ops {
		total += len(raw)
		if total > readerstore.MaxBatchBytes {
			return nil, nil, bounded.Fail(413, "request_body_too_large")
		}
		var identity readercontract.WireOp
		if err := json.Unmarshal(raw, &identity); err != nil || identity.OpSeq <= 0 || identity.OpTS < 0 {
			return nil, nil, bounded.Fail(400, "invalid_op")
		}
		if identity.SiteID != verifiedSite {
			return nil, nil, bounded.Fail(403, "site_binding_mismatch")
		}
		if _, ok := tables[identity.Table]; ok {
			readerRaw = append(readerRaw, raw)
		} else {
			var op Op
			if err := json.Unmarshal(raw, &op); err != nil {
				return nil, nil, bounded.Fail(400, "invalid_op")
			}
			op = withV5Defaults(op)
			payload, err := json.Marshal(op)
			if err != nil {
				return nil, nil, err
			}
			writerOps = append(writerOps, op)
			entries = append(entries, readerstore.RelayEntry{Table: op.Table, PK: op.PK, SiteID: op.SiteID, OpSeq: op.OpSeq, OpTS: op.WallTS, Payload: string(payload)})
		}
	}
	p, err := readerstore.Prepare(verifiedSite, readerRaw)
	if err != nil {
		if errors.Is(err, readerstore.ErrBudget) {
			return nil, nil, bounded.Fail(413, "reader_budget_exceeded")
		}
		return nil, nil, bounded.Fail(400, "invalid_reader_envelope")
	}
	readerEntries := p.RelayEntries()
	entries = append(entries, readerEntries...)
	// Canonical identity comparisons are prepared before acquiring the writer.
	seen := map[int64]string{}
	for _, e := range entries {
		payload, err := canonicalPayload(e.Payload)
		if err != nil {
			return nil, nil, err
		}
		if old, ok := seen[e.OpSeq]; ok && old != payload {
			return nil, nil, bounded.Fail(409, "operation_identity_reused")
		}
		seen[e.OpSeq] = payload
	}
	extra := &batchExtension{
		preservePayload: func(table string) bool { _, ok := tables[table]; return ok },
		receipt: func(ctx context.Context, tx *sql.Tx, site string, seq int64) (bool, error) {
			var found int
			err := tx.QueryRowContext(ctx, `SELECT 1 FROM reader_store_incoming WHERE site_id=? AND op_seq=?`, site, seq).Scan(&found)
			if err == sql.ErrNoRows {
				return false, nil
			}
			return err == nil, err
		},
	}
	extra.stage = func(ctx context.Context, tx *sql.Tx, now int64) (int64, []RejectedOp, error) {
		// Check collisions across BOTH halves before generic writer dedup can hide
		// one. Never materialize an oversized historical relay payload.
		for _, e := range entries {
			var old sql.NullString
			err := tx.QueryRowContext(ctx, `SELECT CASE WHEN length(CAST(payload AS BLOB))<=? THEN payload ELSE NULL END FROM sync_ops WHERE site_id=? AND op_seq=?`, readerstore.MaxRowBytes, e.SiteID, e.OpSeq).Scan(&old)
			if err == nil {
				if !old.Valid {
					return 0, nil, bounded.Fail(413, "historical_identity_too_large")
				}
				// Both paths persist deterministic payloads: canonical JSON for
				// readers, the existing Op codec for writers. Do not decode ink
				// strings again while holding the writer just to compare identity.
				if old.String != e.Payload {
					return 0, nil, bounded.Fail(409, "operation_identity_reused")
				}
			} else if err != sql.ErrNoRows {
				return 0, nil, err
			}
			if _, reader := tables[e.Table]; !reader {
				found, err := extra.receipt(ctx, tx, e.SiteID, e.OpSeq)
				if err != nil {
					return 0, nil, err
				}
				if found {
					return 0, nil, bounded.Fail(409, "operation_identity_reused")
				}
			}
		}
		if err := p.CommitTx(ctx, tx); err != nil {
			if errors.Is(err, readerstore.ErrIdentityReused) {
				return 0, nil, bounded.Fail(409, "operation_identity_reused")
			}
			return 0, nil, err
		}
		var maxTS int64
		var rejected []RejectedOp
		for _, e := range readerEntries {
			if e.OpTS > maxTS {
				maxTS = e.OpTS
			}
			if e.Reason != "" {
				rejected = append(rejected, RejectedOp{SiteID: e.SiteID, OpSeq: e.OpSeq, Reason: e.Reason})
				continue
			}
			var found int
			err := tx.QueryRowContext(ctx, `SELECT 1 FROM sync_ops WHERE site_id=? AND op_seq=?`, e.SiteID, e.OpSeq).Scan(&found)
			if err == nil {
				continue
			}
			if err != sql.ErrNoRows {
				return 0, nil, err
			}
			op := Op{Table: e.Table, PK: e.PK, SiteID: e.SiteID, OpSeq: e.OpSeq, WallTS: e.OpTS}
			if err := appendPayload(ctx, tx, op, e.Payload, now); err != nil {
				return 0, nil, err
			}
		}
		return maxTS, rejected, nil
	}
	return s.exchangeBounded(ctx, req, limits, writerOps, extra)
}

func canonicalPayload(payload string) (string, error) {
	decoder := json.NewDecoder(bytes.NewBufferString(payload))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	b, err := json.Marshal(value)
	return string(b), err
}
