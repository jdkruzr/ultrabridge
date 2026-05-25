// Package mapping converts between UB's taskstore.Task and the SPC wire task
// (dto.SPCTask) at the controller boundary — no second store. It replicates the
// small field mapping that internal/tasksync/supernote uses (which Phase 5
// deletes), citing it as the reference, and is independent of that package.
package mapping

import (
	"database/sql"
	"time"

	"github.com/sysop/ultrabridge/internal/spcserver/dto"
	"github.com/sysop/ultrabridge/internal/taskstore"
)

// TaskToSPC maps a stored task to the SPC wire shape. Status is converted from
// CalDAV casing to SPC casing; sort flags are derived from completion (SPC-only,
// not persisted by taskstore). completedTime/lastModified carry the Supernote
// quirk (creation/completion times) straight through.
func TaskToSPC(t taskstore.Task) dto.SPCTask {
	// Status passes through verbatim: the device wire and UB's DB both use
	// lowercase needsAction/completed (docs/PRIVATE_CLOUD_REFERENCE.md §status).
	// The CalDAV uppercase forms belong to the iCal VTODO boundary only — running
	// status through SupernoteStatus here downgraded completed→needsAction,
	// un-completing tasks on the device (the "zombie task" bug).
	status := t.Status.String
	sortVal, sortCompleted := 1, 0
	if status == "completed" {
		sortVal, sortCompleted = 0, 1
	}
	lastMod := t.LastModified.Int64
	isDeleted := t.IsDeleted
	if isDeleted == "" {
		isDeleted = "N"
	}
	return dto.SPCTask{
		ID:               t.TaskID,
		TaskListID:       t.TaskListID.String,
		Title:            t.Title.String,
		Detail:           t.Detail.String,
		Status:           status,
		Importance:       t.Importance.String,
		DueTime:          t.DueTime,
		CompletedTime:    t.CompletedTime.Int64,
		LastModified:     lastMod,
		Recurrence:       t.Recurrence.String,
		IsReminderOn:     t.IsReminderOn,
		Links:            t.Links.String,
		IsDeleted:        isDeleted,
		Sort:             sortVal,
		SortCompleted:    sortCompleted,
		SortTime:         lastMod,
		PlanerSort:       sortVal,
		PlanerSortTime:   lastMod,
		AllSort:          sortVal,
		AllSortCompleted: sortCompleted,
		AllSortTime:      lastMod,
	}
}

// SPCToTask maps an SPC wire task to a stored task. Status is converted to
// CalDAV casing. A task without an ID is treated as new: it gets an MD5 id
// (title+creation-time, matching the Supernote device convention), and its
// creation time (the completedTime quirk) defaults to now when unset.
func SPCToTask(s dto.SPCTask) taskstore.Task {
	completedTime := s.CompletedTime
	id := s.ID
	if id == "" {
		if completedTime == 0 {
			completedTime = time.Now().UnixMilli() // creation time
		}
		id = taskstore.GenerateTaskID(s.Title, completedTime)
	}
	isDeleted := s.IsDeleted
	if isDeleted == "" {
		isDeleted = "N"
	}
	return taskstore.Task{
		TaskID:        id,
		TaskListID:    nullString(s.TaskListID),
		Title:         nullString(s.Title),
		Detail:        nullString(s.Detail),
		LastModified:  nullInt64(s.LastModified),
		Recurrence:    nullString(s.Recurrence),
		IsReminderOn:  s.IsReminderOn,
		Status:        nullString(s.Status), // verbatim lowercase; see TaskToSPC
		Importance:    nullString(s.Importance),
		DueTime:       s.DueTime,
		CompletedTime: nullInt64(completedTime),
		Links:         nullString(s.Links),
		IsDeleted:     isDeleted,
	}
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }
func nullInt64(n int64) sql.NullInt64    { return sql.NullInt64{Int64: n, Valid: n != 0} }
