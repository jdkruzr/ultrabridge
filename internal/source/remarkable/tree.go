package remarkable

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Document is a single node in the reMarkable document tree — a folder or a
// document, named, with a parent link for tree assembly. PageCount is 0 when
// unknown (folders, or legacy-sync documents whose page count isn't stored).
type Document struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Type      string `json:"type"` // "folder" | "document"
	Parent    string `json:"parent"`
	PageCount int    `json:"page_count"`
}

// indexEntry is one line of a hashtree index file:
// "<hash>:<type>:<entryName>:<subfiles>:<size>".
type indexEntry struct {
	Hash      string
	Type      string
	EntryName string
	Subfiles  int
	Size      int64
}

// rmMetadata is the subset of a document's .metadata blob we surface.
type rmMetadata struct {
	VisibleName string `json:"visibleName"`
	Type        string `json:"type"` // "DocumentType" | "CollectionType"
	Parent      string `json:"parent"`
	Deleted     bool   `json:"deleted"`
}

// rmContent is the subset of a document's .content blob we surface.
type rmContent struct {
	FileType  string      `json:"fileType"`
	PageCount int         `json:"pageCount"`
	Pages     []any       `json:"pages"`
	CPages    rmCPageList `json:"cPages"`
}

type rmCPageList struct {
	Pages []rmCPage `json:"pages"`
}

type rmCPage struct {
	ID string `json:"id"`
}

// RenderDocument is the resolved on-disk shape needed to render a reMarkable
// document from the sync-v3 blob hashtree.
type RenderDocument struct {
	ID            string
	Name          string
	Type          string
	FileType      string
	PageCount     int
	Revision      string
	PDFPath       string
	CacheDir      string
	PageRM        map[string]RenderBlob
	PageAssets    map[string][]RenderBlob
	PageOrder     []string
	Renderable    bool
	RenderableWhy string
}

// RenderBlob identifies one synced blob payload and its content-addressed hash.
type RenderBlob struct {
	Hash string
	Path string
}

// parseIndex parses a hashtree index file (schema v3 or v4). The first line is
// the schema version; v4 carries an extra summary line we skip. Faithful to the
// rmfakecloud format (internal/storage/models/hashtree.go).
func parseIndex(r []byte) ([]indexEntry, error) {
	sc := bufio.NewScanner(bytes.NewReader(r))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	if !sc.Scan() {
		return nil, nil // empty index
	}
	switch schema := strings.TrimSpace(sc.Text()); schema {
	case "4":
		if !sc.Scan() {
			return nil, fmt.Errorf("v4 index missing summary line")
		}
	case "3":
		// no summary line
	default:
		return nil, fmt.Errorf("unknown index schema %q", schema)
	}

	var entries []indexEntry
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) != 5 {
			return nil, fmt.Errorf("index entry has %d fields, want 5: %q", len(fields), line)
		}
		subfiles, err := strconv.Atoi(fields[3])
		if err != nil {
			return nil, fmt.Errorf("index entry subfiles %q: %w", line, err)
		}
		size, err := strconv.ParseInt(fields[4], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("index entry size %q: %w", line, err)
		}
		entries = append(entries, indexEntry{
			Hash:      fields[0],
			Type:      fields[1],
			EntryName: fields[2],
			Subfiles:  subfiles,
			Size:      size,
		})
	}
	return entries, sc.Err()
}

// listDocumentTree builds the document/folder listing. It prefers the modern
// sync-v3 blob hashtree (root -> top index -> per-doc sub-index -> .metadata/
// .content). When no root blob exists (or the top index isn't synced yet) it
// falls back to the legacy document-storage v2 metadata table.
func (s *store) listDocumentTree(ctx context.Context) ([]Document, error) {
	rootRec, err := s.getBlob(ctx, rootBlobID)
	if errors.Is(err, errBlobNotFound) {
		return s.legacyDocumentTree(ctx)
	}
	if err != nil {
		return nil, err
	}
	topHashRaw, err := osReadFile(rootRec.Path)
	if err != nil {
		return nil, err
	}
	topHash := strings.TrimSpace(string(topHashRaw))
	if topHash == "" {
		return s.legacyDocumentTree(ctx)
	}
	topData, err := s.readBlob(ctx, topHash)
	if err != nil {
		// Root points at an index we haven't received yet — sync in progress.
		return s.legacyDocumentTree(ctx)
	}
	topEntries, err := parseIndex(topData)
	if err != nil {
		return nil, fmt.Errorf("parse top index: %w", err)
	}

	docs := make([]Document, 0, len(topEntries))
	for _, docEntry := range topEntries {
		doc, ok, err := s.documentFromSubtree(ctx, docEntry)
		if err != nil {
			return nil, err
		}
		if ok {
			docs = append(docs, doc)
		}
	}
	sortDocuments(docs)
	return docs, nil
}

