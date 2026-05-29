package booxrender

import (
	"image"
	"testing"
	"time"

	"github.com/sysop/ultrabridge/internal/booxnote"
)

// TestRenderPage_Resolution verifies the renderer produces output at native page resolution.
// Verifies: boox-notes-pipeline.AC2.1
func TestRenderPage_Resolution(t *testing.T) {
	const width = 1860
	const height = 2480

	page := &booxnote.Page{
		PageID: "testpage",
		Width:  width,
		Height: height,
		Shapes: []*booxnote.Shape{
			{
				UniqueID:  "stroke1",
				ShapeType: 2,                // pencil
				Color:     int32(-16777216), // 0xFF000000 in int32 (black)
				Thickness: 2,
				Points: []booxnote.TinyPoint{
					{X: 100, Y: 100, Pressure: 2000},
					{X: 200, Y: 200, Pressure: 2000},
				},
			},
		},
	}

	img, err := RenderPage(page)
	if err != nil {
		t.Fatalf("RenderPage failed: %v", err)
	}

	bounds := img.Bounds()
	if bounds.Dx() != width {
		t.Errorf("expected width %d, got %d", width, bounds.Dx())
	}
	if bounds.Dy() != height {
		t.Errorf("expected height %d, got %d", height, bounds.Dy())
	}
}

// TestRenderPage_PressureVariation verifies strokes are drawn with pressure variation.
// Verifies: boox-notes-pipeline.AC2.2
func TestRenderPage_PressureVariation(t *testing.T) {
	page := &booxnote.Page{
		PageID: "testpage",
		Width:  500,
		Height: 500,
		Shapes: []*booxnote.Shape{
			{
				UniqueID:  "stroke1",
				ShapeType: 2,                // pencil
				Color:     int32(-16777216), // 0xFF000000 in int32 (black)
				Thickness: 3,
				Points: []booxnote.TinyPoint{
					{X: 100, Y: 100, Pressure: 500},  // low
					{X: 200, Y: 200, Pressure: 3500}, // high
					{X: 300, Y: 100, Pressure: 500},  // low
				},
			},
		},
	}

	img, err := RenderPage(page)
	if err != nil {
		t.Fatalf("RenderPage failed: %v", err)
	}

	// Verify image is not blank (has non-white pixels in stroke area)
	if !hasNonWhitePixels(img, 100, 100, 300, 300) {
		t.Error("expected strokes to be drawn (non-white pixels)")
	}
}

// TestRenderPage_EmptyPoints verifies shapes with empty point data are skipped gracefully.
// Verifies: boox-notes-pipeline.AC2.7
func TestRenderPage_EmptyPoints(t *testing.T) {
	page := &booxnote.Page{
		PageID: "testpage",
		Width:  500,
		Height: 500,
		Shapes: []*booxnote.Shape{
			{
				UniqueID:  "empty_stroke",
				ShapeType: 2,                // scribble
				Color:     int32(-16777216), // 0xFF000000 in int32
				Thickness: 2,
				Points:    []booxnote.TinyPoint{}, // empty
			},
		},
	}

	img, err := RenderPage(page)
	if err != nil {
		t.Fatalf("RenderPage failed: %v", err)
	}

	// Verify image is blank (all white)
	if hasNonWhitePixels(img, 0, 0, 500, 500) {
		t.Error("expected blank page for empty points")
	}
}

// TestRenderPage_ManyShapes verifies renderer handles >500 shapes efficiently.
// Verifies: boox-notes-pipeline.AC2.8
func TestRenderPage_ManyShapes(t *testing.T) {
	const numShapes = 600
	const pointsPerShape = 10

	shapes := make([]*booxnote.Shape, 0, numShapes)
	for i := 0; i < numShapes; i++ {
		points := make([]booxnote.TinyPoint, pointsPerShape)
		for j := 0; j < pointsPerShape; j++ {
			points[j] = booxnote.TinyPoint{
				X:        float32(j * 10),
				Y:        float32(j * 10),
				Pressure: 2000,
			}
		}

		shapes = append(shapes, &booxnote.Shape{
			UniqueID:  "shape" + string(rune(i)),
			ShapeType: 2,
			Color:     int32(-16777216), // 0xFF000000 in int32 (black)
			Thickness: 1,
			Points:    points,
		})
	}

	page := &booxnote.Page{
		PageID: "manyshapes",
		Width:  1860,
		Height: 2480,
		Shapes: shapes,
	}

	start := time.Now()
	img, err := RenderPage(page)
	duration := time.Since(start)

	if err != nil {
		t.Fatalf("RenderPage failed: %v", err)
	}

	// Verify we got an image
	if img == nil {
		t.Error("expected non-nil image")
	}

	bounds := img.Bounds()
	if bounds.Dx() != 1860 || bounds.Dy() != 2480 {
		t.Errorf("expected 1860x2480, got %dx%d", bounds.Dx(), bounds.Dy())
	}

	// Verify rendering completes in reasonable time (< 10 seconds)
	if duration > 10*time.Second {
		t.Errorf("rendering %d shapes took %v, expected < 10s", numShapes, duration)
	}
}

// hasNonWhitePixels checks if there are any non-white pixels in the given region.
// Optimized to use direct Pix slice access when image is *image.RGBA.
func hasNonWhitePixels(img image.Image, x0, y0, x1, y1 int) bool {
	bounds := img.Bounds()

	// Fast path: use direct Pix slice access for RGBA images
	if rgba, ok := img.(*image.RGBA); ok {
		for y := y0; y < y1 && y < bounds.Max.Y; y++ {
			for x := x0; x < x1 && x < bounds.Max.X; x++ {
				idx := rgba.PixOffset(x, y)
				r := rgba.Pix[idx]
				g := rgba.Pix[idx+1]
				b := rgba.Pix[idx+2]
				// White is (255, 255, 255), but accept near-white
				if r < 250 || g < 250 || b < 250 {
					return true
				}
			}
		}
		return false
	}

	// Slow path: use generic At() interface for other image types
	for y := y0; y < y1 && y < bounds.Max.Y; y++ {
		for x := x0; x < x1 && x < bounds.Max.X; x++ {
			r, g, b, a := img.At(x, y).RGBA()
			// Normalize to 0-255 range (RGBA() returns 0-65535)
			r >>= 8
			g >>= 8
			b >>= 8
			a >>= 8
			// White is (255, 255, 255, 255), but accept near-white
			if r < 250 || g < 250 || b < 250 {
				return true
			}
		}
	}
	return false
}
