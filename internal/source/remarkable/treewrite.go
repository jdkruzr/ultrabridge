package remarkable

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Server-authored hashtree mutations. The device is the other writer of the
// same tree; every commit here goes through the identical root-generation CAS
// the device uses (putBlob with matchGeneration), so whichever side commits
// second re-reads and re-applies. Formats mirror rmfakecloud (the canonical
// open-source reMarkable cloud): the tablet independently recomputes every
// hash in the tree, so index serialization and composite hashing must be
// byte-exact.
var (
	ErrNoHashTree      = errors.New("remarkable: no synced hashtree yet")
	ErrNotFound        = errors.New("remarkable: document not found")
	ErrParentNotFound  = errors.New("remarkable: parent folder not found")
	ErrNotAFolder      = errors.New("remarkable: parent is not a folder")
	ErrNoPayload       = errors.New("remarkable: document has no downloadable payload")
	ErrUnsupportedFile = errors.New("remarkable: unsupported file type")
	ErrTreeConflict    = errors.New("remarkable: tree commit conflict")
	ErrFolderNotEmpty  = errors.New("remarkable: folder is not empty")
)

const (
	entryTypeFile = "0"
	entryTypeDoc  = "80000000"
	trashParent   = "trash"

	// Doc sub-indexes are always schema 3 on the wire (rmfakecloud hardcodes
	// this even when the root index is schema 4).
	docIndexSchema = "3"
)

// contentTemplate is the .content blob for a fresh PDF/EPUB document,
// byte-for-byte from rmfakecloud's createContent (documentcreator.go) —
// device-proven bytes, including the leading and trailing newlines. The single
// %s is the file type ("pdf" | "epub"). The tablet fills in real page data
// after first open.
const contentTemplate = `
{
	"dummyDocument": false,
	"extraMetadata": {
		"LastPen": "Finelinerv2",
		"LastTool": "Finelinerv2",
		"ThicknessScale": "",
		"LastFinelinerv2Size": "1"
	},
	"fileType": "%s",
	"fontName": "",
	"lastOpenedPage": 0,
	"lineHeight": -1,
	"margins": 180,
	"orientation": "portrait",
	"pageCount": 0,
	"pages": [],
	"textScale": 1,
	"transform": {
		"m11": 1,
		"m12": 0,
		"m13": 0,
		"m21": 0,
		"m22": 1,
		"m23": 0,
		"m31": 0,
		"m32": 0,
		"m33": 1
	}
}
`

// rmMetadataWrite is the full .metadata field set for a NEW document or
// folder, matching rmfakecloud's MetadataFile exactly (all fields emitted,
// camelCase, ms-epoch times as decimal strings). Rewrites of existing
// metadata go through map[string]any instead to preserve device-owned fields
// we don't model.
type rmMetadataWrite struct {
	VisibleName      string `json:"visibleName"`
	Type             string `json:"type"` // "DocumentType" | "CollectionType"
	Parent           string `json:"parent"`
	CreatedTime      string `json:"createdTime"`
	LastModified     string `json:"lastModified"`
	LastOpened       string `json:"lastOpened"`
	LastOpenedPage   int    `json:"lastOpenedPage"`
	Version          int    `json:"version"`
	Pinned           bool   `json:"pinned"`
	Synced           bool   `json:"synced"`
	Modified         bool   `json:"modified"`
	Deleted          bool   `json:"deleted"`
	MetadataModified bool   `json:"metadatamodified"`
}

func nowMillisString() string {
	return strconv.FormatInt(time.Now().UnixMilli(), 10)
}

// treeState is one attempt's view of the top index: the schema it was stored
// with (preserved on rewrite) and its document entries. Mutation closures
// edit entries and stage blobs; mutateTree serializes and commits.
type treeState struct {
	schema  string
	entries []indexEntry
}

func (t *treeState) find(docID string) (int, bool) {
	for i := range t.entries {
		if t.entries[i].EntryName == docID {
			return i, true
		}
	}
	return -1, false
}

func blobHashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// hashOfEntries computes a composite hash the way the device does: sha256
// over the concatenated raw 32-byte decoded child hashes, in EntryName order.
// This is the blob ID for doc sub-indexes and the top index (NOT the sha256
// of the serialized index bytes).
func hashOfEntries(entries []indexEntry) (string, error) {
	sorted := sortedByName(entries)
	h := sha256.New()
	for _, e := range sorted {
		raw, err := hex.DecodeString(e.Hash)
		if err != nil {
			return "", fmt.Errorf("entry %s has invalid hash %q: %w", e.EntryName, e.Hash, err)
		}
		h.Write(raw)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// serializeIndex renders an index blob: schema line, a summary line for
// schema 4, then one entry per line in EntryName order, each \n-terminated.
func serializeIndex(schema string, entries []indexEntry) []byte {
	sorted := sortedByName(entries)
	var buf bytes.Buffer
	buf.WriteString(schema)
	buf.WriteByte('\n')
	if schema == "4" {
		var total int64
		for _, e := range sorted {
			total += e.Size
		}
		fmt.Fprintf(&buf, "0:.:%d:%d\n", len(sorted), total)
	}
	for _, e := range sorted {
		fmt.Fprintf(&buf, "%s:%s:%s:%d:%d\n", e.Hash, e.Type, e.EntryName, e.Subfiles, e.Size)
	}
	return buf.Bytes()
}

func sortedByName(entries []indexEntry) []indexEntry {
	out := make([]indexEntry, len(entries))
	copy(out, entries)
	sort.Slice(out, func(i, j int) bool { return out[i].EntryName < out[j].EntryName })
	return out
}

func sumEntrySizes(entries []indexEntry) int64 {
	var total int64
	for _, e := range entries {
		total += e.Size
	}
	return total
}

// stageBlobBytes stores a small in-memory blob under its content hash.
// Content-addressed, so redoing it on a CAS retry is a no-op re-upsert.
func (s *store) stageBlobBytes(ctx context.Context, data []byte) (string, int64, error) {
	hash := blobHashHex(data)
	if _, err := s.putBlob(ctx, hash, bytes.NewReader(data), 0); err != nil {
		return "", 0, err
	}
	return hash, int64(len(data)), nil
}

// stageBlobStream spools a payload to a temp file (same filesystem as the
// blob store) while hashing, then stores it under its content hash.
func (s *store) stageBlobStream(ctx context.Context, r io.Reader) (string, int64, error) {
	dir := filepath.Join(s.dataPath, "blobs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, err
	}
	tmp, err := os.CreateTemp(dir, "upload-*")
	if err != nil {
		return "", 0, err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()
	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		return "", 0, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	hash := hex.EncodeToString(h.Sum(nil))
	if _, err := s.putBlob(ctx, hash, tmp, 0); err != nil {
		return "", 0, err
	}
	return hash, size, nil
}

// mutateTree runs one server-authored tree mutation under the same
// root-generation CAS the device uses. The closure must be a pure function of
// the fresh treeState (find by docID, never captured indices): on a
// generation conflict with a concurrent device sync it re-runs against a
// re-read tree. Blob staging inside the closure is content-addressed and
// therefore safe to redo.
func (s *store) mutateTree(ctx context.Context, mutate func(ctx context.Context, t *treeState) error) error {
	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		rootRec, err := s.getBlob(ctx, rootBlobID)
		if errors.Is(err, errBlobNotFound) {
			return ErrNoHashTree
		}
		if err != nil {
			return err
		}
		topHashRaw, err := osReadFile(rootRec.Path)
		if err != nil {
			return err
		}
		topHash := strings.TrimSpace(string(topHashRaw))

		t := treeState{schema: docIndexSchema}
		if topHash != "" {
			topData, err := s.readBlob(ctx, topHash)
			if err != nil {
				// The root names a top index we haven't received — a device
				// sync is in flight. Refuse rather than author a tree that
				// would orphan the device's documents.
				return fmt.Errorf("remarkable: top index %s not synced yet: %w", topHash, err)
			}
			schema, entries, err := parseIndexWithSchema(topData)
			if err != nil {
				return fmt.Errorf("parse top index: %w", err)
			}
			t.schema = schema
			t.entries = entries
		}

		if err := mutate(ctx, &t); err != nil {
			return err
		}

		newTopHash, err := hashOfEntries(t.entries)
		if err != nil {
			return err
		}
		if _, err := s.putBlob(ctx, newTopHash, bytes.NewReader(serializeIndex(t.schema, t.entries)), 0); err != nil {
			return err
		}
		_, err = s.putBlob(ctx, rootBlobID, strings.NewReader(newTopHash), rootRec.Generation)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errGenerationMismatch) {
			return err
		}
		// Lost the race against a device commit — re-read and re-apply.
	}
	return ErrTreeConflict
}

// stageDocEntry writes a document's sub-index blob and returns its top-index
// entry (hash = composite of the file entries; that hash doubles as the
// document revision everywhere else in this package).
func (s *store) stageDocEntry(ctx context.Context, docID string, files []indexEntry) (indexEntry, error) {
	docHash, err := hashOfEntries(files)
	if err != nil {
		return indexEntry{}, err
	}
	if _, err := s.putBlob(ctx, docHash, bytes.NewReader(serializeIndex(docIndexSchema, files)), 0); err != nil {
		return indexEntry{}, err
	}
	return indexEntry{
		Hash:      docHash,
		Type:      entryTypeDoc,
		EntryName: docID,
		Subfiles:  len(files),
		Size:      sumEntrySizes(files),
	}, nil
}

// createDocument authors a fresh document (metadata + content + payload) into
// the tree. The payload blob must already be staged (hash/size from
// stageBlobStream). fileType is "pdf" or "epub".
func (s *store) createDocument(ctx context.Context, docID, visibleName, parent, fileType, payloadHash string, payloadSize int64) error {
	return s.mutateTree(ctx, func(ctx context.Context, t *treeState) error {
		if _, ok := t.find(docID); ok {
			return fmt.Errorf("remarkable: document %s already exists", docID)
		}
		if err := s.validateParent(ctx, t, parent); err != nil {
			return err
		}
		now := nowMillisString()
		metaJSON, err := json.Marshal(rmMetadataWrite{
			VisibleName:      visibleName,
			Type:             "DocumentType",
			Parent:           parent,
			CreatedTime:      now,
			LastModified:     now,
			Version:          1,
			Synced:           true,
			MetadataModified: true,
		})
		if err != nil {
			return err
		}
		metaHash, metaSize, err := s.stageBlobBytes(ctx, metaJSON)
		if err != nil {
			return err
		}
		content := []byte(fmt.Sprintf(contentTemplate, fileType))
		contentHash, contentSize, err := s.stageBlobBytes(ctx, content)
		if err != nil {
			return err
		}
		files := []indexEntry{
			{Hash: metaHash, Type: entryTypeFile, EntryName: docID + ".metadata", Size: metaSize},
			{Hash: contentHash, Type: entryTypeFile, EntryName: docID + ".content", Size: contentSize},
			{Hash: payloadHash, Type: entryTypeFile, EntryName: docID + "." + fileType, Size: payloadSize},
		}
		entry, err := s.stageDocEntry(ctx, docID, files)
		if err != nil {
			return err
		}
		t.entries = append(t.entries, entry)
		return nil
	})
}

// createFolder authors a metadata-only CollectionType node.
func (s *store) createFolder(ctx context.Context, docID, name, parent string) error {
	return s.mutateTree(ctx, func(ctx context.Context, t *treeState) error {
		if _, ok := t.find(docID); ok {
			return fmt.Errorf("remarkable: document %s already exists", docID)
		}
		if err := s.validateParent(ctx, t, parent); err != nil {
			return err
		}
		now := nowMillisString()
		metaJSON, err := json.Marshal(rmMetadataWrite{
			VisibleName:      name,
			Type:             "CollectionType",
			Parent:           parent,
			CreatedTime:      now,
			LastModified:     now,
			Version:          1,
			Synced:           true,
			MetadataModified: true,
		})
		if err != nil {
			return err
		}
		metaHash, metaSize, err := s.stageBlobBytes(ctx, metaJSON)
		if err != nil {
			return err
		}
		files := []indexEntry{
			{Hash: metaHash, Type: entryTypeFile, EntryName: docID + ".metadata", Size: metaSize},
		}
		entry, err := s.stageDocEntry(ctx, docID, files)
		if err != nil {
			return err
		}
		t.entries = append(t.entries, entry)
		return nil
	})
}

// nodeMetaInTree loads a node's raw .metadata as a map, preserving fields we
// don't model. Returns ErrNotFound when the node or its metadata is absent.
func (s *store) nodeMetaInTree(ctx context.Context, t *treeState, docID string) (map[string]any, error) {
	i, ok := t.find(docID)
	if !ok {
		return nil, ErrNotFound
	}
	subData, err := s.readBlob(ctx, t.entries[i].Hash)
	if err != nil {
		return nil, ErrNotFound
	}
	_, files, err := parseIndexWithSchema(subData)
	if err != nil {
		return nil, fmt.Errorf("parse sub-index for %s: %w", docID, err)
	}
	for _, fe := range files {
		if fe.EntryName == docID+".metadata" {
			var meta map[string]any
			if err := s.readJSONBlob(ctx, fe.Hash, &meta); err != nil {
				return nil, ErrNotFound
			}
			return meta, nil
		}
	}
	return nil, ErrNotFound
}

// rewriteMetadataInTree patches one node's .metadata through map[string]any
// (device-owned fields we don't model survive untouched), bumps version,
// stamps lastModified, and recomputes the sub-index and top-index entries —
// including the metadata entry's Size, which rmfakecloud forgets to update.
func (s *store) rewriteMetadataInTree(ctx context.Context, t *treeState, docID string, patch func(meta map[string]any) error) error {
	i, ok := t.find(docID)
	if !ok {
		return ErrNotFound
	}
	subData, err := s.readBlob(ctx, t.entries[i].Hash)
	if err != nil {
		return fmt.Errorf("read sub-index for %s: %w", docID, err)
	}
	_, files, err := parseIndexWithSchema(subData)
	if err != nil {
		return fmt.Errorf("parse sub-index for %s: %w", docID, err)
	}
	metaIdx := -1
	for j := range files {
		if files[j].EntryName == docID+".metadata" {
			metaIdx = j
			break
		}
	}
	if metaIdx < 0 {
		return ErrNotFound
	}
	var meta map[string]any
	if err := s.readJSONBlob(ctx, files[metaIdx].Hash, &meta); err != nil {
		return fmt.Errorf("read metadata for %s: %w", docID, err)
	}
	if err := patch(meta); err != nil {
		return err
	}
	version := 1
	if v, ok := meta["version"].(float64); ok {
		version = int(v) + 1
	}
	meta["version"] = version
	meta["lastModified"] = nowMillisString()
	meta["metadatamodified"] = true
	meta["synced"] = true
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	metaHash, metaSize, err := s.stageBlobBytes(ctx, metaJSON)
	if err != nil {
		return err
	}
	files[metaIdx].Hash = metaHash
	files[metaIdx].Size = metaSize
	entry, err := s.stageDocEntry(ctx, docID, files)
	if err != nil {
		return err
	}
	t.entries[i] = entry
	return nil
}

// validateParent checks a prospective parent: "" is My files; "trash" and
// anything that isn't a live CollectionType node are refused.
func (s *store) validateParent(ctx context.Context, t *treeState, parent string) error {
	if parent == "" {
		return nil
	}
	if parent == trashParent {
		return fmt.Errorf("%w: %q", ErrParentNotFound, parent)
	}
	meta, err := s.nodeMetaInTree(ctx, t, parent)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w: %q", ErrParentNotFound, parent)
	}
	if err != nil {
		return err
	}
	if metaString(meta, "type") != "CollectionType" {
		return fmt.Errorf("%w: %q", ErrNotAFolder, parent)
	}
	if metaBool(meta, "deleted") || metaString(meta, "parent") == trashParent {
		return fmt.Errorf("%w: %q", ErrParentNotFound, parent)
	}
	return nil
}

