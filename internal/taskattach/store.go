package taskattach

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
)

// errBadKey is returned when an attachment key isn't a sha256 hex digest. The
// key flows in from a URL path segment, so validating it before any filesystem
// access is what keeps a crafted key (e.g. one containing "../") from escaping
// Root — the URL signature must never be the only thing between a caller and
// the filesystem.
var errBadKey = errors.New("taskattach: key is not a sha256 hex digest")

// validSHA reports whether s is exactly 64 lowercase hex chars.
func validSHA(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// BlobStore is a content-addressed file store for inline-binary attachment
// bytes lifted out of the ical_blob (the de-bloat path). Content is keyed by
// sha256, so identical attachments across tasks dedup to one file on disk; the
// tasks DB keeps only a reference, never the megabytes.
type BlobStore struct{ Root string }

// Put writes data content-addressed by its sha256, returning the hex digest
// and byte size. Idempotent: if the content already exists the existing file
// is left untouched (dedup). Writes go to a temp file and are renamed into
// place so a concurrent reader never observes a partial blob.
func (b BlobStore) Put(data []byte) (sha string, size int64, err error) {
	sum := sha256.Sum256(data)
	sha = hex.EncodeToString(sum[:])
	dst := b.pathFor(sha)
	if fi, statErr := os.Stat(dst); statErr == nil {
		return sha, fi.Size(), nil // already present — dedup
	}
	dir := filepath.Dir(dst)
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", 0, err
	}
	tmpName := tmp.Name()
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", 0, err
	}
	if err = tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", 0, err
	}
	if err = os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return "", 0, err
	}
	return sha, int64(len(data)), nil
}

// Open returns the stored content as a *os.File (an io.ReadSeeker, so
// http.ServeContent can serve range requests) plus its size. The caller closes
// the file.
func (b BlobStore) Open(sha string) (*os.File, int64, error) {
	if !validSHA(sha) {
		return nil, 0, errBadKey
	}
	f, err := os.Open(b.pathFor(sha))
	if err != nil {
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, fi.Size(), nil
}

// pathFor shards by the first two hex bytes (<Root>/aa/bb/<sha>) so one
// directory never accumulates every attachment.
func (b BlobStore) pathFor(sha string) string {
	if len(sha) < 4 {
		return filepath.Join(b.Root, sha)
	}
	return filepath.Join(b.Root, sha[0:2], sha[2:4], sha)
}
