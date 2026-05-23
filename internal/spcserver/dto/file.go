package dto

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
