package caldav

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	ical "github.com/emersion/go-ical"
	"github.com/sysop/ultrabridge/internal/taskattach"
	"github.com/sysop/ultrabridge/internal/taskstore"
)

// buildInlineAttachBlob builds a VCALENDAR blob whose VTODO carries one inline
// BASE64 binary ATTACH (ENCODING=BASE64;VALUE=BINARY) with the given fmttype/
// filename, the shape a client like Thunderbird emits.
func buildInlineAttachBlob(t *testing.T, data []byte, fmttype, filename string) string {
	t.Helper()
	cal := ical.NewCalendar()
	cal.Props.SetText("VERSION", "2.0")
	cal.Props.SetText("PRODID", "test")
	todo := ical.NewComponent("VTODO")
	todo.Props.SetText("UID", "t1")
	todo.Props.SetDateTime("DTSTAMP", time.Unix(0, 0).UTC())
	todo.Props.SetText("SUMMARY", "has attach")
	att := ical.NewProp("ATTACH")
	att.SetBinary(data)
	if fmttype != "" {
		att.Params.Set("FMTTYPE", fmttype)
	}
	if filename != "" {
		att.Params.Set("FILENAME", filename)
	}
	todo.Props.Set(att)
	cal.Children = append(cal.Children, todo)
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.String()
}

func attachDecodedBytes(t *testing.T, blob string) []byte {
	t.Helper()
	cal, err := ical.NewDecoder(strings.NewReader(blob)).Decode()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	todo, err := FindVTODO(cal)
	if err != nil || todo == nil {
		t.Fatalf("no vtodo: %v", err)
	}
	p := todo.Props.Get("ATTACH")
	if p == nil {
		t.Fatal("no ATTACH prop")
	}
	b, err := p.Binary()
	if err != nil {
		t.Fatalf("ATTACH not inline binary: %v", err)
	}
	return b
}

func newAttachDeps(t *testing.T) AttachDeps {
	t.Helper()
	return AttachDeps{
		Store:   &taskattach.BlobStore{Root: t.TempDir()},
		Signer:  &taskattach.Signer{Secret: "test-secret"},
		BaseURL: "https://ub.example.com",
	}
}

// buildInlineAttachCal is buildInlineAttachBlob's *ical.Calendar sibling, for
// driving the backend PUT path directly.
func buildInlineAttachCal(data []byte, fmttype, filename string) *ical.Calendar {
	cal := ical.NewCalendar()
	cal.Props.SetText("VERSION", "2.0")
	cal.Props.SetText("PRODID", "test")
	todo := ical.NewComponent("VTODO")
	todo.Props.SetText("UID", "t1")
	todo.Props.SetDateTime("DTSTAMP", time.Unix(0, 0).UTC())
	todo.Props.SetText("SUMMARY", "has attach")
	att := ical.NewProp("ATTACH")
	att.SetBinary(data)
	if fmttype != "" {
		att.Params.Set("FMTTYPE", fmttype)
	}
	if filename != "" {
		att.Params.Set("FILENAME", filename)
	}
	todo.Props.Set(att)
	cal.Children = append(cal.Children, todo)
	return cal
}

// TestBackend_PutDebloatsGetReconstructs exercises the actual wiring: a PUT
// with inline-binary ATTACH lands a de-bloated blob in the store, and the
// outbound GET path reconstructs byte-equivalent inline binary.
func TestBackend_PutDebloatsGetReconstructs(t *testing.T) {
	store := newMockTaskStore()
	deps := newAttachDeps(t)
	backend := NewBackend(store, "/caldav", "Test", "preserve", nil)
	backend.SetTaskAttach(deps.Store, deps.Signer, deps.BaseURL)

	data := bytes.Repeat([]byte("BINARY\x00"), 1500)
	cal := buildInlineAttachCal(data, "image/png", "p.png")
	ctx := context.Background()
	if _, err := backend.PutCalendarObject(ctx, "/caldav/user/calendars/tasks/t1.ics", cal, nil); err != nil {
		t.Fatalf("PUT: %v", err)
	}

	var stored *taskstore.Task
	for _, tk := range store.tasks {
		stored = tk
	}
	if stored == nil || !stored.ICalBlob.Valid {
		t.Fatal("no stored blob")
	}
	if strings.Contains(stored.ICalBlob.String, "ENCODING=BASE64") {
		t.Errorf("stored blob still carries inline base64:\n%s", stored.ICalBlob.String)
	}
	if !strings.Contains(stored.ICalBlob.String, "X-UB-INLINE") {
		t.Errorf("stored blob missing de-bloat marker")
	}

	obj := backend.taskToCalendarObject(stored)
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(obj.Data); err != nil {
		t.Fatal(err)
	}
	if got := attachDecodedBytes(t, buf.String()); !bytes.Equal(got, data) {
		t.Errorf("GET did not reconstruct the original %d bytes (got %d)", len(data), len(got))
	}
}

