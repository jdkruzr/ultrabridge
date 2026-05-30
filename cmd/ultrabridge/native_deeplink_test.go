package main

import (
	"encoding/base64"
	"testing"
)

// These tests mirror cmd/ub-mcp/native_deeplink_test.go beat-for-beat so
// the two MCP surfaces' deep-link decoders stay in sync — the parity is
// the contract documented in cmd/ub-mcp/CLAUDE.md's "two surfaces" note.
// If a future change to one decoder's behavior doesn't show up here, the
// surfaces have silently drifted.

func TestDecodeMCPNativeDeepLink_Success(t *testing.T) {
	payload := `{"appName":"note","fileId":"x","filePath":"/storage/Note/Vocabulary.note","page":3,"pageId":"defabc"}`
	encoded := base64.StdEncoding.EncodeToString([]byte(payload))

	dl, ok := decodeMCPNativeDeepLink(encoded)
	if !ok {
		t.Fatalf("expected successful decode")
	}
	if dl.AppName != "note" {
		t.Errorf("AppName: got %q, want note", dl.AppName)
	}
	if dl.Filename != "Vocabulary.note" {
		t.Errorf("Filename: got %q, want Vocabulary.note", dl.Filename)
	}
	if dl.Page != 3 {
		t.Errorf("Page: got %d, want 3", dl.Page)
	}
}

func TestDecodeMCPNativeDeepLink_FailurePaths(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"plain http URL", "https://ub.example/files/boox?detail=foo"},
		{"empty string", ""},
		{"non-base64 starting with eyJ", "eyJ!!!invalid"},
		{"base64 of non-JSON", base64.StdEncoding.EncodeToString([]byte("hello world"))},
		{"base64 of JSON without AppName", base64.StdEncoding.EncodeToString([]byte(`{"page":1}`))},
		{"relative path", "/files/boox?detail=foo"},
		{"forestnote native scheme", "forestnote://notebook/abc/page/def"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if dl, ok := decodeMCPNativeDeepLink(tc.input); ok {
				t.Errorf("expected decode failure; got %+v", dl)
			}
		})
	}
}

func TestDecodeMCPNativeDeepLink_FilenameEdgeCases(t *testing.T) {
	cases := map[string]string{
		"/foo/bar.note": "bar.note",
		"bar.note":      "bar.note",
		"/bar.note":     "bar.note",
		"/a/b/c/d.note": "d.note",
		"":              "",
	}
	for filePath, wantFilename := range cases {
		t.Run(filePath, func(t *testing.T) {
			payload := `{"appName":"note","filePath":"` + filePath + `","page":1}`
			encoded := base64.StdEncoding.EncodeToString([]byte(payload))
			dl, ok := decodeMCPNativeDeepLink(encoded)
			if !ok {
				t.Fatal("decode failed")
			}
			if dl.Filename != wantFilename {
				t.Errorf("Filename: got %q, want %q", dl.Filename, wantFilename)
			}
		})
	}
}
