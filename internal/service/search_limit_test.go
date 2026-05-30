package service

import (
	"context"
	"io"
	"log/slog"
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
