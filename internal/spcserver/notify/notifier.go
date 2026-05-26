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
	// Emit the "to-do" event (SocketConstant.EVENT_TODO): the device's
	// TaskService binds "to-do" and its onReceive unconditionally triggers a
	// task sync. "ServerMessage" is the FILE channel (→ file sync, Phase 2);
	// using it does not pull tasks. Confirmed from the decompiled task app
	// (TaskService SocketServiceManager("to-do"), 2026-05-23).
	if n.reg.Emit(userID, "to-do", payload) == 0 {
		n.logger.Debug("task STARTSYNC: no device connected", "userId", userID)
	}
	return nil
}

// NotifyFile pushes a FILE-SYN STARTSYNC over the "ServerMessage" file channel
// (CLAUDE.md: ServerMessage is the FILE channel; to-do is tasks). UB fires it
// after a server-side file change (e.g. an upload finishing, or — later — a web/
// other-device mutation) so the device re-pulls. Like Notify it is best-effort:
// no userId / no live connection returns nil, and the device's next periodic
// file sync catches anything missed. NOT load-bearing for the device's own
// upload round-trip (the device initiates that itself).
func (n *SocketNotifier) NotifyFile(ctx context.Context) error {
	userID, err := n.userID(ctx)
	if err != nil {
		n.logger.Warn("FILE-SYN: userId resolve failed", "error", err)
		return nil
	}
	if userID == "" {
		return nil // no device has logged in yet
	}
	now := time.Now().UnixMilli()
	payload := fmt.Sprintf(
		`{"code":"200","timestamp":%d,"msgType":"FILE-SYN","data":[{"messageType":"STARTSYNC","equipmentNo":"ultrabridge","timestamp":%d}]}`,
		now, now,
	)
	if n.reg.Emit(userID, "ServerMessage", payload) == 0 {
		n.logger.Debug("file STARTSYNC: no device connected", "userId", userID)
	}
	return nil
}

// NotifyDigestDelete pushes a DELETE_DIGEST tombstone over the "digest" event so
// the device removes its local copy of a server/web-deleted digest. Unlike the
// STARTSYNC nudges above, this carries the item id: the device treats a digest
// merely *absent* from query/summary/hash as something to re-assert (re-push),
// so only an explicit DELETE_DIGEST makes it delete locally (D2 tombstone). The
// wire shape mirrors the real SPC server's SocketDigestMessageData<
// DigestMessageTemplate> (SocketIoConstant.EVENT_DIGEST / MSG_TYPE_DIGEST), which
// on a delete populates only messageType/dataType/equipmentNo/timestamp/id.
// dataType is the digest's sourceType ("1"=PDF, "2"=note). Best-effort like the
// other notifiers: no userId / no live connection returns nil.
func (n *SocketNotifier) NotifyDigestDelete(ctx context.Context, id int64, dataType string) error {
	userID, err := n.userID(ctx)
	if err != nil {
		n.logger.Warn("DIGEST-SYN: userId resolve failed", "error", err)
		return nil
	}
	if userID == "" {
		return nil // no device has logged in yet
	}
	now := time.Now().UnixMilli()
	payload := fmt.Sprintf(
		`{"code":"200","timestamp":%d,"msgType":"DIGEST-SYN","data":[{"messageType":"DELETE_DIGEST","dataType":%q,"equipmentNo":"ultrabridge","timestamp":%d,"id":%d}]}`,
		now, dataType, now, id,
	)
	if n.reg.Emit(userID, "digest", payload) == 0 {
		n.logger.Debug("digest DELETE_DIGEST: no device connected", "userId", userID)
	}
	return nil
}
