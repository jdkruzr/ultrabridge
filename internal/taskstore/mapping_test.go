package taskstore

import (
	"database/sql"
	"testing"
	"time"
)

func TestGenerateTaskID(t *testing.T) {
	tests := []struct {
		name      string
		title     string
		createdMs int64
		want      string
	}{
		{
			name:      "simple task",
			title:     "Buy milk",
			createdMs: 1234567890,
			want:      "21b7268d35a34a6751fe73704248e087", // md5("Buy milk1234567890")
		},
		{
			name:      "empty title",
			title:     "",
			createdMs: 0,
			want:      "cfcd208495d565ef66e7dff9f98764da", // md5("0")
		},
		{
			name:      "special characters",
			title:     "Task with 特殊 chars!@#",
			createdMs: 9999999999,
			want:      "781f40cf3abd6aecc387407eab829bb3", // md5("Task with 特殊 chars!@#9999999999")
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GenerateTaskID(tt.title, tt.createdMs)
			if got != tt.want {
				t.Errorf("GenerateTaskID(%q, %d) = %q, want %q", tt.title, tt.createdMs, got, tt.want)
			}
		})
	}
}

func TestComputeETag(t *testing.T) {
	baseTask := &Task{
		TaskID:       "task123",
		Title:        sql.NullString{String: "Buy milk", Valid: true},
		Status:       sql.NullString{String: "needsAction", Valid: true},
		DueTime:      1000,
		LastModified: sql.NullInt64{Int64: 5000, Valid: true},
	}

	baseETag := ComputeETag(baseTask)

	tests := []struct {
		name     string
		modifyFn func(*Task)
		wantDiff bool
	}{
		{
			name: "title change",
			modifyFn: func(task *Task) {
				task.Title = sql.NullString{String: "Buy eggs", Valid: true}
			},
			wantDiff: true,
		},
		{
			name: "status change",
			modifyFn: func(task *Task) {
				task.Status = sql.NullString{String: "completed", Valid: true}
			},
			wantDiff: true,
		},
		{
			name: "due_time change",
			modifyFn: func(task *Task) {
				task.DueTime = 2000
			},
			wantDiff: true,
		},
		{
			name: "last_modified change",
			modifyFn: func(task *Task) {
				task.LastModified = sql.NullInt64{Int64: 6000, Valid: true}
			},
			wantDiff: true,
		},
		{
			name: "no change",
			modifyFn: func(task *Task) {
				// Don't change anything
			},
			wantDiff: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			taskCopy := *baseTask
			tt.modifyFn(&taskCopy)
			newETag := ComputeETag(&taskCopy)

			if tt.wantDiff && newETag == baseETag {
				t.Errorf("ComputeETag should change when field changes, but got same ETag")
			}
			if !tt.wantDiff && newETag != baseETag {
				t.Errorf("ComputeETag should stay same, but got different ETag")
			}
		})
	}
}

func TestComputeCTag(t *testing.T) {
	tests := []struct {
		name  string
		tasks []Task
		want  string
	}{
		{
			name:  "empty list",
			tasks: []Task{},
			want:  "0",
		},
		{
			name: "single task",
			tasks: []Task{
				{LastModified: sql.NullInt64{Int64: 1000, Valid: true}},
			},
			want: "1000",
		},
		{
			name: "multiple tasks",
			tasks: []Task{
				{LastModified: sql.NullInt64{Int64: 1000, Valid: true}},
				{LastModified: sql.NullInt64{Int64: 5000, Valid: true}},
				{LastModified: sql.NullInt64{Int64: 3000, Valid: true}},
			},
			want: "5000",
		},
		{
			name: "some tasks without last_modified",
			tasks: []Task{
				{LastModified: sql.NullInt64{Int64: 1000, Valid: true}},
				{LastModified: sql.NullInt64{Valid: false}},
				{LastModified: sql.NullInt64{Int64: 2000, Valid: true}},
			},
			want: "2000",
		},
		{
			name: "all tasks without last_modified",
			tasks: []Task{
				{LastModified: sql.NullInt64{Valid: false}},
				{LastModified: sql.NullInt64{Valid: false}},
			},
			want: "0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeCTag(tt.tasks)
			if got != tt.want {
				t.Errorf("ComputeCTag() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCompletionTime(t *testing.T) {
	tests := []struct {
		name      string
		task      *Task
		wantTime  time.Time
		wantValid bool
	}{
		{
			name: "completed task",
			task: &Task{
				Status:       sql.NullString{String: "completed", Valid: true},
				LastModified: sql.NullInt64{Int64: 1704067200000, Valid: true}, // 2024-01-01 00:00:00 UTC
			},
			wantTime:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			wantValid: true,
		},
		{
			name: "not completed task",
			task: &Task{
				Status:       sql.NullString{String: "needsAction", Valid: true},
				LastModified: sql.NullInt64{Int64: 1704067200000, Valid: true},
			},
			wantTime:  time.Time{},
			wantValid: false,
		},
		{
			name: "completed but no last_modified",
			task: &Task{
				Status:       sql.NullString{String: "completed", Valid: true},
				LastModified: sql.NullInt64{Valid: false},
			},
			wantTime:  time.Time{},
			wantValid: false,
		},
		{
			name: "completed_time and last_modified differ",
			task: &Task{
				Status:        sql.NullString{String: "completed", Valid: true},
				CompletedTime: sql.NullInt64{Int64: 1704067200000, Valid: true}, // creation time
				LastModified:  sql.NullInt64{Int64: 1704153600000, Valid: true}, // completion time (24h later)
			},
			wantTime:  time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC), // should be last_modified, not completed_time
			wantValid: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotTime, gotValid := CompletionTime(tt.task)
			if gotValid != tt.wantValid {
				t.Errorf("CompletionTime() valid = %v, want %v", gotValid, tt.wantValid)
			}
			if gotValid && !gotTime.Equal(tt.wantTime) {
				t.Errorf("CompletionTime() time = %v, want %v", gotTime, tt.wantTime)
			}
		})
	}
}

func TestMsToTime(t *testing.T) {
	tests := []struct {
		name string
		ms   int64
		want time.Time
	}{
		{
			name: "zero",
			ms:   0,
			want: time.Time{},
		},
		{
			name: "known timestamp",
			ms:   1704067200000,
			want: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "another known timestamp",
			ms:   1704153600000,
			want: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MsToTime(tt.ms)
			if !got.Equal(tt.want) {
				t.Errorf("MsToTime(%d) = %v, want %v", tt.ms, got, tt.want)
			}
		})
	}
}

func TestTimeToMs(t *testing.T) {
	tests := []struct {
		name string
		time time.Time
		want int64
	}{
		{
			name: "zero time",
			time: time.Time{},
			want: 0,
		},
		{
			name: "known timestamp",
			time: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			want: 1704067200000,
		},
		{
			name: "another known timestamp",
			time: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC),
			want: 1704153600000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TimeToMs(tt.time)
			if got != tt.want {
				t.Errorf("TimeToMs(%v) = %d, want %d", tt.time, got, tt.want)
			}
		})
	}
}

