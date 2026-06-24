package service

import "testing"

func TestSearchLocationEncodingRoundTrips(t *testing.T) {
	value := encodeSearchLocation("forestnote", "folder-1", "Projects/Work")
	got, ok := ParseSearchLocation(value)
	if !ok {
		t.Fatalf("ParseSearchLocation failed for %q", value)
	}
	if got.Source != "forestnote" || got.ID != "folder-1" || got.FullPath != "Projects/Work" {
		t.Fatalf("decoded location = %+v", got)
	}

	agg := encodeSearchLocation("", "", "Work")
	got, ok = ParseSearchLocation(agg)
	if !ok {
		t.Fatalf("ParseSearchLocation failed for aggregate %q", agg)
	}
	if got.Source != "" || got.FullPath != "Work" {
		t.Fatalf("decoded aggregate = %+v", got)
	}
}

func TestAggregateSearchLocationsRequiresSameFullPathAcrossSources(t *testing.T) {
	got := aggregateSearchLocations([]SearchLocation{
		{Source: "boox", FullPath: "Work", Count: 2},
		{Source: "supernote", FullPath: "Work", Count: 0},
		{Source: "forestnote", FullPath: "Personal/Work", Count: 0},
	})
	if len(got) != 1 {
		t.Fatalf("aggregates = %+v, want one exact Work aggregate", got)
	}
	if got[0].Source != "" || got[0].FullPath != "Work" || got[0].Count != 2 {
		t.Fatalf("aggregate = %+v", got[0])
	}
}
