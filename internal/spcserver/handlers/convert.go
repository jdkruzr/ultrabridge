package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	gosnote "github.com/jdkruzr/go-sn/note"
	"github.com/sysop/ultrabridge/internal/forestpdf"
	"github.com/sysop/ultrabridge/internal/spcserver/envelope"
	"github.com/sysop/ultrabridge/internal/spcserver/fileids"
	"github.com/sysop/ultrabridge/internal/spcserver/mapping"
	"github.com/sysop/ultrabridge/internal/spcserver/oss"
)

const convertDir = ".convert"

// ConvertHandler implements SPC note/PDF export endpoints used by the web and
// Partner app. Generated artifacts stay under FileRoot/.convert, which normal
// file listings hide, and are exposed only through signed /api/oss/download URLs.
type ConvertHandler struct {
	Root   string
	Reg    *fileids.Registry
	Signer *oss.Signer
	Logger *slog.Logger
}

type convertDTO struct {
	ID string `json:"id"`
}

type pngPageVO struct {
	PageNo int    `json:"pageNo"`
	URL    string `json:"url"`
}

func (h *ConvertHandler) log() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

// NoteToPNG handles POST /api/file/note/to/png.
func (h *ConvertHandler) NoteToPNG(w http.ResponseWriter, r *http.Request) {
	var req convertDTO
	_ = json.NewDecoder(r.Body).Decode(&req)
	abs, ok := h.sourcePath(r, req.ID)
	if !ok {
		writeConvertNotFound(w)
		return
	}
	pages, err := renderNotePNGs(abs)
	if err != nil {
		h.log().Warn("note/to/png render", "path", abs, "err", err)
		writeConvertNotFound(w)
		return
	}
	outDir := filepath.Join(h.Root, convertDir, req.ID)
	_ = os.MkdirAll(outDir, 0o755)
	var vos []pngPageVO
	for i, data := range pages {
		name := "page_" + strconv.Itoa(i+1) + ".png"
		out := filepath.Join(outDir, name)
		if err := os.WriteFile(out, data, 0o644); err != nil {
			writeConvertNotFound(w)
			return
		}
		rel := "/" + filepath.ToSlash(filepath.Join(convertDir, req.ID, name))
		vos = append(vos, pngPageVO{PageNo: i + 1, URL: h.signedURL(r, rel, "0")})
	}
	envelope.WriteJSON(w, struct {
		envelope.BaseVO
		PNGPageVOList []pngPageVO `json:"pngPageVOList"`
	}{BaseVO: envelope.OK(), PNGPageVOList: vos})
}

// NoteToPDF handles POST /api/file/note/to/pdf.
func (h *ConvertHandler) NoteToPDF(w http.ResponseWriter, r *http.Request) {
	var req convertDTO
	_ = json.NewDecoder(r.Body).Decode(&req)
	abs, ok := h.sourcePath(r, req.ID)
	if !ok {
		writeConvertNotFound(w)
		return
	}
	url, err := h.notePDFURL(r, abs, req.ID)
	if err != nil {
		h.log().Warn("note/to/pdf render", "path", abs, "err", err)
		writeConvertNotFound(w)
		return
	}
	envelope.WriteJSON(w, struct {
		envelope.BaseVO
		URL string `json:"url"`
	}{BaseVO: envelope.OK(), URL: url})
}

// PDFWithMarkToPDF handles POST /api/file/pdfwithmark/to/pdf. UB does not yet
// synthesize Supernote PDF mark overlays; for plain PDFs it returns the raw PDF
// URL, and for .note files it returns the rendered note PDF.
func (h *ConvertHandler) PDFWithMarkToPDF(w http.ResponseWriter, r *http.Request) {
	var req convertDTO
	_ = json.NewDecoder(r.Body).Decode(&req)
	abs, ok := h.sourcePath(r, req.ID)
	if !ok {
		writeConvertNotFound(w)
		return
	}
	entry, err := mapping.EntryFor(r.Context(), h.Root, abs, h.Reg)
	if err != nil {
		writeConvertNotFound(w)
		return
	}
	var url string
	if strings.EqualFold(filepath.Ext(abs), ".note") {
		url, err = h.notePDFURL(r, abs, req.ID)
		if err != nil {
			writeConvertNotFound(w)
			return
		}
	} else {
		url = h.signedURL(r, entry.PathDisplay, entry.ID)
	}
	envelope.WriteJSON(w, struct {
		envelope.BaseVO
		URL string `json:"url"`
	}{BaseVO: envelope.OK(), URL: url})
}

