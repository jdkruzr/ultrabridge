package auth

import (
	"context"
	"net/http"

	"github.com/sysop/ultrabridge/internal/spcserver/envelope"
)

// UserIDSettingKey is the notedb settings key under which the device's userId
// is persisted once harvested. Runtime-managed (not an appconfig key).
const UserIDSettingKey = "spc_user_id"

// SettingStore is the minimal persistence the middleware needs. Implemented
// over notedb in main wiring; a fake is used in tests. Keeping it tiny keeps
// the auth package import-light and unit-testable.
type SettingStore interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, val string) error
}

type ctxKey int

const userIDCtxKey ctxKey = 0

// UserID returns the verified userId placed in the context by Middleware, or ""
// if none.
func UserID(ctx context.Context) string {
	uid, _ := ctx.Value(userIDCtxKey).(string)
	return uid
}

// WithUserID returns a context carrying uid as the verified userId, the same key
// UserID reads. Used by Middleware and by tests that exercise protected handlers
// without minting a token.
func WithUserID(ctx context.Context, uid string) context.Context {
	return context.WithValue(ctx, userIDCtxKey, uid)
}

// CanonicalUserID maps a verified token userId onto UB's stored single-user
// identity. The first valid token still seeds spc_user_id, but once that setting
// exists it wins over later cached real-SPC tokens so all source data, sockets,
// and push queues stay in one user lane.
func CanonicalUserID(ctx context.Context, store SettingStore, verified string) string {
	if store == nil {
		return verified
	}
	existing, err := store.Get(ctx, UserIDSettingKey)
	if err != nil {
		return verified
	}
	if existing != "" {
		return existing
	}
	_ = store.Set(ctx, UserIDSettingKey, verified)
	return verified
}

// Middleware gates a handler on a valid x-access-token. On success it puts the
// canonical UB userId in the request context and, the first time it sees a valid
// token, harvests that userId into store under UserIDSettingKey. On failure it
// writes the SPC "not logged in" envelope (E0712), which prompts the device to
// re-login, and does not call next.
func Middleware(secret string, store SettingStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid, err := Verify(r.Header.Get("x-access-token"), secret)
		if err != nil {
			// E0712: "You are not logged in or your login has expired. Please
			// log in again!" (SPC error enum).
			envelope.WriteError(w, "E0712", "You are not logged in or your login has expired. Please log in again!")
			return
		}

		uid = CanonicalUserID(r.Context(), store, uid)

		next.ServeHTTP(w, r.WithContext(WithUserID(r.Context(), uid)))
	})
}
