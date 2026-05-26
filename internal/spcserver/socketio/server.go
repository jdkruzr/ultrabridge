package socketio

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sysop/ultrabridge/internal/spcserver/auth"
)

// Handler serves the device-facing Engine.IO v3 websocket endpoint
// (/socket.io/). It authenticates on the handshake token, completes the EIO
// open + SIO connect exchange, answers keepalives, and registers the live
// connection so the rest of UB can push to it. See docs/spc-protocol.md §3.
// DigestQueue is the optional digest-tombstone seam. On each ratta_ping the
// handler drains the user's pending DELETE_DIGEST frames (DrainDigest) and emits
// them on the "digest" event; when the device replies 42["digest","Received"]
// the handler clears the delivered rows (AckDigest). nil disables it.
// *spcserver/notify.TombstoneQueue satisfies it.
type DigestQueue interface {
	DrainDigest(ctx context.Context, userID string) (payload string, ok bool)
	AckDigest(ctx context.Context, userID string)
}

type Handler struct {
	secret   string
	reg      *Registry
	logger   *slog.Logger
	upgrader websocket.Upgrader
	digest   DigestQueue
}

// SetDigestQueue wires the digest-tombstone delivery seam (SPC server mode with
// a digest store; nil otherwise). Set before serving.
func (h *Handler) SetDigestQueue(q DigestQueue) { h.digest = q }

// NewHandler builds the websocket handler. permessage-deflate is intentionally
// not negotiated (EnableCompression stays false) — see the 1c design note; the
// fallback is uncompressed frames.
func NewHandler(secret string, reg *Registry, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		secret: secret,
		reg:    reg,
		logger: logger,
		upgrader: websocket.Upgrader{
			// The device sends no Origin; accept all.
			CheckOrigin: func(*http.Request) bool { return true },
		},
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Auth gate before upgrade: an invalid/missing token fails the handshake
	// (the dialer sees a non-101 status). The sign param is accepted-and-ignored
	// in 1c — token verification is the gate.
	userID, err := auth.Verify(r.URL.Query().Get("token"), h.secret)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	ws, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already wrote the error response
	}
	defer ws.Close()

	c := NewConn(userID, ws)
	// Engine.IO open, then the Socket.IO CONNECT (40) for the default namespace.
	// The device's io.socket client connects to "/" and, per its onopen(), does
	// NOT send a CONNECT itself — it waits to RECEIVE 40 from the server to fire
	// its "connect" event. Without this proactive 40 the client never considers
	// itself connected and runs an unbounded reconnect loop (30/60/120/240s
	// backoff). Confirmed from the decompiled io.socket.client.Socket.onopen +
	// SocketManager.reconnectTask (2026-05-23).
	if err := c.write(EncodeOpen(newSID())); err != nil {
		return
	}
	if err := c.write([]byte("40")); err != nil {
		return
	}
	h.reg.Add(c)
	defer h.reg.Remove(c)

	start := time.Now()
	var pings, rattas, others int
	h.logger.Info("spc socket connected", "userId", userID, "type", r.URL.Query().Get("type"))
	defer func() {
		h.logger.Info("spc socket closed", "userId", userID,
			"dur", time.Since(start).Round(time.Second).String(),
			"pings", pings, "ratta", rattas, "other", others)
	}()

	// Heartbeat is client-driven (engine.io-client EIO3: the client sends ping
	// "2", we reply pong "3" in the read loop). The server does not initiate
	// pings. Reap dead clients via a read deadline refreshed on every frame —
	// the device sends a ping every ~pingInterval, so no frame for a full
	// pingTimeout means the link is gone.
	readTimeout := time.Duration(PingTimeout) * time.Millisecond
	_ = ws.SetReadDeadline(time.Now().Add(readTimeout))

	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			h.logger.Info("spc socket read end", "userId", userID,
				"err", err.Error(), "dur", time.Since(start).Round(time.Second).String(),
				"pings", pings, "ratta", rattas)
			return
		}
		_ = ws.SetReadDeadline(time.Now().Add(readTimeout))

		switch kind, event, payload := ClassifyFrame(msg); kind {
		case KindPing:
			pings++
			_ = c.write([]byte{eioPong}) // "3"
		case KindConnect:
			_ = c.write([]byte("40")) // echo Socket.IO connect
		case KindEvent:
			switch {
			case event == "ratta_ping":
				rattas++
				_ = c.write(EncodeEvent("ratta_ping", "Received"))
				// Drain any pending digest tombstones to this device — the real
				// SPC server delivers queued digest messages on the heartbeat.
				if h.digest != nil {
					if p, ok := h.digest.DrainDigest(r.Context(), userID); ok {
						// Emit the payload as a STRING arg (the device gson-parses
						// args[0]) — same convention as the to-do/ServerMessage
						// nudges and the real server's sendEvent("digest", json).
						_ = c.write(EncodeEvent("digest", p))
					}
				}
			case event == "digest" && h.digest != nil && string(payload) == `"Received"`:
				// The device confirms it processed the digest frame; clear the
				// delivered tombstones so they aren't re-sent on the next ping.
				h.digest.AckDigest(r.Context(), userID)
			default:
				others++
				h.logger.Debug("spc socket event ignored", "event", event, "userId", userID)
			}
		default:
			// open/pong/unknown from the client — nothing to do.
		}
	}
}

// newSID returns a random Engine.IO session id.
func newSID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
