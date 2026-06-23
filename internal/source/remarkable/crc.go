package remarkable

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"net/http"
)

// reMarkable's sync15 client mirrors Google Cloud Storage: it validates an
// `x-goog-hash: crc32c=<base64>` integrity header on storage responses. Blob
// downloads already carry it (serveBlobByID), but the sync root responses did
// not — so the device read the root, could not validate it, and aborted with
// "unable to sync document content" before ever attempting an upload. The CRC
// is CRC32C (Castagnoli), 4 bytes big-endian, std-base64 — matching GCS and
// rmfakecloud's crcJSON.

const gcsHashHeader = "x-goog-hash"

var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// crc32cHeaderValue returns the `crc32c=<base64>` value for x-goog-hash.
func crc32cHeaderValue(b []byte) string {
	var be [4]byte
	binary.BigEndian.PutUint32(be[:], crc32.Checksum(b, crc32cTable))
	return "crc32c=" + base64.StdEncoding.EncodeToString(be[:])
}

// writeJSONHashed marshals v and writes it with the GCS-style crc32c integrity
// header the sync15 device requires on root responses.
func writeJSONHashed(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "encoding error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(gcsHashHeader, crc32cHeaderValue(b))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
