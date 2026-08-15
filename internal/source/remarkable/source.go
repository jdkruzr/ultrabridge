package remarkable

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/sysop/ultrabridge/internal/source"
)

// Source implements source.Source for the reMarkable sync server.
type Source struct {
	name     string
	cfg      Config
	db       *sql.DB
	deps     source.SharedDeps
	store    *store
	protocol *protocol
	hub      *hub
	indexer  *metadataIndexer
	ocr      *ocrProcessor
}

// NewSource constructs a reMarkable source from a source row and dependencies.
func NewSource(db *sql.DB, row source.SourceRow, deps source.SharedDeps) (*Source, error) {
	var cfg Config
	if err := json.Unmarshal([]byte(row.ConfigJSON), &cfg); err != nil {
		return nil, fmt.Errorf("parse remarkable config: %w", err)
	}
	if strings.TrimSpace(cfg.DataPath) == "" {
		return nil, fmt.Errorf("parse remarkable config: data_path is required")
	}
	return &Source{name: row.Name, cfg: cfg, db: db, deps: deps}, nil
}

func (s *Source) Type() string { return "remarkable" }
func (s *Source) Name() string { return s.name }

func (s *Source) Start(ctx context.Context) error {
	if err := migrate(ctx, s.db); err != nil {
		return err
	}
	s.store = newStore(s.db, s.cfg.DataPath)
	if err := s.store.ensurePaths(); err != nil {
		return fmt.Errorf("remarkable ensure paths: %w", err)
	}
	logger := s.deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	s.hub = newHub(logger)
	s.indexer = newMetadataIndexer(s.store, s.deps.Indexer, logger)
	if s.indexer != nil {
		if err := s.indexer.indexAll(ctx); err != nil {
			logger.Warn("remarkable metadata indexing failed", "error", err)
		}
	}
	s.ocr = newOCRProcessor(s.store, ocrDeps{
		indexer:    s.deps.Indexer,
		ocrClient:  s.deps.OCRClient,
		embedder:   s.deps.Embedder,
		embedStore: s.deps.EmbedStore,
		embedModel: s.deps.EmbedModel,
	}, logger)
	if s.ocr != nil {
		s.ocr.Start(ctx)
	}
	s.protocol = newProtocol(s.cfg, s.store, logger, s.hub, s.indexer, s.ocr)
	return nil
}

func (s *Source) Stop() {
	if s.ocr != nil {
		s.ocr.Stop()
	}
	if s.hub != nil {
		s.hub.close()
	}
}

// RegisterRoutes mounts the device-facing reMarkable protocol surface.
func (s *Source) RegisterRoutes(mux *http.ServeMux) {
	if s.protocol != nil {
		s.protocol.register(mux)
	}
}

// Devices lists the known paired devices for the single shared account.
func (s *Source) Devices(ctx context.Context) ([]DeviceRow, error) {
	if s.store == nil {
		return nil, fmt.Errorf("remarkable source not started")
	}
	return s.store.listDevices(ctx)
}

// SetDeviceLabel sets (or clears, on an empty label) the operator's name for a
// paired device. Distinct from the device-reported description, which the
// pairing heartbeat overwrites. Returns whether the device existed.
func (s *Source) SetDeviceLabel(ctx context.Context, deviceID, label string) (bool, error) {
	if s.store == nil {
		return false, fmt.Errorf("remarkable source not started")
	}
	return s.store.setDeviceLabel(ctx, deviceID, label)
}

// ListDocuments returns the synced document/folder tree (read-only). It reads
// the modern sync-v3 blob hashtree when present and falls back to the legacy
// document-storage v2 metadata table otherwise.
func (s *Source) ListDocuments(ctx context.Context) ([]Document, error) {
	if s.store == nil {
		return nil, fmt.Errorf("remarkable source not started")
	}
	return s.store.listDocumentTree(ctx)
}

// RenderDocument resolves the synced blob bundle needed to render one document.
func (s *Source) RenderDocument(ctx context.Context, documentID string) (RenderDocument, error) {
	if s.store == nil {
		return RenderDocument{}, fmt.Errorf("remarkable source not started")
	}
	return s.store.renderDocument(ctx, documentID)
}

// afterServerMutation runs the same post-commit hooks a device-driven root
// commit gets — FTS/OCR refresh, then a SyncComplete fan-out with no
// originating device so every connected tablet pulls the new root. With no
// devices connected this is a no-op; the next sync reconciles.
func (s *Source) afterServerMutation(ctx context.Context) {
	if s.protocol != nil {
		s.protocol.refreshMetadataIndex(ctx)
	}
	if s.hub != nil {
		s.hub.notifySync("remarkable", "", "UltraBridge")
	}
}

