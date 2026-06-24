package remarkable

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestUserScopesForConfigAddsHWRWhenConfigured(t *testing.T) {
	if got := userScopesForConfig(Config{}); strings.Contains(got, "hwc") {
		t.Fatalf("scopes without hwr config = %q, should not include hwc", got)
	}
	got := userScopesForConfig(Config{HWRApplicationKey: "app-key", HWRHMAC: "hmac-secret"})
	for _, want := range []string{"intgr", "sync:tortoise", "hwcmail:-1", "hwc"} {
		if !strings.Contains(got, want) {
			t.Fatalf("scopes = %q, missing %q", got, want)
		}
	}
}

func TestNewUserJWTCarriesSuppliedScopes(t *testing.T) {
	scopes := baseUserScopes + " " + hwrUserScopes
	tok, err := newUserJWT("jti-1", "tester", tokenClaims{DeviceID: "dev-1", DeviceDesc: "rm"}, scopes, time.Hour)
	if err != nil {
		t.Fatalf("newUserJWT: %v", err)
	}
	var claims userTokenClaims
	if _, _, err := new(jwt.Parser).ParseUnverified(tok, &claims); err != nil {
		t.Fatalf("parse token: %v", err)
	}
	if claims.Scopes != scopes {
		t.Fatalf("scopes = %q, want %q", claims.Scopes, scopes)
	}
}
