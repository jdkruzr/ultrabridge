// Package fnpath is the single source of truth for the opaque "forestnote://"
// URI scheme that addresses synced ForestNote notebooks and pages across the
// sync bridge, search/RAG faceting, the note service, and the web layer.
//
// A notebook is addressed as forestnote://{notebook_id}; a page within it as
// forestnote://{notebook_id}/{page_id}. The page id is a stable ULID, so the
// search index key never changes when pages are reordered.
package fnpath

import "strings"

// Scheme is the prefix every ForestNote URI carries.
const Scheme = "forestnote://"

// Notebook returns the URI for a notebook.
func Notebook(notebookID string) string { return Scheme + notebookID }

// Page returns the URI for a page within a notebook.
func Page(notebookID, pageID string) string { return Scheme + notebookID + "/" + pageID }

// Is reports whether path uses the ForestNote scheme.
func Is(path string) bool { return strings.HasPrefix(path, Scheme) }

// PageID extracts the trailing page-id segment from a page URI. For a
// notebook-only URI (no page) it returns the notebook id; callers that render
// pages always pass a full page URI.
func PageID(path string) string { return path[strings.LastIndex(path, "/")+1:] }
