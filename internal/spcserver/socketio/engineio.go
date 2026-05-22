// Package socketio implements the device-facing Engine.IO v3 / Socket.IO v1
// server UB exposes on the same listener as the REST API. The device connects
// directly over websocket (no polling phase) and uses it for keepalive
// (ratta_ping) and server-pushed sync nudges. See docs/spc-protocol.md §3.
package socketio

import (
	"encoding/json"
	"strings"
)

// Engine.IO v3 packet-type prefixes (first byte of a frame).
const (
	eioOpen    = '0' // server → client handshake
	eioClose   = '1'
	eioPing    = '2' // client → server (client-initiated in EIO v3)
	eioPong    = '3' // server → client
	eioMessage = '4' // wraps a Socket.IO packet
)

// Socket.IO v1 packet-type prefixes (second byte, after eioMessage).
const (
	sioConnect = '0' // "40"
	sioEvent   = '2' // "42"
)

// Handshake timing the device expects (ms).
const (
	PingInterval = 5000
	PingTimeout  = 25000
)

// Kind classifies an inbound frame.
type Kind int

const (
	KindUnknown Kind = iota
	KindPing         // native Engine.IO ping ("2")
	KindPong         // native Engine.IO pong ("3")
	KindOpen         // Engine.IO open ("0{...}")
	KindConnect      // Socket.IO connect ("40")
	KindEvent        // Socket.IO event ("42[\"name\",payload]")
	KindMessage      // other Engine.IO message ("4...")
)

type openPayload struct {
	Sid          string   `json:"sid"`
	Upgrades     []string `json:"upgrades"`
	PingInterval int      `json:"pingInterval"`
	PingTimeout  int      `json:"pingTimeout"`
}

// EncodeOpen builds the Engine.IO open frame ("0" + handshake JSON). upgrades is
// an empty array (we do not negotiate transport upgrades — the device is
// already on websocket).
func EncodeOpen(sid string) []byte {
	b, _ := json.Marshal(openPayload{
		Sid:          sid,
		Upgrades:     []string{},
		PingInterval: PingInterval,
		PingTimeout:  PingTimeout,
	})
	return append([]byte{eioOpen}, b...)
}

// ClassifyFrame inspects an inbound frame and returns its kind plus, for events
// ("42[...]"), the event name and raw payload (the second array element, or nil
// when absent as in 42["ratta_ping"]). The parse is tolerant: a malformed event
// array yields KindEvent with an empty name rather than an error.
func ClassifyFrame(frame []byte) (kind Kind, event string, payload json.RawMessage) {
	if len(frame) == 0 {
		return KindUnknown, "", nil
	}
	s := string(frame)
	switch s {
	case "2":
		return KindPing, "", nil
	case "3":
		return KindPong, "", nil
	}
	switch {
	case frame[0] == eioOpen:
		return KindOpen, "", nil
	case strings.HasPrefix(s, "42"):
		name, pay := parseEvent(frame[2:])
		return KindEvent, name, pay
	case strings.HasPrefix(s, "40"):
		return KindConnect, "", nil
	case frame[0] == eioMessage:
		return KindMessage, "", nil
	default:
		return KindUnknown, "", nil
	}
}

// parseEvent decodes a Socket.IO event array `["name", payload?]`.
func parseEvent(arr []byte) (event string, payload json.RawMessage) {
	var parts []json.RawMessage
	if err := json.Unmarshal(arr, &parts); err != nil || len(parts) == 0 {
		return "", nil
	}
	_ = json.Unmarshal(parts[0], &event)
	if len(parts) > 1 {
		payload = parts[1]
	}
	return event, payload
}
