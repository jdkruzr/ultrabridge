package readerstore_test

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sysop/ultrabridge/internal/readerstore"
	"github.com/sysop/ultrabridge/internal/syncassets"
	"github.com/sysop/ultrabridge/internal/syncidentity"
	"github.com/sysop/ultrabridge/internal/syncstore"
)

func preservedHostState(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, table := range []string{"fn_notebook", "fn_page_text_from_client", "sync_ops", "sync_seq", "sync_site", "sync_cursors", "rhizome_asset", "rhizome_asset_chunk", "sync_device_identity"} {
		rows, err := db.Query("SELECT * FROM " + table)
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var all []string
		for rows.Next() {
			values := make([]any, len(cols))
			args := make([]any, len(cols))
			for i := range args {
				args[i] = &values[i]
			}
			if err = rows.Scan(args...); err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(values)
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, string(b))
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		// Each fixture table has at most one row. Keep the assertion explicit.
		if len(all) > 1 {
			t.Fatal("Expand snapshot ordering before adding fixture rows")
		}
		out[table] = strings.Join(all, "\n")
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(version)
	out["user_version"] = string(b)
	return out
}

func TestPopulatedHostUpgradeFailurePreservesRelayAssetsAndRevocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mixed.db")
	db := open(t, path)
	for _, install := range []func() error{func() error { return syncstore.Migrate(ctx, db) }, func() error { return syncassets.Migrate(ctx, db) }, func() error { return syncidentity.Install(ctx, db) }} {
		if err := install(); err != nil {
			t.Fatal(err)
		}
	}
	identity := syncidentity.Store{DB: db}
	if err := identity.Enroll(ctx, syncidentity.Enrollment{SiteID: siteA, TokenHash: strings.Repeat("a", 64)}); err != nil {
		t.Fatal(err)
	}
	if err := identity.Revoke(ctx, siteA); err != nil {
		t.Fatal(err)
	}
	store := syncstore.New(db)
	result, err := store.ApplyBatch(ctx, siteA, []syncstore.Op{{Table: "notebook", PK: "00000000000000000000000011", SiteID: siteA, OpSeq: 1, WallTS: 1000,
		Cols: map[string]any{"name": "Keep notebook", "sort_order": float64(0), "created_at": float64(1000), "deleted_at": nil, "folder_id": nil, "aspect_long_axis": nil}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rejected) != 0 {
		t.Fatalf("Invalid fixture: %+v", result)
	}
	if err = store.RecordCursor(ctx, siteA, 7, "Existing device"); err != nil {
		t.Fatal(err)
	}
	data := []byte("original book bytes")
	digest := sha256.Sum256(data)
	id := hex.EncodeToString(digest[:])
	exec(t, db, "INSERT INTO rhizome_asset(asset_id,byte_length,chunk_bytes,state) VALUES(?,?,262144,'ready')", id, len(data))
	exec(t, db, "INSERT INTO rhizome_asset_chunk VALUES(?,0,?,?)", id, id, data)
	exec(t, db, "PRAGMA user_version=77")
	// Fail at the version-advance near the end, after the new mirror DDL.
	exec(t, db, "CREATE TABLE reader_store_version(id INTEGER NOT NULL PRIMARY KEY CHECK(id=1),version INTEGER NOT NULL)")
	exec(t, db, "INSERT INTO reader_store_version VALUES(1,1)")
	exec(t, db, "CREATE TRIGGER fail_upgrade BEFORE UPDATE ON reader_store_version BEGIN SELECT RAISE(ABORT,'migration failure'); END")
	before := preservedHostState(t, db)
	if readerstore.Install(ctx, db) == nil {
		t.Fatal("Failed upgrade accepted")
	}
	if !reflect.DeepEqual(before, preservedHostState(t, db)) {
		t.Fatal("Failed upgrade changed host state")
	}
	var n int
	if err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='fn_reader_book'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("Partial reader schema committed")
	}
	exec(t, db, "DROP TRIGGER fail_upgrade")
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db = open(t, path)
	for i := 0; i < 2; i++ {
		if err = readerstore.Install(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(before, preservedHostState(t, db)) {
		t.Fatal("Retry changed host state")
	}
}
