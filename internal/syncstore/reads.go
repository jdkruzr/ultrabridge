package syncstore

import (
	"context"
	"database/sql"
	"fmt"
)

// StrokeData is a materialized stroke's render-relevant columns (the bridge maps
// this onto forestrender.Stroke; syncstore stays render-agnostic).
type StrokeData struct {
	Color       int64
	PenWidthMin int64
	PenWidthMax int64
	Points      []byte
	Z           int64
}

// LivePage reports a page's parent notebook and whether it is live (exists and
// not soft-deleted). A missing or deleted page returns live=false — the bridge
// then skips rendering it.
func (s *Store) LivePage(ctx context.Context, pagePK string) (notebookID string, live bool, err error) {
	var nb sql.NullString
	var del sql.NullInt64
	switch e := s.db.QueryRowContext(ctx,
		`SELECT notebook_id, deleted_at FROM fn_page WHERE id = ?`, pagePK).Scan(&nb, &del); e {
	case nil:
		return nb.String, !del.Valid, nil
	case sql.ErrNoRows:
		return "", false, nil
	default:
		return "", false, fmt.Errorf("live page: %w", e)
	}
}

// LivePageStrokes returns a page's non-deleted strokes in z order.
func (s *Store) LivePageStrokes(ctx context.Context, pagePK string) ([]StrokeData, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT color, pen_width_min, pen_width_max, points, z
		   FROM fn_stroke WHERE page_id = ? AND deleted_at IS NULL ORDER BY z`, pagePK)
	if err != nil {
		return nil, fmt.Errorf("live strokes: %w", err)
	}
	defer rows.Close()

	var out []StrokeData
	for rows.Next() {
		var d StrokeData
		if err := rows.Scan(&d.Color, &d.PenWidthMin, &d.PenWidthMax, &d.Points, &d.Z); err != nil {
			return nil, fmt.Errorf("scan stroke: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
