package booxnote

import (
	"bytes"
	"encoding/json"
	"testing"

	pb "github.com/sysop/ultrabridge/internal/booxnote/proto"
)

// TestOpen_ExtractsTitle verifies AC1.1: Parser extracts note title from note_info protobuf.
func TestOpen_ExtractsTitle(t *testing.T) {
	reader := buildTestNote(t, noteOpts{
		NoteID: "note123",
		Title:  "My Test Note",
		Pages: []*testPage{
			{
				PageID: "page1",
				Width:  1860,
				Height: 2480,
			},
		},
	})

	size, err := reader.Seek(0, 2) // Seek to end to get size
	if err != nil {
		t.Fatalf("Seek failed: %v", err)
	}
	_, err = reader.Seek(0, 0) // Reset to beginning
	if err != nil {
		t.Fatalf("Seek failed: %v", err)
	}
	note, err := Open(reader, size)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	if note.Title != "My Test Note" {
		t.Errorf("got title %q, want %q", note.Title, "My Test Note")
	}
}

// TestOpen_ExtractsPageDimensions verifies AC1.2: Parser extracts page list with dimensions.
func TestOpen_ExtractsPageDimensions(t *testing.T) {
	reader := buildTestNote(t, noteOpts{
		NoteID: "note123",
		Title:  "Test",
		Pages: []*testPage{
			{
				PageID: "page1",
				Width:  1860,
				Height: 2480,
			},
		},
	})

	size, err := reader.Seek(0, 2) // Seek to end to get size
	if err != nil {
		t.Fatalf("Seek failed: %v", err)
	}
	_, err = reader.Seek(0, 0) // Reset to beginning
	if err != nil {
		t.Fatalf("Seek failed: %v", err)
	}
	note, err := Open(reader, size)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	if len(note.Pages) != 1 {
		t.Fatalf("got %d pages, want 1", len(note.Pages))
	}

	page := note.Pages[0]
	if page.Width != 1860 {
		t.Errorf("got width %.0f, want 1860", page.Width)
	}
	if page.Height != 2480 {
		t.Errorf("got height %.0f, want 2480", page.Height)
	}
}

// TestOpen_DeserializesShapes verifies AC1.3: Parser deserializes ShapeInfoProtoList
// with shapeType, color, thickness, boundingRect, matrixValues.
func TestOpen_DeserializesShapes(t *testing.T) {
	boundingRect := map[string]float64{
		"left":   10.0,
		"top":    20.0,
		"right":  100.0,
		"bottom": 200.0,
	}
	boundingRectJSON, err := json.Marshal(boundingRect)
	if err != nil {
		t.Fatalf("marshal boundingRect: %v", err)
	}

	matrixValues := []float64{1.0, 0.0, 0.0, 1.0, 50.0, 75.0}
	matrixValuesJSON, err := json.Marshal(matrixValues)
	if err != nil {
		t.Fatalf("marshal matrixValues: %v", err)
	}

	shapes := []*pb.ShapeInfoProto{
		{
			UniqueId:     "shape1",
			ShapeType:    1,
			Color:        -16777216, // 0xFF000000 as signed int32
			FillColor:    -16776961, // 0xFF0000FF as signed int32
			Thickness:    2.5,
			Zorder:       1,
			BoundingRect: string(boundingRectJSON),
			MatrixValues: string(matrixValuesJSON),
		},
	}

	reader := buildTestNote(t, noteOpts{
		NoteID: "note123",
		Title:  "Test",
		Pages: []*testPage{
			{
				PageID: "page1",
				Width:  1860,
				Height: 2480,
				Shapes: shapes,
			},
		},
	})

	size, err := reader.Seek(0, 2) // Seek to end to get size
	if err != nil {
		t.Fatalf("Seek failed: %v", err)
	}
	_, err = reader.Seek(0, 0) // Reset to beginning
	if err != nil {
		t.Fatalf("Seek failed: %v", err)
	}
	note, err := Open(reader, size)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	if len(note.Pages) != 1 {
		t.Fatalf("got %d pages, want 1", len(note.Pages))
	}

	page := note.Pages[0]
	if len(page.Shapes) != 1 {
		t.Fatalf("got %d shapes, want 1", len(page.Shapes))
	}

	shape := page.Shapes[0]
	if shape.ShapeType != 1 {
		t.Errorf("got shapeType %d, want 1", shape.ShapeType)
	}
	if shape.Color != -16777216 { // 0xFF000000
		t.Errorf("got color 0x%X, want 0xFF000000", uint32(shape.Color))
	}
	if shape.FillColor != -16776961 { // 0xFF0000FF
		t.Errorf("got fillColor 0x%X, want 0xFF0000FF", uint32(shape.FillColor))
	}
	if shape.Thickness != 2.5 {
		t.Errorf("got thickness %.1f, want 2.5", shape.Thickness)
	}

	if shape.BoundingRect == nil {
		t.Errorf("got nil boundingRect, want parsed rect")
	} else {
		if shape.BoundingRect.Left != 10.0 {
			t.Errorf("got boundingRect.Left %.1f, want 10.0", shape.BoundingRect.Left)
		}
	}

	if len(shape.MatrixValues) != 6 {
		t.Errorf("got %d matrix values, want 6", len(shape.MatrixValues))
	}
}

