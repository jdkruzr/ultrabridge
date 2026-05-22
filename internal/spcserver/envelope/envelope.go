// Package envelope holds the SPC response envelope (BaseVO) and JSON write
// helpers. It is a leaf package: handlers, auth middleware, and the server all
// import it, and it imports nothing from internal/spcserver — this breaks the
// import cycle that would otherwise form between the spcserver package (which
// wires the subpackages) and those subpackages (which need the envelope).
package envelope

import (
	"encoding/json"
	"net/http"
)

// BaseVO is the SPC response envelope. Every VO embeds it anonymously so that
// payload fields serialize at the top level alongside these three fields,
// never under a "data" key. Mirrors com/ratta/vo/BaseVO.java (fields: success
// bool, errorCode String, errorMsg String; success defaults true).
type BaseVO struct {
	Success   bool   `json:"success"`
	ErrorCode string `json:"errorCode"`
	ErrorMsg  string `json:"errorMsg"`
}

// OK returns a success envelope with empty error fields.
func OK() BaseVO { return BaseVO{Success: true} }

// WriteJSON writes v as the SPC JSON response: Content-Type matching the real
// SPC server (charset=UTF-8) followed by the JSON encoding of v.
func WriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json;charset=UTF-8")
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes a failed BaseVO envelope carrying the given SPC error code
// and message. errorCode values come from the SPC error enum (see
// docs/spc-protocol.md §7).
func WriteError(w http.ResponseWriter, code, msg string) {
	WriteJSON(w, BaseVO{Success: false, ErrorCode: code, ErrorMsg: msg})
}
