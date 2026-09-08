package assets

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
)

// SQLStore uses the host's SQLite library. The host owns the driver, connection
// lifetime, durable journal policy and quotas. No whole-asset DB transaction.
type SQLStore struct{ DB *sql.DB }

// Migrate is deliberately explicit; constructing a store never changes a DB.
func (s *SQLStore) Migrate(ctx context.Context) error {
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS rhizome_asset (
		 asset_id TEXT PRIMARY KEY, byte_length INTEGER NOT NULL CHECK(byte_length >= 0),
		 chunk_bytes INTEGER NOT NULL CHECK(chunk_bytes = 262144),
		 state TEXT NOT NULL CHECK(state IN ('staging','verifying','ready','invalid')),
		 generation INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS rhizome_asset_chunk (
		 asset_id TEXT NOT NULL, chunk_index INTEGER NOT NULL CHECK(chunk_index >= 0),
		 sha256 TEXT NOT NULL, bytes BLOB NOT NULL CHECK(length(bytes) BETWEEN 1 AND 262144),
		 PRIMARY KEY(asset_id, chunk_index))`,
	} {
		if _, err := s.DB.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func describe(ctx context.Context, q queryer, id string) (Info, int64, error) {
	var info Info
	var generation int64
	if !ValidID(id) {
		return info, 0, Fail(400, "invalid_asset_id")
	}
	err := q.QueryRowContext(ctx, `SELECT asset_id,byte_length,chunk_bytes,state,generation FROM rhizome_asset WHERE asset_id=?`, id).
		Scan(&info.ID, &info.ByteLength, &info.ChunkBytes, &info.State, &generation)
	if errors.Is(err, sql.ErrNoRows) {
		err = Fail(404, "asset_not_found")
	}
	return info, generation, err
}

func (s *SQLStore) Describe(ctx context.Context, id string) (Info, error) {
	i, _, err := describe(ctx, s.DB, id)
	return i, err
}

func (s *SQLStore) Stage(ctx context.Context, d Descriptor) (Info, bool, error) {
	if err := d.Validate(); err != nil {
		return Info{}, false, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Info{}, false, err
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO rhizome_asset(asset_id,byte_length,chunk_bytes,state) VALUES(?,?,?,'staging')`, d.ID, d.ByteLength, d.ChunkBytes)
	if err != nil {
		return Info{}, false, err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return Info{}, false, err
	}
	i, _, err := describe(ctx, tx, d.ID)
	if err != nil {
		return Info{}, false, err
	}
	if i.Descriptor != d {
		return Info{}, false, Fail(409, "descriptor_conflict")
	}
	return i, n == 1, tx.Commit()
}

func (s *SQLStore) ListChunks(ctx context.Context, id string, start int64, limit int) (Page, error) {
	i, err := s.Describe(ctx, id)
	if err != nil {
		return Page{}, err
	}
	if start < 0 || start > i.ChunkCount() || limit < 1 || limit > PageEntries {
		return Page{}, Fail(400, "invalid_page")
	}
	end := start + int64(limit)
	if end > i.ChunkCount() {
		end = i.ChunkCount()
	}
	p := Page{Entries: make([]Entry, 0, end-start)}
	// One bounded query, including missing indices in the wire projection.
	rows, err := s.DB.QueryContext(ctx, `SELECT chunk_index,sha256 FROM rhizome_asset_chunk WHERE asset_id=? AND chunk_index>=? AND chunk_index<? ORDER BY chunk_index`, id, start, end)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()
	present := make(map[int64]string)
	for rows.Next() {
		var index int64
		var digest string
		if err := rows.Scan(&index, &digest); err != nil {
			return Page{}, err
		}
		present[index] = digest
	}
	if err := rows.Err(); err != nil {
		return Page{}, err
	}
	for index := start; index < end; index++ {
		length, _ := i.ChunkLength(index)
		e := Entry{Index: index, ByteLength: length}
		if digest, ok := present[index]; ok {
			e.SHA256 = &digest
		}
		p.Entries = append(p.Entries, e)
	}
	if end < i.ChunkCount() {
		p.NextStart = &end
	}
	return p, nil
}

func (s *SQLStore) ReadChunk(ctx context.Context, id string, index int64) (Chunk, error) {
	i, err := s.Describe(ctx, id)
	if err != nil {
		return Chunk{}, err
	}
	if _, err := i.ChunkLength(index); err != nil {
		return Chunk{}, err
	}
	if i.State != "ready" {
		return Chunk{}, Fail(409, "asset_not_ready")
	}
	return s.readStoredChunk(ctx, id, index)
}

