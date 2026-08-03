package caldav

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sysop/ultrabridge/internal/taskstore"
)

// stubSyncStore is a hand-fed SyncStore: the handler's behavior depends only on
// what these three calls return, so the tests set them directly rather than
// driving a database.
type stubSyncStore struct {
	max     int64
	floor   int64
	changed []taskstore.Task
	live    []taskstore.Task
	since   int64 // records what the handler asked for
}

func (s *stubSyncStore) MaxUpdatedAtAll(ctx context.Context) (int64, error) { return s.max, nil }
func (s *stubSyncStore) SyncFloor(ctx context.Context) (int64, error)       { return s.floor, nil }
func (s *stubSyncStore) ListChangedSince(ctx context.Context, sinceMs int64) ([]taskstore.Task, error) {
	s.since = sinceMs
	return s.changed, nil
}
func (s *stubSyncStore) List(ctx context.Context) ([]taskstore.Task, error) { return s.live, nil }

func task(id string, updatedAt int64, deleted bool) taskstore.Task {
	d := "N"
	if deleted {
		d = "Y"
	}
	return taskstore.Task{
		TaskID:    id,
		Title:     sql.NullString{String: id, Valid: true},
		Status:    sql.NullString{String: taskstore.StatusNeedsAction, Valid: true},
		UpdatedAt: updatedAt,
		IsDeleted: d,
	}
}

// syncRequest posts a sync-collection REPORT and returns the recorder.
func syncRequest(t *testing.T, store *stubSyncStore, body string) *httptest.ResponseRecorder {
	t.Helper()
	backend := NewBackend(newMockTaskStore(), "/caldav", "Tasks", "preserve", nil)
	passthrough := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot) // marks "fell through to the library"
	})
	h := SyncCollectionStub(passthrough, store, backend)
	req := httptest.NewRequest("REPORT", "/caldav/user/calendars/tasks/", strings.NewReader(body))
	req.Header.Set("Depth", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const syncReportBody = `<?xml version="1.0" encoding="utf-8"?>
<D:sync-collection xmlns:D="DAV:">
  <D:sync-token>%TOKEN%</D:sync-token>
  <D:sync-level>1</D:sync-level>
  <D:prop><D:getetag/></D:prop>
</D:sync-collection>`

func bodyWithToken(tok string) string {
	return strings.Replace(syncReportBody, "%TOKEN%", tok, 1)
}

// TestSyncCollection_InitialSync: a client with no prior state sends an empty
// sync-token and must receive every live resource plus a token to resume from.
// Tombstones are deliberately absent — a client that has never synced has
// nothing to forget.
func TestSyncCollection_InitialSync(t *testing.T) {
	store := &stubSyncStore{
		max:  5000,
		live: []taskstore.Task{task("alpha", 1000, false), task("beta", 2000, false)},
	}
	rec := syncRequest(t, store, bodyWithToken(""))

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("status: got %d, want 207. body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"/caldav/user/calendars/tasks/alpha.ics",
		"/caldav/user/calendars/tasks/beta.ics",
		"<D:sync-token>urn:ultrabridge:sync:5000</D:sync-token>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("initial sync response missing %q\nbody=%s", want, body)
		}
	}
	if strings.Contains(body, "404 Not Found") {
		t.Error("initial sync should not report removals")
	}
}

// TestSyncCollection_IncrementalReportsRemovals: the point of RFC 6578. A
// deleted task must come back as a bare <status>404</status> response with no
// propstat, which is how a client learns to drop it.
func TestSyncCollection_IncrementalReportsRemovals(t *testing.T) {
	store := &stubSyncStore{
		max:     9000,
		changed: []taskstore.Task{task("edited", 8000, false), task("gone", 8500, true)},
	}
	rec := syncRequest(t, store, bodyWithToken("urn:ultrabridge:sync:7000"))

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("status: got %d, want 207. body=%s", rec.Code, rec.Body.String())
	}
	if store.since != 7000 {
		t.Errorf("queried from %d, want 7000 (the client's token)", store.since)
	}
	body := rec.Body.String()

	if !strings.Contains(body, "<D:status>HTTP/1.1 404 Not Found</D:status>") {
		t.Errorf("deleted task not reported as removed\nbody=%s", body)
	}
	// The tombstone response must carry no propstat — RFC 6578 §3.2.
	goneIdx := strings.Index(body, "gone.ics")
	if goneIdx < 0 {
		t.Fatalf("tombstone href missing\nbody=%s", body)
	}
	rest := body[goneIdx:]
	end := strings.Index(rest, "</D:response>")
	if end < 0 {
		t.Fatalf("malformed response element\nbody=%s", body)
	}
	if strings.Contains(rest[:end], "propstat") {
		t.Errorf("tombstone response carries a propstat; RFC 6578 wants a bare status\n%s", rest[:end])
	}
	if !strings.Contains(body, "<D:sync-token>urn:ultrabridge:sync:9000</D:sync-token>") {
		t.Errorf("missing or wrong new sync-token\nbody=%s", body)
	}
}

