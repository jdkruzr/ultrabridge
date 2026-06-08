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

// TextBoxData is a materialized text box's render-relevant columns (the bridge
// maps this onto forestrender.TextBox; syncstore stays render-agnostic). Geometry
// and FontSize are virtual units (page short axis = 10,000), the same space as
// stroke points; Color is the unsigned ARGB int64 stored verbatim; Z is the paint
// band (0 = below ink, 1 = above).
type TextBoxData struct {
	X, Y, Width, Height int64
	Text                string
	FontName            string
	FontSize            int64
	Color               int64
	Weight              int64
	BorderWidth         int64
	Z                   int64
}

type PageTextData struct {
	Text  string
	Model string
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

// LivePageTextBoxes returns a page's non-deleted text boxes in z order (band
// 0 before 1, so a caller drawing in slice order paints below-ink boxes first).
func (s *Store) LivePageTextBoxes(ctx context.Context, pagePK string) ([]TextBoxData, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT x, y, width, height, text, font_name, font_size, color, weight, border_width, z
		   FROM fn_text_box WHERE page_id = ? AND deleted_at IS NULL ORDER BY z`, pagePK)
	if err != nil {
		return nil, fmt.Errorf("live text boxes: %w", err)
	}
	defer rows.Close()

	var out []TextBoxData
	for rows.Next() {
		var d TextBoxData
		if err := rows.Scan(&d.X, &d.Y, &d.Width, &d.Height, &d.Text, &d.FontName,
			&d.FontSize, &d.Color, &d.Weight, &d.BorderWidth, &d.Z); err != nil {
			return nil, fmt.Errorf("scan text box: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// LivePageTextFromClient returns the latest non-deleted device OCR row for a page, if any.
func (s *Store) LivePageTextFromClient(ctx context.Context, pagePK string) (PageTextData, bool, error) {
	var d PageTextData
	err := s.db.QueryRowContext(ctx,
		`SELECT text, COALESCE(model, '') FROM fn_page_text_from_client WHERE id = ? AND deleted_at IS NULL`,
		pagePK).Scan(&d.Text, &d.Model)
	switch err {
	case nil:
		return d, true, nil
	case sql.ErrNoRows:
		return PageTextData{}, false, nil
	default:
		return PageTextData{}, false, fmt.Errorf("live client page text: %w", err)
	}
}
