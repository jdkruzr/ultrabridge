package staging

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sysop/ultrabridge/internal/notedb"
)

func newStore(t *testing.T, now time.Time) (*Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	db, err := notedb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	root := t.TempDir()
	clk := now
	return &Store{Root: root, DB: db, Now: func() time.Time { return clk }}, ctx
}

func md5Hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

// AC1.2 Stage: bytes land under .staging/<innerName>; bad innerName rejected.
func TestStageWritesAndRejectsBadInnerName(t *testing.T) {
	s, _ := newStore(t, time.Unix(1000, 0))
	body := []byte("hello supernote")

	n, err := s.Stage("inner-abc", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if n != int64(len(body)) {
		t.Fatalf("Stage wrote %d, want %d", n, len(body))
	}
	got, err := os.ReadFile(filepath.Join(s.Root, stagingDir, "inner-abc"))
	if err != nil {
		t.Fatalf("read staged: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("staged bytes mismatch: %q", got)
	}

	for _, bad := range []string{"", "a/b", "..", "../x", `a\b`} {
		if _, err := s.Stage(bad, strings.NewReader("x")); err == nil {
			t.Fatalf("Stage(%q) should be rejected", bad)
		}
	}
}

// AC1.3 Verify: md5 or size mismatch rejects without promoting.
func TestFinalizeRejectsMismatch(t *testing.T) {
	s, ctx := newStore(t, time.Unix(1000, 0))
	body := []byte("note-bytes-v1")
	if err := s.Record(ctx, "inner-1", "/Note", "foo.note", int64(len(body)), time.Hour); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if _, err := s.Stage("inner-1", strings.NewReader(string(body))); err != nil {
		t.Fatalf("Stage: %v", err)
	}

	// Wrong md5.
	if _, err := s.Finalize(ctx, "inner-1", "00000000000000000000000000000000", int64(len(body))); err == nil {
		t.Fatalf("Finalize with wrong md5 should fail")
	}
	// Wrong size.
	if _, err := s.Finalize(ctx, "inner-1", md5Hex(body), 999); err == nil {
		t.Fatalf("Finalize with wrong size should fail")
	}
	// Target must not exist.
	if _, err := os.Stat(filepath.Join(s.Root, "Note", "foo.note")); !os.IsNotExist(err) {
		t.Fatalf("target should not exist after rejected finalize, err=%v", err)
	}
}

// AC1.4 Promote: correct md5/size → atomic rename to target; staging temp gone.
func TestFinalizePromotes(t *testing.T) {
	s, ctx := newStore(t, time.Unix(1000, 0))
	body := []byte("note-bytes-final")
	if err := s.Record(ctx, "inner-2", "/Note/Personal", "bar.note", int64(len(body)), time.Hour); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if _, err := s.Stage("inner-2", strings.NewReader(string(body))); err != nil {
		t.Fatalf("Stage: %v", err)
	}

	abs, err := s.Finalize(ctx, "inner-2", md5Hex(body), int64(len(body)))
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	want := filepath.Join(s.Root, "Note", "Personal", "bar.note")
	if abs != want {
		t.Fatalf("promoted to %q, want %q", abs, want)
	}
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("target bytes mismatch")
	}
	// Staging temp cleaned up.
	if _, err := os.Stat(filepath.Join(s.Root, stagingDir, "inner-2")); !os.IsNotExist(err) {
		t.Fatalf("staged temp should be gone, err=%v", err)
	}
}

// AC1.4 traversal: a recorded target escaping the root is refused.
func TestFinalizeRefusesTraversal(t *testing.T) {
	s, ctx := newStore(t, time.Unix(1000, 0))
	body := []byte("x")
	if err := s.Record(ctx, "inner-3", "/../../etc", "passwd", int64(len(body)), time.Hour); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if _, err := s.Stage("inner-3", strings.NewReader(string(body))); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, err := s.Finalize(ctx, "inner-3", md5Hex(body), int64(len(body))); err == nil {
		t.Fatalf("Finalize with escaping target should be refused")
	}
}

// AC1.5 Sweep: stale applied staged files removed; fresh kept.
func TestSweepRemovesOrphans(t *testing.T) {
	start := time.Unix(1000, 0)
	s, ctx := newStore(t, start)

	// Stale: recorded with a 1s TTL, then clock advances well past it.
	if err := s.Record(ctx, "stale", "/Note", "old.note", 1, time.Second); err != nil {
		t.Fatalf("Record stale: %v", err)
	}
	if _, err := s.Stage("stale", strings.NewReader("o")); err != nil {
		t.Fatalf("Stage stale: %v", err)
	}

	// Advance clock past the stale TTL but record a fresh one at the new time.
	s.Now = func() time.Time { return start.Add(time.Hour) }
	if err := s.Record(ctx, "fresh", "/Note", "new.note", 1, time.Hour); err != nil {
		t.Fatalf("Record fresh: %v", err)
	}
	if _, err := s.Stage("fresh", strings.NewReader("n")); err != nil {
		t.Fatalf("Stage fresh: %v", err)
	}

	if err := s.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Root, stagingDir, "stale")); !os.IsNotExist(err) {
		t.Fatalf("stale staged file should be swept, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Root, stagingDir, "fresh")); err != nil {
		t.Fatalf("fresh staged file should survive sweep: %v", err)
	}
}
