package syncstore

import (
	"crypto/rand"
	"strings"
	"time"
)

// Crockford base32, uppercase — the ULID alphabet (spec/protocol.md §I.1). site_id and pk are
// client-minted ULIDs; their ASCII order equals ULID numeric order, which the LWW tie-break relies on.
const ulidAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// IsULID reports whether s is a canonical 26-char uppercase Crockford ULID.
func IsULID(s string) bool {
	if len(s) != 26 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(ulidAlphabet, s[i]) < 0 {
			return false
		}
	}
	return true
}

// NewULID mints a canonical 26-char uppercase Crockford ULID: a 48-bit millisecond timestamp
// followed by 80 bits of crypto-random. The server needs this when it authors ops (its site_id
// travels on the wire, where peers validate it as a ULID).
func NewULID() string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	if _, err := rand.Read(b[6:]); err != nil {
		panic("syncstore: crypto/rand failed minting ULID: " + err.Error())
	}
	return encodeULID(b)
}

// encodeULID renders 16 raw bytes as 26 Crockford base32 chars, 5 bits at a time, MSB-first.
func encodeULID(b [16]byte) string {
	d := make([]byte, 26)
	d[0] = ulidAlphabet[(b[0]&224)>>5]
	d[1] = ulidAlphabet[b[0]&31]
	d[2] = ulidAlphabet[(b[1]&248)>>3]
	d[3] = ulidAlphabet[((b[1]&7)<<2)|((b[2]&192)>>6)]
	d[4] = ulidAlphabet[(b[2]&62)>>1]
	d[5] = ulidAlphabet[((b[2]&1)<<4)|((b[3]&240)>>4)]
	d[6] = ulidAlphabet[((b[3]&15)<<1)|((b[4]&128)>>7)]
	d[7] = ulidAlphabet[(b[4]&124)>>2]
	d[8] = ulidAlphabet[((b[4]&3)<<3)|((b[5]&224)>>5)]
	d[9] = ulidAlphabet[b[5]&31]
	d[10] = ulidAlphabet[(b[6]&248)>>3]
	d[11] = ulidAlphabet[((b[6]&7)<<2)|((b[7]&192)>>6)]
	d[12] = ulidAlphabet[(b[7]&62)>>1]
	d[13] = ulidAlphabet[((b[7]&1)<<4)|((b[8]&240)>>4)]
	d[14] = ulidAlphabet[((b[8]&15)<<1)|((b[9]&128)>>7)]
	d[15] = ulidAlphabet[(b[9]&124)>>2]
	d[16] = ulidAlphabet[((b[9]&3)<<3)|((b[10]&224)>>5)]
	d[17] = ulidAlphabet[b[10]&31]
	d[18] = ulidAlphabet[(b[11]&248)>>3]
	d[19] = ulidAlphabet[((b[11]&7)<<2)|((b[12]&192)>>6)]
	d[20] = ulidAlphabet[(b[12]&62)>>1]
	d[21] = ulidAlphabet[((b[12]&1)<<4)|((b[13]&240)>>4)]
	d[22] = ulidAlphabet[((b[13]&15)<<1)|((b[14]&128)>>7)]
	d[23] = ulidAlphabet[(b[14]&124)>>2]
	d[24] = ulidAlphabet[((b[14]&3)<<3)|((b[15]&224)>>5)]
	d[25] = ulidAlphabet[b[15]&31]
	return string(d)
}
