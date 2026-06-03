package registry

import "testing"

// The live-cutover guard (Go half): ForestNote's registry MUST reproduce its production v3 schema
// hash, and the canonical string MUST be byte-identical to the Kotlin side. If either drifts this
// goes red before any device is affected. Mirrors the Kotlin SchemaHashTest.
func TestForestNoteReproducesV3Hash(t *testing.T) {
	const v3 = "724411eb845ad3487393a77cb5559690e69332c35fdb5ee3e85c1767bf71f3fe"
	if got := ForestNote().SchemaHash(); got != v3 {
		t.Fatalf("schema hash mismatch:\n got %s\nwant %s", got, v3)
	}
}

func TestCanonicalStringIsTablesThenColumnsAlphabetical(t *testing.T) {
	const want = "folder:created_at,deleted_at,name,parent_folder_id,sort_order;" +
		"notebook:created_at,deleted_at,folder_id,name,sort_order;" +
		"page:created_at,deleted_at,notebook_id,sort_order,template,template_pitch_mm;" +
		"page_text_from_client:created_at,deleted_at,model,ocr_at,text;" +
		"page_text_from_server:created_at,deleted_at,model,ocr_at,text;" +
		"stroke:color,created_at,deleted_at,page_id,pen_width_max,pen_width_min,points,z;" +
		"text_box:border_width,color,created_at,deleted_at,font_name,font_size,height," +
		"page_id,text,weight,width,x,y,z"
	if got := ForestNote().Canonical(); got != want {
		t.Fatalf("canonical mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestKnownColsAreSortedPerTable(t *testing.T) {
	kc := ForestNote().KnownCols()
	stroke := kc["stroke"]
	want := []string{"color", "created_at", "deleted_at", "page_id", "pen_width_max", "pen_width_min", "points", "z"}
	if len(stroke) != len(want) {
		t.Fatalf("stroke knownCols = %v, want %v", stroke, want)
	}
	for i := range want {
		if stroke[i] != want[i] {
			t.Fatalf("stroke knownCols[%d] = %q, want %q (full %v)", i, stroke[i], want[i], stroke)
		}
	}
}
