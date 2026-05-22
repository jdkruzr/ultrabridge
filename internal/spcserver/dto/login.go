// Package dto holds the SPC request DTOs and response VOs, with field names
// matching the decompiled SPC Java sources verbatim (camelCase as Jackson
// emits them). Response VOs embed envelope.BaseVO so success/errorCode/errorMsg
// serialize flat alongside the payload. See docs/spc-protocol.md §8 for
// field-name gotchas.
package dto

import "github.com/sysop/ultrabridge/internal/spcserver/envelope"

// LoginDTO is the terminal-login request body (com/ratta/user/dto/LoginDTO.java).
// equipment is the device-type code (Integer); equipmentNo is the device serial;
// password is the challenge-hashed webPassword (see docs/spc-protocol.md §2.1).
type LoginDTO struct {
	Account     string `json:"account"`
	Password    string `json:"password"`
	CountryCode string `json:"countryCode"`
	Browser     string `json:"browser"`
	Equipment   int    `json:"equipment"`
	LoginMethod string `json:"loginMethod"`
	Language    string `json:"language"`
	EquipmentNo string `json:"equipmentNo"`
	Timestamp   int64  `json:"timestamp"`
}

// LoginVO is the terminal-login response (com/ratta/user/vo/LoginVO.java).
// isBind / isBindEquipment are string flags ("1"/"0"). LastUpdateTime is a Java
// Date; Jackson's default serializes it as epoch millis.
type LoginVO struct {
	envelope.BaseVO
	Token           string `json:"token"`
	Counts          string `json:"counts"`
	UserName        string `json:"userName"`
	AvatarsUrl      string `json:"avatarsUrl"`
	LastUpdateTime  int64  `json:"lastUpdateTime"`
	IsBind          string `json:"isBind"`
	IsBindEquipment string `json:"isBindEquipment"`
	SoldOutCount    int    `json:"soldOutCount"`
}

// RandomCodeVO is the login-challenge response carrying the one-time randomCode
// (com/ratta/user/vo/RandomCodeVO.java).
type RandomCodeVO struct {
	envelope.BaseVO
	RandomCode string `json:"randomCode"`
	Timestamp  int64  `json:"timestamp"`
}

// QueryTokenVO is the /user/query/token response
// (com/ratta/user/vo/QueryTokenVO.java): BaseVO + a single token field.
type QueryTokenVO struct {
	envelope.BaseVO
	Token string `json:"token"`
}

// UserCheckVO is the check/exists/server response
// (com/ratta/user/vo/UserCheckVO.java). dms and uniqueMachineId are SPC
// multi-server/sharding hints UB does not use; the device proceeds on a
// well-formed success (proven 0c). userId carries the resolved user id.
type UserCheckVO struct {
	envelope.BaseVO
	Dms             string `json:"dms"`
	UserId          int64  `json:"userId"`
	UniqueMachineId string `json:"uniqueMachineId"`
}
