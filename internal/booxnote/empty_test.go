package booxnote

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

// buildNoteInfo emits the minimal note_info wire encoding Open needs: title
// (field 6) and pageNameList (field 20) as a bare JSON array.
func buildNoteInfo(t *testing.T, title string, pageNames []string) []byte {
	t.Helper()
	names, err := json.Marshal(pageNames)
	if err != nil {
		t.Fatalf("marshal pageNameList: %v", err)
	}
	var b []byte
	b = protowire.AppendTag(b, 6, protowire.BytesType)
	b = protowire.AppendString(b, title)
	b = protowire.AppendTag(b, 20, protowire.BytesType)
	b = protowire.AppendBytes(b, names)
	return b
}

// buildArchive writes a .note-shaped ZIP from an explicit entry list.
func buildArchive(t *testing.T, entries map[string][]byte) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return bytes.NewReader(buf.Bytes())
}

// TestOpen_EmptyNotebook models the archive shape found in production behind
// Boox jobs 1504 and 1571 (Tomorrow.note, 20250829 Presidio Session Notes.note):
// note_info declares a page, but the archive has no virtual/page/pb directory
// and no entry under the bare page id — a notebook created on the device and
// never drawn in. That must be reported as ErrEmptyNotebook so the pipeline
// skips it, not as a generic parse failure that lands in `failed` forever.
func TestOpen_EmptyNotebook(t *testing.T) {
	const noteID = "effb14ba3645472c9b34b74932773bc9"
	const pageID = "0070b35c7cf8430397bfed00e250d82f"

	r := buildArchive(t, map[string][]byte{
		noteID + "/note/pb/note_info": buildNoteInfo(t, "Tomorrow", []string{pageID}),
		// A zero-byte resource entry under an unrelated uuid, exactly as the
		// real files carry — deliberately NOT the declared page id.
		noteID + "/resource/pb/208e4fed-7245-4f65-b99a-f579aba1ded5#1776493770972": {},
		noteID + "/extra/pb/extra": []byte("{}"),
	})

	_, err := Open(r, int64(r.Len()))
	if err == nil {
		t.Fatal("expected an error for a notebook with no page data")
	}
	if !errors.Is(err, ErrEmptyNotebook) {
		t.Fatalf("err = %v, want ErrEmptyNotebook", err)
	}
	if !strings.Contains(err.Error(), "1 declared page") {
		t.Errorf("error should report how many pages were declared: %v", err)
	}
}

// TestOpen_PartiallyMissingPagesStillFails is the other half of the rule: an
// archive where some declared pages have real content and others vanished is
// truncated or corrupt, and must NOT be quietly skipped as empty.
func TestOpen_PartiallyMissingPagesStillFails(t *testing.T) {
	const noteID = "note1"
	const realPage = "page-real"
	const gonePage = "page-gone"

	// A VirtualPage entry for the real page only. Field 1 = pageId,
	// field 6 = pageSize.
	var vp []byte
	vp = protowire.AppendTag(vp, 1, protowire.BytesType)
	vp = protowire.AppendString(vp, realPage)
	vp = protowire.AppendTag(vp, 6, protowire.BytesType)
	vp = protowire.AppendString(vp, "1404x1872")

	// The fallback branch looks up the BARE page id as the entry name (not a
	// noteID-prefixed path), so that is where a resolvable page must live.
	r := buildArchive(t, map[string][]byte{
		noteID + "/note/pb/note_info": buildNoteInfo(t, "Half gone", []string{realPage, gonePage}),
		realPage:                      vp,
	})

	_, err := Open(r, int64(r.Len()))
	if err == nil {
		t.Fatal("expected an error for a partially missing archive")
	}
	if errors.Is(err, ErrEmptyNotebook) {
		t.Fatalf("truncated archive misreported as empty — it would be silently skipped: %v", err)
	}
	if !strings.Contains(err.Error(), "missing from archive") {
		t.Errorf("error should name the missing-page condition: %v", err)
	}
}

// TestOpen_RealEmptyNotebooks runs the rule against the actual files from the
// deployment, when present. Skipped in environments without them so the suite
// stays hermetic.
func TestOpen_RealEmptyNotebooks(t *testing.T) {
	for _, path := range []string{
		"/mnt/supernote/boox-notes/onyx/Palma2_Pro_C/Notebooks/Personal/Tomorrow.note",
		"/mnt/supernote/boox-notes/onyx/Palma2_Pro_C/Notebooks/Moffitt/20250829 Presidio Session Notes.note",
	} {
		f, err := os.Open(path)
		if err != nil {
			t.Skipf("fixture not present: %v", err)
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			t.Fatalf("stat: %v", err)
		}
		_, err = Open(f, fi.Size())
		f.Close()
		if !errors.Is(err, ErrEmptyNotebook) {
			t.Errorf("%s: err = %v, want ErrEmptyNotebook", path, err)
		}
	}
}
