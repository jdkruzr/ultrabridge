package processor

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sysop/ultrabridge/internal/notedb"
)

// openCatalogDB opens an in-memory SQLite DB with the notes schema plus
// the three SPC subset tables. Returns the DB and a spcCatalog backed by it.
func openCatalogDB(t *testing.T) (*sql.DB, *spcCatalog) {
	t.Helper()
	db, err := notedb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mustExec(t, db, `CREATE TABLE f_user_file (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL,
		inner_name TEXT NOT NULL UNIQUE,
		file_name TEXT,
		size INTEGER NOT NULL DEFAULT 0,
		md5 TEXT NOT NULL DEFAULT '',
		update_time INTEGER
	)`)
	mustExec(t, db, `CREATE TABLE f_file_action (
		id INTEGER PRIMARY KEY,
		user_id INTEGER NOT NULL,
		file_id INTEGER NOT NULL,
		file_name TEXT,
		inner_name TEXT,
		path TEXT,
		is_folder TEXT,
		size INTEGER,
		md5 TEXT,
		action TEXT,
		create_time INTEGER,
		update_time INTEGER
	)`)
	mustExec(t, db, `CREATE TABLE f_capacity (
		user_id INTEGER PRIMARY KEY,
		used_capacity INTEGER NOT NULL DEFAULT 0,
		total_capacity INTEGER NOT NULL DEFAULT 0,
		update_time INTEGER
	)`)
	return db, &spcCatalog{db: db}
}

// mustExec runs a SQL statement and fails the test on error.
func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("mustExec %q: %v", query, err)
	}
}

// writeTempFile writes content to a temp file with the given basename and
// returns the full path.
func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAfterInject_UpdatesUserFile verifies AC1.1, AC1.2, AC1.3:
// After injection, f_user_file.size matches file byte count,
// f_user_file.md5 matches hex(md5(file)), and update_time is set.
func TestAfterInject_UpdatesUserFile(t *testing.T) {
	db, catalog := openCatalogDB(t)
	content := "test file content for md5"
	path := writeTempFile(t, "test.note", content)

	// Insert initial f_user_file row and f_capacity row.
	mustExec(t, db, `INSERT INTO f_user_file (user_id, inner_name, file_name, size, md5)
		VALUES (?, ?, ?, ?, ?)`,
		1, filepath.Base(path), "test.note", 100, "oldmd5hash")
	mustExec(t, db, `INSERT INTO f_capacity (user_id, used_capacity)
		VALUES (?, ?)`,
		1, 1000)

	beforeTime := time.Now().UnixMilli()
	err := catalog.AfterInject(context.Background(), path)
	afterTime := time.Now().UnixMilli()

	if err != nil {
		t.Fatalf("AfterInject returned error: %v", err)
	}

	// Compute expected MD5.
	h := md5.New()
	h.Write([]byte(content))
	expectedMD5 := hex.EncodeToString(h.Sum(nil))

	// Query f_user_file to verify updates.
	var size int64
	var md5Hex string
	var updateTime int64
	err = db.QueryRow(`SELECT size, md5, update_time FROM f_user_file WHERE inner_name = ?`,
		filepath.Base(path)).
		Scan(&size, &md5Hex, &updateTime)
	if err != nil {
		t.Fatalf("QueryRow f_user_file: %v", err)
	}

	if size != int64(len(content)) {
		t.Errorf("size mismatch: want %d, got %d", len(content), size)
	}
	if md5Hex != expectedMD5 {
		t.Errorf("md5 mismatch: want %s, got %s", expectedMD5, md5Hex)
	}
	if updateTime < beforeTime || updateTime > afterTime {
		t.Errorf("update_time out of bounds: want [%d, %d], got %d", beforeTime, afterTime, updateTime)
	}
}

// TestAfterInject_MissingUserFile verifies AC1.4:
// If no f_user_file row exists, no error, no update attempted.
func TestAfterInject_MissingUserFile(t *testing.T) {
	db, catalog := openCatalogDB(t)
	content := "file content"
	path := writeTempFile(t, "missing.note", content)

	// Do NOT insert any f_user_file row.

	err := catalog.AfterInject(context.Background(), path)
	if err != nil {
		t.Fatalf("AfterInject returned error: %v", err)
	}

	// Verify f_file_action is empty.
	var actionCount int
	db.QueryRow("SELECT COUNT(*) FROM f_file_action").Scan(&actionCount)
	if actionCount != 0 {
		t.Errorf("f_file_action count: want 0, got %d", actionCount)
	}

	// Verify f_capacity is empty.
	var capacityCount int
	db.QueryRow("SELECT COUNT(*) FROM f_capacity").Scan(&capacityCount)
	if capacityCount != 0 {
		t.Errorf("f_capacity count: want 0, got %d", capacityCount)
	}
}

