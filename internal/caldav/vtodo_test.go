package caldav

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	ical "github.com/emersion/go-ical"
	"github.com/sysop/ultrabridge/internal/taskstore"
)

func TestTaskToVTODO(t *testing.T) {
	tests := []struct {
		name        string
		task        *taskstore.Task
		dueTimeMode string
		verify      func(t *testing.T, cal *ical.Calendar)
	}{
		{
			name: "minimal task with uid and title",
			task: &taskstore.Task{
				TaskID: "test-id-123",
				Title:  sql.NullString{String: "Test Task", Valid: true},
			},
			dueTimeMode: "preserve",
			verify: func(t *testing.T, cal *ical.Calendar) {
				todo, err := FindVTODO(cal)
				if err != nil {
					t.Fatalf("FindVTODO failed: %v", err)
				}
				if todo.Props.Get("UID").Value != "test-id-123" {
					t.Errorf("UID mismatch: got %s", todo.Props.Get("UID").Value)
				}
				if todo.Props.Get("SUMMARY").Value != "Test Task" {
					t.Errorf("SUMMARY mismatch: got %s", todo.Props.Get("SUMMARY").Value)
				}
			},
		},
		{
			name: "task with zero due time",
			task: &taskstore.Task{
				TaskID:  "test-id-123",
				Title:   sql.NullString{String: "Task", Valid: true},
				DueTime: 0,
			},
			dueTimeMode: "preserve",
			verify: func(t *testing.T, cal *ical.Calendar) {
				todo, err := FindVTODO(cal)
				if err != nil {
					t.Fatalf("FindVTODO failed: %v", err)
				}
				if todo.Props.Get("DUE") != nil {
					t.Error("DUE property should not be present for zero DueTime")
				}
			},
		},
		{
			name: "completed task status",
			task: &taskstore.Task{
				TaskID:       "test-id",
				Title:        sql.NullString{String: "Done Task", Valid: true},
				Status:       sql.NullString{String: "completed", Valid: true},
				LastModified: sql.NullInt64{Int64: taskstore.TimeToMs(time.Date(2025, 3, 17, 14, 30, 0, 0, time.UTC)), Valid: true},
			},
			dueTimeMode: "preserve",
			verify: func(t *testing.T, cal *ical.Calendar) {
				todo, err := FindVTODO(cal)
				if err != nil {
					t.Fatalf("FindVTODO failed: %v", err)
				}
				if todo.Props.Get("STATUS").Value != "COMPLETED" {
					t.Errorf("STATUS mismatch for completed: got %s", todo.Props.Get("STATUS").Value)
				}
				if todo.Props.Get("COMPLETED") == nil {
					t.Error("COMPLETED property should be set for completed task")
				}
			},
		},
		{
			name: "needs-action task status",
			task: &taskstore.Task{
				TaskID:       "test-id",
				Title:        sql.NullString{String: "Open Task", Valid: true},
				Status:       sql.NullString{String: "needsAction", Valid: true},
				LastModified: sql.NullInt64{Int64: taskstore.TimeToMs(time.Now().UTC()), Valid: true},
			},
			dueTimeMode: "preserve",
			verify: func(t *testing.T, cal *ical.Calendar) {
				todo, err := FindVTODO(cal)
				if err != nil {
					t.Fatalf("FindVTODO failed: %v", err)
				}
				if todo.Props.Get("STATUS").Value != "NEEDS-ACTION" {
					t.Errorf("STATUS mismatch for needsAction: got %s", todo.Props.Get("STATUS").Value)
				}
			},
		},
		{
			name: "due time with date_only mode",
			task: &taskstore.Task{
				TaskID:  "test-id",
				Title:   sql.NullString{String: "Task", Valid: true},
				DueTime: taskstore.TimeToMs(time.Date(2025, 4, 15, 14, 30, 0, 0, time.UTC)),
			},
			dueTimeMode: "date_only",
			verify: func(t *testing.T, cal *ical.Calendar) {
				todo, err := FindVTODO(cal)
				if err != nil {
					t.Fatalf("FindVTODO failed: %v", err)
				}
				dueProp := todo.Props.Get("DUE")
				if dueProp == nil {
					t.Fatal("DUE property should be set")
				}
				// Check that it's a DATE (not DATE-TIME)
				// If it's DATE-only, ValueType should be "DATE"
				vt := dueProp.ValueType()
				if vt != ical.ValueDate {
					t.Errorf("DUE value type should be DATE for date_only mode, got %s", vt)
				}
			},
		},
		{
			name: "due time with preserve mode",
			task: &taskstore.Task{
				TaskID:  "test-id",
				Title:   sql.NullString{String: "Task", Valid: true},
				DueTime: taskstore.TimeToMs(time.Date(2025, 4, 15, 14, 30, 0, 0, time.UTC)),
			},
			dueTimeMode: "preserve",
			verify: func(t *testing.T, cal *ical.Calendar) {
				todo, err := FindVTODO(cal)
				if err != nil {
					t.Fatalf("FindVTODO failed: %v", err)
				}
				dueProp := todo.Props.Get("DUE")
				if dueProp == nil {
					t.Fatal("DUE property should be set")
				}
				vt := dueProp.ValueType()
				if vt != ical.ValueDateTime {
					t.Errorf("DUE value type should be DATE-TIME for preserve mode, got %s", vt)
				}
			},
		},
		{
			name: "tier 2 fields",
			task: &taskstore.Task{
				TaskID:     "test-id",
				Title:      sql.NullString{String: "Task", Valid: true},
				Detail:     sql.NullString{String: "Important notes", Valid: true},
				Importance: sql.NullString{String: "5", Valid: true},
				Links:      sql.NullString{String: "supernote://note/123/page/1", Valid: true},
			},
			dueTimeMode: "preserve",
			verify: func(t *testing.T, cal *ical.Calendar) {
				todo, err := FindVTODO(cal)
				if err != nil {
					t.Fatalf("FindVTODO failed: %v", err)
				}
				if todo.Props.Get("DESCRIPTION").Value != "Important notes" {
					t.Errorf("DESCRIPTION mismatch")
				}
				if todo.Props.Get("PRIORITY").Value != "5" {
					t.Errorf("PRIORITY mismatch")
				}
				if todo.Props.Get("URL").Value != "supernote://note/123/page/1" {
					t.Errorf("URL mismatch")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cal := TaskToVTODO(tt.task, tt.dueTimeMode)
			if cal == nil {
				t.Fatal("TaskToVTODO returned nil")
			}
			tt.verify(t, cal)
		})
	}
}

func TestVTODOToTask(t *testing.T) {
	tests := []struct {
		name        string
		cal         *ical.Calendar
		dueTimeMode string
		verify      func(t *testing.T, task *taskstore.Task)
		expectErr   bool
	}{
		{
			name: "minimal VTODO",
			cal: createTestCalendar(map[string]string{
				"UID":     "test-id",
				"SUMMARY": "Test Task",
				"STATUS":  "NEEDS-ACTION",
			}),
			dueTimeMode: "preserve",
			verify: func(t *testing.T, task *taskstore.Task) {
				if task.TaskID != "test-id" {
					t.Errorf("TaskID mismatch: got %s", task.TaskID)
				}
				if taskstore.NullStr(task.Title) != "Test Task" {
					t.Errorf("Title mismatch")
				}
				if taskstore.NullStr(task.Status) != "needsAction" {
					t.Errorf("Status mismatch")
				}
			},
		},
		{
			name: "no VTODO component",
			cal: func() *ical.Calendar {
				cal := ical.NewCalendar()
				// Add VEVENT instead
				vevent := ical.NewComponent("VEVENT")
				cal.Children = append(cal.Children, vevent)
				return cal
			}(),
			dueTimeMode: "preserve",
			expectErr:   true,
		},
		{
			name: "RFC5545 escapes are unescaped",
			// Set Prop.Value directly to bypass go-ical's SetText escaping —
			// we want to verify the READ path un-escapes wire-format
			// DESCRIPTION / SUMMARY values that already contain literal
			// "\n" (backslash + n) and "\\" sequences. Using .Value
			// instead of .Text() in vtodo.go let those escapes accumulate
			// across PUT cycles.
			cal: func() *ical.Calendar {
				cal := ical.NewCalendar()
				todo := ical.NewComponent("VTODO")
				todo.Props.Set(&ical.Prop{Name: "UID", Value: "esc-id"})
				todo.Props.Set(&ical.Prop{Name: "STATUS", Value: "NEEDS-ACTION"})
				todo.Props.Set(&ical.Prop{Name: "SUMMARY", Value: `Title with \, comma`})
				todo.Props.Set(&ical.Prop{Name: "DESCRIPTION", Value: `Line one\nLine two\\with backslash`})
				cal.Children = append(cal.Children, todo)
				return cal
			}(),
			dueTimeMode: "preserve",
			verify: func(t *testing.T, task *taskstore.Task) {
				wantTitle := "Title with , comma"
				if got := taskstore.NullStr(task.Title); got != wantTitle {
					t.Errorf("Title escapes not unwound: got %q want %q", got, wantTitle)
				}
				wantDetail := "Line one\nLine two\\with backslash"
				if got := taskstore.NullStr(task.Detail); got != wantDetail {
					t.Errorf("Detail escapes not unwound: got %q want %q", got, wantDetail)
				}
			},
		},
		{
			name: "COMPLETED status",
			cal: createTestCalendar(map[string]string{
				"UID":     "test-id",
				"SUMMARY": "Done Task",
				"STATUS":  "COMPLETED",
			}),
			dueTimeMode: "preserve",
			verify: func(t *testing.T, task *taskstore.Task) {
				if taskstore.NullStr(task.Status) != "completed" {
					t.Errorf("Status mismatch for COMPLETED")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task, err := VTODOToTask(tt.cal, tt.dueTimeMode)
			if (err != nil) != tt.expectErr {
				t.Errorf("error expectation mismatch: %v", err)
			}
			if err == nil && task != nil {
				tt.verify(t, task)
			}
		})
	}
}

func TestFindVTODO(t *testing.T) {
	t.Run("finds VTODO in calendar", func(t *testing.T) {
		cal := ical.NewCalendar()
		vtodo := ical.NewComponent("VTODO")
		cal.Children = append(cal.Children, vtodo)

		found, err := FindVTODO(cal)
		if err != nil {
			t.Errorf("FindVTODO failed: %v", err)
		}
		if found == nil {
			t.Error("FindVTODO returned nil")
		}
	})

	t.Run("returns error when no VTODO", func(t *testing.T) {
		cal := ical.NewCalendar()
		vevent := ical.NewComponent("VEVENT")
		cal.Children = append(cal.Children, vevent)

		_, err := FindVTODO(cal)
		if err == nil {
			t.Error("FindVTODO should return error when no VTODO present")
		}
	})
}

func TestHasVEvent(t *testing.T) {
	t.Run("returns true for calendar with VEVENT", func(t *testing.T) {
		cal := ical.NewCalendar()
		vevent := ical.NewComponent("VEVENT")
		cal.Children = append(cal.Children, vevent)

		if !HasVEvent(cal) {
			t.Error("HasVEvent should return true for calendar with VEVENT")
		}
	})

	t.Run("returns false for calendar without VEVENT", func(t *testing.T) {
		cal := ical.NewCalendar()
		vtodo := ical.NewComponent("VTODO")
		cal.Children = append(cal.Children, vtodo)

		if HasVEvent(cal) {
			t.Error("HasVEvent should return false for calendar without VEVENT")
		}
	})
}

func TestRoundTripVTODOTask(t *testing.T) {
	tests := []struct {
		name        string
		task        *taskstore.Task
		dueTimeMode string
	}{
		{
			name: "round trip minimal task",
			task: &taskstore.Task{
				TaskID: "id-123",
				Title:  sql.NullString{String: "Round Trip Task", Valid: true},
				Status: sql.NullString{String: "needsAction", Valid: true},
			},
			dueTimeMode: "preserve",
		},
		{
			name: "round trip task with all fields",
			task: &taskstore.Task{
				TaskID:       "id-456",
				Title:        sql.NullString{String: "Complex Task", Valid: true},
				Status:       sql.NullString{String: "completed", Valid: true},
				Detail:       sql.NullString{String: "Task details", Valid: true},
				Importance:   sql.NullString{String: "3", Valid: true},
				DueTime:      taskstore.TimeToMs(time.Date(2025, 5, 20, 10, 0, 0, 0, time.UTC)),
				LastModified: sql.NullInt64{Int64: taskstore.TimeToMs(time.Date(2025, 3, 17, 15, 0, 0, 0, time.UTC)), Valid: true},
			},
			dueTimeMode: "preserve",
		},
		{
			name: "round trip task with date_only mode",
			task: &taskstore.Task{
				TaskID:       "id-789",
				Title:        sql.NullString{String: "Date Only Task", Valid: true},
				Status:       sql.NullString{String: "needsAction", Valid: true},
				DueTime:      taskstore.TimeToMs(time.Date(2025, 6, 15, 14, 30, 45, 0, time.UTC)),
				LastModified: sql.NullInt64{Int64: taskstore.TimeToMs(time.Date(2025, 3, 17, 15, 0, 0, 0, time.UTC)), Valid: true},
			},
			dueTimeMode: "date_only",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Task -> VTODO
			cal := TaskToVTODO(tt.task, tt.dueTimeMode)

			// VTODO -> Task
			resultTask, err := VTODOToTask(cal, tt.dueTimeMode)
			if err != nil {
				t.Fatalf("VTODOToTask failed: %v", err)
			}

			// Compare mapped fields
			if resultTask.TaskID != tt.task.TaskID {
				t.Errorf("TaskID mismatch after round trip")
			}
			if taskstore.NullStr(resultTask.Title) != taskstore.NullStr(tt.task.Title) {
				t.Errorf("Title mismatch after round trip")
			}
			if taskstore.NullStr(resultTask.Status) != taskstore.NullStr(tt.task.Status) {
				t.Errorf("Status mismatch after round trip")
			}
			if taskstore.NullStr(resultTask.Detail) != taskstore.NullStr(tt.task.Detail) {
				t.Errorf("Detail mismatch after round trip")
			}
			if taskstore.NullStr(resultTask.Importance) != taskstore.NullStr(tt.task.Importance) {
				t.Errorf("Importance mismatch after round trip")
			}
			// Verify DueTime preservation
			if resultTask.DueTime != tt.task.DueTime {
				// In date_only mode, the time component is stripped, so compare as dates
				if tt.dueTimeMode == "date_only" {
					expectedTime := time.Date(
						time.Unix(0, tt.task.DueTime*1e6).UTC().Year(),
						time.Unix(0, tt.task.DueTime*1e6).UTC().Month(),
						time.Unix(0, tt.task.DueTime*1e6).UTC().Day(),
						0, 0, 0, 0, time.UTC,
					)
					resultTime := time.Unix(0, resultTask.DueTime*1e6).UTC()
					resultDate := time.Date(resultTime.Year(), resultTime.Month(), resultTime.Day(), 0, 0, 0, 0, time.UTC)
					if expectedTime != resultDate {
						t.Errorf("DueTime mismatch after round trip in date_only mode: got %v, want %v", resultDate, expectedTime)
					}
				} else {
					t.Errorf("DueTime mismatch after round trip: got %d, want %d", resultTask.DueTime, tt.task.DueTime)
				}
			}
		})
	}
}

// Helper function to create a test calendar with VTODO properties
func createTestCalendar(props map[string]string) *ical.Calendar {
	cal := ical.NewCalendar()
	cal.Props.SetText("PRODID", "-//UltraBridge//CalDAV//EN")
	cal.Props.SetText("VERSION", "2.0")

	todo := ical.NewComponent("VTODO")

	// DTSTAMP is required by RFC 5545
	todo.Props.SetDateTime("DTSTAMP", time.Now().UTC())

	for key, value := range props {
		todo.Props.SetText(key, value)
	}

	cal.Children = append(cal.Children, todo)
	return cal
}

// TestBlobRoundTrip tests that iCal blobs with Tier 3 properties survive round-trip.
// Verifies AC1.4: CalDAV client sets Tier 3 properties (RRULE, VALARM, CATEGORIES),
// they round-trip perfectly on next GET.
func TestBlobRoundTrip(t *testing.T) {
	tests := []struct {
		name        string
		props       map[string]string
		dueTimeMode string
		setupBlob   func(cal *ical.Calendar) // Optional: modify calendar before VTODOToTask
		verify      func(t *testing.T, original *ical.Calendar, roundTrip *ical.Calendar)
	}{
		{
			name: "RRULE survives round-trip",
			props: map[string]string{
				"UID":     "test-rrule-123",
				"SUMMARY": "Weekly Meeting",
				"STATUS":  "NEEDS-ACTION",
				"RRULE":   "FREQ=WEEKLY;BYDAY=MO",
			},
			dueTimeMode: "preserve",
			verify: func(t *testing.T, original *ical.Calendar, roundTrip *ical.Calendar) {
				origTodo, _ := FindVTODO(original)
				rtTodo, _ := FindVTODO(roundTrip)

				origRRULE := origTodo.Props.Get("RRULE")
				rtRRULE := rtTodo.Props.Get("RRULE")

				if origRRULE == nil || rtRRULE == nil {
					t.Error("RRULE should exist in both calendars")
					return
				}
				if origRRULE.Value != rtRRULE.Value {
					t.Errorf("RRULE mismatch: got %s, want %s", rtRRULE.Value, origRRULE.Value)
				}
			},
		},
		{
			name: "CATEGORIES survives round-trip",
			props: map[string]string{
				"UID":        "test-cat-456",
				"SUMMARY":    "Work Task",
				"STATUS":     "NEEDS-ACTION",
				"CATEGORIES": "Work,UltraBridge",
			},
			dueTimeMode: "preserve",
			verify: func(t *testing.T, original *ical.Calendar, roundTrip *ical.Calendar) {
				origTodo, _ := FindVTODO(original)
				rtTodo, _ := FindVTODO(roundTrip)

				origCat := origTodo.Props.Get("CATEGORIES")
				rtCat := rtTodo.Props.Get("CATEGORIES")

				if origCat == nil || rtCat == nil {
					t.Error("CATEGORIES should exist in both calendars")
					return
				}
				if origCat.Value != rtCat.Value {
					t.Errorf("CATEGORIES mismatch: got %s, want %s", rtCat.Value, origCat.Value)
				}
			},
		},
		{
			name: "X-properties survive round-trip",
			props: map[string]string{
				"UID":          "test-xprop-789",
				"SUMMARY":      "Task with Custom Props",
				"STATUS":       "NEEDS-ACTION",
				"X-CUSTOM-ONE": "CustomValue1",
				"X-CUSTOM-TWO": "CustomValue2",
			},
			dueTimeMode: "preserve",
			verify: func(t *testing.T, original *ical.Calendar, roundTrip *ical.Calendar) {
				origTodo, _ := FindVTODO(original)
				rtTodo, _ := FindVTODO(roundTrip)

				origXOne := origTodo.Props.Get("X-CUSTOM-ONE")
				rtXOne := rtTodo.Props.Get("X-CUSTOM-ONE")
				origXTwo := origTodo.Props.Get("X-CUSTOM-TWO")
				rtXTwo := rtTodo.Props.Get("X-CUSTOM-TWO")

				if origXOne == nil || rtXOne == nil {
					t.Error("X-CUSTOM-ONE should exist in both calendars")
					return
				}
				if origXOne.Value != rtXOne.Value {
					t.Errorf("X-CUSTOM-ONE mismatch: got %s, want %s", rtXOne.Value, origXOne.Value)
				}

				if origXTwo == nil || rtXTwo == nil {
					t.Error("X-CUSTOM-TWO should exist in both calendars")
					return
				}
				if origXTwo.Value != rtXTwo.Value {
					t.Errorf("X-CUSTOM-TWO mismatch: got %s, want %s", rtXTwo.Value, origXTwo.Value)
				}
			},
		},
		{
			name: "Basic properties preserved with RRULE",
			props: map[string]string{
				"UID":        "test-basic-111",
				"SUMMARY":    "Task with RRULE",
				"STATUS":     "COMPLETED",
				"DESCRIPTION": "This task repeats",
				"PRIORITY":   "3",
				"RRULE":      "FREQ=DAILY;COUNT=5",
			},
			dueTimeMode: "preserve",
			verify: func(t *testing.T, original *ical.Calendar, roundTrip *ical.Calendar) {
				origTodo, _ := FindVTODO(original)
				rtTodo, _ := FindVTODO(roundTrip)

				// Check Tier 1/2 fields are preserved
				if origTodo.Props.Get("SUMMARY").Value != rtTodo.Props.Get("SUMMARY").Value {
					t.Error("SUMMARY should be preserved")
				}
				if origTodo.Props.Get("STATUS").Value != rtTodo.Props.Get("STATUS").Value {
					t.Error("STATUS should be preserved")
				}
				if origTodo.Props.Get("DESCRIPTION").Value != rtTodo.Props.Get("DESCRIPTION").Value {
					t.Error("DESCRIPTION should be preserved")
				}
				if origTodo.Props.Get("PRIORITY").Value != rtTodo.Props.Get("PRIORITY").Value {
					t.Error("PRIORITY should be preserved")
				}
				// Check Tier 3 field is preserved
				if origTodo.Props.Get("RRULE").Value != rtTodo.Props.Get("RRULE").Value {
					t.Error("RRULE should be preserved")
				}
			},
		},
		{
			name: "VALARM component survives round-trip",
			props: map[string]string{
				"UID":     "test-valarm-222",
				"SUMMARY": "Task with Alarm",
				"STATUS":  "NEEDS-ACTION",
			},
			dueTimeMode: "preserve",
			setupBlob: func(cal *ical.Calendar) {
				// Add VALARM child component to VTODO
				todo, _ := FindVTODO(cal)
				valarm := ical.NewComponent("VALARM")
				valarm.Props.SetText("TRIGGER", "-PT15M")
				valarm.Props.SetText("ACTION", "DISPLAY")
				valarm.Props.SetText("DESCRIPTION", "Reminder for task")
				todo.Children = append(todo.Children, valarm)
			},
			verify: func(t *testing.T, original *ical.Calendar, roundTrip *ical.Calendar) {
				origTodo, _ := FindVTODO(original)
				rtTodo, _ := FindVTODO(roundTrip)

				// Verify VTODO properties are preserved
				if origTodo.Props.Get("SUMMARY").Value != rtTodo.Props.Get("SUMMARY").Value {
					t.Error("SUMMARY should be preserved with VALARM")
				}

				// Verify VALARM child component exists in round-trip
				var origAlarm, rtAlarm *ical.Component
				for _, child := range origTodo.Children {
					if child.Name == "VALARM" {
						origAlarm = child
						break
					}
				}
				for _, child := range rtTodo.Children {
					if child.Name == "VALARM" {
						rtAlarm = child
						break
					}
				}

				if origAlarm == nil {
					t.Error("Original should have VALARM component")
				}
				if rtAlarm == nil {
					t.Error("Round-trip should have VALARM component")
				}

				// Verify VALARM properties survive
				if origAlarm != nil && rtAlarm != nil {
					if origAlarm.Props.Get("TRIGGER").Value != rtAlarm.Props.Get("TRIGGER").Value {
						t.Error("VALARM TRIGGER should be preserved")
					}
					if origAlarm.Props.Get("ACTION").Value != rtAlarm.Props.Get("ACTION").Value {
						t.Error("VALARM ACTION should be preserved")
					}
					if origAlarm.Props.Get("DESCRIPTION").Value != rtAlarm.Props.Get("DESCRIPTION").Value {
						t.Error("VALARM DESCRIPTION should be preserved")
					}
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create original calendar
			originalCal := createTestCalendar(tt.props)

			// Apply optional setupBlob modifications
			if tt.setupBlob != nil {
				tt.setupBlob(originalCal)
			}

			// VTODOToTask to extract and serialize
			task, err := VTODOToTask(originalCal, tt.dueTimeMode)
			if err != nil {
				t.Fatalf("VTODOToTask failed: %v", err)
			}

			// Verify blob was serialized
			if !task.ICalBlob.Valid || task.ICalBlob.String == "" {
				t.Fatal("ICalBlob should be serialized after VTODOToTask")
			}

			// TaskToVTODO to deserialize and overlay
			roundTripCal := TaskToVTODO(task, tt.dueTimeMode)
			if roundTripCal == nil {
				t.Fatal("TaskToVTODO returned nil")
			}

			// Run verification
			tt.verify(t, originalCal, roundTripCal)
		})
	}
}

// TestBlobOverlayCorrectness verifies that DB-authoritative fields override blob values.
// When a Task has ICalBlob containing SUMMARY="Old Title" but t.Title="New Title",
// TaskToVTODO should return SUMMARY="New Title".
func TestBlobOverlayCorrectness(t *testing.T) {
	t.Run("DB title overrides blob summary", func(t *testing.T) {
		// Create a calendar with old summary
		originalProps := map[string]string{
			"UID":     "test-overlay-id",
			"SUMMARY": "Old Title",
			"STATUS":  "NEEDS-ACTION",
		}
		originalCal := createTestCalendar(originalProps)

		// Convert to task (captures blob)
		task, err := VTODOToTask(originalCal, "preserve")
		if err != nil {
			t.Fatalf("VTODOToTask failed: %v", err)
		}

		// Update the title in the task (but not the blob)
		task.Title = sql.NullString{String: "New Title", Valid: true}

		// Convert back to calendar
		resultCal := TaskToVTODO(task, "preserve")

		// Verify the new title is used
		todo, _ := FindVTODO(resultCal)
		if todo.Props.Get("SUMMARY").Value != "New Title" {
			t.Errorf("SUMMARY should be overridden: got %s, want New Title", todo.Props.Get("SUMMARY").Value)
		}
	})

	t.Run("DB status overrides blob status", func(t *testing.T) {
		originalProps := map[string]string{
			"UID":    "test-status-overlay",
			"STATUS": "NEEDS-ACTION",
		}
		originalCal := createTestCalendar(originalProps)

		task, err := VTODOToTask(originalCal, "preserve")
		if err != nil {
			t.Fatalf("VTODOToTask failed: %v", err)
		}

		// Update status to completed
		task.Status = sql.NullString{String: "completed", Valid: true}
		task.LastModified = sql.NullInt64{Int64: taskstore.TimeToMs(time.Now().UTC()), Valid: true}

		resultCal := TaskToVTODO(task, "preserve")
		todo, _ := FindVTODO(resultCal)

		if todo.Props.Get("STATUS").Value != "COMPLETED" {
			t.Errorf("STATUS should be overridden: got %s, want COMPLETED", todo.Props.Get("STATUS").Value)
		}
	})

	t.Run("DB due clears blob due when set to zero", func(t *testing.T) {
		originalProps := map[string]string{
			"UID":     "test-due-clear",
			"SUMMARY": "Task with Due",
			"DUE":     "20250615T120000Z",
		}
		originalCal := createTestCalendar(originalProps)

		task, err := VTODOToTask(originalCal, "preserve")
		if err != nil {
			t.Fatalf("VTODOToTask failed: %v", err)
		}

		// Verify blob has DUE
		if task.ICalBlob.String == "" {
			t.Fatal("ICalBlob should not be empty")
		}

		// Clear DueTime in task
		task.DueTime = 0

		resultCal := TaskToVTODO(task, "preserve")
		todo, _ := FindVTODO(resultCal)

		if todo.Props.Get("DUE") != nil {
			t.Error("DUE should be removed when DueTime is 0")
		}
	})
}

// TestBlobCorruptFallback verifies that corrupt blobs fall back to building from fields.
// AC requirement: TaskToVTODO should not panic when ICalBlob is corrupt.
func TestBlobCorruptFallback(t *testing.T) {
	t.Run("corrupt blob falls back to fields", func(t *testing.T) {
		task := &taskstore.Task{
			TaskID:        "test-corrupt",
			Title:         sql.NullString{String: "Fallback Task", Valid: true},
			Status:        sql.NullString{String: "needsAction", Valid: true},
			ICalBlob:      sql.NullString{String: "not valid ical content", Valid: true},
			LastModified:  sql.NullInt64{Int64: taskstore.TimeToMs(time.Now().UTC()), Valid: true},
		}

		// Should not panic, should fall back to fields
		cal := TaskToVTODO(task, "preserve")

		if cal == nil {
			t.Fatal("TaskToVTODO returned nil")
		}

		todo, err := FindVTODO(cal)
		if err != nil {
			t.Fatal("Should have found VTODO from fallback")
		}

		// Verify title from fields is used
		if todo.Props.Get("SUMMARY").Value != "Fallback Task" {
			t.Errorf("Should fall back to fields: got %s, want Fallback Task", todo.Props.Get("SUMMARY").Value)
		}
	})
}

// TestSupernoteTaskNoBlob verifies backward compatibility: tasks without blobs
// (imported from Supernote) render correctly as VTODO.
// AC1.5 Success: Task created on Supernote (no ical_blob) renders as valid VTODO
// with correct Tier 1/2 fields.
func TestSupernoteTaskNoBlob(t *testing.T) {
	t.Run("supernote task without blob builds from fields", func(t *testing.T) {
		// Simulate a Supernote-originated task (no blob)
		supernoteTask := &taskstore.Task{
			TaskID:        "supernote-id-123",
			Title:         sql.NullString{String: "Supernote Task", Valid: true},
			Detail:        sql.NullString{String: "Task details from Supernote", Valid: true},
			Status:        sql.NullString{String: "needsAction", Valid: true},
			Importance:    sql.NullString{String: "2", Valid: true},
			DueTime:       taskstore.TimeToMs(time.Date(2025, 6, 30, 0, 0, 0, 0, time.UTC)),
			LastModified:  sql.NullInt64{Int64: taskstore.TimeToMs(time.Date(2025, 3, 17, 10, 0, 0, 0, time.UTC)), Valid: true},
			IsReminderOn:  "Y",
			IsDeleted:     "N",
			ICalBlob:      sql.NullString{}, // NULL blob, no iCal representation
		}

		// Convert to VTODO
		cal := TaskToVTODO(supernoteTask, "preserve")

		if cal == nil {
			t.Fatal("TaskToVTODO returned nil")
		}

		todo, err := FindVTODO(cal)
		if err != nil {
			t.Fatal("Should have found VTODO")
		}

		// Verify Tier 1 fields
		if todo.Props.Get("UID").Value != "supernote-id-123" {
			t.Error("UID should match TaskID")
		}
		if todo.Props.Get("SUMMARY").Value != "Supernote Task" {
			t.Error("SUMMARY should match Title")
		}
		if todo.Props.Get("STATUS").Value != "NEEDS-ACTION" {
			t.Error("STATUS should be correct")
		}

		// Verify Tier 2 fields
		if todo.Props.Get("DESCRIPTION").Value != "Task details from Supernote" {
			t.Error("DESCRIPTION should match Detail")
		}
		if todo.Props.Get("PRIORITY").Value != "2" {
			t.Error("PRIORITY should match Importance")
		}

		// Verify DUE is set
		if todo.Props.Get("DUE") == nil {
			t.Error("DUE should be set")
		}
	})
}

// TestVTODOToTaskBlobSerialization verifies that VTODOToTask serializes the full calendar.
// When a CalDAV client PUTs a VTODO with Tier 3 properties, the full text is stored.
// Note: This test is covered by TestBlobRoundTrip which verifies blob content more thoroughly.
func TestVTODOToTaskBlobSerialization(t *testing.T) {
	t.Run("VTODOToTask serializes full calendar as blob", func(t *testing.T) {
		props := map[string]string{
			"UID":        "test-serial-123",
			"SUMMARY":    "Task with Tier 3",
			"STATUS":     "NEEDS-ACTION",
			"RRULE":      "FREQ=WEEKLY",
			"CATEGORIES": "Work",
			"X-CUSTOM":   "Value",
		}
		cal := createTestCalendar(props)

		task, err := VTODOToTask(cal, "preserve")
		if err != nil {
			t.Fatalf("VTODOToTask failed: %v", err)
		}

		// Verify blob is populated
		if !task.ICalBlob.Valid {
			t.Error("ICalBlob should be Valid")
		}
		if task.ICalBlob.String == "" {
			t.Error("ICalBlob should not be empty")
			return
		}

		// Verify blob contains SUMMARY at minimum (other properties tested in TestBlobRoundTrip)
		if !strings.Contains(task.ICalBlob.String, "SUMMARY") {
			t.Error("Blob should contain SUMMARY")
		}
	})
}

// TestTaskWithoutBlob ensures that tasks with invalid/NULL ICalBlob use the fields path.
func TestTaskWithoutBlob(t *testing.T) {
	t.Run("null blob uses fields path", func(t *testing.T) {
		task := &taskstore.Task{
			TaskID:       "no-blob-task",
			Title:        sql.NullString{String: "Simple Task", Valid: true},
			Status:       sql.NullString{String: "needsAction", Valid: true},
			LastModified: sql.NullInt64{Int64: taskstore.TimeToMs(time.Now().UTC()), Valid: true},
			ICalBlob:     sql.NullString{}, // NULL blob
		}

		cal := TaskToVTODO(task, "preserve")
		if cal == nil {
			t.Fatal("TaskToVTODO returned nil")
		}

		todo, _ := FindVTODO(cal)
		if todo.Props.Get("SUMMARY").Value != "Simple Task" {
			t.Error("Should use fields when blob is NULL")
		}
	})

	t.Run("empty blob uses fields path", func(t *testing.T) {
		task := &taskstore.Task{
			TaskID:       "empty-blob-task",
			Title:        sql.NullString{String: "Another Task", Valid: true},
			Status:       sql.NullString{String: "needsAction", Valid: true},
			LastModified: sql.NullInt64{Int64: taskstore.TimeToMs(time.Now().UTC()), Valid: true},
			ICalBlob:     sql.NullString{String: "", Valid: true}, // Empty blob
		}

		cal := TaskToVTODO(task, "preserve")
		if cal == nil {
			t.Fatal("TaskToVTODO returned nil")
		}

		todo, _ := FindVTODO(cal)
		if todo.Props.Get("SUMMARY").Value != "Another Task" {
			t.Error("Should use fields when blob is empty")
		}
	})
}

// TestPreserveValueDateRoundTrip verifies that a VTODO PUT with
// DUE;VALUE=DATE:YYYYMMDD round-trips back out as DUE;VALUE=DATE in
// "preserve" mode (the default). Regression: prior behavior re-emitted
// floating dates as UTC datetimes, shifting all-day tasks to the previous
// evening for clients in non-UTC timezones.
func TestPreserveValueDateRoundTrip(t *testing.T) {
	t.Run("floating VALUE=DATE survives preserve round-trip via blob", func(t *testing.T) {
		raw := "BEGIN:VCALENDAR\r\n" +
			"PRODID:-//ForestNote//EN\r\n" +
			"VERSION:2.0\r\n" +
			"BEGIN:VTODO\r\n" +
			"DTSTAMP:20260529T022546Z\r\n" +
			"DUE;VALUE=DATE:20260529\r\n" +
			"SUMMARY:test\r\n" +
			"UID:2e7cffed-ecac-46e8-a648-1ee928c4decd\r\n" +
			"END:VTODO\r\n" +
			"END:VCALENDAR\r\n"

		cal, err := ical.NewDecoder(strings.NewReader(raw)).Decode()
		if err != nil {
			t.Fatalf("decode raw VCALENDAR: %v", err)
		}
		todo, err := FindVTODO(cal)
		if err != nil {
			t.Fatalf("find VTODO: %v", err)
		}
		if vt := todo.Props.Get("DUE").ValueType(); vt != ical.ValueDate {
			t.Fatalf("input DUE should already be VALUE=DATE; got %s", vt)
		}

		task, err := VTODOToTask(cal, "preserve")
		if err != nil {
			t.Fatalf("VTODOToTask: %v", err)
		}
		if !task.ICalBlob.Valid || task.ICalBlob.String == "" {
			t.Fatal("expected blob to be captured")
		}

		out := TaskToVTODO(task, "preserve")
		outTodo, err := FindVTODO(out)
		if err != nil {
			t.Fatalf("find VTODO in output: %v", err)
		}
		dueProp := outTodo.Props.Get("DUE")
		if dueProp == nil {
			t.Fatal("output VTODO missing DUE")
		}
		if vt := dueProp.ValueType(); vt != ical.ValueDate {
			t.Errorf("preserve mode should re-emit floating DATE; got VALUE=%s value=%q",
				vt, dueProp.Value)
		}
		if dueProp.Value != "20260529" {
			t.Errorf("DUE value should be raw YYYYMMDD; got %q", dueProp.Value)
		}

		// Encode the whole calendar and confirm the wire form has no T000000Z.
		var buf strings.Builder
		if err := ical.NewEncoder(&buf).Encode(out); err != nil {
			t.Fatalf("encode output: %v", err)
		}
		if strings.Contains(buf.String(), "DUE:20260529T000000Z") {
			t.Errorf("output should not promote floating date to UTC datetime; got:\n%s", buf.String())
		}
	})

	t.Run("datetime DUE stays datetime in preserve mode via blob", func(t *testing.T) {
		raw := "BEGIN:VCALENDAR\r\n" +
			"PRODID:-//Test//EN\r\n" +
			"VERSION:2.0\r\n" +
			"BEGIN:VTODO\r\n" +
			"DTSTAMP:20260529T022546Z\r\n" +
			"DUE:20260529T140000Z\r\n" +
			"SUMMARY:test\r\n" +
			"UID:dt-preserve-1\r\n" +
			"END:VTODO\r\n" +
			"END:VCALENDAR\r\n"

		cal, err := ical.NewDecoder(strings.NewReader(raw)).Decode()
		if err != nil {
			t.Fatalf("decode raw VCALENDAR: %v", err)
		}

		task, err := VTODOToTask(cal, "preserve")
		if err != nil {
			t.Fatalf("VTODOToTask: %v", err)
		}

		out := TaskToVTODO(task, "preserve")
		outTodo, _ := FindVTODO(out)
		dueProp := outTodo.Props.Get("DUE")
		if dueProp == nil {
			t.Fatal("output VTODO missing DUE")
		}
		if vt := dueProp.ValueType(); vt != ical.ValueDateTime {
			t.Errorf("preserve mode should keep datetime as datetime; got VALUE=%s", vt)
		}
	})

	t.Run("date_only mode forces VALUE=DATE even when blob carries datetime", func(t *testing.T) {
		raw := "BEGIN:VCALENDAR\r\n" +
			"PRODID:-//Test//EN\r\n" +
			"VERSION:2.0\r\n" +
			"BEGIN:VTODO\r\n" +
			"DTSTAMP:20260529T022546Z\r\n" +
			"DUE:20260529T140000Z\r\n" +
			"SUMMARY:test\r\n" +
			"UID:dt-force-date\r\n" +
			"END:VTODO\r\n" +
			"END:VCALENDAR\r\n"

		cal, err := ical.NewDecoder(strings.NewReader(raw)).Decode()
		if err != nil {
			t.Fatalf("decode raw VCALENDAR: %v", err)
		}
		task, err := VTODOToTask(cal, "date_only")
		if err != nil {
			t.Fatalf("VTODOToTask: %v", err)
		}
		out := TaskToVTODO(task, "date_only")
		outTodo, _ := FindVTODO(out)
		if vt := outTodo.Props.Get("DUE").ValueType(); vt != ical.ValueDate {
			t.Errorf("date_only mode should force VALUE=DATE; got %s", vt)
		}
	})
}
