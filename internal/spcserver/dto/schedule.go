package dto

import "github.com/sysop/ultrabridge/internal/spcserver/envelope"

// SPCTask is the task wire shape. Field tags mirror the empirically-validated
// set in internal/tasksync/supernote/client.go:232-255 (which reads/writes
// exactly what real SPC emits). Quirk: completedTime holds creation time,
// lastModified holds completion time. taskId is a String here because that is
// what real SPC emits in task-list responses (proven by the working client);
// see docs/spc-protocol.md §8 for the String-in/Long-out gotcha that applies to
// some single-task VOs.
type SPCTask struct {
	ID               string `json:"taskId"`
	TaskListID       string `json:"taskListId,omitempty"`
	Title            string `json:"title"`
	Detail           string `json:"detail,omitempty"`
	Status           string `json:"status"`
	Importance       string `json:"importance,omitempty"`
	DueTime          int64  `json:"dueTime"`
	CompletedTime    int64  `json:"completedTime"`
	LastModified     int64  `json:"lastModified"`
	Recurrence       string `json:"recurrence,omitempty"`
	IsReminderOn     string `json:"isReminderOn"`
	Links            string `json:"links,omitempty"`
	IsDeleted        string `json:"isDeleted"`
	Sort             int    `json:"sort"`
	SortCompleted    int    `json:"sortCompleted"`
	SortTime         int64  `json:"sortTime,omitempty"`
	PlanerSort       int    `json:"planerSort"`
	PlanerSortTime   int64  `json:"planerSortTime,omitempty"`
	AllSort          int    `json:"allSort"`
	AllSortCompleted int    `json:"allSortCompleted"`
	AllSortTime      int64  `json:"allSortTime,omitempty"`
}

// ScheduleTaskDTO is the /schedule/task/all request (ScheduleTaskDTO.java). Note
// nextPageTokens is PLURAL on the request, vs the singular nextPageToken on the
// response VO — the asymmetry is load-bearing (§8).
type ScheduleTaskDTO struct {
	MaxResults     string `json:"maxResults"`
	NextPageTokens string `json:"nextPageTokens"`
	NextSyncToken  int64  `json:"nextSyncToken"`
}

// ScheduleTaskAllVO is the /schedule/task/all response (ScheduleTaskAllVO.java).
type ScheduleTaskAllVO struct {
	envelope.BaseVO
	// NextPageToken is omitted when empty so the device sees "no more pages"
	// (null) rather than an empty-string cursor it may loop on.
	NextPageToken string    `json:"nextPageToken,omitempty"`
	NextSyncToken int64     `json:"nextSyncToken"`
	ScheduleTask  []SPCTask `json:"scheduleTask"`
}

// ScheduleTaskGroupDO is one task group/list (ScheduleTaskGroupDO.java).
type ScheduleTaskGroupDO struct {
	TaskListID   string `json:"taskListId"`
	UserID       int64  `json:"userId"`
	Title        string `json:"title"`
	LastModified int64  `json:"lastModified"`
	IsDeleted    string `json:"isDeleted"`
}

// ScheduleTaskGroupVO is the /schedule/group/all response
// (ScheduleTaskGroupVO.java).
type ScheduleTaskGroupVO struct {
	envelope.BaseVO
	PageToken         string                `json:"pageToken,omitempty"`
	ScheduleTaskGroup []ScheduleTaskGroupDO `json:"scheduleTaskGroup"`
}

// AddScheduleTaskGroupDTO is the group create/update request
// (AddScheduleTaskGroupDTO.java).
type AddScheduleTaskGroupDTO struct {
	TaskListID   string `json:"taskListId"`
	Title        string `json:"title"`
	LastModified int64  `json:"lastModified"`
	CreateTime   int64  `json:"createTime"`
}

// ScheduleSortDTO is the sort request (ScheduleSortDTO.java). Note lastModify
// has NO trailing 'd' (§8).
type ScheduleSortDTO struct {
	TaskListID string `json:"taskListId"`
	Title      string `json:"title"`
	LastModify int64  `json:"lastModify"`
	Content    string `json:"content"`
}

// GetScheduleSortVO is the getScheduleSort response (GetScheduleSortVO.java).
type GetScheduleSortVO struct {
	envelope.BaseVO
	TaskListID      string `json:"taskListId"`
	Title           string `json:"title"`
	LastModify      int64  `json:"lastModify"`
	Content         string `json:"content"`
	NextIndexNumber int    `json:"nextIndexNumber"`
}
