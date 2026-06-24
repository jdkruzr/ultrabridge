package remarkable

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultHWRHost = "https://cloud.myscript.com"
	hwrAPIPath     = "/api/v4.0/iink/batch"
	hwrJIIX        = "application/vnd.myscript.jiix"
)

var errHWRNotConfigured = errors.New("remarkable hwr not configured")

type hwrClient struct {
	cfg        Config
	httpClient *http.Client
}

func newHWRClient(cfg Config) *hwrClient {
	return &hwrClient{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *hwrClient) Recognize(ctx context.Context, body []byte) ([]byte, error) {
	key := strings.TrimSpace(c.cfg.HWRApplicationKey)
	hmacSecret := strings.TrimSpace(c.cfg.HWRHMAC)
	if key == "" || hmacSecret == "" {
		return nil, errHWRNotConfigured
	}

	data := body
	if override := strings.TrimSpace(c.cfg.HWRLangOverride); override != "" {
		modified, err := overrideHWRLanguage(body, override)
		if err != nil {
			return nil, err
		}
		data = modified
	}

	mac := hmac.New(sha512.New, []byte(key+hmacSecret))
	_, _ = mac.Write(data)

	host := strings.TrimRight(strings.TrimSpace(c.cfg.HWRHost), "/")
	if host == "" {
		host = defaultHWRHost
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+hwrAPIPath, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", hwrJIIX)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("applicationKey", key)
	req.Header.Set("hmac", hex.EncodeToString(mac.Sum(nil)))

	httpClient := c.httpClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	respBody, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("myscript status %d", res.StatusCode)
	}
	return respBody, nil
}

func overrideHWRLanguage(body []byte, lang string) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse hwr json: %w", err)
	}
	cfg, ok := payload["configuration"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("configuration schema missing in hwr json")
	}
	cfg["lang"] = lang
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal hwr json: %w", err)
	}
	return out, nil
}