// checkNoCycle refuses moving a folder under itself or any of its
// descendants by walking the new parent's ancestor chain.
func (s *store) checkNoCycle(ctx context.Context, t *treeState, docID, newParent string) error {
	const maxDepth = 100
	cur := newParent
	for depth := 0; cur != "" && cur != trashParent && depth < maxDepth; depth++ {
		if cur == docID {
			return fmt.Errorf("remarkable: cannot move %s into its own subtree", docID)
		}
		meta, err := s.nodeMetaInTree(ctx, t, cur)
		if err != nil {
			return nil // broken chain — parent validation already vouched for the target itself
		}
		cur = metaString(meta, "parent")
	}
	return nil
}

// trashNode moves a node to the tablet's trash. Non-empty folders are
// refused (trashing the folder alone would strand its children in a
// half-visible state on the UB side; cascade is a possible follow-up).
func (s *store) trashNode(ctx context.Context, docID string) error {
	return s.mutateTree(ctx, func(ctx context.Context, t *treeState) error {
		meta, err := s.nodeMetaInTree(ctx, t, docID)
		if err != nil {
			return err
		}
		if metaString(meta, "type") == "CollectionType" {
			for _, e := range t.entries {
				if e.EntryName == docID {
					continue
				}
				childMeta, err := s.nodeMetaInTree(ctx, t, e.EntryName)
				if err != nil {
					continue
				}
				if metaString(childMeta, "parent") == docID && !metaBool(childMeta, "deleted") {
					return fmt.Errorf("%w: %s", ErrFolderNotEmpty, docID)
				}
			}
		}
		return s.rewriteMetadataInTree(ctx, t, docID, func(meta map[string]any) error {
			meta["parent"] = trashParent
			return nil
		})
	})
}

// moveNode re-parents a node after validating the target folder and
// refusing cycles, all under one CAS attempt.
func (s *store) moveNode(ctx context.Context, docID, newParent string) error {
	return s.mutateTree(ctx, func(ctx context.Context, t *treeState) error {
		if err := s.validateParent(ctx, t, newParent); err != nil {
			return err
		}
		if err := s.checkNoCycle(ctx, t, docID, newParent); err != nil {
			return err
		}
		return s.rewriteMetadataInTree(ctx, t, docID, func(meta map[string]any) error {
			meta["parent"] = newParent
			return nil
		})
	})
}

// renameNode sets a node's visible name.
func (s *store) renameNode(ctx context.Context, docID, name string) error {
	return s.mutateTree(ctx, func(ctx context.Context, t *treeState) error {
		return s.rewriteMetadataInTree(ctx, t, docID, func(meta map[string]any) error {
			meta["visibleName"] = name
			return nil
		})
	})
}

func metaString(meta map[string]any, key string) string {
	v, _ := meta[key].(string)
	return v
}

func metaBool(meta map[string]any, key string) bool {
	v, _ := meta[key].(bool)
	return v
}
