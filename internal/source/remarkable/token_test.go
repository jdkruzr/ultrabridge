package remarkable

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestUserScopesForConfigFakesConnectBundle(t *testing.T) {
	got := userScopesForConfig(Config{})
	for _, want := range []string{"intgr", "screenshare", "docedit", "sync:tortoise", "hws", "hwcmail:-1", "hwc", "mail:-1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("scopes = %q, missing %q", got, want)
		}
	}
}

func TestUserScopesForConfigAlwaysAdvertisesSearch(t *testing.T) {
	got := userScopesForConfig(Config{})
	if !strings.Contains(got, "hws") {
		t.Fatalf("scopes = %q, missing handwriting search scope", got)
	}
}

func TestNewUserJWTCarriesSuppliedScopes(t *testing.T) {
	scopes := baseUserScopes
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
