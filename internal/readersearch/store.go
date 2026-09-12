package readersearch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sysop/ultrabridge/internal/readerstore"
)

var ErrChanged = errors.New("reader search snapshot changed; retry")

type Store struct {
	db      *sql.DB
	reader  *readerstore.Store
	wake    chan struct{}
	running atomic.Bool
	step    sync.Mutex
}

func New(db *sql.DB) *Store {
	return &Store{db: db, reader: readerstore.New(db), wake: make(chan struct{}, 1)}
}

// Schedule returns only after durable, idempotent handoff. The worker's journal
// checkpoint may then advance; actual indexing has its own restartable job.
func (s *Store) Schedule(ctx context.Context, c readerstore.Change) error {
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO reader_search_jobs(seq,table_name,pk)
	 SELECT seq,table_name,pk FROM reader_store_changes WHERE seq=? AND table_name=? AND pk=?`, c.Seq, c.Key.Table, c.Key.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var found int
		if err = s.db.QueryRowContext(ctx, "SELECT 1 FROM reader_search_jobs WHERE seq=? AND table_name=? AND pk=?", c.Seq, c.Key.Table, c.Key.ID).Scan(&found); err != nil {
			return fmt.Errorf("invalid reader change handoff: %w", err)
		}
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

type job struct {
	seq              int64
	table, pk, after string
}

// Step handles at most limit annotation targets; every target commits its index
// and fan-out cursor atomically. One owner per library, no network/ink work in TX.
func (s *Store) Step(ctx context.Context, limit int) (bool, error) {
	if limit < 1 || limit > 128 {
		return false, readerstore.ErrBudget
	}
	s.step.Lock()
	defer s.step.Unlock()
	var j job
	err := s.db.QueryRowContext(ctx, "SELECT seq,table_name,pk,after_id FROM reader_search_jobs WHERE done=0 AND retry_at<=? ORDER BY seq LIMIT 1", time.Now().UnixMilli()).Scan(&j.seq, &j.table, &j.pk, &j.after)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err = s.process(ctx, j, limit); err != nil {
		delay := int64(1000)
		if errors.Is(err, ErrChanged) {
			delay = 25
		}
		// Failure remains visible/durable. Other ready jobs are not starved by
		// an unsupported or oversized annotation. Cancellation writes nothing.
		if ctx.Err() == nil {
			if _, e := s.db.ExecContext(ctx, "UPDATE reader_search_jobs SET retry_at=?,error_code='projection_failed' WHERE seq=?", time.Now().UnixMilli()+delay, j.seq); e != nil {
				return true, errors.Join(err, e)
			}
		}
		return true, err
	}
	return true, nil
}
func (s *Store) process(ctx context.Context, j job, limit int) error {
	condition := affects(j.table, "?")
	args := []any{j.pk, j.after, limit}
	if condition == "0" {
		args = []any{j.after, limit}
	}
	rows, err := s.db.QueryContext(ctx, "SELECT a.id FROM fn_reader_annotation a WHERE "+condition+" AND a.id>? ORDER BY a.id LIMIT ?", args...)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		input, err := s.reader.SearchSnapshot(ctx, id, readerstore.DefaultLimits())
		if err != nil {
			return err
		}
		doc, err := project(input)
		if err != nil {
			return err
		}
		if err = s.publish(ctx, j.seq, id, input.Revision, doc); err != nil {
			return err
		}
	}
	if len(ids) < limit {
		_, err = s.db.ExecContext(ctx, "UPDATE reader_search_jobs SET done=1,error_code='',retry_at=0 WHERE seq=?", j.seq)
	}
	return err
}
func (s *Store) publish(ctx context.Context, seq int64, id string, revision int64, d *document) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE reader_search_version SET version=version WHERE id=1"); err != nil {
		return err
	}
	var changed int
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM fn_reader_annotation a WHERE a.id=? AND "+dirty("?")+")", id, revision).Scan(&changed); err != nil {
		return err
	}
	if changed != 0 {
		return ErrChanged
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM reader_search_fts WHERE rowid IN(SELECT id FROM reader_search_documents WHERE annotation_id=?)", id); err != nil {
		return err
	}
	if d == nil {
		if _, err = tx.ExecContext(ctx, "DELETE FROM reader_search_documents WHERE annotation_id=?", id); err != nil {
			return err
		}
	} else {
		var rowid int64
		err = tx.QueryRowContext(ctx, `INSERT INTO reader_search_documents(annotation_id,book_id,title,quote,recognized_text,anchor,input_hash,alternatives,revision) VALUES(?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(annotation_id) DO UPDATE SET book_id=excluded.book_id,title=excluded.title,quote=excluded.quote,recognized_text=excluded.recognized_text,anchor=excluded.anchor,input_hash=excluded.input_hash,alternatives=excluded.alternatives,revision=excluded.revision RETURNING id`, id, d.book, d.title, d.quote, d.text, d.anchor, d.hash, d.alternatives, revision).Scan(&rowid)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO reader_search_fts(rowid,title,quote,recognized_text) VALUES(?,?,?,?)", rowid, d.title, d.quote, d.text); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE reader_search_jobs SET after_id=?,error_code='',retry_at=0 WHERE seq=?", id, seq); err != nil {
		return err
	}
	return tx.Commit()
}

// Run owns one background index loop; transient/error jobs retain their cursor.
// Cancel and join before closing DB. onError must be quick and payload-free.
func (s *Store) Run(ctx context.Context, onError func(error)) error {
	if !s.running.CompareAndSwap(false, true) {
		return fmt.Errorf("reader search already running")
	}
	defer s.running.Store(false)
	for ctx.Err() == nil {
		more, err := s.Step(ctx, 32)
		delay := 10 * time.Millisecond
		var wake <-chan struct{}
		if !more {
			delay = 100 * time.Millisecond
			wake = s.wake
		}
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			if onError != nil {
				onError(err)
			}
			delay = 100 * time.Millisecond
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		case <-wake:
			t.Stop()
		}
	}
	return ctx.Err()
}
