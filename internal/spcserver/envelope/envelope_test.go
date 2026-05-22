package envelope

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// TestBaseVOSerializesFlat verifies that a VO embedding BaseVO anonymously
// serializes its payload fields at the top level alongside success/errorCode/
// errorMsg, never nested under a "data" key.
// Verifies: spc-phase-1.AC1.4
func TestBaseVOSerializesFlat(t *testing.T) {
	type fooVO struct {
		BaseVO
		Foo string `json:"foo"`
	}

	b, err := json.Marshal(fooVO{BaseVO: OK(), Foo: "bar"})
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	for _, key := range []string{"success", "errorCode", "errorMsg", "foo"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("expected top-level key %q in %s", key, b)
		}
	}
	if _, ok := raw["data"]; ok {
		t.Errorf("payload must not be nested under a \"data\" key: %s", b)
	}
}

// TestOK verifies OK() yields a success envelope with empty error fields.
func TestOK(t *testing.T) {
	b, err := json.Marshal(OK())
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	if string(b) != `{"success":true,"errorCode":"","errorMsg":""}` {
		t.Errorf("unexpected OK() JSON: %s", b)
	}
}

// TestWriteJSON verifies the Content-Type header and encoded body.
func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, OK())

	if ct := rec.Header().Get("Content-Type"); ct != "application/json;charset=UTF-8" {
		t.Errorf("expected SPC Content-Type, got %q", ct)
	}
	// json.Encoder appends a trailing newline.
	if got := rec.Body.String(); got != `{"success":true,"errorCode":"","errorMsg":""}`+"\n" {
		t.Errorf("unexpected body: %q", got)
	}
}

// TestWriteError verifies WriteError emits a failed envelope with the code/msg.
func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, "E0330", "NextSyncToken timeout")

	var got BaseVO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if got.Success {
		t.Errorf("expected success=false, got true")
	}
	if got.ErrorCode != "E0330" || got.ErrorMsg != "NextSyncToken timeout" {
		t.Errorf("unexpected error fields: %+v", got)
	}
}
