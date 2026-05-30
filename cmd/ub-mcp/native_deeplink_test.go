package main

import (
	"encoding/base64"
	"testing"
)

// TestDecodeNativeDeepLink_Success covers the happy path: a base64-encoded
// JSON blob with the Supernote/Viwoods shape produces a decoded struct
// with Filename derived from FilePath. This is the friendly-render path
// for UB-5 — without it, list_tasks output dumps the raw base64 wall.
func TestDecodeNativeDeepLink_Success(t *testing.T) {
	payload := `{"appName":"note","fileId":"x","filePath":"/storage/Note/Vocabulary.note","page":3,"pageId":"defabc"}`
	encoded := base64.StdEncoding.EncodeToString([]byte(payload))

	dl, ok := decodeNativeDeepLink(encoded)
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

// TestDecodeNativeDeepLink_FailurePaths covers everything that should NOT
// be misidentified as a native deep-link — they fall back to "render the
// URL verbatim" rather than producing a malformed Source label.
func TestDecodeNativeDeepLink_FailurePaths(t *testing.T) {
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
			if dl, ok := decodeNativeDeepLink(tc.input); ok {
				t.Errorf("expected decode failure; got %+v", dl)
			}
		})
	}
}

// TestDecodeNativeDeepLink_FilenameEdgeCases probes the inline pathBase
// helper across separator/edge variants without standing up a full table.
func TestDecodeNativeDeepLink_FilenameEdgeCases(t *testing.T) {
	cases := map[string]string{
		"/foo/bar.note":  "bar.note",
		"bar.note":       "bar.note", // no slash
		"/bar.note":      "bar.note",
		"/a/b/c/d.note":  "d.note",
		"":               "", // empty FilePath → Filename stays empty
	}
	for filePath, wantFilename := range cases {
		t.Run(filePath, func(t *testing.T) {
			payload := `{"appName":"note","filePath":"` + filePath + `","page":1}`
			encoded := base64.StdEncoding.EncodeToString([]byte(payload))
			dl, ok := decodeNativeDeepLink(encoded)
			if !ok {
				t.Fatal("decode failed")
			}
			if dl.Filename != wantFilename {
				t.Errorf("Filename: got %q, want %q", dl.Filename, wantFilename)
			}
		})
	}
}
