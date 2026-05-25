package dto

import "github.com/sysop/ultrabridge/internal/spcserver/envelope"

// Summary (a.k.a. "digest") request DTOs and response VOs for F_SummaryController
// (/api/file/.../summary*). Field names are verbatim from the decompiled
// com/ratta/file/{dto,vo,domain}/*.java — NONE carry @JsonProperty, so Jackson
// serializes the Java field names as-is (camelCase). Two casing traps confirmed
// against source:
//   - request/domain use handwriteMD5 (uppercase) but the lightweight response
//     SummaryInfoVO uses handwriteMd5 (lowercase d5) — SummaryInfoVO.java:15.
//   - SummaryDO.isSummaryGroup / isDeleted are Strings ("Y"/"N"), not booleans
//     (SummaryDO.java:25,36).
//
// CAPTURE-PENDING: SummaryDO.createTime/updateTime are java.util.Date on the real
// server; their wire form (epoch millis vs ISO-8601) depends on the Jackson
// config and has not been observed on the wire. They are modelled here as
// omitempty millis (server bookkeeping the device does not diff on) and must be
// confirmed against a device capture before relying on them.

// --- Item DTOs ---

// AddSummaryDTO is POST /api/file/add/summary (AddSummaryDTO.java).
type AddSummaryDTO struct {
	UniqueIdentifier       string `json:"uniqueIdentifier"`
	FileID                 int64  `json:"fileId"`
	ParentUniqueIdentifier string `json:"parentUniqueIdentifier"`
	Content                string `json:"content"`
	DataSource             string `json:"dataSource"`
	SourcePath             string `json:"sourcePath"`
	SourceType             int    `json:"sourceType"`
	Tags                   string `json:"tags"`
	MD5Hash                string `json:"md5Hash"`
	Metadata               string `json:"metadata"`
	CommentStr             string `json:"commentStr"`
	CommentHandwriteName   string `json:"commentHandwriteName"`
	HandwriteInnerName     string `json:"handwriteInnerName"`
	HandwriteMD5           string `json:"handwriteMD5"`
	CreationTime           int64  `json:"creationTime"`
	LastModifiedTime       int64  `json:"lastModifiedTime"`
	Author                 string `json:"author"`
}

// UpdateSummaryDTO is PUT /api/file/update/summary (UpdateSummaryDTO.java). No
// creationTime/fileId — only mutable fields.
type UpdateSummaryDTO struct {
	ID                     int64  `json:"id"`
	ParentUniqueIdentifier string `json:"parentUniqueIdentifier"`
	Content                string `json:"content"`
	SourcePath             string `json:"sourcePath"`
	DataSource             string `json:"dataSource"`
	SourceType             int    `json:"sourceType"`
	Tags                   string `json:"tags"`
	MD5Hash                string `json:"md5Hash"`
	Metadata               string `json:"metadata"`
	CommentStr             string `json:"commentStr"`
	CommentHandwriteName   string `json:"commentHandwriteName"`
	HandwriteInnerName     string `json:"handwriteInnerName"`
	HandwriteMD5           string `json:"handwriteMD5"`
	LastModifiedTime       int64  `json:"lastModifiedTime"`
	Author                 string `json:"author"`
}

// DeleteSummaryDTO is DELETE /api/file/delete/summary (DeleteSummaryDTO.java).
type DeleteSummaryDTO struct {
	ID int64 `json:"id"`
}

// QuerySummaryDTO is the body of /query/summary and /query/summary/{hash,id}
// (QuerySummaryDTO.java). page is 1-based; ids is the explicit fetch list.
type QuerySummaryDTO struct {
	Page                   int     `json:"page"`
	Size                   int     `json:"size"`
	ParentUniqueIdentifier string  `json:"parentUniqueIdentifier"`
	IDs                    []int64 `json:"ids"`
}

// --- Group DTOs ---

