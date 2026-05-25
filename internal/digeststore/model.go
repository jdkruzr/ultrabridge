// Package digeststore is the canonical store for Supernote "digests" (the SPC
// API calls them "summary"): a user-curated saved excerpt from a note or
// document plus an optional handwritten ".mark" annotation.
//
// It is the digest analogue of taskdb: the SPC protocol layer
// (internal/spcserver/handlers.SummaryHandler) owns no storage and maps its
// wire DTOs through a narrow interface onto this store. The tables live in the
// shared notedb so the UB-native surfacing planned for a later phase (FTS /
// RAG / web) can read them directly; they are migrated package-locally in SPC
// server mode (precedent: fileids.Migrate / staging.Migrate), not by
// notedb.Open.
//
// The Digest model is faithful 1:1 to the real SPC SummaryDO so a digest
// created on-device round-trips losslessly: every field the device sends is
// persisted and served back unchanged. A single table holds both digest items
// and digest groups (libraries), distinguished by IsGroup — matching real SPC's
// t_summary.is_summary_group.
package digeststore

import "errors"

// ErrNotFound is returned when a digest or tag does not exist (or is
// soft-deleted / owned by another user).
var ErrNotFound = errors.New("digeststore: not found")

// Digest is one row of the digests table: either a digest item (IsGroup=false)
// or a digest group/library (IsGroup=true). Field semantics mirror the real SPC
// SummaryDO verbatim. Timestamps are millisecond UTC unix (0 = unset); FileID 0
// means unset.
type Digest struct {
	ID                     int64
	FileID                 int64
	UserID                 int64
	Name                   string
	UniqueIdentifier       string
	ParentUniqueIdentifier string
	Content                string
	SourcePath             string
	DataSource             string
	SourceType             int
	IsGroup                bool
	Description            string
	Tags                   string // comma-separated tag names
	MD5Hash                string
	Metadata               string // opaque JSON string
	CommentStr             string
	CommentHandwriteName   string
	HandwriteInnerName     string
	HandwriteMD5           string
	CreationTime           int64
	LastModifiedTime       int64
	Author                 string
	IsDeleted              bool
	CreatedAt              int64 // DB row creation (millis UTC)
	UpdatedAt              int64 // DB row last update (millis UTC)
}

// Tag is one row of the digest_tags table: a flat user-scoped label.
type Tag struct {
	ID        int64
	UserID    int64
	Name      string
	CreatedAt int64
}
