package socketio

import (
	"encoding/json"
	"testing"
)

// TestEncodeOpen verifies the Engine.IO open packet: "0" + JSON with the four
// handshake fields, upgrades as an empty array (not null).
// Verifies: spc-phase-1.AC3.1
func TestEncodeOpen(t *testing.T) {
	frame := EncodeOpen("abc123")
	if len(frame) == 0 || frame[0] != '0' {
		t.Fatalf("open frame must start with '0', got %q", frame)
	}

	var got struct {
		Sid          string   `json:"sid"`
		Upgrades     []string `json:"upgrades"`
		PingInterval int      `json:"pingInterval"`
		PingTimeout  int      `json:"pingTimeout"`
	}
	if err := json.Unmarshal(frame[1:], &got); err != nil {
		t.Fatalf("open payload not valid JSON: %v (%q)", err, frame)
	}
	if got.Sid != "abc123" {
		t.Errorf("sid: got %q, want abc123", got.Sid)
	}
	if got.Upgrades == nil || len(got.Upgrades) != 0 {
		t.Errorf("upgrades must be an empty array, got %v", got.Upgrades)
	}
	if got.PingInterval != 5000 || got.PingTimeout != 25000 {
		t.Errorf("ping interval/timeout: got %d/%d, want 5000/25000", got.PingInterval, got.PingTimeout)
	}
	// upgrades:[] must serialize as [], never null.
	if string(frame) == `0{"sid":"abc123","upgrades":null,"pingInterval":5000,"pingTimeout":25000}` {
		t.Errorf("upgrades serialized as null")
	}
}

func TestClassifyFrame(t *testing.T) {
	tests := []struct {
		name      string
		frame     string
		wantKind  Kind
		wantEvent string
		wantPay   string // "" = none
	}{
		{"ping", "2", KindPing, "", ""},
		{"pong", "3", KindPong, "", ""},
		{"connect", "40", KindConnect, "", ""},
		{"open", `0{"sid":"x"}`, KindOpen, "", ""},
		{"event no payload", `42["ratta_ping"]`, KindEvent, "ratta_ping", ""},
		{"event with payload", `42["ClientMessage",{"a":1}]`, KindEvent, "ClientMessage", `{"a":1}`},
		{"empty", "", KindUnknown, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kind, event, payload := ClassifyFrame([]byte(tc.frame))
			if kind != tc.wantKind {
				t.Errorf("kind: got %v, want %v", kind, tc.wantKind)
			}
			if event != tc.wantEvent {
				t.Errorf("event: got %q, want %q", event, tc.wantEvent)
			}
			if string(payload) != tc.wantPay {
				t.Errorf("payload: got %q, want %q", payload, tc.wantPay)
			}
		})
	}
}
