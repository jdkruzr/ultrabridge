// Pattern: Imperative Shell -- coordinates FTS5 search, vector similarity, and DB enrichment
package rag

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sysop/ultrabridge/internal/fnpath"
	"github.com/sysop/ultrabridge/internal/search"
)

// Source type constants tag each result by its origin so the UI can facet on
// them. Derived in enrichResult from the note_path namespace and the
// boox_notes/notes tables — there is no source_type column.
const (
	SourceSupernote  = "supernote"
	SourceBoox       = "boox"
	SourceForestNote = "forestnote"
	SourceRemarkable = "remarkable"
	SourceDigest     = "digest"
)

// SearchRequest is the input for hybrid search.
type SearchRequest struct {
	Query        string
	Folder       string // legacy exact folder filter (empty = all)
	Device       string // filter by device model (empty = all)
	Sources      []string
	Locations    []LocationFilter
	DateFrom     time.Time // legacy modified lower bound
	DateTo       time.Time // legacy modified upper bound
	CreatedFrom  time.Time
	CreatedTo    time.Time
	ModifiedFrom time.Time
	ModifiedTo   time.Time
	Sort         string // relevance|date_asc|date_desc
	Limit        int    // 0 = default (20)
}

// LocationFilter narrows search to a source-specific folder, or to an aggregate
// full folder path across sources when Source is empty.
type LocationFilter struct {
	Source   string
	ID       string
	FullPath string
}

// SearchResult is one ranked result with full metadata for citation.
type SearchResult struct {
	NotePath   string
	Page       int
	BodyText   string
	TitleText  string
	Score      float64
	Folder     string
	LocationID string
	Device     string
	SourceType string // one of the Source* consts
	NoteDate   time.Time
	CreatedAt  time.Time
	ModifiedAt time.Time
}

// SearchRetriever is the interface for hybrid search. Defined as an interface
// for testability — the web handler accepts this interface, not the concrete type.
type SearchRetriever interface {
	Search(ctx context.Context, req SearchRequest) ([]SearchResult, error)
}

// Retriever provides hybrid search over note content. Implements SearchRetriever.
type Retriever struct {
	db          *sql.DB
	searchIndex search.SearchIndex
	embedStore  *Store
	embedder    Embedder
	logger      *slog.Logger
}

func NewRetriever(db *sql.DB, searchIndex search.SearchIndex, embedStore *Store, embedder Embedder, logger *slog.Logger) *Retriever {
	return &Retriever{
		db:          db,
		searchIndex: searchIndex,
		embedStore:  embedStore,
		embedder:    embedder,
		logger:      logger,
	}
}