// AddSummaryGroupDTO is POST /api/file/add/summary/group (AddSummaryGroupDTO.java).
type AddSummaryGroupDTO struct {
	UniqueIdentifier string `json:"uniqueIdentifier"`
	Name             string `json:"name"`
	Description      string `json:"description"`
	MD5Hash          string `json:"md5Hash"`
	CreationTime     int64  `json:"creationTime"`
	LastModifiedTime int64  `json:"lastModifiedTime"`
}

// UpdateSummaryGroupDTO is PUT /api/file/update/summary/group (UpdateSummaryGroupDTO.java).
type UpdateSummaryGroupDTO struct {
	ID                   int64  `json:"id"`
	UniqueIdentifier     string `json:"uniqueIdentifier"`
	Name                 string `json:"name"`
	Description          string `json:"description"`
	Metadata             string `json:"metadata"`
	CommentStr           string `json:"commentStr"`
	CommentHandwriteName string `json:"commentHandwriteName"`
	HandwriteInnerName   string `json:"handwriteInnerName"`
	MD5Hash              string `json:"md5Hash"`
	LastModifiedTime     int64  `json:"lastModifiedTime"`
}

// DeleteSummaryGroupDTO is DELETE /api/file/delete/summary/group.
type DeleteSummaryGroupDTO struct {
	ID int64 `json:"id"`
}

// QuerySummaryGroupDTO is POST /api/file/query/summary/group (QuerySummaryGroupDTO.java).
type QuerySummaryGroupDTO struct {
	Page int `json:"page"`
	Size int `json:"size"`
}

// --- Tag DTOs ---

// AddSummaryTagDTO is POST /api/file/add/summary/tag.
type AddSummaryTagDTO struct {
	Name string `json:"name"`
}

// UpdateSummaryTagDTO is PUT /api/file/update/summary/tag.
type UpdateSummaryTagDTO struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// DeleteSummaryTagDTO is DELETE /api/file/delete/summary/tag.
type DeleteSummaryTagDTO struct {
	ID int64 `json:"id"`
}

// --- .mark blob DTOs ---

// UploadSummaryApplyDTO is POST /api/file/upload/apply/summary (UploadSummaryApplyDTO.java).
type UploadSummaryApplyDTO struct {
	FileName    string `json:"fileName"`
	EquipmentNo string `json:"equipmentNo"`
}

// DownloadSummaryDTO is POST /api/file/download/summary (DownloadSummaryDTO.java).
type DownloadSummaryDTO struct {
	ID int64 `json:"id"`
}

// --- Domain-object wire shapes ---

// SummaryDO is the full digest record returned in the query VOs' summaryDOList
// (com/ratta/file/domain/SummaryDO.java). isSummaryGroup/isDeleted are "Y"/"N"
// strings. createTime/updateTime are capture-pending (see file header).
type SummaryDO struct {
	ID                     int64  `json:"id"`
	FileID                 int64  `json:"fileId,omitempty"`
	Name                   string `json:"name"`
	UserID                 int64  `json:"userId"`
	UniqueIdentifier       string `json:"uniqueIdentifier"`
	ParentUniqueIdentifier string `json:"parentUniqueIdentifier"`
	Content                string `json:"content"`
	SourcePath             string `json:"sourcePath"`
	DataSource             string `json:"dataSource"`
	SourceType             int    `json:"sourceType"`
	IsSummaryGroup         string `json:"isSummaryGroup"`
	Description            string `json:"description"`
	Tags                   string `json:"tags"`
	MD5Hash                string `json:"md5Hash"`
	Metadata               string `json:"metadata"`
	CommentStr             string `json:"commentStr"`
	CommentHandwriteName   string `json:"commentHandwriteName"`
	HandwriteInnerName     string `json:"handwriteInnerName"`
	HandwriteMD5           string `json:"handwriteMD5"`
	CreationTime           int64  `json:"creationTime"`
	LastModifiedTime       int64  `json:"lastModifiedTime"`
	IsDeleted              string `json:"isDeleted"`
	CreateTime             int64  `json:"createTime,omitempty"`
	UpdateTime             int64  `json:"updateTime,omitempty"`
	Author                 string `json:"author"`
}

