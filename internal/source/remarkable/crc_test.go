package remarkable

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sysop/ultrabridge/internal/source"
)

func TestCRC32CHexToHeader(t *testing.T) {
	// writeAtomically stores CRC32C as big-endian hex; the header must be the
	// std-base64 of those 4 bytes.
	if got, ok := crc32cHexToHeader("3012b0cd"); !ok || got != "crc32c=MBKwzQ==" {
		t.Fatalf("crc32cHexToHeader = %q, %v", got, ok)
	}
	for _, bad := range []string{"", "zz", "3012b0", "3012b0cdee"} {
		if _, ok := crc32cHexToHeader(bad); ok {
			t.Errorf("crc32cHexToHeader(%q) accepted malformed input", bad)
		}
	}
}

// TestServeBlob_CRCHeaderIsDeviceVerifiable pins the wire contract the tablet
// enforces on every blob download: `x-goog-hash: crc32c=<std-base64 of the
// 4 big-endian CRC32C bytes>`. The store keeps the CRC as hex; serving that
// hex verbatim reads on-device as "no expected crc32 value provided" →
// ChecksumError → "Server index is empty" (the first server-authored upload
// was the first blob the device ever had to download, which is why weeks of
// device-authored syncing never tripped it).
func TestServeBlob_CRCHeaderIsDeviceVerifiable(t *testing.T) {
	db := testDB(t)
	row := source.SourceRow{
		Type:       "remarkable",
		Name:       "RM",
		ConfigJSON: `{"data_path":"` + t.TempDir() + `","pairing_code":"123456"}`,
	}
	src, err := NewSource(db, row, source.SharedDeps{})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if err := src.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(src.Stop)
	mux := http.NewServeMux()
	src.RegisterRoutes(mux)
	token := pairUserToken(t, mux, "rm-device-crc", "reMarkable Paper Pro Move")

	body := "index bytes the device will crc-check"
	req := httptest.NewRequest(http.MethodPut, "/sync/v3/files/blob-crc", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("blob put = %d body=%s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/sync/v3/files/blob-crc", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("blob get = %d body=%s", w.Code, w.Body.String())
	}

	hdr := w.Header().Get("x-goog-hash")
	if !strings.HasPrefix(hdr, "crc32c=") {
		t.Fatalf("x-goog-hash = %q, want crc32c= prefix", hdr)
	}
	// Decode exactly the way rm-sync does: std base64 to 4 raw bytes.
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(hdr, "crc32c="))
	if err != nil {
		t.Fatalf("header value is not valid base64 (device hard-fails on this): %q err=%v", hdr, err)
	}
	if len(raw) != 4 {
		t.Fatalf("decoded crc is %d bytes, want 4 (header %q)", len(raw), hdr)
	}
	want := crc32.Checksum([]byte(body), crc32.MakeTable(crc32.Castagnoli))
	if got := binary.BigEndian.Uint32(raw); got != want {
		t.Fatalf("crc = %08x, want %08x", got, want)
	}
}