// Search implements SearchRetriever using hybrid fusion (FTS5 + vector similarity via RRF).
func (r *Retriever) Search(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 20
	}

	// Post-merge filters (source/folder/device/date) prune after fusion, so
	// over-fetch from each leg to keep a full page when a filter is active.
	overfetch := 2
	if len(req.Sources) > 0 || req.Device != "" || len(req.Locations) > 0 ||
		!req.DateFrom.IsZero() || !req.DateTo.IsZero() ||
		!req.CreatedFrom.IsZero() || !req.CreatedTo.IsZero() ||
		!req.ModifiedFrom.IsZero() || !req.ModifiedTo.IsZero() || req.Sort != "" {
		overfetch = 4
	}
	if req.Sort == "date_asc" || req.Sort == "date_desc" {
		overfetch = 10
	}

	// 1. FTS5 keyword search (always available)
	ftsResults, err := r.searchIndex.Search(ctx, search.SearchQuery{
		Text:   req.Query,
		Folder: req.Folder,
		Limit:  limit * overfetch, // fetch extra for fusion + post-merge filtering
	})
	if err != nil {
		return nil, fmt.Errorf("fts search: %w", err)
	}

	// 2. Vector similarity search (if embeddings available and embedder can embed query)
	var vecRanked []rankedDoc
	if r.embedStore != nil && r.embedder != nil {
		allEmbeddings := r.embedStore.AllEmbeddings()
		if len(allEmbeddings) > 0 {
			queryVec, err := r.embedder.Embed(ctx, req.Query)
			if err != nil {
				r.logger.Warn("query embedding failed, falling back to FTS-only", "err", err)
			} else {
				// Score all embeddings by cosine similarity
				type scored struct {
					rec   EmbeddingRecord
					score float32
				}
				var candidates []scored
				for _, rec := range allEmbeddings {
					sim := CosineSimilarity(queryVec, rec.Embedding)
					if sim > 0 {
						candidates = append(candidates, scored{rec, sim})
					}
				}
				sort.Slice(candidates, func(i, j int) bool {
					return candidates[i].score > candidates[j].score
				})
				// Take the top results for fusion, deduped to the best-scoring
				// chunk per (path,page) — a page embeds as multiple chunks, but
				// fusion ranks at page granularity, so counting every chunk would
				// over-weight long (many-chunk) pages.
				topN := limit * overfetch
				seenPage := map[string]bool{}
				for _, c := range candidates {
					key := c.rec.NotePath + "\x00" + strconv.Itoa(c.rec.Page)
					if seenPage[key] {
						continue
					}
					seenPage[key] = true
					vecRanked = append(vecRanked, rankedDoc{
						notePath: c.rec.NotePath,
						page:     c.rec.Page,
						rank:     len(vecRanked) + 1,
					})
					if len(vecRanked) >= topN {
						break
					}
				}
			}
		}
	}

	// 3. Reciprocal Rank Fusion
	type docKey struct {
		notePath string
		page     int
	}
	rrfScores := map[docKey]float64{}

	// FTS5 ranks
	for rank, r := range ftsResults {
		key := docKey{r.Path, r.Page}
		rrfScores[key] += 1.0 / float64(60+rank+1)
	}

	// Vector ranks
	for _, r := range vecRanked {
		key := docKey{r.notePath, r.page}
		rrfScores[key] += 1.0 / float64(60+r.rank)
	}

	// Sort by RRF score descending
	type rrfEntry struct {
		key   docKey
		score float64
	}
	var merged []rrfEntry
	for k, s := range rrfScores {
		merged = append(merged, rrfEntry{k, s})
	}
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].score > merged[j].score
	})

	// 4. Enrich + filter. Relevance order can stop after limit; date order needs
	// the filtered candidate set before the final sort.
	// No pre-truncation: post-merge filters (source/folder/device/date) may prune,
	// so we may need to look past the first `limit` fused candidates.
	results := make([]SearchResult, 0, limit)
	for _, entry := range merged {
		if req.Sort != "date_asc" && req.Sort != "date_desc" && len(results) >= limit {
			break
		}
		result, err := r.enrichResult(ctx, entry.key.notePath, entry.key.page, entry.score)
		if err != nil {
			r.logger.Warn("enrich result failed", "path", entry.key.notePath, "page", entry.key.page, "err", err)
			continue
		}
		// Apply post-merge filters (source type, folder, device, date range)
		if len(req.Sources) > 0 && !containsStr(req.Sources, result.SourceType) {
			continue
		}
		if req.Folder != "" && result.Folder != req.Folder {
			continue
		}
		if req.Device != "" && result.Device != req.Device {
			continue
		}
		if len(req.Locations) > 0 && !matchesLocation(req.Locations, result) {
			continue
		}
		modFrom, modTo := req.ModifiedFrom, req.ModifiedTo
		if modFrom.IsZero() {
			modFrom = req.DateFrom
		}
		if modTo.IsZero() {
			modTo = req.DateTo
		}
		if !req.CreatedFrom.IsZero() && (result.CreatedAt.IsZero() || result.CreatedAt.Before(req.CreatedFrom)) {
			continue
		}
		if !req.CreatedTo.IsZero() && (result.CreatedAt.IsZero() || result.CreatedAt.After(req.CreatedTo)) {
			continue
		}
		if !modFrom.IsZero() && (result.ModifiedAt.IsZero() || result.ModifiedAt.Before(modFrom)) {
			continue
		}
		if !modTo.IsZero() && (result.ModifiedAt.IsZero() || result.ModifiedAt.After(modTo)) {
			continue
		}
		results = append(results, *result)
	}
	if req.Sort == "date_asc" || req.Sort == "date_desc" {
		sort.SliceStable(results, func(i, j int) bool {
			a, b := resultSortDate(results[i]), resultSortDate(results[j])
			if a.Equal(b) {
				return results[i].Score > results[j].Score
			}
			if req.Sort == "date_asc" {
				return a.Before(b)
			}
			return a.After(b)
		})
		if len(results) > limit {
			results = results[:limit]
		}
	}

	return results, nil
}

func matchesLocation(filters []LocationFilter, result *SearchResult) bool {
	for _, f := range filters {
		if f.Source != "" && f.Source != result.SourceType {
			continue
		}
		if f.ID != "" && f.ID != result.LocationID {
			continue
		}
		if f.FullPath != "" && normalizeFolderPath(f.FullPath) != normalizeFolderPath(result.Folder) {
			continue
		}
		return true
	}
	return false
}

