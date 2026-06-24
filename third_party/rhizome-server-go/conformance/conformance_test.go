// Package conformance runs the shared, language-neutral vectors in /conformance/vectors against
// the Go implementation. It is the Go half of the dual-language contract (the Kotlin client runs
// the same vectors). See /conformance/README.md for the format and the loader contract.
//
// Phase 3: `merge` vectors are asserted against syncstore.Merge (registry-driven knownCols), and
// `wire-codec` against wirecodec.Encode/Decode — proving Kotlin↔Go agreement. Any other category
// is skipped LOUDLY (t.Skip with a reason), never a silent pass.
package conformance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jdkruzr/rhizome/server-go/compaction"
	"github.com/jdkruzr/rhizome/server-go/hlc"
	"github.com/jdkruzr/rhizome/server-go/registry"
	"github.com/jdkruzr/rhizome/server-go/schemaevo"
	"github.com/jdkruzr/rhizome/server-go/syncstore"
	"github.com/jdkruzr/rhizome/server-go/wirecodec"
)

// vectorsDir walks up from the test's working directory to find the shared
// vectors. Standalone Rhizome checkouts keep them under /conformance/vectors;
// UltraBridge vendors this module with the shared contract under /docs/sync/vectors.
func vectorsDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		for _, rel := range []string{
			filepath.Join("conformance", "vectors"),
			filepath.Join("docs", "sync", "vectors"),
		} {
			cand := filepath.Join(dir, rel)
			if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
				return cand
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate shared vectors above %q", dir)
		}
		dir = parent
	}
}

type wireOp struct {
	Table  string                     `json:"table"`
	PK     string                     `json:"pk"`
	SiteID string                     `json:"site_id"`
	OpSeq  int64                      `json:"op_seq"`
	OpTs   int64                      `json:"op_ts"`
	WallTs int64                      `json:"wall_ts"`
	Cols   map[string]json.RawMessage `json:"cols"`
}

func (w wireOp) toOp() syncstore.Op {
	return syncstore.Op{Table: w.Table, PK: w.PK, SiteID: w.SiteID, OpSeq: w.OpSeq, OpTs: w.timestamp(), Cols: w.Cols}
}

func (w wireOp) timestamp() int64 {
	if w.OpTs != 0 {
		return w.OpTs
	}
	return w.WallTs
}

type wireCase struct {
	Type string          `json:"type"`
	Wire json.RawMessage `json:"wire"`
}

type hlcStep struct {
	Op     string `json:"op"`
	Wall   int64  `json:"wall"`
	Remote int64  `json:"remote"`
	Expect int64  `json:"expect"`
}

// logEntry is a sequenced op in a compaction vector's log / expected_log.
type logEntry struct {
	Seq int64  `json:"seq"`
	Op  wireOp `json:"op"`
}

type vector struct {
	Category      string              `json:"category"`
	Name          string              `json:"name"`
	Description   string              `json:"description"`
	Ops           []wireOp            `json:"ops"`
	ExpectedState map[string][]wireOp `json:"expected_state"`
	Cases         []wireCase          `json:"cases"`
	Initial       int64               `json:"initial"`
	Steps         []hlcStep           `json:"steps"`
	TombstoneCols map[string]string   `json:"tombstone_cols"`
	Watermark     int64               `json:"watermark"`
	Log           []logEntry          `json:"log"`
	ExpectedLog   []logEntry          `json:"expected_log"`
	// schema-evolution (§I.9). A null stored_hash unmarshals to "" (never reconciled).
	StoredHash         string `json:"stored_hash"`
	CurrentHash        string `json:"current_hash"`
	Cursor             int64  `json:"cursor"`
	ExpectedCursor     int64  `json:"expected_cursor"`
	ExpectedStoredHash string `json:"expected_stored_hash"`
}

func loadVectors(t *testing.T) []struct {
	file string
	v    vector
} {
	t.Helper()
	dir := vectorsDir(t)
	matches, err := filepath.Glob(filepath.Join(dir, "*.vector.json"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) == 0 {
		t.Fatalf("no vectors found in %q", dir)
	}
	out := make([]struct {
		file string
		v    vector
	}, 0, len(matches))
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			t.Fatalf("read %s: %v", m, err)
		}
		var v vector
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatalf("parse %s: %v", filepath.Base(m), err)
		}
		out = append(out, struct {
			file string
			v    vector
		}{filepath.Base(m), v})
	}
	return out
}

