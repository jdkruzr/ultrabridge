package readerstore

import (
	"context"
	"fmt"
)

// Change is a durable invalidation, not a snapshot of the row at Seq. Consumers
// read current state, handle book/session/reference fan-out in bounded pages,
// and durably schedule idempotent work before returning success.
type Change struct {
	Seq int64
	Key Key
}

func (s *Store) incomingHead(ctx context.Context) (int64, error) {
	var head int64
	err := s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq),0) FROM reader_store_incoming").Scan(&head)
	return head, err
}

// Changes returns one detached key-only page after the downstream checkpoint.
// No transaction/DB connection remains held while a consumer runs.
func (s *Store) Changes(ctx context.Context, limit int) ([]Change, error) {
	if limit < 1 || limit > 128 {
		return nil, ErrBudget
	}
	rows, err := s.db.QueryContext(ctx, `SELECT seq,table_name,pk FROM reader_store_changes
	 WHERE seq>(SELECT seq FROM reader_store_change_cursor WHERE id=1) ORDER BY seq LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var changes []Change
	for rows.Next() {
		var c Change
		if err := rows.Scan(&c.Seq, &c.Key.Table, &c.Key.ID); err != nil {
			return nil, err
		}
		changes = append(changes, c)
	}
	return changes, rows.Err()
}

// CompleteChange advances exactly one contiguous journal entry. Replay of an
// already completed entry is safe; an old callback cannot clear newer work.
// Delivery is at-least-once (including crashes after consumer success).
func (s *Store) CompleteChange(ctx context.Context, seq int64) error {
	if seq <= 0 {
		return fmt.Errorf("invalid change sequence")
	}
	tx, err := writer(ctx, s.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var cursor int64
	if err = tx.QueryRowContext(ctx, "SELECT seq FROM reader_store_change_cursor WHERE id=1").Scan(&cursor); err != nil {
		return err
	}
	if seq <= cursor {
		return tx.Commit()
	}
	var next int64
	if err = tx.QueryRowContext(ctx, "SELECT MIN(seq) FROM reader_store_changes WHERE seq>?", cursor).Scan(&next); err != nil {
		return err
	}
	if seq != next {
		return fmt.Errorf("change completion would skip pending work")
	}
	if _, err = tx.ExecContext(ctx, "UPDATE reader_store_change_cursor SET seq=? WHERE id=1", seq); err != nil {
		return err
	}
	return tx.Commit()
}
