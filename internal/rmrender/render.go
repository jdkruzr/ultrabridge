package rmrender

import (
	"image"
	"image/color"
	"math"

	"github.com/fogleman/gg"
)

const (
	defaultWidth  = 1404
	defaultHeight = 1872
)

// RenderPage renders a parsed page on a white canvas.
func RenderPage(p Page) (image.Image, error) {
	return RenderPageOn(p, nil)
}

// RenderPageOn renders a parsed page over an optional base image, typically a
// rasterized source PDF page.
func RenderPageOn(p Page, base image.Image) (image.Image, error) {
	w, h := p.Width, p.Height
	if base != nil {
		b := base.Bounds()
		w, h = b.Dx(), b.Dy()
	} else if w <= 0 || h <= 0 {
		w, h = defaultWidth, defaultHeight
	}

	dc := gg.NewContext(w, h)
	dc.SetColor(color.White)
	dc.Clear()
	if base != nil {
		dc.DrawImage(base, 0, 0)
	}

	for _, st := range p.Strokes {
		drawStroke(dc, st, w, h, p.CenteredX)
	}
	return dc.Image(), nil
}

func drawStroke(dc *gg.Context, st Stroke, canvasW, canvasH int, centeredX bool) {
	if len(st.Points) < 2 {
		return
	}
	r, g, b, a := strokeColor(st)
	dc.SetRGBA(r, g, b, a)
	dc.SetLineCap(gg.LineCapRound)
	dc.SetLineJoin(gg.LineJoinRound)

	for i := 0; i < len(st.Points)-1; i++ {
		p0, p1 := st.Points[i], st.Points[i+1]
		dc.SetLineWidth(strokeWidth(st, p0, p1))
		x0, y0 := mapPoint(p0, canvasW, canvasH, centeredX)
		x1, y1 := mapPoint(p1, canvasW, canvasH, centeredX)
		dc.MoveTo(x0, y0)
		dc.LineTo(x1, y1)
		dc.Stroke()
	}
}

func mapPoint(p Point, canvasW, canvasH int, centeredX bool) (float64, float64) {
	x, y := p.X, p.Y
	// v6 line coordinates use a centered x-axis (-702..702) for the 1404px
	// portrait page, so shift them onto the raster canvas.
	if centeredX {
		x += defaultWidth / 2
	}
	sx := float64(canvasW) / defaultWidth
	sy := float64(canvasH) / defaultHeight
	return x * sx, y * sy
}

func strokeWidth(st Stroke, p0, p1 Point) float64 {
	raw := (float64(p0.Width) + float64(p1.Width)) / 2
	if raw <= 0 {
		raw = 8
	}
	width := raw * 0.10 * math.Max(st.ThicknessScale, 0.25)
	switch st.PenType {
	case 5, 18: // highlighter
		width *= 1.7
	case 6, 8: // eraser
		width *= 2
	case 1, 7, 13, 14: // pencils
		width *= 0.8
	}
	return math.Max(width, 0.8)
}

func strokeColor(st Stroke) (r, g, b, a float64) {
	if st.ColorARGB != nil {
		v := *st.ColorARGB
		a = float64((v>>24)&0xff) / 255
		r = float64((v>>16)&0xff) / 255
		g = float64((v>>8)&0xff) / 255
		b = float64(v&0xff) / 255
		if a == 0 {
			a = 1
		}
		return r, g, b, a
	}
	switch st.PenType {
	case 6, 8:
		return 1, 1, 1, 1
	}
	switch st.Color {
	case 1, 8:
		return 0.56, 0.56, 0.56, 1
	case 2:
		return 1, 1, 1, 1
	case 3, 9, 13:
		return 0.98, 0.91, 0.10, highlighterAlpha(st)
	case 4, 10:
		return 0.57, 0.85, 0.44, highlighterAlpha(st)
	case 5, 12:
		return 0.75, 0.50, 0.82, highlighterAlpha(st)
	case 6:
		return 0.19, 0.29, 0.88, 1
	case 7:
		return 0.76, 0.19, 0.20, 1
	case 11:
		return 0.45, 0.82, 0.91, highlighterAlpha(st)
	default:
		return 0, 0, 0, 1
	}
}

func highlighterAlpha(st Stroke) float64 {
	if st.PenType == 5 || st.PenType == 18 || st.PenType == 23 {
		return 0.45
	}
	return 1
}
