package service

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

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
	if res.Path != "/notes/work/alpha.note" || res.Page != 3 || res.Title != "Alpha" || res.SourceType != rag.SourceBoox || res.Score != float32(0.75) {
		t.Fatalf("result mapping mismatch: %+v", res)
	}
	if !strings.Contains(res.Snippet, "needle") || !strings.HasPrefix(res.Snippet, "…") {
		t.Fatalf("snippet = %q; want centered truncation containing query", res.Snippet)
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
