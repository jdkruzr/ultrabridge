// Package syncsvc is the service layer for device sync: it turns a /sync/v1
// request into an ApplyBatch + relay pull, owning envelope validation and the
// wire DTO. It decouples the HTTP handler (synchttp) from the store (syncstore),
// mirroring internal/service's boundary rule. See
// docs/sync/forestnote-sync-protocol.md.
package syncsvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/sysop/ultrabridge/internal/syncstore"
)

// ProtocolVersion is the wire version this server speaks (spec §8).
const ProtocolVersion = 1

// Sentinel errors mapped to HTTP status by synchttp (spec §7.1).
var (
	ErrBadRequest         = errors.New("bad request")                  // 400
	ErrSchemaMismatch     = errors.New("schema hash mismatch")         // 409
	ErrUnsupportedVersion = errors.New("unsupported protocol version") // 409
)

// MaxDeviceNameLen caps the stored device_name (in runes). Over-long names are
// truncated, never rejected — a cosmetic label must not be able to brick sync.
const MaxDeviceNameLen = 128

// Request / Response are the /sync/v1 wire envelope (spec §4).
type Request struct {
	ProtocolVersion int    `json:"protocol_version"`
	SchemaHash      string `json:"schema_hash"`
	SiteID          string `json:"site_id"`
	// DeviceName is an OPTIONAL human-readable label (spec §4) shown in the
	// device-management UI. Absent/empty preserves any stored name.
	DeviceName string         `json:"device_name,omitempty"`
	Cursor     int64          `json:"cursor"`
	Ops        []syncstore.Op `json:"ops"`
}

type Response struct {
	ProtocolVersion int                    `json:"protocol_version"`
	AcceptedThrough int64                  `json:"accepted_through"`
	Rejected        []syncstore.RejectedOp `json:"rejected"`
	Ops             []syncstore.Op         `json:"ops"`
	Cursor          int64                  `json:"cursor"`
	HasMore         bool                   `json:"has_more"`
}

// Store is the subset of syncstore the service needs (satisfied by *syncstore.Store).
type Store interface {
	ApplyBatch(ctx context.Context, siteID string, ops []syncstore.Op) (syncstore.ApplyResult, error)
	OpsSince(ctx context.Context, cursor int64, excludeSite string, limit int) ([]syncstore.Op, int64, bool, error)
	RecordCursor(ctx context.Context, siteID string, lastPullSeq int64, deviceName string) error
}

// Bridge is notified of pages whose live content changed, so it can re-render →
// OCR → index → embed off the sync path (Phase 2). nil in Phase 1 (no-op).
type Bridge interface {
	PagesChanged(ctx context.Context, pages []syncstore.TablePK)
}

// Service handles /sync/v1.
type Service struct {
	store      Store
	bridge     Bridge // may be nil
	batchLimit int
	logger     *slog.Logger
}

func New(store Store, batchLimit int, bridge Bridge, logger *slog.Logger) *Service {
	if batchLimit <= 0 {
		batchLimit = 500
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{store: store, bridge: bridge, batchLimit: batchLimit, logger: logger}
}

// Sync ingests the request's ops, then returns this device's accepted_through,
// any rejected ops, and the relay ops it has not yet seen.
func (s *Service) Sync(ctx context.Context, req Request) (Response, error) {
	if req.ProtocolVersion != ProtocolVersion {
		return Response{}, fmt.Errorf("%w: got %d, want %d", ErrUnsupportedVersion, req.ProtocolVersion, ProtocolVersion)
	}
	if !syncstore.AcceptsSchemaHash(req.SchemaHash) {
		return Response{}, fmt.Errorf("%w", ErrSchemaMismatch)
	}
	if !syncstore.IsULID(req.SiteID) {
		return Response{}, fmt.Errorf("%w: site_id is not a ULID", ErrBadRequest)
	}
	if req.Cursor < 0 {
		return Response{}, fmt.Errorf("%w: cursor must be >= 0", ErrBadRequest)
	}

	applyRes, err := s.store.ApplyBatch(ctx, req.SiteID, req.Ops)
	if err != nil {
		return Response{}, err // → 500
	}

	ops, newCursor, hasMore, err := s.store.OpsSince(ctx, req.Cursor, req.SiteID, s.batchLimit)
	if err != nil {
		return Response{}, err
	}

	// Best-effort bookkeeping; the wire is client-driven so a failure is non-fatal.
	if err := s.store.RecordCursor(ctx, req.SiteID, newCursor, normalizeDeviceName(req.DeviceName)); err != nil {
		s.logger.Warn("sync: record cursor failed", "site", req.SiteID, "err", err)
	}

	// Hand changed pages to the bridge off the request's critical path (Phase 2).
	if s.bridge != nil && len(applyRes.ChangedPages) > 0 {
		s.bridge.PagesChanged(ctx, applyRes.ChangedPages)
	}

	rejected := applyRes.Rejected
	if rejected == nil {
		rejected = []syncstore.RejectedOp{} // emit [] not null
	}
	if ops == nil {
		ops = []syncstore.Op{}
	}

	return Response{
		ProtocolVersion: ProtocolVersion,
		AcceptedThrough: applyRes.AcceptedThrough,
		Rejected:        rejected,
		Ops:             ops,
		Cursor:          newCursor,
		HasMore:         hasMore,
	}, nil
}

// normalizeDeviceName trims the optional envelope label and truncates it to
// MaxDeviceNameLen runes (rune-wise so a multibyte name is never cut mid-
// character). Returns "" for an absent/blank name, which RecordCursor treats
// as "preserve the stored name".
func normalizeDeviceName(name string) string {
	name = strings.TrimSpace(name)
	if r := []rune(name); len(r) > MaxDeviceNameLen {
		return string(r[:MaxDeviceNameLen])
	}
	return name
}
