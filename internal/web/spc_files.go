package web

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sysop/ultrabridge/internal/spcserver/mapping"
)

var spcFolders = []string{"Document", "EXPORT", "MyStyle", "Note", "SCREENSHOT", "INBOX"}

const spcBrowserUploadLimit = 512 << 20

func (h *Handler) SetSPCFileNotifier(fn func(context.Context) error) { h.spcFileNotify = fn }

// spcFolderPath shares the device protocol's folder mapping. The web browser
// must never introduce a second /Document or /MyStyle identity beside the
// native DOCUMENT/Document and NOTE/MyStyle directories.
func (h *Handler) spcFolderPath(_ *os.Root, name string) string {
	return strings.TrimPrefix(mapping.NativePath(name), "/")
}

func splitSPCBrowserPath(raw string) (string, string, error) {
	if strings.Contains(raw, "\\") || strings.HasPrefix(raw, "/") {
		return "", "", os.ErrPermission
	}
	for _, part := range strings.Split(raw, "/") {
		if part == ".." || strings.HasPrefix(part, ".") {
			return "", "", os.ErrPermission
		}
	}
	parts := strings.SplitN(raw, "/", 2)
	for _, name := range spcFolders {
		if parts[0] == name {
			rel := "."
			if len(parts) > 1 && parts[1] != "" {
				rel = parts[1]
			}
			return name, rel, nil
		}
	}
	return "", "", os.ErrPermission
}

func (h *Handler) openSPCFolder(raw string, create bool) (*os.Root, string, error) {
	name, rel, err := splitSPCBrowserPath(raw)
	if err != nil {
		return nil, "", err
	}
	root, err := os.OpenRoot(h.spcFileRoot)
	if err != nil {
		return nil, "", err
	}
	defer root.Close()
	folder := h.spcFolderPath(root, name)
	if create {
		if err := root.MkdirAll(folder, 0755); err != nil {
			return nil, "", err
		}
	}
	info, err := root.Lstat(folder)
	if err != nil {
		return nil, "", err
	}
	if !info.IsDir() {
		return nil, "", os.ErrPermission
	}
	sub, err := root.OpenRoot(folder)
	return sub, rel, err
}

type spcBrowserEntry struct {
	Name, URL, DownloadURL, DetailURL string
	IsDir                             bool
	Size                              int64
}

func (h *Handler) handleSPCFiles(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("path")
	entries := []spcBrowserEntry{}
	if raw == "" {
		for _, name := range spcFolders {
			entries = append(entries, spcBrowserEntry{Name: name, IsDir: true, URL: "/files/supernote?path=" + url.QueryEscape(name)})
		}
	} else {
		root, rel, err := h.openSPCFolder(raw, false)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			http.Error(w, "invalid folder", http.StatusBadRequest)
			return
		}
		if err == nil {
			defer root.Close()
			f, err := root.Open(rel)
			if err != nil {
				http.Error(w, "folder not found", http.StatusNotFound)
				return
			}
			defer f.Close()
			children, err := f.ReadDir(-1)
			if err != nil {
				http.Error(w, "could not read folder", http.StatusInternalServerError)
				return
			}
			for _, child := range children {
				if strings.HasPrefix(child.Name(), ".") || child.Type()&os.ModeSymlink != 0 {
					continue
				}
				info, err := root.Lstat(filepath.Join(rel, child.Name()))
				if err != nil || (!info.IsDir() && !info.Mode().IsRegular()) {
					continue
				}
				childPath := path.Join(raw, child.Name())
				entry := spcBrowserEntry{Name: child.Name(), IsDir: child.IsDir(), Size: info.Size(), URL: "/files/supernote?path=" + url.QueryEscape(childPath), DownloadURL: "/files/supernote/download?path=" + url.QueryEscape(childPath)}
				if strings.HasPrefix(childPath, "Note/") && strings.EqualFold(filepath.Ext(child.Name()), ".note") && h.notesPathPrefix != "" {
					abs := filepath.Join(h.notesPathPrefix, strings.TrimPrefix(childPath, "Note/"))
					entry.DetailURL = "/files/supernote?detail=" + url.QueryEscape(abs) + "&back=" + url.QueryEscape(raw)
				}
				entries = append(entries, entry)
			}
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].IsDir != entries[j].IsDir {
				return entries[i].IsDir
			}
			return entries[i].Name < entries[j].Name
		})
	}
	h.renderTemplate(w, r, "files_spc", map[string]interface{}{"activeTab": "files-supernote", "spcEntries": entries, "spcPath": raw, "navCrumbs": supernoteCrumbs(buildBreadcrumbs(raw))})
}

func (h *Handler) handleSPCDownload(w http.ResponseWriter, r *http.Request) {
	if h.spcFileRoot == "" {
		http.NotFound(w, r)
		return
	}
	root, rel, err := h.openSPCFolder(r.URL.Query().Get("path"), false)
	if err != nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	defer root.Close()
	f, err := root.Open(rel)
	if err != nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": info.Name()}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

func (h *Handler) handleSPCUpload(w http.ResponseWriter, r *http.Request) {
	if h.spcFileRoot == "" {
		http.NotFound(w, r)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host != r.Host {
			http.Error(w, "invalid origin", http.StatusForbidden)
			return
		}
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		http.Error(w, "invalid origin", http.StatusForbidden)
		return
	}
	raw := r.URL.Query().Get("path")
	root, rel, err := h.openSPCFolder(raw, true)
	if err != nil {
		http.Error(w, "invalid folder", http.StatusBadRequest)
		return
	}
	defer root.Close()
	dir, err := root.OpenRoot(rel)
	if err != nil {
		http.Error(w, "folder not found", http.StatusNotFound)
		return
	}
	defer dir.Close()
	r.Body = http.MaxBytesReader(w, r.Body, spcBrowserUploadLimit)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		if r.MultipartForm != nil {
			r.MultipartForm.RemoveAll()
		}
		http.Error(w, "invalid upload or file exceeds 512 MiB", http.StatusBadRequest)
		return
	}
	defer r.MultipartForm.RemoveAll()
	f, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "choose a file", http.StatusBadRequest)
		return
	}
	defer f.Close()
	name := header.Filename
	if name == "" || strings.HasPrefix(name, ".") || strings.ContainsAny(name, "/\\") {
		http.Error(w, "invalid filename", http.StatusBadRequest)
		return
	}
	// A hidden staging file is excluded by SPC listings. Root.Link publishes
	// completed bytes atomically and refuses to overwrite an existing file.
	hidden := ".upload-" + rand.Text()
	out, err := dir.OpenFile(hidden, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		http.Error(w, "upload failed", http.StatusInternalServerError)
		return
	}
	defer dir.Remove(hidden)
	_, copyErr := io.Copy(out, f)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		http.Error(w, "upload failed", http.StatusInternalServerError)
		return
	}
	if err := dir.Link(hidden, name); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, os.ErrExist) {
			status = http.StatusConflict
		}
		http.Error(w, "file already exists or upload could not be saved", status)
		return
	}
	if h.spcFileNotify != nil {
		if err := h.spcFileNotify(r.Context()); err != nil {
			h.logger.Warn("file sync notification failed", "error", err)
		}
	}
	http.Redirect(w, r, "/files/supernote?path="+url.QueryEscape(raw), http.StatusSeeOther)
}
