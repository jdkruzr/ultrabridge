package readercontract

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"strconv"
	"strings"
	"unicode/utf8"
)

// CompositeID hashes Kotlin's compact JSON string array, without HTML escaping or
// Unicode normalization. encoding/json's default escaping is not this contract.
func CompositeID(parts ...string) (string, error) {
	var b strings.Builder
	b.WriteByte('[')
	for i, s := range parts {
		if !utf8.ValidString(s) {
			return "", fmt.Errorf("invalid UTF-8")
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		for _, c := range s {
			switch c {
			case '"', '\\':
				b.WriteByte('\\')
				b.WriteRune(c)
			case '\b':
				b.WriteString(`\b`)
			case '\t':
				b.WriteString(`\t`)
			case '\n':
				b.WriteString(`\n`)
			case '\f':
				b.WriteString(`\f`)
			case '\r':
				b.WriteString(`\r`)
			default:
				if c < 0x20 {
					b.WriteString(fmt.Sprintf(`\u%04x`, c))
				} else {
					b.WriteRune(c)
				}
			}
		}
		b.WriteByte('"')
	}
	b.WriteByte(']')
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:]), nil
}

type Ink struct {
	ID, BrushKind                                      string
	Color, WidthMin, WidthMax, BrushVersion, BrushSeed int64
	Points, Dynamics                                   []byte
}

var brushes = strings.Fields("fountain pencil_hb pencil_2b pencil_4b pencil_6b pencil_8b brush ballpoint translucent_marker marker fineliner calligraphy highlighter calligraphy_reverse calligraphy_broad calligraphy_chisel dashed")

func (ink Ink) Bottom() (int64, error) {
	if !validID(ink.ID) || ink.Color < -2147483648 || ink.Color > 2147483647 || ink.WidthMin < 0 || ink.WidthMax < ink.WidthMin || ink.WidthMax <= 0 || ink.WidthMax > 2147483647 || ink.BrushSeed < -2147483648 || ink.BrushSeed > 2147483647 || ink.BrushVersion != 1 {
		return 0, fmt.Errorf("invalid ink identity/style")
	}
	found := false
	for _, b := range brushes {
		found = found || b == ink.BrushKind
	}
	if !found || len(ink.Points) == 0 || len(ink.Points)%20 != 0 || len(ink.Points) > 4*1024*1024 || len(ink.Dynamics) > 1024*1024 {
		return 0, fmt.Errorf("invalid ink encoding")
	}
	var bottom int64
	for offset := 0; offset < len(ink.Points); offset += 20 {
		p := ink.Points[offset:]
		x, y, pressure := int32(binary.LittleEndian.Uint32(p)), int32(binary.LittleEndian.Uint32(p[4:])), int32(binary.LittleEndian.Uint32(p[8:]))
		if x < 0 || y < 0 || pressure < 0 || pressure > 1000 {
			return 0, fmt.Errorf("invalid canonical point")
		}
		if edge := int64(y) + ink.WidthMax; edge > bottom {
			bottom = edge
		}
	}
	if ink.Dynamics != nil {
		count := len(ink.Points) / 20
		if len(ink.Dynamics) != 8+count*4 || binary.LittleEndian.Uint32(ink.Dynamics) != 0x31444e46 || binary.LittleEndian.Uint32(ink.Dynamics[4:]) != uint32(count) {
			return 0, fmt.Errorf("invalid dynamics")
		}
	}
	return bottom, nil
}

func hashNumber(h hash.Hash, n int64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(n))
	_, _ = h.Write(b[:])
}
func hashBytes(h hash.Hash, b []byte) { hashNumber(h, int64(len(b))); _, _ = h.Write(b) }

// Fingerprint consumes canonical virtual geometry and caller-supplied paint order.
// It streams into SHA-256; it never concatenates an annotation-sized buffer.
func Fingerprint(width, height int64, ordered []Ink) (string, error) {
	if width <= 0 || height < 0 {
		return "", fmt.Errorf("invalid canvas")
	}
	h := sha256.New()
	_, _ = h.Write([]byte("FNRI1"))
	hashNumber(h, width)
	hashNumber(h, height)
	for _, ink := range ordered {
		if _, err := ink.Bottom(); err != nil {
			return "", err
		}
		hashBytes(h, []byte(ink.ID))
		hashBytes(h, []byte(ink.BrushKind))
		for _, n := range []int64{ink.Color, ink.WidthMin, ink.WidthMax, ink.BrushVersion, ink.BrushSeed} {
			hashNumber(h, n)
		}
		hashBytes(h, ink.Points)
		hashBytes(h, ink.Dynamics)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func utf16Length(s string) int {
	n := 0
	for _, c := range s {
		n++
		if c > 0xffff {
			n++
		}
	}
	return n
}
func kotlinBlank(s string) bool {
	for _, c := range s {
		if !(c >= 9 && c <= 13 || c >= 0x1c && c <= 0x20 || c == 0xa0 || c == 0x1680 || c >= 0x2000 && c <= 0x200a || c == 0x2028 || c == 0x2029 || c == 0x202f || c == 0x205f || c == 0x3000) {
			return false
		}
	}
	return true
}
func validID(s string) bool {
	return utf8.ValidString(s) && !kotlinBlank(s) && utf16Length(s) <= 512 && !strings.ContainsRune(s, 0)
}

// Go's JSON decoder replaces invalid UTF-16 escapes silently. Reject them at the
// outer string boundary instead; literal escaped JSON stored inside a string is opaque.
func validStringEscapes(raw []byte) bool {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			return false
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return false
		}
		n, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return false
		}
		if n < 0xd800 || n > 0xdbff {
			continue
		}
		if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
			return false
		}
		low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
		if err != nil || low < 0xdc00 || low > 0xdfff {
			return false
		}
		i += 6
	}
	return true
}
