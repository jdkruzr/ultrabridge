package notedb

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// TestMigrate_NoteEmbeddingsChunkUpgrade proves the in-place upgrade of a
// pre-chunking note_embeddings table: it gains the `chunk` column + widened
// unique key, existing rows are preserved as chunk 0, and re-running migrate is
// idempotent. This guards the production DB rewrite that fires on restart.
func TestMigrate_NoteEmbeddingsChunkUpgrade(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:test_emb_upgrade?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	// Build the OLD-shape table (no chunk, UNIQUE(note_path,page)) and seed a row.
	mustExec(t, db, `CREATE TABLE note_embeddings (
		note_path TEXT NOT NULL, page INTEGER NOT NULL,
		embedding BLOB NOT NULL, model TEXT NOT NULL, created_at INTEGER NOT NULL,
		UNIQUE(note_path, page))`)
	mustExec(t, db, `INSERT INTO note_embeddings (note_path,page,embedding,model,created_at)
		VALUES ('/old.note', 2, X'01020304', 'legacy-model', 123)`)

	if err := migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// chunk column now exists.
	var hasChunk int
	db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('note_embeddings') WHERE name='chunk'`).Scan(&hasChunk)
	if hasChunk != 1 {
		t.Fatalf("chunk column missing after migrate")
	}

	// Legacy row preserved as chunk 0, data intact.
	var page, chunk, createdAt int
	var model string
	if err := db.QueryRowContext(ctx,
		`SELECT page, chunk, model, created_at FROM note_embeddings WHERE note_path='/old.note'`).
		Scan(&page, &chunk, &model, &createdAt); err != nil {
		t.Fatalf("legacy row lost: %v", err)
	}
	if page != 2 || chunk != 0 || model != "legacy-model" || createdAt != 123 {
		t.Errorf("legacy row mangled: page=%d chunk=%d model=%q created=%d", page, chunk, model, createdAt)
	}

	// New widened unique key allows multiple chunks per page.
	mustExec(t, db, `INSERT INTO note_embeddings (note_path,page,chunk,embedding,model,created_at)
		VALUES ('/old.note', 2, 1, X'05', 'm', 9)`)

	// Idempotent: a second migrate is a no-op (no error, chunk row still there).
	if err := migrate(ctx, db); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var rows int
	db.QueryRowContext(ctx, `SELECT COUNT(*) FROM note_embeddings WHERE note_path='/old.note'`).Scan(&rows)
	if rows != 2 {
		t.Errorf("expected 2 chunk rows after idempotent re-migrate, got %d", rows)
	}
}

func mustExec(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}