func TestDebloatReconstruct_RoundTrip(t *testing.T) {
	data := bytes.Repeat([]byte("PNG\x00DATA"), 2000) // ~16 KB of "binary"
	blob := buildInlineAttachBlob(t, data, "image/png", "pic.png")
	deps := newAttachDeps(t)

	// Sanity: pre-de-bloat the metadata reports inline + correct decoded size.
	if pre := ParseBlobMetadata(blob); len(pre.Attachments) != 1 ||
		!pre.Attachments[0].Inline || pre.Attachments[0].Size != int64(len(data)) {
		t.Fatalf("pre-debloat metadata wrong: %+v", pre.Attachments)
	}

	debloated := DebloatInlineAttachments(blob, deps)
	if debloated == blob {
		t.Fatal("de-bloat did not change the blob")
	}
	if len(debloated) >= len(blob) {
		t.Errorf("de-bloated blob (%d) should be much smaller than original (%d)", len(debloated), len(blob))
	}
	for _, marker := range []string{"X-UB-INLINE", "X-UB-SHA256", "https://ub.example.com/api/v1/attachments/"} {
		if !strings.Contains(debloated, marker) {
			t.Errorf("de-bloated blob missing %q:\n%s", marker, debloated)
		}
	}
	// The base64 payload must be gone (no ENCODING=BASE64 left on the prop).
	if strings.Contains(debloated, "ENCODING=BASE64") {
		t.Errorf("de-bloated blob still carries inline base64 encoding:\n%s", debloated)
	}

	// Exposed metadata on the de-bloated blob: inline, fetchable URL, real size.
	meta := ParseBlobMetadata(debloated)
	if len(meta.Attachments) != 1 {
		t.Fatalf("want 1 attachment, got %d", len(meta.Attachments))
	}
	a := meta.Attachments[0]
	if !a.Inline || a.Size != int64(len(data)) || a.FmtType != "image/png" || a.Filename != "pic.png" {
		t.Errorf("de-bloated metadata wrong: %+v", a)
	}
	if !strings.HasPrefix(a.URI, "https://ub.example.com/api/v1/attachments/") {
		t.Errorf("expected a signed fetch URL, got %q", a.URI)
	}

	// Reconstruct → decode-equivalent inline binary.
	cal, err := ical.NewDecoder(strings.NewReader(debloated)).Decode()
	if err != nil {
		t.Fatal(err)
	}
	ReconstructInlineAttachments(cal, deps.Store)
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		t.Fatal(err)
	}
	got := attachDecodedBytes(t, buf.String())
	if !bytes.Equal(got, data) {
		t.Errorf("reconstructed bytes != original (%d vs %d)", len(got), len(data))
	}
	// Markers gone; FMTTYPE preserved.
	rp := func() *ical.Prop {
		todo, _ := FindVTODO(cal)
		return todo.Props.Get("ATTACH")
	}()
	if rp.Params.Get("X-UB-INLINE") != "" {
		t.Errorf("reconstruct left X-UB-INLINE marker")
	}
	if rp.Params.Get("FMTTYPE") != "image/png" {
		t.Errorf("reconstruct dropped FMTTYPE: %q", rp.Params.Get("FMTTYPE"))
	}
}

