package bounded

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
)

const Header = "X-Rhizome-Bounded-Rows"

func ReadRequest(w http.ResponseWriter, r *http.Request, defaults Limits) (Request, Limits, error) {
	var req Request
	if err := defaults.Validate(); err != nil {
		return req, defaults, err
	}
	if r.Method != "POST" {
		return req, defaults, Fail(405, "method_not_allowed")
	}
	if r.Header.Get(Header) != "1" {
		return req, defaults, Fail(400, "invalid_bounded_rows_version")
	}
	l := defaults
	for _, h := range []struct {
		name   string
		target *int
	}{
		{"X-Rhizome-Max-Response-Bytes", &l.MaxBodyBytes},
		{"X-Rhizome-Max-Row-Bytes", &l.MaxRowBytes},
	} {
		if values, exists := r.Header[http.CanonicalHeaderKey(h.name)]; exists {
			if len(values) != 1 {
				return req, l, Fail(400, "invalid_limits")
			}
			v := values[0]
			for _, c := range v {
				if c < '0' || c > '9' {
					return req, l, Fail(400, "invalid_limits")
				}
			}
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n <= 0 {
				return req, l, Fail(400, "invalid_limits")
			}
			if n < int64(*h.target) {
				*h.target = int(n)
			}
		}
	}
	if l.TargetPageBytes > l.MaxBodyBytes {
		l.TargetPageBytes = l.MaxBodyBytes
	}
	if err := l.Validate(); err != nil {
		return req, l, err
	}
	// Receive request limit is the server's offer, not the client's smaller
	// response limit. The raw body is bounded before decoding any row objects.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(defaults.MaxBodyBytes)))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return req, l, Fail(413, "request_body_too_large")
		}
		return req, l, Fail(400, "invalid_body")
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return req, l, Fail(400, "invalid_json")
	}
	if err := req.ValidateRows(defaults); err != nil {
		return req, l, err
	}
	return req, l, nil
}

func WriteError(w http.ResponseWriter, err error) {
	e := &Error{Status: 503, Code: "sync_unavailable"}
	var known *Error
	if errors.As(err, &known) {
		e = known
	}
	// Identity can originate in a rejected request; bound even this error body.
	copy := *e
	if len(copy.SiteID) > 128 {
		copy.SiteID = copy.SiteID[:128]
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(copy.Status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": copy})
}

type AssetLimits struct {
	ChunkBytes          int `json:"chunk_bytes"`
	ManifestPageEntries int `json:"manifest_page_entries"`
}
type Capabilities struct {
	Version              int          `json:"capabilities_version"`
	Features             []string     `json:"features"`
	AcceptedSchemaHashes []string     `json:"accepted_schema_hashes"`
	Rows                 Limits       `json:"rows"`
	Assets               *AssetLimits `json:"assets,omitempty"`
}

// CapabilityHandler must be mounted behind the host's sync authorization and
// ONLY after its bounded backend/routes are ready. Hashes come from the host's
// actual registry/grace set; no reader hash is invented by this package.
func CapabilityHandler(limits Limits, hashes []string, assetsReady bool) (http.Handler, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	c := Capabilities{1, []string{"bounded-rows-v1"}, append([]string{}, hashes...), limits, nil}
	if assetsReady {
		c.Features = append(c.Features, "assets-v1")
		c.Assets = &AssetLimits{262144, 256}
	}
	body, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	if len(body) > 65536 {
		return nil, Fail(400, "capabilities_too_large")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			WriteError(w, Fail(405, "method_not_allowed"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}), nil
}
