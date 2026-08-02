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

// TestStatusToSPC pins the outbound projection onto the device's two states.
func TestStatusToSPC(t *testing.T) {
	for _, tc := range []struct{ stored, want string }{
		{taskstore.StatusNeedsAction, "needsAction"},
		{taskstore.StatusInProcess, "needsAction"}, // device has no "in process"
		{taskstore.StatusCompleted, "completed"},
		{taskstore.StatusCancelled, "completed"}, // closest to "abandoned"
		{"", "needsAction"},
	} {
		if got := StatusToSPC(tc.stored); got != tc.want {
			t.Errorf("StatusToSPC(%q) = %q, want %q", tc.stored, got, tc.want)
		}
	}
}

// TestMergeStatusIsNonDestructive pins the inbound half. The device can only
// ever report the projection StatusToSPC produced, so a device value that
// already agrees with the stored status carries no new information and must not
// flatten the richer local state. Without this, every device sync round-tripped
// inProcess and cancelled back down to the device's two states.
func TestMergeStatusIsNonDestructive(t *testing.T) {
	for _, tc := range []struct {
		device, stored, want string
	}{
		// Echoes of what we sent — local state wins.
		{"completed", taskstore.StatusCancelled, taskstore.StatusCancelled},
		{"needsAction", taskstore.StatusInProcess, taskstore.StatusInProcess},
		// Genuine device transitions.
		{"completed", taskstore.StatusNeedsAction, taskstore.StatusCompleted},
		{"completed", taskstore.StatusInProcess, taskstore.StatusCompleted},
		{"needsAction", taskstore.StatusCompleted, taskstore.StatusNeedsAction},
		{"needsAction", taskstore.StatusCancelled, taskstore.StatusNeedsAction},
		// No-ops.
		{"completed", taskstore.StatusCompleted, taskstore.StatusCompleted},
		{"needsAction", taskstore.StatusNeedsAction, taskstore.StatusNeedsAction},
		// Absent on the wire — leave the stored value alone.
		{"", taskstore.StatusInProcess, taskstore.StatusInProcess},
	} {
		if got := mergeStatus(tc.device, tc.stored); got != tc.want {
			t.Errorf("mergeStatus(device=%q, stored=%q) = %q, want %q",
				tc.device, tc.stored, got, tc.want)
		}
	}
}

// TestSPCLastModifiedNeverZero covers the device-visibility requirement: a task
// whose lastModified is 0 is stored but invisible to the device and partner app
// (docs/PRIVATE_CLOUD_REFERENCE.md §Required fields).
func TestSPCLastModifiedNeverZero(t *testing.T) {
	const completedAt, updatedAt = int64(1_700_000_000_000), int64(1_760_000_000_000)

	completed := taskstore.Task{
		Status:      sql.NullString{String: taskstore.StatusCompleted, Valid: true},
		CompletedAt: sql.NullInt64{Int64: completedAt, Valid: true},
		UpdatedAt:   updatedAt,
	}
	if got := spcLastModified(completed); got != completedAt {
		t.Errorf("completed task lastModified: got %d, want completed_at %d", got, completedAt)
	}

	active := taskstore.Task{
		Status:    sql.NullString{String: taskstore.StatusNeedsAction, Valid: true},
		UpdatedAt: updatedAt,
	}
	if got := spcLastModified(active); got != updatedAt {
		t.Errorf("active task lastModified: got %d, want updated_at %d", got, updatedAt)
	}

	// Falls back rather than emitting 0, which would hide the task on-device.
	legacy := taskstore.Task{
		Status:       sql.NullString{String: taskstore.StatusNeedsAction, Valid: true},
		LastModified: sql.NullInt64{Int64: 42, Valid: true},
	}
	if got := spcLastModified(legacy); got != 42 {
		t.Errorf("legacy row lastModified: got %d, want 42", got)
	}
}

// TestMergeCompletedAtTracksStatus verifies the completion instant is stamped on
// the transition, held steady while completed, and cleared on the way out.
func TestMergeCompletedAtTracksStatus(t *testing.T) {
	const deviceCompletion = int64(1_754_000_000_000)

	t.Run("stamped on transition from the device's lastModified", func(t *testing.T) {
		existing := taskstore.Task{Status: sql.NullString{String: taskstore.StatusNeedsAction, Valid: true}}
		got := MergeSPCIntoTask(existing, dto.SPCTask{Status: "completed", LastModified: deviceCompletion})
		if !got.CompletedAt.Valid || got.CompletedAt.Int64 != deviceCompletion {
			t.Errorf("completed_at: got %v, want %d", got.CompletedAt, deviceCompletion)
		}
	})

	t.Run("preserved while it stays completed", func(t *testing.T) {
		existing := taskstore.Task{
			Status:      sql.NullString{String: taskstore.StatusCompleted, Valid: true},
			CompletedAt: sql.NullInt64{Int64: deviceCompletion, Valid: true},
		}
		// A later device write (e.g. a sort-order reshuffle) must not move it.
		got := MergeSPCIntoTask(existing, dto.SPCTask{Status: "completed", LastModified: deviceCompletion + 999_999})
		if got.CompletedAt.Int64 != deviceCompletion {
			t.Errorf("completed_at moved: got %d, want %d", got.CompletedAt.Int64, deviceCompletion)
		}
	})

	t.Run("cleared when the device un-completes", func(t *testing.T) {
		existing := taskstore.Task{
			Status:      sql.NullString{String: taskstore.StatusCompleted, Valid: true},
			CompletedAt: sql.NullInt64{Int64: deviceCompletion, Valid: true},
		}
		got := MergeSPCIntoTask(existing, dto.SPCTask{Status: "needsAction", LastModified: deviceCompletion + 1})
		if got.CompletedAt.Valid {
			t.Errorf("completed_at should be cleared, got %v", got.CompletedAt)
		}
	})

	t.Run("cancelled task stays cancelled and keeps no completion instant", func(t *testing.T) {
		existing := taskstore.Task{Status: sql.NullString{String: taskstore.StatusCancelled, Valid: true}}
		got := MergeSPCIntoTask(existing, dto.SPCTask{Status: "completed", LastModified: deviceCompletion})
		if taskstore.NullStr(got.Status) != taskstore.StatusCancelled {
			t.Errorf("status: got %q, want cancelled", taskstore.NullStr(got.Status))
		}
		if got.CompletedAt.Valid {
			t.Errorf("cancelled task should have no completed_at, got %v", got.CompletedAt)
		}
	})
}
