// Package groups is the task-group (task-list) seam. Phase 1 ships a single
// synthesized group (Option A) matching UB's single-collection CalDAV model;
// the GroupProvider interface lets a future DB-backed multi-collection impl
// swap in without touching the task handlers. See
// docs/future-work/multi-collection-task-lists.md.
package groups

import (
	"time"

	"github.com/sysop/ultrabridge/internal/spcserver/dto"
)

// DefaultID is the stable taskListId of the single synthesized group. It MUST
// be a numeric (Long-parseable) string: the device parses taskListId as a Long
// (real SPC uses snowflake ids), so a non-numeric value like "default" throws
// NumberFormatException on the device and aborts task/data sync (observed
// 2026-05-23). "1" is the smallest stable valid value.
const DefaultID = "1"

// GroupProvider supplies the device-visible task groups.
type GroupProvider interface {
	// Groups returns the groups for a user (one, in the single-group impl).
	Groups(userID int64) []dto.ScheduleTaskGroupDO
	// DefaultID is the taskListId tasks belong to when unspecified.
	DefaultID() string
}

// Single is the Phase-1 single-group provider: one list titled after the CalDAV
// collection, holding every task.
type Single struct {
	title   string
	created int64
}

// NewSingle builds a single-group provider titled by the CalDAV collection name.
func NewSingle(title string) *Single {
	return &Single{title: title, created: time.Now().UnixMilli()}
}

func (s *Single) DefaultID() string { return DefaultID }

func (s *Single) Groups(userID int64) []dto.ScheduleTaskGroupDO {
	return []dto.ScheduleTaskGroupDO{{
		TaskListID:   DefaultID,
		UserID:       userID,
		Title:        s.title,
		LastModified: s.created,
		IsDeleted:    "N",
	}}
}