// knownCols normalizes the (ForestNote-derived) merge vectors — the Go half of the contract.
var knownCols = registry.ForestNote().KnownCols()

func TestVectors(t *testing.T) {
	var merge, wireCodec, hlcN, compactionN, schemaEvoN, skipped int
	var sawExplicitCategory bool
	for _, entry := range loadVectors(t) {
		entry := entry
		t.Run(entry.v.Name, func(t *testing.T) {
			if entry.v.Name == "" {
				t.Fatalf("%s: missing name", entry.file)
			}
			category := entry.v.Category
			if category != "" {
				sawExplicitCategory = true
			} else if entry.v.ExpectedState != nil {
				// UltraBridge's vendored shared vectors use the original merge-only
				// shape: no category field, ops + expected_state, and wall_ts.
				category = "merge"
			}
			switch category {
			case "merge":
				assertMerge(t, entry.v)
				merge++
			case "wire-codec":
				assertWireCodec(t, entry.v)
				wireCodec++
			case "hlc":
				assertHlc(t, entry.v)
				hlcN++
			case "compaction":
				assertCompaction(t, entry.v)
				compactionN++
			case "schema-evolution":
				assertSchemaEvolution(t, entry.v)
				schemaEvoN++
			case "":
				t.Fatalf("%s: missing category", entry.file)
			default:
				skipped++
				t.Skipf("category %q not yet handled by the Go runner", entry.v.Category)
			}
		})
	}
	if merge == 0 {
		t.Fatalf("expected at least one merge vector")
	}
	if sawExplicitCategory && hlcN == 0 {
		t.Fatalf("expected at least one hlc vector")
	}
	if sawExplicitCategory && compactionN == 0 {
		t.Fatalf("expected at least one compaction vector")
	}
	if sawExplicitCategory && schemaEvoN == 0 {
		t.Fatalf("expected at least one schema-evolution vector")
	}
	t.Logf("conformance: %d merge, %d wire-codec, %d hlc, %d compaction, %d schema-evolution asserted, %d skipped", merge, wireCodec, hlcN, compactionN, schemaEvoN, skipped)
}

// assertSchemaEvolution drives the §I.9 reconcile rule over the vector's (stored, current, cursor)
// and asserts the resulting cursor + stored hash match — the canonical one-shot cursor-reset rule.
func assertSchemaEvolution(t *testing.T, v vector) {
	gotCursor, gotStored := schemaevo.Reconcile(v.StoredHash, v.CurrentHash, v.Cursor)
	if gotCursor != v.ExpectedCursor {
		t.Fatalf("%s: cursor got %d want %d", v.Name, gotCursor, v.ExpectedCursor)
	}
	if gotStored != v.ExpectedStoredHash {
		t.Fatalf("%s: stored hash got %q want %q", v.Name, gotStored, v.ExpectedStoredHash)
	}
}

// assertCompaction drives compaction.Sweep over the vector's log and asserts the surviving entries
// equal expected_log exactly — same seqs, in order, with matching op identity and cols.
func assertCompaction(t *testing.T, v vector) {
	if len(v.Log) == 0 {
		t.Fatalf("%s: compaction vector has no log", v.Name)
	}
	entries := make([]compaction.Entry, len(v.Log))
	for i, e := range v.Log {
		entries[i] = compaction.Entry{Seq: e.Seq, Op: e.Op.toOp()}
	}
	kept, _ := compaction.Sweep(entries, v.TombstoneCols, v.Watermark)
	if len(kept) != len(v.ExpectedLog) {
		t.Fatalf("%s: kept %d entries, want %d", v.Name, len(kept), len(v.ExpectedLog))
	}
	for i, want := range v.ExpectedLog {
		got := kept[i]
		if got.Seq != want.Seq {
			t.Fatalf("%s / entry %d: seq = %d, want %d (order or selection wrong)", v.Name, i, got.Seq, want.Seq)
		}
		w := want.Op
		if got.Op.PK != w.PK || got.Op.Table != w.Table || got.Op.SiteID != w.SiteID || got.Op.OpSeq != w.OpSeq || got.Op.OpTs != w.timestamp() {
			t.Fatalf("%s / entry %d (seq %d): identity (%s,%s,%s,%d,%d) want (%s,%s,%s,%d,%d)",
				v.Name, i, got.Seq, got.Op.Table, got.Op.PK, got.Op.SiteID, got.Op.OpSeq, got.Op.OpTs,
				w.Table, w.PK, w.SiteID, w.OpSeq, w.timestamp())
		}
		if !colsEqual(got.Op.Cols, w.Cols) {
			t.Fatalf("%s / entry %d (seq %d): cols mismatch\n got %v\nwant %v", v.Name, i, got.Seq, got.Op.Cols, w.Cols)
		}
	}
}

