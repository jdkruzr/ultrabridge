package readersearch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
)

type Result struct {
	AnnotationID string        `json:"annotation_id"`
	BookID       string        `json:"book_id"`
	Title        string        `json:"title"`
	Snippet      string        `json:"snippet"`
	Anchor       string        `json:"anchor"`
	InputHash    string        `json:"input_hash"`
	Alternatives []Alternative `json:"alternatives"`
}

func (s *Store) Search(ctx context.Context, text, book string, limit int) ([]Result, error) {
	if len(text) > 256 || !utf8.ValidString(text) || len(book) > 64 || limit < 1 || limit > 100 {
		return nil, fmt.Errorf("invalid search bounds")
	}
	terms := strings.Fields(text)
	if len(terms) > 16 {
		return nil, fmt.Errorf("too many search terms")
	}
	result := []Result{}
	if len(terms) == 0 {
		return result, nil
	}
	for i, term := range terms {
		terms[i] = "\"" + strings.ReplaceAll(term, "\"", "\"\"") + "\""
	}
	query := `SELECT d.annotation_id,d.book_id,d.title,snippet(reader_search_fts,2,'','','…',24),d.anchor,d.input_hash,d.alternatives
	 FROM reader_search_fts JOIN reader_search_documents d ON d.id=reader_search_fts.rowid
	 JOIN fn_reader_annotation a ON a.id=d.annotation_id WHERE reader_search_fts MATCH ? AND NOT ` + dirty("d.revision")
	args := []any{strings.Join(terms, " AND ")}
	if book != "" {
		query += " AND d.book_id=?"
		args = append(args, book)
	}
	query += " ORDER BY bm25(reader_search_fts),d.annotation_id LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r Result
		var alternatives string
		if err = rows.Scan(&r.AnnotationID, &r.BookID, &r.Title, &r.Snippet, &r.Anchor, &r.InputHash, &alternatives); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(alternatives), &r.Alternatives); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// Handler is mounted ONLY behind the readerlab fixture authentication. It is
// not a production endpoint or a new anchor URI/resolver. Results contain IDs.
func (s *Store) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "GET required", 405)
			return
		}
		if len(r.URL.RawQuery) > 2048 {
			http.Error(w, "invalid search bounds", 400)
			return
		}
		q := r.URL.Query().Get("q")
		book := r.URL.Query().Get("book")
		if len(r.URL.RawQuery) > 2048 || len(q) > 256 || !utf8.ValidString(q) || len(strings.Fields(q)) > 16 || len(book) > 64 {
			http.Error(w, "invalid search bounds", 400)
			return
		}
		results, err := s.Search(r.Context(), q, book, 25)
		if err != nil {
			http.Error(w, "search unavailable", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(results)
	})
}
