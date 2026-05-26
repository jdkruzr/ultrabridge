package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/sysop/ultrabridge/internal/digeststore"
	"github.com/sysop/ultrabridge/internal/spcserver/dto"
	"github.com/sysop/ultrabridge/internal/spcserver/envelope"
	"github.com/sysop/ultrabridge/internal/spcserver/oss"
	"github.com/sysop/ultrabridge/internal/spcserver/staging"
)

// markDir is the hidden subtree under FILE_ROOT where digest handwriting (.mark)
// blobs live, keyed by their server-chosen innerName. Dot-prefixed so it is
// excluded from list_folder (isHidden), like .staging/.recycle.
const markDir = ".digests"

// markApplyTTL bounds an unfinished .mark upload slot (reclaimed by the staging
// Sweep). Matches the file upload window.
const markApplyTTL = 30 * time.Minute

// DigestStore is the slice of the canonical digest store the summary handler
// needs (the protocol layer owns no storage — cf. TaskStore). digeststore.Store
// satisfies it.
type DigestStore interface {
	Create(ctx context.Context, d *digeststore.Digest) (int64, error)
	Update(ctx context.Context, d *digeststore.Digest) error
	SoftDelete(ctx context.Context, userID, id int64) error
	SoftDeleteByParent(ctx context.Context, userID int64, parentUID string) (int64, error)
	GetByID(ctx context.Context, userID, id int64) (*digeststore.Digest, error)
	GetByUniqueIdentifier(ctx context.Context, userID int64, uid string) (*digeststore.Digest, error)
	List(ctx context.Context, userID int64, isGroup bool, parentUID string, page, size int) ([]digeststore.Digest, int64, error)
	ListByIDs(ctx context.Context, userID int64, ids []int64) ([]digeststore.Digest, error)
	CreateTag(ctx context.Context, userID int64, name string) (int64, error)
	UpdateTag(ctx context.Context, userID, id int64, name string) error
	DeleteTag(ctx context.Context, userID, id int64) error
	ListTags(ctx context.Context, userID int64) ([]digeststore.Tag, error)
}

// DigestIndexer makes a digest item searchable in UB's shared FTS5/RAG index
// (satisfied by *digestindex.Bridge). Calls are best-effort and non-blocking;
// a nil Indexer disables UB-side surfacing (the digest still round-trips to the
// device). The protocol layer owns no index, just as it owns no storage.
type DigestIndexer interface {
	Index(uid, name, content, comment, tags string)
	Deindex(uid string)
}

// SummaryHandler implements F_SummaryController: the device-facing digest
// ("summary") sync surface — item/group/tag CRUD plus the .mark handwriting
// blob transfer, which reuses the Phase 3/4 OSS signed-URL + staging path.
// Storage lives in Store; the handler is a thin DTO↔domain adapter. Root/Signer/
// Staging are shared with the file handlers; an empty Root or nil Staging
// disables .mark transfer (records still round-trip text-only).
type SummaryHandler struct {
	Store   DigestStore
	Root    string
	Signer  *oss.Signer
	Staging *staging.Store
	Indexer DigestIndexer // optional; nil disables UB-side search/RAG surfacing
	Logger  *slog.Logger
}

// index (re)indexes a digest item for UB search/RAG. No-op for groups, for
// records without a stable identity, or when no indexer is wired.
func (h *SummaryHandler) index(d *digeststore.Digest) {
	if h.Indexer == nil || d == nil || d.IsGroup || d.UniqueIdentifier == "" {
		return
	}
	h.Indexer.Index(d.UniqueIdentifier, d.Name, d.Content, d.CommentStr, d.Tags)
}

// deindex removes a digest from UB search/RAG by its stable identity.
func (h *SummaryHandler) deindex(uid string) {
	if h.Indexer == nil || uid == "" {
		return
	}
	h.Indexer.Deindex(uid)
}

func (h *SummaryHandler) log() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

// --- Items ---

