package remarkable

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const (
	searchIndexVersion     = 2
	searchPageSize         = 5000
	searchLanguage         = "en_US"
	searchStrokeResolution = "WORD"
	searchGenerationScale  = int64(1_000_000)
)

type searchSettingsResponse struct {
	Language      string `json:"language"`
	SearchEnabled bool   `json:"searchEnabled"`
}

type searchDeltaResponse struct {
	Version    int                `json:"version"`
	Generation int64              `json:"generation"`
	Latest     bool               `json:"latest"`
	Changed    []searchPageChange `json:"changed"`
}

type searchPageChange struct {
	DeltaID    string `json:"deltaId"`
	Generation int64  `json:"generation"`
	DocumentID string `json:"documentId,omitempty"`
	PageID     string `json:"pageId"`
	Deleted    bool   `json:"deleted,omitempty"`
}

type searchIndexResponse struct {
	Version     int                   `json:"version"`
	Generation  int64                 `json:"generation"`
	Handwritten searchHandwrittenData `json:"handwritten"`
}

type searchHandwrittenData struct {
	Content     string            `json:"content"`
	MainStrokes searchMainStrokes `json:"mainStrokes"`
}

type searchMainStrokes struct {
	Resolution string     `json:"resolution"`
	Strokes    [][]string `json:"strokes"`
}

type searchPageRow struct {
	NoteContentID int64
	DocumentID    string
	Page          int
	PageID        string
	BodyText      string
	IndexedAt     int64
	Generation    int64
}

func (p *protocol) handleSearchSettings(w http.ResponseWriter, r *http.Request, _ tokenClaims) {
	writeJSON(w, http.StatusOK, searchSettingsResponse{
		Language:      searchLanguage,
		SearchEnabled: true,
	})
}

