package service

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

	"github.com/sysop/ultrabridge/internal/digeststore"
)

// DigestStore is the slice of the canonical digest store the web layer needs:
// the read path (ListItems/ListGroups) plus the delete path (GetItem to resolve
// the owning user + sourceType + uniqueIdentifier, then SoftDelete).
// *digeststore.Store satisfies it. (Kept narrow so the service depends only on
// what it uses, mirroring the other store interfaces here.)
type DigestStore interface {
	ListItems(ctx context.Context, parentUID, tag string, page, size int) ([]digeststore.Digest, int64, error)
	ListGroups(ctx context.Context) ([]digeststore.Digest, error)
	GetItem(ctx context.Context, id int64) (*digeststore.Digest, error)
	SoftDelete(ctx context.Context, userID, id int64) error
}

// DigestDeindexer drops a digest from the shared FTS5/RAG index on delete so a
// web-deleted digest stops surfacing in search and chat. *digestindex.Bridge
// satisfies it (fire-and-forget; the bridge processes off the request path).
type DigestDeindexer interface {
	Deindex(uid string)
}

// DigestTombstoneQueue durably records a DELETE_DIGEST tombstone for the device
// so a UB/web-initiated delete propagates down even if the device is offline:
// the digest socket layer drains the queue on the device's next heartbeat
// (without it the device re-asserts the digest). The real SPC server queues
// these per socket-session in Redis; UB persists them per device-user.
// *spcserver/digesttomb.Store satisfies it structurally, so this package never
// imports spcserver.
type DigestTombstoneQueue interface {
	Enqueue(ctx context.Context, userID, digestID int64, dataType string) error
}

type digestService struct {
	store     DigestStore
	deindexer DigestDeindexer      // optional; nil skips search/RAG de-index
	tombstone DigestTombstoneQueue // optional; nil skips the device tombstone
	logger    *slog.Logger
}

// NewDigestService wraps a digest store for the web layer. deindexer may be nil
// (search/RAG de-index on delete is then skipped). Returns nil if store is nil
// so callers can gate the Digests tab/nav on a nil service.
func NewDigestService(store DigestStore, deindexer DigestDeindexer) DigestService {
	if store == nil {
		return nil
	}
	return &digestService{store: store, deindexer: deindexer, logger: slog.Default()}
}

// SetTombstoneQueue wires the durable device-tombstone seam after construction
// (the queue is built later in main, in SPC server mode).
func (s *digestService) SetTombstoneQueue(q DigestTombstoneQueue) { s.tombstone = q }

// DeleteDigest soft-deletes a digest, drops it from search/RAG, and enqueues a
// DELETE_DIGEST tombstone so the device removes its local copy on its next
// heartbeat (D2 — durable so it survives the device being offline). The
// soft-delete is authoritative (its error propagates); de-index and the
// tombstone enqueue are best-effort.
func (s *digestService) DeleteDigest(ctx context.Context, id int64) error {
	d, err := s.store.GetItem(ctx, id)
	if err != nil {
		return err // ErrNotFound or a real read error; web maps to 404/500
	}
	if err := s.store.SoftDelete(ctx, d.UserID, id); err != nil {
		return err
	}
	if s.deindexer != nil {
		s.deindexer.Deindex(d.UniqueIdentifier)
	}
	if s.tombstone != nil {
		if err := s.tombstone.Enqueue(ctx, d.UserID, id, strconv.Itoa(d.SourceType)); err != nil {
			s.logger.Warn("digest delete tombstone enqueue", "id", id, "error", err)
		}
	}
	return nil
}

func (s *digestService) ListDigests(ctx context.Context, group, tag string, page, perPage int) ([]DigestView, int, error) {
	rows, total, err := s.store.ListItems(ctx, group, tag, page, perPage)
	if err != nil {
		return nil, 0, err
	}
	// Resolve parent group uid → name for display (best-effort; an unknown uid
	// falls back to the raw uid).
	groupNames := map[string]string{}
	if groups, gerr := s.store.ListGroups(ctx); gerr == nil {
		for i := range groups {
			groupNames[groups[i].UniqueIdentifier] = groups[i].Name
		}
	}
	out := make([]DigestView, 0, len(rows))
	for i := range rows {
		out = append(out, toDigestView(&rows[i], groupNames))
	}
	return out, int(total), nil
}

func (s *digestService) ListGroups(ctx context.Context) ([]DigestGroupView, error) {
	groups, err := s.store.ListGroups(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]DigestGroupView, 0, len(groups))
	for i := range groups {
		out = append(out, DigestGroupView{UID: groups[i].UniqueIdentifier, Name: groups[i].Name})
	}
	return out, nil
}

func toDigestView(d *digeststore.Digest, groupNames map[string]string) DigestView {
	group := ""
	if d.ParentUniqueIdentifier != "" {
		if name, ok := groupNames[d.ParentUniqueIdentifier]; ok && name != "" {
			group = name
		} else {
			group = d.ParentUniqueIdentifier
		}
	}
	return DigestView{
		ID:             d.ID,
		Name:           d.Name,
		Excerpt:        d.Content,
		Comment:        d.CommentStr,
		Tags:           splitTags(d.Tags),
		Group:          group,
		SourceLabel:    sourceTypeLabel(d.SourceType),
		HasHandwriting: d.HandwriteInnerName != "",
		CreatedAt:      d.CreationTime,
		ModifiedAt:     d.LastModifiedTime,
	}
}

// sourceTypeLabel maps the SPC sourceType (1=PDF, 2=Note) to a label.
func sourceTypeLabel(t int) string {
	switch t {
	case 1:
		return "PDF"
	case 2:
		return "Note"
	default:
		return ""
	}
}

func splitTags(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