// AddSummary handles POST /api/file/add/summary. Idempotent on uniqueIdentifier:
// a re-pushed digest updates the existing row rather than duplicating it.
func (h *SummaryHandler) AddSummary(w http.ResponseWriter, r *http.Request) {
	var req dto.AddSummaryDTO
	_ = json.NewDecoder(r.Body).Decode(&req)
	uid := userIDInt(r)

	h.promoteMark(r.Context(), req.HandwriteInnerName, req.HandwriteMD5)

	// An item's stable identity is its metadata.unique_identifier, NOT the
	// top-level uniqueIdentifier (which the device leaves empty for items —
	// device-confirmed 2026-05-25, see spc-protocol.md §8). Dedup on whichever is
	// present (top-level for the group-like case, metadata otherwise) and persist
	// it in the unique_identifier column so a re-push never duplicates.
	effectiveUID := req.UniqueIdentifier
	if effectiveUID == "" {
		effectiveUID = metadataUID(req.Metadata)
	}

	// Upsert by the effective identity so a full re-sync doesn't duplicate.
	if effectiveUID != "" {
		if existing, err := h.Store.GetByUniqueIdentifier(r.Context(), uid, effectiveUID); err == nil {
			applyAddToDigest(existing, &req, effectiveUID)
			if err := h.Store.Update(r.Context(), existing); err != nil {
				h.internalErr(w, "add/summary update", err)
				return
			}
			h.index(existing)
			envelope.WriteJSON(w, dto.AddSummaryVO{BaseVO: envelope.OK(), ID: existing.ID})
			return
		}
	}

	d := &digeststore.Digest{UserID: uid}
	applyAddToDigest(d, &req, effectiveUID)
	id, err := h.Store.Create(r.Context(), d)
	if err != nil {
		h.internalErr(w, "add/summary create", err)
		return
	}
	h.index(d)
	envelope.WriteJSON(w, dto.AddSummaryVO{BaseVO: envelope.OK(), ID: id})
}

// UpdateSummary handles PUT /api/file/update/summary. Mutable fields are overlaid
// onto the existing record so fields absent from the DTO (uniqueIdentifier, name,
// fileId, creationTime) are preserved.
func (h *SummaryHandler) UpdateSummary(w http.ResponseWriter, r *http.Request) {
	var req dto.UpdateSummaryDTO
	_ = json.NewDecoder(r.Body).Decode(&req)
	uid := userIDInt(r)

	h.promoteMark(r.Context(), req.HandwriteInnerName, req.HandwriteMD5)

	existing, err := h.Store.GetByID(r.Context(), uid, req.ID)
	if err != nil {
		// Unknown id — accept idempotently (device may re-send); nothing to update.
		h.log().Warn("update/summary: id not found", "id", req.ID)
		envelope.WriteJSON(w, envelope.OK())
		return
	}
	existing.ParentUniqueIdentifier = req.ParentUniqueIdentifier
	existing.Content = req.Content
	existing.SourcePath = req.SourcePath
	existing.DataSource = req.DataSource
	existing.SourceType = req.SourceType
	existing.Tags = req.Tags
	existing.MD5Hash = req.MD5Hash
	existing.Metadata = req.Metadata
	existing.CommentStr = req.CommentStr
	existing.CommentHandwriteName = req.CommentHandwriteName
	existing.HandwriteInnerName = req.HandwriteInnerName
	existing.HandwriteMD5 = req.HandwriteMD5
	existing.LastModifiedTime = req.LastModifiedTime
	existing.Author = req.Author
	if err := h.Store.Update(r.Context(), existing); err != nil {
		h.internalErr(w, "update/summary", err)
		return
	}
	h.index(existing)
	envelope.WriteJSON(w, envelope.OK())
}

// DeleteSummary handles DELETE /api/file/delete/summary (soft delete, idempotent).
func (h *SummaryHandler) DeleteSummary(w http.ResponseWriter, r *http.Request) {
	var req dto.DeleteSummaryDTO
	_ = json.NewDecoder(r.Body).Decode(&req)
	uid := userIDInt(r)
	// Resolve the stable identity before delete so we can drop the search/RAG
	// row (GetByID excludes soft-deleted rows, so it must run first).
	var digestUID string
	if d, err := h.Store.GetByID(r.Context(), uid, req.ID); err == nil {
		digestUID = d.UniqueIdentifier
	}
	if err := h.Store.SoftDelete(r.Context(), uid, req.ID); err != nil && !errors.Is(err, digeststore.ErrNotFound) {
		h.internalErr(w, "delete/summary", err)
		return
	}
	h.deindex(digestUID)
	envelope.WriteJSON(w, envelope.OK())
}

