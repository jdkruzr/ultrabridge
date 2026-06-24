package remarkable

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	errTokenNotFound      = errors.New("remarkable token not found")
	errBlobNotFound       = errors.New("remarkable blob not found")
	errDocumentNotFound   = errors.New("remarkable document not found")
	errGenerationMismatch = errors.New("remarkable generation mismatch")
)

type DeviceRow struct {
	DeviceID   string
	DeviceDesc string
	CreatedAt  int64
	LastSeen   int64
}

type documentMeta struct {
	ID             string `json:"ID"`
	Version        int    `json:"Version"`
	Message        string `json:"Message,omitempty"`
	Success        bool   `json:"Success"`
	BlobURLGet     string `json:"BlobURLGet,omitempty"`
	BlobURLExpires string `json:"BlobURLGetExpires,omitempty"`
	ModifiedClient string `json:"ModifiedClient"`
	Type           string `json:"Type"`
	VisibleName    string `json:"VissibleName"`
	CurrentPage    int    `json:"CurrentPage"`
	Bookmarked     bool   `json:"Bookmarked"`
	Parent         string `json:"Parent"`
}

type tokenClaims struct {
	UserID     string
	DeviceID   string
	DeviceDesc string
	Scopes     string
}

type presignedTarget struct {
	Kind       string
	Scope      string
	DocumentID string
	BlobID     string
}

type blobRecord struct {
	Generation int64
	Size       int64
	CRC32C     string
	Path       string
}

type store struct {
	db       *sql.DB
	dataPath string
}

