package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sysop/ultrabridge/internal/spcserver/login"
)

const (
	testAccount   = "alice@example.com"
	testRawPwd    = "ehh1701jqb"
	testJWTSecret = "test-secret"
)

type fakeSettings struct{ m map[string]string }

func (f *fakeSettings) Get(_ context.Context, k string) (string, error) { return f.m[k], nil }
func (f *fakeSettings) Set(_ context.Context, k, v string) error        { f.m[k] = v; return nil }

func newLoginHandler() *LoginHandler {
	return &LoginHandler{
		DeviceAccount:  testAccount,
		DevicePassword: testRawPwd,
		JWTSecret:      testJWTSecret,
		Codes:          login.NewStore(),
		Store:          &fakeSettings{m: map[string]string{}},
	}
}

func postJSON(t *testing.T, fn http.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	fn(rec, req)
	return rec
}

func decodeSuccess(t *testing.T, rec *httptest.ResponseRecorder) bool {
	t.Helper()
	var env struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
	}
	return env.Success
}

// webPassword reproduces what the device sends for the given code.
func webPassword(code string) string {
	return login.Sha256Hex(login.Md5Hex(testRawPwd) + code)
}

// TestLoginHappyPath: issue a code, present the matching webPassword, get a
// non-empty token. Verifies: spc-phase-1.AC2.1
func TestLoginHappyPath(t *testing.T) {
	h := newLoginHandler()
	code := h.Codes.Issue(testAccount)

	body := `{"account":"` + testAccount + `","password":"` + webPassword(code) + `"}`
	rec := postJSON(t, h.Login, body)

	var vo struct {
		Success bool   `json:"success"`
		Token   string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &vo); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
	}
	if !vo.Success || vo.Token == "" {
		t.Errorf("expected success with non-empty token, got %s", rec.Body.String())
	}
}

// TestLoginWrongPassword: a bad webPassword is rejected.
// Verifies: spc-phase-1.AC2.6
func TestLoginWrongPassword(t *testing.T) {
	h := newLoginHandler()
	h.Codes.Issue(testAccount)

	body := `{"account":"` + testAccount + `","password":"deadbeef"}`
	if decodeSuccess(t, postJSON(t, h.Login, body)) {
		t.Errorf("expected success=false for wrong password")
	}
}

// TestLoginReusedCode: a code consumed by one login cannot be reused.
// Verifies: spc-phase-1.AC2.6
func TestLoginReusedCode(t *testing.T) {
	h := newLoginHandler()
	code := h.Codes.Issue(testAccount)
	body := `{"account":"` + testAccount + `","password":"` + webPassword(code) + `"}`

	if !decodeSuccess(t, postJSON(t, h.Login, body)) {
		t.Fatalf("first login should succeed")
	}
	if decodeSuccess(t, postJSON(t, h.Login, body)) {
		t.Errorf("second login with consumed code should fail")
	}
}

// TestRandomCode returns a non-empty code. Verifies: spc-phase-1.AC2.4
func TestRandomCode(t *testing.T) {
	h := newLoginHandler()
	rec := postJSON(t, h.RandomCode, `{"account":"`+testAccount+`"}`)

	var vo struct {
		Success    bool   `json:"success"`
		RandomCode string `json:"randomCode"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &vo); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !vo.Success || vo.RandomCode == "" {
		t.Errorf("expected success with non-empty randomCode, got %s", rec.Body.String())
	}
}

// TestChallengeAndBootStubs: the well-formed-success stubs the device needs to
// complete login/boot. Verifies: spc-phase-1.AC2.4
func TestChallengeAndBootStubs(t *testing.T) {
	h := newLoginHandler()
	for name, fn := range map[string]http.HandlerFunc{
		"checkExistsServer": h.CheckExistsServer,
		"logout":            h.Logout,
		"bindEquipment":     h.BindEquipment,
		"unlink":            h.Unlink,
		"fileQueryServer":   h.FileQueryServer,
		"queryToken":        h.QueryToken,
	} {
		if !decodeSuccess(t, postJSON(t, fn, `{}`)) {
			t.Errorf("%s: expected success=true", name)
		}
	}
}
