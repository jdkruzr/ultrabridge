package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sysop/ultrabridge/internal/digeststore"
	"github.com/sysop/ultrabridge/internal/service"
)

type fakeDigestService struct {
	items      []service.DigestView
	groups     []service.DigestGroupView
	deletedIDs []int64
	deleteErr  error
}

func (f *fakeDigestService) ListDigests(_ context.Context, _, _ string, _, _ int) ([]service.DigestView, int, error) {
	return f.items, len(f.items), nil
}
func (f *fakeDigestService) ListGroups(_ context.Context) ([]service.DigestGroupView, error) {
	return f.groups, nil
}
func (f *fakeDigestService) GetDigest(_ context.Context, id int64) (service.DigestView, error) {
	for _, it := range f.items {
		if it.ID == id {
			return it, nil
		}
	}
	return service.DigestView{}, digeststore.ErrNotFound
}
func (f *fakeDigestService) DeleteDigest(_ context.Context, id int64) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deletedIDs = append(f.deletedIDs, id)
	return nil
}
func (f *fakeDigestService) SetTombstoneQueue(service.DigestTombstoneQueue) {}

func TestDigestsTab_RendersItems(t *testing.T) {
	h := newTestHandler()
	h.SetDigestService(&fakeDigestService{
		items: []service.DigestView{
			{ID: 1, Name: "Quarterly goals", Excerpt: "ship the thing", Tags: []string{"work"}, Group: "Planning", SourceLabel: "Note", HasHandwriting: true, ModifiedAt: 1700000000000},
		},
		groups: []service.DigestGroupView{{UID: "g1", Name: "Planning"}},
	})

	req := httptest.NewRequest("GET", "/digests", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /digests = %d; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"Quarterly goals", "ship the thing", "Planning", "work", "✎"} {
		if !strings.Contains(body, want) {
			t.Errorf("digests page missing %q\n%s", want, body)
		}
	}
}

func TestDeleteDigest_CallsServiceAndShowsControl(t *testing.T) {
	h := newTestHandler()
	fake := &fakeDigestService{
		items: []service.DigestView{{ID: 7, Name: "Quarterly goals", Excerpt: "ship it", SourceLabel: "Note"}},
	}
	h.SetDigestService(fake)

	// The delete control must be present on the rendered tab.
	getReq := httptest.NewRequest("GET", "/digests", nil)
	getReq.Header.Set("HX-Request", "true")
	getW := httptest.NewRecorder()
	h.ServeHTTP(getW, getReq)
	if !strings.Contains(getW.Body.String(), "/digests/7") {
		t.Errorf("expected a delete control targeting /digests/7:\n%s", getW.Body.String())
	}

	// And the DELETE route must drive the service.
	req := httptest.NewRequest("DELETE", "/digests/7", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("DELETE /digests/7 = %d; body=%s", w.Code, w.Body.String())
	}
	if len(fake.deletedIDs) != 1 || fake.deletedIDs[0] != 7 {
		t.Errorf("DeleteDigest not called with 7: %v", fake.deletedIDs)
	}
}

func TestDeleteDigest_NilServiceNotFound(t *testing.T) {
	h := newTestHandler() // no digest service
	req := httptest.NewRequest("DELETE", "/digests/7", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 when digests disabled, got %d", w.Code)
	}
}

func TestDigestsTab_NilServiceShowsNotice(t *testing.T) {
	h := newTestHandler() // no digest service wired
	req := httptest.NewRequest("GET", "/digests", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /digests = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "device sync server") {
		t.Errorf("expected disabled notice, got:\n%s", w.Body.String())
	}
}

func TestDigestDetail_RendersAndLinks(t *testing.T) {
	h := newTestHandler()
	h.SetDigestService(&fakeDigestService{
		items: []service.DigestView{{
			ID: 5, Name: "Trip notes", Excerpt: "buy tickets", Comment: "call airline",
			Tags: []string{"travel"}, SourceLabel: "Note", SourceType: 2,
			SourcePath: "NOTE/Note/trip.note", HasHandwriting: true,
		}},
	})

	// The list row links into the detail page.
	listReq := httptest.NewRequest("GET", "/digests", nil)
	listReq.Header.Set("HX-Request", "true")
	listW := httptest.NewRecorder()
	h.ServeHTTP(listW, listReq)
	if !strings.Contains(listW.Body.String(), `hx-get="/digests/5"`) {
		t.Errorf("digest row should link to /digests/5:\n%s", listW.Body.String())
	}

	// The detail page renders the full excerpt + comment + source path.
	req := httptest.NewRequest("GET", "/digests/5", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /digests/5 = %d; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"Trip notes", "buy tickets", "call airline", "NOTE/Note/trip.note"} {
		if !strings.Contains(body, want) {
			t.Errorf("digest detail missing %q\n%s", want, body)
		}
	}
	// No SPC file root wired in this handler → no source image (avoids a
	// broken <img>), but the .mark note still shows.
	if strings.Contains(body, `src="/digests/5/render"`) {
		t.Errorf("expected no source image without an SPC file root")
	}
}

func TestDigestDetail_NotFound(t *testing.T) {
	h := newTestHandler()
	h.SetDigestService(&fakeDigestService{items: []service.DigestView{{ID: 1}}})
	req := httptest.NewRequest("GET", "/digests/999", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("GET /digests/999 = %d, want 404", w.Code)
	}
}

func TestDigestRender_PDFOrMissingIs404(t *testing.T) {
	h := newTestHandler()
	h.SetSPCFileRoot(t.TempDir()) // root set, but the source file won't exist
	h.SetDigestService(&fakeDigestService{
		items: []service.DigestView{
			{ID: 1, SourceType: 1, SourcePath: "doc.pdf"},           // PDF: unsupported
			{ID: 2, SourceType: 2, SourcePath: "NOTE/missing.note"}, // Note, but absent on disk
		},
	})
	for _, id := range []string{"1", "2"} {
		req := httptest.NewRequest("GET", "/digests/"+id+"/render", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("GET /digests/%s/render = %d, want 404", id, w.Code)
		}
	}
}
