package syncsvc

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sysop/ultrabridge/internal/syncstore"
)

// newSvcWithDB mirrors newSvc but also returns the DB handle so tests can
// assert what landed in sync_cursors.
func newSvcWithDB(t *testing.T) (*Service, *sql.DB) {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)",
		filepath.Join(t.TempDir(), "sync.db"))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := syncstore.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return New(syncstore.New(db), 0, nil, nil), db
}

func storedDeviceName(t *testing.T, db *sql.DB, siteID string) string {
	t.Helper()
	var name string
	if err := db.QueryRowContext(context.Background(),
		`SELECT device_name FROM sync_cursors WHERE site_id = ?`, siteID).Scan(&name); err != nil {
		t.Fatalf("read device_name: %v", err)
	}
	return name
}

func TestSync_RecordsDeviceName(t *testing.T) {
	svc, db := newSvcWithDB(t)
	ctx := context.Background()

	r := req(siteA, 0)
	r.DeviceName = "  Viwoods AiPaper  " // surrounding whitespace is trimmed
	if _, err := svc.Sync(ctx, r); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := storedDeviceName(t, db, siteA); got != "Viwoods AiPaper" {
		t.Errorf("device_name = %q, want trimmed %q", got, "Viwoods AiPaper")
	}
}

func TestSync_AbsentDeviceNamePreservesStored(t *testing.T) {
	svc, db := newSvcWithDB(t)
	ctx := context.Background()

	named := req(siteA, 0)
	named.DeviceName = "Tablet"
	if _, err := svc.Sync(ctx, named); err != nil {
		t.Fatalf("named sync: %v", err)
	}
	// An old client never sets the field; the stored name must survive.
	if _, err := svc.Sync(ctx, req(siteA, 0)); err != nil {
		t.Fatalf("unnamed sync: %v", err)
	}
	if got := storedDeviceName(t, db, siteA); got != "Tablet" {
		t.Errorf("device_name = %q, want preserved %q", got, "Tablet")
	}
}

func TestSync_TruncatesOverlongDeviceName(t *testing.T) {
	svc, db := newSvcWithDB(t)
	ctx := context.Background()

	// Multibyte runes prove truncation is rune-wise, not byte-wise.
	r := req(siteA, 0)
	r.DeviceName = strings.Repeat("é", MaxDeviceNameLen+50)
	if _, err := svc.Sync(ctx, r); err != nil {
		t.Fatalf("sync: %v", err)
	}
	got := storedDeviceName(t, db, siteA)
	if runes := []rune(got); len(runes) != MaxDeviceNameLen {
		t.Errorf("stored name is %d runes, want %d", len(runes), MaxDeviceNameLen)
	}
	if !strings.HasPrefix(got, "é") || strings.ContainsRune(got, '�') {
		t.Errorf("truncation mangled the name: %q", got)
	}
}