func (p *protocol) handleSearchDelta(w http.ResponseWriter, r *http.Request, _ tokenClaims) {
	since, err := parseSearchGeneration(r.Header.Get("If-None-Match"))
	if err != nil {
		http.Error(w, "invalid If-None-Match generation", http.StatusBadRequest)
		return
	}
	rows, hasMore, err := p.store.listSearchPages(r.Context(), since, searchPageSize)
	if err != nil {
		http.Error(w, "failed to list search delta", http.StatusInternalServerError)
		return
	}
	if len(rows) == 0 {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	changed := make([]searchPageChange, 0, len(rows))
	for _, row := range rows {
		changed = append(changed, searchPageChange{
			DeltaID:    newSearchDeltaID(),
			Generation: row.Generation,
			DocumentID: compactSearchID(row.DocumentID),
			PageID:     compactSearchID(row.PageID),
		})
	}

	generation := rows[0].Generation
	w.Header().Set("ETag", strconv.FormatInt(generation, 10))
	writeJSON(w, http.StatusOK, searchDeltaResponse{
		Version:    searchIndexVersion,
		Generation: generation,
		Latest:     !hasMore,
		Changed:    changed,
	})
}

func (p *protocol) handleSearchIndex(w http.ResponseWriter, r *http.Request, _ tokenClaims) {
	docID := r.PathValue("docId")
	pageID := r.PathValue("pageId")
	if docID == "" || pageID == "" {
		http.NotFound(w, r)
		return
	}

	row, ok, err := p.store.getSearchPage(r.Context(), docID, pageID)
	if err != nil {
		http.Error(w, "failed to load search index", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	tokens := searchTokens(row.BodyText)
	if len(tokens) == 0 {
		http.NotFound(w, r)
		return
	}

	writeJSON(w, http.StatusOK, searchIndexResponse{
		Version:    searchIndexVersion,
		Generation: row.Generation,
		Handwritten: searchHandwrittenData{
			Content: strings.Join(tokens, " "),
			MainStrokes: searchMainStrokes{
				Resolution: searchStrokeResolution,
				Strokes:    tokenAlignedStrokes(tokens),
			},
		},
	})
}

func (p *protocol) handleSearchError(w http.ResponseWriter, r *http.Request, _ tokenClaims) {
	if r.Body != nil {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = r.Body.Close()
		if strings.TrimSpace(string(body)) != "" {
			p.logger.Warn("remarkable search error report", "body", string(body))
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *store) listSearchPages(ctx context.Context, since int64, limit int) ([]searchPageRow, bool, error) {
	if limit <= 0 {
		limit = searchPageSize
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, note_path, page, COALESCE(body_text, ''), indexed_at
		FROM note_content
		WHERE note_path LIKE ? AND page >= 0
		ORDER BY indexed_at DESC, id DESC`, remarkablePathPrefix+"%")
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	renderCache := map[string]RenderDocument{}
	found := make([]searchPageRow, 0, limit)
	hasMore := false
	for rows.Next() {
		row, ok, err := s.scanSearchPageRow(ctx, rows, renderCache)
		if err != nil {
			return nil, false, err
		}
		if !ok || row.Generation <= since {
			continue
		}
		if len(found) >= limit {
			hasMore = true
			break
		}
		found = append(found, row)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	return found, hasMore, nil
}

func (s *store) getSearchPage(ctx context.Context, docID, pageID string) (searchPageRow, bool, error) {
	doc, err := s.renderDocument(ctx, docID)
	if errors.Is(err, errDocumentNotFound) {
		var ok bool
		doc, ok, err = s.renderDocumentBySearchID(ctx, docID)
		if err != nil {
			return searchPageRow{}, false, err
		}
		if !ok {
			return searchPageRow{}, false, nil
		}
		docID = doc.ID
	}
	if err != nil {
		return searchPageRow{}, false, err
	}
	page := -1
	for i, candidate := range doc.PageOrder {
		if candidate == pageID || compactSearchID(candidate) == pageID {
			page = i
			pageID = candidate
			break
		}
	}
	if page < 0 {
		return searchPageRow{}, false, nil
	}

	var row searchPageRow
	row.DocumentID = docID
	row.PageID = pageID
	row.Page = page
	err = s.db.QueryRowContext(ctx, `
		SELECT id, COALESCE(body_text, ''), indexed_at
		FROM note_content
		WHERE note_path = ? AND page = ?`, remarkablePath(docID), page).
		Scan(&row.NoteContentID, &row.BodyText, &row.IndexedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return searchPageRow{}, false, nil
	}
	if err != nil {
		return searchPageRow{}, false, err
	}
	row.Generation = searchGeneration(row.IndexedAt, row.NoteContentID)
	if !isSearchableHandwritingText(row.BodyText) {
		return searchPageRow{}, false, nil
	}
	return row, true, nil
}

func (s *store) renderDocumentBySearchID(ctx context.Context, searchID string) (RenderDocument, bool, error) {
	rootRec, err := s.getBlob(ctx, rootBlobID)
	if errors.Is(err, errBlobNotFound) {
		return RenderDocument{}, false, nil
	}
	if err != nil {
		return RenderDocument{}, false, err
	}
	topHashRaw, err := osReadFile(rootRec.Path)
	if err != nil {
		return RenderDocument{}, false, err
	}
	topHash := strings.TrimSpace(string(topHashRaw))
	if topHash == "" {
		return RenderDocument{}, false, nil
	}
	topData, err := s.readBlob(ctx, topHash)
	if err != nil {
		return RenderDocument{}, false, nil
	}
	topEntries, err := parseIndex(topData)
	if err != nil {
		return RenderDocument{}, false, fmt.Errorf("parse top index: %w", err)
	}
	for _, entry := range topEntries {
		if compactSearchID(entry.EntryName) != searchID {
			continue
		}
		doc, err := s.renderDocument(ctx, entry.EntryName)
		if errors.Is(err, errDocumentNotFound) {
			return RenderDocument{}, false, nil
		}
		return doc, err == nil, err
	}
	return RenderDocument{}, false, nil
}

type searchRowScanner interface {
	Scan(dest ...any) error
}

func (s *store) scanSearchPageRow(ctx context.Context, scanner searchRowScanner, renderCache map[string]RenderDocument) (searchPageRow, bool, error) {
	var notePath string
	var row searchPageRow
	if err := scanner.Scan(&row.NoteContentID, &notePath, &row.Page, &row.BodyText, &row.IndexedAt); err != nil {
		return searchPageRow{}, false, err
	}
	if !isSearchableHandwritingText(row.BodyText) {
		return searchPageRow{}, false, nil
	}
	docID, ok := strings.CutPrefix(notePath, remarkablePathPrefix)
	if !ok || docID == "" {
		return searchPageRow{}, false, nil
	}
	doc, ok := renderCache[docID]
	if !ok {
		var err error
		doc, err = s.renderDocument(ctx, docID)
		if errors.Is(err, errDocumentNotFound) {
			return searchPageRow{}, false, nil
		}
		if err != nil {
			return searchPageRow{}, false, err
		}
		renderCache[docID] = doc
	}
	if row.Page < 0 || row.Page >= len(doc.PageOrder) {
		return searchPageRow{}, false, nil
	}
	row.DocumentID = docID
	row.PageID = doc.PageOrder[row.Page]
	row.Generation = searchGeneration(row.IndexedAt, row.NoteContentID)
	return row, true, nil
}

func parseSearchGeneration(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	raw = strings.Trim(raw, `"`)
	if raw == "" {
		return 0, nil
	}
	return strconv.ParseInt(raw, 10, 64)
}

func searchGeneration(indexedAt, rowID int64) int64 {
	return indexedAt*searchGenerationScale + rowID
}

func isSearchableHandwritingText(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)
	switch lower {
	case "(blank page)", "blank page", "there is no handwritten text on the page.":
		return false
	}
	if strings.Contains(lower, "no handwritten text") {
		return false
	}
	return true
}

func searchTokens(text string) []string {
	if !isSearchableHandwritingText(text) {
		return nil
	}
	return strings.Fields(text)
}

func tokenAlignedStrokes(tokens []string) [][]string {
	strokes := make([][]string, len(tokens))
	for i := range strokes {
		strokes[i] = []string{}
	}
	return strokes
}

func newSearchDeltaID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("delta-%d", len(b))
	}
	return hex.EncodeToString(b[:])
}

func compactSearchID(id string) string {
	parts := strings.Split(id, "-")
	if len(parts) != 5 || len(parts[0]) != 8 || len(parts[1]) != 4 || len(parts[2]) != 4 || len(parts[3]) != 4 || len(parts[4]) != 12 {
		return id
	}
	for _, part := range parts {
		if _, err := hex.DecodeString(part); err != nil {
			return id
		}
	}
	return strings.Join(parts, "")
}