func assertHlc(t *testing.T, v vector) {
	var wall int64
	clock := hlc.New(v.Initial, func() int64 { return wall })
	for i, step := range v.Steps {
		wall = step.Wall
		var got int64
		switch step.Op {
		case "local":
			got = clock.LocalEvent()
		case "receive":
			got = clock.ReceiveEvent(step.Remote)
		default:
			t.Fatalf("%s / step %d: unknown hlc op %q", v.Name, i, step.Op)
		}
		if got != step.Expect {
			t.Fatalf("%s / step %d (%s, wall=%d): got %d want %d", v.Name, i, step.Op, step.Wall, got, step.Expect)
		}
	}
}

func assertMerge(t *testing.T, v vector) {
	if len(v.Ops) == 0 {
		t.Fatalf("%s: merge vector has no ops", v.Name)
	}
	if v.ExpectedState == nil {
		t.Fatalf("%s: merge vector has no expected_state", v.Name)
	}
	ops := make([]syncstore.Op, len(v.Ops))
	for i, w := range v.Ops {
		ops[i] = w.toOp()
	}
	winners := syncstore.Merge(ops, knownCols)

	for table, rows := range v.ExpectedState {
		expected := make(map[string]wireOp, len(rows))
		for _, r := range rows {
			expected[r.PK] = r
		}
		got := make(map[string]syncstore.Op)
		for k, w := range winners {
			if k.Table == table {
				got[k.PK] = w
			}
		}
		if len(got) != len(expected) {
			t.Fatalf("%s / %s: surviving pk count = %d, want %d", v.Name, table, len(got), len(expected))
		}
		for pk, er := range expected {
			w, ok := got[pk]
			if !ok {
				t.Fatalf("%s / %s / %s: expected surviving row missing", v.Name, table, pk)
			}
			if w.SiteID != er.SiteID || w.OpSeq != er.OpSeq || w.OpTs != er.timestamp() {
				t.Fatalf("%s / %s / %s: winner key (%s,%d,%d) want (%s,%d,%d)",
					v.Name, table, pk, w.SiteID, w.OpSeq, w.OpTs, er.SiteID, er.OpSeq, er.timestamp())
			}
			if !colsEqual(w.Cols, er.Cols) {
				t.Fatalf("%s / %s / %s: cols mismatch\n got %v\nwant %v", v.Name, table, pk, w.Cols, er.Cols)
			}
		}
	}
}

func assertWireCodec(t *testing.T, v vector) {
	for i, c := range v.Cases {
		native, err := wirecodec.Decode(registry.ColumnType(c.Type), c.Wire)
		if err != nil {
			t.Fatalf("%s / case %d (%s): decode: %v", v.Name, i, c.Type, err)
		}
		re, err := wirecodec.Encode(registry.ColumnType(c.Type), native)
		if err != nil {
			t.Fatalf("%s / case %d (%s): encode: %v", v.Name, i, c.Type, err)
		}
		if !jsonEqual(re, c.Wire) {
			t.Fatalf("%s / case %d (%s): wire round-trip got %s want %s", v.Name, i, c.Type, re, c.Wire)
		}
	}
}

// colsEqual compares two cols maps for semantic JSON equality (formatting-independent).
func colsEqual(a, b map[string]json.RawMessage) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || !jsonEqual(av, bv) {
			return false
		}
	}
	return true
}

// jsonEqual reports whether two raw JSON values are semantically equal (parsed, then DeepEqual) —
// robust to whitespace and integer-vs-float formatting differences between the two sides.
func jsonEqual(a, b json.RawMessage) bool {
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}
