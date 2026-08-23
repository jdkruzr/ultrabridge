package registry

import "testing"

// The live-cutover guard (Go half): ForestNote's registry MUST reproduce its production v5 schema
// hash, and the canonical string MUST be byte-identical to the Kotlin side. If either drifts this
// goes red before any device is affected. Mirrors the Kotlin SchemaHashTest.
func TestForestNoteReproducesV5Hash(t *testing.T) {
	const v5 = "ed367ffd86b24c3b53f7a85b4f46b7f0cb69e0c6fbd0e1048289a659b4c967dd"
	if got := ForestNote().SchemaHash(); got != v5 {
		t.Fatalf("schema hash mismatch:\n got %s\nwant %s", got, v5)
	}
}

func TestCanonicalStringIsTablesThenColumnsAlphabetical(t *testing.T) {
	const want = "folder:created_at,deleted_at,name,parent_folder_id,sort_order;" +
		"notebook:aspect_long_axis,created_at,deleted_at,folder_id,name,page_height,page_width,sort_order;" +
		"page:created_at,deleted_at,notebook_id,sort_order,template,template_pitch_mm;" +
		"page_text_from_client:created_at,deleted_at,model,ocr_at,text;" +
		"page_text_from_server:created_at,deleted_at,model,ocr_at,text;" +
		"stroke:brush_kind,brush_seed,brush_version,color,created_at,deleted_at,page_id,pen_width_max,pen_width_min,point_dynamics,points,z;" +
		"text_box:border_width,color,created_at,deleted_at,font_name,font_size,height," +
		"page_id,text,weight,width,x,y,z"
	if got := ForestNote().Canonical(); got != want {
		t.Fatalf("canonical mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestKnownColsAreSortedPerTable(t *testing.T) {
	kc := ForestNote().KnownCols()
	stroke := kc["stroke"]
	want := []string{"brush_kind", "brush_seed", "brush_version", "color", "created_at", "deleted_at", "page_id", "pen_width_max", "pen_width_min", "point_dynamics", "points", "z"}
	if len(stroke) != len(want) {
		t.Fatalf("stroke knownCols = %v, want %v", stroke, want)
	}
	for i := range want {
		if stroke[i] != want[i] {
			t.Fatalf("stroke knownCols[%d] = %q, want %q (full %v)", i, stroke[i], want[i], stroke)
		}
	}
}