// TestOpen_CorrelatesShapesToPoints verifies AC1.5: Parser correlates shapes to point data via UUID.
func TestOpen_CorrelatesShapesToPoints(t *testing.T) {
	shapes := []*pb.ShapeInfoProto{
		{
			UniqueId:  "shape-abc123",
			ShapeType: 1,
			Zorder:    1,
		},
	}

	points := []TinyPoint{
		{X: 10.5, Y: 20.5, Size: 5, Pressure: 100, Time: 1000},
		{X: 11.5, Y: 21.5, Size: 5, Pressure: 100, Time: 1001},
		{X: 12.5, Y: 22.5, Size: 5, Pressure: 100, Time: 1002},
	}

	reader := buildTestNote(t, noteOpts{
		NoteID: "note123",
		Title:  "Test",
		Pages: []*testPage{
			{
				PageID: "page1",
				Width:  1860,
				Height: 2480,
				Shapes: shapes,
				Points: map[string][]TinyPoint{
					"shape-abc123": points,
				},
			},
		},
	})

	size, err := reader.Seek(0, 2) // Seek to end to get size
	if err != nil {
		t.Fatalf("Seek failed: %v", err)
	}
	_, err = reader.Seek(0, 0) // Reset to beginning
	if err != nil {
		t.Fatalf("Seek failed: %v", err)
	}
	note, err := Open(reader, size)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	shape := note.Pages[0].Shapes[0]
	if len(shape.Points) != 3 {
		t.Fatalf("got %d points, want 3", len(shape.Points))
	}

	if shape.Points[0].X != 10.5 {
		t.Errorf("got first point X %.1f, want 10.5", shape.Points[0].X)
	}
	if shape.Points[0].Pressure != 100 {
		t.Errorf("got first point pressure %d, want 100", shape.Points[0].Pressure)
	}

	if shape.Points[2].Time != 1002 {
		t.Errorf("got last point time %d, want 1002", shape.Points[2].Time)
	}
}

// TestOpen_CorruptZIP verifies AC1.6: Parser returns clear error for corrupt/truncated ZIP.
func TestOpen_CorruptZIP(t *testing.T) {
	corruptData := []byte{0xFF, 0xFE, 0xFD} // Not a valid ZIP

	reader := bytes.NewReader(corruptData)
	_, err := Open(reader, int64(len(corruptData)))

	if err == nil {
		t.Errorf("got nil error for corrupt ZIP, want error")
	}
	if err.Error() == "" {
		t.Errorf("got empty error message, want descriptive error")
	}
}

// TestOpen_BlankPage verifies AC1.8: Parser handles notes with zero shapes (blank pages).
func TestOpen_BlankPage(t *testing.T) {
	reader := buildTestNote(t, noteOpts{
		NoteID: "note123",
		Title:  "Test",
		Pages: []*testPage{
			{
				PageID: "page1",
				Width:  1860,
				Height: 2480,
				// No shapes
			},
		},
	})

	size, err := reader.Seek(0, 2) // Seek to end to get size
	if err != nil {
		t.Fatalf("Seek failed: %v", err)
	}
	_, err = reader.Seek(0, 0) // Reset to beginning
	if err != nil {
		t.Fatalf("Seek failed: %v", err)
	}
	note, err := Open(reader, size)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	if len(note.Pages) != 1 {
		t.Fatalf("got %d pages, want 1", len(note.Pages))
	}

	page := note.Pages[0]
	if page.Shapes != nil && len(page.Shapes) > 0 {
		t.Errorf("got %d shapes on blank page, want none", len(page.Shapes))
	}
}

// TestOpen_MultiplePages verifies AC1.9: Parser handles multi-page notes.
func TestOpen_MultiplePages(t *testing.T) {
	reader := buildTestNote(t, noteOpts{
		NoteID: "note123",
		Title:  "Multi-page Test",
		Pages: []*testPage{
			{
				PageID: "page1",
				Width:  1860,
				Height: 2480,
			},
			{
				PageID: "page2",
				Width:  1860,
				Height: 2480,
			},
			{
				PageID: "page3",
				Width:  1860,
				Height: 2480,
			},
		},
	})

	size, err := reader.Seek(0, 2) // Seek to end to get size
	if err != nil {
		t.Fatalf("Seek failed: %v", err)
	}
	_, err = reader.Seek(0, 0) // Reset to beginning
	if err != nil {
		t.Fatalf("Seek failed: %v", err)
	}
	note, err := Open(reader, size)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	if len(note.Pages) != 3 {
		t.Errorf("got %d pages, want 3", len(note.Pages))
	}

	if note.Pages[0].PageID != "page1" {
		t.Errorf("got first page ID %q, want page1", note.Pages[0].PageID)
	}
	if note.Pages[1].PageID != "page2" {
		t.Errorf("got second page ID %q, want page2", note.Pages[1].PageID)
	}
	if note.Pages[2].PageID != "page3" {
		t.Errorf("got third page ID %q, want page3", note.Pages[2].PageID)
	}
}
