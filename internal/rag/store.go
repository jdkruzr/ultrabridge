package rag

// FCIS: Imperative Shell

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"
)

// EmbeddingRecord represents one stored embedding (one chunk of one page).
type EmbeddingRecord struct {
	NotePath  string
	Page      int
	Chunk     int
	Embedding []float32
}

// Store manages note_embeddings CRUD and an in-memory vector cache.
type Store struct {
	db     *sql.DB
	logger *slog.Logger

	mu    sync.RWMutex
	cache []EmbeddingRecord // all embeddings loaded into memory
}

func NewStore(db *sql.DB, logger *slog.Logger) *Store {
	return &Store{db: db, logger: logger}
}

// Save inserts or replaces an embedding for (note_path, page, chunk).
// Also updates the in-memory cache.
func (s *Store) Save(ctx context.Context, notePath string, page, chunk int, embedding []float32, model string) error {
	blob := float32sToBytes(embedding)
	now := time.Now().Unix()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO note_embeddings (note_path, page, chunk, embedding, model, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(note_path, page, chunk) DO UPDATE SET embedding=excluded.embedding, model=excluded.model, created_at=excluded.created_at`,
		notePath, page, chunk, blob, model, now,
	)
	if err != nil {
		return fmt.Errorf("save embedding: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Update or append in cache (keyed by path+page+chunk)
	found := false
	for i := range s.cache {
		if s.cache[i].NotePath == notePath && s.cache[i].Page == page && s.cache[i].Chunk == chunk {
			s.cache[i].Embedding = embedding
			found = true
			break
		}
	}
	if !found {
		s.cache = append(s.cache, EmbeddingRecord{
			NotePath:  notePath,
			Page:      page,
			Chunk:     chunk,
			Embedding: embedding,
		})
	}

	return nil
}

// DeletePage removes all chunk embeddings for a single (note_path, page) from
// the DB and the in-memory cache. Used before re-embedding a page so a now-
// shorter page leaves no stale chunk vectors.
func (s *Store) DeletePage(ctx context.Context, notePath string, page int) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM note_embeddings WHERE note_path = ? AND page = ?`, notePath, page); err != nil {
		return fmt.Errorf("delete page embeddings: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.cache[:0]
	for _, rec := range s.cache {
		if rec.NotePath != notePath || rec.Page != page {
			kept = append(kept, rec)
		}
	}
	s.cache = kept
	return nil
}

// Delete removes all embeddings for a note path (every page) from the DB and the
// in-memory cache, atomically with respect to the cache lock. Used when a synced
// page is deleted/emptied so stale vectors stop surfacing in RAG retrieval.
func (s *Store) Delete(ctx context.Context, notePath string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM note_embeddings WHERE note_path = ?`, notePath); err != nil {
		return fmt.Errorf("delete embeddings: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.cache[:0]
	for _, rec := range s.cache {
		if rec.NotePath != notePath {
			kept = append(kept, rec)
		}
	}
	s.cache = kept
	return nil
}

// LoadAll reads all embeddings from the database into the in-memory cache.
// Call this on startup.
func (s *Store) LoadAll(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT note_path, page, chunk, embedding FROM note_embeddings`)
	if err != nil {
		return 0, fmt.Errorf("query embeddings: %w", err)
	}
	defer rows.Close()

	var records []EmbeddingRecord
	for rows.Next() {
		var rec EmbeddingRecord
		var blob []byte
		if err := rows.Scan(&rec.NotePath, &rec.Page, &rec.Chunk, &blob); err != nil {
			return 0, fmt.Errorf("scan embedding: %w", err)
		}
		rec.Embedding = bytesToFloat32s(blob)
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate embeddings: %w", err)
	}

	s.mu.Lock()
	s.cache = records
	s.mu.Unlock()

	return len(records), nil
}

// AllEmbeddings returns a snapshot of all cached embeddings.
// Used by the retriever for vector similarity search.
func (s *Store) AllEmbeddings() []EmbeddingRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]EmbeddingRecord, len(s.cache))
	for i, rec := range s.cache {
		out[i].NotePath = rec.NotePath
		out[i].Page = rec.Page
		out[i].Chunk = rec.Chunk
		out[i].Embedding = make([]float32, len(rec.Embedding))
		copy(out[i].Embedding, rec.Embedding)
	}
	return out
}

// UnembeddedPages returns (note_path, page, body_text) for pages in note_content
// that have no corresponding note_embeddings row.
func (s *Store) UnembeddedPages(ctx context.Context) ([]struct {
	NotePath string
	Page     int
	BodyText string
}, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT nc.note_path, nc.page, nc.body_text
		 FROM note_content nc
		 LEFT JOIN note_embeddings ne ON nc.note_path = ne.note_path AND nc.page = ne.page
		 WHERE ne.note_path IS NULL AND nc.body_text != ''`)
	if err != nil {
		return nil, fmt.Errorf("query unembedded: %w", err)
	}
	defer rows.Close()

	var pages []struct {
		NotePath string
		Page     int
		BodyText string
	}
	for rows.Next() {
		var p struct {
			NotePath string
			Page     int
			BodyText string
		}
		if err := rows.Scan(&p.NotePath, &p.Page, &p.BodyText); err != nil {
			return nil, fmt.Errorf("scan unembedded: %w", err)
		}
		pages = append(pages, p)
	}
	return pages, rows.Err()
}

// float32sToBytes converts a float32 slice to a byte slice (little-endian).
func float32sToBytes(fs []float32) []byte {
	buf := make([]byte, len(fs)*4)
	for i, f := range fs {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

// bytesToFloat32s converts a byte slice to a float32 slice (little-endian).
func bytesToFloat32s(b []byte) []float32 {
	n := len(b) / 4
	fs := make([]float32, n)
	for i := range fs {
		fs[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return fs
}

// CosineSimilarity computes the cosine similarity between two vectors.
func CosineSimilarity(a, b []float32) float32 {
	if len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float32
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / float32(math.Sqrt(float64(normA)*float64(normB)))
}
