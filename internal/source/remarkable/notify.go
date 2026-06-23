package remarkable

import (
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Wire format for the reMarkable live notification channel
// (/notifications/ws/json/1). Field tags mirror the real cloud / rmfakecloud
// (internal/messages/messages.go) exactly — they are the wire contract.

type wsMessage struct {
	Message      notificationMessage `json:"message"`
	Subscription string              `json:"subscription,omitempty"`
}

type notificationMessage struct {
	Attributes attributes `json:"attributes"`
	MessageID3 string     `json:"messageid,omitempty"` // nanosecond timestamp string
}

type attributes struct {
	Auth0UserID      string `json:"auth0UserID"`
	Event            string `json:"event"`
	SourceDeviceID   string `json:"sourceDeviceID"`
	SourceDeviceDesc string `json:"sourceDeviceDesc,omitempty"`
}

const eventSyncComplete = "SyncComplete"

const (
	wsWriteWait  = 10 * time.Second
	wsPongWait   = 60 * time.Second
	wsPingPeriod = (wsPongWait * 9) / 10
	wsSendBuffer = 8
)

// client is one connected device's notification socket.
type client struct {
	deviceID string
	send     chan *wsMessage
	done     chan struct{}
}

// hub is an in-memory registry of connected notification sockets, keyed by
// user. It fans a sync event out to every connected device of a user except
// the one that triggered it (best-effort; drops on a full/closed peer).
type hub struct {
	logger *slog.Logger
	mu     sync.RWMutex
	// userID -> set of connected clients
	clients map[string]map[*client]struct{}
	closed  bool
}

func newHub(logger *slog.Logger) *hub {
	if logger == nil {
		logger = slog.Default()
	}
	return &hub{logger: logger, clients: make(map[string]map[*client]struct{})}
}

func (h *hub) register(userID string, c *client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	set := h.clients[userID]
	if set == nil {
		set = make(map[*client]struct{})
		h.clients[userID] = set
	}
	set[c] = struct{}{}
	return true
}

func (h *hub) unregister(userID string, c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if set := h.clients[userID]; set != nil {
		delete(set, c)
		if len(set) == 0 {
			delete(h.clients, userID)
		}
	}
}

// notifySync fans a SyncComplete event out to every connected device of
// userID except sourceDeviceID. Best-effort: a peer whose buffer is full or
// whose socket is closing is skipped.
func (h *hub) notifySync(userID, sourceDeviceID, sourceDeviceDesc string) {
	msg := &wsMessage{
		Message: notificationMessage{
			MessageID3: strconv.FormatInt(time.Now().UnixNano(), 10),
			Attributes: attributes{
				Auth0UserID:      userID,
				Event:            eventSyncComplete,
				SourceDeviceID:   sourceDeviceID,
				SourceDeviceDesc: sourceDeviceDesc,
			},
		},
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	delivered := 0
	for c := range h.clients[userID] {
		if c.deviceID == sourceDeviceID {
			continue
		}
		select {
		case c.send <- msg:
			delivered++
		case <-c.done:
		default:
			h.logger.Warn("remarkable: dropping notification (slow peer)", "device", c.deviceID)
		}
	}
	if delivered == 0 {
		h.logger.Debug("remarkable: sync notification, no connected peers", "user", userID, "source", sourceDeviceID)
	}
}

// connectWS registers a websocket connection and blocks until it closes,
// running the read pump (close/control-frame detection) inline and the write
// pump (queued messages + keepalive pings) in a goroutine.
func (h *hub) connectWS(userID, deviceID string, conn *websocket.Conn) {
	c := &client{
		deviceID: deviceID,
		send:     make(chan *wsMessage, wsSendBuffer),
		done:     make(chan struct{}),
	}
	if !h.register(userID, c) {
		conn.Close()
		return
	}
	defer func() {
		h.unregister(userID, c)
		close(c.done)
		conn.Close()
	}()

	go h.writePump(conn, c)
	h.readPump(conn, c)
}

func (h *hub) readPump(conn *websocket.Conn, c *client) {
	conn.SetReadLimit(4096)
	_ = conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsPongWait))
	})
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

func (h *hub) writePump(conn *websocket.Conn, c *client) {
	ticker := time.NewTicker(wsPingPeriod)
	defer ticker.Stop()
	for {
		select {
		case msg, ok := <-c.send:
			_ = conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if !ok {
				_ = conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := conn.WriteJSON(msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-c.done:
			return
		}
	}
}

func (h *hub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for _, set := range h.clients {
		for c := range set {
			// Closing send signals the write pump to send a close frame; the
			// read pump then unwinds connectWS which closes the conn.
			close(c.send)
		}
	}
	h.clients = make(map[string]map[*client]struct{})
}
