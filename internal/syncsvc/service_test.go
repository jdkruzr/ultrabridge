package syncsvc

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sysop/ultrabridge/internal/syncstore"
)

const (
	siteA = "0000000000000000000000000A"
	siteB = "0000000000000000000000000B"
	nb1   = "00000000000000000000000NB1"
	pg1   = "00000000000000000000000PG1"
	st1   = "00000000000000000000000ST1"
)

func newSvc(t *testing.T, bridge Bridge, batchLimit int) *Service {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)",
		filepath.Join(t.TempDir(), "sync.db"))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := syncstore.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return New(syncstore.New(db), batchLimit, bridge, nil)
}

func nbOp(site string, opSeq, wall int64, name string) syncstore.Op {
	return syncstore.Op{
		Table: "notebook", PK: nb1, SiteID: site, OpSeq: opSeq, WallTS: wall,
		Cols: map[string]any{"name": name, "sort_order": float64(0), "created_at": float64(1000), "deleted_at": nil},
	}
}

func req(site string, cursor int64, ops ...syncstore.Op) Request {
	return Request{
		ProtocolVersion: ProtocolVersion,
		SchemaHash:      syncstore.SchemaHash(),
		SiteID:          site,
		Cursor:          cursor,
		Ops:             ops,
	}
}

func TestSync_RejectsBadEnvelope(t *testing.T) {
	svc := newSvc(t, nil, 0)
	ctx := context.Background()

	r := req(siteA, 0)
	r.ProtocolVersion = 2
	if _, err := svc.Sync(ctx, r); !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("bad version: want ErrUnsupportedVersion, got %v", err)
	}

	r = req(siteA, 0)
	r.SchemaHash = "deadbeef"
	if _, err := svc.Sync(ctx, r); !errors.Is(err, ErrSchemaMismatch) {
		t.Errorf("bad schema: want ErrSchemaMismatch, got %v", err)
	}

	r = req("not-a-ulid", 0)
	if _, err := svc.Sync(ctx, r); !errors.Is(err, ErrBadRequest) {
		t.Errorf("bad site_id: want ErrBadRequest, got %v", err)
	}

	r = req(siteA, -1)
	if _, err := svc.Sync(ctx, r); !errors.Is(err, ErrBadRequest) {
		t.Errorf("negative cursor: want ErrBadRequest, got %v", err)
	}
}

func TestSync_ApplyAndRelay(t *testing.T) {
	svc := newSvc(t, nil, 0)
	ctx := context.Background()

	// A pushes two ops.
	respA, err := svc.Sync(ctx, req(siteA, 0, nbOp(siteA, 1, 1000, "v1"), nbOp(siteA, 2, 2000, "v2")))
	if err != nil {
		t.Fatalf("A push: %v", err)
	}
	if respA.AcceptedThrough != 2 {
		t.Errorf("A accepted_through = %d, want 2", respA.AcceptedThrough)
	}
	if len(respA.Ops) != 0 {
		t.Errorf("A should receive no relay ops (only its own exist), got %d", len(respA.Ops))
	}

	// B pulls — sees A's ops, not its own (none).
	respB, err := svc.Sync(ctx, req(siteB, 0))
	if err != nil {
		t.Fatalf("B pull: %v", err)
	}
	if len(respB.Ops) != 2 || respB.HasMore {
		t.Errorf("B pull = %d ops has_more=%v, want 2/false", len(respB.Ops), respB.HasMore)
	}
	if respB.Cursor != 2 {
		t.Errorf("B cursor = %d, want 2", respB.Cursor)
	}
}

func TestSync_RejectedAndAcceptedThrough(t *testing.T) {
	svc := newSvc(t, nil, 0)
	ctx := context.Background()
	poison := syncstore.Op{Table: "notebook", PK: nb1, SiteID: siteA, OpSeq: 2, WallTS: 2000,
		Cols: map[string]any{"name": "x"}} // missing cols

	resp, err := svc.Sync(ctx, req(siteA, 0, nbOp(siteA, 1, 1000, "v1"), poison, nbOp(siteA, 3, 3000, "v3")))
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(resp.Rejected) != 1 || resp.Rejected[0].OpSeq != 2 {
		t.Errorf("want op 2 rejected, got %+v", resp.Rejected)
	}
	if resp.AcceptedThrough != 3 {
		t.Errorf("accepted_through = %d, want 3 (poison op counted)", resp.AcceptedThrough)
	}
}

func TestSync_EmitsEmptyRejectedSlice(t *testing.T) {
	svc := newSvc(t, nil, 0)
	resp, err := svc.Sync(context.Background(), req(siteA, 0, nbOp(siteA, 1, 1000, "v1")))
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	b, _ := json.Marshal(resp)
	if !strings.Contains(string(b), `"rejected":[]`) {
		t.Errorf("rejected should marshal to [] not null: %s", b)
	}
}

func TestSync_Paging(t *testing.T) {
	svc := newSvc(t, nil, 2) // batchLimit 2
	ctx := context.Background()
	svc.Sync(ctx, req(siteA, 0, nbOp(siteA, 1, 1000, "v1"), nbOp(siteA, 2, 2000, "v2"), nbOp(siteA, 3, 3000, "v3")))

	resp, err := svc.Sync(ctx, req(siteB, 0))
	if err != nil {
		t.Fatalf("B pull: %v", err)
	}
	if len(resp.Ops) != 2 || !resp.HasMore || resp.Cursor != 2 {
		t.Errorf("paged pull = %d ops has_more=%v cursor=%d, want 2/true/2", len(resp.Ops), resp.HasMore, resp.Cursor)
	}
}

type captureBridge struct{ pages []syncstore.TablePK }

func (c *captureBridge) PagesChanged(_ context.Context, pages []syncstore.TablePK) {
	c.pages = append(c.pages, pages...)
}

func TestSync_BridgeNotifiedOfChangedPages(t *testing.T) {
	cb := &captureBridge{}
	svc := newSvc(t, cb, 0)
	stroke := syncstore.Op{
		Table: "stroke", PK: st1, SiteID: siteA, OpSeq: 1, WallTS: 1000,
		Cols: map[string]any{
			"page_id": pg1, "color": float64(4278190080), "pen_width_min": float64(2),
			"pen_width_max": float64(6), "points": "MgAAADwAAADIAAAAAAAAAAEAAAA=",
			"z": float64(0), "created_at": float64(1000), "deleted_at": nil,
		},
	}
	if _, err := svc.Sync(context.Background(), req(siteA, 0, stroke)); err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(cb.pages) != 1 || cb.pages[0].PK != pg1 {
		t.Errorf("bridge pages = %+v, want [page %s]", cb.pages, pg1)
	}
}
