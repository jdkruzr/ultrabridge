package readercontract

import (
	"encoding/json"
	"fmt"
	"sort"
	"unicode/utf16"
	"unicode/utf8"
)

// AnnotationRows is a detached, bounded snapshot of generic LWW winners, one row
// per identity. The host must supply a consistent snapshot, including provenance.
// BookPresent means metadata presence, not verified original-byte readiness.
type AnnotationRows struct {
	Annotation                                  Record
	BookPresent, BookDeleted, AnnotationDeleted bool
	Sessions, Strokes, Claims, Values           []Record
}

type ProjectionStatus string

const (
	Ready       ProjectionStatus = "READY"
	Deleted     ProjectionStatus = "DELETED"
	Cancelled   ProjectionStatus = "CANCELLED"
	Pending     ProjectionStatus = "PENDING"
	Unsupported ProjectionStatus = "UNSUPPORTED"
	Invalid     ProjectionStatus = "INVALID"
)

type ProjectionIssue struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}
type AnnotationProjection struct {
	ID               string            `json:"id"`
	Status           ProjectionStatus  `json:"status"`
	Visible          bool              `json:"visible"`
	CanvasWidth      int64             `json:"canvasWidth"`
	RequestedHeight  *int64            `json:"requestedHeight"`
	EffectiveHeight  *int64            `json:"effectiveHeight"`
	Anchor           *string           `json:"anchor"`
	HighlightPresent *bool             `json:"highlightPresent"`
	Strokes          []Record          `json:"strokes"`
	InputHash        *string           `json:"inputHash"`
	Issues           []ProjectionIssue `json:"issues"`
}

// compareUTF16 matches Kotlin String.compareTo for opaque Unicode IDs. Go UTF-8
// byte order differs for supplementary characters versus BMP characters >= E000.
// Iterate code units without allocating strings/slices inside sort comparisons.
func compareUTF16(a, b string) int {
	var pendingA, pendingB rune
	next := func(s *string, pending *rune) rune {
		if *pending != 0 {
			c := *pending
			*pending = 0
			return c
		}
		if len(*s) == 0 {
			return -1
		}
		c, size := utf8.DecodeRuneInString(*s)
		*s = (*s)[size:]
		if c > 0xffff {
			high, low := utf16.EncodeRune(c)
			*pending = low
			return high
		}
		return c
	}
	for {
		x, y := next(&a, &pendingA), next(&b, &pendingB)
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
		if x == -1 {
			return 0
		}
	}
}