// TestAfterInject_InsertsFileAction verifies AC2.1, AC2.2:
// A new f_file_action row is inserted with action='A', correct file_id, user_id,
// md5, size, inner_name, and non-zero id with matching create_time/update_time.
func TestAfterInject_InsertsFileAction(t *testing.T) {
	db, catalog := openCatalogDB(t)
	content := "action test content"
	path := writeTempFile(t, "action.note", content)

	// Insert f_user_file and f_capacity.
	mustExec(t, db, `INSERT INTO f_user_file (id, user_id, inner_name, file_name, size, md5)
		VALUES (?, ?, ?, ?, ?, ?)`,
		1, 2, filepath.Base(path), "action.note", 50, "oldmd5")
	mustExec(t, db, `INSERT INTO f_capacity (user_id, used_capacity)
		VALUES (?, ?)`,
		2, 2000)

	err := catalog.AfterInject(context.Background(), path)
	if err != nil {
		t.Fatalf("AfterInject returned error: %v", err)
	}

	// Query f_file_action to verify the row.
	h := md5.New()
	h.Write([]byte(content))
	expectedMD5 := hex.EncodeToString(h.Sum(nil))

	var actionID, fileID, userID, size int64
	var md5Hex, innerName, action string
	var createTime, updateTime int64
	err = db.QueryRow(`SELECT id, file_id, user_id, size, md5, inner_name, action, create_time, update_time
		FROM f_file_action LIMIT 1`).
		Scan(&actionID, &fileID, &userID, &size, &md5Hex, &innerName, &action, &createTime, &updateTime)
	if err != nil {
		t.Fatalf("QueryRow f_file_action: %v", err)
	}

	if actionID == 0 {
		t.Error("action id is zero")
	}
	if fileID != 1 {
		t.Errorf("file_id: want 1, got %d", fileID)
	}
	if userID != 2 {
		t.Errorf("user_id: want 2, got %d", userID)
	}
	if size != int64(len(content)) {
		t.Errorf("size: want %d, got %d", len(content), size)
	}
	if md5Hex != expectedMD5 {
		t.Errorf("md5: want %s, got %s", expectedMD5, md5Hex)
	}
	if innerName != filepath.Base(path) {
		t.Errorf("inner_name: want %s, got %s", filepath.Base(path), innerName)
	}
	if action != "A" {
		t.Errorf("action: want 'A', got %s", action)
	}
	if createTime != updateTime {
		t.Errorf("create_time != update_time: %d != %d", createTime, updateTime)
	}
}

// TestAfterInject_AdjustsCapacity verifies AC3.1:
// f_capacity.used_capacity is updated by new_size - old_size.
func TestAfterInject_AdjustsCapacity(t *testing.T) {
	db, catalog := openCatalogDB(t)
	oldSize := int64(100)
	newContent := strings.Repeat("x", 200) // 200 bytes
	path := writeTempFile(t, "capacity.note", newContent)

	// Insert f_user_file with old size and f_capacity.
	mustExec(t, db, `INSERT INTO f_user_file (id, user_id, inner_name, file_name, size, md5)
		VALUES (?, ?, ?, ?, ?, ?)`,
		1, 3, filepath.Base(path), "capacity.note", oldSize, "oldmd5")
	mustExec(t, db, `INSERT INTO f_capacity (user_id, used_capacity)
		VALUES (?, ?)`,
		3, 1000)

	err := catalog.AfterInject(context.Background(), path)
	if err != nil {
		t.Fatalf("AfterInject returned error: %v", err)
	}

	// Query f_capacity to verify delta application.
	var usedCapacity int64
	db.QueryRow("SELECT used_capacity FROM f_capacity WHERE user_id=?", 3).
		Scan(&usedCapacity)

	expectedCapacity := int64(1000) + (int64(200) - oldSize) // 1100
	if usedCapacity != expectedCapacity {
		t.Errorf("used_capacity: want %d, got %d", expectedCapacity, usedCapacity)
	}
}

// TestAfterInject_ZeroDeltaCapacity verifies AC3.2:
// If new_size == old_size, capacity delta is zero and update proceeds.
func TestAfterInject_ZeroDeltaCapacity(t *testing.T) {
	db, catalog := openCatalogDB(t)
	sizeN := int64(150)
	content := strings.Repeat("y", 150) // Exactly 150 bytes
	path := writeTempFile(t, "zerodelta.note", content)

	// Insert f_user_file with same size and f_capacity.
	mustExec(t, db, `INSERT INTO f_user_file (id, user_id, inner_name, file_name, size, md5)
		VALUES (?, ?, ?, ?, ?, ?)`,
		1, 4, filepath.Base(path), "zerodelta.note", sizeN, "oldmd5")
	mustExec(t, db, `INSERT INTO f_capacity (user_id, used_capacity)
		VALUES (?, ?)`,
		4, 500)

	err := catalog.AfterInject(context.Background(), path)
	if err != nil {
		t.Fatalf("AfterInject returned error: %v", err)
	}

	// Query f_capacity to verify no change.
	var usedCapacity int64
	db.QueryRow("SELECT used_capacity FROM f_capacity WHERE user_id=?", 4).
		Scan(&usedCapacity)

	if usedCapacity != 500 {
		t.Errorf("used_capacity: want 500, got %d", usedCapacity)
	}
}

