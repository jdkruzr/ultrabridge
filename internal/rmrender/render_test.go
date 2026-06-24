package rmrender

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"math"
	"testing"
)

func TestParseAndRenderLineItem(t *testing.T) {
	data := testRMFile(testLineItemBlock())
	page, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(page.Strokes) != 1 {
		t.Fatalf("strokes = %d, want 1", len(page.Strokes))
	}
	if got := len(page.Strokes[0].Points); got != 2 {
		t.Fatalf("points = %d, want 2", got)
	}

	img, err := RenderPage(page)
	if err != nil {
		t.Fatalf("RenderPage: %v", err)
	}
	if img.Bounds().Dx() != defaultWidth || img.Bounds().Dy() != defaultHeight {
		t.Fatalf("bounds = %v", img.Bounds())
	}
	if imageBlank(img) {
		t.Fatal("rendered page appears blank near expected stroke")
	}
}

func testRMFile(block []byte) []byte {
	var out bytes.Buffer
	header := []byte(headerText)
	out.Write(header)
	for out.Len() < headerLen {
		out.WriteByte(' ')
	}
	out.Write(block)
	return out.Bytes()
}

func testLineItemBlock() []byte {
	var val bytes.Buffer
	val.WriteByte(itemLine)
	writeTag(&val, 1, tagByte4)
	binary.Write(&val, binary.LittleEndian, uint32(2)) // ballpoint
	writeTag(&val, 2, tagByte4)
	binary.Write(&val, binary.LittleEndian, uint32(0)) // black
	writeTag(&val, 3, tagByte8)
	binary.Write(&val, binary.LittleEndian, math.Float64bits(1))
	writeTag(&val, 5, tagLength4)
	points := testPoints()
	binary.Write(&val, binary.LittleEndian, uint32(len(points)))
	val.Write(points)

	var item bytes.Buffer
	writeTag(&item, 6, tagLength4)
	binary.Write(&item, binary.LittleEndian, uint32(val.Len()))
	item.Write(val.Bytes())

	var block bytes.Buffer
	binary.Write(&block, binary.LittleEndian, uint32(item.Len()))
	block.Write([]byte{0, 1, 1, blockLineItem})
	block.Write(item.Bytes())
	return block.Bytes()
}

func testPoints() []byte {
	var out bytes.Buffer
	for _, p := range []struct {
		x, y  float32
		width uint16
	}{
		{0, 100, 20},
		{100, 100, 20},
	} {
		binary.Write(&out, binary.LittleEndian, math.Float32bits(p.x))
		binary.Write(&out, binary.LittleEndian, math.Float32bits(p.y))
		binary.Write(&out, binary.LittleEndian, uint16(0))
		binary.Write(&out, binary.LittleEndian, p.width)
		out.WriteByte(0)
		out.WriteByte(255)
	}
	return out.Bytes()
}

func writeTag(b *bytes.Buffer, index, typ int) {
	writeVarint(b, uint64(index<<4|typ))
}

func writeVarint(b *bytes.Buffer, v uint64) {
	for v >= 0x80 {
		b.WriteByte(byte(v) | 0x80)
		v >>= 7
	}
	b.WriteByte(byte(v))
}

func isWhite(c color.Color) bool {
	r, g, b, _ := c.RGBA()
	return r > 0xf000 && g > 0xf000 && b > 0xf000
}

func imageBlank(img interface {
	Bounds() image.Rectangle
	At(x, y int) color.Color
}) bool {
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y += 10 {
		for x := b.Min.X; x < b.Max.X; x += 10 {
			if !isWhite(img.At(x, y)) {
				return false
			}
		}
	}
	return true
}