// UploadDocument authors a new document from a PDF or EPUB payload and
// commits it into the synced hashtree. filename supplies both the visible
// name (extension stripped) and the file type; parentID is "" for My files
// or a folder's document ID.
func (s *Source) UploadDocument(ctx context.Context, filename, parentID string, payload io.Reader) (Document, error) {
	if s.store == nil {
		return Document{}, fmt.Errorf("remarkable source not started")
	}
	ext := strings.ToLower(filepath.Ext(filename))
	var fileType string
	switch ext {
	case ".pdf":
		fileType = "pdf"
	case ".epub":
		fileType = "epub"
	default:
		return Document{}, fmt.Errorf("%w: %q", ErrUnsupportedFile, ext)
	}
	name := strings.TrimSpace(strings.TrimSuffix(filepath.Base(filename), ext))
	if name == "" {
		return Document{}, fmt.Errorf("%w: empty document name", ErrUnsupportedFile)
	}

	payloadHash, payloadSize, err := s.store.stageBlobStream(ctx, payload)
	if err != nil {
		return Document{}, err
	}
	if payloadSize == 0 {
		return Document{}, fmt.Errorf("%w: empty payload", ErrUnsupportedFile)
	}
	docID := uuid.New().String()
	if err := s.store.createDocument(ctx, docID, name, parentID, fileType, payloadHash, payloadSize); err != nil {
		return Document{}, err
	}
	s.afterServerMutation(ctx)
	return Document{ID: docID, Name: name, Type: "document", Parent: parentID, FileType: fileType}, nil
}

// DownloadDocument streams a document's original payload (the PDF or EPUB it
// was created from — annotations are not baked in). Notebooks have no
// payload and return ErrNoPayload.
func (s *Source) DownloadDocument(ctx context.Context, documentID string) (io.ReadCloser, string, string, error) {
	if s.store == nil {
		return nil, "", "", fmt.Errorf("remarkable source not started")
	}
	doc, err := s.store.renderDocument(ctx, documentID)
	if errors.Is(err, errDocumentNotFound) {
		return nil, "", "", ErrNotFound
	}
	if err != nil {
		return nil, "", "", err
	}
	path, ext, contentType := doc.PDFPath, ".pdf", "application/pdf"
	if path == "" {
		path, ext, contentType = doc.EPUBPath, ".epub", "application/epub+zip"
	}
	if path == "" {
		return nil, "", "", ErrNoPayload
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, "", "", err
	}
	name := strings.TrimSpace(doc.Name)
	if name == "" {
		name = documentID
	}
	return f, name + ext, contentType, nil
}

// DeleteDocument moves a document or empty folder to the tablet's trash
// (parent "trash") and drops it from UB's listing and search index. The
// tablet can restore it from its own trash screen.
func (s *Source) DeleteDocument(ctx context.Context, documentID string) error {
	if s.store == nil {
		return fmt.Errorf("remarkable source not started")
	}
	if err := s.store.trashNode(ctx, documentID); err != nil {
		return err
	}
	if s.protocol != nil {
		s.protocol.deleteMetadataIndex(ctx, documentID)
	}
	s.afterServerMutation(ctx)
	return nil
}

// CreateFolder authors a new folder under parentID ("" = My files).
func (s *Source) CreateFolder(ctx context.Context, name, parentID string) (Document, error) {
	if s.store == nil {
		return Document{}, fmt.Errorf("remarkable source not started")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return Document{}, fmt.Errorf("remarkable: folder name is required")
	}
	docID := uuid.New().String()
	if err := s.store.createFolder(ctx, docID, name, parentID); err != nil {
		return Document{}, err
	}
	s.afterServerMutation(ctx)
	return Document{ID: docID, Name: name, Type: "folder", Parent: parentID}, nil
}

// MoveNode re-parents a document or folder ("" = My files).
func (s *Source) MoveNode(ctx context.Context, documentID, newParentID string) error {
	if s.store == nil {
		return fmt.Errorf("remarkable source not started")
	}
	if err := s.store.moveNode(ctx, documentID, newParentID); err != nil {
		return err
	}
	s.afterServerMutation(ctx)
	return nil
}

// RenameNode sets a document or folder's visible name.
func (s *Source) RenameNode(ctx context.Context, documentID, newName string) error {
	if s.store == nil {
		return fmt.Errorf("remarkable source not started")
	}
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return fmt.Errorf("remarkable: name is required")
	}
	if err := s.store.renameNode(ctx, documentID, newName); err != nil {
		return err
	}
	s.afterServerMutation(ctx)
	return nil
}

// ReprocessDocument forces all renderable pages in a document through the
// server-side fulltext OCR backend.
func (s *Source) ReprocessDocument(ctx context.Context, documentID string) error {
	if s.ocr == nil {
		return fmt.Errorf("remarkable OCR is not configured")
	}
	return s.ocr.ReprocessDocument(ctx, documentID)
}

// OCRStatus returns the render-to-fulltext queue snapshot.
func (s *Source) OCRStatus(ctx context.Context) (OCRQueueStatus, error) {
	if s.ocr == nil {
		return OCRQueueStatus{}, nil
	}
	return s.ocr.Status(ctx)
}
