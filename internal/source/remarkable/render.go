package remarkable

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"

	"github.com/sysop/ultrabridge/internal/pdfrender"
	"github.com/sysop/ultrabridge/internal/rmrender"
)

const rendererVersion = "v1"

// RenderPageJPEG renders a resolved reMarkable document page to JPEG bytes.
func RenderPageJPEG(ctx context.Context, doc RenderDocument, pageIdx int) ([]byte, error) {
	if pageIdx < 0 {
		pageIdx = 0
	}
	if doc.PageCount > 0 && pageIdx >= doc.PageCount {
		return nil, fmt.Errorf("page index %d out of range", pageIdx)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	cachePath := renderCachePath(doc, pageIdx)
	if cachePath != "" {
		if data, err := os.ReadFile(cachePath); err == nil {
			return data, nil
		}
	}

	var base image.Image
	var pdfJPEG []byte
	var err error
	if doc.PDFPath != "" {
		pdfJPEG, err = pdfrender.RenderPage(doc.PDFPath, pageIdx, 144)
		if err != nil && len(doc.PageRM) == 0 {
			return nil, fmt.Errorf("render source pdf: %w", err)
		}
		if err == nil {
			base, _, _ = image.Decode(bytes.NewReader(pdfJPEG))
		}
	}

	pageID := ""
	if pageIdx < len(doc.PageOrder) {
		pageID = doc.PageOrder[pageIdx]
	}
	rmBlob, hasRM := doc.PageRM[pageID]
	if !hasRM && doc.PDFPath != "" && len(pdfJPEG) > 0 {
		writeRenderCache(cachePath, pdfJPEG)
		return pdfJPEG, nil
	}
	if !hasRM {
		return nil, fmt.Errorf("remarkable page %d not renderable", pageIdx)
	}

	rmData, err := os.ReadFile(rmBlob.Path)
	if err != nil {
		return nil, fmt.Errorf("read remarkable page: %w", err)
	}
	parsed, err := rmrender.Parse(rmData)
	if err != nil {
		return nil, fmt.Errorf("parse remarkable page: %w", err)
	}
	img, err := rmrender.RenderPageOn(parsed, base)
	if err != nil {
		return nil, fmt.Errorf("render remarkable page: %w", err)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	writeRenderCache(cachePath, out)
	return out, nil
}

func renderCachePath(doc RenderDocument, pageIdx int) string {
	if doc.CacheDir == "" || doc.Revision == "" {
		return ""
	}
	id := strings.NewReplacer("/", "_", "\\", "_").Replace(doc.ID)
	rev := strings.NewReplacer("/", "_", "\\", "_").Replace(doc.Revision)
	return filepath.Join(doc.CacheDir, id, fmt.Sprintf("%s-page_%d-%s.jpg", rev, pageIdx, rendererVersion))
}

func writeRenderCache(path string, data []byte) {
	if path == "" || len(data) == 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}
