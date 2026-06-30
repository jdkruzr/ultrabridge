package search

import (
	"context"
	"database/sql"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
	"unicode"
)

// SearchIndex indexes and queries note page content.
type SearchIndex interface {
	Index(ctx context.Context, doc NoteDocument) error
	Search(ctx context.Context, q SearchQuery) ([]SearchResult, error)
	Delete(ctx context.Context, path string) error
	// IndexPage satisfies processor.Indexer — convenience wrapper around Index.
	// titleText and keywords are populated for page 0 only; pass empty strings for other pages.
	IndexPage(ctx context.Context, path string, pageIdx int, source, bodyText, titleText, keywords string) error
	// GetContent returns all indexed content for a note, ordered by page.
	GetContent(ctx context.Context, path string) ([]NoteDocument, error)
	// GetContentByPrefix returns indexed content for every note_path matching the
	// LIKE pattern (e.g. "forestnote://{nb}/%"), keyed by note_path.
	GetContentByPrefix(ctx context.Context, likePattern string) (map[string]NoteDocument, error)
	// ListFolders returns distinct parent directory names from indexed content.
	ListFolders(ctx context.Context) ([]string, error)
}

// Store implements SearchIndex using SQLite FTS5.
type Store struct {
	db *sql.DB
}

// New creates a search Store.
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

func (s *Store) IndexPage(ctx context.Context, path string, pageIdx int, source, bodyText, titleText, keywords string) error {
	return s.Index(ctx, NoteDocument{
		Path:      path,
		Page:      pageIdx,
		BodyText:  bodyText,
		TitleText: titleText,
		Keywords:  keywords,
		Source:    source,
	})
}

