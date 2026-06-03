package forestnote

// Config is the ForestNote source's settings, parsed from sources.config_json.
// Unlike Boox/Supernote, ForestNote has no filesystem root — its notes arrive
// over the /sync/v1 device-sync protocol and live in the syncstore mirror. The
// relay batch size is the only hot-path knob; the rest gate relay-log compaction.
type Config struct {
	BatchLimit int `json:"batch_limit"` // 0 → defaultBatchLimit

	// Compaction enables periodic reclamation of the sync_ops relay log (collapse superseded
	// full-row snapshots + purge tombstones every device has pulled past). OFF by default: the
	// log grows by one snapshot per edit, but the first post-cutover deploy runs WITHOUT sweeping
	// so the operator can inspect/back up the log before any destructive pass. Set "compaction":
	// true in the source config_json once validated.
	Compaction bool `json:"compaction"`
	// CompactionIntervalSec is how often the sweep runs when enabled (0 → defaultCompactionIntervalSec).
	CompactionIntervalSec int `json:"compaction_interval_sec"`
	// CompactionStaleHorizonSec evicts a device unseen for longer than this from the watermark min
	// (0 → defaultStaleHorizonSec). Larger = more conservative: a tombstone becomes reclaimable only
	// after a longer silence, so a device that syncs infrequently is not prematurely evicted.
	CompactionStaleHorizonSec int `json:"compaction_stale_horizon_sec"`
}

// defaultBatchLimit matches syncsvc's own default when unset.
const defaultBatchLimit = 500

// Compaction scheduling defaults (used when the corresponding Config knob is 0). Low-frequency by
// design: the relay log is not latency-sensitive and a long stale horizon avoids evicting a device
// that simply has not synced in a while.
const (
	defaultCompactionIntervalSec = 6 * 60 * 60       // 6 hours
	defaultStaleHorizonSec       = 30 * 24 * 60 * 60 // 30 days
)
