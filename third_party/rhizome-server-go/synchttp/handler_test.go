package synchttp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jdkruzr/rhizome/server-go/auth"
	"github.com/jdkruzr/rhizome/server-go/registry"
	"github.com/jdkruzr/rhizome/server-go/syncstore"
	"github.com/jdkruzr/rhizome/server-go/syncsvc"
)

const (
	user = "dev"
	pass = "secret"
)

func newHandler() *Handler {
	store := syncstore.NewStore(registry.ForestNote().KnownCols())
	svc := syncsvc.New(store, []string{registry.ForestNote().SchemaHash()}, 500)
	return New(svc, auth.NewBasic(user, pass))
}

func strokeOp(site, pk string, opSeq, opTs int64) syncstore.Op {
	return syncstore.Op{
		Table: "stroke", PK: pk, SiteID: site, OpSeq: opSeq, OpTs: opTs,
		Cols: map[string]json.RawMessage{
			"page_id":       json.RawMessage(`"` + pk26("PAGE") + `"`),
			"color":         json.RawMessage(`4278190080`),
			"pen_width_min": json.RawMessage(`2`),
			"pen_width_max": json.RawMessage(`8`),
			"points":        json.RawMessage(`"AAEC"`),
			"z":             json.RawMessage(`5`),
			"created_at":    json.RawMessage(`100`),
			"deleted_at":    json.RawMessage(`null`),
		},
	}
}

func pk26(prefix string) string { return (prefix + strings.Repeat("0", 26))[:26] }

func post(t *testing.T, h *Handler, body []byte, withAuth bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/sync/v1", bytes.NewReader(body))
	if withAuth {
		req.SetBasicAuth(user, pass)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func marshalReq(t *testing.T, r syncsvc.Request) []byte {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestRejectsMissingAuth(t *testing.T) {
	rec := post(t, newHandler(), []byte(`{}`), false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("missing WWW-Authenticate challenge")
	}
}

func TestRejectsBadSchemaHash(t *testing.T) {
	body := marshalReq(t, syncsvc.Request{
		ProtocolVersion: 1, SchemaHash: "deadbeef", SiteID: pk26("AAAA"), Cursor: 0,
	})
	rec := post(t, newHandler(), body, true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func TestRejectsMalformedJSON(t *testing.T) {
	rec := post(t, newHandler(), []byte(`{not json`), true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestRejectsNonPost(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/sync/v1", nil)
	req.SetBasicAuth(user, pass)
	rec := httptest.NewRecorder()
	newHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestRoundTripPushThenRelayToOtherSite(t *testing.T) {
	h := newHandler()
	hash := registry.ForestNote().SchemaHash()
	siteA, siteB := pk26("AAAA"), pk26("BBBB")

	// Site A pushes one stroke op.
	pushBody := marshalReq(t, syncsvc.Request{
		ProtocolVersion: 1, SchemaHash: hash, SiteID: siteA, Cursor: 0,
		Ops: []syncstore.Op{strokeOp(siteA, pk26("S1"), 1, 100)},
	})
	rec := post(t, h, pushBody, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("push status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var pushResp syncsvc.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &pushResp); err != nil {
		t.Fatalf("decode push resp: %v", err)
	}
	if pushResp.AcceptedThrough != 1 {
		t.Fatalf("accepted_through = %d, want 1", pushResp.AcceptedThrough)
	}
	if len(pushResp.Rejected) != 0 {
		t.Fatalf("unexpected rejects: %v", pushResp.Rejected)
	}
	if len(pushResp.Ops) != 0 {
		t.Fatalf("A should not be relayed its own op; got %d", len(pushResp.Ops))
	}

	// Site B pulls and should receive A's op.
	pullBody := marshalReq(t, syncsvc.Request{
		ProtocolVersion: 1, SchemaHash: hash, SiteID: siteB, Cursor: 0,
	})
	rec = post(t, h, pullBody, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("pull status = %d, want 200", rec.Code)
	}
	var pullResp syncsvc.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &pullResp); err != nil {
		t.Fatalf("decode pull resp: %v", err)
	}
	if len(pullResp.Ops) != 1 || pullResp.Ops[0].PK != pk26("S1") {
		t.Fatalf("B should receive A's stroke; got %+v", pullResp.Ops)
	}
	if pullResp.Cursor != 1 || pullResp.HasMore {
		t.Fatalf("cursor = %d has_more = %v, want 1/false", pullResp.Cursor, pullResp.HasMore)
	}
	// Rejected/Ops must be [] not null on the wire.
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"rejected":[]`)) {
		t.Fatalf("rejected should serialize as [], body: %s", rec.Body.String())
	}
}
