package socketio

import (
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
type Handler struct {
	secret   string
	reg      *Registry
	logger   *slog.Logger
	upgrader websocket.Upgrader
}

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

		switch kind, event, _ := ClassifyFrame(msg); kind {
		case KindPing:
			pings++
			_ = c.write([]byte{eioPong}) // "3"
		case KindConnect:
			_ = c.write([]byte("40")) // echo Socket.IO connect
		case KindEvent:
			if event == "ratta_ping" {
				rattas++
				_ = c.write(EncodeEvent("ratta_ping", "Received"))
			} else {
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