// --- Groups ---

// AddSummaryGroup handles POST /api/file/add/summary/group. Idempotent on uid.
func (h *SummaryHandler) AddSummaryGroup(w http.ResponseWriter, r *http.Request) {
	var req dto.AddSummaryGroupDTO
	_ = json.NewDecoder(r.Body).Decode(&req)
	uid := userIDInt(r)

	if req.UniqueIdentifier != "" {
		if existing, err := h.Store.GetByUniqueIdentifier(r.Context(), uid, req.UniqueIdentifier); err == nil {
			existing.Name = req.Name
			existing.Description = req.Description
			existing.MD5Hash = req.MD5Hash
			existing.LastModifiedTime = req.LastModifiedTime
			if err := h.Store.Update(r.Context(), existing); err != nil {
				h.internalErr(w, "add/summary/group update", err)
				return
			}
			envelope.WriteJSON(w, dto.AddSummaryGroupVO{BaseVO: envelope.OK(), ID: existing.ID})
			return
		}
	}

	d := &digeststore.Digest{
		UserID:           uid,
		IsGroup:          true,
		UniqueIdentifier: req.UniqueIdentifier,
		Name:             req.Name,
		Description:      req.Description,
		MD5Hash:          req.MD5Hash,
		CreationTime:     req.CreationTime,
		LastModifiedTime: req.LastModifiedTime,
	}
	id, err := h.Store.Create(r.Context(), d)
	if err != nil {
		h.internalErr(w, "add/summary/group create", err)
		return
	}
	envelope.WriteJSON(w, dto.AddSummaryGroupVO{BaseVO: envelope.OK(), ID: id})
}

// UpdateSummaryGroup handles PUT /api/file/update/summary/group.
func (h *SummaryHandler) UpdateSummaryGroup(w http.ResponseWriter, r *http.Request) {
	var req dto.UpdateSummaryGroupDTO
	_ = json.NewDecoder(r.Body).Decode(&req)
	uid := userIDInt(r)

	h.promoteMark(r.Context(), req.HandwriteInnerName, "")

	existing, err := h.Store.GetByID(r.Context(), uid, req.ID)
	if err != nil {
		h.log().Warn("update/summary/group: id not found", "id", req.ID)
		envelope.WriteJSON(w, envelope.OK())
		return
	}
	if req.UniqueIdentifier != "" {
		existing.UniqueIdentifier = req.UniqueIdentifier
	}
	existing.Name = req.Name
	existing.Description = req.Description
	existing.Metadata = req.Metadata
	existing.CommentStr = req.CommentStr
	existing.CommentHandwriteName = req.CommentHandwriteName
	existing.HandwriteInnerName = req.HandwriteInnerName
	existing.MD5Hash = req.MD5Hash
	existing.LastModifiedTime = req.LastModifiedTime
	if err := h.Store.Update(r.Context(), existing); err != nil {
		h.internalErr(w, "update/summary/group", err)
		return
	}
	envelope.WriteJSON(w, envelope.OK())
}

