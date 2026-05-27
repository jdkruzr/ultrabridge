package forestnote

// Config is the ForestNote source's settings, parsed from sources.config_json.
// Unlike Boox/Supernote, ForestNote has no filesystem root — its notes arrive
// over the /sync/v1 device-sync protocol and live in the syncstore mirror. The
// only knob is the relay batch size (ops returned per pull response).
type Config struct {
	BatchLimit int `json:"batch_limit"` // 0 → defaultBatchLimit
}

// defaultBatchLimit matches syncsvc's own default when unset.
const defaultBatchLimit = 500