func migrate(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS remarkable_devices (
			device_id TEXT PRIMARY KEY,
			device_desc TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			last_seen INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS remarkable_tokens (
			token TEXT PRIMARY KEY,
			token_kind TEXT NOT NULL,
			device_id TEXT NOT NULL,
			device_desc TEXT NOT NULL,
			scopes TEXT NOT NULL DEFAULT '',
			expires_at INTEGER NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS remarkable_presigned (
			token TEXT PRIMARY KEY,
			target_kind TEXT NOT NULL,
			scope TEXT NOT NULL,
			document_id TEXT NOT NULL DEFAULT '',
			blob_id TEXT NOT NULL DEFAULT '',
			expires_at INTEGER NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS remarkable_documents (
			id TEXT PRIMARY KEY,
			version INTEGER NOT NULL DEFAULT 1,
			modified_client TEXT NOT NULL DEFAULT '',
			doc_type TEXT NOT NULL DEFAULT '',
			visible_name TEXT NOT NULL DEFAULT '',
			current_page INTEGER NOT NULL DEFAULT 0,
			bookmarked INTEGER NOT NULL DEFAULT 0,
			parent_id TEXT NOT NULL DEFAULT '',
			payload_path TEXT NOT NULL DEFAULT '',
			deleted INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS remarkable_blobs (
			blob_id TEXT PRIMARY KEY,
			generation INTEGER NOT NULL,
			size_bytes INTEGER NOT NULL,
			crc32c TEXT NOT NULL,
			payload_path TEXT NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS remarkable_ocr_jobs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			document_id TEXT NOT NULL,
			page INTEGER NOT NULL,
			revision TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'pending',
			attempts INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			queued_at INTEGER NOT NULL DEFAULT 0,
			started_at INTEGER NOT NULL DEFAULT 0,
			finished_at INTEGER NOT NULL DEFAULT 0,
			UNIQUE(document_id, page)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_remarkable_ocr_jobs_status ON remarkable_ocr_jobs(status, queued_at)`,
	}
	for _, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("remarkable migrate: %w", err)
		}
	}
	return nil
}

func newStore(db *sql.DB, dataPath string) *store {
	return &store{db: db, dataPath: dataPath}
}

func (s *store) ensurePaths() error {
	for _, rel := range []string{"documents", "blobs", "rendered"} {
		if err := os.MkdirAll(filepath.Join(s.dataPath, rel), 0o755); err != nil {
			return err
		}
	}
	return nil
}

func (s *store) touchDevice(ctx context.Context, deviceID, deviceDesc string) error {
	now := time.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO remarkable_devices(device_id, device_desc, created_at, last_seen)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(device_id) DO UPDATE SET
			device_desc = excluded.device_desc,
			last_seen = excluded.last_seen
	`, deviceID, deviceDesc, now, now)
	return err
}

func (s *store) listDevices(ctx context.Context) ([]DeviceRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT device_id, device_desc, created_at, last_seen
		FROM remarkable_devices
		ORDER BY last_seen DESC, device_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceRow
	for rows.Next() {
		var d DeviceRow
		if err := rows.Scan(&d.DeviceID, &d.DeviceDesc, &d.CreatedAt, &d.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *store) issueToken(ctx context.Context, kind, deviceID, deviceDesc, scopes string, ttl time.Duration) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	now := time.Now().UnixMilli()
	expires := time.Now().Add(ttl).UnixMilli()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO remarkable_tokens(token, token_kind, device_id, device_desc, scopes, expires_at, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)`,
		token, kind, deviceID, deviceDesc, scopes, expires, now,
	); err != nil {
		return "", err
	}
	return token, nil
}

func (s *store) loadToken(ctx context.Context, token, kind string) (tokenClaims, error) {
	var claims tokenClaims
	var expires int64
	err := s.db.QueryRowContext(ctx, `
		SELECT device_id, device_desc, scopes, expires_at
		FROM remarkable_tokens
		WHERE token = ? AND token_kind = ?`,
		token, kind,
	).Scan(&claims.DeviceID, &claims.DeviceDesc, &claims.Scopes, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return tokenClaims{}, errTokenNotFound
	}
	if err != nil {
		return tokenClaims{}, err
	}
	if expires < time.Now().UnixMilli() {
		return tokenClaims{}, errTokenNotFound
	}
	claims.UserID = "remarkable"
	return claims, nil
}

func (s *store) issuePresigned(ctx context.Context, target presignedTarget, ttl time.Duration) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	now := time.Now().UnixMilli()
	expires := time.Now().Add(ttl).UnixMilli()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO remarkable_presigned(token, target_kind, scope, document_id, blob_id, expires_at, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)`,
		token, target.Kind, target.Scope, target.DocumentID, target.BlobID, expires, now)
	if err != nil {
		return "", err
	}
	return token, nil
}

func (s *store) loadPresigned(ctx context.Context, token string) (presignedTarget, error) {
	var t presignedTarget
	var expires int64
	err := s.db.QueryRowContext(ctx, `
		SELECT target_kind, scope, document_id, blob_id, expires_at
		FROM remarkable_presigned
		WHERE token = ?`, token).
		Scan(&t.Kind, &t.Scope, &t.DocumentID, &t.BlobID, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return presignedTarget{}, errTokenNotFound
	}
	if err != nil {
		return presignedTarget{}, err
	}
	if expires < time.Now().UnixMilli() {
		return presignedTarget{}, errTokenNotFound
	}
	return t, nil
}

func (s *store) putDocument(ctx context.Context, docID string, body io.Reader) error {
	path := filepath.Join(s.dataPath, "documents", digestName(docID))
	size, _, err := writeAtomically(path, body)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO remarkable_documents(id, payload_path, updated_at)
		VALUES(?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			payload_path = excluded.payload_path,
			updated_at = excluded.updated_at`,
		docID, path, now)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE remarkable_documents
		SET version = CASE WHEN version <= 0 THEN 1 ELSE version END,
		    updated_at = ?,
		    deleted = 0
		WHERE id = ?`, now, docID)
	if err != nil {
		return err
	}
	_ = size
	return nil
}

func (s *store) getDocument(docID string) (string, error) {
	var path string
	err := s.db.QueryRow(`SELECT payload_path FROM remarkable_documents WHERE id = ? AND deleted = 0`, docID).Scan(&path)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errDocumentNotFound
	}
	return path, err
}

func (s *store) upsertMetadata(ctx context.Context, meta documentMeta) error {
	now := time.Now().UnixMilli()
	bookmarked := 0
	if meta.Bookmarked {
		bookmarked = 1
	}
	if meta.Version <= 0 {
		meta.Version = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO remarkable_documents(
			id, version, modified_client, doc_type, visible_name, current_page, bookmarked, parent_id, updated_at, deleted
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, 0)
		ON CONFLICT(id) DO UPDATE SET
			version = excluded.version,
			modified_client = excluded.modified_client,
			doc_type = excluded.doc_type,
			visible_name = excluded.visible_name,
			current_page = excluded.current_page,
			bookmarked = excluded.bookmarked,
			parent_id = excluded.parent_id,
			updated_at = excluded.updated_at,
			deleted = 0`,
		meta.ID, meta.Version, meta.ModifiedClient, meta.Type, meta.VisibleName,
		meta.CurrentPage, bookmarked, meta.Parent, now)
	return err
}

func (s *store) listMetadata(ctx context.Context, docID string) ([]documentMeta, error) {
	query := `
		SELECT id, version, modified_client, doc_type, visible_name, current_page, bookmarked, parent_id
		FROM remarkable_documents
		WHERE deleted = 0`
	var args []any
	if docID != "" {
		query += ` AND id = ?`
		args = append(args, docID)
	}
	query += ` ORDER BY updated_at DESC, id ASC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []documentMeta
	for rows.Next() {
		var m documentMeta
		var bookmarked int
		if err := rows.Scan(&m.ID, &m.Version, &m.ModifiedClient, &m.Type, &m.VisibleName, &m.CurrentPage, &bookmarked, &m.Parent); err != nil {
			return nil, err
		}
		m.Bookmarked = bookmarked != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *store) deleteDocument(ctx context.Context, docID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE remarkable_documents
		SET deleted = 1, updated_at = ?
		WHERE id = ?`, time.Now().UnixMilli(), docID)
	return err
}

func (s *store) putBlob(ctx context.Context, blobID string, body io.Reader, matchGeneration int64) (int64, error) {
	path := filepath.Join(s.dataPath, "blobs", digestName(blobID))
	size, crc, err := writeAtomically(path, body)
	if err != nil {
		return 0, err
	}
	now := time.Now().UnixMilli()

	// Hot path: ordinary blob writes carry no optimistic-concurrency check
	// (matchGeneration == 0). Do the read-modify-write as a single atomic
	// UPSERT ... RETURNING so the statement takes the write lock directly. The
	// old "BEGIN; SELECT; INSERT" pattern took a read lock first and then
	// upgraded to write; under the concurrent blob-upload burst two such
	// transactions would each hold a read lock and deadlock on the upgrade.
	// SQLite returns BUSY immediately for that deadlock (busy_timeout cannot
	// wait it out), which was the ~3.5% of 500s during the bulk first sync.
	if matchGeneration == 0 {
		var newGen int64
		err := retryOnBusy(func() error {
			return s.db.QueryRowContext(ctx, `
				INSERT INTO remarkable_blobs(blob_id, generation, size_bytes, crc32c, payload_path, updated_at)
				VALUES(?, 1, ?, ?, ?, ?)
				ON CONFLICT(blob_id) DO UPDATE SET
					generation = remarkable_blobs.generation + 1,
					size_bytes = excluded.size_bytes,
					crc32c = excluded.crc32c,
					payload_path = excluded.payload_path,
					updated_at = excluded.updated_at
				RETURNING generation`,
				blobID, size, crc, path, now).Scan(&newGen)
		})
		if err != nil {
			return 0, err
		}
		return newGen, nil
	}

	// Optimistic path (root commits): verify the caller's expected generation.
	// Effectively single-writer and low-frequency, but retried for safety.
	var newGen int64
	err = retryOnBusy(func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var current blobRecord
		err = tx.QueryRowContext(ctx, `
			SELECT generation, size_bytes, crc32c, payload_path
			FROM remarkable_blobs WHERE blob_id = ?`, blobID).
			Scan(&current.Generation, &current.Size, &current.CRC32C, &current.Path)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if errors.Is(err, sql.ErrNoRows) || current.Generation != matchGeneration {
			return errGenerationMismatch
		}
		g := current.Generation + 1
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO remarkable_blobs(blob_id, generation, size_bytes, crc32c, payload_path, updated_at)
			VALUES(?, ?, ?, ?, ?, ?)
			ON CONFLICT(blob_id) DO UPDATE SET
				generation = excluded.generation,
				size_bytes = excluded.size_bytes,
				crc32c = excluded.crc32c,
				payload_path = excluded.payload_path,
				updated_at = excluded.updated_at`,
			blobID, g, size, crc, path, now); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		newGen = g
		return nil
	})
	if err != nil {
		return 0, err
	}
	return newGen, nil
}

// retryOnBusy replays fn while it returns a transient SQLite busy/locked error.
// busy_timeout handles plain lock waits; this covers the residual cases (e.g. a
// write-write deadlock that SQLite reports immediately) where replaying lets one
// writer win and the other proceed on its next attempt.
func retryOnBusy(fn func() error) error {
	const maxAttempts = 10
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err = fn(); err == nil || !isBusyErr(err) {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 3 * time.Millisecond)
	}
	return err
}

func isBusyErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "sqlite_busy") ||
		strings.Contains(msg, "sqlite_locked")
}

