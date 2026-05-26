package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sysop/ultrabridge/internal/spcserver/dto"
	"github.com/sysop/ultrabridge/internal/spcserver/envelope"
	"github.com/sysop/ultrabridge/internal/spcserver/fileids"
	"github.com/sysop/ultrabridge/internal/spcserver/mapping"
)

// recycleDir is the dot-prefixed soft-delete area under FILE_ROOT. Like
// .staging it is excluded from list_folder (isHidden), so recycled files vanish
// from the device's tree without being destroyed. Recycle-bin CRUD (list/
// restore/purge) is Phase 5; Phase 4 only moves files in.
const recycleDir = ".recycle"

// Verbatim FileErrorCodeEnum codes (see docs/spc-protocol.md §7 table).
const (
	errDeleteMissingCode = "E0318" // delete: target gone
	errDeleteMissingMsg  = "The folder or file you want to delete does not exist"
	errMoveMissingCode   = "E0320" // move: source gone
	errMoveMissingMsg    = "The folder or file you want to move or rename does not exist"
	errCopyMissingCode   = "E0308" // copy: source gone
	errCopyMissingMsg    = "File does not exist"
	errSameNameCode      = "E0322" // move/copy: name collision, autorename off
	errSameNameMsg       = "A file with the same name already exists"
)

// IndexStore mutates a note's entries in a UB index (FTS content or RAG
// embeddings) to follow a file through delete/move/copy. search.Store and
// *rag.Store both satisfy it. Delete drops a deleted file's entries; Rename
// repoints a moved file's entries; Copy duplicates them for a copied file.
type IndexStore interface {
	Delete(ctx context.Context, path string) error
	Rename(ctx context.Context, oldPath, newPath string) error
	Copy(ctx context.Context, srcPath, dstPath string) error
}

// FileMover repoints a file's inventory/job rows (the notes→jobs FK pair) when
// it moves, so the Files tab + job history track the new path. *notestore.Store
// satisfies it.
type FileMover interface {
	RenameFile(ctx context.Context, oldPath, newPath string) error
}

// MutationHandler serves the SPC file-mutation write-path (Phase 4c): delete
// (soft, to .recycle/), move, and copy. It shares the FileHandler's Root and
// registry. Notifier (optional) fires a best-effort FILE-SYN after a change.
// ContentIndex/EmbedIndex (optional) keep the FTS index and RAG embeddings in
// step with the filesystem on delete/move/copy; FileRecords (optional) does the
// same for the notes/jobs inventory on move. All nil ⇒ the mutation still
// happens on disk, the index just goes stale (a moved note would keep surfacing
// at its old path, a copy would not be searchable).
type MutationHandler struct {
	Root         string
	Reg          *fileids.Registry
	Notifier     UploadNotifier
	ContentIndex IndexStore
	EmbedIndex   IndexStore
	FileRecords  FileMover
	Now          func() time.Time
	Logger       *slog.Logger
}

func (h *MutationHandler) log() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

