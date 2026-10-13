package booxpipeline

import (
	"context"
	"log/slog"
	"net/url"
	"path/filepath"

	"github.com/sysop/ultrabridge/internal/taskstore"
)

// TaskCreator abstracts the subset of caldav.TaskStore needed for todo creation.
type TaskCreator interface {
	List(ctx context.Context) ([]taskstore.Task, error)
	Create(ctx context.Context, t *taskstore.Task) error
}

// CreateTasksFromTodos creates CalDAV tasks from extracted red ink todos,
// deduplicating against existing tasks by title (both incomplete and
// completed) and, when ledger is non-nil, against every to-do ever emitted
// from a note with the same basename — so a to-do whose task was deleted or
// purged stays gone when its note is re-processed. A ledger read error skips
// that to-do: missing one beats re-flooding the task list. externalBaseURL, when non-empty, is prepended to the
// /files/boox?detail=... link in the task's Detail so CalDAV clients see
// an absolute URL they can render as clickable (e.g.
// https://ub.example.com/files/boox?detail=...). Empty fallback emits
// the bare relative path.
func CreateTasksFromTodos(ctx context.Context, tc TaskCreator, ledger TodoLedger, notePath string, todos []TodoItem, externalBaseURL string, logger *slog.Logger) int {
	existing, err := tc.List(ctx)
	if err != nil {
		logger.Error("todo: list tasks for dedup", "error", err)
		return 0
	}
	titleSet := make(map[string]bool, len(existing))
	for _, t := range existing {
		if t.Title.Valid {
			titleSet[t.Title.String] = true
		}
	}

	noteName := filepath.Base(notePath)
	record := func(text string) {
		if ledger == nil {
			return
		}
		if err := ledger.RecordTodo(ctx, noteName, text); err != nil {
			logger.Warn("todo: record in ledger", "text", text, "error", err)
		}
	}

	created := 0
	for _, todo := range todos {
		if ledger != nil {
			seen, err := ledger.TodoEmitted(ctx, noteName, todo.Text)
			if err != nil {
				logger.Error("todo: ledger lookup, skipping", "text", todo.Text, "error", err)
				continue
			}
			if seen {
				logger.Info("todo: skipping previously emitted", "text", todo.Text, "note", noteName)
				continue
			}
		}
		if titleSet[todo.Text] {
			logger.Info("todo: skipping duplicate", "text", todo.Text)
			record(todo.Text)
			continue
		}
		// Detail format:
		//   "From Boox red ink in <basename>\nOpen: /files/boox?detail=<encoded>"
		//
		// Line 1 is human-readable (shows up in CalDAV clients as-is).
		// Line 2 is a relative URL that opens the Boox Files tab with the
		// note's details modal auto-opened. The _task_row template parses
		// this format to render a proper <a href> that navigates via HTMX
		// to /files/boox?detail=<path>. CalDAV clients which auto-linkify
		// URLs will also expose the second line as a clickable link.
		detailURL := "/files/boox?detail=" + url.QueryEscape(notePath)
		if externalBaseURL != "" {
			// Trim any trailing slash so we don't emit "https://host//files/...".
			trimmed := externalBaseURL
			if trimmed[len(trimmed)-1] == '/' {
				trimmed = trimmed[:len(trimmed)-1]
			}
			detailURL = trimmed + detailURL
		}
		task := &taskstore.Task{
			Title: taskstore.SqlStr(todo.Text),
			Detail: taskstore.SqlStr(
				"From Boox red ink in " + noteName + "\nOpen: " + detailURL,
			),
		}
		if err := tc.Create(ctx, task); err != nil {
			logger.Error("todo: create task", "text", todo.Text, "error", err)
		} else {
			logger.Info("todo: created task from red ink", "text", todo.Text, "task_id", task.TaskID)
			titleSet[todo.Text] = true
			record(todo.Text)
			created++
		}
	}
	return created
}
