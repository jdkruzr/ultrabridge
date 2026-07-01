package syncstore

import (
	"testing"

	"github.com/jdkruzr/rhizome/server-go/registry"
)

// These guards prove the RhizomeSync library registry reproduces ForestNote's live production schema
// (schemaHashV4, declared in op_test.go) byte-for-byte. They fail loudly if the library declaration
// and UB's knownCols/tableOrder ever disagree.

func TestRhizomeRegistryReproducesUBSchema(t *testing.T) {
	if got := SchemaHash(); got != schemaHashV4 {
		t.Fatalf("UB SchemaHash() = %s, want v4 %s", got, schemaHashV4)
	}

	reg := registry.ForestNote()
	if got := reg.SchemaHash(); got != schemaHashV4 {
		t.Fatalf("registry.ForestNote().SchemaHash() = %s, want v4 %s", got, schemaHashV4)
	}
	if lib, ub := reg.Canonical(), canonicalSchema(); lib != ub {
		t.Fatalf("canonical schema mismatch:\n lib = %q\n  ub = %q", lib, ub)
	}
}

// The library registry's KnownCols must match UB's hand-coded knownCols exactly — this is the map
// that drives normalize/validation, so a divergence would silently change which columns relay.
func TestRhizomeKnownColsMatchUB(t *testing.T) {
	lib := registry.ForestNote().KnownCols()
	if len(lib) != len(knownCols) {
		t.Fatalf("table count mismatch: lib=%d ub=%d", len(lib), len(knownCols))
	}
	for table, ubCols := range knownCols {
		libCols, ok := lib[table]
		if !ok {
			t.Fatalf("library registry missing table %q", table)
		}
		if len(libCols) != len(ubCols) {
			t.Fatalf("table %q col count: lib=%v ub=%v", table, libCols, ubCols)
		}
		for i := range ubCols {
			if libCols[i] != ubCols[i] {
				t.Fatalf("table %q col[%d]: lib=%q ub=%q", table, i, libCols[i], ubCols[i])
			}
		}
	}
}
