package dto

import "github.com/sysop/ultrabridge/internal/spcserver/envelope"

// EntriesVO is the Dropbox-style file/folder entry the device reads in
// list_folder and the single-entry query VOs. Field names are verbatim from
// com/ratta/file/vo/EntriesVO.java (no @JsonProperty overrides — Jackson
// serializes the Java field names as-is, so the snake_case names below are the
// real wire keys). Tag is "file" or "folder" (EntriesVO.java:16 "文件夹或者文件标志").
// ContentHash carries the file MD5 (EntriesVO.java:24 "文件md5").
type EntriesVO struct {
	Tag            string `json:"tag"`
	ID             string `json:"id"`
	Name           string `json:"name"`
	PathDisplay    string `json:"path_display"`
	ContentHash    string `json:"content_hash"`
	IsDownloadable bool   `json:"is_downloadable"`
	Size           int64  `json:"size"`
	LastUpdateTime int64  `json:"lastUpdateTime"`
	ParentPath     string `json:"parent_path"`
}

// CapacityVO is the response to POST /api/file/capacity/query (the variant the
// device hits in 0b). Extends BaseVO (com/ratta/file/vo/CapacityVO.java) so
// success/usedCapacity/totalCapacity serialize flat.
type CapacityVO struct {
	envelope.BaseVO
	UsedCapacity  int64 `json:"usedCapacity"`
	TotalCapacity int64 `json:"totalCapacity"`
}

// CapacityLocalDTO is the get_space_usage request (CapacityLocalDTO.java:
// equipmentNo). capacity/query takes no body.
type CapacityLocalDTO struct {
	EquipmentNo string `json:"equipmentNo"`
}

// CapacityLocalVO is the response to POST /api/file/2/users/get_space_usage.
// Extends BaseVO (com/ratta/file/vo/CapacityLocalVO.java); allocationVO is a
// nested object (AllocationVO does NOT extend BaseVO).
type CapacityLocalVO struct {
	envelope.BaseVO
	Used         int64        `json:"used"`
	AllocationVO AllocationVO `json:"allocationVO"`
	EquipmentNo  string       `json:"equipmentNo"`
}

// AllocationVO is the nested quota descriptor inside CapacityLocalVO
// (com/ratta/file/vo/AllocationVO.java: tag, allocated).
type AllocationVO struct {
	Tag       string `json:"tag"`
	Allocated int64  `json:"allocated"`
}

// ListFolderLocalDTO is the list_folder request (ListFolderLocalDTO.java:
// equipmentNo, id Long, recursive). id is a pointer so a null/absent id (the
// device's way of asking for the root) is distinguishable from id 0.
// list_folder_v3 (ListFolderV3DTO) has the identical shape and decodes into this.
type ListFolderLocalDTO struct {
	EquipmentNo string `json:"equipmentNo"`
	ID          *int64 `json:"id"`
	Recursive   bool   `json:"recursive"`
}

// ListFolderLocalVO is the list_folder response (ListFolderLocalVO.java extends
// BaseVO: equipmentNo, entries).
type ListFolderLocalVO struct {
	envelope.BaseVO
	EquipmentNo string      `json:"equipmentNo"`
	Entries     []EntriesVO `json:"entries"`
}

// FileQueryLocalDTO is the query_v3 request (FileQueryLocalDTO.java: equipmentNo,
// id String).
type FileQueryLocalDTO struct {
	EquipmentNo string `json:"equipmentNo"`
	ID          string `json:"id"`
}

// FileQueryLocalVO is the query_v3 response (FileQueryLocalVO.java extends
// BaseVO: equipmentNo, entriesVO). EntriesVO is a pointer so a missing file
// serializes as null (the device probes existence via query_v3).
type FileQueryLocalVO struct {
	envelope.BaseVO
	EquipmentNo string     `json:"equipmentNo"`
	EntriesVO   *EntriesVO `json:"entriesVO"`
}

// FileQueryByPathLocalDTO is the query/by/path_v3 request
// (FileQueryByPathLocalDTO.java: equipmentNo, path String — may be non-normalized).
type FileQueryByPathLocalDTO struct {
	EquipmentNo string `json:"equipmentNo"`
	Path        string `json:"path"`
}

// FileQueryByPathLocalVO is the query/by/path_v3 response
// (FileQueryByPathLocalVO.java extends BaseVO: equipmentNo, entriesVO).
type FileQueryByPathLocalVO struct {
	envelope.BaseVO
	EquipmentNo string     `json:"equipmentNo"`
	EntriesVO   *EntriesVO `json:"entriesVO"`
}

