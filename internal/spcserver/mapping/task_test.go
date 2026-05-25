package mapping

import (
	"database/sql"
	"testing"

	"github.com/sysop/ultrabridge/internal/spcserver/dto"
	"github.com/sysop/ultrabridge/internal/taskstore"
)

// fullSPC is a populated task for round-trip checks (id set so no id-generation
// or now() path is hit).
func fullSPC() dto.SPCTask {
	return dto.SPCTask{
		ID:            "abc123",
		TaskListID:    "default",
		Title:         "buy milk",
		Detail:        "2%",
		Status:        "completed",
		Importance:    "high",
		DueTime:       1700000000000,
		CompletedTime: 1690000000000, // creation time (quirk)
		LastModified:  1695000000000, // completion time (quirk)
		Recurrence:    "FREQ=DAILY",
		IsReminderOn:  "1",
		Links:         "http://x",
		IsDeleted:     "N",
	}
}

// TestRoundTripPreservesFields verifies TaskToSPC(SPCToTask(s)) preserves the
// fields taskstore persists. Verifies: spc-phase-1.AC4.3
func TestRoundTripPreservesFields(t *testing.T) {
	s := fullSPC()
	got := TaskToSPC(SPCToTask(s))

	checks := []struct {
		name      string
		got, want any
	}{
		{"ID", got.ID, s.ID},
		{"TaskListID", got.TaskListID, s.TaskListID},
		{"Title", got.Title, s.Title},
		{"Detail", got.Detail, s.Detail},
		{"Status", got.Status, s.Status},
		{"Importance", got.Importance, s.Importance},
		{"DueTime", got.DueTime, s.DueTime},
		{"CompletedTime", got.CompletedTime, s.CompletedTime},
		{"LastModified", got.LastModified, s.LastModified},
		{"Recurrence", got.Recurrence, s.Recurrence},
		{"IsReminderOn", got.IsReminderOn, s.IsReminderOn},
		{"Links", got.Links, s.Links},
		{"IsDeleted", got.IsDeleted, s.IsDeleted},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
}

// TestStatusIsPassthroughLowercase verifies the SPC mapping stores and emits the
// device's native lowercase status verbatim (needsAction/completed). That casing
// matches BOTH the device wire (docs/PRIVATE_CLOUD_REFERENCE.md §status) and UB's
// DB convention. The CalDAV uppercase forms (COMPLETED/NEEDS-ACTION) belong ONLY
// to the iCal VTODO boundary (caldav/vtodo.go), NOT this device boundary — routing
// status through CalDAVStatus/SupernoteStatus here un-completed tasks on the device
// (the "zombie task" bug) and wrote DB statuses UB's own completion checks ignore.
func TestStatusIsPassthroughLowercase(t *testing.T) {
	// inbound: device → store. Must store the device's lowercase verbatim.
	if tk := SPCToTask(dto.SPCTask{Status: "completed"}); tk.Status.String != "completed" {
		t.Errorf("SPC completed → store: got %q, want completed", tk.Status.String)
	}
	if tk := SPCToTask(dto.SPCTask{Status: "needsAction"}); tk.Status.String != "needsAction" {
		t.Errorf("SPC needsAction → store: got %q, want needsAction", tk.Status.String)
	}
	// outbound: store → device. A completed task MUST stay completed; the bug
	// downgraded it to needsAction, un-completing it on the device.
	if s := TaskToSPC(taskstore.Task{Status: sql.NullString{String: "completed", Valid: true}}); s.Status != "completed" {
		t.Errorf("store completed → SPC: got %q, want completed (zombie regression)", s.Status)
	}
	if s := TaskToSPC(taskstore.Task{Status: sql.NullString{String: "needsAction", Valid: true}}); s.Status != "needsAction" {
		t.Errorf("store needsAction → SPC: got %q, want needsAction", s.Status)
	}
}

// TestCompletedSortFlags verifies a completed task carries SortCompleted=1/Sort=0.
func TestCompletedSortFlags(t *testing.T) {
	completed := TaskToSPC(SPCToTask(dto.SPCTask{Status: "completed", ID: "x"}))
	if completed.SortCompleted != 1 || completed.Sort != 0 {
		t.Errorf("completed sort flags: sort=%d sortCompleted=%d", completed.Sort, completed.SortCompleted)
	}
	open := TaskToSPC(SPCToTask(dto.SPCTask{Status: "needsAction", ID: "y"}))
	if open.Sort != 1 || open.SortCompleted != 0 {
		t.Errorf("open sort flags: sort=%d sortCompleted=%d", open.Sort, open.SortCompleted)
	}
}

// TestSoftDeletePreserved verifies isDeleted=Y round-trips.
func TestSoftDeletePreserved(t *testing.T) {
	s := fullSPC()
	s.IsDeleted = "Y"
	if got := TaskToSPC(SPCToTask(s)); got.IsDeleted != "Y" {
		t.Errorf("isDeleted: got %q, want Y", got.IsDeleted)
	}
}

// TestNewTaskGetsGeneratedID verifies a task with no ID gets an MD5 id.
func TestNewTaskGetsGeneratedID(t *testing.T) {
	tk := SPCToTask(dto.SPCTask{Title: "new", CompletedTime: 1690000000000})
	if len(tk.TaskID) != 32 {
		t.Errorf("expected 32-char MD5 id, got %q (len %d)", tk.TaskID, len(tk.TaskID))
	}
}
