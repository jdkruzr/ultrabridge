package mapping

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sysop/ultrabridge/internal/spcserver/dto"
	"github.com/sysop/ultrabridge/internal/spcserver/fileids"
)

// EntryFor builds the SPC EntriesVO for the file or folder at absPath, which
// must live under root. It stats the path, assigns/looks up its registry id, and
// (for files) lazily computes the MD5 content_hash. A hash error degrades to an
// empty content_hash rather than failing the whole listing — a browse must not
// break because one file is unreadable.
func EntryFor(ctx context.Context, root, absPath string, reg *fileids.Registry) (dto.EntriesVO, error) {
	fi, err := os.Lstat(absPath)
	if err != nil {
		return dto.EntriesVO{}, fmt.Errorf("mapping EntryFor stat %q: %w", absPath, err)
	}
	id, err := reg.IDFor(ctx, absPath)
	if err != nil {
		return dto.EntriesVO{}, err
	}

	display := displayPath(root, absPath)
	e := dto.EntriesVO{
		ID:             strconv.FormatInt(id, 10),
		Name:           nameFor(root, absPath),
		PathDisplay:    display,
		ParentPath:     parentDisplay(display),
		LastUpdateTime: fi.ModTime().UnixMilli(),
	}
	if fi.IsDir() {
		e.Tag = "folder"
		// size 0, not downloadable, empty content_hash (zero values)
		return e, nil
	}
	e.Tag = "file"
	e.Size = fi.Size()
	e.IsDownloadable = true
	if h, err := reg.MD5For(ctx, absPath); err == nil {
		e.ContentHash = h
	}
	return e, nil
}

// SafeResolve maps an SPC request path (root-relative, leading slash, possibly
// non-normalized with double slashes — see docs/spc-protocol.md §6) to an
// absolute filesystem path under root. It rejects any path that escapes root via
// "..". The empty path and "/" both resolve to root.
func SafeResolve(root, reqPath string) (string, error) {
	root = filepath.Clean(root)
	// Walk the segments tracking depth so an explicit "../" escape is rejected
	// (path.Clean on an absolute path would silently absorb it to root, hiding
	// the intent). Empty segments (double slashes) and "." are skipped.
	depth := 0
	for _, seg := range strings.Split(strings.TrimPrefix(reqPath, "/"), "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			depth--
			if depth < 0 {
				return "", fmt.Errorf("mapping SafeResolve: path %q escapes root", reqPath)
			}
		default:
			depth++
		}
	}
	rel := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(reqPath, "/")), "/")
	if rel == "" || rel == "." {
		return root, nil
	}
	return filepath.Join(root, filepath.FromSlash(rel)), nil
}

// displayPath returns the SPC path_display: root-relative, leading slash,
// forward slashes. The root itself is "/".
func displayPath(root, absPath string) string {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(absPath))
	if err != nil || rel == "." {
		return "/"
	}
	return "/" + filepath.ToSlash(rel)
}

// parentDisplay returns the parent of an SPC path_display ("/" for top-level).
func parentDisplay(display string) string {
	if display == "/" {
		return "/"
	}
	return path.Dir(display)
}

// nameFor returns the entry name (basename); the root's name is empty.
func nameFor(root, absPath string) string {
	if filepath.Clean(root) == filepath.Clean(absPath) {
		return ""
	}
	return filepath.Base(absPath)
}
