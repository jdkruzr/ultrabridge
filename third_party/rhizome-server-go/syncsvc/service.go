// Package syncsvc is the service layer for device sync: it turns a /sync/v1 request into an
// ApplyBatch + relay pull, owning envelope validation and the wire DTO, and decoupling the HTTP
// handler (synchttp) from the relay (syncstore). See spec/protocol.md §I.6.
package syncsvc

import (
	"errors"
	"fmt"

	"github.com/jdkruzr/rhizome/server-go/syncstore"
)

// ProtocolVersion is the wire version this server speaks.
const ProtocolVersion = 1

// Sentinel errors mapped to HTTP status by synchttp.
var (
	ErrBadRequest         = errors.New("bad request")                  // 400
	ErrSchemaMismatch     = errors.New("schema hash mismatch")         // 409
	ErrUnsupportedVersion = errors.New("unsupported protocol version") // 409
)

// Request / Response are the /sync/v1 wire envelope (spec/protocol.md §I.6).
type Request struct {
	ProtocolVersion int            `json:"protocol_version"`
	SchemaHash      string         `json:"schema_hash"`
	SiteID          string         `json:"site_id"`
	Cursor          int64          `json:"cursor"`
	Ops             []syncstore.Op `json:"ops"`
}

type Response struct {
	ProtocolVersion int                    `json:"protocol_version"`
	AcceptedThrough int64                  `json:"accepted_through"`
	Rejected        []syncstore.RejectedOp `json:"rejected"`
	Ops             []syncstore.Op         `json:"ops"`
	Cursor          int64                  `json:"cursor"`
	HasMore         bool                   `json:"has_more"`
}

// Store is the subset of the relay the service needs (satisfied by *syncstore.Store).
type Store interface {
	ApplyBatch(siteID string, ops []syncstore.Op) syncstore.ApplyResult
	OpsSince(cursor int64, excludeSite string, limit int) ([]syncstore.Op, int64, bool)
}

// Service handles /sync/v1 against a relay store and a configured set of accepted schema hashes
// (the current hash plus any grace-window hashes — spec/schema-evolution.md).
type Service struct {
	store      Store
	accepted   map[string]bool
	batchLimit int
}

// New builds a Service. acceptedHashes is the set the server will sync against (current + grace).
func New(store Store, acceptedHashes []string, batchLimit int) *Service {
	if batchLimit <= 0 {
		batchLimit = 500
	}
	set := make(map[string]bool, len(acceptedHashes))
	for _, h := range acceptedHashes {
		set[h] = true
	}
	return &Service{store: store, accepted: set, batchLimit: batchLimit}
}

// Sync ingests the request's ops, then returns this device's accepted_through, any rejected ops,
// and the relay ops it has not yet seen (authored by other sites).
func (s *Service) Sync(req Request) (Response, error) {
	if req.ProtocolVersion != ProtocolVersion {
		return Response{}, fmt.Errorf("%w: got %d, want %d", ErrUnsupportedVersion, req.ProtocolVersion, ProtocolVersion)
	}
	if !s.accepted[req.SchemaHash] {
		return Response{}, ErrSchemaMismatch
	}
	if !syncstore.IsULID(req.SiteID) {
		return Response{}, fmt.Errorf("%w: site_id is not a ULID", ErrBadRequest)
	}
	if req.Cursor < 0 {
		return Response{}, fmt.Errorf("%w: cursor must be >= 0", ErrBadRequest)
	}

	applyRes := s.store.ApplyBatch(req.SiteID, req.Ops)
	ops, newCursor, hasMore := s.store.OpsSince(req.Cursor, req.SiteID, s.batchLimit)

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
