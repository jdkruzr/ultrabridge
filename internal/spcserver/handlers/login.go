package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/sysop/ultrabridge/internal/spcserver/auth"
	"github.com/sysop/ultrabridge/internal/spcserver/dto"
	"github.com/sysop/ultrabridge/internal/spcserver/envelope"
	"github.com/sysop/ultrabridge/internal/spcserver/login"
)

// LoginHandler serves the terminal-login, challenge, and login-handshake/boot
// endpoints. Its dependencies are injected by server.go (Task 11).
type LoginHandler struct {
	DeviceAccount  string // "" = accept any account
	DevicePassword string // raw account password; UB derives md5Hex(raw)
	JWTSecret      string
	Codes          *login.Store
	Store          auth.SettingStore
}

// decode reads the JSON body into a LoginDTO, tolerating missing/extra fields.
func decodeLoginDTO(r *http.Request) dto.LoginDTO {
	var d dto.LoginDTO
	_ = json.NewDecoder(r.Body).Decode(&d)
	return d
}

// RandomCode handles POST /api/official/user/query/random/code: issues a
// one-time code for the account (com/ratta/user controller).
func (h *LoginHandler) RandomCode(w http.ResponseWriter, r *http.Request) {
	d := decodeLoginDTO(r)
	code := h.Codes.Issue(d.Account)
	envelope.WriteJSON(w, dto.RandomCodeVO{
		BaseVO:     envelope.OK(),
		RandomCode: code,
		Timestamp:  time.Now().UnixMilli(),
	})
}

// CheckExistsServer handles POST /api/official/user/check/exists/server. The
// device proceeds on a well-formed success (proven 0c); we return the resolved
// userId and leave the SPC sharding hints empty.
func (h *LoginHandler) CheckExistsServer(w http.ResponseWriter, r *http.Request) {
	vo := dto.UserCheckVO{BaseVO: envelope.OK()}
	if uid, err := login.ResolveUserID(r.Context(), h.Store); err == nil {
		vo.UserId = parseUserID(uid)
	}
	envelope.WriteJSON(w, vo)
}

// Login handles POST /api/official/user/account/login/{equipment,new}: validates
// the challenge-hashed password against the one-time code, mints a JWT, and
// returns a LoginVO. Verifies the recipe in docs/spc-protocol.md §2.1.
func (h *LoginHandler) Login(w http.ResponseWriter, r *http.Request) {
	d := decodeLoginDTO(r)

	if h.DeviceAccount != "" && d.Account != h.DeviceAccount {
		envelope.WriteError(w, "E0711", "Incorrect username or password.")
		return
	}
	code, ok := h.Codes.Consume(d.Account)
	if !ok {
		// E0562: "Random number has expired" — also covers missing/reused codes.
		envelope.WriteError(w, "E0562", "Random number has expired")
		return
	}
	if !login.CheckWebPassword(h.DevicePassword, code, d.Password) {
		envelope.WriteError(w, "E0711", "Incorrect username or password.")
		return
	}
	userID, err := login.ResolveUserID(r.Context(), h.Store)
	if err != nil {
		envelope.WriteError(w, "E0712", "login failed")
		return
	}
	envelope.WriteJSON(w, dto.LoginVO{
		BaseVO:          envelope.OK(),
		Token:           auth.Mint(userID, h.JWTSecret),
		Counts:          "0",
		IsBind:          "1",
		IsBindEquipment: "1",
		LastUpdateTime:  time.Now().UnixMilli(),
	})
}

// LoginProbe handles Partner App reachability probes that hit the login URL with
// GET before the real POST challenge flow. It does not authenticate or mint a
// token; it only tells the client this SPC-compatible host is alive.
func (h *LoginHandler) LoginProbe(w http.ResponseWriter, r *http.Request) {
	envelope.WriteJSON(w, envelope.OK())
}

// QueryToken handles POST /api/user/query/token: echoes the presented token.
func (h *LoginHandler) QueryToken(w http.ResponseWriter, r *http.Request) {
	envelope.WriteJSON(w, dto.QueryTokenVO{
		BaseVO: envelope.OK(),
		Token:  r.Header.Get("x-access-token"),
	})
}

// Logout handles POST /api/user/logout.
func (h *LoginHandler) Logout(w http.ResponseWriter, r *http.Request) {
	envelope.WriteJSON(w, envelope.OK())
}

// BindEquipment handles POST /api/terminal/user/bindEquipment (login handshake
// step; E_EquipmentController.java:88). UB has nothing to bind — return success.
func (h *LoginHandler) BindEquipment(w http.ResponseWriter, r *http.Request) {
	envelope.WriteJSON(w, envelope.OK())
}

// Unlink handles POST /api/terminal/equipment/unlink (device logout;
// E_EquipmentController.java:95).
func (h *LoginHandler) Unlink(w http.ResponseWriter, r *http.Request) {
	envelope.WriteJSON(w, envelope.OK())
}

// FileQueryServer handles GET /api/file/query/server (boot reachability check;
// F_FileLocalController.java:235).
func (h *LoginHandler) FileQueryServer(w http.ResponseWriter, r *http.Request) {
	envelope.WriteJSON(w, envelope.OK())
}

// UserQuery is the protected probe wired in 1b: reachable only with a valid
// x-access-token (via auth.Middleware). It returns a minimal success carrying
// the authenticated userId from context.
func UserQuery(w http.ResponseWriter, r *http.Request) {
	envelope.WriteJSON(w, struct {
		envelope.BaseVO
		UserId string `json:"userId"`
	}{BaseVO: envelope.OK(), UserId: auth.UserID(r.Context())})
}

// parseUserID converts a decimal userId string to int64, returning 0 on error
// (UserCheckVO.userId is informational; the device proceeds on success).
func parseUserID(s string) int64 {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	return n
}
