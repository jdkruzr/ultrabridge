# Source Sync-Semantics Surfacing + Settings IA — Implementation Plan

**Goal:** Make each backend's sync semantics a typed, testable domain concept (`SyncModel`) in `internal/source`.

**Architecture:** A pure value type + exhaustive lookup keyed on source *type*. No DB, no I/O — a constant function. The descriptor is the single source of truth later consumed by the `/api/sources` JSON and the HTML banners (Phases 2, 3, 6).

**Tech Stack:** Go (stdlib only — `encoding/json`, `strconv`), `modernc.org/sqlite` elsewhere but not here. Table-driven tests per the existing `internal/source/source_test.go` idiom.

**Scope:** Phase 1 of 6.

**Codebase verified:** 2026-06-12. `internal/source/source.go` defines `Source` + `SourceRow` (type is `Type string`, one of `supernote|boox|forestnote`). `registry.go` holds `ErrUnknownType`. No existing `Direction`/`SyncModel`/`MarshalJSON` in the package. Tests use plain `testing` table style, error via `t.Errorf`.

---

## Acceptance Criteria Coverage

This phase implements and tests:

### sync-model-and-settings-ia.AC1: Sync model is a typed, exhaustive descriptor
- **sync-model-and-settings-ia.AC1.1 Success:** `SyncModelFor("supernote"|"boox"|"forestnote")` returns the correct `{Label, Direction, Authority, DeletesPropagate, Blurb}` for each, pinned by test.
- **sync-model-and-settings-ia.AC1.2 Success:** `Direction` marshals to stable wire strings (`"one_way_in"` / `"two_way"`).
- **sync-model-and-settings-ia.AC1.3 Edge:** An unknown source type returns the explicit `Unmanaged` descriptor, never a zero/blank value.
- **sync-model-and-settings-ia.AC1.4 Success:** Boox is the only descriptor with `DeletesPropagate == false`.

---

<!-- START_SUBCOMPONENT_A (tasks 1-2) -->
<!-- START_TASK_1 -->
### Task 1: SyncModel descriptor + Direction enum

**Files:**
- Create: `internal/source/syncmodel.go`

**Implementation:**

This file is the *contract* consumed by Phases 2/3/6, so specify it fully. `Direction` is an `int` enum with a stable wire encoding via `MarshalJSON`; `String()` backs both the JSON encoding and the template glyph/tone mapping added in Phase 3 (so templates compare strings, never raw ints).

```go
package source

import "strconv"

// Direction describes which way notes flow for a source's sync model.
type Direction int

const (
	// OneWayIn — the device exports to UltraBridge; UB never writes back.
	OneWayIn Direction = iota
	// TwoWay — notes sync in both directions.
	TwoWay
)

// String returns the stable wire token for d. It backs MarshalJSON and the
// template glyph/tone mapping (templates compare these strings, not raw ints).
func (d Direction) String() string {
	switch d {
	case TwoWay:
		return "two_way"
	case OneWayIn:
		return "one_way_in"
	default:
		return "unknown"
	}
}

// MarshalJSON encodes the Direction as its wire token.
func (d Direction) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(d.String())), nil
}

// SyncModel is the typed, derived classification of how a source syncs. It is
// keyed on source type (never a device instance) and is a constant function of
// that type — not stored. It is the single source of truth surfaced by the
// /api/sources JSON (Phase 2) and the sync-model banners (Phases 3, 6).
type SyncModel struct {
	Label            string    `json:"label"`
	Direction        Direction `json:"direction"`
	Authority        string    `json:"authority"`
	DeletesPropagate bool      `json:"deletes_propagate"`
	Blurb            string    `json:"blurb"`
}

// SyncModelFor returns the SyncModel for a source type. It is exhaustive over
// the three known types; any other (or empty) type returns the explicit
// Unmanaged descriptor — never a zero value.
func SyncModelFor(sourceType string) SyncModel {
	switch sourceType {
	case "supernote":
		return SyncModel{
			Label:            "Two-way sync",
			Direction:        TwoWay,
			Authority:        "Shared (UB-hosted)",
			DeletesPropagate: true,
			Blurb:            "Files sync both ways with your Supernote. Deleting a note moves it to a recoverable recycle bin.",
		}
	case "boox":
		return SyncModel{
			Label:            "Receive-only",
			Direction:        OneWayIn,
			Authority:        "Device",
			DeletesPropagate: false,
			Blurb:            "Boox exports notes to UltraBridge one way. Deletes and renames on the device never reach UltraBridge — remove notes here manually.",
		}
	case "forestnote":
		return SyncModel{
			Label:            "Live mirror",
			Direction:        TwoWay,
			Authority:        "Shared (row-level LWW)",
			DeletesPropagate: true,
			Blurb:            "ForestNote mirrors your notes two ways in real time. Deletes are recoverable tombstones that converge across devices.",
		}
	default:
		return Unmanaged
	}
}

// Unmanaged is the explicit fallback for an unrecognized source type. It is a
// named value (not a zero literal) so callers and templates get a coherent,
// non-blank descriptor.
var Unmanaged = SyncModel{
	Label:            "Unmanaged",
	Direction:        OneWayIn,
	Authority:        "Unknown",
	DeletesPropagate: false,
	Blurb:            "This source type has no defined sync model.",
}
```

**Verification:**

Run: `go build -C /home/sysop/src/ultrabridge ./internal/source/`
Expected: builds without errors.

**Commit:** `feat(source): add SyncModel descriptor keyed on source type`
<!-- END_TASK_1 -->

<!-- START_TASK_2 -->
### Task 2: SyncModel table test

**Verifies:** sync-model-and-settings-ia.AC1.1, AC1.2, AC1.3, AC1.4

**Files:**
- Create: `internal/source/syncmodel_test.go` (unit)

**Testing:**

Follow the table-driven style in `source_test.go` (plain `testing`, `t.Errorf`, no external deps). Tests must verify each AC:

- **AC1.1:** A table over `{"supernote","boox","forestnote"}` asserting the full expected `SyncModel` for each via `reflect.DeepEqual` (or field-by-field) — Label, Direction, Authority, DeletesPropagate, and Blurb all pinned. This locks the user-facing wording.
- **AC1.2:** `json.Marshal(TwoWay)` yields `"two_way"` and `json.Marshal(OneWayIn)` yields `"one_way_in"`; assert by marshaling each Direction constant directly and comparing the raw bytes. Also assert that marshaling a `SyncModel` embeds the string token (decode into a `map[string]any` and check `m["direction"] == "two_way"` for supernote).
- **AC1.3:** `SyncModelFor("bogus")` and `SyncModelFor("")` both `DeepEqual` `Unmanaged`, and `Unmanaged.Label != ""` (proves it's not a zero value).
- **AC1.4:** Iterate the three known descriptors; assert exactly one (`boox`) has `DeletesPropagate == false`.

**Verification:**

Run: `go test -C /home/sysop/src/ultrabridge ./internal/source/`
Expected: all tests pass.

**Commit:** `test(source): pin SyncModel descriptors and Direction wire encoding`
<!-- END_TASK_2 -->
<!-- END_SUBCOMPONENT_A -->
