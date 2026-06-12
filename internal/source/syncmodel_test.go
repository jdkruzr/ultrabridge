package source

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestSyncModelFor pins the full descriptor for each known source type.
// sync-model-and-settings-ia.AC1.1: SyncModelFor returns the correct
// {Label, Direction, Authority, DeletesPropagate, Blurb} for each type.
func TestSyncModelFor(t *testing.T) {
	tests := []struct {
		sourceType string
		want       SyncModel
	}{
		{
			sourceType: "supernote",
			want: SyncModel{
				Label:            "Two-way sync",
				Direction:        TwoWay,
				Authority:        "Shared (UB-hosted)",
				DeletesPropagate: true,
				Blurb:            "Files sync both ways with your Supernote. Deleting a note moves it to a recoverable recycle bin.",
			},
		},
		{
			sourceType: "boox",
			want: SyncModel{
				Label:            "Receive-only",
				Direction:        OneWayIn,
				Authority:        "Device",
				DeletesPropagate: false,
				Blurb:            "Boox exports notes to UltraBridge one way. Deletes and renames on the device never reach UltraBridge — remove notes here manually.",
			},
		},
		{
			sourceType: "forestnote",
			want: SyncModel{
				Label:            "Live mirror",
				Direction:        TwoWay,
				Authority:        "Shared (row-level LWW)",
				DeletesPropagate: true,
				Blurb:            "ForestNote mirrors your notes two ways in real time. Deletes are recoverable tombstones that converge across devices.",
			},
		},
	}

	for _, tt := range tests {
		got := SyncModelFor(tt.sourceType)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("SyncModelFor(%q) = %+v, want %+v", tt.sourceType, got, tt.want)
		}
	}
}

// TestDirectionWireEncoding pins the stable JSON wire tokens for Direction.
// sync-model-and-settings-ia.AC1.2: Direction marshals to "one_way_in" / "two_way".
func TestDirectionWireEncoding(t *testing.T) {
	tests := []struct {
		d    Direction
		want string
	}{
		{TwoWay, `"two_way"`},
		{OneWayIn, `"one_way_in"`},
	}

	for _, tt := range tests {
		raw, err := json.Marshal(tt.d)
		if err != nil {
			t.Fatalf("json.Marshal(%v): %v", tt.d, err)
		}
		if string(raw) != tt.want {
			t.Errorf("json.Marshal(%v) = %s, want %s", tt.d, raw, tt.want)
		}
	}

	// Round-trip: the wire token decodes back to the same Direction, and an
	// unknown token errors rather than silently zeroing.
	for _, d := range []Direction{TwoWay, OneWayIn} {
		raw, _ := json.Marshal(d)
		var back Direction
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Errorf("round-trip %v: %v", d, err)
		} else if back != d {
			t.Errorf("round-trip %v = %v", d, back)
		}
	}
	var bad Direction
	if err := json.Unmarshal([]byte(`"sideways"`), &bad); err == nil {
		t.Error("unmarshal of unknown token succeeded, want error")
	}

	// A marshaled SyncModel embeds the string token, not a raw int.
	raw, err := json.Marshal(SyncModelFor("supernote"))
	if err != nil {
		t.Fatalf("json.Marshal(SyncModelFor(supernote)): %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if m["direction"] != "two_way" {
		t.Errorf("marshaled SyncModel direction = %v, want two_way", m["direction"])
	}
}

// TestSyncModelForUnknown verifies the explicit Unmanaged fallback.
// sync-model-and-settings-ia.AC1.3: unknown source types return Unmanaged,
// never a zero/blank value.
func TestSyncModelForUnknown(t *testing.T) {
	for _, sourceType := range []string{"bogus", ""} {
		got := SyncModelFor(sourceType)
		if !reflect.DeepEqual(got, Unmanaged) {
			t.Errorf("SyncModelFor(%q) = %+v, want Unmanaged %+v", sourceType, got, Unmanaged)
		}
	}
	if Unmanaged.Label == "" {
		t.Error("Unmanaged.Label is empty; fallback must not be a zero value")
	}
}

// TestDeletesPropagateExactlyOneFalse verifies Boox is the lone non-propagating
// descriptor. sync-model-and-settings-ia.AC1.4.
func TestDeletesPropagateExactlyOneFalse(t *testing.T) {
	var nonPropagating []string
	for _, sourceType := range []string{"supernote", "boox", "forestnote"} {
		if !SyncModelFor(sourceType).DeletesPropagate {
			nonPropagating = append(nonPropagating, sourceType)
		}
	}
	if len(nonPropagating) != 1 || nonPropagating[0] != "boox" {
		t.Errorf("DeletesPropagate == false for %v, want exactly [boox]", nonPropagating)
	}
}
