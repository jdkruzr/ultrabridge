// Package handlers holds the device-facing SPC HTTP handlers. Handlers
// translate SPC-shaped JSON to/from UB's existing stores at the controller
// boundary. They import internal/spcserver/envelope for the response envelope
// but never the parent spcserver package, keeping the import graph acyclic.
package handlers

import (
	"net/http"

	"github.com/sysop/ultrabridge/internal/spcserver/envelope"
)

// bindStatusVO mirrors com/ratta/equipment/vo/BindStatusVO.java: extends BaseVO
// with a single Boolean bindStatus.
type bindStatusVO struct {
	envelope.BaseVO
	BindStatus bool `json:"bindStatus"`
}

// BindStatus handles POST /api/equipment/bind/status
// (E_EquipmentController.java:101). The device polls this ~4×/session and only
// needs a well-formed success with bindStatus=true. Unauthenticated in 1a; the
// real SPC reads x-access-token here only to log the device. Auth-protecting it
// is deferred to 1b (see docs/spc-protocol.md §5/§11).
func BindStatus(w http.ResponseWriter, r *http.Request) {
	envelope.WriteJSON(w, bindStatusVO{BaseVO: envelope.OK(), BindStatus: true})
}
