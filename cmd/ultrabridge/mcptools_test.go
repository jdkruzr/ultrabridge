package main

import (
	"strings"
	"testing"
)

// formatMCPTask is the surface the Claude Web connector renders (the in-process
// /mcp endpoint). It must surface task ATTACHMENTS — mirroring the standalone
// ub-mcp sidecar's formatTask. Regression guard for the FN↔UB ATTACH work that
// originally updated the REST API + sidecar but missed this formatter.
func TestFormatMCPTask_RendersAttachment(t *testing.T) {
	task := mcpTask{
		ID:     "t1",
		Title:  "discovery",
		Status: "needsAction",
		Attachments: []mcpAttachment{{
			URL:      "https://ub.example/api/v1/attachments/abc?sig=x",
			FmtType:  "text/plain",
			Filename: "recognized-text.txt",
			Size:     9,
			Inline:   true,
		}},
	}
	out := formatMCPTask(task)
	want := "Attachment: recognized-text.txt [text/plain, 9 bytes] https://ub.example/api/v1/attachments/abc?sig=x"
	if !strings.Contains(out, want) {
		t.Errorf("attachment not rendered.\n got: %q\nwant substring: %q", out, want)
	}
}

func TestFormatMCPTask_NoAttachmentNoLine(t *testing.T) {
	out := formatMCPTask(mcpTask{ID: "t2", Title: "x", Status: "needsAction"})
	if strings.Contains(out, "Attachment:") {
		t.Errorf("unexpected Attachment line for an attachment-less task:\n%s", out)
	}
}