// documentFromSubtree resolves one document's sub-index and reads its
// .metadata (required) and .content (optional, for page count). ok is false
// when the subtree or metadata isn't available yet, or the document is deleted.
func (s *store) documentFromSubtree(ctx context.Context, docEntry indexEntry) (Document, bool, error) {
	subData, err := s.readBlob(ctx, docEntry.Hash)
	if err != nil {
		return Document{}, false, nil // subtree not synced yet — skip quietly
	}
	fileEntries, err := parseIndex(subData)
	if err != nil {
		return Document{}, false, fmt.Errorf("parse sub-index for %s: %w", docEntry.EntryName, err)
	}

	var metaHash, contentHash string
	for _, fe := range fileEntries {
		switch {
		case strings.HasSuffix(fe.EntryName, ".metadata"):
			metaHash = fe.Hash
		case strings.HasSuffix(fe.EntryName, ".content"):
			contentHash = fe.Hash
		}
	}
	if metaHash == "" {
		return Document{}, false, nil // can't name it without metadata
	}

	var meta rmMetadata
	if err := s.readJSONBlob(ctx, metaHash, &meta); err != nil {
		return Document{}, false, nil // metadata blob not synced yet — skip
	}
	if meta.Deleted {
		return Document{}, false, nil
	}

	doc := Document{
		ID:     docEntry.EntryName,
		Name:   meta.VisibleName,
		Type:   mapEntryType(meta.Type),
		Parent: meta.Parent,
	}
	if contentHash != "" {
		var content rmContent
		if err := s.readJSONBlob(ctx, contentHash, &content); err == nil {
			doc.PageCount = content.PageCount
			if doc.PageCount == 0 {
				doc.PageCount = len(content.Pages)
			}
		}
	}
	return doc, true, nil
}

