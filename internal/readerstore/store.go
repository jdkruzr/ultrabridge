package readerstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	merge "github.com/jdkruzr/rhizome/server-go/syncstore"
	"github.com/sysop/ultrabridge/internal/readercontract"
)

const MaxRowBytes = 8 * 1024 * 1024
const MaxBatchBytes = 16 * 1024 * 1024

var ErrBudget = errors.New("reader storage budget exceeded; no partial result")
var ErrIdentityReused = errors.New("incoming operation identity reused")

type Store struct{ db *sql.DB }

func New(db *sql.DB) *Store { return &Store{db: db} }

type preparedRow struct {
	op            readercontract.WireOp
	payload       string
	state, reason string
}

// Prepared owns canonical immutable payload copies, never the caller's slices/maps.
type Prepared struct{ rows []preparedRow }

// RelayEntry is immutable metadata for the host's existing relay transaction.
// Reason is nonempty only for a shape-invalid row: retain its receipt but do not
// relay it. Valid pending rows still need the dependency/ownership gate in Drain.
type RelayEntry struct {
	Table, PK, SiteID, Payload, Reason string
	OpSeq, OpTS                        int64
}

func (p *Prepared) RelayEntries() []RelayEntry {
	out := make([]RelayEntry, len(p.rows))
	for i, r := range p.rows {
		out[i] = RelayEntry{r.op.Table, r.op.PK, r.op.SiteID, r.payload, r.reason, r.op.OpSeq, r.op.OpTS}
	}
	return out
}

// Prepare runs decoding, canonicalization and full ink validation OFF the DB writer.
// verifiedSite must be supplied by host credential binding, not request.site_id.
// Invalid envelopes/identity/auth reject the batch; malformed domain rows with a
// usable identity are retained as quarantined diagnostic records.
func Prepare(verifiedSite string, rawOps [][]byte) (*Prepared, error) {
	if err := readercontract.RequireClientAuthor(readercontract.WireOp{SiteID: verifiedSite}, verifiedSite); err != nil {
		return nil, err
	}
	if len(rawOps) > 500 {
		return nil, ErrBudget
	}
	p := &Prepared{}
	size := 0
	canonicalSize := 0
	for _, raw := range rawOps {
		size += len(raw)
		if len(raw) > MaxRowBytes || size > MaxBatchBytes {
			return nil, ErrBudget
		}
		op, _, validation := readercontract.DecodeJSON(raw)
		if _, ok := definitions[op.Table]; !ok {
			return nil, fmt.Errorf("unknown reader table")
		}
		if err := readercontract.RequireClientAuthor(op, verifiedSite); err != nil {
			return nil, err
		}
		if op.OpSeq <= 0 || op.OpTS < 0 || len(op.PK) > 2048 {
			return nil, fmt.Errorf("invalid operation identity")
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return nil, err
		}
		if len(object) != 6 {
			return nil, fmt.Errorf("invalid envelope")
		}
		for _, key := range []string{"table", "pk", "site_id", "op_ts", "op_seq", "cols"} {
			v, ok := object[key]
			if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
				return nil, fmt.Errorf("missing envelope field %s", key)
			}
		}
		// Canonicalize only the outer JSON; strings containing selectors remain exact.
		var value any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		canonical, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		// Invalid strings must not be silently repaired by Go JSON transcoding;
		// retain the original invalid payload for quarantine diagnostics/retries.
		if validation != nil {
			canonical = raw
		}
		canonicalSize += len(canonical)
		if len(canonical) > MaxRowBytes || canonicalSize > MaxBatchBytes {
			return nil, ErrBudget
		}
		op.Cols = nil // Only envelope identity is needed after preparation.
		row := preparedRow{op: op, payload: string(canonical), state: "pending"}
		if validation != nil {
			row.state = "quarantined"
			row.reason = "Invalid reader row"
		}
		p.rows = append(p.rows, row)
	}
	return p, nil
}

