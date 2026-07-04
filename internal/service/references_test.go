package service

import (
	"testing"

	"github.com/sysop/ultrabridge/internal/fnpath"
)

func TestReferenceForPath(t *testing.T) {
	cases := []struct {
		name       string
		path       string
		sourceType string
		page       int
		wantDetail string
		wantNative string
	}{
		{
			name:       "supernote file",
			path:       "/notes/Work Plan.note",
			sourceType: "supernote",
			wantDetail: "/files/supernote?detail=%2Fnotes%2FWork+Plan.note",
		},
		{
			name:       "boox file",
			path:       "/boox/Palma/Note/Work.note",
			sourceType: "boox",
			wantDetail: "/files/boox?detail=%2Fboox%2FPalma%2FNote%2FWork.note",
		},
		{
			name:       "forestnote page",
			path:       fnpath.Page("NB1", "PG1"),
			sourceType: "forestnote",
			wantDetail: "/files/forestnote?notebook=NB1&page=PG1",
			wantNative: "forestnote://notebook/NB1/page/PG1",
		},
		{
			name:       "forestnote notebook",
			path:       fnpath.Notebook("NB1"),
			sourceType: "forestnote",
			wantDetail: "/files/forestnote?notebook=NB1",
		},
		{
			name:       "remarkable document",
			path:       RemarkablePath("doc-1"),
			sourceType: "remarkable",
			wantDetail: "/files/remarkable?document=doc-1",
		},
		{
			name:       "derive forestnote",
			path:       fnpath.Page("NB2", "PG2"),
			wantDetail: "/files/forestnote?notebook=NB2&page=PG2",
			wantNative: "forestnote://notebook/NB2/page/PG2",
		},
		{
			name:       "unknown source stays empty",
			path:       "/notes/Work Plan.note",
			wantDetail: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ReferenceForPath(tc.path, tc.sourceType, tc.page)
			if got.DetailPath != tc.wantDetail || got.NativeURL != tc.wantNative {
				t.Fatalf("ReferenceForPath = %+v, want detail=%q native=%q", got, tc.wantDetail, tc.wantNative)
			}
		})
	}
}
