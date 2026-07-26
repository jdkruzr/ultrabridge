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
		"templates/_pipeline_bar.html",
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

	// pipeline bar (both worker sources configured): one bar carries the
	// controls for every source that has a worker, plus the presence marker
	// the JS renderer needs for Supernote.
	var pb bytes.Buffer
	if err := tmpl.ExecuteTemplate(&pb, "_pipeline_bar", map[string]any{
		"HasSupernoteSource": true, "HasBooxSource": true,
	}); err != nil {
		t.Fatalf("pipeline bar: %v", err)
	}
	for _, want := range []string{
		"/processor/supernote/start", "/processor/supernote/stop",
		"/processor/boox/start", "/processor/boox/stop",
		`id="pipeline-summary"`, `id="pipeline-detail"`, `data-has-sn="1"`,
	} {
		if !strings.Contains(pb.String(), want) {
			t.Errorf("pipeline bar missing %q in:\n%s", want, pb.String())
		}
	}

	// pipeline bar with no worker-backed source (e.g. an SPC-server-only or
	// ForestNote-only deployment): status surface only, no processor controls,
	// and data-has-sn must be 0 so the renderer doesn't invent an "SN idle"
	// segment out of the always-present top-level counters.
	var pbNone bytes.Buffer
	if err := tmpl.ExecuteTemplate(&pbNone, "_pipeline_bar", map[string]any{
		"HasSupernoteSource": false, "HasBooxSource": false,
	}); err != nil {
		t.Fatalf("pipeline bar (no workers): %v", err)
	}
	if strings.Contains(pbNone.String(), "/processor/") {
		t.Errorf("worker-less pipeline bar should not render processor controls:\n%s", pbNone.String())
	}
	if !strings.Contains(pbNone.String(), `data-has-sn="0"`) {
		t.Errorf("worker-less pipeline bar missing data-has-sn=\"0\"")
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
		// loader URLs — slashes are JS-string-escaped (\/) in <script> context.
		`"\/files\/history?path=x"`,
		`"\/files\/boox\/versions?path=x"`,
		// Order-independent guard: the loaders must NOT be invoked at parse
		// time (on a full-page render of a ?detail= URL the helper defs come
		// later in the document → ReferenceError). They run immediately if
		// defined, else defer to DOMContentLoaded.
		"window.ubLoadJobInfo", "window.ubLoadVersions",
		"DOMContentLoaded",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("detail grid missing %q in:\n%s", want, s)
		}
	}
	// Regression guard: the old unguarded parse-time invocation must not return.
	for _, banned := range []string{`<script>ubLoadJobInfo(`, `<script>ubLoadVersions(`} {
		if strings.Contains(s, banned) {
			t.Errorf("detail grid has unguarded parse-time loader call %q", banned)
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

// TestPipelineBarOutsideSwapTarget is the regression test for the bug this
// whole surface was built to fix: the status bar used to live INSIDE
// <main id="main-content">, which is the target of every sidebar link's
// hx-get with the default innerHTML swap. On HX-Request renderTemplate emits
// only the "content" template, so the first client-side navigation wiped the
// bar out and it stayed gone until a full reload.
//
// The invariant is positional: the bar must be rendered before — and
// therefore outside — the #main-content div.
func TestPipelineBarOutsideSwapTarget(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewHandler(&mockTaskService{}, &mockNoteService{}, &mockSearchService{},
		&mockConfigService{}, nil, "/notes", "", logger, logging.NewLogBroadcaster())

	// No HX-Request header: full layout render.
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	body := w.Body.String()
	bar := strings.Index(body, `id="pipeline-bar"`)
	main := strings.Index(body, `<div id="main-content">`)
	if bar < 0 {
		t.Fatalf("layout did not render the pipeline bar")
	}
	if main < 0 {
		t.Fatalf("layout did not render the #main-content swap target")
	}
	if bar > main {
		t.Errorf("pipeline bar renders inside the #main-content swap target "+
			"(bar at %d, target at %d) — HTMX navigation will destroy it", bar, main)
	}

	// The bar is layout chrome, so it must NOT appear in an HX-Request
	// response; otherwise a swap would nest a second copy inside the target.
	hxReq := httptest.NewRequest("GET", "/", nil)
	hxReq.Header.Set("HX-Request", "true")
	hxW := httptest.NewRecorder()
	handler.ServeHTTP(hxW, hxReq)

	if strings.Contains(hxW.Body.String(), `id="pipeline-bar"`) {
		t.Error("HX-Request fragment response contains the pipeline bar; it should be layout-only")
	}
}