// SynchronousStartLocalDTO / VO bracket a sync session (SynchronousStartLocalDTO
// .java: equipmentNo; SynchronousStartLocalVO.java extends BaseVO: equipmentNo,
// synType Boolean). synType's exact semantics (full vs incremental signal) are
// confirmed against the device in 2d.
type SynchronousStartLocalDTO struct {
	EquipmentNo string `json:"equipmentNo"`
}

type SynchronousStartLocalVO struct {
	envelope.BaseVO
	EquipmentNo string `json:"equipmentNo"`
	SynType     bool   `json:"synType"`
}

// SynchronousEndLocalDTO / VO close a sync session (SynchronousEndLocalDTO.java:
// equipmentNo, flag; SynchronousEndLocalVO.java extends BaseVO: equipmentNo).
type SynchronousEndLocalDTO struct {
	EquipmentNo string `json:"equipmentNo"`
	Flag        string `json:"flag"`
}

type SynchronousEndLocalVO struct {
	envelope.BaseVO
	EquipmentNo string `json:"equipmentNo"`
}

// CreateFolderLocalDTO / VO back create_folder_v2 (CreateFolderLocalDTO.java:
// equipmentNo, path, autorename; CreateFolderLocalVO.java extends BaseVO:
// equipmentNo, metadata). Phase 2 stubs this to success without writing, so
// metadata is omitted.
type CreateFolderLocalDTO struct {
	EquipmentNo string `json:"equipmentNo"`
	Path        string `json:"path"`
	Autorename  bool   `json:"autorename"`
}

type CreateFolderLocalVO struct {
	envelope.BaseVO
	EquipmentNo string      `json:"equipmentNo"`
	Metadata    *MetadataVO `json:"metadata,omitempty"`
}

// MetadataVO is the entry descriptor returned inside CreateFolderLocalVO
// (MetadataVO.java: tag, id, name, path_display) — a subset of EntriesVO.
type MetadataVO struct {
	Tag         string `json:"tag"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	PathDisplay string `json:"path_display"`
}

// FileQueryV2DTO / VO back query/deleteApi, a file-by-id query
// (FileQueryV2DTO.java: equipmentNo, id String; FileQueryV2VO.java extends
// BaseVO: equipmentNo, entriesVO). Not hit in 0b; Phase 2 returns success + null.
type FileQueryV2DTO struct {
	EquipmentNo string `json:"equipmentNo"`
	ID          string `json:"id"`
}

type FileQueryV2VO struct {
	envelope.BaseVO
	EquipmentNo string     `json:"equipmentNo"`
	EntriesVO   *EntriesVO `json:"entriesVO"`
}

// FileDownloadLocalDTO is the download_v3 request
// (com/ratta/file/dto/FileDownloadLocalDTO.java declares id as Long, but the
// device sends it as a QUOTED STRING — the SPC String-in/Long-out gotcha (§8),
// confirmed from device traffic 2026-05-23: {"equipmentNo":...,"id":"16"}.
// Jackson coerces the quoted value to Long server-side; Go's encoding/json will
// not unmarshal a string into int64, so id MUST be typed string here and parsed
// in the handler — same as FileQueryLocalDTO.ID for query_v3).
type FileDownloadLocalDTO struct {
	EquipmentNo string `json:"equipmentNo"`
	ID          string `json:"id"`
}

// FileDownloadLocalVO is the download_v3 response
// (com/ratta/file/vo/FileDownloadLocalVO.java extends BaseVO). url is the
// presigned /api/oss/download URL the device then GETs. Snake_case wire keys
// (path_display/content_hash/is_downloadable) match EntriesVO — no @JsonProperty
// in the Java, so the field names are the wire keys. id is a String here
// (String-out, Long-in — the SPC id-type split, §8). Size is a pointer so a
// missing file serializes null rather than 0.
type FileDownloadLocalVO struct {
	envelope.BaseVO
	EquipmentNo    string `json:"equipmentNo"`
	ID             string `json:"id"`
	URL            string `json:"url"`
	Name           string `json:"name"`
	PathDisplay    string `json:"path_display"`
	ContentHash    string `json:"content_hash"`
	Size           *int64 `json:"size"`
	IsDownloadable bool   `json:"is_downloadable"`
}

// FileDownloadApplyVO is the response to POST /api/oss/generate/download/url
// (com/ratta/file/vo/FileDownloadApplyVO.java). NOTE: it implements Serializable
// only and does NOT extend BaseVO — there is no success/errorCode field; the
// device reads the bare {url, signature, timestamp, nonce, pathId}.
type FileDownloadApplyVO struct {
	URL       string `json:"url"`
	Signature string `json:"signature"`
	Timestamp int64  `json:"timestamp"`
	Nonce     string `json:"nonce"`
	PathID    string `json:"pathId"`
}
