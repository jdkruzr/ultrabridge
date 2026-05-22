package socketio

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sysop/ultrabridge/internal/spcserver/auth"
)

const wsSecret = "ws-test-secret"

// dialWS connects a gorilla client to the handler with the given token query.
func dialWS(t *testing.T, srv *httptest.Server, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	u := strings.Replace(srv.URL, "http", "ws", 1) +
		"/socket.io/?EIO=3&transport=websocket&type=SN-TEST&random=1&sign=x&token=" + token
	return websocket.DefaultDialer.Dial(u, nil)
}

func readFrame(t *testing.T, c *websocket.Conn) string {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(msg)
}

// TestHandshake verifies the EIO open packet, then the 40 connect echo.
// Verifies: spc-phase-1.AC3.1
func TestHandshake(t *testing.T) {
	srv := httptest.NewServer(NewHandler(wsSecret, NewRegistry(), nil))
	defer srv.Close()

	c, _, err := dialWS(t, srv, auth.Mint("u1", wsSecret))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	open := readFrame(t, c)
	if open == "" || open[0] != '0' {
		t.Fatalf("expected open frame, got %q", open)
	}
	var p struct {
		Sid          string   `json:"sid"`
		Upgrades     []string `json:"upgrades"`
		PingInterval int      `json:"pingInterval"`
		PingTimeout  int      `json:"pingTimeout"`
	}
	if err := json.Unmarshal([]byte(open[1:]), &p); err != nil {
		t.Fatalf("open JSON: %v", err)
	}
	if p.Sid == "" || len(p.Upgrades) != 0 || p.PingInterval != 5000 || p.PingTimeout != 25000 {
		t.Errorf("bad open payload: %+v", p)
	}

	if err := c.WriteMessage(websocket.TextMessage, []byte("40")); err != nil {
		t.Fatalf("write 40: %v", err)
	}
	if got := readFrame(t, c); got != "40" {
		t.Errorf("connect echo: got %q, want 40", got)
	}
}

// TestPingPong verifies native ping 2 → pong 3. Verifies: spc-phase-1.AC3.2
func TestPingPong(t *testing.T) {
	srv := httptest.NewServer(NewHandler(wsSecret, NewRegistry(), nil))
	defer srv.Close()
	c, _, err := dialWS(t, srv, auth.Mint("u1", wsSecret))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	readFrame(t, c) // open

	c.WriteMessage(websocket.TextMessage, []byte("2"))
	if got := readFrame(t, c); got != "3" {
		t.Errorf("pong: got %q, want 3", got)
	}
}

// TestRattaPing verifies 42["ratta_ping"] → 42["ratta_ping","Received"].
// Verifies: spc-phase-1.AC3.3
func TestRattaPing(t *testing.T) {
	srv := httptest.NewServer(NewHandler(wsSecret, NewRegistry(), nil))
	defer srv.Close()
	c, _, err := dialWS(t, srv, auth.Mint("u1", wsSecret))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	readFrame(t, c) // open

	c.WriteMessage(websocket.TextMessage, []byte(`42["ratta_ping"]`))
	if got := readFrame(t, c); got != `42["ratta_ping","Received"]` {
		t.Errorf("ratta_ping ack: got %q", got)
	}
}

// TestRejectsBadToken verifies an invalid/missing token fails the handshake.
// Verifies: spc-phase-1.AC3.4
func TestRejectsBadToken(t *testing.T) {
	srv := httptest.NewServer(NewHandler(wsSecret, NewRegistry(), nil))
	defer srv.Close()

	for _, tok := range []string{"", "garbage", auth.Mint("u1", "wrong-secret")} {
		if c, _, err := dialWS(t, srv, tok); err == nil {
			c.Close()
			t.Errorf("expected handshake rejection for token %q", tok)
		}
	}
}

// TestEmitReachesClient verifies a registered connection receives a server push.
// Verifies: spc-phase-1.AC3.6 (integration through the handler)
func TestEmitReachesClient(t *testing.T) {
	reg := NewRegistry()
	srv := httptest.NewServer(NewHandler(wsSecret, reg, nil))
	defer srv.Close()
	c, _, err := dialWS(t, srv, auth.Mint("u1", wsSecret))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	readFrame(t, c) // open

	// Wait until the handler has registered the conn, then emit.
	deadline := time.Now().Add(2 * time.Second)
	for reg.Emit("u1", "ServerMessage", map[string]string{"op": "STARTSYNC"}) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("conn never registered")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := readFrame(t, c); got != `42["ServerMessage",{"op":"STARTSYNC"}]` {
		t.Errorf("pushed frame: got %q", got)
	}
}
