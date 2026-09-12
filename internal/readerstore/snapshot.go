package readerstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/sysop/ultrabridge/internal/readercontract"
)

type budget struct {
	rows  int
	bytes int64
}

func load(ctx context.Context, tx *sql.Tx, table, id string, b *budget) (*readercontract.Record, error) {
	if _, ok := definitions[table]; !ok {
		return nil, fmt.Errorf("unknown reader table")
	}
	cols := columns(table)
	sizes := []string{}
	names := []string{}
	for _, c := range cols {
		sizes = append(sizes, "COALESCE(length(CAST("+c.name+" AS BLOB)),0)")
		names = append(names, c.name)
	}
	var n int64
	err := tx.QueryRowContext(ctx, "SELECT "+strings.Join(sizes, "+")+" FROM fn_"+table+" WHERE id=?", id).Scan(&n)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if b.rows <= 0 || n > b.bytes || n > MaxRowBytes {
		return nil, ErrBudget
	}
	b.rows--
	b.bytes -= n
	values := make([]any, len(cols))
	pointers := make([]any, len(cols))
	for i := range values {
		pointers[i] = &values[i]
	}
	if err = tx.QueryRowContext(ctx, "SELECT "+strings.Join(names, ",")+" FROM fn_"+table+" WHERE id=?", id).Scan(pointers...); err != nil {
		return nil, err
	}
	r := &readercontract.Record{ID: id, Columns: map[string]any{}}
	for i, c := range cols {
		v := values[i]
		if v == nil {
			if c.required != 0 {
				return nil, fmt.Errorf("null required stored column")
			}
		} else {
			ok := false
			switch c.affinity {
			case "TEXT":
				_, ok = v.(string)
			case "BLOB":
				_, ok = v.([]byte)
			case "INTEGER":
				_, ok = v.(int64)
			}
			if !ok {
				return nil, fmt.Errorf("invalid stored type for %s.%s", table, c.name)
			}
		}
		if i > 0 && i < len(cols)-3 {
			r.Columns[c.name] = v
		}
	}
	i := len(values) - 3
	r.Version = &readercontract.Version{OpTS: values[i].(int64), OpSeq: values[i+1].(int64), SiteID: values[i+2].(string)}
	return r, nil
}

type Limits struct {
	Rows  int
	Bytes int64
}

func DefaultLimits() Limits { return Limits{4096, 16 * 1024 * 1024} }

