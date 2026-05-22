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
	if err := c.write(EncodeOpen(newSID())); err != nil {
		return
	}
	h.reg.Add(c)
	defer h.reg.Remove(c)

	// Reap dead clients: the device sends a frame (ping/ratta_ping) every ~5s,
	// so a missed pingTimeout window means the link is gone.
	readTimeout := time.Duration(PingTimeout) * time.Millisecond
	_ = ws.SetReadDeadline(time.Now().Add(readTimeout))

	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			return
		}
		_ = ws.SetReadDeadline(time.Now().Add(readTimeout))

		switch kind, event, _ := ClassifyFrame(msg); kind {
		case KindPing:
			_ = c.write([]byte{eioPong}) // "3"
		case KindConnect:
			_ = c.write([]byte("40")) // echo Socket.IO connect
		case KindEvent:
			if event == "ratta_ping" {
				_ = c.write(EncodeEvent("ratta_ping", "Received"))
			} else {
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