func (h *ConvertHandler) sourcePath(r *http.Request, rawID string) (string, bool) {
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || h.Root == "" || h.Reg == nil {
		return "", false
	}
	abs, found, err := h.Reg.PathFor(r.Context(), id)
	if err != nil || !found {
		return "", false
	}
	if fi, err := os.Lstat(abs); err == nil && !fi.IsDir() {
		return abs, true
	}
	return "", false
}

func (h *ConvertHandler) notePDFURL(r *http.Request, abs, id string) (string, error) {
	jpegs, err := renderNoteJPEGs(abs)
	if err != nil {
		return "", err
	}
	var pdf bytes.Buffer
	if err := forestpdf.AssemblePDF(jpegs, &pdf); err != nil {
		return "", err
	}
	outDir := filepath.Join(h.Root, convertDir, id)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	out := filepath.Join(outDir, "note.pdf")
	if err := os.WriteFile(out, pdf.Bytes(), 0o644); err != nil {
		return "", err
	}
	rel := "/" + filepath.ToSlash(filepath.Join(convertDir, id, "note.pdf"))
	return h.signedURL(r, rel, "0"), nil
}

func (h *ConvertHandler) signedURL(r *http.Request, pathDisplay, pathID string) string {
	encPath := oss.EncryptPath(pathDisplay)
	ts := h.nowMillis()
	nonce := newNonce()
	sig := h.Signer.DownloadSignature(encPath, ts, nonce)
	return fmt.Sprintf("%s/api/oss/download?path=%s&signature=%s&timestamp=%d&nonce=%s&pathId=%s",
		requestBaseURL(r), encPath, sig, ts, nonce, pathID)
}

func (h *ConvertHandler) nowMillis() int64 {
	if h.Signer != nil && h.Signer.Now != nil {
		return h.Signer.Now().UnixMilli()
	}
	return time.Now().UnixMilli()
}

func renderNotePNGs(abs string) ([][]byte, error) {
	imgs, err := renderNoteImages(abs)
	if err != nil {
		return nil, err
	}
	out := make([][]byte, 0, len(imgs))
	for _, img := range imgs {
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return nil, err
		}
		out = append(out, buf.Bytes())
	}
	return out, nil
}

func renderNoteJPEGs(abs string) ([][]byte, error) {
	imgs, err := renderNoteImages(abs)
	if err != nil {
		return nil, err
	}
	out := make([][]byte, 0, len(imgs))
	for _, img := range imgs {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
			return nil, err
		}
		out = append(out, buf.Bytes())
	}
	return out, nil
}

func renderNoteImages(abs string) ([]image.Image, error) {
	f, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	n, err := gosnote.Load(f)
	if err != nil {
		return nil, err
	}
	if len(n.Pages) == 0 {
		return nil, fmt.Errorf("note has no pages")
	}
	imgs := make([]image.Image, 0, len(n.Pages))
	for _, p := range n.Pages {
		tp, err := n.TotalPathData(p)
		if err != nil || tp == nil {
			continue
		}
		pageW, pageH := n.PageDimensions(p)
		objs, err := gosnote.DecodeObjects(tp, pageW, pageH)
		if err != nil {
			return nil, err
		}
		imgs = append(imgs, gosnote.RenderObjects(objs, pageW, pageH, nil))
	}
	if len(imgs) == 0 {
		return nil, fmt.Errorf("note has no renderable pages")
	}
	return imgs, nil
}

func writeConvertNotFound(w http.ResponseWriter) {
	envelope.WriteJSON(w, envelope.BaseVO{Success: false, ErrorCode: errFileNotExistCode, ErrorMsg: errFileNotExistMsg})
}