func TestDebloat_Idempotent(t *testing.T) {
	blob := buildInlineAttachBlob(t, []byte("some bytes here"), "text/plain", "n.txt")
	deps := newAttachDeps(t)
	once := DebloatInlineAttachments(blob, deps)
	twice := DebloatInlineAttachments(once, deps)
	if once != twice {
		t.Errorf("de-bloat not idempotent:\nonce:\n%s\ntwice:\n%s", once, twice)
	}
}

func TestDebloat_URIAttachUntouched(t *testing.T) {
	blob := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:test\r\n" +
		"BEGIN:VTODO\r\nUID:t1\r\nDTSTAMP:20260101T000000Z\r\n" +
		"ATTACH:https://example.com/file.pdf\r\n" +
		"END:VTODO\r\nEND:VCALENDAR\r\n"
	if got := DebloatInlineAttachments(blob, newAttachDeps(t)); got != blob {
		t.Errorf("URI ATTACH should be untouched:\n%s", got)
	}
}

func TestDebloat_NoDepsNoOp(t *testing.T) {
	blob := buildInlineAttachBlob(t, []byte("xyz"), "", "")
	if got := DebloatInlineAttachments(blob, AttachDeps{}); got != blob {
		t.Error("zero deps should be a no-op")
	}
}

func TestReconstruct_MissingFileLeavesURI(t *testing.T) {
	blob := buildInlineAttachBlob(t, []byte("data"), "text/plain", "n.txt")
	deps := newAttachDeps(t)
	debloated := DebloatInlineAttachments(blob, deps)
	// Reconstruct against a DIFFERENT (empty) store — the content isn't there.
	empty := &taskattach.BlobStore{Root: t.TempDir()}
	cal, err := ical.NewDecoder(strings.NewReader(debloated)).Decode()
	if err != nil {
		t.Fatal(err)
	}
	ReconstructInlineAttachments(cal, empty) // must not panic
	todo, _ := FindVTODO(cal)
	p := todo.Props.Get("ATTACH")
	if p.Params.Get("X-UB-INLINE") != "1" {
		t.Errorf("missing content should leave the URI form (marker intact), got params %v", p.Params)
	}
}

type attachSpec struct {
	uri      string // non-empty → URI attachment; else inline binary
	data     []byte
	fmttype  string
	filename string
}

func buildMultiAttachBlob(t *testing.T, specs []attachSpec) string {
	t.Helper()
	cal := ical.NewCalendar()
	cal.Props.SetText("VERSION", "2.0")
	cal.Props.SetText("PRODID", "test")
	todo := ical.NewComponent("VTODO")
	todo.Props.SetText("UID", "t1")
	todo.Props.SetDateTime("DTSTAMP", time.Unix(0, 0).UTC())
	for _, s := range specs {
		att := ical.NewProp("ATTACH")
		if s.uri != "" {
			att.Value = s.uri
		} else {
			att.SetBinary(s.data)
		}
		if s.fmttype != "" {
			att.Params.Set("FMTTYPE", s.fmttype)
		}
		if s.filename != "" {
			att.Params.Set("FILENAME", s.filename)
		}
		todo.Props[ical.PropAttach] = append(todo.Props[ical.PropAttach], *att)
	}
	cal.Children = append(cal.Children, todo)
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.String()
}

func attachProps(t *testing.T, blob string) []ical.Prop {
	t.Helper()
	cal, err := ical.NewDecoder(strings.NewReader(blob)).Decode()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	todo, err := FindVTODO(cal)
	if err != nil || todo == nil {
		t.Fatalf("no vtodo: %v", err)
	}
	return todo.Props[ical.PropAttach]
}