func resultSortDate(r SearchResult) time.Time {
	if !r.ModifiedAt.IsZero() {
		return r.ModifiedAt
	}
	if !r.CreatedAt.IsZero() {
		return r.CreatedAt
	}
	return time.Time{}
}

func fillDateFallbacks(result *SearchResult) {
	if result.ModifiedAt.IsZero() {
		result.ModifiedAt = result.CreatedAt
	}
	if result.CreatedAt.IsZero() {
		result.CreatedAt = result.ModifiedAt
	}
}

func containsStr(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

type rankedDoc struct {
	notePath string
	page     int
	rank     int
}

// enrichResult fetches metadata for a result via SQL JOINs and returns a populated SearchResult.
func (r *Retriever) enrichResult(ctx context.Context, notePath string, page int, score float64) (*SearchResult, error) {
	result := &SearchResult{
		NotePath: notePath,
		Page:     page,
		Score:    score,
	}

	var indexedAt sql.NullInt64
	err := r.db.QueryRowContext(ctx,
		`SELECT COALESCE(body_text, ''), COALESCE(title_text, ''), indexed_at FROM note_content WHERE note_path = ? AND page = ?`,
		notePath, page,
	).Scan(&result.BodyText, &result.TitleText, &indexedAt)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("query note_content: %w", err)
	}
	if indexedAt.Valid && indexedAt.Int64 > 0 {
		result.ModifiedAt = time.Unix(indexedAt.Int64, 0)
		result.NoteDate = result.ModifiedAt
	}

	// Opaque-path sources are classified by their note_path namespace (no
	// boox_notes/notes row exists for them). Digest and ForestNote content live
	// only in note_content, keyed by these prefixes.
	if strings.HasPrefix(notePath, "digest://") {
		result.SourceType = SourceDigest
		result.Device = "Supernote"
		uid := strings.TrimPrefix(notePath, "digest://")
		var created, updated sql.NullInt64
		if err := r.db.QueryRowContext(ctx,
			`SELECT created_at, updated_at FROM digests WHERE unique_identifier = ? AND is_deleted = 'N'`,
			uid,
		).Scan(&created, &updated); err == nil {
			result.CreatedAt = unixMilliTime(created)
			result.ModifiedAt = unixMilliTime(updated)
			fillDateFallbacks(result)
			result.NoteDate = result.ModifiedAt
		}
		return result, nil
	}
	if fnpath.Is(notePath) {
		result.SourceType = SourceForestNote
		result.Device = "ForestNote"
		nbID := fnpath.NotebookID(notePath)
		var folderID sql.NullString
		var created, modified sql.NullInt64
		if err := r.db.QueryRowContext(ctx, `
			SELECT COALESCE(n.folder_id, ''), COALESCE(n.created_at, 0),
			       MAX(n.lww_wall_ts,
			           COALESCE((SELECT MAX(p.lww_wall_ts) FROM fn_page p
			                      WHERE p.notebook_id = n.id AND p.deleted_at IS NULL), 0),
			           COALESCE((SELECT MAX(s.lww_wall_ts) FROM fn_stroke s
			                      JOIN fn_page p ON p.id = s.page_id
			                      WHERE p.notebook_id = n.id AND p.deleted_at IS NULL AND s.deleted_at IS NULL), 0),
			           COALESCE((SELECT MAX(t.lww_wall_ts) FROM fn_text_box t
			                      JOIN fn_page p ON p.id = t.page_id
			                      WHERE p.notebook_id = n.id AND p.deleted_at IS NULL AND t.deleted_at IS NULL), 0))
			  FROM fn_notebook n
			 WHERE n.id = ? AND n.deleted_at IS NULL`,
			nbID,
		).Scan(&folderID, &created, &modified); err == nil {
			result.LocationID = folderID.String
			result.Folder = r.forestNoteFolderPath(ctx, folderID.String)
			result.CreatedAt = unixMilliTime(created)
			result.ModifiedAt = unixMilliTime(modified)
			fillDateFallbacks(result)
			result.NoteDate = result.ModifiedAt
		}
		return result, nil
	}
	if strings.HasPrefix(notePath, "remarkable://") {
		result.SourceType = SourceRemarkable
		result.Device = "reMarkable"
		docID := strings.TrimPrefix(notePath, "remarkable://")
		var parentID, modifiedClient sql.NullString
		var updated sql.NullInt64
		if err := r.db.QueryRowContext(ctx,
			`SELECT parent_id, modified_client, updated_at FROM remarkable_documents WHERE id = ? AND deleted = 0`,
			docID,
		).Scan(&parentID, &modifiedClient, &updated); err == nil {
			result.LocationID = parentID.String
			result.Folder = r.remarkableFolderPath(ctx, parentID.String)
			if t, ok := parseRemarkableTime(modifiedClient.String); ok {
				result.ModifiedAt = t
			} else {
				result.ModifiedAt = unixMilliTime(updated)
			}
			result.CreatedAt = result.ModifiedAt
			result.NoteDate = result.ModifiedAt
		}
		if result.Folder == "" {
			result.Folder = metadataFolderLine(result.BodyText)
		}
		return result, nil
	}

	// Try boox_notes first for metadata
	var folder, device sql.NullString
	var createdAt, updatedAt sql.NullInt64
	err = r.db.QueryRowContext(ctx,
		`SELECT folder, device_model, created_at, updated_at FROM boox_notes WHERE path = ?`,
		notePath,
	).Scan(&folder, &device, &createdAt, &updatedAt)
	if err == nil {
		result.SourceType = SourceBoox
		result.Folder = folder.String
		result.LocationID = folder.String
		result.Device = device.String
		result.CreatedAt = unixMilliTime(createdAt)
		result.ModifiedAt = unixMilliTime(updatedAt)
		fillDateFallbacks(result)
		result.NoteDate = result.ModifiedAt
		return result, nil
	}

	// Fall back to notes table (Supernote)
	var relPath sql.NullString
	var snCreatedAt, snUpdatedAt sql.NullInt64
	err = r.db.QueryRowContext(ctx,
		`SELECT rel_path, created_at, updated_at FROM notes WHERE path = ?`,
		notePath,
	).Scan(&relPath, &snCreatedAt, &snUpdatedAt)
	if err == nil {
		result.SourceType = SourceSupernote
		result.Device = "Supernote"
		if relPath.Valid {
			dir := path.Dir(relPath.String)
			result.Folder = filepathLikeDir(dir)
			result.LocationID = result.Folder
		}
		result.CreatedAt = unixMilliTime(snCreatedAt)
		result.ModifiedAt = unixMilliTime(snUpdatedAt)
		fillDateFallbacks(result)
		result.NoteDate = result.ModifiedAt
		return result, nil
	}

	// Neither table matched — fall back to path-based folder extraction.
	// Defense-in-depth for orphaned note_content rows; default the source to
	// Supernote (the device's native content) so a facet filter still includes it.
	result.SourceType = SourceSupernote
	if notePath != "" {
		dir := path.Dir(notePath)
		result.Folder = filepathLikeDir(dir)
		result.LocationID = result.Folder
	}
	return result, nil
}