// Snapshot obtains one bounded, consistent read transaction, including lifecycle
// and row provenance. Oversize fails before fetching blobs; never returns a partial
// snapshot. Pending inbox entries are not yet row winners or proof of completeness.
func (s *Store) Snapshot(ctx context.Context, id string, limits Limits) (*readercontract.AnnotationRows, error) {
	if limits.Rows < 1 || limits.Rows > 16384 || limits.Bytes < 1 || limits.Bytes > 64*1024*1024 {
		return nil, ErrBudget
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	b := budget{limits.Rows, limits.Bytes}
	result, err := snapshotTx(ctx, tx, id, &b)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func snapshotTx(ctx context.Context, tx *sql.Tx, id string, b *budget) (*readercontract.AnnotationRows, error) {
	a, err := load(ctx, tx, "reader_annotation", id, b)
	if err != nil || a == nil {
		return nil, err
	}
	bookID := a.Columns["book_id"].(string)
	book, err := load(ctx, tx, "reader_book", bookID, b)
	if err != nil {
		return nil, err
	}
	bookLife, err := load(ctx, tx, "reader_book_lifecycle", bookID, b)
	if err != nil {
		return nil, err
	}
	annotationLife, err := load(ctx, tx, "reader_annotation_lifecycle", id, b)
	if err != nil {
		return nil, err
	}
	group := func(table, where string) ([]readercontract.Record, error) {
		rows, err := tx.QueryContext(ctx, "SELECT id FROM fn_"+table+" WHERE "+where+" ORDER BY id LIMIT ?", id, b.rows+1)
		if err != nil {
			return nil, err
		}
		ids := []string{}
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
			return nil, err
		}
		if len(ids) > b.rows {
			return nil, ErrBudget
		}
		result := make([]readercontract.Record, 0, len(ids))
		for _, id := range ids {
			r, err := load(ctx, tx, table, id, b)
			if err != nil {
				return nil, err
			}
			if r == nil {
				return nil, fmt.Errorf("snapshot row disappeared")
			}
			result = append(result, *r)
		}
		return result, nil
	}
	result := &readercontract.AnnotationRows{Annotation: *a, BookPresent: book != nil, BookDeleted: bookLife != nil && bookLife.Columns["deleted"] == int64(1), AnnotationDeleted: annotationLife != nil && annotationLife.Columns["deleted"] == int64(1)}
	if result.Sessions, err = group("reader_edit_session", "annotation_id=?"); err != nil {
		return nil, err
	}
	if result.Strokes, err = group("reader_stroke", "annotation_id=?"); err != nil {
		return nil, err
	}
	if result.Values, err = group("reader_annotation_value", "session_id IN (SELECT id FROM fn_reader_edit_session WHERE annotation_id=?)"); err != nil {
		return nil, err
	}
	if result.Claims, err = group("reader_erase_claim", "stroke_id IN (SELECT id FROM fn_reader_stroke WHERE annotation_id=?)"); err != nil {
		return nil, err
	}
	return result, nil
}

// SearchInput detaches ink, recognition alternatives and a journal watermark
// from ONE read snapshot. Reduction/hash happens after releasing this transaction.
type SearchInput struct {
	Rows        *readercontract.AnnotationRows
	Book, Title *readercontract.Record
	Recognition []readercontract.Record
	Revision    int64
}

func (s *Store) SearchSnapshot(ctx context.Context, id string, limits Limits) (*SearchInput, error) {
	if limits.Rows < 1 || limits.Rows > 16384 || limits.Bytes < 1 || limits.Bytes > 64<<20 {
		return nil, ErrBudget
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result := &SearchInput{}
	if err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq),0) FROM reader_store_changes").Scan(&result.Revision); err != nil {
		return nil, err
	}
	b := budget{limits.Rows, limits.Bytes}
	if result.Rows, err = snapshotTx(ctx, tx, id, &b); err != nil {
		return nil, err
	}
	if result.Rows != nil {
		bookID := result.Rows.Annotation.Columns["book_id"].(string)
		if result.Book, err = load(ctx, tx, "reader_book", bookID, &b); err != nil {
			return nil, err
		}
		if result.Title, err = load(ctx, tx, "reader_book_title", bookID, &b); err != nil {
			return nil, err
		}
		rows, err := tx.QueryContext(ctx, "SELECT id FROM fn_reader_recognition WHERE annotation_id=? ORDER BY id LIMIT ?", id, b.rows+1)
		if err != nil {
			return nil, err
		}
		var ids []string
		for rows.Next() {
			var key string
			if err = rows.Scan(&key); err != nil {
				break
			}
			ids = append(ids, key)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return nil, err
		}
		if len(ids) > b.rows {
			return nil, ErrBudget
		}
		for _, key := range ids {
			r, err := load(ctx, tx, "reader_recognition", key, &b)
			if err != nil {
				return nil, err
			}
			if r != nil {
				result.Recognition = append(result.Recognition, *r)
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

// Projection closes the snapshot transaction BEFORE reducing/hashing. No writer
// is held while processing ink. A nil result means the annotation is absent.
func (s *Store) Projection(ctx context.Context, id string, limits Limits) (*readercontract.AnnotationProjection, error) {
	rows, err := s.Snapshot(ctx, id, limits)
	if err != nil || rows == nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	p := readercontract.Reduce(*rows)
	return &p, nil
}
