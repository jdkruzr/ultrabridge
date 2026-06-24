package remarkable

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/sysop/ultrabridge/internal/search"
)

const (
	remarkablePathPrefix = "remarkable://"
	metadataPage         = -1
)

type pageIndexer interface {
	IndexPage(ctx context.Context, path string, pageIdx int, source, bodyText, titleText, keywords string) error
}

type contentDeleter interface {
	Delete(ctx context.Context, path string) error
}

type contentLister interface {
	GetContentByPrefix(ctx context.Context, likePattern string) (map[string]search.NoteDocument, error)
}

// metadataIndexer keeps the synced reMarkable tree visible to keyword search
// before page rendering/OCR exists. It writes a reserved metadata row per
// document, leaving real page slots free for the future renderer.
type metadataIndexer struct {
	store   *store
	indexer pageIndexer
	logger  *slog.Logger
}

func newMetadataIndexer(st *store, indexer pageIndexer, logger *slog.Logger) *metadataIndexer {
	if st == nil || indexer == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &metadataIndexer{store: st, indexer: indexer, logger: logger}
}

func (m *metadataIndexer) indexAll(ctx context.Context) error {
	if m == nil {
		return nil
	}
	docs, err := m.store.listDocumentTree(ctx)
	if err != nil {
		return err
	}
	byID := make(map[string]Document, len(docs))
	for _, doc := range docs {
		byID[doc.ID] = doc
	}

	active := map[string]bool{}
	for _, doc := range docs {
		if doc.Type == "folder" {
			continue
		}
		path := remarkablePath(doc.ID)
		active[path] = true
		if err := m.indexer.IndexPage(ctx, path, metadataPage, "remarkable", metadataBody(doc, byID), doc.Name, metadataKeywords(doc, byID)); err != nil {
			return fmt.Errorf("index %s metadata: %w", doc.ID, err)
		}
	}
	if err := m.pruneStale(ctx, active); err != nil {
		m.logger.Warn("remarkable metadata prune failed", "error", err)
	}
	return nil
}

func (m *metadataIndexer) deleteDocument(ctx context.Context, id string) error {
	if m == nil {
		return nil
	}
	deleter, ok := m.indexer.(contentDeleter)
	if !ok {
		return nil
	}
	return deleter.Delete(ctx, remarkablePath(id))
}

func (m *metadataIndexer) pruneStale(ctx context.Context, active map[string]bool) error {
	lister, ok := m.indexer.(contentLister)
	if !ok {
		return nil
	}
	deleter, ok := m.indexer.(contentDeleter)
	if !ok {
		return nil
	}
	existing, err := lister.GetContentByPrefix(ctx, remarkablePathPrefix+"%")
	if err != nil {
		return err
	}
	for path := range existing {
		if !active[path] {
			if err := deleter.Delete(ctx, path); err != nil {
				return err
			}
		}
	}
	return nil
}

func metadataBody(doc Document, byID map[string]Document) string {
	parts := []string{doc.Name}
	folder := folderPath(doc.Parent, byID)
	if folder != "" {
		parts = append(parts, "Folder: "+folder)
	}
	if doc.PageCount > 0 {
		parts = append(parts, "Pages: "+strconv.Itoa(doc.PageCount))
	}
	return strings.Join(parts, "\n")
}

func metadataKeywords(doc Document, byID map[string]Document) string {
	parts := []string{doc.Name, folderPath(doc.Parent, byID)}
	if doc.PageCount > 0 {
		parts = append(parts, strconv.Itoa(doc.PageCount)+" pages")
	}
	return strings.Join(parts, " ")
}

func folderPath(parentID string, byID map[string]Document) string {
	var names []string
	seen := map[string]bool{}
	for parentID != "" && !seen[parentID] {
		seen[parentID] = true
		parent, ok := byID[parentID]
		if !ok {
			break
		}
		if parent.Name != "" {
			names = append([]string{parent.Name}, names...)
		}
		parentID = parent.Parent
	}
	return strings.Join(names, " / ")
}

func remarkablePath(id string) string {
	return remarkablePathPrefix + id
}