// DeleteSummaryGroup handles DELETE /api/file/delete/summary/group. Deleting a
// group cascades a soft-delete to its member items (matching SummaryMapper's
// softDeletionSummaryByParentUniqueIdentifier).
func (h *SummaryHandler) DeleteSummaryGroup(w http.ResponseWriter, r *http.Request) {
	var req dto.DeleteSummaryGroupDTO
	_ = json.NewDecoder(r.Body).Decode(&req)
	uid := userIDInt(r)

	if grp, err := h.Store.GetByID(r.Context(), uid, req.ID); err == nil && grp.UniqueIdentifier != "" {
		// Deindex member items before the cascade soft-delete (List excludes
		// soft-deleted rows, so it must run first). size<=0 = no limit.
		if members, _, lerr := h.Store.List(r.Context(), uid, false, grp.UniqueIdentifier, 0, 0); lerr == nil {
			for i := range members {
				h.deindex(members[i].UniqueIdentifier)
			}
		}
		if _, derr := h.Store.SoftDeleteByParent(r.Context(), uid, grp.UniqueIdentifier); derr != nil {
			h.log().Warn("delete/summary/group cascade", "uid", grp.UniqueIdentifier, "err", derr)
		}
	}
	if err := h.Store.SoftDelete(r.Context(), uid, req.ID); err != nil && !errors.Is(err, digeststore.ErrNotFound) {
		h.internalErr(w, "delete/summary/group", err)
		return
	}
	envelope.WriteJSON(w, envelope.OK())
}

// --- Queries ---

// QuerySummary handles POST /api/file/query/summary (all items, paginated).
func (h *SummaryHandler) QuerySummary(w http.ResponseWriter, r *http.Request) {
	var req dto.QuerySummaryDTO
	_ = json.NewDecoder(r.Body).Decode(&req)
	rows, total, err := h.Store.List(r.Context(), userIDInt(r), false, req.ParentUniqueIdentifier, req.Page, req.Size)
	if err != nil {
		h.internalErr(w, "query/summary", err)
		return
	}
	envelope.WriteJSON(w, dto.QuerySummaryVO{
		BaseVO:        envelope.OK(),
		TotalRecords:  total,
		TotalPages:    totalPages(total, req.Size),
		CurrentPage:   pageOrOne(req.Page),
		PageSize:      req.Size,
		SummaryDOList: toSummaryDOs(rows),
	})
}

// QuerySummaryGroup handles POST /api/file/query/summary/group (all groups).
func (h *SummaryHandler) QuerySummaryGroup(w http.ResponseWriter, r *http.Request) {
	var req dto.QuerySummaryGroupDTO
	_ = json.NewDecoder(r.Body).Decode(&req)
	rows, total, err := h.Store.List(r.Context(), userIDInt(r), true, "", req.Page, req.Size)
	if err != nil {
		h.internalErr(w, "query/summary/group", err)
		return
	}
	envelope.WriteJSON(w, dto.QuerySummaryGroupVO{
		BaseVO:        envelope.OK(),
		TotalRecords:  total,
		TotalPages:    totalPages(total, req.Size),
		CurrentPage:   pageOrOne(req.Page),
		PageSize:      req.Size,
		SummaryDOList: toSummaryDOs(rows),
	})
}

// QuerySummaryHash handles POST /api/file/query/summary/hash: the lightweight
// id+md5 list the device diffs to decide what to push/pull.
func (h *SummaryHandler) QuerySummaryHash(w http.ResponseWriter, r *http.Request) {
	var req dto.QuerySummaryDTO
	_ = json.NewDecoder(r.Body).Decode(&req)
	rows, total, err := h.Store.List(r.Context(), userIDInt(r), false, req.ParentUniqueIdentifier, req.Page, req.Size)
	if err != nil {
		h.internalErr(w, "query/summary/hash", err)
		return
	}
	infos := make([]dto.SummaryInfoVO, 0, len(rows))
	for i := range rows {
		d := &rows[i]
		infos = append(infos, dto.SummaryInfoVO{
			ID:                   d.ID,
			UserID:               d.UserID,
			MD5Hash:              d.MD5Hash,
			HandwriteMd5:         d.HandwriteMD5,
			CommentHandwriteName: d.CommentHandwriteName,
			LastModifiedTime:     d.LastModifiedTime,
			MetadataMap:          parseMetadataMap(d.Metadata),
		})
	}
	envelope.WriteJSON(w, dto.QuerySummaryMD5HashVO{
		BaseVO:            envelope.OK(),
		TotalRecords:      total,
		TotalPages:        totalPages(total, req.Size),
		CurrentPage:       pageOrOne(req.Page),
		PageSize:          req.Size,
		SummaryInfoVOList: infos,
	})
}

