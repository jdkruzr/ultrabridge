package staging

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sysop/ultrabridge/internal/spcserver/mapping"
)

// stagingDir is the dot-prefixed holding area under FILE_ROOT for in-flight
// uploads. It is skipped by list_folder (dot entries are excluded), so staged
// bytes never appear in the device's browsable tree.
const stagingDir = ".staging"

const maxUploadChunks = 1024

// Store accepts and finalizes SPC uploads under a single FILE_ROOT, backed by
// the spc_uploads table for apply→finish correlation and orphan cleanup. Now is
// an injectable clock for tests; a nil Now uses time.Now.
type Store struct {
	Root string
	DB   *sql.DB
	Now  func() time.Time
	mu   sync.Mutex
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// stagingPath returns the absolute path for innerName under .staging, rejecting
// any name that is empty or contains a path separator or "..". The innerName is
// server-chosen (a UUID), so a non-trivial name means a forged/garbage request.
func (s *Store) stagingPath(innerName string) (string, error) {
	if innerName == "" || strings.ContainsAny(innerName, `/\`) || strings.Contains(innerName, "..") {
		return "", fmt.Errorf("staging: invalid innerName %q", innerName)
	}
	return filepath.Join(s.Root, stagingDir, innerName), nil
}

// Record inserts the apply-time spc_uploads row: the innerName→target mapping
// plus a TTL after which Sweep reclaims an abandoned upload.
func (s *Store) Record(ctx context.Context, innerName, targetPath, fileName string, claimedSize int64, ttl time.Duration) error {
	now := s.now()
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO spc_uploads(inner_name, target_path, file_name, claimed_md5, claimed_size, status, created_at, expires_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		innerName, targetPath, fileName, "", claimedSize, statusApplied,
		now.UnixMilli(), now.Add(ttl).UnixMilli())
	if err != nil {
		return fmt.Errorf("staging Record %q: %w", innerName, err)
	}
	return nil
}

// Stage streams r into .staging/<innerName>, returning the number of bytes
// written. It overwrites any prior partial stage for the same innerName.
func (s *Store) Stage(innerName string, r io.Reader) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stage(innerName, r)
}

func (s *Store) stage(innerName string, r io.Reader) (int64, error) {
	p, err := s.stagingPath(innerName)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return 0, fmt.Errorf("staging mkdir: %w", err)
	}
	f, err := os.Create(p)
	if err != nil {
		return 0, fmt.Errorf("staging create %q: %w", p, err)
	}
	n, copyErr := io.Copy(f, r)
	closeErr := f.Close()
	if copyErr != nil {
		return n, fmt.Errorf("staging write %q: %w", p, copyErr)
	}
	if closeErr != nil {
		return n, fmt.Errorf("staging close %q: %w", p, closeErr)
	}
	_ = os.RemoveAll(filepath.Join(s.Root, stagingDir, ".parts", innerName))
	return n, nil
}

// StagePart stores one chunk for an upload and assembles the ordinary staging
// file as soon as all chunks are present. Chunks may arrive out of order or be
// retried; a retry replaces only that numbered chunk.
func (s *Store) StagePart(innerName, uploadID string, partNumber, totalChunks int, r io.Reader) (int64, bool, error) {
	if uploadID == "" {
		return 0, false, fmt.Errorf("staging: empty uploadId")
	}
	if partNumber < 1 || totalChunks < 1 || totalChunks > maxUploadChunks || partNumber > totalChunks {
		return 0, false, fmt.Errorf("staging: invalid chunk %d of %d", partNumber, totalChunks)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	staged, err := s.stagingPath(innerName)
	if err != nil {
		return 0, false, err
	}
	if _, err := os.Stat(staged); err == nil {
		return 0, true, nil
	} else if !os.IsNotExist(err) {
		return 0, false, fmt.Errorf("staging stat %q: %w", innerName, err)
	}

	idHash := sha256.Sum256([]byte(uploadID))
	partsDir := filepath.Join(s.Root, stagingDir, ".parts", innerName, hex.EncodeToString(idHash[:]), fmt.Sprintf("%d", totalChunks))
	if err := os.MkdirAll(partsDir, 0o755); err != nil {
		return 0, false, fmt.Errorf("staging chunk mkdir: %w", err)
	}
	partPath := filepath.Join(partsDir, fmt.Sprintf("%08d", partNumber))
	tmpPath := partPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return 0, false, fmt.Errorf("staging chunk create: %w", err)
	}
	n, copyErr := io.Copy(f, r)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return n, false, fmt.Errorf("staging chunk write: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return n, false, fmt.Errorf("staging chunk close: %w", closeErr)
	}
	if err := os.Rename(tmpPath, partPath); err != nil {
		_ = os.Remove(tmpPath)
		return n, false, fmt.Errorf("staging chunk commit: %w", err)
	}

	for i := 1; i <= totalChunks; i++ {
		if _, err := os.Stat(filepath.Join(partsDir, fmt.Sprintf("%08d", i))); err != nil {
			if os.IsNotExist(err) {
				return n, false, nil
			}
			return n, false, fmt.Errorf("staging chunk stat: %w", err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(staged), 0o755); err != nil {
		return n, false, fmt.Errorf("staging merge mkdir: %w", err)
	}
	mergedPath := staged + ".merge"
	merged, err := os.Create(mergedPath)
	if err != nil {
		return n, false, fmt.Errorf("staging merge create: %w", err)
	}
	mergeOK := false
	defer func() {
		_ = merged.Close()
		if !mergeOK {
			_ = os.Remove(mergedPath)
		}
	}()
	for i := 1; i <= totalChunks; i++ {
		part, err := os.Open(filepath.Join(partsDir, fmt.Sprintf("%08d", i)))
		if err != nil {
			return n, false, fmt.Errorf("staging merge open: %w", err)
		}
		_, copyErr := io.Copy(merged, part)
		closeErr := part.Close()
		if copyErr != nil {
			return n, false, fmt.Errorf("staging merge copy: %w", copyErr)
		}
		if closeErr != nil {
			return n, false, fmt.Errorf("staging merge close part: %w", closeErr)
		}
	}
	if err := merged.Close(); err != nil {
		return n, false, fmt.Errorf("staging merge close: %w", err)
	}
	if err := os.Rename(mergedPath, staged); err != nil {
		return n, false, fmt.Errorf("staging merge commit: %w", err)
	}
	mergeOK = true
	_ = os.RemoveAll(filepath.Join(s.Root, stagingDir, ".parts", innerName))
	return n, true, nil
}

// Finalize verifies the staged file's md5 and size against the claimed values
// and, on a match, atomically renames it to its target path under FILE_ROOT
// (SafeResolve-guarded). It returns the absolute promoted path. On any mismatch
// the staged file is left untouched and the target is never created.
//
// targetRelPath is the root-relative destination the CALLER computes from the
// upload/finish request (its `path` parent + `fileName`). The promotion target
// is taken from finish rather than the apply-recorded row because the device's
// apply and finish disagree on whether `path` includes the filename (apply sends
// the full path; finish sends the parent dir + a separate fileName — see the §8
// wire note in docs/spc-protocol.md).
func (s *Store) Finalize(ctx context.Context, innerName, claimedMD5 string, claimedSize int64, targetRelPath string) (string, error) {
	src, err := s.stagingPath(innerName)
	if err != nil {
		return "", err
	}

	fi, err := os.Stat(src)
	if err != nil {
		return "", fmt.Errorf("staging Finalize stat %q: %w", innerName, err)
	}
	if claimedSize >= 0 && fi.Size() != claimedSize {
		return "", fmt.Errorf("staging Finalize %q: size mismatch (staged %d, claimed %d)", innerName, fi.Size(), claimedSize)
	}
	sum, err := md5File(src)
	if err != nil {
		return "", err
	}
	if claimedMD5 != "" && !strings.EqualFold(sum, claimedMD5) {
		return "", fmt.Errorf("staging Finalize %q: md5 mismatch (staged %s, claimed %s)", innerName, sum, claimedMD5)
	}

	dst, err := mapping.SafeResolve(s.Root, targetRelPath)
	if err != nil {
		return "", fmt.Errorf("staging Finalize %q: %w", innerName, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", fmt.Errorf("staging Finalize mkdir %q: %w", dst, err)
	}
	if err := os.Rename(src, dst); err != nil {
		return "", fmt.Errorf("staging Finalize rename %q→%q: %w", src, dst, err)
	}

	if _, err := s.DB.ExecContext(ctx,
		`UPDATE spc_uploads SET status = ?, claimed_md5 = ? WHERE inner_name = ?`,
		statusFinalized, sum, innerName); err != nil {
		// The file is already promoted; a status-bookkeeping failure is non-fatal.
		return dst, nil
	}
	return dst, nil
}

// Sweep removes staged files (and their rows) for still-applied uploads whose
// TTL has expired — abandoned applies that never finished.
func (s *Store) Sweep(ctx context.Context) error {
	cutoff := s.now().UnixMilli()
	rows, err := s.DB.QueryContext(ctx,
		`SELECT inner_name FROM spc_uploads WHERE status = ? AND expires_at < ?`,
		statusApplied, cutoff)
	if err != nil {
		return fmt.Errorf("staging Sweep query: %w", err)
	}
	var stale []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return fmt.Errorf("staging Sweep scan: %w", err)
		}
		stale = append(stale, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("staging Sweep rows: %w", err)
	}
	rows.Close()

	for _, name := range stale {
		if p, err := s.stagingPath(name); err == nil {
			_ = os.Remove(p)
		}
		_ = os.RemoveAll(filepath.Join(s.Root, stagingDir, ".parts", name))
		if _, err := s.DB.ExecContext(ctx, `DELETE FROM spc_uploads WHERE inner_name = ?`, name); err != nil {
			return fmt.Errorf("staging Sweep delete %q: %w", name, err)
		}
	}
	return nil
}

// md5File returns the lowercase hex MD5 of the file at path.
func md5File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("staging md5 open %q: %w", path, err)
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("staging md5 read %q: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
