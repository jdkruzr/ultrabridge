package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeStore is an in-memory SettingStore for middleware tests.
type fakeStore struct {
	m        map[string]string
	setCalls int
}

func newFakeStore() *fakeStore { return &fakeStore{m: map[string]string{}} }

func (f *fakeStore) Get(_ context.Context, key string) (string, error) { return f.m[key], nil }
func (f *fakeStore) Set(_ context.Context, key, val string) error {
	f.m[key] = val
	f.setCalls++
	return nil
}

// TestMiddlewareValidToken verifies a valid token passes through, exposes the
// userId in context, and harvests it into the store on first sight.
// Verifies: spc-phase-1.AC2.2
func TestMiddlewareValidToken(t *testing.T) {
	store := newFakeStore()
	var nextCalled bool
	var gotUID string
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		nextCalled = true
		gotUID = UserID(r.Context())
	})

	h := Middleware(testSecret, store, next)
	req := httptest.NewRequest(http.MethodPost, "/api/user/query", nil)
	req.Header.Set("x-access-token", Mint("uid123", testSecret))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if !nextCalled {
		t.Fatalf("expected next handler to be called")
	}
	if gotUID != "uid123" {
		t.Errorf("UserID(ctx): got %q, want uid123", gotUID)
	}
	if store.m[UserIDSettingKey] != "uid123" {
		t.Errorf("expected harvested spc_user_id=uid123, got %q", store.m[UserIDSettingKey])
	}
}

// TestMiddlewareRejectsBadToken verifies a missing/garbage token yields a
// success:false envelope and does not call next.
// Verifies: spc-phase-1.AC2.2
func TestMiddlewareRejectsBadToken(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
	}{
		{"missing", ""},
		{"garbage", "not.a.jwt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			var nextCalled bool
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { nextCalled = true })

			h := Middleware(testSecret, store, next)
			req := httptest.NewRequest(http.MethodPost, "/api/user/query", nil)
			if tc.token != "" {
				req.Header.Set("x-access-token", tc.token)
			}
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			if nextCalled {
				t.Errorf("next must not be called on bad token")
			}
			var env struct {
				Success bool `json:"success"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("unmarshal envelope: %v (body=%s)", err, rec.Body.String())
			}
			if env.Success {
				t.Errorf("expected success=false envelope, got %s", rec.Body.String())
			}
		})
	}
}

// TestMiddlewareHarvestNoOpWhenSet verifies the userId harvest does not
// overwrite an already-persisted spc_user_id.
func TestMiddlewareHarvestNoOpWhenSet(t *testing.T) {
	store := newFakeStore()
	store.m[UserIDSettingKey] = "existing-id"
	store.setCalls = 0

	var gotUID string
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotUID = UserID(r.Context())
	})
	h := Middleware(testSecret, store, next)
	req := httptest.NewRequest(http.MethodPost, "/api/user/query", nil)
	req.Header.Set("x-access-token", Mint("a-different-id", testSecret))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if gotUID != "existing-id" {
		t.Errorf("UserID(ctx) should use canonical spc_user_id: got %q", gotUID)
	}
	if store.m[UserIDSettingKey] != "existing-id" {
		t.Errorf("harvest must not overwrite existing spc_user_id, got %q", store.m[UserIDSettingKey])
	}
	if store.setCalls != 0 {
		t.Errorf("expected no Set calls when spc_user_id already present, got %d", store.setCalls)
	}
}
