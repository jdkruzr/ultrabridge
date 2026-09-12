package readerstore_test

import (
	"context"
	"database/sql"
	. "github.com/sysop/ultrabridge/internal/readerstore"
	"github.com/sysop/ultrabridge/internal/syncassets"
	"github.com/sysop/ultrabridge/internal/syncstore"
	_ "modernc.org/sqlite"
	"path/filepath"
	"testing"
)

var ctx = context.Background()

const siteA = "0000000000000000000000000A"

func open(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}
func exec(t *testing.T, db *sql.DB, stmt string, args ...any) {
	t.Helper()
	if _, err := db.Exec(stmt, args...); err != nil {
		t.Fatal(err)
	}
}
func count(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestInstallAdditiveIdempotentAndMismatchRollsBack(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "notes.db"))
	if err := syncstore.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := syncassets.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	exec(t, db, "PRAGMA user_version=77")
	exec(t, db, "INSERT INTO fn_page_text_from_client(id,text,ocr_at,created_at,lww_wall_ts,lww_op_seq,lww_site_id) VALUES('keep','existing OCR',0,0,1,1,?)", siteA)
	beforeHash := syncstore.SchemaHash()
	for i := 0; i < 2; i++ {
		if err := Install(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	var version int
	db.QueryRow("PRAGMA user_version").Scan(&version)
	if version != 77 || count(t, db, "fn_page_text_from_client") != 1 || syncstore.SchemaHash() != beforeHash || count(t, db, "sync_ops") != 0 {
		t.Fatal("modified writer state")
	}
	broken := open(t, filepath.Join(t.TempDir(), "broken.db"))
	exec(t, broken, "CREATE TABLE fn_reader_stroke(id TEXT PRIMARY KEY,wrong TEXT)")
	exec(t, broken, "CREATE TABLE keep(value TEXT)")
	exec(t, broken, "INSERT INTO keep VALUES('untouched')")
	if Install(ctx, broken) == nil {
		t.Fatal("incompatible schema accepted")
	}
	var n int
	broken.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='fn_reader_book'").Scan(&n)
	if n != 0 || count(t, broken, "keep") != 1 {
		t.Fatal("partial install or data loss")
	}
	exec(t, db, "UPDATE reader_store_version SET version=99")
	if Install(ctx, db) == nil {
		t.Fatal("future store version accepted")
	}
}