func reconstructToBlob(t *testing.T, blob string, store *taskattach.BlobStore) string {
	t.Helper()
	cal, err := ical.NewDecoder(strings.NewReader(blob)).Decode()
	if err != nil {
		t.Fatal(err)
	}
	ReconstructInlineAttachments(cal, store)
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// TestDebloat_MixedAndMultiInlineOrder covers the plan's "multi-ATTACH order
// preserved" requirement that the earlier tests skipped: a URI attachment
// adjacent to inline binary must survive de-bloat in order and intact, and
// several inline attachments must each round-trip to their own bytes.
func TestDebloat_MixedAndMultiInlineOrder(t *testing.T) {
	deps := newAttachDeps(t)
	b1 := bytes.Repeat([]byte("AAA\x01"), 500)
	b2 := bytes.Repeat([]byte("BBB\x02"), 700)

	t.Run("mixed URI/inline/URI order preserved + URIs untouched", func(t *testing.T) {
		blob := buildMultiAttachBlob(t, []attachSpec{
			{uri: "https://example.com/first"},
			{data: b1, fmttype: "image/png", filename: "mid.png"},
			{uri: "https://example.com/second"},
		})
		out := DebloatInlineAttachments(blob, deps)
		meta := ParseBlobMetadata(out)
		if len(meta.Attachments) != 3 {
			t.Fatalf("want 3 attachments, got %d", len(meta.Attachments))
		}
		if meta.Attachments[0].URI != "https://example.com/first" || meta.Attachments[0].Inline {
			t.Errorf("first should be the untouched URI: %+v", meta.Attachments[0])
		}
		if !meta.Attachments[1].Inline || meta.Attachments[1].Size != int64(len(b1)) {
			t.Errorf("middle should be de-bloated inline: %+v", meta.Attachments[1])
		}
		if meta.Attachments[2].URI != "https://example.com/second" || meta.Attachments[2].Inline {
			t.Errorf("third should be the untouched URI: %+v", meta.Attachments[2])
		}
		props := attachProps(t, reconstructToBlob(t, out, deps.Store))
		if len(props) != 3 {
			t.Fatalf("want 3 after reconstruct, got %d", len(props))
		}
		if props[0].Value != "https://example.com/first" || props[2].Value != "https://example.com/second" {
			t.Errorf("URIs not intact after reconstruct: %q / %q", props[0].Value, props[2].Value)
		}
		got, err := props[1].Binary()
		if err != nil || !bytes.Equal(got, b1) {
			t.Errorf("middle bytes not reconstructed (err=%v)", err)
		}
	})

	t.Run("multiple inline attachments each reconstruct to own bytes", func(t *testing.T) {
		blob := buildMultiAttachBlob(t, []attachSpec{
			{data: b1, fmttype: "application/octet-stream"},
			{data: b2, fmttype: "application/pdf"},
		})
		out := DebloatInlineAttachments(blob, deps)
		props := attachProps(t, reconstructToBlob(t, out, deps.Store))
		if len(props) != 2 {
			t.Fatalf("want 2, got %d", len(props))
		}
		g0, e0 := props[0].Binary()
		g1, e1 := props[1].Binary()
		if e0 != nil || e1 != nil || !bytes.Equal(g0, b1) || !bytes.Equal(g1, b2) {
			t.Errorf("multi-inline reconstruct wrong: e0=%v e1=%v eq0=%v eq1=%v",
				e0, e1, bytes.Equal(g0, b1), bytes.Equal(g1, b2))
		}
	})
}

// TestDebloat_ETagInvariant pins the safety property the whole design rests on:
// de-bloating the stored blob does NOT change the task's CalDAV ETag, so it can
// never trigger a spurious re-sync. ComputeETag deliberately ignores ICalBlob.
func TestDebloat_ETagInvariant(t *testing.T) {
	blob := buildInlineAttachBlob(t, bytes.Repeat([]byte("x"), 4096), "application/pdf", "doc.pdf")
	task := &taskstore.Task{
		TaskID:       "t1",
		Title:        sql.NullString{String: "T", Valid: true},
		Status:       sql.NullString{String: "needsAction", Valid: true},
		LastModified: sql.NullInt64{Int64: 1700000000000, Valid: true},
		ICalBlob:     sql.NullString{String: blob, Valid: true},
	}
	before := taskstore.ComputeETag(task)
	task.ICalBlob.String = DebloatInlineAttachments(blob, newAttachDeps(t))
	after := taskstore.ComputeETag(task)
	if before != after {
		t.Errorf("de-bloat changed the ETag (%q → %q) — would cause a re-sync loop", before, after)
	}
}
