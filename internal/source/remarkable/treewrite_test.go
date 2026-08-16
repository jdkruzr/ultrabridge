package remarkable

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sysop/ultrabridge/internal/source"
)

// --- hashing / serialization vectors ---
// The tablet independently recomputes every hash in the tree, so these are
// byte-exact contracts, not implementation details.

func TestBlobHashHex_Vector(t *testing.T) {
	if got := blobHashHex([]byte("hello world")); got != "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9" {
		t.Fatalf("blobHashHex = %s", got)
	}
}

func TestHashOfEntries_SortsByNameAndHashesRawBytes(t *testing.T) {
	// sha256("a") / sha256("b"); entries deliberately passed metadata-first so
	// the test proves the sort: composite = sha256(raw(sha(b)) || raw(sha(a)))
	// because "doc.content" < "doc.metadata".
	entries := []indexEntry{
		{Hash: "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb", Type: "0", EntryName: "doc.metadata", Size: 1},
		{Hash: "3e23e8160039594a33894f6564e1b1348bbd7a0088d42c4acb73eeaed59c009d", Type: "0", EntryName: "doc.content", Size: 1},
	}
	got, err := hashOfEntries(entries)
	if err != nil {
		t.Fatalf("hashOfEntries: %v", err)
	}
	if got != "18d79cb747ea174c59f3a3b41768672526d56fecc58360a99d283d0f9b0a3cc0" {
		t.Fatalf("composite = %s", got)
	}
	// The input slice must not be reordered (mutation closures rely on it).
	if entries[0].EntryName != "doc.metadata" {
		t.Fatal("hashOfEntries reordered its input")
	}
	if _, err := hashOfEntries([]indexEntry{{Hash: "not-hex", EntryName: "x"}}); err == nil {
		t.Fatal("hashOfEntries accepted a non-hex hash")
	}
}

func TestSerializeIndex_ByteExact(t *testing.T) {
	entries := []indexEntry{
		{Hash: "bb", Type: "0", EntryName: "doc.metadata", Subfiles: 0, Size: 60},
		{Hash: "aa", Type: "0", EntryName: "doc.content", Subfiles: 0, Size: 40},
	}
	wantV3 := "3\naa:0:doc.content:0:40\nbb:0:doc.metadata:0:60\n"
	if got := string(serializeIndex("3", entries)); got != wantV3 {
		t.Fatalf("v3 index:\n%q\nwant:\n%q", got, wantV3)
	}
	wantV4 := "4\n0:.:2:100\naa:0:doc.content:0:40\nbb:0:doc.metadata:0:60\n"
	if got := string(serializeIndex("4", entries)); got != wantV4 {
		t.Fatalf("v4 index:\n%q\nwant:\n%q", got, wantV4)
	}
}

func TestSerializeIndex_RoundTripsThroughParse(t *testing.T) {
	entries := []indexEntry{
		{Hash: "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb", Type: entryTypeDoc, EntryName: "doc-1", Subfiles: 3, Size: 1234},
		{Hash: "3e23e8160039594a33894f6564e1b1348bbd7a0088d42c4acb73eeaed59c009d", Type: entryTypeDoc, EntryName: "doc-2", Subfiles: 1, Size: 77},
	}
	for _, schema := range []string{"3", "4"} {
		gotSchema, got, err := parseIndexWithSchema(serializeIndex(schema, entries))
		if err != nil {
			t.Fatalf("schema %s: parse: %v", schema, err)
		}
		if gotSchema != schema {
			t.Fatalf("schema %s round-tripped as %s", schema, gotSchema)
		}
		if len(got) != len(entries) {
			t.Fatalf("schema %s: %d entries, want %d", schema, len(got), len(entries))
		}
		for i := range got {
			if got[i] != entries[i] {
				t.Fatalf("schema %s entry %d = %+v, want %+v", schema, i, got[i], entries[i])
			}
		}
	}
}

// --- tree authoring ---

// seedEmptyRoot gives the store a root blob with an empty payload — the
// "device has synced, tree is empty" baseline every mutation test builds on.
func seedEmptyRoot(t *testing.T, st *store) {
	t.Helper()
	if _, err := st.putBlob(context.Background(), rootBlobID, strings.NewReader(""), 0); err != nil {
		t.Fatalf("seed root: %v", err)
	}
}