// QuerySummaryByID handles POST /api/file/query/summary/id.
func (h *SummaryHandler) QuerySummaryByID(w http.ResponseWriter, r *http.Request) {
	var req dto.QuerySummaryDTO
	_ = json.NewDecoder(r.Body).Decode(&req)
	rows, err := h.Store.ListByIDs(r.Context(), userIDInt(r), req.IDs)
	if err != nil {
		h.internalErr(w, "query/summary/id", err)
		return
	}
	envelope.WriteJSON(w, dto.QuerySummaryByIdVO{BaseVO: envelope.OK(), SummaryDOList: toSummaryDOs(rows)})
}

// --- Tags ---

// AddSummaryTag handles POST /api/file/add/summary/tag (idempotent by name).
func (h *SummaryHandler) AddSummaryTag(w http.ResponseWriter, r *http.Request) {
	var req dto.AddSummaryTagDTO
	_ = json.NewDecoder(r.Body).Decode(&req)
	uid := userIDInt(r)

	if existing, _ := h.Store.ListTags(r.Context(), uid); existing != nil {
		for _, t := range existing {
			if t.Name == req.Name {
				envelope.WriteJSON(w, dto.AddSummaryTagVO{BaseVO: envelope.OK(), ID: t.ID})
				return
			}
		}
	}
	id, err := h.Store.CreateTag(r.Context(), uid, req.Name)
	if err != nil {
		h.internalErr(w, "add/summary/tag", err)
		return
	}
	envelope.WriteJSON(w, dto.AddSummaryTagVO{BaseVO: envelope.OK(), ID: id})
}

// UpdateSummaryTag handles PUT /api/file/update/summary/tag.
func (h *SummaryHandler) UpdateSummaryTag(w http.ResponseWriter, r *http.Request) {
	var req dto.UpdateSummaryTagDTO
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := h.Store.UpdateTag(r.Context(), userIDInt(r), req.ID, req.Name); err != nil && !errors.Is(err, digeststore.ErrNotFound) {
		h.internalErr(w, "update/summary/tag", err)
		return
	}
	envelope.WriteJSON(w, envelope.OK())
}

// DeleteSummaryTag handles DELETE /api/file/delete/summary/tag.
func (h *SummaryHandler) DeleteSummaryTag(w http.ResponseWriter, r *http.Request) {
	var req dto.DeleteSummaryTagDTO
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := h.Store.DeleteTag(r.Context(), userIDInt(r), req.ID); err != nil && !errors.Is(err, digeststore.ErrNotFound) {
		h.internalErr(w, "delete/summary/tag", err)
		return
	}
	envelope.WriteJSON(w, envelope.OK())
}

// QuerySummaryTag handles GET /api/file/query/summary/tag.
func (h *SummaryHandler) QuerySummaryTag(w http.ResponseWriter, r *http.Request) {
	tags, err := h.Store.ListTags(r.Context(), userIDInt(r))
	if err != nil {
		h.internalErr(w, "query/summary/tag", err)
		return
	}
	out := make([]dto.SummaryTagDO, 0, len(tags))
	for _, t := range tags {
		out = append(out, dto.SummaryTagDO{ID: t.ID, Name: t.Name, UserID: t.UserID, CreatedAt: t.CreatedAt})
	}
	envelope.WriteJSON(w, dto.QuerySummaryTagVO{BaseVO: envelope.OK(), SummaryTagDOList: out})
}

// --- .mark handwriting blob transfer (reuses the OSS signed-URL + staging path) ---

// UploadApplySummary handles POST /api/file/upload/apply/summary: mint an
// innerName + a presigned /api/oss/upload URL the device POSTs the .mark bytes
// to (the existing tokenless UploadStream sinks them into .staging). The blob is
// promoted to .digests at add/update time (see promoteMark).
func (h *SummaryHandler) UploadApplySummary(w http.ResponseWriter, r *http.Request) {
	var req dto.UploadSummaryApplyDTO
	_ = json.NewDecoder(r.Body).Decode(&req)

	innerName := newNonce() + ".mark"
	if h.Staging != nil {
		// target_path/file_name here are bookkeeping for the orphan sweep; the
		// real promotion target (.digests/<innerName>) is supplied at promote time.
		if err := h.Staging.Record(r.Context(), innerName, markDir, req.FileName, 0, markApplyTTL); err != nil {
			h.log().Error("upload/apply/summary Record", "innerName", innerName, "err", err)
		}
	}
	envelope.WriteJSON(w, dto.UploadSummaryApplyVO{
		BaseVO:        envelope.OK(),
		FullUploadURL: h.signedUploadURL(r, innerName),
		InnerName:     innerName,
	})
}