// renderDocument resolves one document into the minimal file bundle required for
// page rendering. It only supports the modern sync-v3 hashtree because the
// legacy document-storage metadata table does not expose page payloads.
func (s *store) renderDocument(ctx context.Context, documentID string) (RenderDocument, error) {
	rootRec, err := s.getBlob(ctx, rootBlobID)
	if err != nil {
		if errors.Is(err, errBlobNotFound) {
			return RenderDocument{}, errDocumentNotFound
		}
		return RenderDocument{}, err
	}
	topHashRaw, err := osReadFile(rootRec.Path)
	if err != nil {
		return RenderDocument{}, err
	}
	topHash := strings.TrimSpace(string(topHashRaw))
	if topHash == "" {
		return RenderDocument{}, errDocumentNotFound
	}
	topData, err := s.readBlob(ctx, topHash)
	if err != nil {
		return RenderDocument{}, errDocumentNotFound
	}
	topEntries, err := parseIndex(topData)
	if err != nil {
		return RenderDocument{}, fmt.Errorf("parse top index: %w", err)
	}

	var docEntry indexEntry
	found := false
	for _, e := range topEntries {
		if e.EntryName == documentID {
			docEntry, found = e, true
			break
		}
	}
	if !found {
		return RenderDocument{}, errDocumentNotFound
	}

	subData, err := s.readBlob(ctx, docEntry.Hash)
	if err != nil {
		return RenderDocument{}, fmt.Errorf("read document subtree: %w", err)
	}
	fileEntries, err := parseIndex(subData)
	if err != nil {
		return RenderDocument{}, fmt.Errorf("parse sub-index for %s: %w", documentID, err)
	}

	filesByName := make(map[string]indexEntry, len(fileEntries))
	for _, fe := range fileEntries {
		filesByName[fe.EntryName] = fe
	}

	metaEntry, ok := filesByName[documentID+".metadata"]
	if !ok {
		return RenderDocument{}, errDocumentNotFound
	}
	var meta rmMetadata
	if err := s.readJSONBlob(ctx, metaEntry.Hash, &meta); err != nil {
		return RenderDocument{}, errDocumentNotFound
	}
	if meta.Deleted || mapEntryType(meta.Type) != "document" {
		return RenderDocument{}, errDocumentNotFound
	}

	out := RenderDocument{
		ID:         documentID,
		Name:       meta.VisibleName,
		Type:       "document",
		Revision:   docEntry.Hash,
		CacheDir:   filepath.Join(s.dataPath, "rendered"),
		PageRM:     map[string]RenderBlob{},
		PageAssets: map[string][]RenderBlob{},
	}

	if contentEntry, ok := filesByName[documentID+".content"]; ok {
		var content rmContent
		if err := s.readJSONBlob(ctx, contentEntry.Hash, &content); err == nil {
			out.FileType = content.FileType
			out.PageCount = content.PageCount
			out.PageOrder = renderPageOrder(content)
			if out.PageCount == 0 {
				out.PageCount = len(out.PageOrder)
			}
		}
	}

	if pdfEntry, ok := filesByName[documentID+".pdf"]; ok {
		if rec, err := s.getBlob(ctx, pdfEntry.Hash); err == nil {
			out.PDFPath = rec.Path
		}
	}

	prefix := documentID + "/"
	for _, fe := range fileEntries {
		if !strings.HasPrefix(fe.EntryName, prefix) {
			continue
		}
		rel := strings.TrimPrefix(fe.EntryName, prefix)
		pageID, suffix, ok := strings.Cut(rel, "/")
		if !ok {
			pageID = strings.TrimSuffix(rel, ".rm")
			suffix = ""
		}
		if pageID == "" {
			continue
		}
		rec, err := s.getBlob(ctx, fe.Hash)
		if err != nil {
			continue
		}
		blob := RenderBlob{Hash: fe.Hash, Path: rec.Path}
		if suffix == "" && strings.HasSuffix(rel, ".rm") {
			out.PageRM[pageID] = blob
		} else {
			out.PageAssets[pageID] = append(out.PageAssets[pageID], blob)
		}
	}

	if len(out.PageOrder) == 0 {
		for pageID := range out.PageRM {
			out.PageOrder = append(out.PageOrder, pageID)
		}
		sort.Strings(out.PageOrder)
	}
	if out.PageCount == 0 {
		out.PageCount = len(out.PageOrder)
	}
	out.Renderable = out.PDFPath != "" || len(out.PageRM) > 0
	if !out.Renderable {
		out.RenderableWhy = "no renderable PDF or page .rm blobs synced yet"
	}
	return out, nil
}

func renderPageOrder(content rmContent) []string {
	var out []string
	for _, p := range content.CPages.Pages {
		if p.ID != "" {
			out = append(out, p.ID)
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, raw := range content.Pages {
		switch v := raw.(type) {
		case string:
			if v != "" {
				out = append(out, v)
			}
		case map[string]any:
			if id, ok := v["id"].(string); ok && id != "" {
				out = append(out, id)
			}
		}
	}
	return out
}

// legacyDocumentTree maps the document-storage v2 metadata table to Documents.
// Page count isn't tracked on that path, so it stays 0.
func (s *store) legacyDocumentTree(ctx context.Context) ([]Document, error) {
	rows, err := s.listMetadata(ctx, "")
	if err != nil {
		return nil, err
	}
	docs := make([]Document, 0, len(rows))
	for _, m := range rows {
		docs = append(docs, Document{
			ID:     m.ID,
			Name:   m.VisibleName,
			Type:   mapEntryType(m.Type),
			Parent: m.Parent,
		})
	}
	sortDocuments(docs)
	return docs, nil
}

func (s *store) readBlob(ctx context.Context, blobID string) ([]byte, error) {
	rec, err := s.getBlob(ctx, blobID)
	if err != nil {
		return nil, err
	}
	return osReadFile(rec.Path)
}

func (s *store) readJSONBlob(ctx context.Context, blobID string, v any) error {
	data, err := s.readBlob(ctx, blobID)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func mapEntryType(t string) string {
	if t == "CollectionType" {
		return "folder"
	}
	return "document"
}

// sortDocuments yields a stable, human-friendly order: folders before
// documents, then case-insensitive by name, then by ID.
func sortDocuments(docs []Document) {
	sort.Slice(docs, func(i, j int) bool {
		a, b := docs[i], docs[j]
		if (a.Type == "folder") != (b.Type == "folder") {
			return a.Type == "folder"
		}
		an, bn := strings.ToLower(a.Name), strings.ToLower(b.Name)
		if an != bn {
			return an < bn
		}
		return a.ID < b.ID
	})
}
