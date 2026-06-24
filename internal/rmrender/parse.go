package rmrender

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
)

const (
	headerV6      = "reMarkable .lines file, version=6"
	headerV5      = "reMarkable .lines file, version=5"
	headerLen     = 43
	blockLineItem = 5

	tagByte1   = 0x1
	tagByte4   = 0x4
	tagByte8   = 0x8
	tagLength4 = 0xC
	tagID      = 0xF

	itemLine = 3
)

// Page is one parsed reMarkable v6 page. V1 intentionally keeps only the
// vector strokes needed for image rendering.
type Page struct {
	Width     int
	Height    int
	CenteredX bool
	Strokes   []Stroke
}

type Stroke struct {
	PenType        int
	Color          int
	ColorARGB      *uint32
	ThicknessScale float64
	Points         []Point
}

type Point struct {
	X, Y      float64
	Speed     uint16
	Width     uint16
	Direction uint8
	Pressure  uint8
}

// Parse reads the v6 .rm page format used by current reMarkable firmware.
func Parse(data []byte) (Page, error) {
	if len(data) < headerLen {
		return Page{Width: 1404, Height: 1872}, fmt.Errorf("rm file too small")
	}
	switch {
	case bytes.HasPrefix(data, []byte(headerV6)):
		return parseV6(data)
	case bytes.HasPrefix(data, []byte(headerV5)):
		return parseV5(data)
	default:
		return Page{Width: 1404, Height: 1872}, fmt.Errorf("unsupported rm header")
	}
}

func parseV6(data []byte) (Page, error) {
	p := Page{Width: 1404, Height: 1872, CenteredX: true}
	r := newReader(data)
	r.skip(headerLen)
	for r.remaining() >= 8 {
		blockLen, ok := r.u32()
		if !ok || r.remaining() < 4 {
			break
		}
		r.skip(3) // reserved, min version, current version
		blockType, ok := r.u8()
		if !ok {
			break
		}
		contentStart := r.pos
		contentEnd := contentStart + int(blockLen)
		if contentEnd < contentStart || contentEnd > len(data) {
			break
		}
		if blockType == blockLineItem && blockLen > 0 {
			if st, ok := parseLineItem(newReader(data[contentStart:contentEnd])); ok && len(st.Points) > 0 {
				p.Strokes = append(p.Strokes, st)
			}
		}
		r.pos = contentEnd
	}
	return p, nil
}

func parseV5(data []byte) (Page, error) {
	p := Page{Width: 1404, Height: 1872}
	r := newReader(data)
	r.skip(headerLen)
	layerCount, ok := r.u32()
	if !ok {
		return p, fmt.Errorf("v5 missing layer count")
	}
	for layer := uint32(0); layer < layerCount; layer++ {
		strokeCount, ok := r.u32()
		if !ok {
			return p, fmt.Errorf("v5 layer %d missing stroke count", layer)
		}
		for i := uint32(0); i < strokeCount; i++ {
			st, ok := parseV5Stroke(r)
			if !ok {
				return p, fmt.Errorf("v5 stroke %d in layer %d truncated", i, layer)
			}
			if len(st.Points) > 0 {
				p.Strokes = append(p.Strokes, st)
			}
		}
	}
	return p, nil
}

func parseV5Stroke(r *reader) (Stroke, bool) {
	pen, ok := r.u32()
	if !ok {
		return Stroke{}, false
	}
	color, ok := r.u32()
	if !ok {
		return Stroke{}, false
	}
	if !r.skip(4) {
		return Stroke{}, false
	}
	width, ok := r.f32()
	if !ok {
		return Stroke{}, false
	}
	if !r.skip(4) {
		return Stroke{}, false
	}
	segments, ok := r.u32()
	if !ok {
		return Stroke{}, false
	}
	st := Stroke{PenType: int(pen), Color: int(color), ThicknessScale: 1}
	for i := uint32(0); i < segments; i++ {
		x, ok := r.f32()
		if !ok {
			return Stroke{}, false
		}
		y, ok := r.f32()
		if !ok {
			return Stroke{}, false
		}
		pressure, ok := r.f32()
		if !ok {
			return Stroke{}, false
		}
		if !r.skip(4) {
			return Stroke{}, false
		}
		pointWidth, ok := r.f32()
		if !ok {
			return Stroke{}, false
		}
		if !r.skip(4) {
			return Stroke{}, false
		}
		w := pointWidth
		if w <= 0 {
			w = width
		}
		st.Points = append(st.Points, Point{
			X: float64(x),
			Y: float64(y),
			// Keep v5 widths on roughly the same scale as v6's uint16 sample
			// widths before strokeWidth applies the shared renderer scaling.
			Width:    uint16(maxFloat32(w*10, 1)),
			Pressure: uint8(clampFloat32(pressure*255, 0, 255)),
		})
	}
	return st, true
}

