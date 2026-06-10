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
	xdraw "golang.org/x/image/draw"
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

	// forestNoteHighlighterGray matches PenParams.HIGHLIGHTER_GRAY in ForestNote.
	// The Android app draws this color with DST_OVER so highlighter strokes sit
	// behind normal ink even when their z is later. The server renderer emulates
	// that by painting highlighter strokes before other strokes.
	forestNoteHighlighterGray = uint32(0xFFDCDCDC)

	// margin pads the rendered bounding box (px). ForestNote has no page
	// width/height yet, so v1 renders from the stroke extent (§9 open item).
	margin = 24

	// maxCanvas caps a runaway bounding box (defensive).
	maxCanvas = 20000

	// renderScale shrinks the final page image. The canvas is built at 1:1 with
	// the virtual-unit coordinate space (short axis = 10,000), which yields a
	// ~10000px-tall, ~100-megapixel, multi-MB JPEG — needlessly slow to OCR,
	// transfer, and display. We render at full resolution, then downscale the
	// finished image by this factor so strokes AND text-box glyphs shrink
	// uniformly (scaling gg's draw ops directly does not scale rasterized text).
	// 0.5 = half each dimension ≈ a quarter of the pixels/bytes. 1.0 disables.
	renderScale = 0.5
)

// downscale resamples img by renderScale with a high-quality filter (kept sharp
// for OCR legibility). A no-op when renderScale == 1.
func downscale(img image.Image) image.Image {
	if renderScale == 1.0 {
		return img
	}
	b := img.Bounds()
	w := int(float64(b.Dx()) * renderScale)
	h := int(float64(b.Dy()) * renderScale)
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, b, xdraw.Over, nil)
	return dst
}

// Stroke is one renderable stroke. The bridge maps a fn_stroke mirror row onto
// this; forestrender does not import syncstore (keeps rendering dependency-free).
type Stroke struct {
	Color       int64  // packed ARGB
	PenWidthMin int64  // device units (treated as px for v1)
	PenWidthMax int64  // device units
	Points      []byte // little-endian int32 array, 5 ints/point
	Z           int64  // draw order within the page
}

// TextBox is one renderable text box. The bridge maps a fn_text_box mirror row
// onto this. Geometry and FontSize are in the SAME virtual-unit space as stroke
// points (page short axis = 10,000), so the renderer draws them 1:1 alongside the
// ink with no separate scale. Z is the paint band: 0 = below ink, 1 = above ink.
type TextBox struct {
	X, Y, Width, Height int64
	Text                string
	FontName            string // tablet basename; not resolvable server-side (see fonts.go)
	FontSize            int64
	Color               int64 // packed ARGB (unsigned int64, like Stroke.Color)
	Weight              int64 // 400 = normal, 700 = bold
	BorderWidth         int64 // px; 0 = no border
	Z                   int64 // 0 = below ink, 1 = above ink
}

// lineSpacing is the wrapped-text line height multiple for box text.
const lineSpacing = 1.3

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

// RenderPage renders a page's strokes and text boxes onto a white canvas sized to
// their combined bounding box plus a margin. Strokes and boxes share one virtual
// coordinate space, so both draw 1:1. Paint order matches the client: below-ink
// boxes (z==0), then ink (in Z order), then above-ink boxes (z==1). A page with no
// strokes and no boxes yields a small blank white image and no error.
func RenderPage(strokes []Stroke, boxes []TextBox) (image.Image, error) {
	type decoded struct {
		pts      []Point
		min, max int64
		r, g, b  float64
		behind   bool
	}
	// Draw in Z order so later strokes paint over earlier ones. Copy first to
	// avoid mutating the caller's slice.
	ordered := append([]Stroke(nil), strokes...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Z < ordered[j].Z })

	var ds []decoded
	minX, minY := int32(math.MaxInt32), int32(math.MaxInt32)
	maxX, maxY := int32(math.MinInt32), int32(math.MinInt32)
	any := false
	grow := func(x, y int32) {
		any = true
		if x < minX {
			minX = x
		}
		if y < minY {
			minY = y
		}
		if x > maxX {
			maxX = x
		}
		if y > maxY {
			maxY = y
		}
	}

	for _, s := range ordered {
		pts := DecodePoints(s.Points)
		if len(pts) < 2 {
			continue // a single point draws nothing legible; skip (matches booxrender)
		}
		r, g, b, _ := decodeARGB(int32(s.Color))
		ds = append(ds, decoded{pts: pts, min: s.PenWidthMin, max: s.PenWidthMax, r: r, g: g, b: b, behind: isHighlighterColor(s.Color)})
		for _, p := range pts {
			grow(p.X, p.Y)
		}
	}

	// Expand the box to include every text box rect, so a box outside the ink
	// extent — or a page with boxes and no ink at all — still fits on the canvas.
	for _, b := range boxes {
		grow(clampI32(b.X), clampI32(b.Y))
		grow(clampI32(b.X+b.Width), clampI32(b.Y+b.Height))
	}

	if !any {
		dc := gg.NewContext(100, 100)
		dc.SetColor(color.White)
		dc.Clear()
		return downscale(dc.Image()), nil
	}

	w := clampCanvas(int(maxX-minX) + 2*margin)
	h := clampCanvas(int(maxY-minY) + 2*margin)
	offX, offY := float64(margin-int(minX)), float64(margin-int(minY))

	dc := gg.NewContext(w, h)
	dc.SetColor(color.White)
	dc.Clear()
	dc.SetLineCap(gg.LineCapRound)
	dc.SetLineJoin(gg.LineJoinRound)

	// Below-ink text boxes first.
	for _, b := range boxes {
		if b.Z == 0 {
			drawBox(dc, b, offX, offY)
		}
	}

	drawStroke := func(d decoded) {
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

	for _, d := range ds {
		if d.behind {
			drawStroke(d)
		}
	}
	for _, d := range ds {
		if !d.behind {
			drawStroke(d)
		}
	}

	// Above-ink text boxes last.
	for _, b := range boxes {
		if b.Z != 0 {
			drawBox(dc, b, offX, offY)
		}
	}
	return downscale(dc.Image()), nil
}

// drawBox paints one text box: an optional border rect, then the wrapped text
// clipped to the box (overflow is retained in the data, not drawn — matching the
// client). Colors come from the packed ARGB exactly as strokes decode it.
func drawBox(dc *gg.Context, b TextBox, offX, offY float64) {
	x := float64(b.X) + offX
	y := float64(b.Y) + offY
	w, h := float64(b.Width), float64(b.Height)
	r, g, bl, a := decodeARGB(int32(b.Color))

	if b.BorderWidth > 0 {
		dc.SetRGBA(r, g, bl, a)
		dc.SetLineWidth(float64(b.BorderWidth))
		dc.DrawRectangle(x, y, w, h)
		dc.Stroke()
	}
	if b.Text == "" {
		return
	}
	dc.DrawRectangle(x, y, w, h)
	dc.Clip()
	dc.SetRGBA(r, g, bl, a)
	dc.SetFontFace(faceFor(b.Weight, b.FontSize))
	dc.DrawStringWrapped(b.Text, x, y, 0, 0, w, lineSpacing, gg.AlignLeft)
	dc.ResetClip()
}

// clampI32 narrows an int64 box coordinate to int32 (the bounding-box accumulator
// type), saturating rather than overflowing on absurd input.
func clampI32(v int64) int32 {
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	if v < math.MinInt32 {
		return math.MinInt32
	}
	return int32(v)
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

func isHighlighterColor(color int64) bool {
	return uint32(int32(color)) == forestNoteHighlighterGray
}
