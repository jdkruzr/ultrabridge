package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sysop/ultrabridge/internal/service"
)

type fakeDigestService struct {
	items  []service.DigestView
	groups []service.DigestGroupView
}

func (f *fakeDigestService) ListDigests(_ context.Context, _, _ string, _, _ int) ([]service.DigestView, int, error) {
	return f.items, len(f.items), nil
}
func (f *fakeDigestService) ListGroups(_ context.Context) ([]service.DigestGroupView, error) {
	return f.groups, nil
}

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
