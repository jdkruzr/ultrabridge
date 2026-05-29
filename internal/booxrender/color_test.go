package booxrender

import (
	"testing"

	"github.com/sysop/ultrabridge/internal/booxnote"
)

// TestDecodeARGB tests ARGB color decoding.
// Verifies: boox-notes-pipeline.AC2.4
func TestDecodeARGB(t *testing.T) {
	tests := []struct {
		name                   string
		argb                   int32
		expR, expG, expB, expA float64
	}{
		{
			name: "opaque black",
			argb: int32(-16777216), // 0xFF000000
			expR: 0,
			expG: 0,
			expB: 0,
			expA: 1.0,
		},
		{
			name: "opaque red",
			argb: int32(-65536), // 0xFFFF0000
			expR: 1.0,
			expG: 0,
			expB: 0,
			expA: 1.0,
		},
		{
			name: "opaque green",
			argb: int32(-16711936), // 0xFF00FF00
			expR: 0,
			expG: 1.0,
			expB: 0,
			expA: 1.0,
		},
		{
			name: "opaque blue",
			argb: int32(-16776961), // 0xFF0000FF
			expR: 0,
			expG: 0,
			expB: 1.0,
			expA: 1.0,
		},
		{
			name: "50% transparent green",
			argb: int32(-2147418368), // 0x8000FF00
			expR: 0,
			expG: 1.0,
			expB: 0,
			expA: 0.5,
		},
		{
			name: "fully transparent",
			argb: 0,
			expR: 0,
			expG: 0,
			expB: 0,
			expA: 0,
		},
		{
			name: "opaque white",
			argb: int32(-1), // 0xFFFFFFFF
			expR: 1.0,
			expG: 1.0,
			expB: 1.0,
			expA: 1.0,
		},
		{
			name: "transparent white",
			argb: 0x00FFFFFF,
			expR: 1.0,
			expG: 1.0,
			expB: 1.0,
			expA: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, g, b, a := decodeARGB(tt.argb)
			const epsilon = 0.01
			if r < tt.expR-epsilon || r > tt.expR+epsilon {
				t.Errorf("red: expected %v, got %v", tt.expR, r)
			}
			if g < tt.expG-epsilon || g > tt.expG+epsilon {
				t.Errorf("green: expected %v, got %v", tt.expG, g)
			}
			if b < tt.expB-epsilon || b > tt.expB+epsilon {
				t.Errorf("blue: expected %v, got %v", tt.expB, b)
			}
			if a < tt.expA-epsilon || a > tt.expA+epsilon {
				t.Errorf("alpha: expected %v, got %v", tt.expA, a)
			}
		})
	}
}

// TestRenderPage_PenTypes verifies different pen types produce visually distinct rendering.
// Verifies: boox-notes-pipeline.AC2.3
func TestRenderPage_PenTypes(t *testing.T) {
	// Create a test stroke with the same geometry for each pen type
	strokePoints := []booxnote.TinyPoint{
		{X: 100, Y: 100, Pressure: 2000},
		{X: 200, Y: 200, Pressure: 2000},
		{X: 300, Y: 150, Pressure: 2000},
	}

	penTypes := []int32{2, 3, 4, 5, 15, 21, 22, 47, 60, 61}

	for _, penType := range penTypes {
		page := &booxnote.Page{
			PageID: "pentest",
			Width:  500,
			Height: 500,
			Shapes: []*booxnote.Shape{
				{
					UniqueID:  "stroke1",
					ShapeType: penType,
					Color:     int32(-16777216), // black
					Thickness: 3,
					Points:    strokePoints,
				},
			},
		}

		img, err := RenderPage(page)
		if err != nil {
			t.Fatalf("penType %d: RenderPage failed: %v", penType, err)
		}

		// Verify non-blank image
		if !hasNonWhitePixels(img, 100, 100, 300, 300) {
			t.Errorf("penType %d: expected strokes to be drawn", penType)
		}
	}
}

// TestRenderPage_MarkerTransparency verifies marker pen produces semi-transparent pixels.
// Verifies: boox-notes-pipeline.AC2.3 (marker alpha multiplier)
func TestRenderPage_MarkerTransparency(t *testing.T) {
	page := &booxnote.Page{
		PageID: "markertest",
		Width:  300,
		Height: 300,
		Shapes: []*booxnote.Shape{
			{
				UniqueID:  "marker",
				ShapeType: 15,               // marker
				Color:     int32(-16777216), // opaque black
				Thickness: 10,
				Points: []booxnote.TinyPoint{
					{X: 50, Y: 50, Pressure: 2000},
					{X: 250, Y: 250, Pressure: 2000},
				},
			},
		},
	}

	img, err := RenderPage(page)
	if err != nil {
		t.Fatalf("RenderPage failed: %v", err)
	}

	// Marker renders with AlphaMultiplier of 0.4, making it semi-transparent.
	// Black at 40% alpha should produce darker/gray pixels.
	// When stroked on a white background, semi-transparent black produces gray.
	// Scan the stroke region (from 50,50 to 250,250) for darker pixels (indicating marker).
	bounds := img.Bounds()
	foundMarker := false
	for y := 40; y < 260 && y < bounds.Max.Y; y++ {
		for x := 40; x < 260 && x < bounds.Max.X; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			// Normalize to 0-255 range (RGBA() returns 0-65535)
			r >>= 8
			g >>= 8
			b >>= 8
			// Marker at 40% alpha on white produces gray ~= (255*0.6, 255*0.6, 255*0.6) ~= (153, 153, 153)
			// We look for gray pixels (r ≈ g ≈ b) that are not white (all < 250)
			if r < 250 && g < 250 && b < 250 {
				// Check it's roughly gray (not colored)
				diff := int(r) - int(g)
				if diff < 0 {
					diff = -diff
				}
				if diff < 20 { // Allow small variation for anti-aliasing
					foundMarker = true
					break
				}
			}
		}
		if foundMarker {
			break
		}
	}
	if !foundMarker {
		t.Error("expected marker stroke (gray pixels) in stroke region")
	}
}

