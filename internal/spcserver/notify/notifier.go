// Package notify provides the server-mode STARTSYNC notifier. It implements the
// caldav.SyncNotifier / service.SyncNotifier contract (Notify(ctx) error) by
// pushing a FILE-SYN STARTSYNC event to the device over the Engine.IO registry,
// replacing the client-mode sync.Notifier when UB is acting as the SPC server.
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Emitter is the subset of socketio.Registry the notifier needs.
type Emitter interface {
	Emit(userID, event string, payload any) int
}

// UserIDFunc resolves the device userId to push to (the harvested/resolved
// spc_user_id).
type UserIDFunc func(ctx context.Context) (string, error)

// SocketNotifier pushes STARTSYNC over the Engine.IO registry.
type SocketNotifier struct {
	reg    Emitter
	userID UserIDFunc
	logger *slog.Logger
}

// NewSocketNotifier builds the notifier.
func NewSocketNotifier(reg Emitter, userID UserIDFunc, logger *slog.Logger) *SocketNotifier {
	if logger == nil {
		logger = slog.Default()
	}
	return &SocketNotifier{reg: reg, userID: userID, logger: logger}
}

// Notify pushes a FILE-SYN STARTSYNC ServerMessage to the device. It is
// best-effort: a missing userId (no device has logged in) or no live connection
// returns nil rather than failing the originating DB write — UB has no offline
// queue, so a missed nudge is caught by the device's next periodic sync (see
// docs/future-work/spc-no-analogue-features.md). The payload mirrors the
// client-mode sync.Notifier's wire shape (the data rides as a JSON string).
func (n *SocketNotifier) Notify(ctx context.Context) error {
	userID, err := n.userID(ctx)
	if err != nil {
		n.logger.Warn("STARTSYNC: userId resolve failed", "error", err)
		return nil
	}
	if userID == "" {
		return nil // no device has logged in yet
	}
	now := time.Now().UnixMilli()
	// TASK-SYN (SocketIoConstant.MSG_TYPE_TASK) routes the device to its task/
	// data sync; FILE-SYN routes to file sync (Phase 2). UB's notifier fires on
	// task writes, so it nudges TASK-SYN.
	payload := fmt.Sprintf(
		`{"code":"200","timestamp":%d,"msgType":"TASK-SYN","data":[{"messageType":"STARTSYNC","equipmentNo":"ultrabridge","timestamp":%d}]}`,
		now, now,
	)
	if n.reg.Emit(userID, "ServerMessage", payload) == 0 {
		n.logger.Debug("STARTSYNC: no device connected", "userId", userID)
	}
	return nil
}
