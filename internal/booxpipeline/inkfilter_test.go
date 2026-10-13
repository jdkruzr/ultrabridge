package booxpipeline

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/sysop/ultrabridge/internal/booxnote"
	pb "github.com/sysop/ultrabridge/internal/booxnote/proto"
	"github.com/sysop/ultrabridge/internal/booxnote/testutil"
	"github.com/sysop/ultrabridge/internal/notedb"
)

const (
	argbBlack     = int32(-16777216) // 0xFF000000
	argbRed       = int32(-40605)    // 0xFFFF6163, the Boox red pen
	argbOrangeRed = int32(-48128)    // 0xFFFF4400, a separate pen
)

func TestParseInkColors(t *testing.T) {
	cases := []struct {
		in   string
		want []uint32
	}{
		{"", DefaultTodoInkColors},
		{"   ", DefaultTodoInkColors},
		{"garbage, #12", DefaultTodoInkColors},
		{"FF6163", []uint32{0xFFFF6163}},
		{"#ff6163", []uint32{0xFFFF6163}},
		{"0xFFFF6163", []uint32{0xFFFF6163}},
		{"80FF6163", []uint32{0x80FF6163}},
		{"FF6163, FF4400", []uint32{0xFFFF6163, 0xFFFF4400}},
		{"FF6163 nope 00AAFF", []uint32{0xFFFF6163, 0xFF00AAFF}},
	}
	for _, c := range cases {
		got := ParseInkColors(c.in)
		if len(got) != len(c.want) {
			t.Errorf("ParseInkColors(%q) = %X, want %X", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("ParseInkColors(%q) = %X, want %X", c.in, got, c.want)
				break
			}
		}
	}
}

func TestInkOnlyPage(t *testing.T) {
	page := &booxnote.Page{
		PageID: "p", Width: 100, Height: 100,
		Shapes: []*booxnote.Shape{
			{UniqueID: "black", Color: argbBlack},
			{UniqueID: "red", Color: argbRed, FillColor: argbRed},
			{UniqueID: "orange", Color: argbOrangeRed},
			{UniqueID: "red2", Color: argbRed},
		},
	}

	got := inkOnlyPage(page, DefaultTodoInkColors)
	if got == nil {
		t.Fatal("inkOnlyPage returned nil for a page with red strokes")
	}
	if len(got.Shapes) != 2 || got.Shapes[0].UniqueID != "red" || got.Shapes[1].UniqueID != "red2" {
		t.Fatalf("kept shapes = %v, want [red red2]", shapeIDs(got.Shapes))
	}
	for _, s := range got.Shapes {
		if s.Color != argbBlack {
			t.Errorf("shape %s color = %X, want recolored to black", s.UniqueID, uint32(s.Color))
		}
	}
	if got.Shapes[0].FillColor != argbBlack {
		t.Errorf("fill color = %X, want recolored to black", uint32(got.Shapes[0].FillColor))
	}
	// The source page must be untouched: the cached render and index pass use it.
	if page.Shapes[1].Color != argbRed || len(page.Shapes) != 4 {
		t.Error("inkOnlyPage mutated its input page")
	}

	if inkOnlyPage(&booxnote.Page{Shapes: []*booxnote.Shape{{Color: argbBlack}, {Color: argbOrangeRed}}}, DefaultTodoInkColors) != nil {
		t.Error("inkOnlyPage should return nil when no shape is in the to-do ink")
	}
	// Orange-red is selectable when explicitly configured.
	if inkOnlyPage(&booxnote.Page{Shapes: []*booxnote.Shape{{Color: argbOrangeRed}}}, []uint32{0xFFFF4400}) == nil {
		t.Error("configured orange-red ink was not selected")
	}
}

func shapeIDs(shapes []*booxnote.Shape) []string {
	ids := make([]string, len(shapes))
	for i, s := range shapes {
		ids[i] = s.UniqueID
	}
	return ids
}