func (s *SQLStore) readStoredChunk(ctx context.Context, id string, index int64) (Chunk, error) {
	var c Chunk
	err := s.DB.QueryRowContext(ctx, `SELECT sha256,bytes FROM rhizome_asset_chunk WHERE asset_id=? AND chunk_index=?`, id, index).Scan(&c.SHA256, &c.Bytes)
	if errors.Is(err, sql.ErrNoRows) {
		err = Fail(409, "missing_chunks")
	}
	return c, err
}

func (s *SQLStore) WriteChunk(ctx context.Context, id string, index int64, b []byte, digest string) error {
	// Hash before acquiring the writer; a rejected chunk never gets a DB row.
	if len(b) > ChunkBytes {
		return Fail(413, "chunk_too_large")
	}
	if !ValidID(digest) {
		return Fail(400, "invalid_chunk_digest")
	}
	if Digest(b) != digest {
		return Fail(422, "chunk_hash_mismatch")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Acquire SQLite's writer before reading mutable state (avoids snapshot upgrades).
	if _, err = tx.ExecContext(ctx, `UPDATE rhizome_asset SET generation=generation WHERE asset_id=?`, id); err != nil {
		return err
	}
	i, _, err := describe(ctx, tx, id)
	if err != nil {
		return err
	}
	length, err := i.ChunkLength(index)
	if err != nil {
		return err
	}
	if len(b) != length {
		return Fail(400, "invalid_chunk_length")
	}
	var old string
	err = tx.QueryRowContext(ctx, `SELECT sha256 FROM rhizome_asset_chunk WHERE asset_id=? AND chunk_index=?`, id, index).Scan(&old)
	if err == nil {
		if old != digest {
			return Fail(409, "chunk_conflict")
		}
		if i.State == "invalid" {
			return Fail(409, "asset_invalid")
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if i.State != "staging" {
		return Fail(409, "asset_not_staging")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO rhizome_asset_chunk(asset_id,chunk_index,sha256,bytes) VALUES(?,?,?,?)`, id, index, digest, b); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLStore) Complete(ctx context.Context, id string) (Info, error) {
	// Verifying is durable. Another request after a crash restarts hashing. The
	// generation CAS prevents an older verifier publishing after an invalid reset.
	if !ValidID(id) {
		return Info{}, Fail(400, "invalid_asset_id")
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE rhizome_asset SET state='verifying' WHERE asset_id=? AND state='staging'`, id); err != nil {
		return Info{}, err
	}
	i, generation, err := describe(ctx, s.DB, id)
	if err != nil {
		return Info{}, err
	}
	if i.State == "ready" {
		return i, nil
	}
	if i.State != "verifying" {
		return Info{}, Fail(409, "asset_invalid")
	}
	h := sha256.New()
	for index := int64(0); index < i.ChunkCount(); index++ {
		c, err := s.readStoredChunk(ctx, id, index)
		if err != nil {
			var ae *Error
			if errors.As(err, &ae) && ae.Code == "missing_chunks" {
				_, resetErr := s.DB.ExecContext(ctx, `UPDATE rhizome_asset SET state='staging' WHERE asset_id=? AND state='verifying' AND generation=?`, id, generation)
				if resetErr != nil {
					return Info{}, resetErr
				}
			}
			return Info{}, err
		}
		length, _ := i.ChunkLength(index)
		if len(c.Bytes) != length || Digest(c.Bytes) != c.SHA256 {
			return s.finish(ctx, i, generation, "invalid")
		}
		_, _ = h.Write(c.Bytes)
	}
	if hex.EncodeToString(h.Sum(nil)) != id {
		return s.finish(ctx, i, generation, "invalid")
	}
	return s.finish(ctx, i, generation, "ready")
}

func (s *SQLStore) finish(ctx context.Context, i Info, generation int64, state string) (Info, error) {
	r, err := s.DB.ExecContext(ctx, `UPDATE rhizome_asset SET state=? WHERE asset_id=? AND state='verifying' AND generation=?`, state, i.ID, generation)
	if err != nil {
		return Info{}, err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return Info{}, err
	}
	if n == 0 {
		current, err := s.Describe(ctx, i.ID)
		if err != nil {
			return Info{}, err
		}
		if current.State == "ready" {
			return current, nil
		}
		return Info{}, Fail(409, "verification_changed")
	}
	if state == "invalid" {
		return Info{}, Fail(422, "asset_hash_mismatch")
	}
	i.State = state
	return i, nil
}

func (s *SQLStore) ResetInvalid(ctx context.Context, id string) error {
	if !ValidID(id) {
		return Fail(400, "invalid_asset_id")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, `UPDATE rhizome_asset SET state='staging',generation=generation+1 WHERE asset_id=? AND state='invalid'`, id)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return Fail(409, "asset_not_invalid")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM rhizome_asset_chunk WHERE asset_id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}