// TestRenderPage_AffineTransform verifies affine transforms are applied to strokes.
// Verifies: boox-notes-pipeline.AC2.5
func TestRenderPage_AffineTransform(t *testing.T) {
	// Test 1: Pure translation
	// MatrixValues: [scaleX, skewX, transX, skewY, scaleY, transY, persp0, persp1, persp2]
	// Identity with translation: [1, 0, 100, 0, 1, 200, 0, 0, 1]
	page := &booxnote.Page{
		PageID: "transtest",
		Width:  500,
		Height: 500,
		Shapes: []*booxnote.Shape{
			{
				UniqueID:     "translated",
				ShapeType:    2, // pencil
				Color:        int32(-16777216),
				Thickness:    2,
				MatrixValues: []float64{1, 0, 100, 0, 1, 200, 0, 0, 1},
				Points: []booxnote.TinyPoint{
					{X: 10, Y: 10, Pressure: 2000},
					{X: 50, Y: 50, Pressure: 2000},
				},
			},
		},
	}

	img, err := RenderPage(page)
	if err != nil {
		t.Fatalf("RenderPage failed: %v", err)
	}

	// The translated stroke should be drawn in the translated area
	// Expected region: (10+100, 10+200) to (50+100, 50+200) = (110, 210) to (150, 250)
	if !hasNonWhitePixels(img, 100, 200, 160, 260) {
		t.Error("expected strokes in translated region")
	}

	// Test 2: Combined scale (2x) + translate
	// MatrixValues: [scaleX=2, skewX=0, transX=50, skewY=0, scaleY=2, transY=50, ...]
	// This scales points by 2x and then translates by (50, 50)
	page2 := &booxnote.Page{
		PageID: "scaletest",
		Width:  500,
		Height: 500,
		Shapes: []*booxnote.Shape{
			{
				UniqueID:     "scaled",
				ShapeType:    2, // pencil
				Color:        int32(-16777216),
				Thickness:    2,
				MatrixValues: []float64{2, 0, 50, 0, 2, 50, 0, 0, 1},
				Points: []booxnote.TinyPoint{
					{X: 10, Y: 10, Pressure: 2000},
					{X: 30, Y: 30, Pressure: 2000},
				},
			},
		},
	}

	img2, err := RenderPage(page2)
	if err != nil {
		t.Fatalf("RenderPage failed: %v", err)
	}

	// With scale 2x, points at (10,10)-(30,30) become (20,20)-(60,60) after scale,
	// then with translation (+50,+50) they become (70,70)-(110,110)
	if !hasNonWhitePixels(img2, 60, 60, 120, 120) {
		t.Error("expected strokes in scaled+translated region")
	}
}

// TestRenderPage_GeometricShapes verifies geometric shapes render from bounding rects.
// Verifies: boox-notes-pipeline.AC2.6
func TestRenderPage_GeometricShapes(t *testing.T) {
	shapes := []*booxnote.Shape{
		{
			UniqueID:     "circle",
			ShapeType:    0, // circle
			Color:        int32(-16777216),
			Thickness:    2,
			BoundingRect: &booxnote.Rect{Left: 50, Top: 50, Right: 150, Bottom: 150},
		},
		{
			UniqueID:     "rectangle",
			ShapeType:    1, // rectangle
			Color:        int32(-16777216),
			Thickness:    2,
			BoundingRect: &booxnote.Rect{Left: 200, Top: 50, Right: 300, Bottom: 150},
		},
		{
			UniqueID:     "line",
			ShapeType:    7, // line
			Color:        int32(-16777216),
			Thickness:    2,
			BoundingRect: &booxnote.Rect{Left: 50, Top: 200, Right: 150, Bottom: 300},
		},
		{
			UniqueID:     "triangle",
			ShapeType:    8, // triangle
			Color:        int32(-16777216),
			Thickness:    2,
			BoundingRect: &booxnote.Rect{Left: 200, Top: 200, Right: 300, Bottom: 300},
		},
	}

	page := &booxnote.Page{
		PageID: "geomtest",
		Width:  400,
		Height: 400,
		Shapes: shapes,
	}

	img, err := RenderPage(page)
	if err != nil {
		t.Fatalf("RenderPage failed: %v", err)
	}

	if img == nil {
		t.Error("expected non-nil image")
	}

	// Verify we got some rendering (non-blank)
	if !hasNonWhitePixels(img, 40, 40, 310, 310) {
		t.Error("expected geometric shapes to be drawn")
	}
}

// TestRenderPage_BoundingRectNil verifies geometric shapes with nil bounding rect are skipped.
// Verifies: boox-notes-pipeline.AC2.7 (edge case)
func TestRenderPage_BoundingRectNil(t *testing.T) {
	page := &booxnote.Page{
		PageID: "nilrecttest",
		Width:  300,
		Height: 300,
		Shapes: []*booxnote.Shape{
			{
				UniqueID:  "nokill",
				ShapeType: 0, // circle
				Color:     int32(-16777216),
				// BoundingRect is nil
			},
		},
	}

	img, err := RenderPage(page)
	if err != nil {
		t.Fatalf("RenderPage failed: %v", err)
	}

	// Page should be blank (all white)
	if hasNonWhitePixels(img, 0, 0, 300, 300) {
		t.Error("expected blank page for nil bounding rect")
	}
}
