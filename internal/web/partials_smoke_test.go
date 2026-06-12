package web

import (
	"bytes"
	"html/template"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sysop/ultrabridge/internal/logging"
	"github.com/sysop/ultrabridge/internal/service"
	"github.com/sysop/ultrabridge/internal/source"
)

func TestSharedPartialsRender(t *testing.T) {
	funcs := template.FuncMap{
		"add":      func(a, b int) int { return a + b },
		"sub":      func(a, b int) int { return a - b },
		"urlquery": template.URLQueryEscaper,
	}
	tmpl := template.Must(template.New("t").Funcs(funcs).ParseFS(templateFS,
		"templates/_files_pagination.html",
		"templates/_files_status_panel.html",
		"templates/_files_breadcrumb.html",
	))

	// pagination: page 2 of 3, with preserved params (urlquery-escaped in
	// attribute context, e.g. space -> "+" -> "&#43;", matching the inline
	// per-tab forms this partial replaced). Params range in sorted key order.
	var pg bytes.Buffer
	if err := tmpl.ExecuteTemplate(&pg, "_files_pagination", map[string]any{
		"BaseURL": "/files/boox", "Page": 2, "TotalPages": 3,
		"Params": map[string]string{"folder": "My Notes", "device": "NoteAir"},
	}); err != nil {
		t.Fatalf("pagination: %v", err)
	}
	for _, want := range []string{"/files/boox?page=1&device=NoteAir&folder=My&#43;Notes", "/files/boox?page=3&device=NoteAir&folder=My&#43;Notes", "Page 2 of 3"} {
		if !strings.Contains(pg.String(), want) {
			t.Errorf("pagination missing %q in:\n%s", want, pg.String())
		}
	}

	// status panel (worker source): slug threads into /processor/<slug>/{start,stop}.
	var sp bytes.Buffer
	if err := tmpl.ExecuteTemplate(&sp, "_files_status_panel", pipelinePanel{Source: "supernote", StartStop: true}); err != nil {
		t.Fatalf("status panel: %v", err)
	}
	for _, want := range []string{"/processor/supernote/start", "/processor/supernote/stop", `id="proc-status"`} {
		if !strings.Contains(sp.String(), want) {
			t.Errorf("status panel missing %q", want)
		}
	}

	// status panel (no-worker source, e.g. ForestNote): Note instead of controls.
	var spFN bytes.Buffer
	if err := tmpl.ExecuteTemplate(&spFN, "_files_status_panel", pipelinePanel{Note: "Re-OCR is per-notebook."}); err != nil {
		t.Fatalf("status panel (note): %v", err)
	}
	if strings.Contains(spFN.String(), "/processor/") {
		t.Errorf("no-worker panel should not render processor controls:\n%s", spFN.String())
	}
	if !strings.Contains(spFN.String(), "Re-OCR is per-notebook.") {
		t.Errorf("no-worker panel missing note text")
	}

	// breadcrumb: []crumb renders label + nav url.
	var bc bytes.Buffer
	if err := tmpl.ExecuteTemplate(&bc, "_files_breadcrumb", []crumb{
		{Label: "Home", HxGet: "/files/supernote?path="},
		{Label: "Sub", HxGet: "/files/supernote?path=Sub"},
	}); err != nil {
		t.Fatalf("breadcrumb: %v", err)
	}
	for _, want := range []string{">Home<", ">Sub<", `hx-get="/files/supernote?path=Sub"`} {
		if !strings.Contains(bc.String(), want) {
			t.Errorf("breadcrumb missing %q in:\n%s", want, bc.String())
		}
	}
}