// DownloadSummary handles POST /api/file/download/summary: mint a presigned
// /api/oss/download URL for the digest's .mark blob (served by the existing
// DownloadStream). E0321 if the digest has no handwriting.
func (h *SummaryHandler) DownloadSummary(w http.ResponseWriter, r *http.Request) {
	var req dto.DownloadSummaryDTO
	_ = json.NewDecoder(r.Body).Decode(&req)

	d, err := h.Store.GetByID(r.Context(), userIDInt(r), req.ID)
	if err != nil || d.HandwriteInnerName == "" {
		envelope.WriteJSON(w, dto.DownloadSummaryVO{
			BaseVO: envelope.BaseVO{Success: false, ErrorCode: errFileNotExistCode, ErrorMsg: errFileNotExistMsg},
		})
		return
	}
	rel := path.Join(markDir, d.HandwriteInnerName)
	envelope.WriteJSON(w, dto.DownloadSummaryVO{BaseVO: envelope.OK(), URL: h.signedDownloadURL(r, rel)})
}

// promoteMark verifies + atomically promotes a staged .mark blob to .digests,
// best-effort: a missing blob, md5 mismatch, or absent staging is logged and
// skipped (the digest record still persists — text-first resilience; the blob
// can be re-uploaded). innerName "" (no handwriting on this digest) is a no-op.
func (h *SummaryHandler) promoteMark(ctx context.Context, innerName, md5 string) {
	if innerName == "" || h.Staging == nil {
		return
	}
	target := path.Join(markDir, innerName)
	if _, err := h.Staging.Finalize(ctx, innerName, md5, -1, target); err != nil {
		h.log().Warn("digest .mark promote failed", "innerName", innerName, "err", err)
	}
}

// signedUploadURL builds the presigned /api/oss/upload URL for a .mark innerName
// (fileSize signed as 0, like the file upload path).
func (h *SummaryHandler) signedUploadURL(r *http.Request, innerName string) string {
	encPath := oss.EncryptPath(innerName)
	ts := h.nowMillis()
	nonce := newNonce()
	sig := h.Signer.UploadSignature(encPath, ts, nonce, 0)
	return requestBaseURL(r) + "/api/oss/upload?signature=" + sig +
		"&timestamp=" + strconv.FormatInt(ts, 10) + "&nonce=" + nonce + "&path=" + encPath
}

// signedDownloadURL builds the presigned /api/oss/download URL for a root-relative
// blob path (e.g. .digests/<innerName>).
func (h *SummaryHandler) signedDownloadURL(r *http.Request, relPath string) string {
	encPath := oss.EncryptPath(relPath)
	ts := h.nowMillis()
	nonce := newNonce()
	sig := h.Signer.DownloadSignature(encPath, ts, nonce)
	return fmt.Sprintf("%s/api/oss/download?path=%s&signature=%s&timestamp=%s&nonce=%s&pathId=",
		requestBaseURL(r), encPath, sig, strconv.FormatInt(ts, 10), nonce)
}

func (h *SummaryHandler) nowMillis() int64 {
	if h.Signer != nil && h.Signer.Now != nil {
		return h.Signer.Now().UnixMilli()
	}
	return time.Now().UnixMilli()
}

func (h *SummaryHandler) internalErr(w http.ResponseWriter, op string, err error) {
	h.log().Error("summary "+op, "err", err)
	envelope.WriteJSON(w, envelope.BaseVO{Success: false, ErrorMsg: "internal error"})
}

// --- mapping helpers ---

