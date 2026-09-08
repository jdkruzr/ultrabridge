// Package syncsvc is the service layer for device sync: it turns a /sync/v1 request into an
// ApplyBatch + relay pull, owning envelope validation and the wire DTO, and decoupling the HTTP
// handler (synchttp) from the relay (syncstore). See spec/protocol.md §I.6.
package syncsvc

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jdkruzr/rhizome/server-go/bounded"
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
	if err := s.validate(req); err != nil {
		return Response{}, err
	}
	return s.syncValidated(req), nil
}

func (s *Service) validate(req Request) error {
	if req.ProtocolVersion != ProtocolVersion {
		return fmt.Errorf("%w: got %d, want %d", ErrUnsupportedVersion, req.ProtocolVersion, ProtocolVersion)
	}
	if !s.accepted[req.SchemaHash] {
		return ErrSchemaMismatch
	}
	if !syncstore.IsULID(req.SiteID) {
		return fmt.Errorf("%w: site_id is not a ULID", ErrBadRequest)
	}
	if req.Cursor < 0 {
		return fmt.Errorf("%w: cursor must be >= 0", ErrBadRequest)
	}
	return nil
}

func (s *Service) syncValidated(req Request) Response {
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
	}
}

func (s *Service) SyncBounded(req bounded.Request, limits bounded.Limits) ([]byte, error) {
	if err := s.validate(Request{ProtocolVersion: req.ProtocolVersion, SchemaHash: req.SchemaHash, SiteID: req.SiteID, Cursor: req.Cursor}); err != nil {
		return nil, err
	}
	if err := req.ValidateRows(limits); err != nil {
		return nil, err
	}
	store, ok := s.store.(interface {
		ExchangeBounded(string, int64, []syncstore.Op, bounded.Limits) ([]byte, error)
	})
	if !ok {
		return nil, bounded.Fail(503, "bounded_store_unavailable")
	}
	ops := make([]syncstore.Op, len(req.Ops))
	for i, raw := range req.Ops {
		if err := json.Unmarshal(raw, &ops[i]); err != nil {
			return nil, bounded.Fail(400, "invalid_op")
		}
	}
	return store.ExchangeBounded(req.SiteID, req.Cursor, ops, limits)
}
