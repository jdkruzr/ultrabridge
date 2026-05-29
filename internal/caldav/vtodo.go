package caldav

import (
	"bytes"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	ical "github.com/emersion/go-ical"
	"github.com/sysop/ultrabridge/internal/taskstore"
)

// TaskToVTODO converts a task store Task to an ical.Calendar containing a VTODO.
// If the task has an ICalBlob, it deserializes the blob and overlays DB-authoritative
// fields on top. Otherwise, it builds the calendar from structured fields.
func TaskToVTODO(t *taskstore.Task, dueTimeMode string) *ical.Calendar {
	if t.ICalBlob.Valid && t.ICalBlob.String != "" {
		return taskToVTODOFromBlob(t, dueTimeMode)
	}
	return taskToVTODOFromFields(t, dueTimeMode)
}

// taskToVTODOFromFields builds a VTODO calendar from structured fields only.
// This is the original implementation, used for tasks without iCal blobs.
func taskToVTODOFromFields(t *taskstore.Task, dueTimeMode string) *ical.Calendar {
	cal := ical.NewCalendar()
	cal.Props.SetText("PRODID", "-//UltraBridge//CalDAV//EN")
	cal.Props.SetText("VERSION", "2.0")

	todo := ical.NewComponent("VTODO")
	todo.Props.SetText("UID", t.TaskID)

	// DTSTAMP is required by RFC 5545
	if t.LastModified.Valid {
		todo.Props.SetDateTime("DTSTAMP", taskstore.MsToTime(t.LastModified.Int64))
	} else {
		todo.Props.SetDateTime("DTSTAMP", time.Now().UTC())
	}

	if t.Title.Valid && t.Title.String != "" {
		todo.Props.SetText("SUMMARY", t.Title.String)
	}

	status := taskstore.CalDAVStatus(taskstore.NullStr(t.Status))
	todo.Props.SetText("STATUS", status)

	if t.DueTime != 0 {
		// Fields-only path: no blob to consult, so "preserve" falls
		// through to datetime (current behavior). Callers that need
		// floating-date semantics should round-trip through a blob.
		setDueOnTodo(todo, taskstore.MsToTime(t.DueTime), dueTimeMode)
	}

	if t.LastModified.Valid {
		lm := taskstore.MsToTime(t.LastModified.Int64)
		todo.Props.SetDateTime("LAST-MODIFIED", lm)
	}

	// Completion time: use last_modified (NOT completed_time) per Supernote quirk
	if ct, ok := taskstore.CompletionTime(t); ok {
		todo.Props.SetDateTime("COMPLETED", ct)
	}

	// Tier 2 fields
	if t.Detail.Valid && t.Detail.String != "" {
		todo.Props.SetText("DESCRIPTION", t.Detail.String)
	}
	if t.Importance.Valid && t.Importance.String != "" {
		todo.Props.SetText("PRIORITY", t.Importance.String)
	}

	// Links (read-only, informational)
	if t.Links.Valid && t.Links.String != "" {
		todo.Props.SetText("URL", t.Links.String)
	}

	cal.Children = append(cal.Children, todo)
	return cal
}

// taskToVTODOFromBlob deserializes the stored iCal blob and overlays
// DB-authoritative fields on top, preserving all Tier 3 properties.
func taskToVTODOFromBlob(t *taskstore.Task, dueTimeMode string) *ical.Calendar {
	dec := ical.NewDecoder(strings.NewReader(t.ICalBlob.String))
	cal, err := dec.Decode()
	if err != nil {
		// Fallback: if blob is corrupt, build from fields
		return taskToVTODOFromFields(t, dueTimeMode)
	}

	todo, err := FindVTODO(cal)
	if err != nil {
		return taskToVTODOFromFields(t, dueTimeMode)
	}

	// Overlay DB-authoritative fields (these may have been updated
	// via sync or direct DB operations since the blob was stored)
	todo.Props.SetText("UID", t.TaskID)

	if t.Title.Valid && t.Title.String != "" {
		todo.Props.SetText("SUMMARY", t.Title.String)
	}

	status := taskstore.CalDAVStatus(taskstore.NullStr(t.Status))
	todo.Props.SetText("STATUS", status)

	if t.DueTime != 0 {
		// "preserve" must honor the original VTODO's DUE form: a client
		// PUT of DUE;VALUE=DATE:YYYYMMDD (RFC 5545 floating all-day) must
		// be re-emitted as VALUE=DATE on the way back out. Promoting it
		// to UTC midnight datetime shifts the task to the previous
		// evening for clients in non-UTC timezones. The original prop is
		// still on the decoded blob at this point because we haven't
		// overlaid yet — read its ValueType before overwriting.
		setDueOnTodo(todo, taskstore.MsToTime(t.DueTime), dueTimeMode)
	} else {
		// Remove DUE if cleared
		delete(todo.Props, "DUE")
	}

	if t.LastModified.Valid {
		lm := taskstore.MsToTime(t.LastModified.Int64)
		todo.Props.SetDateTime("DTSTAMP", lm)
		todo.Props.SetDateTime("LAST-MODIFIED", lm)
	}

	if ct, ok := taskstore.CompletionTime(t); ok {
		todo.Props.SetDateTime("COMPLETED", ct)
	} else {
		delete(todo.Props, "COMPLETED")
	}

	// Overlay Tier 2 fields (may have been updated in DB after blob storage)
	if t.Detail.Valid && t.Detail.String != "" {
		todo.Props.SetText("DESCRIPTION", t.Detail.String)
	}
	if t.Importance.Valid && t.Importance.String != "" {
		todo.Props.SetText("PRIORITY", t.Importance.String)
	}

	return cal
}

