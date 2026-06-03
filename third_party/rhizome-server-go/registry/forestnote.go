package registry

// ForestNote is ForestNote's synced data shape declared as a Registry — the canonical worked
// example AND the live-cutover guard: its SchemaHash MUST equal ForestNote's production v3 hash
// (see registry_test.go and the Kotlin SchemaHashTest). At Phase 8 this declaration moves into
// ForestNote/UltraBridge; it lives here now so both the registry tests and the conformance runner
// (which normalizes the migrated UB merge vectors) share one definition.
//
// Columns are the NON-pk columns (pk "id" rides in the op's pk field). Only column NAMES affect
// the schema hash; types matter for the wire codec and the optional mirror's affinities.
func ForestNote() Registry {
	ts := func(name string, nullable bool) Column {
		return Column{Name: name, Type: Timestamp, Nullable: nullable}
	}
	return Registry{Tables: []Table{
		{
			Name: "folder", PK: "id", Tombstone: "deleted_at",
			Columns: []Column{
				{Name: "name", Type: Text},
				{Name: "sort_order", Type: Int},
				ts("created_at", false),
				ts("deleted_at", true),
				{Name: "parent_folder_id", Type: Text, Nullable: true},
			},
		},
		{
			Name: "notebook", PK: "id", Tombstone: "deleted_at",
			Columns: []Column{
				{Name: "name", Type: Text},
				{Name: "sort_order", Type: Int},
				ts("created_at", false),
				ts("deleted_at", true),
				{Name: "folder_id", Type: Text, Nullable: true},
			},
		},
		{
			Name: "page", PK: "id", Tombstone: "deleted_at",
			Columns: []Column{
				{Name: "notebook_id", Type: Text},
				{Name: "sort_order", Type: Int},
				ts("created_at", false),
				ts("deleted_at", true),
				{Name: "template", Type: Text, Nullable: true},
				{Name: "template_pitch_mm", Type: Int, Nullable: true},
			},
		},
		{
			Name: "stroke", PK: "id", Tombstone: "deleted_at",
			Columns: []Column{
				{Name: "page_id", Type: Text},
				{Name: "color", Type: ColorInt},
				{Name: "pen_width_min", Type: Int},
				{Name: "pen_width_max", Type: Int},
				{Name: "points", Type: Blob},
				{Name: "z", Type: Int},
				ts("created_at", false),
				ts("deleted_at", true),
			},
		},
		{
			Name: "text_box", PK: "id", Tombstone: "deleted_at",
			Columns: []Column{
				{Name: "page_id", Type: Text},
				{Name: "x", Type: Int}, {Name: "y", Type: Int},
				{Name: "width", Type: Int}, {Name: "height", Type: Int},
				{Name: "text", Type: Text},
				{Name: "font_name", Type: Text},
				{Name: "font_size", Type: Int},
				{Name: "color", Type: ColorInt},
				{Name: "weight", Type: Int},
				{Name: "border_width", Type: Int},
				{Name: "z", Type: Int},
				ts("created_at", false),
				ts("deleted_at", true),
			},
		},
		{
			Name: "page_text_from_server", PK: "id", Tombstone: "deleted_at", ServerAuthoredOnly: true,
			Columns: []Column{
				{Name: "text", Type: Text},
				ts("ocr_at", false),
				{Name: "model", Type: Text, Nullable: true},
				ts("created_at", false),
				ts("deleted_at", true),
			},
		},
		{
			Name: "page_text_from_client", PK: "id", Tombstone: "deleted_at",
			Columns: []Column{
				{Name: "text", Type: Text},
				ts("ocr_at", false),
				{Name: "model", Type: Text, Nullable: true},
				ts("created_at", false),
				ts("deleted_at", true),
			},
		},
	}}
}
