package notify

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/sysop/ultrabridge/internal/spcserver/digesttomb"
)

// tombStore is the slice of digesttomb.Store the queue needs.
type tombStore interface {
	Pending(ctx context.Context, userID int64) ([]digesttomb.Tombstone, error)
	Ack(ctx context.Context, userID int64) error
}

// TombstoneQueue delivers durable digest-delete tombstones to a device over the
// "digest" socket event. It implements socketio's digest-queue seam: DrainDigest
// is called on each ratta_ping (emit the user's pending DELETE_DIGEST frames),
// AckDigest on the device's "Received" reply (clear the delivered rows). The
// device userId arrives as a string from the socket layer; the store keys on the
// numeric spc_user_id, so both convert here.
type TombstoneQueue struct {
	store  tombStore
	logger *slog.Logger
}

func NewTombstoneQueue(store tombStore, logger *slog.Logger) *TombstoneQueue {
	if logger == nil {
		logger = slog.Default()
	}
	return &TombstoneQueue{store: store, logger: logger}
}

// DrainDigest returns the combined DIGEST-SYN payload of the user's pending
// tombstones (and marks them sent), or ("", false) if there are none / the
// userID isn't numeric / the store errors. The payload mirrors the real SPC
// server's SocketDigestMessageData<DigestMessageTemplate>: a DELETE_DIGEST entry
// per digest populates only messageType/dataType/equipmentNo/timestamp/id.
func (q *TombstoneQueue) DrainDigest(ctx context.Context, userID string) (string, bool) {
	uid, err := strconv.ParseInt(userID, 10, 64)
	if err != nil {
		return "", false
	}
	pending, err := q.store.Pending(ctx, uid)
	if err != nil {
		q.logger.Warn("digest tombstone drain", "userId", userID, "error", err)
		return "", false
	}
	if len(pending) == 0 {
		return "", false
	}
	now := time.Now().UnixMilli()
	entries := make([]string, 0, len(pending))
	for _, tb := range pending {
		entries = append(entries, fmt.Sprintf(
			`{"messageType":"DELETE_DIGEST","dataType":%q,"equipmentNo":"ultrabridge","timestamp":%d,"id":%d}`,
			tb.DataType, now, tb.DigestID))
	}
	return fmt.Sprintf(`{"code":"200","timestamp":%d,"msgType":"DIGEST-SYN","data":[%s]}`,
		now, strings.Join(entries, ",")), true
}

// AckDigest clears the tombstones already delivered to userID (the device
// confirmed receipt with a "Received" reply on the digest event).
func (q *TombstoneQueue) AckDigest(ctx context.Context, userID string) {
	uid, err := strconv.ParseInt(userID, 10, 64)
	if err != nil {
		return
	}
	if err := q.store.Ack(ctx, uid); err != nil {
		q.logger.Warn("digest tombstone ack", "userId", userID, "error", err)
	}
}
