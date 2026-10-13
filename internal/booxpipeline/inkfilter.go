package booxpipeline

import (
	"image"
	"image/color"
	"math"
	"strconv"
	"strings"

	"github.com/sysop/ultrabridge/internal/booxnote"
)

// DefaultTodoInkColors is the ink the to-do pass extracts when the
// boox_todo_ink_colors setting is empty: the Boox "red" pen, measured from
// stroke data across a real library (0xFFFF6163 on every device surveyed).
// The visually similar orange-red pen (0xFFFF4400) is deliberately NOT here —
// it is a separate color the user writes with for other purposes.
var DefaultTodoInkColors = []uint32{0xFFFF6163}

// DefaultTodoPrompt is used when the boox_todo_prompt setting is empty. The
// image it is paired with has already been reduced to black-on-white strokes
// of the to-do ink, so the prompt never mentions color: asking a vision model
// to judge "redness" is what produced to-dos from notes with no red at all.
const DefaultTodoPrompt = `/no_think This image contains handwriting extracted from a notebook page; everything else has been removed. Transcribe each separate passage (a word, a line, or a group of lines that belong together). For each passage, return a JSON object on its own line exactly like: {"type":"todo","text":"the transcribed passage"}. Join lines within a passage with a single space. Do not add commentary. If the image is blank, return nothing.`

// ParseInkColors parses a comma- or whitespace-separated list of hex colors
// (RRGGBB or AARRGGBB, optional leading '#' or "0x") into packed ARGB values.
// Six-digit colors get an opaque alpha. Unparseable entries are skipped; an
// empty result falls back to DefaultTodoInkColors so a typo can't silently
// turn the to-do pass into a no-op.
func ParseInkColors(s string) []uint32 {
	var out []uint32
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		f = strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(f), "#"), "0x")
		if len(f) != 6 && len(f) != 8 {
			continue
		}
		v, err := strconv.ParseUint(f, 16, 32)
		if err != nil {
			continue
		}
		if len(f) == 6 {
			v |= 0xFF000000
		}
		out = append(out, uint32(v))
	}
	if len(out) == 0 {
		return DefaultTodoInkColors
	}
	return out
}

// inkOnlyPage returns a copy of page holding only the shapes drawn in one of
// colors, recolored opaque black so the rendered image carries no color cue.
// Returns nil when no shape matches — the caller skips the page entirely.
// Shape matching is exact on the packed ARGB value: Boox pens write a fixed
// palette color per stroke, so there is no tolerance to tune.
func inkOnlyPage(page *booxnote.Page, colors []uint32) *booxnote.Page {
	want := make(map[uint32]bool, len(colors))
	for _, c := range colors {
		want[c] = true
	}
	const black = int32(-16777216) // 0xFF000000
	var kept []*booxnote.Shape
	for _, s := range page.Shapes {
		if !want[uint32(s.Color)] {
			continue
		}
		cp := *s
		cp.Color = black
		if cp.FillColor != 0 {
			cp.FillColor = black
		}
		kept = append(kept, &cp)
	}
	if len(kept) == 0 {
		return nil
	}
	out := *page
	out.Shapes = kept
	return &out
}

// Pixel-filter tuning for raster sources (PDF exports), which carry no stroke
// colors. A pixel is modeled as white blended toward an ink color with
// coverage t; it is kept when it is far enough along that white→ink line to
// be ink rather than paper noise (t), and close enough to the line's
// direction. The direction test is angular, not a fixed RGB distance: an
// anti-aliased edge of a neighboring pen is only a short distance from the
// red line in absolute terms, but it points the wrong way — orange-red
// (0xFF4400) sits ~9° off red, while JPEG-shifted red stays within ~4°.
const (
	minInkCoverage   = 0.25  // ignore faint fringes and JPEG noise
	maxInkAngleTan   = 0.105 // tan(6°): allowed off-axis deviation per unit of ink
	inkResidualSlack = 4.0   // absolute RGB slack for quantization at low coverage
	minInkPixelsPage = 150   // fewer kept pixels than this = no ink on the page
)

// inkOnlyImage keeps the pixels of img that match one of colors, drawn as
// grayscale ink (darker = more coverage) on white, and returns the number of
// kept pixels. Matching along the white→ink line keeps anti-aliased edges
// while rejecting other hues: black ink's gray edges sit far from the red
// line, and orange-red (0xFF4400) diverges on the green/blue ratio.
func inkOnlyImage(img image.Image, colors []uint32) (*image.Gray, int) {
	type ink struct{ dr, dg, db, len2, len float64 }
	inks := make([]ink, 0, len(colors))
	for _, c := range colors {
		dr := 255 - float64((c>>16)&0xFF)
		dg := 255 - float64((c>>8)&0xFF)
		db := 255 - float64(c&0xFF)
		if l2 := dr*dr + dg*dg + db*db; l2 > 0 {
			inks = append(inks, ink{dr, dg, db, l2, math.Sqrt(l2)})
		}
	}

	b := img.Bounds()
	out := image.NewGray(b)
	kept := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r16, g16, b16, _ := img.At(x, y).RGBA()
			// Distance from white, per channel.
			pr := 255 - float64(r16>>8)
			pg := 255 - float64(g16>>8)
			pb := 255 - float64(b16>>8)
			best := 0.0
			for _, k := range inks {
				t := (pr*k.dr + pg*k.dg + pb*k.db) / k.len2
				if t < minInkCoverage {
					continue
				}
				if t > 1 {
					t = 1
				}
				er, eg, eb := pr-t*k.dr, pg-t*k.dg, pb-t*k.db
				allowed := maxInkAngleTan*t*k.len + inkResidualSlack
				if er*er+eg*eg+eb*eb <= allowed*allowed && t > best {
					best = t
				}
			}
			if best > 0 {
				out.SetGray(x, y, color.Gray{Y: uint8(255 * (1 - best))})
				kept++
			} else {
				out.SetGray(x, y, color.Gray{Y: 255})
			}
		}
	}
	return out, kept
}
