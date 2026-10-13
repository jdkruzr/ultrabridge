package caldav

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gocaldav "github.com/emersion/go-webdav/caldav"
	"github.com/sysop/ultrabridge/internal/taskstore"
)

// TestMissingObjectsAre404 drives the real go-webdav handler: a missing
// calendar object must answer 404, not the 500 go-webdav produces for any
// unwrapped backend error. Regression for Cfait's companion-object cleanup
// (DELETE evt-<id>.ics, evt-<id>-start.ics, ...) logging 500s in production.
func TestMissingObjectsAre404(t *testing.T) {
	store := newMockTaskStore()
	store.tasks["live-task"] = &taskstore.Task{
		TaskID:        "live-task",
		Title:         taskstore.SqlStr("Still here"),
		Status:        taskstore.SqlStr("needsAction"),
		LastModified:  sql.NullInt64{Int64: 1000, Valid: true},
		CompletedTime: sql.NullInt64{Int64: 1000, Valid: true},
	}
	h := &gocaldav.Handler{Backend: NewBackend(store, "/caldav", "Tasks", "preserve", nil), Prefix: "/caldav"}

	do := func(method, name string) int {
		req := httptest.NewRequest(method, "/caldav/user/calendars/tasks/"+name, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	for _, name := range []string{
		"evt-ff52db77fa73aea9c99d86641e73e8bb.ics",
		"evt-ff52db77fa73aea9c99d86641e73e8bb-start.ics",
		"evt-ff52db77fa73aea9c99d86641e73e8bb-due.ics",
		"evt-ff52db77fa73aea9c99d86641e73e8bb-session-0.ics",
		"never-existed.ics",
	} {
		if got := do(http.MethodDelete, name); got != http.StatusNotFound {
			t.Errorf("DELETE %s = %d, want 404", name, got)
		}
		if got := do(http.MethodGet, name); got != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", name, got)
		}
	}

	// A real delete still succeeds, and repeating it is then a 404.
	if got := do(http.MethodDelete, "live-task.ics"); got != http.StatusNoContent {
		t.Fatalf("DELETE live-task.ics = %d, want 204", got)
	}
	if got := do(http.MethodDelete, "live-task.ics"); got != http.StatusNotFound {
		t.Errorf("repeat DELETE live-task.ics = %d, want 404", got)
	}
}

// TestMultigetMissingHrefIs404 checks that a calendar-multiget naming a gone
// object reports 404 for that href inside the 207, rather than a 500 that
// clients may treat as a transient server fault and retry.
func TestMultigetMissingHrefIs404(t *testing.T) {
	h := &gocaldav.Handler{Backend: NewBackend(newMockTaskStore(), "/caldav", "Tasks", "preserve", nil), Prefix: "/caldav"}
	body := `<?xml version="1.0" encoding="utf-8"?>
<C:calendar-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop><D:getetag/><C:calendar-data/></D:prop>
  <D:href>/caldav/user/calendars/tasks/gone.ics</D:href>
</C:calendar-multiget>`
	req := httptest.NewRequest("REPORT", "/caldav/user/calendars/tasks/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/xml")
	req.Header.Set("Depth", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("REPORT = %d, want 207; body: %s", rec.Code, rec.Body.String())
	}
	resp := rec.Body.String()
	if !strings.Contains(resp, "404") || strings.Contains(resp, "500") {
		t.Errorf("multiget for a missing href should carry 404, not 500; body: %s", resp)
	}
}
