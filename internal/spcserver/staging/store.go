package staging

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sysop/ultrabridge/internal/spcserver/mapping"
)

// stagingDir is the dot-prefixed holding area under FILE_ROOT for in-flight
// uploads. It is skipped by list_folder (dot entries are excluded), so staged
// bytes never appear in the device's browsable tree.
const stagingDir = ".staging"

// Store accepts and finalizes SPC uploads under a single FILE_ROOT, backed by
// the spc_uploads table for apply→finish correlation and orphan cleanup. Now is
// an injectable clock for tests; a nil Now uses time.Now.
type Store struct {
	Root string
	DB   *sql.DB
	Now  func() time.Time
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
	return n, nil
}

// Finalize verifies the staged file's md5 and size against the claimed values
// and, on a match, atomically renames it to its target path under FILE_ROOT
// (SafeResolve-guarded). It returns the absolute promoted path. On any mismatch
// the staged file is left untouched and the target is never created.
func (s *Store) Finalize(ctx context.Context, innerName, claimedMD5 string, claimedSize int64) (string, error) {
	src, err := s.stagingPath(innerName)
	if err != nil {
		return "", err
	}

	var targetPath, fileName string
	if err := s.DB.QueryRowContext(ctx,
		`SELECT target_path, file_name FROM spc_uploads WHERE inner_name = ?`, innerName).
		Scan(&targetPath, &fileName); err != nil {
		return "", fmt.Errorf("staging Finalize lookup %q: %w", innerName, err)
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

	// Concatenate raw (not path.Join, which would Clean a "../" escape away
	// before SafeResolve's depth-walk could reject it).
	dst, err := mapping.SafeResolve(s.Root, targetPath+"/"+fileName)
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