func TestMsToTimeTimeToMsRoundTrip(t *testing.T) {
	testCases := []int64{
		0,
		1000,
		1704067200000,
		9999999999999,
	}

	for _, ms := range testCases {
		t.Run("", func(t *testing.T) {
			tm := MsToTime(ms)
			got := TimeToMs(tm)
			if got != ms {
				t.Errorf("Round-trip failed: %d -> %v -> %d", ms, tm, got)
			}
		})
	}
}

func TestCalDAVStatus(t *testing.T) {
	tests := []struct {
		name   string
		status string
		want   string
	}{
		{
			name:   "completed",
			status: "completed",
			want:   "COMPLETED",
		},
		{
			name:   "needs action",
			status: "needsAction",
			want:   "NEEDS-ACTION",
		},
		{
			name:   "empty",
			status: "",
			want:   "NEEDS-ACTION",
		},
		{
			name:   "unknown",
			status: "unknown",
			want:   "NEEDS-ACTION",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CalDAVStatus(tt.status)
			if got != tt.want {
				t.Errorf("CalDAVStatus(%q) = %q, want %q", tt.status, got, tt.want)
			}
		})
	}
}

func TestSuperNoteStatus(t *testing.T) {
	tests := []struct {
		name   string
		status string
		want   string
	}{
		{
			name:   "completed",
			status: "COMPLETED",
			want:   "completed",
		},
		{
			name:   "needs action",
			status: "NEEDS-ACTION",
			want:   "needsAction",
		},
		{
			name:   "empty",
			status: "",
			want:   "needsAction",
		},
		{
			name:   "unknown",
			status: "unknown",
			want:   "needsAction",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SupernoteStatus(tt.status)
			if got != tt.want {
				t.Errorf("SupernoteStatus(%q) = %q, want %q", tt.status, got, tt.want)
			}
		})
	}
}

func TestCalDAVStatusSuperNoteStatusRoundTrip(t *testing.T) {
	testCases := []string{
		"completed",
		"needsAction",
		"",
	}

	for _, status := range testCases {
		t.Run(status, func(t *testing.T) {
			caldav := CalDAVStatus(status)
			got := SupernoteStatus(caldav)
			expected := status
			if status == "" {
				expected = "needsAction"
			}
			if got != expected {
				t.Errorf("Round-trip failed: %q -> %q -> %q (expected %q)", status, caldav, got, expected)
			}
		})
	}
}

func TestNullStr(t *testing.T) {
	tests := []struct {
		name string
		ns   sql.NullString
		want string
	}{
		{
			name: "valid string",
			ns:   sql.NullString{String: "hello", Valid: true},
			want: "hello",
		},
		{
			name: "null string",
			ns:   sql.NullString{Valid: false},
			want: "",
		},
		{
			name: "empty valid string",
			ns:   sql.NullString{String: "", Valid: true},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NullStr(tt.ns)
			if got != tt.want {
				t.Errorf("NullStr() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSqlStr(t *testing.T) {
	tests := []struct {
		name string
		s    string
		want sql.NullString
	}{
		{
			name: "non-empty string",
			s:    "hello",
			want: sql.NullString{String: "hello", Valid: true},
		},
		{
			name: "empty string",
			s:    "",
			want: sql.NullString{String: "", Valid: false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SqlStr(tt.s)
			if got != tt.want {
				t.Errorf("SqlStr(%q) = %v, want %v", tt.s, got, tt.want)
			}
		})
	}
}