func (h *MutationHandler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// DeleteFolder soft-deletes a file/folder: it moves the target under
// <FILE_ROOT>/.recycle/<timestamp>/<originalRelPath> (preserving the original
// layout for a future restore) and reports the deleted entry's metadata.
// POST /api/file/3/files/delete_folder_v3 (F_FileLocalController.java:123).
func (h *MutationHandler) DeleteFolder(w http.ResponseWriter, r *http.Request) {
	var req dto.DeleteFolderLocalDTO
	_ = json.NewDecoder(r.Body).Decode(&req)

	fail := func() {
		envelope.WriteJSON(w, dto.DeleteFolderLocalVO{
			BaseVO:      envelope.BaseVO{Success: false, ErrorCode: errDeleteMissingCode, ErrorMsg: errDeleteMissingMsg},
			EquipmentNo: req.EquipmentNo,
		})
	}

	id, perr := strconv.ParseInt(req.ID, 10, 64)
	if perr != nil || h.Root == "" {
		fail()
		return
	}
	abs, found, err := h.Reg.PathFor(r.Context(), id)
	if err != nil || !found {
		fail()
		return
	}
	if _, err := os.Lstat(abs); err != nil {
		fail() // registered id whose file is already gone
		return
	}

	// Capture metadata before the move (path_display/name/id from the live entry).
	entry, err := mapping.EntryFor(r.Context(), h.Root, abs, h.Reg)
	if err != nil {
		h.log().Error("delete_folder_v3 EntryFor", "path", abs, "err", err)
		fail()
		return
	}

	if err := h.recycle(entry.PathDisplay, abs); err != nil {
		h.log().Error("delete_folder_v3 recycle", "path", abs, "err", err)
		fail()
		return
	}

	// De-index best-effort: the file is already recycled, so a de-index error
	// must not fail the device's delete. Keyed on the same absolute path used at
	// index time (pipeline.Enqueue → note_content/note_embeddings.note_path).
	h.deindex(r.Context(), abs)

	// Prune any user folder this delete just emptied. The device deletes a
	// folder's *contents* by file id and never sends a folder delete, so without
	// this the emptied folder lingers and list_folder re-syncs it back ("zombie"
	// folder, hardware-confirmed 2026-05-26). Native buckets are never pruned.
	h.pruneEmptyParents(filepath.Dir(abs))

	if h.Notifier != nil {
		_ = h.Notifier.NotifyFile(r.Context())
	}
	envelope.WriteJSON(w, dto.DeleteFolderLocalVO{
		BaseVO:      envelope.OK(),
		EquipmentNo: req.EquipmentNo,
		Metadata: &dto.MetadataVO{
			Tag:         entry.Tag,
			ID:          entry.ID,
			Name:        entry.Name,
			PathDisplay: entry.PathDisplay,
		},
	})
}

// deindex drops the file's FTS content and RAG embeddings, best-effort: each
// index is optional and a failure is logged, never propagated.
func (h *MutationHandler) deindex(ctx context.Context, abs string) {
	if h.ContentIndex != nil {
		if err := h.ContentIndex.Delete(ctx, abs); err != nil {
			h.log().Error("delete_folder_v3 deindex content", "path", abs, "err", err)
		}
	}
	if h.EmbedIndex != nil {
		if err := h.EmbedIndex.Delete(ctx, abs); err != nil {
			h.log().Error("delete_folder_v3 deindex embeddings", "path", abs, "err", err)
		}
	}
}

// reindexMove repoints the file's FTS content, RAG embeddings, and notes/jobs
// inventory from src to dst, best-effort. Content is unchanged by a move, so
// repointing beats re-OCR; a failure is logged, never fails the device's move.
func (h *MutationHandler) reindexMove(ctx context.Context, src, dst string) {
	if h.ContentIndex != nil {
		if err := h.ContentIndex.Rename(ctx, src, dst); err != nil {
			h.log().Error("move_v3 reindex content", "src", src, "dst", dst, "err", err)
		}
	}
	if h.EmbedIndex != nil {
		if err := h.EmbedIndex.Rename(ctx, src, dst); err != nil {
			h.log().Error("move_v3 reindex embeddings", "src", src, "dst", dst, "err", err)
		}
	}
	if h.FileRecords != nil {
		if err := h.FileRecords.RenameFile(ctx, src, dst); err != nil {
			h.log().Error("move_v3 reindex file records", "src", src, "dst", dst, "err", err)
		}
	}
}

// reindexCopy duplicates the source's FTS content and RAG embeddings under dst
// so the copy is immediately searchable, best-effort. Content is identical, so
// duplicating beats re-OCR; a failure is logged, never fails the device's copy.
// notes/jobs inventory for the copy is left to the next scan (a copy is a new,
// independently-tracked file).
func (h *MutationHandler) reindexCopy(ctx context.Context, src, dst string) {
	if h.ContentIndex != nil {
		if err := h.ContentIndex.Copy(ctx, src, dst); err != nil {
			h.log().Error("copy_v3 reindex content", "src", src, "dst", dst, "err", err)
		}
	}
	if h.EmbedIndex != nil {
		if err := h.EmbedIndex.Copy(ctx, src, dst); err != nil {
			h.log().Error("copy_v3 reindex embeddings", "src", src, "dst", dst, "err", err)
		}
	}
}

// pruneEmptyParents removes now-empty ancestor directories left behind when the
// device deletes a folder's last file. It walks up from dir, os.Remove-ing each
// empty directory, and stops at the device's native buckets: only user folders
// (root-relative depth >= 3, e.g. NOTE/Note/<folder>) are prunable — never the
// FileRoot, the top-level buckets (NOTE/DOCUMENT/EXPORT/…), or their standard
// subdirs (NOTE/Note, DOCUMENT/Document, both depth 2). Best-effort: os.Remove
// fails on a non-empty dir (ENOTEMPTY), which naturally stops the walk; any
// error stops it. Pruned folders aren't recycled — an empty dir carries no data,
// and a later restore of the file from .recycle recreates the parent.
func (h *MutationHandler) pruneEmptyParents(dir string) {
	for {
		rel, err := filepath.Rel(h.Root, dir)
		if err != nil || rel == "." {
			return
		}
		if len(strings.Split(filepath.ToSlash(rel), "/")) < 3 {
			return // a native bucket / standard subdir / the root itself
		}
		if err := os.Remove(dir); err != nil {
			return // non-empty or already gone — stop walking up
		}
		dir = filepath.Dir(dir)
	}
}

// recycle moves abs (whose root-relative path is pathDisplay) under
// .recycle/<millis>/<relPath>, creating parents. The timestamped generation
// keeps repeated deletes of the same path from colliding.
func (h *MutationHandler) recycle(pathDisplay, abs string) error {
	rel := strings.TrimPrefix(pathDisplay, "/")
	gen := strconv.FormatInt(h.now().UnixMilli(), 10)
	dest := filepath.Join(h.Root, recycleDir, gen, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	return os.Rename(abs, dest)
}

// Move relocates a file into the to_path parent directory, keeping its filename
// and (crucially) its stable device-facing id. POST /api/file/3/files/move_v3
// (F_FileLocalController.java:177).
func (h *MutationHandler) Move(w http.ResponseWriter, r *http.Request) {
	var req dto.FileMoveLocalDTO
	_ = json.NewDecoder(r.Body).Decode(&req)
	fail := func(code, msg string) {
		envelope.WriteJSON(w, dto.FileMoveLocalVO{
			BaseVO:      envelope.BaseVO{Success: false, ErrorCode: code, ErrorMsg: msg},
			EquipmentNo: req.EquipmentNo,
		})
	}

	id, src, ok := h.resolveSource(r, req.ID)
	if !ok {
		fail(errMoveMissingCode, errMoveMissingMsg)
		return
	}
	dest, code := h.targetPath(req.ToPath, req.Autorename)
	if code != "" {
		fail(codeMsg(code))
		return
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		h.log().Error("move_v3 mkdir", "dest", dest, "err", err)
		fail(errMoveMissingCode, errMoveMissingMsg)
		return
	}
	if err := os.Rename(src, dest); err != nil {
		h.log().Error("move_v3 rename", "src", src, "dest", dest, "err", err)
		fail(errMoveMissingCode, errMoveMissingMsg)
		return
	}
	// Repoint the existing id at the new path so the device's id stays valid.
	if err := h.Reg.UpdatePath(r.Context(), id, dest); err != nil {
		h.log().Error("move_v3 UpdatePath", "id", id, "dest", dest, "err", err)
	}
	// Repoint the search/RAG index + notes inventory so the moved note follows
	// its file instead of going stale at the old path (best-effort).
	h.reindexMove(r.Context(), src, dest)
	// Prune the source folder if moving the note out just emptied it — same
	// zombie risk as delete (the device never sends a folder delete, so an
	// emptied user folder would re-sync via list_folder). Native buckets are
	// never pruned.
	h.pruneEmptyParents(filepath.Dir(src))
	h.respondEntry(w, r, dest, func(e *dto.EntriesVO) {
		envelope.WriteJSON(w, dto.FileMoveLocalVO{BaseVO: envelope.OK(), EquipmentNo: req.EquipmentNo, EntriesVO: e})
	}, func() { fail(errMoveMissingCode, errMoveMissingMsg) })
}

// Copy duplicates a file into the to_path parent directory; the copy receives a
// fresh id. POST /api/file/3/files/copy_v3 (F_FileLocalController.java:184).
func (h *MutationHandler) Copy(w http.ResponseWriter, r *http.Request) {
	var req dto.FileCopyLocalDTO
	_ = json.NewDecoder(r.Body).Decode(&req)
	fail := func(code, msg string) {
		envelope.WriteJSON(w, dto.FileCopyLocalVO{
			BaseVO:      envelope.BaseVO{Success: false, ErrorCode: code, ErrorMsg: msg},
			EquipmentNo: req.EquipmentNo,
		})
	}

	_, src, ok := h.resolveSource(r, req.ID)
	if !ok {
		fail(errCopyMissingCode, errCopyMissingMsg)
		return
	}
	dest, code := h.targetPath(req.ToPath, req.Autorename)
	if code != "" {
		fail(codeMsg(code))
		return
	}
	if err := copyFile(src, dest); err != nil {
		h.log().Error("copy_v3 copyFile", "src", src, "dest", dest, "err", err)
		fail(errCopyMissingCode, errCopyMissingMsg)
		return
	}
	// Duplicate the search/RAG index entries so the copy is immediately
	// searchable without re-OCR (best-effort).
	h.reindexCopy(r.Context(), src, dest)
	h.respondEntry(w, r, dest, func(e *dto.EntriesVO) {
		envelope.WriteJSON(w, dto.FileCopyLocalVO{BaseVO: envelope.OK(), EquipmentNo: req.EquipmentNo, EntriesVO: e})
	}, func() { fail(errCopyMissingCode, errCopyMissingMsg) })
}

// resolveSource parses a String-in id and resolves it to a live absolute path.
func (h *MutationHandler) resolveSource(r *http.Request, rawID string) (id int64, abs string, ok bool) {
	id, perr := strconv.ParseInt(rawID, 10, 64)
	if perr != nil || h.Root == "" {
		return 0, "", false
	}
	p, found, err := h.Reg.PathFor(r.Context(), id)
	if err != nil || !found {
		return 0, "", false
	}
	if _, err := os.Lstat(p); err != nil {
		return 0, "", false
	}
	return id, p, true
}

// targetPath resolves the move/copy destination. The device sends `to_path` as
// the FULL destination path (including the new filename), NOT a parent directory
// — despite the decompiled "Target parent directory" annotation. Confirmed on
// hardware 2026-05-24: a rename sent to_path=/NOTE/Note/<newname>.note (see §8).
// Traversal-guarded. On a collision it returns errSameNameCode unless autorename
// is set, in which case it appends " (n)" before the extension until free.
func (h *MutationHandler) targetPath(toPath string, autorename bool) (dest, code string) {
	dest, err := mapping.SafeResolve(h.Root, toPath)
	if err != nil {
		return "", errSameNameCode // escaping path: refuse (never write outside root)
	}
	if _, err := os.Lstat(dest); os.IsNotExist(err) {
		return dest, ""
	}
	if !autorename {
		return "", errSameNameCode
	}
	ext := filepath.Ext(dest)
	base := strings.TrimSuffix(dest, ext)
	for n := 1; n < 1000; n++ {
		cand := fmt.Sprintf("%s (%d)%s", base, n, ext)
		if _, err := os.Lstat(cand); os.IsNotExist(err) {
			return cand, ""
		}
	}
	return "", errSameNameCode
}

// respondEntry builds the EntriesVO for the file now at dest and hands it to ok;
// if the entry can't be built it calls bad.
func (h *MutationHandler) respondEntry(w http.ResponseWriter, r *http.Request, dest string, ok func(*dto.EntriesVO), bad func()) {
	entry, err := mapping.EntryFor(r.Context(), h.Root, dest, h.Reg)
	if err != nil {
		h.log().Error("mutation EntryFor", "path", dest, "err", err)
		bad()
		return
	}
	if h.Notifier != nil {
		_ = h.Notifier.NotifyFile(r.Context())
	}
	ok(&entry)
}

// codeMsg maps an error code to its (code, message) pair for the fail helpers.
func codeMsg(code string) (string, string) {
	switch code {
	case errSameNameCode:
		return errSameNameCode, errSameNameMsg
	default:
		return code, ""
	}
}

// copyFile copies src to dst byte-for-byte, creating parent dirs.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, cerr := io.Copy(out, in)
	closeErr := out.Close()
	if cerr != nil {
		return cerr
	}
	return closeErr
}