// VTODOToTask extracts task fields from an ical.Calendar containing a VTODO.
// Returns the extracted task and the UID. Does not set user_id or task_id generation
// — caller handles those. Also serializes the full calendar as ICalBlob for round-trip fidelity.
func VTODOToTask(cal *ical.Calendar, dueTimeMode string) (*taskstore.Task, error) {
	var todo *ical.Component
	for _, child := range cal.Children {
		if child.Name == "VTODO" {
			todo = child
			break
		}
	}
	if todo == nil {
		return nil, fmt.Errorf("no VTODO component found")
	}

	t := &taskstore.Task{}

	if uid := todo.Props.Get("UID"); uid != nil {
		t.TaskID = uid.Value
	}
	if summary := todo.Props.Get("SUMMARY"); summary != nil {
		// .Text() un-escapes per RFC 5545 (\\ → \, \n → real newline,
		// \, → comma, \; → semicolon). Reading .Value raw preserves the
		// backslash escapes, which compounded across PUT/pull cycles
		// (each round doubled backslashes and turned newlines into
		// literal "\n"). Falling back to .Value on parse error keeps
		// behavior safe for malformed input.
		if v, err := summary.Text(); err == nil {
			t.Title = taskstore.SqlStr(v)
		} else {
			t.Title = taskstore.SqlStr(summary.Value)
		}
	}
	if status := todo.Props.Get("STATUS"); status != nil {
		t.Status = taskstore.SqlStr(taskstore.SupernoteStatus(status.Value))
	}
	if due := todo.Props.Get("DUE"); due != nil {
		dueTime, err := due.DateTime(time.UTC)
		if err == nil {
			if dueTimeMode == "date_only" {
				// Strip time component
				dueTime = time.Date(dueTime.Year(), dueTime.Month(), dueTime.Day(),
					0, 0, 0, 0, time.UTC)
			}
			t.DueTime = taskstore.TimeToMs(dueTime)
		}
	}
	if desc := todo.Props.Get("DESCRIPTION"); desc != nil {
		// Same RFC 5545 un-escape rationale as SUMMARY above.
		if v, err := desc.Text(); err == nil {
			t.Detail = taskstore.SqlStr(v)
		} else {
			t.Detail = taskstore.SqlStr(desc.Value)
		}
	}
	if prio := todo.Props.Get("PRIORITY"); prio != nil {
		t.Importance = taskstore.SqlStr(prio.Value)
	}

	// Handle completion time mapping (Supernote quirk: last_modified = actual completion time)
	if taskstore.NullStr(t.Status) == "completed" {
		now := time.Now().UnixMilli()
		if completed := todo.Props.Get("COMPLETED"); completed != nil {
			ct, err := completed.DateTime(time.UTC)
			if err == nil {
				t.LastModified = sql.NullInt64{Int64: taskstore.TimeToMs(ct), Valid: true}
			} else {
				t.LastModified = sql.NullInt64{Int64: now, Valid: true}
			}
		} else {
			t.LastModified = sql.NullInt64{Int64: now, Valid: true}
		}
	}

	// Store full VCALENDAR as blob for round-trip fidelity
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err == nil {
		t.ICalBlob = sql.NullString{String: buf.String(), Valid: true}
	} else {
		slog.Warn("failed to encode ical blob", "err", err)
	}

	return t, nil
}

// setDueOnTodo writes the DUE property in the form dictated by dueTimeMode:
//
//   - "date_only": always emit DUE;VALUE=DATE:YYYYMMDD
//   - "datetime":  always emit DUE:YYYYMMDDTHHMMSSZ
//   - "preserve" (and anything else, including the default empty string):
//     keep whatever VALUE the existing DUE prop on this VTODO carries — DATE
//     stays DATE, DATE-TIME stays DATE-TIME, and if there is no existing DUE
//     prop (e.g. fields-only synth) we fall back to DATE-TIME.
//
// The caller is responsible for having already merged any blob into todo; we
// only inspect ValueType, never values, so passing a freshly-built todo with
// no DUE is safe.
func setDueOnTodo(todo *ical.Component, due time.Time, dueTimeMode string) {
	switch dueTimeMode {
	case "date_only":
		todo.Props.SetDate("DUE", due)
		return
	case "datetime":
		todo.Props.SetDateTime("DUE", due)
		return
	}
	// preserve (or unset / unknown)
	if existing := todo.Props.Get("DUE"); existing != nil && existing.ValueType() == ical.ValueDate {
		todo.Props.SetDate("DUE", due)
		return
	}
	todo.Props.SetDateTime("DUE", due)
}

// FindVTODO returns the first VTODO component in the calendar, or error.
func FindVTODO(cal *ical.Calendar) (*ical.Component, error) {
	for _, child := range cal.Children {
		if child.Name == "VTODO" {
			return child, nil
		}
	}
	return nil, fmt.Errorf("no VTODO component found")
}

// HasVEvent returns true if the calendar contains a VEVENT component.
func HasVEvent(cal *ical.Calendar) bool {
	for _, child := range cal.Children {
		if child.Name == "VEVENT" {
			return true
		}
	}
	return false
}
