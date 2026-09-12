package readercontract

import (
	"fmt"
	"reflect"
)

// RequireClientAuthor requires identity verified by the host's credential layer.
// A request's self-declared site_id is NOT verification. Shared account Basic auth
// does not supply this binding; production activation must establish it separately.
func RequireClientAuthor(op WireOp, verifiedSite string) error {
	if !sitePattern.MatchString(verifiedSite) || op.SiteID != verifiedSite {
		return fmt.Errorf("operation author differs from verified client")
	}
	return nil
}

type Decision struct{ State, Reason string }
type Lookup func(table, id string) *Record

func compareVersion(a, b *Version) int {
	if a.OpTS < b.OpTS {
		return -1
	}
	if a.OpTS > b.OpTS {
		return 1
	}
	if a.OpSeq < b.OpSeq {
		return -1
	}
	if a.OpSeq > b.OpSeq {
		return 1
	}
	if a.SiteID < b.SiteID {
		return -1
	}
	if a.SiteID > b.SiteID {
		return 1
	}
	return 0
}

// Check is a pure domain gate, not a database reducer or authentication layer.
// The caller supplies previously validated rows and actual stored provenance;
// missing provenance remains nil. Applied means eligible for generic LWW, not
// necessarily its winner. Pending must be retained and retried after dependencies.
func Check(table string, r Record, lookup Lookup) Decision {
	if err := Validate(table, r); err != nil {
		return Decision{"quarantined", err.Error()}
	}
	return CheckPrepared(table, r, lookup)
}

// CheckPrepared is the transaction-side gate for rows already fully validated
// off-writer with DecodeJSON/Validate. The host must keep that prepared data private
// and immutable. Lookup returns typed, validated stored rows with actual provenance.
func CheckPrepared(table string, r Record, lookup Lookup) Decision {
	invalid := func(s string) Decision { return Decision{"quarantined", s} }
	pending := func(s string) Decision { return Decision{"pending", s} }
	if r.Version == nil || !sitePattern.MatchString(r.Version.SiteID) || r.Version.OpTS < 0 || r.Version.OpSeq <= 0 {
		return invalid("Missing/invalid incoming provenance")
	}
	old := lookup(table, r.ID)
	var immutable []string
	switch table {
	case "reader_book", "reader_annotation", "reader_stroke", "content_reference":
		for key := range r.Columns {
			immutable = append(immutable, key)
		}
	case "reader_edit_session":
		immutable = []string{"annotation_id", "owner_site", "kind"}
	case "reader_erase_claim":
		immutable = []string{"session_id", "stroke_id"}
	case "reader_annotation_value":
		immutable = []string{"session_id", "property"}
	case "reader_position":
		immutable = []string{"book_id", "site_id"}
	case "reader_recognition":
		immutable = []string{"annotation_id", "producer_id"}
	}
	if old != nil {
		for _, k := range immutable {
			if !reflect.DeepEqual(old.Columns[k], r.Columns[k]) {
				return invalid("Immutable identity conflict")
			}
		}
	}
	author := r.Version.SiteID
	if table == "reader_position" && r.text("site_id") != author {
		return invalid("Position owner mismatch")
	}
	if table == "reader_recognition" && r.text("producer_id") != "client:"+author {
		return invalid("Producer is not this client; authenticated server-producer binding is not activated")
	}
	if table == "reader_edit_session" {
		if r.text("kind") == "interactive" && r.Columns["owner_site"] != author {
			return invalid("Session owner mismatch")
		}
		annotation := lookup("reader_annotation", r.text("annotation_id"))
		if annotation == nil {
			return pending("Missing annotation")
		}
		if annotation.text("creator_session_id") == r.ID && r.text("kind") == "interactive" && annotation.Version != nil && annotation.Version.SiteID != author {
			return invalid("Creator session owner mismatch")
		}
		if r.text("kind") == "import" {
			expected, _ := CompositeID("lab-import-v1", annotation.text("book_id"), annotation.ID)
			if r.ID != expected {
				return invalid("Invalid baseline import session identity")
			}
		}
		if old != nil && old.text("state") != "open" && old.text("state") != r.text("state") {
			if r.text("state") != "open" {
				return invalid("Conflicting terminal states")
			}
			if old.Version == nil {
				return pending("Unversioned terminal session")
			}
			if compareVersion(r.Version, old.Version) >= 0 {
				return invalid("Terminal session cannot reopen")
			}
		}
	}
	if table == "reader_stroke" || table == "reader_erase_claim" || table == "reader_annotation_value" {
		session := lookup("reader_edit_session", r.text("session_id"))
		if session == nil {
			return pending("Missing session")
		}
		if session.text("kind") == "interactive" && session.Columns["owner_site"] != author {
			return invalid("Contribution owner mismatch")
		}
		if session.text("kind") == "import" && table != "reader_stroke" {
			return invalid("Import baseline is immutable")
		}
		if table == "reader_stroke" {
			if r.text("annotation_id") != session.text("annotation_id") {
				return invalid("Cross-annotation stroke")
			}
			paintSite := author
			if session.text("kind") == "import" {
				paintSite = "import"
			}
			if r.text("paint_site") != paintSite {
				return invalid("Paint owner mismatch")
			}
		}
		if table == "reader_erase_claim" {
			stroke := lookup("reader_stroke", r.text("stroke_id"))
			if stroke == nil {
				return pending("Missing erased stroke")
			}
			if stroke.text("annotation_id") != session.text("annotation_id") {
				return invalid("Cross-annotation erase")
			}
		}
	}
	if old != nil && old.Version == nil {
		for k, v := range r.Columns {
			if !reflect.DeepEqual(v, old.Columns[k]) {
				return pending("Unversioned local row; join ordering required")
			}
		}
	}
	return Decision{State: "applied"}
}
