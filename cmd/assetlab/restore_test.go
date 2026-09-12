package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sysop/ultrabridge/internal/readerstore"
	"github.com/sysop/ultrabridge/internal/syncassets"
	"github.com/sysop/ultrabridge/internal/syncidentity"
	"github.com/sysop/ultrabridge/internal/syncstore"
)

func restoreSeed(t *testing.T) (context.Context, *sql.DB, string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "source.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	for _, init := range []func(context.Context, *sql.DB) error{syncstore.Migrate, syncassets.Migrate, readerstore.Install, syncidentity.Install} {
		if err = init(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{"PRAGMA journal_mode=WAL", "PRAGMA wal_autocheckpoint=0", `INSERT INTO fn_notebook(id,name,sort_order,created_at,lww_site_id,lww_op_seq,lww_wall_ts) VALUES('kept','Kept',0,0,'0000000000000000000000000B',17,1900000000000)`} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return ctx, db, path
}

func TestRestoreFencesCredentialsRetiresMirrorReplicaAndReseedsServerWithoutReauthoring(t *testing.T) {
	ctx, db, source := restoreSeed(t)
	token := syncidentity.TokenPrefix + fmt.Sprintf("%064x", 123)
	hash := sha256.Sum256([]byte(token))
	if err := (syncidentity.Store{DB: db}).Enroll(ctx, syncidentity.Enrollment{SiteID: "0000000000000000000000000A", TokenHash: fmt.Sprintf("%x", hash)}); err != nil {
		t.Fatal(err)
	}
	before, err := inventoryFixture(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "restore")
	result, err := prepareRestore(ctx, source, dir, "attempt", func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	again, err := prepareRestore(ctx, source, dir, "attempt", func(string) {})
	if err != nil || !reflect.DeepEqual(result, again) {
		t.Fatal(again, err)
	}
	path := result["path"].(string)
	if err = validateRestoreStartup(path, false); err == nil {
		t.Fatal("unenrolled restored target admitted")
	}
	if err = validateRestoreStartup(path, true); err != nil {
		t.Fatal(err)
	}
	restored, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	s := syncidentity.Store{DB: restored}
	if _, err = s.Resolve(ctx, token); err == nil {
		t.Fatal("old credential revived")
	}
	for _, site := range []string{"0000000000000000000000000A", "0000000000000000000000000B"} {
		if err = s.Enroll(ctx, syncidentity.Enrollment{SiteID: site, TokenHash: fmt.Sprintf("%064x", 456), AdoptLegacy: true}); err == nil {
			t.Fatal("retired replica adopted", site)
		}
	}
	var replica string
	var seq, clock int64
	if err = restored.QueryRow("SELECT site_id,last_op_seq,last_hlc FROM sync_site").Scan(&replica, &seq, &clock); err != nil {
		t.Fatal(err)
	}
	if replica != result["replica"] || seq != 0 || clock < 1900000000000 {
		t.Fatal("bad new server clock/identity", replica, seq, clock)
	}
	after, err := inventoryFixture(ctx, db)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("source changed", err)
	}
	copy, err := inventoryFixture(ctx, restored)
	if err != nil {
		t.Fatal(err)
	}
	for table, hash := range before["tables"].(map[string]string) {
		if table == "sync_site" || table == "sync_device_identity" || table == "sync_retired_replica" {
			continue
		}
		if copy["tables"].(map[string]string)[table] != hash {
			t.Fatal("history rewritten", table)
		}
	}
	if _, err = prepareRestore(ctx, source, dir, "different", func(string) {}); err == nil {
		t.Fatal("foreign restore request accepted")
	}
}

func TestInterruptedRestoreCannotServeAndRetryKeepsReservedReplica(t *testing.T) {
	for _, point := range []string{"restore_reserved", "restore_snapshot", "restore_fenced", "restore_committed"} {
		t.Run(point, func(t *testing.T) {
			ctx, _, source := restoreSeed(t)
			dir := filepath.Join(t.TempDir(), "restore")
			func() {
				defer func() {
					if recover() == nil {
						t.Fatal("checkpoint not reached")
					}
				}()
				_, err := prepareRestore(ctx, source, dir, "attempt", func(name string) {
					if name == point {
						panic("interrupted")
					}
				})
				if err != nil {
					t.Fatal(err)
				}
			}()
			if point != "restore_committed" && validateRestoreStartup(filepath.Join(dir, "restored.db"), true) == nil {
				t.Fatal("incomplete restore admitted")
			}
			result, err := prepareRestore(ctx, source, dir, "attempt", func(string) {})
			if err != nil {
				t.Fatal(err)
			}
			if err = validateRestoreStartup(result["path"].(string), true); err != nil {
				t.Fatal(err)
			}
		})
	}
}
