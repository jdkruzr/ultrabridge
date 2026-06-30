package service

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sysop/ultrabridge/internal/rag"
)

// fakeRetriever captures the SearchRequest the service passed in so the
// limit-plumbing test can assert what reached the retrieval layer.
type fakeRetriever struct {
	lastReq rag.SearchRequest
	results []rag.SearchResult
}

func (f *fakeRetriever) Search(_ context.Context, req rag.SearchRequest) ([]rag.SearchResult, error) {
	f.lastReq = req
	return f.results, nil
}

// TestSearchService_LimitClamping is the service-layer half of the UB-1
// fix: confirms the service clamps a caller-supplied limit through the
// defined default-and-ceiling, then threads the resolved value into
// rag.SearchRequest.Limit. Previously the retriever was free to use its
// own default since no limit was passed.
func TestSearchService_LimitClamping(t *testing.T) {
	cases := []struct {
		name    string
		in      int
		wantOut int
	}{
		{"zero-passes-default", 0, searchDefaultLimit},
		{"negative-passes-default", -5, searchDefaultLimit},
		{"in-range-passes-verbatim", 7, 7},
		{"at-ceiling-passes-verbatim", searchMaxLimit, searchMaxLimit},
		{"above-ceiling-clamps-down", searchMaxLimit + 50, searchMaxLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRetriever{}
			svc := &searchService{
				retriever: r,
				logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			if _, err := svc.Search(context.Background(), "any query", "", nil, tc.in); err != nil {
				t.Fatalf("Search: %v", err)
			}
			if r.lastReq.Limit != tc.wantOut {
				t.Errorf("retriever Limit: got %d, want %d (caller passed %d)",
					r.lastReq.Limit, tc.wantOut, tc.in)
			}
			if r.lastReq.Mode != rag.SearchModeHybrid {
				t.Errorf("legacy Search mode = %q, want hybrid", r.lastReq.Mode)
			}
		})
	}

	t.Run("nil retriever returns nil safely", func(t *testing.T) {
		svc := &searchService{
			retriever: nil,
			logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		got, err := svc.Search(context.Background(), "q", "", nil, 10)
		if err != nil {
			t.Errorf("nil-retriever Search should not error: %v", err)
		}
		if got != nil {
			t.Errorf("nil-retriever Search should return nil; got %v", got)
		}
	})
}

func TestSearchService_RequestPlumbingAndResultMapping(t *testing.T) {
	body := "intro " + strings.Repeat("context ", 30) + "needle " + strings.Repeat("tail ", 30)
	r := &fakeRetriever{results: []rag.SearchResult{{
		NotePath:   "/notes/work/alpha.note",
		Page:       3,
		TitleText:  "Alpha",
		BodyText:   body,
		Score:      0.75,
		SourceType: rag.SourceBoox,
		Device:     "Palma2",
	}}}
	svc := &searchService{
		retriever: r,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	got, err := svc.Search(context.Background(), "needle", "work", []string{rag.SourceBoox}, 9)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if r.lastReq.Query != "needle" || r.lastReq.Folder != "work" || r.lastReq.Limit != 9 {
		t.Fatalf("request not plumbed: %+v", r.lastReq)
	}
	if len(r.lastReq.Sources) != 1 || r.lastReq.Sources[0] != rag.SourceBoox {
		t.Fatalf("sources not plumbed: %+v", r.lastReq.Sources)
	}
	if len(got) != 1 {
		t.Fatalf("results len = %d, want 1", len(got))
	}
	res := got[0]
	if res.Path != "/notes/work/alpha.note" || res.Page != 3 || res.Title != "Alpha" || res.SourceType != rag.SourceBoox || res.Device != "Palma2" || res.Score != float32(0.75) {
		t.Fatalf("result mapping mismatch: %+v", res)
	}
	if !strings.Contains(res.Snippet, "needle") || !strings.HasPrefix(res.Snippet, "…") {
		t.Fatalf("snippet = %q; want centered truncation containing query", res.Snippet)
	}
}

func TestSearchService_MetadataPageSentinelIsNotExposed(t *testing.T) {
	r := &fakeRetriever{results: []rag.SearchResult{{
		NotePath:   "remarkable://doc-1",
		Page:       -1,
		TitleText:  "Alpha Plan",
		BodyText:   "Alpha Plan\nFolder: Projects",
		SourceType: rag.SourceRemarkable,
	}}}
	svc := &searchService{
		retriever: r,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	got, err := svc.Search(context.Background(), "Alpha", "", []string{rag.SourceRemarkable}, 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("results len = %d, want 1", len(got))
	}
	if got[0].Page != 0 {
		t.Fatalf("display page = %d, want sentinel hidden as 0", got[0].Page)
	}
	if got[0].Title != "Alpha Plan" || got[0].Path != "remarkable://doc-1" {
		t.Fatalf("result = %+v", got[0])
	}
}

func TestSearchService_AdvancedOptionsPlumbToRetriever(t *testing.T) {
	createdFrom := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	modifiedTo := time.Date(2026, 6, 30, 23, 59, 59, 0, time.UTC)
	r := &fakeRetriever{}
	svc := &searchService{
		retriever: r,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	_, err := svc.SearchAdvanced(context.Background(), "alpha", SearchOptions{
		DeviceModel: "Palma2",
		Sources:     []string{rag.SourceForestNote},
		Locations:   []SearchLocationFilter{{Source: rag.SourceForestNote, ID: "folder-1", FullPath: "Work"}},
		CreatedFrom: createdFrom,
		ModifiedTo:  modifiedTo,
		Sort:        "date_desc",
		Mode:        SearchModeKeyword,
		Limit:       12,
	})
	if err != nil {
		t.Fatalf("SearchAdvanced: %v", err)
	}
	if r.lastReq.Sort != "date_desc" || r.lastReq.Limit != 12 {
		t.Fatalf("sort/limit not plumbed: %+v", r.lastReq)
	}
	if r.lastReq.Device != "Palma2" {
		t.Fatalf("device model not plumbed: %+v", r.lastReq)
	}
	if r.lastReq.Mode != rag.SearchModeKeyword {
		t.Fatalf("mode not plumbed: %+v", r.lastReq)
	}
	if !r.lastReq.CreatedFrom.Equal(createdFrom) || !r.lastReq.ModifiedTo.Equal(modifiedTo) {
		t.Fatalf("dates not plumbed: %+v", r.lastReq)
	}
	if len(r.lastReq.Locations) != 1 || r.lastReq.Locations[0].ID != "folder-1" || r.lastReq.Locations[0].FullPath != "Work" {
		t.Fatalf("locations not plumbed: %+v", r.lastReq.Locations)
	}
}

func TestMakeSnippetEdges(t *testing.T) {
	if got := makeSnippet("short body", "missing", 20); got != "short body" {
		t.Fatalf("short snippet = %q", got)
	}
	long := strings.Repeat("abc ", 100)
	if got := makeSnippet(long, "not-present", 25); len(got) > 28 || !strings.HasSuffix(got, "…") {
		t.Fatalf("missing-term snippet = %q", got)
	}
	centered := makeSnippet(strings.Repeat("left ", 40)+"needle "+strings.Repeat("right ", 40), "needle", 60)
	if !strings.HasPrefix(centered, "…") || !strings.HasSuffix(centered, "…") || !strings.Contains(centered, "needle") {
		t.Fatalf("centered snippet = %q", centered)
	}
}