// Reduce derives effective state AFTER generic LWW and guarded ingress. It does
// not authenticate authors, adjudicate incoming history, mutate rows, or author
// replacement operations. No DB, clock, network or render work belongs here.
// Returned stroke records share immutable snapshot data; callers must not mutate it.
func Reduce(rows AnnotationRows) AnnotationProjection {
	a := rows.Annotation
	p := AnnotationProjection{ID: a.ID, Strokes: []Record{}, Issues: []ProjectionIssue{}}
	p.CanvasWidth, _ = a.Columns["canvas_width"].(int64)
	malformed := func() AnnotationProjection {
		p.Status = Invalid
		if rows.BookDeleted || rows.AnnotationDeleted {
			p.Status = Deleted
		}
		p.Issues = []ProjectionIssue{{a.ID, "Malformed stored annotation"}}
		return p
	}
	if err := validateShape("reader_annotation", a, true); err != nil {
		return malformed()
	}
	for _, group := range []struct {
		table   string
		records []Record
	}{
		{"reader_edit_session", rows.Sessions}, {"reader_stroke", rows.Strokes},
		{"reader_erase_claim", rows.Claims}, {"reader_annotation_value", rows.Values},
	} {
		for _, r := range group.records {
			if err := validateShape(group.table, r, false); err != nil {
				return malformed()
			}
		}
	}
	pending, unsupported, invalid := !rows.BookPresent, false, false
	issue := func(id, reason string) { p.Issues = append(p.Issues, ProjectionIssue{id, reason}) }
	wait := func(id, reason string) { pending = true; issue(id, reason) }
	sessions := make(map[string]Record, len(rows.Sessions))
	active := map[string]bool{}
	for _, session := range rows.Sessions {
		sessions[session.ID] = session
	}
	for _, session := range rows.Sessions {
		if session.text("annotation_id") != a.ID {
			continue
		}
		owner := session.Columns["owner_site"]
		valid := session.text("kind") == "interactive" && owner != nil && (session.Version == nil || session.Version.SiteID == owner) || session.text("kind") == "import" && owner == nil && session.text("state") == "finished"
		if !valid {
			invalid = true
			issue(session.ID, "Invalid session ownership/state")
			continue
		}
		if session.text("state") != "cancelled" {
			active[session.ID] = true
		}
	}
	contributes := func(record Record) bool {
		id := record.text("session_id")
		session, ok := sessions[id]
		if !ok {
			wait(record.ID, "Missing session "+id)
			return false
		}
		if session.text("annotation_id") != a.ID {
			invalid = true
			issue(record.ID, "Cross-annotation contribution")
			return false
		}
		if session.text("kind") == "interactive" && record.Version != nil && record.Version.SiteID != session.Columns["owner_site"] {
			invalid = true
			issue(record.ID, "Contribution author differs from session owner")
			return false
		}
		return active[id]
	}
	filter := func(records []Record) []Record {
		result := make([]Record, 0, len(records))
		for _, r := range records {
			if contributes(r) {
				result = append(result, r)
			}
		}
		return result
	}
	creator := a.text("creator_session_id")
	if _, ok := sessions[creator]; !ok {
		wait(creator, "Missing creator session")
	}
	candidateInk, values, claims := filter(rows.Strokes), filter(rows.Values), filter(rows.Claims)
	knownInk := make(map[string]bool, len(rows.Strokes))
	for _, stroke := range rows.Strokes {
		knownInk[stroke.ID] = true
	}
	erased := map[string]bool{}
	for _, claim := range claims {
		if !knownInk[claim.text("stroke_id")] {
			wait(claim.ID, "Missing erased stroke")
		}
		if claim.number("active") == 1 {
			erased[claim.text("stroke_id")] = true
		}
	}
	for _, stroke := range candidateInk {
		if !erased[stroke.ID] {
			p.Strokes = append(p.Strokes, stroke)
		}
	}
	sort.SliceStable(p.Strokes, func(i, j int) bool {
		a, b := p.Strokes[i], p.Strokes[j]
		if a.number("paint_order") != b.number("paint_order") {
			return a.number("paint_order") < b.number("paint_order")
		}
		if c := compareUTF16(a.text("paint_site"), b.text("paint_site")); c != 0 {
			return c < 0
		}
		return compareUTF16(a.ID, b.ID) < 0
	})
	exists := active[creator]
	for _, group := range [][]Record{candidateInk, values, claims} {
		for _, r := range group {
			if r.text("session_id") != creator {
				exists = true
			}
		}
	}
	// Select entire raw properties by real row versions. A missing version is not
	// a synthetic zero and must not resolve competing contributions by arrival order.
	property := func(name, fallback string) (map[string]json.RawMessage, *string) {
		var selected *Record
		count := 0
		unversioned := false
		for i := range values {
			r := &values[i]
			if r.text("property") != name {
				continue
			}
			count++
			unversioned = unversioned || r.Version == nil
			if selected == nil || r.Version != nil && selected.Version != nil && compareVersion(r.Version, selected.Version) > 0 {
				selected = r
			}
		}
		if count > 1 && unversioned {
			wait(a.ID, "Unversioned competing "+name+" contributions; offline ordering required")
			return nil, nil
		}
		raw, id := fallback, a.ID
		if selected != nil {
			raw, id = selected.text("value_json"), selected.ID
		}
		parsed, version, _ := versioned(raw) // shape validation above established a valid object
		if version != 1 {
			unsupported = true
			issue(id, "Unsupported "+name+" version")
			return nil, nil
		}
		return parsed, &raw
	}
	_, p.Anchor = property("anchor", a.text("initial_anchor_json"))
	if value, _ := property("height", fmt.Sprintf(`{"version":1,"height":%d}`, a.number("initial_height"))); value != nil {
		height, _ := jsonInteger(value["height"])
		p.RequestedHeight = &height
	}
	if value, _ := property("highlight_present", `{"version":1,"present":true}`); value != nil {
		var present bool
		_ = json.Unmarshal(value["present"], &present)
		p.HighlightPresent = &present
	}
	var bottom int64
	for _, stroke := range p.Strokes {
		edge, err := stroke.ink().Bottom()
		if err != nil {
			unsupported = true
			issue(stroke.ID, "Unsupported ink")
		} else if edge > bottom {
			bottom = edge
		}
	}
	if p.RequestedHeight != nil {
		height := max(*p.RequestedHeight, bottom)
		p.EffectiveHeight = &height
	}
	deleted := rows.BookDeleted || rows.AnnotationDeleted
	switch {
	case deleted:
		p.Status = Deleted
	case invalid:
		p.Status = Invalid
	case unsupported:
		p.Status = Unsupported
	case pending:
		p.Status = Pending
	case !exists:
		p.Status = Cancelled
	default:
		p.Status = Ready
	}
	p.Visible = !deleted && exists && !invalid && !unsupported
	if p.Status == Ready && p.EffectiveHeight != nil {
		ink := make([]Ink, 0, len(p.Strokes))
		for _, s := range p.Strokes {
			ink = append(ink, s.ink())
		}
		hash, _ := Fingerprint(p.CanvasWidth, *p.EffectiveHeight, ink)
		p.InputHash = &hash
	}
	sort.SliceStable(p.Issues, func(i, j int) bool {
		a, b := p.Issues[i], p.Issues[j]
		if c := compareUTF16(a.ID, b.ID); c != 0 {
			return c < 0
		}
		return compareUTF16(a.Reason, b.Reason) < 0
	})
	return p
}