// readTopIndex returns the current top index (schema + entries) via the root.
func readTopIndex(t *testing.T, st *store) (string, []indexEntry) {
	t.Helper()
	ctx := context.Background()
	rec, err := st.getBlob(ctx, rootBlobID)
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	raw, err := osReadFile(rec.Path)
	if err != nil {
		t.Fatalf("read root payload: %v", err)
	}
	topHash := strings.TrimSpace(string(raw))
	if topHash == "" {
		return "", nil
	}
	data, err := st.readBlob(ctx, topHash)
	if err != nil {
		t.Fatalf("read top index %s: %v", topHash, err)
	}
	schema, entries, err := parseIndexWithSchema(data)
	if err != nil {
		t.Fatalf("parse top index: %v", err)
	}
	return schema, entries
}

func TestMutateTree_RequiresRoot(t *testing.T) {
	st := newTestStore(t)
	err := st.createFolder(context.Background(), "folder-1", "Books", "")
	if !errors.Is(err, ErrNoHashTree) {
		t.Fatalf("createFolder on legacy-only store = %v, want ErrNoHashTree", err)
	}
}

func TestCreateDocument_RoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	seedEmptyRoot(t, st)

	payload := []byte("%PDF-1.4 fake book contents")
	payloadHash, payloadSize, err := st.stageBlobStream(ctx, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("stage payload: %v", err)
	}
	if payloadSize != int64(len(payload)) {
		t.Fatalf("payload size = %d, want %d", payloadSize, len(payload))
	}
	if err := st.createFolder(ctx, "folder-1", "Books", ""); err != nil {
		t.Fatalf("createFolder: %v", err)
	}
	if err := st.createDocument(ctx, "doc-1", "Moby Dick", "folder-1", "pdf", payloadHash, payloadSize); err != nil {
		t.Fatalf("createDocument: %v", err)
	}

	docs, err := st.listDocumentTree(ctx)
	if err != nil {
		t.Fatalf("listDocumentTree: %v", err)
	}
	byID := map[string]Document{}
	for _, d := range docs {
		byID[d.ID] = d
	}
	folder, ok := byID["folder-1"]
	if !ok || folder.Type != "folder" || folder.Name != "Books" {
		t.Fatalf("folder = %+v (found %v)", folder, ok)
	}
	doc, ok := byID["doc-1"]
	if !ok || doc.Type != "document" || doc.Name != "Moby Dick" || doc.Parent != "folder-1" || doc.FileType != "pdf" {
		t.Fatalf("doc = %+v (found %v)", doc, ok)
	}

	rd, err := st.renderDocument(ctx, "doc-1")
	if err != nil {
		t.Fatalf("renderDocument: %v", err)
	}
	if rd.PDFPath == "" {
		t.Fatal("renderDocument resolved no PDF payload")
	}
	got, err := osReadFile(rd.PDFPath)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("payload round-trip mismatch (err=%v)", err)
	}

	// The top index must be internally consistent: root payload equals the
	// composite hash of its entries, and each doc entry's hash matches its
	// serialized sub-index blob id.
	_, entries := readTopIndex(t, st)
	if len(entries) != 2 {
		t.Fatalf("top index has %d entries, want 2", len(entries))
	}
	for _, e := range entries {
		sub, err := st.readBlob(ctx, e.Hash)
		if err != nil {
			t.Fatalf("sub-index %s unreadable: %v", e.EntryName, err)
		}
		_, files, err := parseIndexWithSchema(sub)
		if err != nil {
			t.Fatalf("sub-index %s: %v", e.EntryName, err)
		}
		wantHash, err := hashOfEntries(files)
		if err != nil {
			t.Fatalf("hash sub-index %s: %v", e.EntryName, err)
		}
		if wantHash != e.Hash {
			t.Fatalf("doc entry %s hash %s != composite of its files %s", e.EntryName, e.Hash, wantHash)
		}
		if e.Subfiles != len(files) {
			t.Fatalf("doc entry %s subfiles %d != %d files", e.EntryName, e.Subfiles, len(files))
		}
		var sum int64
		for _, f := range files {
			sum += f.Size
		}
		if e.Size != sum {
			t.Fatalf("doc entry %s size %d != files total %d", e.EntryName, e.Size, sum)
		}
	}
}