func (s *Store) Index(ctx context.Context, doc NoteDocument) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO note_content (note_path, page, title_text, body_text, keywords, source, model, indexed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(note_path, page) DO UPDATE SET
			title_text=excluded.title_text,
			body_text=excluded.body_text,
			keywords=excluded.keywords,
			source=excluded.source,
			model=excluded.model,
			indexed_at=excluded.indexed_at`,
		doc.Path, doc.Page, doc.TitleText, doc.BodyText, doc.Keywords,
		doc.Source, doc.Model, now,
	)
	if err != nil {
		return fmt.Errorf("search index: %w", err)
	}
	return nil
}

func (s *Store) Search(ctx context.Context, q SearchQuery) ([]SearchResult, error) {
	if strings.TrimSpace(q.Text) == "" {
		return nil, nil
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 25
	}

	// bm25() returns negative floats; ORDER BY ASC puts best matches first.
	// snippet() targets body_text (column index 3: note_path, page, title_text, body_text, keywords).
	query := `
		SELECT
			nc.note_path,
			nc.page,
			bm25(note_fts) AS score,
			snippet(note_fts, 3, '', '', '...', 25) AS snip
		FROM note_fts
		JOIN note_content nc ON nc.id = note_fts.rowid
		WHERE note_fts MATCH ?`
	args := []interface{}{buildFTS5Query(q.Text)}

	if q.Folder != "" {
		// Filter by folder path segment — matches "/{folder}/" anywhere in the path.
		query += ` AND nc.note_path LIKE ?`
		args = append(args, "%/"+q.Folder+"/%")
	}

	query += ` ORDER BY bm25(note_fts) ASC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search query: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.Path, &r.Page, &r.Score, &r.Snippet); err != nil {
			return nil, fmt.Errorf("search scan: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

func (s *Store) ListFolders(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT note_path FROM note_content`)
	if err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}
	defer rows.Close()

	seen := map[string]bool{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		// Extract the parent directory name (last directory component before the filename).
		dir := path.Dir(p)
		folder := path.Base(dir)
		if folder != "." && folder != "/" && !seen[folder] {
			seen[folder] = true
		}
	}

	folders := make([]string, 0, len(seen))
	for f := range seen {
		folders = append(folders, f)
	}
	sort.Strings(folders)
	return folders, rows.Err()
}

func (s *Store) Delete(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM note_content WHERE note_path=?", path)
	return err
}

// Rename repoints every indexed page from oldPath to newPath. Used when a note
// is moved/renamed so its FTS entries follow the file instead of going stale.
// The note_content_au trigger keeps note_fts in sync on UPDATE.
func (s *Store) Rename(ctx context.Context, oldPath, newPath string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE note_content SET note_path=? WHERE note_path=?", newPath, oldPath)
	return err
}

// Copy duplicates every indexed page of srcPath under dstPath. Used when a note
// is copied so the copy is independently searchable (content is identical, so
// no re-OCR is needed). dst is cleared first so the copy is idempotent.
func (s *Store) Copy(ctx context.Context, srcPath, dstPath string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM note_content WHERE note_path=?", dstPath); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO note_content (note_path, page, title_text, body_text, keywords, source, model, indexed_at)
		SELECT ?, page, title_text, body_text, keywords, source, model, indexed_at
		FROM note_content WHERE note_path=?`, dstPath, srcPath); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetContent(ctx context.Context, path string) ([]NoteDocument, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT note_path, page, COALESCE(title_text,''), COALESCE(body_text,''),
		       COALESCE(keywords,''), COALESCE(source,''), COALESCE(model,'')
		FROM note_content WHERE note_path=? ORDER BY page`, path)
	if err != nil {
		return nil, fmt.Errorf("get content: %w", err)
	}
	defer rows.Close()
	var docs []NoteDocument
	for rows.Next() {
		var d NoteDocument
		if err := rows.Scan(&d.Path, &d.Page, &d.TitleText, &d.BodyText, &d.Keywords, &d.Source, &d.Model); err != nil {
			return nil, fmt.Errorf("get content scan: %w", err)
		}
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

// GetContentByPrefix returns indexed content for every note_path matching the
// given LIKE prefix pattern, keyed by note_path. It exists so a caller can fetch
// all of a ForestNote notebook's per-page documents (each indexed on a distinct
// forestnote://{nb}/{page} path) in one query instead of N GetContent round-trips
// against the single-writer notedb. The caller supplies the full LIKE pattern
// (e.g. "forestnote://{nb}/%"); backslash is the escape char so literal % / _ in
// the prefix are matched verbatim.
func (s *Store) GetContentByPrefix(ctx context.Context, likePattern string) (map[string]NoteDocument, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT note_path, page, COALESCE(title_text,''), COALESCE(body_text,''),
		       COALESCE(keywords,''), COALESCE(source,''), COALESCE(model,'')
		FROM note_content WHERE note_path LIKE ? ESCAPE '\' ORDER BY note_path, page`, likePattern)
	if err != nil {
		return nil, fmt.Errorf("get content by prefix: %w", err)
	}
	defer rows.Close()
	out := make(map[string]NoteDocument)
	for rows.Next() {
		var d NoteDocument
		if err := rows.Scan(&d.Path, &d.Page, &d.TitleText, &d.BodyText, &d.Keywords, &d.Source, &d.Model); err != nil {
			return nil, fmt.Errorf("get content by prefix scan: %w", err)
		}
		out[d.Path] = d
	}
	return out, rows.Err()
}

// buildFTS5Query keeps exact-phrase behavior while letting natural multi-word
// queries match when the terms appear separately, e.g. "Froster Glacier" should
// match "S3/Glacier ... (Froster)" instead of falling through to vector-only
// hybrid results.
func buildFTS5Query(input string) string {
	trimmed := strings.TrimSpace(input)
	trimmed = strings.Trim(trimmed, `"`)
	phrase := escapeFTS5Phrase(trimmed)
	terms := fts5Terms(trimmed)
	if len(terms) <= 1 {
		return phrase
	}
	parts := make([]string, 0, len(terms))
	seen := map[string]bool{}
	for _, term := range terms {
		key := strings.ToLower(term)
		if seen[key] {
			continue
		}
		seen[key] = true
		parts = append(parts, escapeFTS5Phrase(term))
	}
	if len(parts) <= 1 {
		return phrase
	}
	return phrase + " OR (" + strings.Join(parts, " AND ") + ")"
}

func fts5Terms(input string) []string {
	return strings.FieldsFunc(input, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r))
	})
}

// escapeFTS5Phrase wraps text in double quotes and escapes internal quotes,
// preventing FTS5 syntax injection while preserving phrase matching.
func escapeFTS5Phrase(input string) string {
	escaped := strings.ReplaceAll(input, `"`, `""`)
	return `"` + escaped + `"`
}