// TestSyncCollection_ExpiredTokenIsRejected: once tombstones are purged we can
// no longer tell a client what it missed, so its token must be refused with the
// RFC's precondition so it falls back to a full resync instead of silently
// keeping deleted tasks forever.
func TestSyncCollection_ExpiredTokenIsRejected(t *testing.T) {
	store := &stubSyncStore{max: 9000, floor: 5000}
	rec := syncRequest(t, store, bodyWithToken("urn:ultrabridge:sync:4000"))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403. body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "valid-sync-token") {
		t.Errorf("403 must carry DAV:valid-sync-token\nbody=%s", rec.Body.String())
	}
}

// TestSyncCollection_MalformedTokenIsRejected: a token we can't parse is
// indistinguishable from one we can't honor.
func TestSyncCollection_MalformedTokenIsRejected(t *testing.T) {
	store := &stubSyncStore{max: 9000}
	rec := syncRequest(t, store, bodyWithToken("not-a-token"))
	if rec.Code != http.StatusForbidden {
		t.Errorf("status: got %d, want 403 for an unparseable token", rec.Code)
	}
}

// TestSyncCollection_PassesThroughOtherReports: calendar-query and
// calendar-multiget must keep reaching the go-webdav library.
func TestSyncCollection_PassesThroughOtherReports(t *testing.T) {
	store := &stubSyncStore{max: 1}
	rec := syncRequest(t, store, `<?xml version="1.0"?>
<C:calendar-query xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:D="DAV:">
  <D:prop><D:getetag/></D:prop>
</C:calendar-query>`)
	if rec.Code != http.StatusTeapot {
		t.Errorf("calendar-query was intercepted (got %d); it must reach the library", rec.Code)
	}
}

// TestSyncCollection_CalendarDataIncludesBody: when the client asks for
// calendar-data it gets the VTODO inline, saving a follow-up multiget.
func TestSyncCollection_CalendarDataIncludesBody(t *testing.T) {
	store := &stubSyncStore{
		max:     3000,
		changed: []taskstore.Task{task("withbody", 2500, false)},
	}
	rec := syncRequest(t, store, `<?xml version="1.0" encoding="utf-8"?>
<D:sync-collection xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:sync-token>urn:ultrabridge:sync:2000</D:sync-token>
  <D:prop><D:getetag/><C:calendar-data/></D:prop>
</D:sync-collection>`)

	body := rec.Body.String()
	if !strings.Contains(body, "calendar-data") {
		t.Fatalf("no calendar-data element in response\nbody=%s", body)
	}
	for _, want := range []string{"BEGIN:VCALENDAR", "BEGIN:VTODO", "UID:withbody"} {
		if !strings.Contains(body, want) {
			t.Errorf("calendar-data missing %q\nbody=%s", want, body)
		}
	}
}

// TestSyncCollection_LimitTruncatesAndResumes: with DAV:limit the server may
// return a partial set, but the token must then point at the truncation
// boundary so the next request picks up exactly where this one stopped.
func TestSyncCollection_LimitTruncatesAndResumes(t *testing.T) {
	store := &stubSyncStore{
		max: 9000,
		changed: []taskstore.Task{
			task("one", 1000, false), task("two", 2000, false), task("three", 3000, false),
		},
	}
	rec := syncRequest(t, store, `<?xml version="1.0" encoding="utf-8"?>
<D:sync-collection xmlns:D="DAV:">
  <D:sync-token>urn:ultrabridge:sync:500</D:sync-token>
  <D:limit><D:nresults>2</D:nresults></D:limit>
  <D:prop><D:getetag/></D:prop>
</D:sync-collection>`)

	body := rec.Body.String()
	if strings.Contains(body, "three.ics") {
		t.Errorf("nresults=2 was not honored\nbody=%s", body)
	}
	// Token resumes at the last included row, not at the collection maximum —
	// otherwise the truncated remainder would be skipped forever.
	if !strings.Contains(body, "<D:sync-token>urn:ultrabridge:sync:2000</D:sync-token>") {
		t.Errorf("truncated response must resume at the boundary, not the max\nbody=%s", body)
	}
}
