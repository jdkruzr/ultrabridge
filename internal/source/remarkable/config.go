package remarkable

// Config holds reMarkable source-specific settings parsed from
// sources.config_json.
type Config struct {
	DataPath      string `json:"data_path"`
	PairingCode   string `json:"pairing_code"`
	DeviceAccount string `json:"device_account"`
}