// SummaryTagDO is one tag in QuerySummaryTagVO.summaryTagDOList
// (com/ratta/file/domain/SummaryTagDO.java).
type SummaryTagDO struct {
	ID               int64  `json:"id"`
	Name             string `json:"name"`
	UserID           int64  `json:"userId"`
	UniqueIdentifier string `json:"uniqueIdentifier,omitempty"`
	CreatedAt        int64  `json:"createdAt,omitempty"`
}

// SummaryInfoVO is the lightweight per-digest record in QuerySummaryMD5HashVO
// (com/ratta/file/vo/SummaryInfoVO.java) — the device diffs these by md5Hash to
// decide what to push/pull. NOTE handwriteMd5 (lowercase d5), unlike the
// uppercase handwriteMD5 everywhere else.
type SummaryInfoVO struct {
	ID                   int64             `json:"id"`
	UserID               int64             `json:"userId"`
	MD5Hash              string            `json:"md5Hash"`
	HandwriteMd5         string            `json:"handwriteMd5"`
	CommentHandwriteName string            `json:"commentHandwriteName"`
	LastModifiedTime     int64             `json:"lastModifiedTime"`
	MetadataMap          map[string]string `json:"metadataMap"`
}

// --- Response VOs ---

// AddSummaryVO is the /add/summary response (AddSummaryVO.java).
type AddSummaryVO struct {
	envelope.BaseVO
	ID int64 `json:"id"`
}

// AddSummaryGroupVO is the /add/summary/group response.
type AddSummaryGroupVO struct {
	envelope.BaseVO
	ID int64 `json:"id"`
}

// AddSummaryTagVO is the /add/summary/tag response.
type AddSummaryTagVO struct {
	envelope.BaseVO
	ID int64 `json:"id"`
}

// QuerySummaryByIdVO is the /query/summary/id response (QuerySummaryByIdVO.java).
type QuerySummaryByIdVO struct {
	envelope.BaseVO
	SummaryDOList []SummaryDO `json:"summaryDOList"`
}

// QuerySummaryVO is the /query/summary response (QuerySummaryVO.java).
type QuerySummaryVO struct {
	envelope.BaseVO
	TotalRecords  int64       `json:"totalRecords"`
	TotalPages    int         `json:"totalPages"`
	CurrentPage   int         `json:"currentPage"`
	PageSize      int         `json:"pageSize"`
	SummaryDOList []SummaryDO `json:"summaryDOList"`
}

// QuerySummaryGroupVO is the /query/summary/group response (QuerySummaryGroupVO.java).
type QuerySummaryGroupVO struct {
	envelope.BaseVO
	TotalRecords  int64       `json:"totalRecords"`
	TotalPages    int         `json:"totalPages"`
	CurrentPage   int         `json:"currentPage"`
	PageSize      int         `json:"pageSize"`
	SummaryDOList []SummaryDO `json:"summaryDOList"`
}

// QuerySummaryMD5HashVO is the /query/summary/hash response (QuerySummaryMD5HashVO.java).
type QuerySummaryMD5HashVO struct {
	envelope.BaseVO
	TotalRecords      int64           `json:"totalRecords"`
	TotalPages        int             `json:"totalPages"`
	CurrentPage       int             `json:"currentPage"`
	PageSize          int             `json:"pageSize"`
	SummaryInfoVOList []SummaryInfoVO `json:"summaryInfoVOList"`
}

// QuerySummaryTagVO is the /query/summary/tag response (QuerySummaryTagVO.java).
type QuerySummaryTagVO struct {
	envelope.BaseVO
	SummaryTagDOList []SummaryTagDO `json:"summaryTagDOList"`
}

// UploadSummaryApplyVO is the /upload/apply/summary response (UploadSummaryApplyVO.java).
type UploadSummaryApplyVO struct {
	envelope.BaseVO
	FullUploadURL string `json:"fullUploadUrl"`
	PartUploadURL string `json:"partUploadUrl"`
	InnerName     string `json:"innerName"`
}

// DownloadSummaryVO is the /download/summary response (DownloadSummaryVO.java).
type DownloadSummaryVO struct {
	envelope.BaseVO
	URL string `json:"url"`
}
