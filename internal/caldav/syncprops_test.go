package caldav

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// libraryResponse mimics what go-webdav emits for a Depth:0 PROPFIND on the
// collection: a 200 propstat for properties it knows and a 404 propstat listing
// the ones it doesn't. Captured from a live instance, prefixless namespaces and
// all — the injection has to cope with the real shape, not a tidied one.
const libraryResponse = `<?xml version="1.0" encoding="UTF-8"?>
<multistatus xmlns="DAV:"><response xmlns="DAV:"><href>/caldav/user/calendars/tasks/</href>` +
	`<propstat xmlns="DAV:"><prop xmlns="DAV:"><unknown-thing xmlns="DAV:"></unknown-thing></prop>` +
	`<status>HTTP/1.1 404 Not Found</status></propstat>` +
	`<propstat xmlns="DAV:"><prop xmlns="DAV:"><displayname xmlns="DAV:">Tasks</displayname></prop>` +
	`<status>HTTP/1.1 200 OK</status></propstat></response></multistatus>`

// propfindRequest drives the injection middleware with a PROPFIND body. The
// captured request the library would have received is returned too, so tests
// can assert that properties we answer ourselves were stripped from it.
func propfindRequest(t *testing.T, store *stubSyncStore, body string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	backend := NewBackend(newMockTaskStore(), "/caldav", "Tasks", "preserve", nil)
	var seenByLibrary string
	library := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seenByLibrary = string(b)
		w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = io.WriteString(w, libraryResponse)
	})
	h := SyncPropsStub(library, store, backend)
	req := httptest.NewRequest("PROPFIND", "/caldav/user/calendars/tasks/", strings.NewReader(body))
	req.Header.Set("Depth", "0")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, seenByLibrary
}

func propfindBody(props string) string {
	return `<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:" xmlns:CS="http://calendarserver.org/ns/">
  <D:prop>` + props + `</D:prop>
</D:propfind>`
}

// TestSyncProps_CtagAlone is Cfait's exact request: one property, which the
// library has no idea about. Answering it entirely ourselves avoids having to
// splice into a response at all.
func TestSyncProps_CtagAlone(t *testing.T) {
	store := &stubSyncStore{max: 4242}
	rec, _ := propfindRequest(t, store, propfindBody(`<CS:getctag/>`))

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("status: got %d, want 207. body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "urn:ultrabridge:sync:4242") {
		t.Errorf("ctag value missing\nbody=%s", body)
	}
	if !strings.Contains(body, "200 OK") {
		t.Errorf("ctag must come back in a 200 propstat\nbody=%s", body)
	}
	if strings.Contains(body, "404") {
		t.Errorf("ctag must not be reported as missing\nbody=%s", body)
	}
}

// TestSyncProps_TokenMatchesCtag: both carry the same value from the same
// query. A client that saw them diverge could bootstrap against the wrong one.
func TestSyncProps_TokenMatchesCtag(t *testing.T) {
	store := &stubSyncStore{max: 777}
	ctagRec, _ := propfindRequest(t, store, propfindBody(`<CS:getctag/>`))
	tokenRec, _ := propfindRequest(t, store, propfindBody(`<D:sync-token/>`))

	extract := func(body, tag string) string {
		i := strings.Index(body, ">urn:ultrabridge:sync:")
		if i < 0 {
			t.Fatalf("no token value in %s response: %s", tag, body)
		}
		rest := body[i+1:]
		return rest[:strings.Index(rest, "<")]
	}
	ctag := extract(ctagRec.Body.String(), "getctag")
	token := extract(tokenRec.Body.String(), "sync-token")
	if ctag != token {
		t.Errorf("getctag %q != sync-token %q; they must be the same value", ctag, token)
	}
}

// TestSyncProps_SupportedReportSet: the library never emits this property at
// all, so without it clients have no way to discover any report we support.
func TestSyncProps_SupportedReportSet(t *testing.T) {
	store := &stubSyncStore{max: 1}
	rec, _ := propfindRequest(t, store, propfindBody(`<D:supported-report-set/>`))

	body := rec.Body.String()
	for _, want := range []string{"sync-collection", "calendar-query", "calendar-multiget"} {
		if !strings.Contains(body, want) {
			t.Errorf("supported-report-set omits %q\nbody=%s", want, body)
		}
	}
}

// TestSyncProps_MixedRequestSplicesIntoLibraryResponse is the case a real
// client like DAVx5 produces: a long prop list mixing things the library knows
// with things only we do. The library's answers must survive intact and ours
// must be added alongside.
func TestSyncProps_MixedRequestSplicesIntoLibraryResponse(t *testing.T) {
	store := &stubSyncStore{max: 555}
	rec, seenByLibrary := propfindRequest(t, store,
		propfindBody(`<D:displayname/><D:sync-token/><D:unknown-thing/>`))

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("status: got %d, want 207. body=%s", rec.Code, rec.Body.String())
	}
	// Properties we own must not reach the library, or it reports them 404 and
	// the client sees the same property twice with contradictory statuses.
	if strings.Contains(seenByLibrary, "sync-token") {
		t.Errorf("sync-token was not stripped from the request\nlibrary saw: %s", seenByLibrary)
	}
	if !strings.Contains(seenByLibrary, "displayname") {
		t.Errorf("displayname must still reach the library\nlibrary saw: %s", seenByLibrary)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "Tasks") {
		t.Errorf("library's displayname answer was lost\nbody=%s", body)
	}
	if !strings.Contains(body, "urn:ultrabridge:sync:555") {
		t.Errorf("sync-token was not spliced in\nbody=%s", body)
	}
	// The library's own 404 for a genuinely unknown property must survive.
	if !strings.Contains(body, "404 Not Found") {
		t.Errorf("library's 404 propstat was lost\nbody=%s", body)
	}
}

// TestSyncProps_UnrelatedRequestPassesThrough: a PROPFIND naming none of our
// properties must reach the library byte-for-byte and come back untouched.
func TestSyncProps_UnrelatedRequestPassesThrough(t *testing.T) {
	store := &stubSyncStore{max: 1}
	rec, _ := propfindRequest(t, store, propfindBody(`<D:displayname/>`))
	if rec.Body.String() != libraryResponse {
		t.Errorf("response was modified despite naming none of our properties\ngot=%s", rec.Body.String())
	}
}

// TestSyncProps_AllpropUntouched: RFC 6578 §3 says DAV:sync-token is not
// returned for allprop. Injecting it there would push an expensive, easily
// stale value at clients that never asked.
func TestSyncProps_AllpropUntouched(t *testing.T) {
	store := &stubSyncStore{max: 1}
	rec, _ := propfindRequest(t, store, `<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:"><D:allprop/></D:propfind>`)
	if rec.Body.String() != libraryResponse {
		t.Errorf("allprop response was modified\ngot=%s", rec.Body.String())
	}
}
