package taskstore

import (
	"crypto/md5"
	"database/sql"
	"fmt"
	"strconv"
	"time"
)

// GenerateTaskID creates an MD5 hash from title + creation timestamp,
// matching the convention used by Supernote devices.
func GenerateTaskID(title string, createdAtMs int64) string {
	data := title + strconv.FormatInt(createdAtMs, 10)
	return fmt.Sprintf("%x", md5.Sum([]byte(data)))
}

// ComputeETag generates an ETag for a task based on its mutable fields.
//
// updated_at is the entropy that matters: it is the store-owned watermark
// stamped on every write, so any change to any field moves it — including
// fields not named here (description, priority, categories, ical_blob).
// It replaced last_modified, which stopped being a reliable "something
// changed" signal once completion time moved to its own column.
func ComputeETag(t *Task) string {
	data := t.TaskID +
		NullStr(t.Title) +
		NullStr(t.Status) +
		strconv.FormatInt(t.DueTime, 10) +
		strconv.FormatInt(t.UpdatedAt, 10)
	return fmt.Sprintf("%x", md5.Sum([]byte(data)))
}

// ComputeCTag returns the max last_modified value as a string,
// suitable for use as a CalDAV collection CTag.
func ComputeCTag(tasks []Task) string {
	var max int64
	for _, t := range tasks {
		if t.LastModified.Valid && t.LastModified.Int64 > max {
			max = t.LastModified.Int64
		}
	}
	return strconv.FormatInt(max, 10)
}

// CompletionTime returns the actual completion timestamp for a completed task.
//
// Reads the dedicated completed_at column. It previously read last_modified,
// following the Supernote convention where that field doubles as the completion
// time — but that convention only holds for a client that never edits a task
// after completing it. UB does, and its own Update stamped last_modified on
// every write, so the completion time drifted to the last edit.
func CompletionTime(t *Task) (time.Time, bool) {
	if NullStr(t.Status) != "completed" {
		return time.Time{}, false
	}
	if !t.CompletedAt.Valid || t.CompletedAt.Int64 == 0 {
		return time.Time{}, false
	}
	return MsToTime(t.CompletedAt.Int64), true
}

// MsToTime converts a millisecond UTC timestamp to time.Time.
func MsToTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// TimeToMs converts a time.Time to millisecond UTC timestamp.
// Returns 0 for zero time.
func TimeToMs(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// CalDAVStatus converts a Supernote status string to a CalDAV STATUS value.
func CalDAVStatus(supernoteStatus string) string {
	switch supernoteStatus {
	case "completed":
		return "COMPLETED"
	case "needsAction", "":
		return "NEEDS-ACTION"
	default:
		return "NEEDS-ACTION"
	}
}

// SupernoteStatus converts a CalDAV STATUS value to a Supernote status string.
func SupernoteStatus(caldavStatus string) string {
	switch caldavStatus {
	case "COMPLETED":
		return "completed"
	case "NEEDS-ACTION", "":
		return "needsAction"
	default:
		return "needsAction"
	}
}

// NullStr extracts a string from sql.NullString. Exported for use by caldav package.
func NullStr(ns sql.NullString) string {
	if ns.Valid {
		return ns.String
	}
	return ""
}

// SqlStr creates a sql.NullString. Exported for use by caldav package.
func SqlStr(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

func nullInt64Str(ni sql.NullInt64) string {
	if ni.Valid {
		return strconv.FormatInt(ni.Int64, 10)
	}
	return "0"
}
