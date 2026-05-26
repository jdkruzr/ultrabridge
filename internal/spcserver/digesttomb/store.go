// Package digesttomb owns the durable per-device digest tombstone queue: when a
// digest is deleted server-side (e.g. from UB's web UI), the device must be told
// to drop its local copy via a DELETE_DIGEST socket push — but the device may be
// offline. The real SPC server queues these per socket-session in Redis and
// drains them on the device's ratta_ping heartbeat (lost on reconnect). UB keeps
// them in SQLite keyed per device-user, so they survive reconnects and restarts:
// a delete enqueues a row; each ratta_ping drains the user's pending rows (marks
// them sent); the device's "Received" ack deletes the sent rows; a TTL sweep
// reclaims any a never-returning device left behind.
//
// The table is migrated by this package (from main, in server mode), not by
// notedb.Open — same gating precedent as fileids/staging.
package digesttomb

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Tombstone is one pending "this digest is deleted" notice for a device-user.
type Tombstone struct {
	UserID   int64
	DigestID int64
	DataType string // sourceType as a string: "1"=PDF, "2"=note
}

// Store is the tombstone queue over the shared notedb handle.
type Store struct {
	db  *sql.DB
	now func() int64 // ms-epoch, injectable for tests
}

// New returns a Store backed by db (the shared notedb pool).
func New(db *sql.DB) *Store {
	return &Store{db: db, now: func() int64 { return time.Now().UnixMilli() }}
}

// Migrate creates the spc_digest_tombstones table idempotently. The (user_id,
// digest_id) pair is unique so re-deleting the same digest is a no-op. sent_at=0
// means not-yet-delivered; >0 means emitted on a ping drain (awaiting ack).
func Migrate(ctx context.Context, db *sql.DB) error {
	const stmt = `CREATE TABLE IF NOT EXISTS spc_digest_tombstones (
		user_id    INTEGER NOT NULL,
		digest_id  INTEGER NOT NULL,
		data_type  TEXT NOT NULL DEFAULT '2',
		created_at INTEGER NOT NULL DEFAULT 0,
		sent_at    INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (user_id, digest_id)
	)`
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("digesttomb migration: %w", err)
	}
	return nil
}

// Enqueue records a pending tombstone for (userID, digestID). Idempotent: a
// repeat for the same pair leaves the existing row (and its sent state) intact.
func (s *Store) Enqueue(ctx context.Context, userID, digestID int64, dataType string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO spc_digest_tombstones (user_id, digest_id, data_type, created_at, sent_at)
		 VALUES (?,?,?,?,0)
		 ON CONFLICT(user_id, digest_id) DO NOTHING`,
		userID, digestID, dataType, s.now())
	if err != nil {
		return fmt.Errorf("digesttomb Enqueue: %w", err)
	}
	return nil
}

// Pending returns all tombstones queued for userID and marks them sent (so the
// caller can emit them to the device). Re-draining before an ack re-returns them
// (redelivery is harmless — DELETE_DIGEST is idempotent on the device).
func (s *Store) Pending(ctx context.Context, userID int64) ([]Tombstone, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT digest_id, data_type FROM spc_digest_tombstones WHERE user_id=? ORDER BY created_at, digest_id`, userID)
	if err != nil {
		return nil, fmt.Errorf("digesttomb Pending: %w", err)
	}
	defer rows.Close()
	var out []Tombstone
	for rows.Next() {
		tb := Tombstone{UserID: userID}
		if err := rows.Scan(&tb.DigestID, &tb.DataType); err != nil {
			return nil, fmt.Errorf("digesttomb Pending scan: %w", err)
		}
		out = append(out, tb)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("digesttomb Pending rows: %w", err)
	}
	if len(out) > 0 {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE spc_digest_tombstones SET sent_at=? WHERE user_id=? AND sent_at=0`, s.now(), userID); err != nil {
			return nil, fmt.Errorf("digesttomb Pending mark-sent: %w", err)
		}
	}
	return out, nil
}

// Ack deletes the tombstones already delivered to userID (the device confirmed
// receipt). Rows enqueued after the last drain (sent_at=0) are kept for the next
// drain.
func (s *Store) Ack(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM spc_digest_tombstones WHERE user_id=? AND sent_at>0`, userID)
	if err != nil {
		return fmt.Errorf("digesttomb Ack: %w", err)
	}
	return nil
}

// Sweep deletes tombstones created before the given ms-epoch cutoff (a device
// that never returned). Returns the number removed.
func (s *Store) Sweep(ctx context.Context, before int64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM spc_digest_tombstones WHERE created_at < ?`, before)
	if err != nil {
		return 0, fmt.Errorf("digesttomb Sweep: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
