package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestBackupIncludesCommittedWALRefusesOverwriteAndDoesNotAlterSource(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	bytes := []byte("A whole, unflattened book")
	sum := sha256.Sum256(bytes)
	id := hex.EncodeToString(sum[:])
	for _, stmt := range []string{"PRAGMA journal_mode=WAL", "PRAGMA wal_autocheckpoint=0",
		"CREATE TABLE fn_reader_book(id TEXT PRIMARY KEY,asset_id TEXT,byte_length INTEGER)",
		"CREATE TABLE rhizome_asset(asset_id TEXT PRIMARY KEY,state TEXT,byte_length INTEGER)",
		"CREATE TABLE rhizome_asset_chunk(asset_id TEXT,chunk_index INTEGER,sha256 TEXT,bytes BLOB)",
		"CREATE TABLE preserved(id TEXT PRIMARY KEY,state TEXT)",
		"INSERT INTO preserved VALUES('session','cancelled')"} {
		if _, err = db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO fn_reader_book VALUES(?,?,?)", []any{id, id, len(bytes)}},
		{"INSERT INTO rhizome_asset VALUES(?,'ready',?)", []any{id, len(bytes)}},
		{"INSERT INTO rhizome_asset_chunk VALUES(?,0,?,?)", []any{id, id, bytes}},
	} {
		if _, err = db.Exec(row.sql, row.args...); err != nil {
			t.Fatal(err)
		}
	}
	before, err := inventoryFixture(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	for _, metadata := range []bool{false, true} {
		name := "full.db"
		if metadata {
			name = "metadata.db"
		}
		path := filepath.Join(dir, name)
		result, err := backupFixture(ctx, db, path, metadata)
		if err != nil {
			t.Fatal(err)
		}
		if result["backup_complete"] == metadata {
			t.Fatalf("incorrect completeness: %v", result)
		}
		original, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = backupFixture(ctx, db, path, metadata); err == nil {
			t.Fatal("overwrote existing snapshot")
		}
		after, err := os.ReadFile(path)
		if err != nil || !reflect.DeepEqual(original, after) {
			t.Fatal("existing snapshot changed")
		}
		source, err := inventoryFixture(ctx, db)
		if err != nil || !reflect.DeepEqual(before, source) {
			t.Fatal("source changed")
		}
		copy, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		restored, err := inventoryFixture(ctx, copy)
		copy.Close()
		if err != nil || !reflect.DeepEqual(restored, result) {
			t.Fatalf("snapshot did not reopen identically: %v", err)
		}
	}
	// A ready flag alone must not qualify a corrupt snapshot as a complete backup.
	if _, err = db.Exec("UPDATE rhizome_asset_chunk SET bytes=?", []byte("corrupt")); err != nil {
		t.Fatal(err)
	}
	corrupt, err := inventoryFixture(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if corrupt["backup_complete"] != false {
		t.Fatal("trusted ready flag without verifying bytes")
	}
}