// TestAfterInject_SelectFails verifies AC4.1:
// If SELECT fails, remaining steps are skipped; job still completes as done.
func TestAfterInject_SelectFails(t *testing.T) {
	// Use a closed database to force SELECT failure.
	db, catalog := openCatalogDB(t)
	db.Close()

	content := "fail test"
	path := writeTempFile(t, "fail.note", content)

	// This should not panic, should return nil (best-effort).
	err := catalog.AfterInject(context.Background(), path)
	if err != nil {
		t.Fatalf("AfterInject returned error: %v", err)
	}
}

// TestAfterInject_UpdateFails_ContinuesToInsertAndCapacity verifies AC4.2:
// If f_user_file UPDATE fails, f_file_action INSERT and f_capacity UPDATE still execute.
func TestAfterInject_UpdateFails_ContinuesToInsertAndCapacity(t *testing.T) {
	db, catalog := openCatalogDB(t)
	content := "update fail test"
	path := writeTempFile(t, "updatefail.note", content)

	// Insert f_user_file and f_capacity.
	mustExec(t, db, `INSERT INTO f_user_file (id, user_id, inner_name, file_name, size, md5)
		VALUES (?, ?, ?, ?, ?, ?)`,
		1, 5, filepath.Base(path), "updatefail.note", 100, "oldmd5")
	mustExec(t, db, `INSERT INTO f_capacity (user_id, used_capacity)
		VALUES (?, ?)`,
		5, 3000)

	// Add a trigger to fail UPDATE on f_user_file.
	mustExec(t, db, `CREATE TRIGGER fail_uf_update BEFORE UPDATE ON f_user_file
		BEGIN SELECT RAISE(FAIL, 'simulated failure'); END`)

	err := catalog.AfterInject(context.Background(), path)
	if err != nil {
		t.Fatalf("AfterInject returned error: %v", err)
	}

	// Verify f_user_file is unchanged (update was blocked).
	var size int64
	var md5Hex string
	db.QueryRow("SELECT size, md5 FROM f_user_file WHERE id=?", 1).Scan(&size, &md5Hex)
	if size != 100 {
		t.Errorf("f_user_file.size: want 100, got %d (update should have failed)", size)
	}
	if md5Hex != "oldmd5" {
		t.Errorf("f_user_file.md5: want oldmd5, got %s (update should have failed)", md5Hex)
	}

	// Verify f_file_action has one row (INSERT still ran).
	var actionCount int
	db.QueryRow("SELECT COUNT(*) FROM f_file_action").Scan(&actionCount)
	if actionCount != 1 {
		t.Errorf("f_file_action count: want 1, got %d (INSERT should have run)", actionCount)
	}

	// Verify f_capacity was updated (UPDATE f_capacity still ran).
	var usedCapacity int64
	db.QueryRow("SELECT used_capacity FROM f_capacity WHERE user_id=?", 5).Scan(&usedCapacity)
	expectedCapacity := int64(3000) + (int64(len(content)) - 100)
	if usedCapacity != expectedCapacity {
		t.Errorf("f_capacity.used_capacity: want %d, got %d (UPDATE should have run)", expectedCapacity, usedCapacity)
	}
}

// TestAfterInject_InsertFails_ContinuesToCapacity verifies AC4.3:
// If f_file_action INSERT fails, f_capacity UPDATE still executes.
func TestAfterInject_InsertFails_ContinuesToCapacity(t *testing.T) {
	db, catalog := openCatalogDB(t)
	content := "insert fail test"
	path := writeTempFile(t, "insertfail.note", content)

	// Insert f_user_file and f_capacity.
	mustExec(t, db, `INSERT INTO f_user_file (id, user_id, inner_name, file_name, size, md5)
		VALUES (?, ?, ?, ?, ?, ?)`,
		1, 6, filepath.Base(path), "insertfail.note", 100, "oldmd5")
	mustExec(t, db, `INSERT INTO f_capacity (user_id, used_capacity)
		VALUES (?, ?)`,
		6, 4000)

	// Add a trigger to fail INSERT on f_file_action.
	mustExec(t, db, `CREATE TRIGGER fail_fa_insert BEFORE INSERT ON f_file_action
		BEGIN SELECT RAISE(FAIL, 'simulated failure'); END`)

	err := catalog.AfterInject(context.Background(), path)
	if err != nil {
		t.Fatalf("AfterInject returned error: %v", err)
	}

	// Verify f_file_action is empty (INSERT was blocked).
	var actionCount int
	db.QueryRow("SELECT COUNT(*) FROM f_file_action").Scan(&actionCount)
	if actionCount != 0 {
		t.Errorf("f_file_action count: want 0, got %d (INSERT should have failed)", actionCount)
	}

	// Verify f_capacity was updated (UPDATE f_capacity still ran).
	var usedCapacity int64
	db.QueryRow("SELECT used_capacity FROM f_capacity WHERE user_id=?", 6).Scan(&usedCapacity)
	expectedCapacity := int64(4000) + (int64(len(content)) - 100)
	if usedCapacity != expectedCapacity {
		t.Errorf("f_capacity.used_capacity: want %d, got %d (UPDATE should have run)", expectedCapacity, usedCapacity)
	}
}
