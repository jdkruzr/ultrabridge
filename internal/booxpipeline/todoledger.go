package booxpipeline

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/sysop/ultrabridge/internal/taskstore"
)

// TodoLedger remembers every red-ink to-do ever emitted, so re-processing a
// note never re-creates a to-do whose task was since completed, deleted, or
// hard-purged. *Store implements it over the boox_todo_ledger table.
type TodoLedger interface {
	TodoEmitted(ctx context.Context, noteName, text string) (bool, error)
	RecordTodo(ctx context.Context, noteName, text string) error
}

// todoTextKey normalizes to-do text for ledger matching: lowercase, letters
// and digits only, single-spaced. Absorbs the punctuation/spacing drift a
// vision model produces between two transcriptions of the same strokes.
func todoTextKey(text string) string {
	var b strings.Builder
	space := false
	for _, r := range strings.ToLower(text) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteRune(r)
			space = false
		} else {
			space = true
		}
	}
	return b.String()
}

// TodoEmitted reports whether a to-do with this text was already emitted from
// a note with this basename.
func (s *Store) TodoEmitted(ctx context.Context, noteName, text string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM boox_todo_ledger WHERE note_name = ? AND text_key = ?`,
		noteName, todoTextKey(text)).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("query todo ledger: %w", err)
	}
	return n > 0, nil
}

// RecordTodo marks a to-do as emitted. Idempotent.
func (s *Store) RecordTodo(ctx context.Context, noteName, text string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO boox_todo_ledger (note_name, text_key, first_seen) VALUES (?, ?, ?)`,
		noteName, todoTextKey(text), time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("record todo: %w", err)
	}
	return nil
}

// TodoSourceNote extracts the source note basename from a red-ink task's
// Detail, in either format CreateTasksFromTodos has ever written:
//
//	"From Boox red ink in <basename>\nOpen: ..."   (2026-04-14 onward)
//	"From Boox red ink: <absolute path>"           (legacy)
func TodoSourceNote(detail string) (string, bool) {
	const newPrefix, legacyPrefix = "From Boox red ink in ", "From Boox red ink: "
	switch {
	case strings.HasPrefix(detail, newPrefix):
		name := detail[len(newPrefix):]
		if i := strings.IndexByte(name, '\n'); i >= 0 {
			name = name[:i]
		}
		return name, name != ""
	case strings.HasPrefix(detail, legacyPrefix):
		name := filepath.Base(strings.TrimSpace(detail[len(legacyPrefix):]))
		return name, name != "" && name != "."
	}
	return "", false
}

// SeedTodoLedger records every red-ink task in tasks (tombstones included) as
// already emitted, and returns how many ledger rows were newly added. Run at
// startup; idempotent, so it is safe every boot. It covers tasks created
// before the ledger existed, and any created while it was unavailable.
func (s *Store) SeedTodoLedger(ctx context.Context, tasks []taskstore.Task) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin seed tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO boox_todo_ledger (note_name, text_key, first_seen) VALUES (?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("prepare seed: %w", err)
	}
	defer stmt.Close()

	var added int64
	for _, t := range tasks {
		if !t.Title.Valid || !t.Detail.Valid {
			continue
		}
		noteName, ok := TodoSourceNote(t.Detail.String)
		if !ok {
			continue
		}
		key := todoTextKey(t.Title.String)
		if key == "" {
			continue
		}
		res, err := stmt.ExecContext(ctx, noteName, key, t.CreatedAt)
		if err != nil {
			return 0, fmt.Errorf("seed todo ledger: %w", err)
		}
		n, _ := res.RowsAffected()
		added += n
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit seed: %w", err)
	}
	return added, nil
}
