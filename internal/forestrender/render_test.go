package forestrender

import (
	"encoding/binary"
	"image"
	"testing"
)

// buildPoints encodes [x,y,pressure,0,0] little-endian int32 per point.
func buildPoints(pts ...[3]int32) []byte {
	b := make([]byte, 0, len(pts)*bytesPerPt)
	for _, p := range pts {
		var buf [bytesPerPt]byte
		binary.LittleEndian.PutUint32(buf[0:4], uint32(p[0]))
		binary.LittleEndian.PutUint32(buf[4:8], uint32(p[1]))
		binary.LittleEndian.PutUint32(buf[8:12], uint32(p[2]))
		b = append(b, buf[:]...)
	}
	return b
}

func nonWhitePixels(img image.Image) int {
	b := img.Bounds()
	count := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			if r>>8 != 0xFF || g>>8 != 0xFF || bl>>8 != 0xFF {
				count++
			}
		}
	}
	return count
}

func TestDecodePoints(t *testing.T) {
	blob := buildPoints([3]int32{10, 20, 128}, [3]int32{11, 22, 130})
	blob = append(blob, 0x01, 0x02) // trailing partial point — must be ignored
	pts := DecodePoints(blob)
	if len(pts) != 2 {
		t.Fatalf("got %d points, want 2 (partial trailing ignored)", len(pts))
	}
	if pts[0] != (Point{10, 20, 128}) || pts[1] != (Point{11, 22, 130}) {
		t.Errorf("decoded %+v", pts)
	}
}

func TestRenderPage_Empty(t *testing.T) {
	img, err := RenderPage(nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if nonWhitePixels(img) != 0 {
		t.Errorf("empty page should be blank white")
	}
}

func TestRenderPage_SinglePointSkipped(t *testing.T) {
	img, err := RenderPage([]Stroke{{Color: 4278190080, PenWidthMin: 2, PenWidthMax: 6, Points: buildPoints([3]int32{5, 5, 100})}})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if nonWhitePixels(img) != 0 {
		t.Errorf("a single-point stroke draws nothing")
	}
}

func TestRenderPage_DrawsStroke(t *testing.T) {
	stroke := Stroke{
		Color: 4278190080, PenWidthMin: 2, PenWidthMax: 6,
		Points: buildPoints([3]int32{10, 10, 2000}, [3]int32{60, 60, 2000}),
	}
	img, err := RenderPage([]Stroke{stroke})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if nonWhitePixels(img) == 0 {
		t.Error("expected a visible stroke, got blank canvas")
	}
	// Canvas should be the stroke extent (50x50) plus 2*margin in each axis.
	want := 50 + 2*margin
	if img.Bounds().Dx() != want || img.Bounds().Dy() != want {
		t.Errorf("canvas = %dx%d, want %dx%d", img.Bounds().Dx(), img.Bounds().Dy(), want, want)
	}
}

func TestPressureToWidth(t *testing.T) {
	// zero pressure clamps to minVisibleWidth even with min 0
	if w := pressureToWidth(0, 0, 10); w != minVisibleWidth {
		t.Errorf("zero pressure width = %v, want %v", w, minVisibleWidth)
	}
	// full pressure → max
	if w := pressureToWidth(pressureMax, 2, 8); w != 8 {
		t.Errorf("full pressure width = %v, want 8", w)
	}
}
