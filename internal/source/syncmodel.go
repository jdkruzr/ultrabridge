package source

import (
	"fmt"
	"strconv"
)

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

// UnmarshalJSON decodes a Direction from its wire token, keeping the type
// symmetric on the wire (clients that decode a SyncModel round-trip cleanly).
func (d *Direction) UnmarshalJSON(b []byte) error {
	s, err := strconv.Unquote(string(b))
	if err != nil {
		return fmt.Errorf("direction: expected string token: %w", err)
	}
	switch s {
	case "two_way":
		*d = TwoWay
	case "one_way_in":
		*d = OneWayIn
	default:
		return fmt.Errorf("direction: unknown token %q", s)
	}
	return nil
}

// SyncModel is the typed, derived classification of how a source syncs. It is
// keyed on source type (never a device instance) and is a constant function of
// that type — not stored. It is the single source of truth surfaced by the
// /api/sources JSON and the sync-model banners on the Files tabs and Settings.
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