func parseLineItem(r *reader) (Stroke, bool) {
	var deleted uint32
	for r.remaining() > 0 {
		t, ok := r.tag()
		if !ok {
			return Stroke{}, false
		}
		switch {
		case t.index == 1 && t.typ == tagID:
			r.skipID()
		case t.index >= 2 && t.index <= 4 && t.typ == tagID:
			r.skipID()
		case t.index == 5 && t.typ == tagByte4:
			deleted, _ = r.u32()
		case t.index == 6 && t.typ == tagLength4:
			n, ok := r.u32()
			if !ok || int(n) > r.remaining() {
				return Stroke{}, false
			}
			if deleted > 0 {
				return Stroke{}, false
			}
			sub := newReader(r.bytes(r.pos, int(n)))
			st, ok := parseLineValue(sub)
			return st, ok
		default:
			r.skipTagValue(t)
		}
	}
	return Stroke{}, false
}

func parseLineValue(r *reader) (Stroke, bool) {
	item, ok := r.u8()
	if !ok || item != itemLine {
		return Stroke{}, false
	}
	st := Stroke{ThicknessScale: 1}
	for r.remaining() > 0 {
		t, ok := r.tag()
		if !ok {
			break
		}
		switch {
		case t.index == 1 && t.typ == tagByte4:
			v, _ := r.u32()
			st.PenType = int(v)
		case t.index == 2 && t.typ == tagByte4:
			v, _ := r.u32()
			st.Color = int(v)
		case t.index == 3 && t.typ == tagByte8:
			st.ThicknessScale, _ = r.f64()
		case t.index == 4 && t.typ == tagByte4:
			r.skip(4)
		case t.index == 5 && t.typ == tagLength4:
			n, ok := r.u32()
			if !ok || int(n) > r.remaining() {
				return st, false
			}
			st.Points = parsePoints(r.bytes(r.pos, int(n)))
			r.skip(int(n))
		case t.index == 8 && t.typ == tagByte4:
			v, _ := r.u32()
			st.ColorARGB = &v
		case t.typ == tagID:
			r.skipID()
		default:
			r.skipTagValue(t)
		}
	}
	return st, true
}

func parsePoints(data []byte) []Point {
	r := newReader(data)
	points := make([]Point, 0, len(data)/14)
	for r.remaining() >= 14 {
		x, _ := r.f32()
		y, _ := r.f32()
		speed, _ := r.u16()
		width, _ := r.u16()
		direction, _ := r.u8()
		pressure, _ := r.u8()
		if !math.IsNaN(float64(x)) && !math.IsNaN(float64(y)) {
			points = append(points, Point{
				X: float64(x), Y: float64(y), Speed: speed, Width: width, Direction: direction, Pressure: pressure,
			})
		}
	}
	return points
}

func maxFloat32(a, b float32) float32 {
	if a > b {
		return a
	}
	return b
}

func clampFloat32(v, lo, hi float32) float32 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

type tag struct {
	index int
	typ   int
}

type reader struct {
	data []byte
	pos  int
}

func newReader(data []byte) *reader { return &reader{data: data} }

func (r *reader) remaining() int { return len(r.data) - r.pos }

func (r *reader) bytes(off, n int) []byte {
	if off < 0 || n < 0 || off+n > len(r.data) {
		return nil
	}
	return r.data[off : off+n]
}

func (r *reader) skip(n int) bool {
	if n < 0 || n > r.remaining() {
		r.pos = len(r.data)
		return false
	}
	r.pos += n
	return true
}

func (r *reader) u8() (uint8, bool) {
	if r.remaining() < 1 {
		return 0, false
	}
	v := r.data[r.pos]
	r.pos++
	return v, true
}

func (r *reader) u16() (uint16, bool) {
	if r.remaining() < 2 {
		return 0, false
	}
	v := binary.LittleEndian.Uint16(r.data[r.pos:])
	r.pos += 2
	return v, true
}

func (r *reader) u32() (uint32, bool) {
	if r.remaining() < 4 {
		return 0, false
	}
	v := binary.LittleEndian.Uint32(r.data[r.pos:])
	r.pos += 4
	return v, true
}

func (r *reader) f32() (float32, bool) {
	v, ok := r.u32()
	return math.Float32frombits(v), ok
}

func (r *reader) f64() (float64, bool) {
	if r.remaining() < 8 {
		return 0, false
	}
	v := binary.LittleEndian.Uint64(r.data[r.pos:])
	r.pos += 8
	return math.Float64frombits(v), true
}

func (r *reader) varint() (uint64, bool) {
	var out uint64
	for shift := uint(0); shift < 64; shift += 7 {
		b, ok := r.u8()
		if !ok {
			return 0, false
		}
		out |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return out, true
		}
	}
	return 0, false
}

func (r *reader) tag() (tag, bool) {
	raw, ok := r.varint()
	if !ok {
		return tag{}, false
	}
	return tag{index: int(raw >> 4), typ: int(raw & 0xF)}, true
}

func (r *reader) skipID() bool {
	if !r.skip(1) {
		return false
	}
	_, ok := r.varint()
	return ok
}

func (r *reader) skipTagValue(t tag) bool {
	switch t.typ {
	case tagByte1:
		return r.skip(1)
	case tagByte4:
		return r.skip(4)
	case tagByte8:
		return r.skip(8)
	case tagLength4:
		n, ok := r.u32()
		return ok && r.skip(int(n))
	case tagID:
		return r.skipID()
	default:
		_, ok := r.varint()
		return ok
	}
}