func TestInkOnlyImage(t *testing.T) {
	// One pixel per sample color, measured or derived from real Boox PDF renders.
	samples := []struct {
		name string
		c    color.RGBA
		keep bool
	}{
		{"red core (measured)", color.RGBA{255, 92, 95, 255}, true},
		{"red exact", color.RGBA{255, 97, 99, 255}, true},
		{"red half-coverage edge", color.RGBA{255, 176, 177, 255}, true},
		{"red jpeg-shifted", color.RGBA{240, 96, 96, 255}, true},
		{"red noisy mid edge", color.RGBA{250, 170, 180, 255}, true},
		{"white", color.RGBA{255, 255, 255, 255}, false},
		{"faint red fringe", color.RGBA{255, 240, 240, 255}, false},
		{"black", color.RGBA{0, 0, 0, 255}, false},
		{"gray edge of black ink", color.RGBA{128, 128, 128, 255}, false},
		{"orange-red pen", color.RGBA{255, 68, 0, 255}, false},
		{"orange-red edge", color.RGBA{255, 160, 128, 255}, false},
		{"orange-red faint edge", color.RGBA{255, 199, 178, 255}, false},
		{"yellow highlighter", color.RGBA{240, 255, 0, 255}, false},
		{"green pen", color.RGBA{0, 176, 54, 255}, false},
		{"blue pen", color.RGBA{0, 0, 132, 255}, false},
	}
	img := image.NewRGBA(image.Rect(0, 0, len(samples), 1))
	for i, s := range samples {
		img.Set(i, 0, s.c)
	}

	out, kept := inkOnlyImage(img, DefaultTodoInkColors)
	wantKept := 0
	for i, s := range samples {
		got := out.GrayAt(i, 0).Y < 255
		if got != s.keep {
			t.Errorf("%s %v: kept=%v, want %v", s.name, s.c, got, s.keep)
		}
		if s.keep {
			wantKept++
		}
	}
	if kept != wantKept {
		t.Errorf("kept count = %d, want %d", kept, wantKept)
	}
	// Full-coverage ink renders darker than a half-coverage edge.
	if out.GrayAt(1, 0).Y >= out.GrayAt(2, 0).Y {
		t.Errorf("exact red (%d) should be darker than its edge (%d)", out.GrayAt(1, 0).Y, out.GrayAt(2, 0).Y)
	}
}

// recordingOCR is an OCRer that records the prompt of every call.
type recordingOCR struct {
	mu      sync.Mutex
	prompts []string
	images  [][]byte
	resp    func(prompt string) string
}

func (r *recordingOCR) Recognize(_ context.Context, jpegData []byte, prompt string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prompts = append(r.prompts, prompt)
	r.images = append(r.images, jpegData)
	return r.resp(prompt), nil
}

func (r *recordingOCR) todoCalls() (n int, imgs [][]byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, p := range r.prompts {
		if p == DefaultTodoPrompt {
			n++
			imgs = append(imgs, r.images[i])
		}
	}
	return n, imgs
}

func strokeShape(id string, argb int32) *pb.ShapeInfoProto {
	return &pb.ShapeInfoProto{UniqueId: id, ShapeType: 5, Color: argb, Thickness: 4.0}
}

func strokePoints(x0, y0 float32) []booxnote.TinyPoint {
	pts := make([]booxnote.TinyPoint, 0, 40)
	for i := 0; i < 40; i++ {
		pts = append(pts, booxnote.TinyPoint{X: x0 + float32(i)*5, Y: y0, Size: 1, Pressure: 2000, Time: uint32(i)})
	}
	return pts
}

