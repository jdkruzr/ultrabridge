package service

import (
	"net/url"
	"strings"

	"github.com/sysop/ultrabridge/internal/fnpath"
)

// NoteReference is the externally useful projection of an internal note key.
// DetailPath is a UB web path; NativeURL is populated only when the source has
// a real source-owned opener.
type NoteReference struct {
	DetailPath string
	NativeURL  string
}

// ReferenceForPath maps an internal indexed note path to the URL surfaces that
// humans and external clients can use. The raw note path remains the stable
// render/index key; this function is only a presentation/reference layer.
func ReferenceForPath(notePath, sourceType string, page int) NoteReference {
	if sourceType == "" {
		switch {
		case fnpath.Is(notePath):
			sourceType = "forestnote"
		case strings.HasPrefix(notePath, RemarkablePathPrefix):
			sourceType = "remarkable"
		default:
			return NoteReference{}
		}
	}

	switch sourceType {
	case "supernote":
		return NoteReference{DetailPath: "/files/supernote?detail=" + url.QueryEscape(notePath)}
	case "boox":
		return NoteReference{DetailPath: "/files/boox?detail=" + url.QueryEscape(notePath)}
	case "forestnote":
		return forestNoteReference(notePath)
	case "remarkable":
		docID := strings.TrimPrefix(notePath, RemarkablePathPrefix)
		if docID == "" || docID == notePath {
			return NoteReference{}
		}
		return NoteReference{DetailPath: "/files/remarkable?document=" + url.QueryEscape(docID)}
	default:
		return NoteReference{}
	}
}

func forestNoteReference(notePath string) NoteReference {
	if !fnpath.Is(notePath) {
		return NoteReference{}
	}
	notebookID := fnpath.NotebookID(notePath)
	if notebookID == "" {
		return NoteReference{}
	}
	ref := NoteReference{DetailPath: "/files/forestnote?notebook=" + url.QueryEscape(notebookID)}
	pageID := forestNotePageID(notePath)
	if pageID == "" {
		return ref
	}
	ref.DetailPath += "&page=" + url.QueryEscape(pageID)
	ref.NativeURL = "forestnote://notebook/" + notebookID + "/page/" + pageID
	return ref
}

func forestNotePageID(notePath string) string {
	rest := strings.TrimPrefix(notePath, fnpath.Scheme)
	i := strings.Index(rest, "/")
	if i < 0 || i == len(rest)-1 {
		return ""
	}
	return rest[i+1:]
}
