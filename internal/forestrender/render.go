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

	// ForestNote stores normalized pressure as millipressure, 0..1000, regardless
	// of the source digitizer's raw USI/EMR range.
	pressureMax = 1000.0

	// minVisibleWidth keeps thin/zero-pressure strokes legible for OCR.
	minVisibleWidth = 1.0

	// forestNoteHighlighterGray matches PenParams.HIGHLIGHTER_GRAY in ForestNote.
	// The Android app draws this color with DST_OVER so highlighter strokes sit
	// behind normal ink even when their z is later. The server renderer emulates
	// that by painting highlighter strokes before other strokes.
	forestNoteHighlighterGray = uint32(0xFFDCDCDC)

	// margin pads the legacy bounding-box renderer. Exact-geometry v5 pages do not use it.
	margin = 24

	// maxCanvas caps a runaway bounding box (defensive).
	maxCanvas = 20000

	// renderScale shrinks output coordinates. Legacy bounding-box pages render at
	// 1:1 then downscale; exact-geometry pages apply it directly so a normal
	// 10000x13333 page never allocates a roughly 500 MB intermediate canvas.
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
	Color         int64  // packed ARGB
	PenWidthMin   int64  // device units (treated as px for v1)
	PenWidthMax   int64  // device units
	Points        []byte // little-endian int32 array, 5 ints/point
	BrushKind     string
	BrushVersion  int64
	BrushSeed     int64
	PointDynamics []byte
	Z             int64 // draw order within the page
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
	return renderPage(strokes, boxes, 0, 0)
}

// RenderPageSized preserves the creator device's exact page rectangle. Off-page legacy content is
// clipped rather than expanding the canvas and reintroducing a letterbox-shaped bounding box.
func RenderPageSized(strokes []Stroke, boxes []TextBox, pageWidth, pageHeight int64) (image.Image, error) {
	return renderPage(strokes, boxes, pageWidth, pageHeight)
}

func renderPage(strokes []Stroke, boxes []TextBox, pageWidth, pageHeight int64) (image.Image, error) {
	type decoded struct {
		pts      []Point
		min, max int64
		r, g, b  float64
		behind   bool
		kind     string
	}
	// Draw in Z order so later strokes paint over earlier ones. Copy first to
	// avoid mutating the caller's slice.
	ordered := append([]Stroke(nil), strokes...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Z < ordered[j].Z })

	var ds []decoded
	minX, minY := int32(math.MaxInt32), int32(math.MaxInt32)
	maxX, maxY := int32(math.MinInt32), int32(math.MinInt32)
	sized := pageWidth > 0 && pageHeight > 0
	any := sized
	grow := func(x, y int32) {
		if sized {
			return
		}
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
		kind := s.BrushKind
		if kind == "" {
			kind = "fountain"
		}
		ds = append(ds, decoded{pts: pts, min: s.PenWidthMin, max: s.PenWidthMax, r: r, g: g, b: b,
			behind: kind == "highlighter" || isHighlighterColor(s.Color), kind: kind})
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
	coordScale := 1.0
	if sized {
		coordScale = renderScale
		w, h = clampCanvas(int(float64(pageWidth)*coordScale)), clampCanvas(int(float64(pageHeight)*coordScale))
		offX, offY = 0, 0
	}

	dc := gg.NewContext(w, h)
	dc.SetColor(color.White)
	dc.Clear()
	dc.SetLineCap(gg.LineCapRound)
	dc.SetLineJoin(gg.LineJoinRound)

	// Below-ink text boxes first.
	for _, b := range boxes {
		if b.Z == 0 {
			drawBox(dc, b, offX, offY, coordScale)
		}
	}

	drawStroke := func(d decoded) {
		opacity := brushOpacity(d.kind)
		dc.SetRGBA(d.r, d.g, d.b, opacity)
		if d.kind == "dashed" {
			dc.SetDash(80, 50)
		} else {
			dc.SetDash()
		}
		for i := 0; i < len(d.pts)-1; i++ {
			p0, p1 := d.pts[i], d.pts[i+1]
			pressure := (float64(p0.Pressure) + float64(p1.Pressure)) / 2.0
			dc.SetLineWidth(brushWidth(d.kind, pressure, d.min, d.max) * coordScale)
			dc.MoveTo((float64(p0.X)+offX)*coordScale, (float64(p0.Y)+offY)*coordScale)
			dc.LineTo((float64(p1.X)+offX)*coordScale, (float64(p1.Y)+offY)*coordScale)
			dc.Stroke()
		}
		dc.SetDash()
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
			drawBox(dc, b, offX, offY, coordScale)
		}
	}
	if sized {
		return dc.Image(), nil
	}
	return downscale(dc.Image()), nil
}

func brushWidth(kind string, pressure float64, lo, hi int64) float64 {
	switch kind {
	case "ballpoint", "fineliner":
		return math.Max(float64(lo+hi)/2, minVisibleWidth)
	case "marker", "translucent_marker", "highlighter":
		return math.Max(float64(hi), minVisibleWidth)
	default:
		return pressureToWidth(pressure, lo, hi)
	}
}

func brushOpacity(kind string) float64 {
	switch kind {
	case "translucent_marker":
		return 0.31
	case "marker":
		return 0.75
	case "pencil_hb":
		return 0.90
	case "pencil_2b":
		return 0.82
	case "pencil_4b":
		return 0.72
	case "pencil_6b":
		return 0.62
	case "pencil_8b":
		return 0.54
	default:
		return 1
	}
}

// drawBox paints one text box: an optional border rect, then the wrapped text
// clipped to the box (overflow is retained in the data, not drawn — matching the
// client). Colors come from the packed ARGB exactly as strokes decode it.
func drawBox(dc *gg.Context, b TextBox, offX, offY, scale float64) {
	x := (float64(b.X) + offX) * scale
	y := (float64(b.Y) + offY) * scale
	w, h := float64(b.Width)*scale, float64(b.Height)*scale
	r, g, bl, a := decodeARGB(int32(b.Color))

	if b.BorderWidth > 0 {
		dc.SetRGBA(r, g, bl, a)
		dc.SetLineWidth(float64(b.BorderWidth) * scale)
		dc.DrawRectangle(x, y, w, h)
		dc.Stroke()
	}
	if b.Text == "" {
		return
	}
	dc.DrawRectangle(x, y, w, h)
	dc.Clip()
	dc.SetRGBA(r, g, bl, a)
	dc.SetFontFace(faceFor(b.Weight, int64(float64(b.FontSize)*scale)))
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
