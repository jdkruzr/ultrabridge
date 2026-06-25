package remarkable

import (
	"crypto/rand"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// The reMarkable client treats the *device* token as an opaque bearer, but it
// parses the *user* token as a JWT: it base64-decodes the payload and reads the
// profile (account display), the granted scopes, and the subscription "level".
// An opaque random string fails that parse ("Could not parse UserToken as Json
// object"), which surfaces on-device as a stuck profile and "not subscribed",
// and blocks sync entirely. So the user token must be a real JWT whose claims
// mirror what the device expects.
//
// We keep UB's existing DB-backed validation as the security boundary: the JWT
// carries UB's random `remarkable_tokens` row id as its `jti`, and validation
// (parseUserJTI -> store.loadToken) checks that row for existence + expiry. The
// signature is therefore not our trust anchor (the device never verifies it for
// a self-hosted cloud either), so we sign with a per-process key and parse the
// returned token unverified — an attacker still cannot forge a valid jti.
//
// Claim shape is taken from the proven rmfakecloud user token
// (internal/app/claims.go), which this exact device/firmware already accepted.

const (
	// baseUserScopes grants the full Connect-ish scope bundle the device sees
	// from rmfakecloud: modern sync, integrations/screenshare/doc editing,
	// handwriting search, handwriting recognition, and mail-sharing scopes.
	// The tablet appears to gate some feature probes client-side from this
	// bundle before it ever calls the corresponding server endpoint.
	baseUserScopes = "intgr screenshare docedit hws hwcmail:-1 hwc mail:-1 sync:tortoise"

	// userTokenVersion mirrors rmfakecloud's tokenVersion.
	userTokenVersion = 10
)

// userJWTSigningKey is a per-process HS256 key. Its only job is to make the
// token a well-formed JWT; trust comes from the DB-backed jti, so the key need
// not persist across restarts (tokens are parsed unverified on the way back in).
var userJWTSigningKey = func() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return []byte("ultrabridge-remarkable-fallback-signing-key")
	}
	return b
}()

// auth0Profile mirrors rmfakecloud's Auth0profile JSON tags exactly; the device
// reads these to populate the on-device account panel.
type auth0Profile struct {
	UserID        string    `json:"UserID"`
	IsSocial      bool      `json:"IsSocial"`
	Connection    string    `json:"Connection"`
	Name          string    `json:"Name"`
	Nickname      string    `json:"NickName"`
	GivenName     string    `json:"GivenName"`
	FamilyName    string    `json:"FamilyName"`
	Email         string    `json:"Email"`
	EmailVerified bool      `json:"EmailVerified"`
	CreatedAt     time.Time `json:"CreatedAt"`
	UpdatedAt     time.Time `json:"UpdatedAt"`
}

// userTokenClaims is the JWT payload the device parses for the user token.
type userTokenClaims struct {
	Profile    auth0Profile `json:"auth0-profile,omitempty"`
	DeviceDesc string       `json:"device-desc"`
	DeviceID   string       `json:"device-id"`
	Scopes     string       `json:"scopes,omitempty"`
	Version    int          `json:"version"`
	Level      string       `json:"level"`
	Tectonic   string       `json:"tectonic"`
	jwt.RegisteredClaims
}

// newUserJWT builds a signed user-token JWT carrying `jti` (UB's DB token id) as
// its identifier. account is the display name/email shown on-device.
func userScopesForConfig(cfg Config) string {
	return baseUserScopes
}

func newUserJWT(jti, account string, dc tokenClaims, scopes string, ttl time.Duration) (string, error) {
	if strings.TrimSpace(account) == "" {
		account = "UltraBridge"
	}
	userID := dc.UserID
	if userID == "" {
		userID = "remarkable"
	}
	if !strings.HasPrefix(userID, "auth0|") {
		userID = "auth0|" + userID
	}
	now := time.Now()
	claims := userTokenClaims{
		Profile: auth0Profile{
			UserID:        userID,
			Connection:    "Username-Password-Authentication",
			Name:          account,
			Nickname:      account,
			Email:         account + " (via UltraBridge)",
			EmailVerified: true,
			CreatedAt:     now,
			UpdatedAt:     now,
		},
		DeviceDesc: dc.DeviceDesc,
		DeviceID:   dc.DeviceID,
		Scopes:     scopes,
		Version:    userTokenVersion,
		Level:      "connect",
		Tectonic:   "eu",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			NotBefore: jwt.NewNumericDate(now),
			IssuedAt:  jwt.NewNumericDate(now),
			Subject:   userID,
			Issuer:    "rM WebApp",
			ID:        jti,
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tok.Header["kid"] = "1"
	return tok.SignedString(userJWTSigningKey)
}

// parseUserJTI extracts the embedded DB-token id (jti) from a user-token JWT.
// It parses unverified (see file comment); ok is false when the bearer is not a
// JWT (e.g. a legacy opaque token), letting callers fall back to a direct lookup.
func parseUserJTI(token string) (jti string, ok bool) {
	claims, ok := parseUserClaims(token)
	if !ok {
		return "", false
	}
	if claims.ID == "" {
		return "", false
	}
	return claims.ID, true
}

func parseUserClaims(token string) (userTokenClaims, bool) {
	var claims userTokenClaims
	if _, _, err := jwt.NewParser().ParseUnverified(token, &claims); err != nil {
		return userTokenClaims{}, false
	}
	if claims.ID == "" {
		return userTokenClaims{}, false
	}
	return claims, true
}
