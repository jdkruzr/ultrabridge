// Package forestrender renders ForestNote strokes to an image for the OCR /
// search / RAG pipeline. It is separate from internal/booxrender because the
// formats differ: ForestNote points are a little-endian int32 array (5 ints per
// point [x, y, pressure, tsHi, tsLo]) with per-stroke color + pen_width_min/max,
// whereas booxrender consumes Boox's big-endian TinyPoint + ARGB/Thickness model.
// See docs/sync/forestnote-sync-protocol.md §2.
package forestrender

import (
	"encoding/binary"
	"image"
	"image/color"
	"math"
	"sort"

	"github.com/fogleman/gg"
)

const (
	// intsPerPoint is the wire layout: [x, y, pressure, tsHi, tsLo] (spec §2).
	intsPerPoint = 5
	bytesPerInt  = 4
	bytesPerPt   = intsPerPoint * bytesPerInt

	// pressureMax normalizes pressure to 0..1. ForestNote's pressure scale is not
	// yet confirmed (open item: docs/sync/forestnote-sync-protocol.md §9); 4095 is
	// the common EMR-digitizer full-scale. Over-estimating only widens strokes
	// slightly — harmless for the v1 OCR/search payoff.
	pressureMax = 4095.0

	// minVisibleWidth keeps thin/zero-pressure strokes legible for OCR.
	minVisibleWidth = 1.0

	// margin pads the rendered bounding box (px). ForestNote has no page
	// width/height yet, so v1 renders from the stroke extent (§9 open item).
	margin = 24

	// maxCanvas caps a runaway bounding box (defensive).
	maxCanvas = 20000
)

// Stroke is one renderable stroke. The bridge maps a fn_stroke mirror row onto
// this; forestrender does not import syncstore (keeps rendering dependency-free).
type Stroke struct {
	Color       int64  // packed ARGB
	PenWidthMin int64  // device units (treated as px for v1)
	PenWidthMax int64  // device units
	Points      []byte // little-endian int32 array, 5 ints/point
	Z           int64  // draw order within the page
}

// Point is a decoded stroke sample.
type Point struct{ X, Y, Pressure int32 }

// DecodePoints parses the LE int32 point blob. Trailing bytes that don't form a
// whole point are ignored (tolerant, like booxrender skipping short shapes).
func DecodePoints(b []byte) []Point {
	n := len(b) / bytesPerPt
	pts := make([]Point, 0, n)
	for i := 0; i < n; i++ {
		off := i * bytesPerPt
		pts = append(pts, Point{
			X:        int32(binary.LittleEndian.Uint32(b[off : off+4])),
			Y:        int32(binary.LittleEndian.Uint32(b[off+4 : off+8])),
			Pressure: int32(binary.LittleEndian.Uint32(b[off+8 : off+12])),
		})
	}
	return pts
}

// RenderPage renders strokes (any order; sorted by Z here) onto a white canvas
// sized to their bounding box plus a margin. An empty/strokeless page yields a
// small blank white image and no error.
func RenderPage(strokes []Stroke) (image.Image, error) {
	type decoded struct {
		pts      []Point
		min, max int64
		r, g, b  float64
	}
	// Draw in Z order so later strokes paint over earlier ones. Copy first to
	// avoid mutating the caller's slice.
	ordered := append([]Stroke(nil), strokes...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Z < ordered[j].Z })

	var ds []decoded
	minX, minY := int32(math.MaxInt32), int32(math.MaxInt32)
	maxX, maxY := int32(math.MinInt32), int32(math.MinInt32)
	any := false

	for _, s := range ordered {
		pts := DecodePoints(s.Points)
		if len(pts) < 2 {
			continue // a single point draws nothing legible; skip (matches booxrender)
		}
		r, g, b, _ := decodeARGB(int32(s.Color))
		ds = append(ds, decoded{pts: pts, min: s.PenWidthMin, max: s.PenWidthMax, r: r, g: g, b: b})
		for _, p := range pts {
			any = true
			if p.X < minX {
				minX = p.X
			}
			if p.Y < minY {
				minY = p.Y
			}
			if p.X > maxX {
				maxX = p.X
			}
			if p.Y > maxY {
				maxY = p.Y
			}
		}
	}

	if !any {
		dc := gg.NewContext(100, 100)
		dc.SetColor(color.White)
		dc.Clear()
		return dc.Image(), nil
	}

	w := clampCanvas(int(maxX-minX) + 2*margin)
	h := clampCanvas(int(maxY-minY) + 2*margin)
	offX, offY := float64(margin-int(minX)), float64(margin-int(minY))

	dc := gg.NewContext(w, h)
	dc.SetColor(color.White)
	dc.Clear()
	dc.SetLineCap(gg.LineCapRound)
	dc.SetLineJoin(gg.LineJoinRound)

	for _, d := range ds {
		dc.SetRGB(d.r, d.g, d.b)
		for i := 0; i < len(d.pts)-1; i++ {
			p0, p1 := d.pts[i], d.pts[i+1]
			pressure := (float64(p0.Pressure) + float64(p1.Pressure)) / 2.0
			dc.SetLineWidth(pressureToWidth(pressure, d.min, d.max))
			dc.MoveTo(float64(p0.X)+offX, float64(p0.Y)+offY)
			dc.LineTo(float64(p1.X)+offX, float64(p1.Y)+offY)
			dc.Stroke()
		}
	}
	return dc.Image(), nil
}

// pressureToWidth maps pressure (0..pressureMax) linearly into [min, max] px,
// clamped to a minimum visible width.
func pressureToWidth(pressure float64, min, max int64) float64 {
	lo, hi := float64(min), float64(max)
	if hi < lo {
		lo, hi = hi, lo
	}
	norm := math.Min(math.Max(pressure/pressureMax, 0), 1)
	return math.Max(lo+norm*(hi-lo), minVisibleWidth)
}

func clampCanvas(v int) int {
	if v < 1 {
		return 1
	}
	if v > maxCanvas {
		return maxCanvas
	}
	return v
}