func TestCreateDocument_ValidatesParent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	seedEmptyRoot(t, st)
	payloadHash, payloadSize, _ := st.stageBlobBytes(ctx, []byte("pdf"))

	err := st.createDocument(ctx, "doc-1", "Doc", "no-such-folder", "pdf", payloadHash, payloadSize)
	if !errors.Is(err, ErrParentNotFound) {
		t.Fatalf("bad parent = %v, want ErrParentNotFound", err)
	}
	err = st.createDocument(ctx, "doc-1", "Doc", trashParent, "pdf", payloadHash, payloadSize)
	if !errors.Is(err, ErrParentNotFound) {
		t.Fatalf("trash parent = %v, want ErrParentNotFound", err)
	}
	if err := st.createDocument(ctx, "doc-1", "Doc", "", "pdf", payloadHash, payloadSize); err != nil {
		t.Fatalf("root parent: %v", err)
	}
	err = st.createDocument(ctx, "doc-2", "Doc2", "doc-1", "pdf", payloadHash, payloadSize)
	if !errors.Is(err, ErrNotAFolder) {
		t.Fatalf("document parent = %v, want ErrNotAFolder", err)
	}
}

func TestSchemaPreservation(t *testing.T) {
	for _, schema := range []string{"3", "4"} {
		t.Run("v"+schema, func(t *testing.T) {
			st := newTestStore(t)
			ctx := context.Background()
			// Seed an empty top index in the given schema, referenced by root.
			topID := "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb"
			seedBlob(t, st, topID, string(serializeIndex(schema, nil)))
			seedBlob(t, st, rootBlobID, topID)

			if err := st.createFolder(ctx, "folder-1", "Books", ""); err != nil {
				t.Fatalf("createFolder: %v", err)
			}
			gotSchema, entries := readTopIndex(t, st)
			if gotSchema != schema {
				t.Fatalf("rewritten top index schema = %s, want %s", gotSchema, schema)
			}
			if len(entries) != 1 || entries[0].EntryName != "folder-1" {
				t.Fatalf("entries = %+v", entries)
			}
		})
	}
}

