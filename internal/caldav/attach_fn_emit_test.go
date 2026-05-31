package caldav

import (
	"encoding/base64"
	"strings"
	"testing"

	ical "github.com/emersion/go-ical"
	"github.com/sysop/ultrabridge/internal/taskattach"
)

// fnSampleB64 is the EXACT VCALENDAR bytes ForestNote's VTodoBuilder emits for a
// lasso → To-do task with an opt-in recognized-text ATTACH. Captured verbatim from
// app/notes VTodoWireSampleTest; base64 of the whole .ics so the CRLFs and the
// hand-rolled 75-octet fold survive in Go source. The fold deliberately splits
// "VALUE=BINARY" across a continuation line — this guards that go-ical unfolds FN's
// hand-rolled emitter output (not a go-ical-canonical re-encoding) and that the
// FN↔UB inline-ATTACH contract holds end to end.
const fnSampleB64 = "QkVHSU46VkNBTEVOREFSDQpWRVJTSU9OOjIuMA0KUFJPRElEOi0vL0ZvcmVzdE5vdGUvL0VODQpCRUdJTjpWVE9ETw0KVUlEOmZuLXNhbXBsZS0xDQpEVFNUQU1QOjIwMjYwNTMxVDEyMDAwMFoNCkxBU1QtTU9ESUZJRUQ6MjAyNjA1MzFUMTIwMDAwWg0KU1VNTUFSWTpzaGlwIHYxLjANClNUQVRVUzpORUVEUy1BQ1RJT04NClVSTDpodHRwczovL3ViLmV4YW1wbGUub3JnL2ZpbGVzL2ZvcmVzdG5vdGU/bm90ZWJvb2s9TkIxJnBhZ2U9UEcxDQpYLUZPUkVTVE5PVEUtTk9URUJPT0stSUQ6TkIxDQpYLUZPUkVTVE5PVEUtUEFHRS1JRDpQRzENClgtRk9SRVNUTk9URS1OT1RFQk9PSy1OQU1FOldvcmsgSm91cm5hbA0KWC1GT1JFU1ROT1RFLVNPVVJDRTpsYXNzbw0KWC1GT1JFU1ROT1RFLU5BVElWRS1VUkw6Zm9yZXN0bm90ZTovL25vdGVib29rL05CMS9wYWdlL1BHMQ0KQVRUQUNIO0ZNVFRZUEU9dGV4dC9wbGFpbjtGSUxFTkFNRT1yZWNvZ25pemVkLXRleHQudHh0O0VOQ09ESU5HPUJBU0U2NDtWQUxVDQogRT1CSU5BUlk6VFdWbGRHbHVaeUJ1YjNSbGN6b2djMmhwY0NCMk1TNHdPeUJtYjJ4c2IzY2dkWEFnZHk4Z1JHRnVZU3dnVXNPNFkNCiBpQW1JSFJsWVcwZ1lXSnZkWFFnVVRNZzRvQ1VJR1JsWVdSc2FXNWxJR2x6SUc1bGVIUWdSbkpwWkdGNUxncEViMjRuZENCbWIzSg0KIG5aWFFnZEdobElHTmhac09wSUhKbFkyVnBjSFJ6SVE9PQ0KRU5EOlZUT0RPDQpFTkQ6VkNBTEVOREFSDQo="

// fnExpectedRecognized is the recognized-handwriting text FN base64-encoded into
// the ATTACH (UTF-8, with ';' ',' '—' 'ø' 'é' and a newline) — what UB must recover.
const fnExpectedRecognized = "Meeting notes: ship v1.0; follow up w/ Dana, Røb & team about Q3 — deadline is next Friday.\nDon't forget the café receipts!"

func fnSampleBlob(t *testing.T) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(fnSampleB64)
	if err != nil {
		t.Fatalf("decode sample: %v", err)
	}
	return string(b)
}

// ParseBlobMetadata must surface FN's inline ATTACH with its FILENAME/FMTTYPE and
// the correct decoded size — proving the inbound MCP-expose path sees it.
func TestForestNoteEmittedAttach_Parse(t *testing.T) {
	meta := ParseBlobMetadata(fnSampleBlob(t))
	if len(meta.Attachments) != 1 {
		t.Fatalf("attachments = %d, want 1 (decode/parse of FN bytes failed?)", len(meta.Attachments))
	}
	a := meta.Attachments[0]
	if !a.Inline {
		t.Errorf("Inline = false, want true (FN emits ENCODING=BASE64;VALUE=BINARY)")
	}
	if a.FmtType != "text/plain" {
		t.Errorf("FmtType = %q, want text/plain", a.FmtType)
	}
	if a.Filename != "recognized-text.txt" {
		t.Errorf("Filename = %q, want recognized-text.txt", a.Filename)
	}
	if want := int64(len(fnExpectedRecognized)); a.Size != want {
		t.Errorf("Size = %d, want %d (decoded byte length)", a.Size, want)
	}
	// The provenance siblings on the same hand-rolled VTODO also survive.
	if meta.NativeURL != "forestnote://notebook/NB1/page/PG1" {
		t.Errorf("NativeURL = %q, want the FN native deep link", meta.NativeURL)
	}
}

// Core fidelity: go-ical unfolds FN's folded ATTACH (incl. the VALUE=BINARY split
// across a fold) and base64-decodes it back to the exact original bytes.
func TestForestNoteEmittedAttach_DecodesToOriginal(t *testing.T) {
	cal, err := ical.NewDecoder(strings.NewReader(fnSampleBlob(t))).Decode()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	todo, err := FindVTODO(cal)
	if err != nil || todo == nil {
		t.Fatalf("no vtodo: %v", err)
	}
	p := todo.Props.Get(ical.PropAttach)
	if p == nil {
		t.Fatal("no ATTACH prop")
	}
	got, err := p.Binary()
	if err != nil {
		t.Fatalf("Binary(): %v", err)
	}
	if string(got) != fnExpectedRecognized {
		t.Errorf("decoded ATTACH mismatch:\n got=%q\nwant=%q", string(got), fnExpectedRecognized)
	}
}

// End to end through the real de-bloat: FN's inline bytes land in the content store,
// the stored blob loses the base64, and reconstruction restores the original.
func TestForestNoteEmittedAttach_DebloatRoundTrip(t *testing.T) {
	blob := fnSampleBlob(t)
	store := &taskattach.BlobStore{Root: t.TempDir()}
	signer := &taskattach.Signer{Secret: "secret"}
	deps := AttachDeps{Store: store, Signer: signer, BaseURL: "https://ub.example.com"}

	debloated := DebloatInlineAttachments(blob, deps)
	if strings.Contains(debloated, "TWVldGluZy") {
		t.Error("de-bloated blob still contains the inline base64 payload")
	}
	if !strings.Contains(debloated, "X-UB-INLINE=1") {
		t.Errorf("de-bloated blob missing X-UB-INLINE marker:\n%s", debloated)
	}

	cal, err := ical.NewDecoder(strings.NewReader(debloated)).Decode()
	if err != nil {
		t.Fatalf("decode debloated: %v", err)
	}
	ReconstructInlineAttachments(cal, store)
	todo, err := FindVTODO(cal)
	if err != nil || todo == nil {
		t.Fatalf("no vtodo after reconstruct: %v", err)
	}
	got, err := todo.Props.Get(ical.PropAttach).Binary()
	if err != nil {
		t.Fatalf("reconstructed Binary(): %v", err)
	}
	if string(got) != fnExpectedRecognized {
		t.Errorf("reconstructed mismatch:\n got=%q\nwant=%q", string(got), fnExpectedRecognized)
	}
}