func (s *store) getBlob(ctx context.Context, blobID string) (blobRecord, error) {
	var r blobRecord
	err := s.db.QueryRowContext(ctx, `
		SELECT generation, size_bytes, crc32c, payload_path
		FROM remarkable_blobs WHERE blob_id = ?`, blobID).
		Scan(&r.Generation, &r.Size, &r.CRC32C, &r.Path)
	if errors.Is(err, sql.ErrNoRows) {
		return blobRecord{}, errBlobNotFound
	}
	return r, err
}

func randomToken() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func digestName(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:]) + ".bin"
}

func writeAtomically(path string, src io.Reader) (int64, string, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, "", err
	}
	tmp, err := os.CreateTemp(dir, "tmp-*")
	if err != nil {
		return 0, "", err
	}
	defer os.Remove(tmp.Name())
	h := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	n, err := io.Copy(io.MultiWriter(tmp, h), src)
	if err != nil {
		tmp.Close()
		return 0, "", err
	}
	if err := tmp.Close(); err != nil {
		return 0, "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return 0, "", err
	}
	crc := hex.EncodeToString([]byte{
		byte(h.Sum32() >> 24), byte(h.Sum32() >> 16), byte(h.Sum32() >> 8), byte(h.Sum32()),
	})
	return n, crc, nil
}