// runNoteTodoPass processes a synthetic one-page note end to end with to-do
// extraction on, and returns the OCR recorder and the todos handed onward.
func runNoteTodoPass(t *testing.T, shapes []*pb.ShapeInfoProto, points map[string][]booxnote.TinyPoint) (*recordingOCR, []TodoItem) {
	t.Helper()
	notePath := testutil.BuildTestNoteFile(t, t.TempDir(), testutil.TestNoteOpts{
		NoteID: "ink-note", Title: "Ink Test",
		Pages: []*testutil.TestPage{{PageID: "page-1", Width: 600, Height: 400, Shapes: shapes, Points: points}},
	})

	db, err := notedb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	ocr := &recordingOCR{resp: func(prompt string) string {
		if prompt == DefaultTodoPrompt {
			return `{"type":"todo","text":"Call the vendor"}`
		}
		return "page text"
	}}
	var mu sync.Mutex
	var found []TodoItem
	notesPath := t.TempDir()
	proc := New(db, notesPath, WorkerConfig{
		Indexer:        &mockIndexer{},
		ContentDeleter: &mockContentDeleter{},
		OCR:            ocr,
		CachePath:      notesPath + "/.cache",
		TodoEnabled:    func() bool { return true },
		TodoPrompt:     func() string { return "" }, // unset setting → DefaultTodoPrompt
		OnTodosFound: func(_ context.Context, _ string, todos []TodoItem) {
			mu.Lock()
			found = append(found, todos...)
			mu.Unlock()
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := proc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer proc.Stop()
	if err := proc.Enqueue(context.Background(), notePath); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if !waitForJobStatus(t, db, notePath, "done", 10*time.Second) {
		t.Fatal("job did not complete in time")
	}
	proc.Stop()
	mu.Lock()
	defer mu.Unlock()
	return ocr, found
}

func TestTodoPass_SkipsPagesWithoutToDoInk(t *testing.T) {
	ocr, found := runNoteTodoPass(t,
		[]*pb.ShapeInfoProto{strokeShape("black", argbBlack), strokeShape("orange", argbOrangeRed)},
		map[string][]booxnote.TinyPoint{"black": strokePoints(50, 100), "orange": strokePoints(50, 200)})

	if n, _ := ocr.todoCalls(); n != 0 {
		t.Errorf("todo OCR calls = %d, want 0 for a page with no red strokes", n)
	}
	if len(found) != 0 {
		t.Errorf("todos = %v, want none", found)
	}
	if len(ocr.prompts) != 1 {
		t.Errorf("total OCR calls = %d, want 1 (the indexing pass only)", len(ocr.prompts))
	}
}

func TestTodoPass_SendsOnlyToDoInkBlackOnWhite(t *testing.T) {
	ocr, found := runNoteTodoPass(t,
		[]*pb.ShapeInfoProto{strokeShape("black", argbBlack), strokeShape("red", argbRed)},
		map[string][]booxnote.TinyPoint{"black": strokePoints(50, 100), "red": strokePoints(50, 300)})

	n, imgs := ocr.todoCalls()
	if n != 1 {
		t.Fatalf("todo OCR calls = %d, want 1", n)
	}
	if len(found) != 1 || found[0].Text != "Call the vendor" {
		t.Errorf("todos = %v, want the transcribed passage", found)
	}

	img, err := jpeg.Decode(bytes.NewReader(imgs[0]))
	if err != nil {
		t.Fatalf("decode todo image: %v", err)
	}
	// The black stroke at y=100 must be gone; the red one at y=300 present
	// and drawn without color.
	if ink := darkestIn(img, 100); ink < 230 {
		t.Errorf("black-ink row still has ink (darkest gray %d) in the to-do image", ink)
	}
	if ink := darkestIn(img, 300); ink > 100 {
		t.Errorf("red-ink row darkest gray = %d, want dark ink", ink)
	}
	if colored := maxChroma(img); colored > 40 {
		t.Errorf("to-do image carries color (max chroma %d); the model should see black on white only", colored)
	}
}

// darkestIn returns the minimum luminance across rows y-6..y+6.
func darkestIn(img image.Image, y int) uint8 {
	min := uint8(255)
	b := img.Bounds()
	for yy := y - 6; yy <= y+6; yy++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			g := color.GrayModel.Convert(img.At(x, yy)).(color.Gray).Y
			if g < min {
				min = g
			}
		}
	}
	return min
}

func maxChroma(img image.Image) int {
	max := 0
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			hi, lo := int(r>>8), int(r>>8)
			for _, v := range []int{int(g >> 8), int(bl >> 8)} {
				if v > hi {
					hi = v
				}
				if v < lo {
					lo = v
				}
			}
			if hi-lo > max {
				max = hi - lo
			}
		}
	}
	return max
}
