package taskattach

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestBlobStore_PutOpenRoundTrip(t *testing.T) {
	st := BlobStore{Root: t.TempDir()}
	data := []byte("hello attachment bytes")

	sha, size, err := st.Put(data)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if size != int64(len(data)) {
		t.Errorf("size = %d, want %d", size, len(data))
	}

	f, osize, err := st.Open(sha)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	if osize != int64(len(data)) {
		t.Errorf("Open size = %d, want %d", osize, len(data))
	}
	got, _ := io.ReadAll(f)
	if !bytes.Equal(got, data) {
		t.Errorf("content mismatch: %q vs %q", got, data)
	}
}

func TestBlobStore_PutDedup(t *testing.T) {
	st := BlobStore{Root: t.TempDir()}
	data := []byte("same content")

	sha1, _, err := st.Put(data)
	if err != nil {
		t.Fatal(err)
	}
	sha2, _, err := st.Put(data)
	if err != nil {
		t.Fatal(err)
	}
	if sha1 != sha2 {
		t.Errorf("identical content produced different shas: %q vs %q", sha1, sha2)
	}
	// Distinct content → distinct sha.
	sha3, _, err := st.Put([]byte("different"))
	if err != nil {
		t.Fatal(err)
	}
	if sha3 == sha1 {
		t.Errorf("different content collided to the same sha")
	}
}

func TestBlobStore_OpenMissing(t *testing.T) {
	st := BlobStore{Root: t.TempDir()}
	if _, _, err := st.Open("0000000000000000000000000000000000000000000000000000000000000000"); err == nil {
		t.Errorf("Open of a missing blob should error")
	}
}

// TestBlobStore_OpenRejectsTraversal pins the containment guard: any key that
// isn't a 64-char lowercase hex digest is rejected before touching the
// filesystem, so a crafted key can never escape Root (the URL signature must
// not be the only guard).
func TestBlobStore_OpenRejectsTraversal(t *testing.T) {
	st := BlobStore{Root: t.TempDir()}
	bad := []string{
		"../../../../../../etc/passwd",
		"/etc/passwd",
		"ab/cd/ef",
		"not-hex-at-all",
		strings.Repeat("a", 63),                                            // too short
		strings.Repeat("a", 65),                                            // too long
		strings.ToUpper(strings.Repeat("a", 64)),                           // uppercase rejected
		"....//....//" + strings.Repeat("a", 52),                           // embedded traversal
		strings.Repeat("a", 62) + "/.",                                     // wrong length + dot
	}
	for _, k := range bad {
		// Assert the dedicated guard (errBadKey) fired BEFORE any os.Open, not
		// just that the open happened to fail — that's what proves containment.
		if _, _, err := st.Open(k); !errors.Is(err, errBadKey) {
			t.Errorf("Open(%q) should be rejected pre-filesystem with errBadKey, got %v", k, err)
		}
	}
}
