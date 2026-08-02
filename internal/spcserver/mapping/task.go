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

// StatusToSPC projects a stored status onto the device's two-state model.
//
// The device has no notion of "in process" or "cancelled". inProcess maps to
// needsAction (still on the list). cancelled maps to completed — the device
// can't express "abandoned", and hiding it from the active list is the closest
// available intent.
//
// Note this is a lossy projection, which is exactly why the inbound direction
// (MergeSPCIntoTask) must not treat a device status as authoritative when the
// stored status already projects onto it.
func StatusToSPC(stored string) string {
	switch stored {
	case taskstore.StatusCompleted, taskstore.StatusCancelled:
		return "completed"
	default:
		return "needsAction"
	}
}

// spcLastModified reconstructs the device's lastModified field, which does
// double duty on the wire: for a completed task the device reads it as the
// completion time, and for any task it feeds sort order.
//
// The device also *hides* tasks whose lastModified is 0
// (docs/PRIVATE_CLOUD_REFERENCE.md §Required fields), so this never returns
// zero if any timestamp is available.
func spcLastModified(t taskstore.Task) int64 {
	if StatusToSPC(t.Status.String) == "completed" && t.CompletedAt.Valid && t.CompletedAt.Int64 > 0 {
		return t.CompletedAt.Int64
	}
	if t.UpdatedAt > 0 {
		return t.UpdatedAt
	}
	return t.LastModified.Int64
}

// TaskToSPC maps a stored task to the SPC wire shape, projecting UB's native
// four-state status and dedicated completed_at column back onto the device's
// two-state model and its overloaded lastModified field. Sort flags are
// SPC-only and derived from the projected status; taskstore does not persist
// them.
func TaskToSPC(t taskstore.Task) dto.SPCTask {
	// Project, don't pass through. Sending the raw stored value would put
	// "inProcess"/"cancelled" on the wire, which the device does not understand.
	// Equally, this must not be FromCalDAVStatus — running status through the
	// CalDAV inbound mapper here downgraded completed→needsAction and
	// un-completed tasks on the device (the "zombie task" bug).
	status := StatusToSPC(t.Status.String)
	sortVal, sortCompleted := 1, 0
	if status == "completed" {
		sortVal, sortCompleted = 0, 1
	}
	lastMod := spcLastModified(t)
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

// MergeSPCIntoTask applies a device task onto the stored row, returning the
// result to persist. Use this for every device *write* against an existing
// task; SPCToTask is only correct when there is no stored row to preserve.
//
// The device sends the fields it knows about and nothing else, so a wire task
// is a partial view, not a replacement. Building a fresh Task from it and
// handing that to Store.Update — which writes every column unconditionally —
// wiped ical_blob and the ForestNote provenance columns, destroying recurrence,
// alarms, hierarchy, dependencies and every X-CFAIT-* property on any task the
// device touched. It also violated the NOT NULL constraint on completed_time
// whenever the wire omitted it.
func MergeSPCIntoTask(existing taskstore.Task, s dto.SPCTask) taskstore.Task {
	merged := existing // carries ical_blob, forestnote_*, created_at, completed_time

	// Fields the device owns. Empty means "not sent", not "cleared" — the wire
	// has no way to distinguish them, so absent values leave the stored value be.
	if s.Title != "" {
		merged.Title = nullString(s.Title)
	}
	if s.Detail != "" {
		merged.Detail = nullString(s.Detail)
	}
	if s.Importance != "" {
		merged.Importance = nullString(s.Importance)
	}
	if s.Recurrence != "" {
		merged.Recurrence = nullString(s.Recurrence)
	}
	if s.Links != "" {
		merged.Links = nullString(s.Links)
	}
	if s.TaskListID != "" {
		merged.TaskListID = nullString(s.TaskListID)
	}
	if s.IsReminderOn != "" {
		merged.IsReminderOn = s.IsReminderOn
	}
	if s.IsDeleted != "" {
		merged.IsDeleted = s.IsDeleted
	}
	if s.DueTime != 0 {
		merged.DueTime = s.DueTime
	}
	if s.LastModified != 0 {
		merged.LastModified = nullInt64(s.LastModified)
	}

	merged.Status = nullString(mergeStatus(s.Status, taskstore.NullStr(existing.Status)))
	merged.CompletedAt = mergeCompletedAt(merged, existing, s)
	return merged
}

// mergeStatus resolves the device's two-state view against the four-state
// store. The device can only report the projection StatusToSPC produced, so a
// device value that already agrees with the stored status carries no new
// information and must not overwrite the richer local state:
//
//	device       stored                    result
//	completed    cancelled                 cancelled   (echo of what we sent)
//	completed    needsAction | inProcess   completed   (a real device completion)
//	needsAction  inProcess                 inProcess   (echo)
//	needsAction  completed | cancelled     needsAction (a real device un-complete)
func mergeStatus(device, stored string) string {
	if device == "" {
		return stored
	}
	if StatusToSPC(stored) == device {
		return stored
	}
	if device == "completed" {
		return taskstore.StatusCompleted
	}
	return taskstore.StatusNeedsAction
}

// mergeCompletedAt keeps the completion instant in step with the merged status:
// stamped on the transition into completed, preserved while it stays completed,
// cleared on the way out.
func mergeCompletedAt(merged, existing taskstore.Task, s dto.SPCTask) sql.NullInt64 {
	if taskstore.NullStr(merged.Status) != taskstore.StatusCompleted {
		return sql.NullInt64{}
	}
	if existing.CompletedAt.Valid && taskstore.NullStr(existing.Status) == taskstore.StatusCompleted {
		return existing.CompletedAt // already completed; don't move the instant
	}
	// Transitioning into completed. The device reports the completion time in
	// lastModified (see spcLastModified for the outbound half).
	if s.LastModified != 0 {
		return sql.NullInt64{Int64: s.LastModified, Valid: true}
	}
	return sql.NullInt64{Int64: time.Now().UnixMilli(), Valid: true}
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }
func nullInt64(n int64) sql.NullInt64    { return sql.NullInt64{Int64: n, Valid: n != 0} }
