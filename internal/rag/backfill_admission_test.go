package rag

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestBackfillReReadsAfterAdmissionAndSkipsRemovedPages(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(map[bool]string{false: "changed", true: "deleted"}[deleted], func(t *testing.T) {
			db := openTestDB(t)
			defer db.Close()
			store := NewStore(db, slog.Default())
			if _, err := db.Exec(`INSERT INTO note_content(note_path,page,body_text,indexed_at) VALUES('forestnote://book/page',0,'old',1)`); err != nil {
				t.Fatal(err)
			}
			var admissions, embeds int
			store.SetBackfillAdmission(func(ctx context.Context, path string, work func(context.Context) error) error {
				admissions++
				query := `UPDATE note_content SET body_text='restored' WHERE note_path=?`
				if deleted {
					query = `DELETE FROM note_content WHERE note_path=?`
				}
				if _, err := db.ExecContext(ctx, query, path); err != nil {
					return err
				}
				return work(ctx)
			})
			embedder := &mockEmbedder{embedFn: func(ctx context.Context, text string) ([]float32, error) {
				embeds++
				if text != "restored" {
					t.Error("stale inventory reached model", text)
				}
				return []float32{1}, nil
			}}
			n, err := Backfill(context.Background(), store, embedder, "fixture", slog.Default())
			if err != nil || admissions != 1 {
				t.Fatal(n, err, admissions)
			}
			want := 1
			if deleted {
				want = 0
			}
			if n != want || embeds != want || len(store.AllEmbeddings()) != want {
				t.Fatal("wrong publication", n, embeds, store.AllEmbeddings())
			}
		})
	}
}

func TestBackfillAdmissionFailureDoesNotSendOrMutate(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	store := NewStore(db, slog.Default())
	if _, err := db.Exec(`INSERT INTO note_content(note_path,page,body_text,indexed_at) VALUES('forestnote://book/page',0,'private',1)`); err != nil {
		t.Fatal(err)
	}
	store.SetBackfillAdmission(func(context.Context, string, func(context.Context) error) error { return context.Canceled })
	n, err := Backfill(context.Background(), store, &mockEmbedder{embedFn: func(context.Context, string) ([]float32, error) { t.Fatal("closed source sent text"); return nil, nil }}, "fixture", slog.Default())
	if n != 0 || !errors.Is(err, context.Canceled) || len(store.AllEmbeddings()) != 0 {
		t.Fatal(n, err)
	}
}

func TestForgetLibraryPrefixPreservesOtherSourcesAndDatabase(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	store := NewStore(db, slog.Default())
	for _, path := range []string{"forestnote://book/page", "forestnote://other/page", "boox.note", "digest://article", "forestnote-lookalike"} {
		if err := store.Save(context.Background(), path, 0, 0, []float32{1}, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	store.ForgetPrefix("forestnote://")
	cache := store.AllEmbeddings()
	if len(cache) != 3 {
		t.Fatal(cache)
	}
	for _, row := range cache {
		if strings.HasPrefix(row.NotePath, "forestnote://") {
			t.Fatal("old vector survived")
		}
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM note_embeddings`).Scan(&count); err != nil || count != 5 {
		t.Fatal("cache hook mutated SQL", count, err)
	}
}