// applyAddToDigest overlays an AddSummaryDTO onto a (new or existing) item.
// effectiveUID is the resolved stable identity (top-level uniqueIdentifier, or
// metadata.unique_identifier for items) — stored so the item is dedupable.
func applyAddToDigest(d *digeststore.Digest, req *dto.AddSummaryDTO, effectiveUID string) {
	d.IsGroup = false
	d.UniqueIdentifier = effectiveUID
	d.FileID = req.FileID
	d.ParentUniqueIdentifier = req.ParentUniqueIdentifier
	d.Content = req.Content
	d.DataSource = req.DataSource
	d.SourcePath = req.SourcePath
	d.SourceType = req.SourceType
	d.Tags = req.Tags
	d.MD5Hash = req.MD5Hash
	d.Metadata = req.Metadata
	d.CommentStr = req.CommentStr
	d.CommentHandwriteName = req.CommentHandwriteName
	d.HandwriteInnerName = req.HandwriteInnerName
	d.HandwriteMD5 = req.HandwriteMD5
	if req.CreationTime != 0 {
		d.CreationTime = req.CreationTime
	}
	d.LastModifiedTime = req.LastModifiedTime
	d.Author = req.Author
}

func toSummaryDOs(rows []digeststore.Digest) []dto.SummaryDO {
	out := make([]dto.SummaryDO, 0, len(rows))
	for i := range rows {
		out = append(out, toSummaryDO(&rows[i]))
	}
	return out
}

func toSummaryDO(d *digeststore.Digest) dto.SummaryDO {
	return dto.SummaryDO{
		ID:                     d.ID,
		FileID:                 d.FileID,
		Name:                   d.Name,
		UserID:                 d.UserID,
		UniqueIdentifier:       d.UniqueIdentifier,
		ParentUniqueIdentifier: d.ParentUniqueIdentifier,
		Content:                d.Content,
		SourcePath:             d.SourcePath,
		DataSource:             d.DataSource,
		SourceType:             d.SourceType,
		IsSummaryGroup:         ynOf(d.IsGroup),
		Description:            d.Description,
		Tags:                   d.Tags,
		MD5Hash:                d.MD5Hash,
		Metadata:               d.Metadata,
		CommentStr:             d.CommentStr,
		CommentHandwriteName:   d.CommentHandwriteName,
		HandwriteInnerName:     d.HandwriteInnerName,
		HandwriteMD5:           d.HandwriteMD5,
		CreationTime:           d.CreationTime,
		LastModifiedTime:       d.LastModifiedTime,
		IsDeleted:              ynOf(d.IsDeleted),
		CreateTime:             d.CreatedAt,
		UpdateTime:             d.UpdatedAt,
		Author:                 d.Author,
	}
}

func ynOf(b bool) string {
	if b {
		return "Y"
	}
	return "N"
}

// metadataUID extracts the item's stable identity from its metadata JSON
// (the device puts it in metadata.unique_identifier, not the top-level field).
// Returns "" if absent/unparseable.
func metadataUID(metadata string) string {
	return parseMetadataMap(metadata)["unique_identifier"]
}

// parseMetadataMap converts the stored metadata JSON string into the
// Map<String,String> the device's SummaryInfoVO carries. Decoding uses
// json.Number so numeric values keep their literal form — without it, a large
// int like source_size 18992668 decodes to float64 and stringifies as
// "1.8992668e+07" (device-confirmed corruption, 2026-05-25). An empty/invalid
// metadata yields nil (serialized as null).
func parseMetadataMap(s string) map[string]string {
	if s == "" {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		switch vv := v.(type) {
		case string:
			out[k] = vv
		case json.Number:
			out[k] = vv.String()
		case nil:
			out[k] = ""
		case bool:
			out[k] = strconv.FormatBool(vv)
		default:
			b, _ := json.Marshal(vv)
			out[k] = string(b)
		}
	}
	return out
}

// totalPages returns the page count for a total and page size (1 when size<=0).
func totalPages(total int64, size int) int {
	if size <= 0 {
		return 1
	}
	return int((total + int64(size) - 1) / int64(size))
}

func pageOrOne(page int) int {
	if page < 1 {
		return 1
	}
	return page
}
