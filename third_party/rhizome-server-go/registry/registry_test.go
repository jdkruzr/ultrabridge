package registry

import "testing"

// The live-cutover guard (Go half): ForestNote's registry MUST reproduce its production v4 schema
// hash, and the canonical string MUST be byte-identical to the Kotlin side. If either drifts this
// goes red before any device is affected. Mirrors the Kotlin SchemaHashTest.
func TestForestNoteReproducesV4Hash(t *testing.T) {
	const v4 = "74e6b5d790c919290d0e1fca3462800a5dc4abb288042dda2b48d4eb0482bbf2"
	if got := ForestNote().SchemaHash(); got != v4 {
		t.Fatalf("schema hash mismatch:\n got %s\nwant %s", got, v4)
	}
}

func TestCanonicalStringIsTablesThenColumnsAlphabetical(t *testing.T) {
	const want = "folder:created_at,deleted_at,name,parent_folder_id,sort_order;" +
		"notebook:aspect_long_axis,created_at,deleted_at,folder_id,name,sort_order;" +
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
