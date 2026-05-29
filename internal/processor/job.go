package processor

import "time"

// Job status constants.
const (
	StatusPending    = "pending"
	StatusInProgress = "in_progress"
	StatusDone       = "done"
	StatusFailed     = "failed"
	StatusSkipped    = "skipped"
)

// SkipReason constants.
const (
	SkipReasonManual    = "manual"
	SkipReasonSizeLimit = "size_limit"
)

// Job is a processing record for a single .note file.
type Job struct {
	ID           int64
	NotePath     string
	Status       string
	SkipReason   string
	OCRSource    string
	APIModel     string
	Attempts     int
	LastError    string
	QueuedAt     time.Time
	StartedAt    time.Time
	FinishedAt   time.Time
	RequeueAfter *time.Time // nil = no delay
}

// ProcessorStatus is a snapshot of queue state.
type ProcessorStatus struct {
	Running  bool
	Pending  int
	InFlight int
	Done     int
	Failed   int
}