func TestDetailPageGridRenders(t *testing.T) {
	funcs := template.FuncMap{} // partial uses no custom funcs
	tmpl := template.Must(template.New("t").Funcs(funcs).ParseFS(templateFS,
		"templates/_detail_page_grid.html"))

	dv := detailView{
		Title:       "foo.note",
		BackURL:     "/files/supernote?path=Sub",
		Meta:        []detailKV{{Label: "Pages", Value: "2"}},
		Pages:       []detailPage{{ImgURL: "/files/render?path=x&page=0&v=2", Caption: "Page 1", BodyText: "hello", Source: "myScript"}},
		Actions:     []detailAction{{Label: "✗ Delete", Danger: true, HxPost: "/files/delete-note", Confirm: "Delete?", OnAfter: "if(event.detail.successful){window.location='/files/boox';}"}},
		JobInfoURL:  "/files/history?path=x",
		VersionsURL: "/files/boox/versions?path=x",
		EmptyMsg:    "nothing",
	}
	var out bytes.Buffer
	if err := tmpl.ExecuteTemplate(&out, "_detail_page_grid", dv); err != nil {
		t.Fatalf("detail grid: %v", err)
	}
	s := out.String()
	for _, want := range []string{
		`hx-get="/files/supernote?path=Sub"`,            // back link
		"foo.note",                                      // title
		`src="/files/render?path=x&amp;page=0&amp;v=2"`, // lazy image (amp-escaped in attr)
		"hello", "myScript", // OCR text + source
		`hx-post="/files/delete-note"`, // action
		// loader calls — slashes are JS-string-escaped (\/) in <script> context.
		`ubLoadJobInfo("\/files\/history?path=x"`,
		`ubLoadVersions("\/files\/boox\/versions?path=x"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("detail grid missing %q in:\n%s", want, s)
		}
	}

	// Empty-pages path renders the empty message, not a grid.
	dv.Pages = nil
	var empty bytes.Buffer
	if err := tmpl.ExecuteTemplate(&empty, "_detail_page_grid", dv); err != nil {
		t.Fatalf("detail grid (empty): %v", err)
	}
	if !strings.Contains(empty.String(), "nothing") {
		t.Errorf("empty detail grid missing EmptyMsg")
	}
}

// TestSupernoteDetailMode drives the handler's ?detail= branch end-to-end and
// asserts the in-tab page grid renders (no modal). Guards the path that
// renderTemplate would otherwise swallow on a template execution error.
func TestSupernoteDetailMode(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	notes := &mockNoteService{
		contents:           make(map[string]interface{}),
		pipelineConfigured: true,
		notePages: map[string][]service.NotePageView{
			"/notes/foo.note": {{Page: 0, Source: "myScript", BodyText: "recognized words"}},
		},
	}
	h := NewHandler(&mockTaskService{}, notes, &mockSearchService{}, &mockConfigService{},
		nil, "/notes", "", logger, logging.NewLogBroadcaster())

	req := httptest.NewRequest("GET", "/files/supernote?detail=/notes/foo.note&back=Sub", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("detail mode status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"foo.note", "recognized words", "← Back", "detail-page-grid"} {
		if !strings.Contains(body, want) {
			t.Errorf("detail body missing %q", want)
		}
	}
	// The old modal must be gone.
	if strings.Contains(body, "history-modal") || strings.Contains(body, "showHistory(") {
		t.Errorf("detail body still references the removed modal")
	}
}

// TestSyncModelBanner renders the shared sync-model banner for each source
// type and pins glyph, tone, and blurb.
// sync-model-and-settings-ia.AC3.1/AC3.2: glyph + label per Direction; both
// two-way sources share ⇅. AC3.3: Boox is attention-toned (muted #c97, never
// error red). AC3.4: Boox blurb states deletes never reach UB. AC7.2: glyphs
// are literal Unicode runes, no icon-library markup.
func TestSyncModelBanner(t *testing.T) {
	tmpl := template.Must(template.New("t").ParseFS(templateFS,
		"templates/_sync_model_banner.html",
	))

	render := func(sourceType string) string {
		t.Helper()
		var buf bytes.Buffer
		if err := tmpl.ExecuteTemplate(&buf, "_sync_model_banner", source.SyncModelFor(sourceType)); err != nil {
			t.Fatalf("render %s: %v", sourceType, err)
		}
		return buf.String()
	}

	sn, bx, fn := render("supernote"), render("boox"), render("forestnote")

	// AC3.1 / AC3.2: glyph derives from Direction; labels render.
	for name, out := range map[string]string{"supernote": sn, "forestnote": fn} {
		if !strings.Contains(out, "⇅") {
			t.Errorf("%s banner missing ⇅ glyph", name)
		}
	}
	if !strings.Contains(sn, "Two-way sync") {
		t.Error("supernote banner missing label Two-way sync")
	}
	if !strings.Contains(fn, "Live mirror") {
		t.Error("forestnote banner missing label Live mirror")
	}
	if !strings.Contains(bx, "⬇") || !strings.Contains(bx, "Receive-only") {
		t.Error("boox banner missing ⬇ glyph or Receive-only label")
	}

	// AC3.3: Boox is attention-toned (muted accent), the others quiet; no
	// banner uses the error-red status color.
	if !strings.Contains(bx, "sync-model-attention") || !strings.Contains(bx, "#c97") {
		t.Error("boox banner missing sync-model-attention tone / #c97 accent")
	}
	for name, out := range map[string]string{"supernote": sn, "forestnote": fn} {
		if !strings.Contains(out, "sync-model-quiet") || strings.Contains(out, "sync-model-attention") {
			t.Errorf("%s banner tone wrong (want quiet, not attention)", name)
		}
	}
	for name, out := range map[string]string{"supernote": sn, "boox": bx, "forestnote": fn} {
		if strings.Contains(out, "var(--status-text-failed)") {
			t.Errorf("%s banner uses error-red status color", name)
		}
	}

	// AC3.4: the Boox blurb states device deletes never reach UB.
	if !strings.Contains(bx, "never reach UltraBridge") {
		t.Error("boox blurb missing 'never reach UltraBridge'")
	}

	// AC7.2: Unicode glyphs only — no svg/icon-font markup in any output.
	for name, out := range map[string]string{"supernote": sn, "boox": bx, "forestnote": fn} {
		for _, banned := range []string{"<svg", "<i class=", "icon-"} {
			if strings.Contains(out, banned) {
				t.Errorf("%s banner contains icon-library markup %q", name, banned)
			}
		}
	}
}