// CommitTx participates in a HOST-OWNED transaction. Caller MUST roll back on any
// error. It neither commits nor emits ACKs nor writes a second relay/clock/cursor.
// HTTP integration must use this alongside the existing relay/ACK transaction.
// Acquire the host writer before this read-before-write hook to avoid WAL upgrades.
func (p *Prepared) CommitTx(ctx context.Context, tx *sql.Tx) error {
	for _, r := range p.rows {
		var old sql.NullString
		err := tx.QueryRowContext(ctx, "SELECT CASE WHEN length(CAST(payload AS BLOB))<=? THEN payload ELSE NULL END FROM reader_store_incoming WHERE site_id=? AND op_seq=?", MaxRowBytes, r.op.SiteID, r.op.OpSeq).Scan(&old)
		if err == nil {
			if !old.Valid {
				return ErrBudget
			}
			if old.String != r.payload {
				return ErrIdentityReused
			}
			continue
		}
		if err != sql.ErrNoRows {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO reader_store_incoming(site_id,op_seq,payload,state,reason) VALUES(?,?,?,?,?)", r.op.SiteID, r.op.OpSeq, r.payload, r.state, r.reason); err != nil {
			return err
		}
	}
	return nil
}
func writer(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE reader_store_version SET version=version WHERE id=1"); err != nil {
		tx.Rollback()
		return nil, err
	}
	return tx, nil
}
func (s *Store) Stage(ctx context.Context, p *Prepared) error {
	tx, err := writer(ctx, s.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = p.CommitTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

type Key struct{ Table, ID string }
type Receipt struct {
	Seq           int64
	State, Reason string
}
type DrainResult struct {
	Records []Receipt
	Changed []Key
	Next    int64
}
type queued struct {
	seq     int64
	payload string
}

// Drain processes one bounded keyset page. Resume at Next; when zero, end the
// sweep. Begin a fresh sweep at zero after dependencies arrive. No busy retry loop.
func (s *Store) Drain(ctx context.Context, after int64, limit int) (DrainResult, error) {
	var result DrainResult
	if after < 0 || limit < 1 || limit > 128 {
		return result, ErrBudget
	}
	rows, err := s.db.QueryContext(ctx, "SELECT seq,length(CAST(payload AS BLOB)),CASE WHEN length(CAST(payload AS BLOB))<=? THEN payload ELSE NULL END FROM reader_store_incoming WHERE state='pending' AND seq>? ORDER BY seq LIMIT ?", MaxRowBytes, after, limit+1)
	if err != nil {
		return result, err
	}
	queue := []queued{}
	size := int64(0)
	for rows.Next() {
		var seq, n int64
		var payload sql.NullString
		if err = rows.Scan(&seq, &n, &payload); err != nil {
			break
		}
		if len(queue) == limit || size+n > MaxBatchBytes {
			if len(queue) == 0 {
				err = ErrBudget
			} else {
				result.Next = queue[len(queue)-1].seq
			}
			break
		}
		if !payload.Valid {
			err = ErrBudget
			break
		}
		queue = append(queue, queued{seq, payload.String})
		size += n
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return DrainResult{}, err
	}
	type decoded struct {
		q   queued
		op  readercontract.WireOp
		row readercontract.Record
	}
	prepared := make([]decoded, 0, len(queue))
	for _, q := range queue {
		op, r, e := readercontract.DecodeJSON([]byte(q.payload))
		if e != nil {
			return DrainResult{}, fmt.Errorf("corrupt pending row: %w", e)
		}
		prepared = append(prepared, decoded{q, op, r})
	}
	tx, err := writer(ctx, s.db)
	if err != nil {
		return DrainResult{}, err
	}
	defer tx.Rollback()
	budget := budget{rows: 1024, bytes: MaxBatchBytes}
	cache := map[Key]*readercontract.Record{}
	var lookupErr error
	lookup := func(table, id string) *readercontract.Record {
		key := Key{table, id}
		if r, ok := cache[key]; ok {
			return r
		}
		r, e := load(ctx, tx, table, id, &budget)
		if e != nil {
			lookupErr = e
		}
		cache[key] = r
		return r
	}
	for _, p := range prepared {
		var state string
		if err = tx.QueryRowContext(ctx, "SELECT state FROM reader_store_incoming WHERE seq=?", p.q.seq).Scan(&state); err != nil {
			return DrainResult{}, err
		}
		if state != "pending" {
			continue
		}
		decision := readercontract.CheckPrepared(p.op.Table, p.row, lookup)
		if lookupErr != nil {
			return DrainResult{}, lookupErr
		}
		if decision.State == "applied" {
			old := lookup(p.op.Table, p.op.PK)
			if lookupErr != nil {
				return DrainResult{}, lookupErr
			}
			version := func(v *readercontract.Version) merge.Op {
				return merge.Op{SiteID: v.SiteID, OpSeq: v.OpSeq, OpTs: v.OpTS}
			}
			if old == nil || old.Version == nil || merge.Less(version(old.Version), version(p.row.Version)) {
				if err = upsert(ctx, tx, p.op.Table, p.row); err != nil {
					return DrainResult{}, err
				}
				key := Key{p.op.Table, p.op.PK}
				r := p.row
				cache[key] = &r
				result.Changed = append(result.Changed, key)
				if _, err = tx.ExecContext(ctx, "INSERT INTO reader_store_changes(table_name,pk) VALUES(?,?)", key.Table, key.ID); err != nil {
					return DrainResult{}, err
				}
			}
		}
		if _, err = tx.ExecContext(ctx, "UPDATE reader_store_incoming SET state=?,reason=? WHERE seq=?", decision.State, decision.Reason, p.q.seq); err != nil {
			return DrainResult{}, err
		}
		result.Records = append(result.Records, Receipt{p.q.seq, decision.State, decision.Reason})
	}
	if err = tx.Commit(); err != nil {
		return DrainResult{}, err
	}
	return result, nil
}

func upsert(ctx context.Context, tx *sql.Tx, table string, r readercontract.Record) error {
	cols := columns(table)
	names := []string{}
	marks := []string{}
	updates := []string{}
	args := []any{r.ID}
	for _, c := range definitions[table].Columns {
		args = append(args, r.Columns[c.Name])
	}
	args = append(args, r.Version.OpTS, r.Version.OpSeq, r.Version.SiteID)
	for _, c := range cols {
		names = append(names, c.name)
		marks = append(marks, "?")
		if c.name != "id" {
			updates = append(updates, c.name+"=excluded."+c.name)
		}
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO fn_"+table+"("+strings.Join(names, ",")+") VALUES("+strings.Join(marks, ",")+") ON CONFLICT(id) DO UPDATE SET "+strings.Join(updates, ","), args...)
	return err
}