func unixMilliTime(v sql.NullInt64) time.Time {
	if !v.Valid || v.Int64 <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(v.Int64)
}

func filepathLikeDir(dir string) string {
	if dir == "." || dir == "/" {
		return ""
	}
	return strings.TrimPrefix(dir, "/")
}

func metadataFolderLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Folder: ") {
			return normalizeFolderPath(strings.TrimSpace(strings.TrimPrefix(line, "Folder: ")))
		}
	}
	return ""
}

func normalizeFolderPath(p string) string {
	parts := strings.Split(strings.TrimSpace(p), "/")
	out := parts[:0]
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return strings.Join(out, "/")
}

func parseRemarkableTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000000Z"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func (r *Retriever) forestNoteFolderPath(ctx context.Context, folderID string) string {
	var names []string
	seen := map[string]bool{}
	for folderID != "" && !seen[folderID] {
		seen[folderID] = true
		var name, parent sql.NullString
		err := r.db.QueryRowContext(ctx,
			`SELECT COALESCE(name, ''), COALESCE(parent_folder_id, '') FROM fn_folder WHERE id = ? AND deleted_at IS NULL`,
			folderID,
		).Scan(&name, &parent)
		if err != nil {
			break
		}
		if name.String != "" {
			names = append([]string{name.String}, names...)
		}
		folderID = parent.String
	}
	return strings.Join(names, "/")
}

func (r *Retriever) remarkableFolderPath(ctx context.Context, folderID string) string {
	var names []string
	seen := map[string]bool{}
	for folderID != "" && !seen[folderID] {
		seen[folderID] = true
		var name, parent sql.NullString
		err := r.db.QueryRowContext(ctx,
			`SELECT visible_name, parent_id FROM remarkable_documents WHERE id = ? AND deleted = 0`,
			folderID,
		).Scan(&name, &parent)
		if err != nil {
			break
		}
		if name.String != "" {
			names = append([]string{name.String}, names...)
		}
		folderID = parent.String
	}
	return strings.Join(names, "/")
}
