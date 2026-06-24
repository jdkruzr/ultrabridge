package remarkable

// Config holds reMarkable source-specific settings parsed from
// sources.config_json.
type Config struct {
	DataPath          string `json:"data_path"`
	PairingCode       string `json:"pairing_code"`
	DeviceAccount     string `json:"device_account"`
	HWRApplicationKey string `json:"hwr_application_key"`
	HWRHMAC           string `json:"hwr_hmac"`
	HWRLangOverride   string `json:"hwr_lang_override"`
	HWRHost           string `json:"hwr_host"`
}
