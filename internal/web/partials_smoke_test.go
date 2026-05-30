package web

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
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

	// status panel: source slug threads into /processor/<slug>/{start,stop}.
	var sp bytes.Buffer
	if err := tmpl.ExecuteTemplate(&sp, "_files_status_panel", "supernote"); err != nil {
		t.Fatalf("status panel: %v", err)
	}
	for _, want := range []string{"/processor/supernote/start", "/processor/supernote/stop", `id="proc-status"`} {
		if !strings.Contains(sp.String(), want) {
			t.Errorf("status panel missing %q", want)
		}
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
