package web

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sysop/ultrabridge/internal/source"
)

// TestForestNoteCompactionToggle covers the relay-log compaction toggle's write-through to the
// ForestNote source row's config_json (#16): merging the flag preserves batch_limit, flips
// RestartRequired, and prefill reads it back. The toggle does NOT live in appconfig — it must land
// in the source row the running source reads at Start.
func TestForestNoteCompactionToggle(t *testing.T) {
	db := initSourceTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// A seeded ForestNote source row carrying an existing config key we must not clobber.
	if _, err := source.AddSource(ctx, db, source.SourceRow{
		Type: "forestnote", Name: "ForestNote", Enabled: true, ConfigJSON: `{"batch_limit":500}`,
	}); err != nil {
		t.Fatalf("seed forestnote source: %v", err)
	}
	h := setupTestHandler(t, db)

	// The toggle finds the row.
	row, ok := h.forestNoteSourceRow(ctx)
	if !ok {
		t.Fatal("forestNoteSourceRow: expected the seeded ForestNote row")
	}
	if sourceConfigBool(row.ConfigJSON, "compaction") {
		t.Fatal("prefill: compaction should default OFF on a fresh row")
	}

	// Enabling merges compaction:true WITHOUT dropping batch_limit, and flags a restart.
	if err := h.setSourceConfigBool(ctx, row, "compaction", true); err != nil {
		t.Fatalf("enable compaction: %v", err)
	}
	if !h.config.IsRestartRequired() {
		t.Error("enabling compaction must flag RestartRequired (source config is read once at Start)")
	}
	row, ok = h.forestNoteSourceRow(ctx)
	if !ok {
		t.Fatal("row vanished after update")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(row.ConfigJSON), &m); err != nil {
		t.Fatalf("reparse config_json: %v", err)
	}
	if b, _ := m["compaction"].(bool); !b {
		t.Errorf("compaction not persisted: %s", row.ConfigJSON)
	}
	// JSON numbers decode as float64; batch_limit must survive the merge.
	if v, _ := m["batch_limit"].(float64); v != 500 {
		t.Errorf("batch_limit clobbered by the merge: got %v, want 500 (%s)", v, row.ConfigJSON)
	}
	if !sourceConfigBool(row.ConfigJSON, "compaction") {
		t.Error("prefill: should read back compaction ON after enabling")
	}

	// Disabling flips it back to false, still preserving batch_limit.
	if err := h.setSourceConfigBool(ctx, row, "compaction", false); err != nil {
		t.Fatalf("disable compaction: %v", err)
	}
	row, _ = h.forestNoteSourceRow(ctx)
	if sourceConfigBool(row.ConfigJSON, "compaction") {
		t.Error("compaction should read OFF after disabling")
	}
	_ = json.Unmarshal([]byte(row.ConfigJSON), &m)
	if v, _ := m["batch_limit"].(float64); v != 500 {
		t.Errorf("batch_limit lost on disable: %s", row.ConfigJSON)
	}
}

// TestForestNoteCompactionToggle_NoSource gates the toggle off when no ForestNote source exists —
// nothing to compact, so the checkbox is hidden and the save path is a no-op.
func TestForestNoteCompactionToggle_NoSource(t *testing.T) {
	db := initSourceTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// Only a non-ForestNote source present.
	if _, err := source.AddSource(ctx, db, source.SourceRow{
		Type: "boox", Name: "Boox", Enabled: true, ConfigJSON: `{}`,
	}); err != nil {
		t.Fatalf("seed boox source: %v", err)
	}
	h := setupTestHandler(t, db)

	if _, ok := h.forestNoteSourceRow(ctx); ok {
		t.Error("forestNoteSourceRow must return ok=false when no ForestNote source is configured")
	}
}

// TestSourceConfigBool pins the defensive parsing: empty/garbage/missing-key all read false.
func TestSourceConfigBool(t *testing.T) {
	cases := []struct {
		name, json, key string
		want            bool
	}{
		{"empty", "", "compaction", false},
		{"garbage", "{not json", "compaction", false},
		{"missing", `{"batch_limit":500}`, "compaction", false},
		{"present true", `{"compaction":true}`, "compaction", true},
		{"present false", `{"compaction":false}`, "compaction", false},
		{"wrong type", `{"compaction":"yes"}`, "compaction", false},
	}
	for _, c := range cases {
		if got := sourceConfigBool(c.json, c.key); got != c.want {
			t.Errorf("%s: sourceConfigBool(%q,%q)=%v want %v", c.name, c.json, c.key, got, c.want)
		}
	}
}
