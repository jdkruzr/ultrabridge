package handlers

import (
	"net/http"

	"github.com/sysop/ultrabridge/internal/spcserver/envelope"
)

// SystemBaseParam handles POST /api/official/system/base/param. The Partner
// app and web UI use this as a pre-login capability probe; UB returns conservative
// SPC defaults that keep uploads/downloads enabled without exposing UB internals.
func SystemBaseParam(w http.ResponseWriter, r *http.Request) {
	envelope.WriteJSON(w, struct {
		envelope.BaseVO
		Random string            `json:"random"`
		Param  map[string]string `json:"param"`
	}{
		BaseVO: envelope.OK(),
		Random: "UB",
		Param: map[string]string{
			"COPY_MAX":            "1000",
			"DOWNLOAD_MAX_NUMBER": "50",
			"EMAIL_CODE_TIME":     "5,5",
			"FILE_MAX":            "1073741824",
			"FILE_TYPE":           "note,pdf,epub,txt,png,jpg,jpeg,doc,docx,xls,xlsx,ppt,pptx",
			"MAX_ERR_COUNTS":      "6",
			"UPLOAD_MAX":          "500",
		},
	})
}

// EmailPublicKey is vestigial SPC web plumbing. Returning a success envelope is
// enough for clients that probe it before falling back to hashed-password login.
func EmailPublicKey(w http.ResponseWriter, r *http.Request) {
	envelope.WriteJSON(w, struct {
		envelope.BaseVO
		PublicKey string `json:"publicKey"`
	}{BaseVO: envelope.OK()})
}

// EmailConfig is server-owned SMTP configuration in real SPC. UB does not expose
// transactional email, so return an empty-but-successful config.
func EmailConfig(w http.ResponseWriter, r *http.Request) {
	envelope.WriteJSON(w, struct {
		envelope.BaseVO
		Config map[string]string `json:"config"`
	}{BaseVO: envelope.OK(), Config: map[string]string{}})
}