func TestRewriteMetadata_PreservesUnknownFieldsAndBumpsVersion(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	seedEmptyRoot(t, st)
	payloadHash, payloadSize, _ := st.stageBlobBytes(ctx, []byte("pdf"))
	if err := st.createDocument(ctx, "doc-1", "Doc", "", "pdf", payloadHash, payloadSize); err != nil {
		t.Fatalf("createDocument: %v", err)
	}

	// Inject a device-owned field UB doesn't model.
	err := st.mutateTree(ctx, func(ctx context.Context, tr *treeState) error {
		return st.rewriteMetadataInTree(ctx, tr, "doc-1", func(meta map[string]any) error {
			meta["pinned"] = true
			meta["deviceOnlyKey"] = "survive-me"
			return nil
		})
	})
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	if err := st.renameNode(ctx, "doc-1", "Renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	var meta map[string]any
	err = st.mutateTree(ctx, func(ctx context.Context, tr *treeState) error {
		m, err := st.nodeMetaInTree(ctx, tr, "doc-1")
		meta = m
		return err
	})
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if meta["visibleName"] != "Renamed" {
		t.Fatalf("visibleName = %v", meta["visibleName"])
	}
	if meta["deviceOnlyKey"] != "survive-me" || meta["pinned"] != true {
		t.Fatalf("device-owned fields lost: %+v", meta)
	}
	// version: 1 on create, 2 after inject, 3 after rename.
	if v, _ := meta["version"].(float64); int(v) != 3 {
		t.Fatalf("version = %v, want 3", meta["version"])
	}
	if meta["metadatamodified"] != true || meta["synced"] != true {
		t.Fatalf("modification flags: %+v", meta)
	}

	// The metadata entry's Size in the sub-index must track the new blob
	// (the rmfakecloud bug we deliberately fixed).
	_, entries := readTopIndex(t, st)
	sub, _ := st.readBlob(ctx, entries[0].Hash)
	_, files, _ := parseIndexWithSchema(sub)
	for _, f := range files {
		if strings.HasSuffix(f.EntryName, ".metadata") {
			blob, err := st.readBlob(ctx, f.Hash)
			if err != nil {
				t.Fatalf("read metadata blob: %v", err)
			}
			if int64(len(blob)) != f.Size {
				t.Fatalf("metadata entry size %d != blob size %d", f.Size, len(blob))
			}
		}
	}
}

func TestTrashNode(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	seedEmptyRoot(t, st)
	payloadHash, payloadSize, _ := st.stageBlobBytes(ctx, []byte("pdf"))
	if err := st.createFolder(ctx, "folder-1", "Books", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.createDocument(ctx, "doc-1", "Doc", "folder-1", "pdf", payloadHash, payloadSize); err != nil {
		t.Fatal(err)
	}

	// Non-empty folder refuses.
	if err := st.trashNode(ctx, "folder-1"); !errors.Is(err, ErrFolderNotEmpty) {
		t.Fatalf("trash non-empty folder = %v, want ErrFolderNotEmpty", err)
	}
	// Trash the doc: gone from listing and render, still present in the top
	// index (the tablet's trash keeps it alive).
	if err := st.trashNode(ctx, "doc-1"); err != nil {
		t.Fatalf("trash doc: %v", err)
	}
	docs, _ := st.listDocumentTree(ctx)
	for _, d := range docs {
		if d.ID == "doc-1" {
			t.Fatal("trashed document still listed")
		}
	}
	if _, err := st.renderDocument(ctx, "doc-1"); !errors.Is(err, errDocumentNotFound) {
		t.Fatalf("render trashed doc = %v, want errDocumentNotFound", err)
	}
	_, entries := readTopIndex(t, st)
	found := false
	for _, e := range entries {
		if e.EntryName == "doc-1" {
			found = true
		}
	}
	if !found {
		t.Fatal("trashed doc dropped from the top index — must stay for the tablet's trash screen")
	}
	// Folder is empty now (its only child is trashed) — trash succeeds.
	if err := st.trashNode(ctx, "folder-1"); err != nil {
		t.Fatalf("trash now-empty folder: %v", err)
	}
	// Unknown node.
	if err := st.trashNode(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("trash unknown = %v, want ErrNotFound", err)
	}
}

func TestMoveNode_RefusesCycles(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	seedEmptyRoot(t, st)
	if err := st.createFolder(ctx, "folder-a", "A", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.createFolder(ctx, "folder-b", "B", "folder-a"); err != nil {
		t.Fatal(err)
	}
	if err := st.moveNode(ctx, "folder-a", "folder-b"); err == nil {
		t.Fatal("moving a folder into its own subtree succeeded")
	}
	if err := st.moveNode(ctx, "folder-a", "folder-a"); err == nil {
		t.Fatal("moving a folder into itself succeeded")
	}
	// A legal move still works.
	if err := st.moveNode(ctx, "folder-b", ""); err != nil {
		t.Fatalf("legal move: %v", err)
	}
	docs, _ := st.listDocumentTree(ctx)
	for _, d := range docs {
		if d.ID == "folder-b" && d.Parent != "" {
			t.Fatalf("folder-b parent = %q after move to root", d.Parent)
		}
	}
}

func TestMutateTree_ConcurrentCommitsStayConsistent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	seedEmptyRoot(t, st)

	const writers = 5
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = st.createFolder(ctx, fmt.Sprintf("folder-%d", i), fmt.Sprintf("F%d", i), "")
		}(i)
	}
	wg.Wait()

	succeeded := 0
	for i, err := range errs {
		if err == nil {
			succeeded++
			continue
		}
		if !errors.Is(err, ErrTreeConflict) {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	if succeeded == 0 {
		t.Fatal("no concurrent commit succeeded")
	}
	// Root must point at a top index whose composite hash matches, holding
	// exactly the folders whose commits reported success.
	_, entries := readTopIndex(t, st)
	if len(entries) != succeeded {
		t.Fatalf("top index has %d entries, want %d (successful commits)", len(entries), succeeded)
	}
	wantRoot, err := hashOfEntries(entries)
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := st.getBlob(ctx, rootBlobID)
	raw, _ := osReadFile(rec.Path)
	if strings.TrimSpace(string(raw)) != wantRoot {
		t.Fatalf("root payload %s != composite of top entries %s", strings.TrimSpace(string(raw)), wantRoot)
	}
}

// --- Source-level file management ---

func newStartedSource(t *testing.T) *Source {
	t.Helper()
	db := testDB(t)
	row := source.SourceRow{
		Type:       "remarkable",
		Name:       "RM",
		ConfigJSON: `{"data_path":"` + t.TempDir() + `","pairing_code":"123456"}`,
	}
	src, err := NewSource(db, row, source.SharedDeps{})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if err := src.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(src.Stop)
	return src
}

func TestSource_UploadDownloadRoundTrip(t *testing.T) {
	src := newStartedSource(t)
	ctx := context.Background()
	seedEmptyRoot(t, src.store)

	for _, tc := range []struct {
		filename, fileType, contentType string
	}{
		{"Moby Dick.pdf", "pdf", "application/pdf"},
		{"Frankenstein.EPUB", "epub", "application/epub+zip"},
	} {
		payload := []byte("payload bytes for " + tc.filename)
		doc, err := src.UploadDocument(ctx, tc.filename, "", bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("upload %s: %v", tc.filename, err)
		}
		if doc.FileType != tc.fileType || doc.Type != "document" {
			t.Fatalf("uploaded doc = %+v", doc)
		}
		stream, name, contentType, err := src.DownloadDocument(ctx, doc.ID)
		if err != nil {
			t.Fatalf("download %s: %v", doc.ID, err)
		}
		got, _ := io.ReadAll(stream)
		stream.Close()
		if !bytes.Equal(got, payload) {
			t.Fatalf("download %s: payload mismatch", tc.filename)
		}
		if contentType != tc.contentType {
			t.Fatalf("content type = %s, want %s", contentType, tc.contentType)
		}
		// Extension is stripped case-insensitively and re-added lowercase.
		wantName := tc.filename[:strings.LastIndex(tc.filename, ".")] + "." + tc.fileType
		if name != wantName {
			t.Fatalf("download name = %q, want %q", name, wantName)
		}
	}

	if _, err := src.UploadDocument(ctx, "notes.txt", "", strings.NewReader("nope")); !errors.Is(err, ErrUnsupportedFile) {
		t.Fatalf("txt upload = %v, want ErrUnsupportedFile", err)
	}
	if _, err := src.UploadDocument(ctx, "empty.pdf", "", strings.NewReader("")); !errors.Is(err, ErrUnsupportedFile) {
		t.Fatalf("empty upload = %v, want ErrUnsupportedFile", err)
	}
}

func TestSource_DeleteHidesDocument(t *testing.T) {
	src := newStartedSource(t)
	ctx := context.Background()
	seedEmptyRoot(t, src.store)

	doc, err := src.UploadDocument(ctx, "Book.pdf", "", strings.NewReader("pdf bytes"))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if err := src.DeleteDocument(ctx, doc.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	docs, err := src.ListDocuments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		if d.ID == doc.ID {
			t.Fatal("deleted document still listed")
		}
	}
	if _, _, _, err := src.DownloadDocument(ctx, doc.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("download after delete = %v, want ErrNotFound", err)
	}
}

func TestSource_MutationNotifiesAllDevices(t *testing.T) {
	srv, src := startNotifyServer(t)
	seedEmptyRoot(t, src.store)

	token := pairUserTokenHTTP(t, srv, "device-a", "reMarkable 2")
	conn, _, err := dialNotifyWS(t, srv, token, "device-a")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	time.Sleep(50 * time.Millisecond)

	if _, err := src.UploadDocument(context.Background(), "Book.pdf", "", strings.NewReader("pdf")); err != nil {
		t.Fatalf("upload: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var got wsMessage
	if err := conn.ReadJSON(&got); err != nil {
		t.Fatalf("device read after server mutation: %v", err)
	}
	if got.Message.Attributes.Event != eventSyncComplete {
		t.Errorf("event = %q, want SyncComplete", got.Message.Attributes.Event)
	}
	// Server-authored: no originating device, so even device-a is notified
	// (asserted by the successful read above) and the source id is empty.
	if got.Message.Attributes.SourceDeviceID != "" {
		t.Errorf("sourceDeviceID = %q, want empty for a server mutation", got.Message.Attributes.SourceDeviceID)
	}
	_ = http.StatusOK
}
