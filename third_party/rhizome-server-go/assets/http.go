package assets

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// NewHandler serves only /sync/assets/v1/. The host MUST wrap it in the same
// authorization/account boundary as row sync. It intentionally does not expose
// /sync/capabilities: bounded rows and registry acceptance are separate gates.
func NewHandler(store Store) http.Handler { return &handler{store} }

type handler struct{ store Store }

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := h.serve(w, r); err != nil {
		status, code := 503, "storage_unavailable"
		var ae *Error
		if errors.As(err, &ae) {
			status, code = ae.Status, ae.Code
		} else {
			// SQLite driver errors expose primary/extended codes through Code().
			var se interface{ Code() int }
			if errors.As(err, &se) && se.Code()&255 == 13 {
				status, code = 507, "storage_full"
			}
		}
		writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": code}})
	}
}

func (h *handler) serve(w http.ResponseWriter, r *http.Request) error {
	const prefix = "/sync/assets/v1/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		return Fail(404, "not_found")
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
	id := parts[0]
	if !ValidID(id) {
		return Fail(400, "invalid_asset_id")
	}
	ctx := r.Context()
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			i, err := h.store.Describe(ctx, id)
			if err != nil {
				return err
			}
			writeJSON(w, 200, i)
			return nil
		case http.MethodPut:
			var wire struct {
				ID         string `json:"asset_id"`
				Length     string `json:"byte_length"`
				ChunkBytes int    `json:"chunk_bytes"`
			}
			dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&wire); err != nil {
				return decodeError(err)
			}
			var extra any
			if err := dec.Decode(&extra); err != io.EOF {
				if err != nil {
					return decodeError(err)
				}
				return Fail(400, "invalid_json")
			}
			length, err := decimal(wire.Length)
			if err != nil {
				return err
			}
			if wire.ID != id {
				return Fail(400, "invalid_descriptor")
			}
			i, created, err := h.store.Stage(ctx, Descriptor{id, length, wire.ChunkBytes})
			if err != nil {
				return err
			}
			status := 200
			if created {
				status = 201
			}
			writeJSON(w, status, i)
			return nil
		default:
			return Fail(405, "method_not_allowed")
		}
	}
	if len(parts) == 2 {
		switch parts[1] {
		case "chunks":
			if r.Method != http.MethodGet {
				return Fail(405, "method_not_allowed")
			}
			start, err := decimal(r.URL.Query().Get("start"))
			if err != nil {
				return err
			}
			limit, err := decimal(r.URL.Query().Get("limit"))
			if err != nil || limit < 1 || limit > PageEntries {
				return Fail(400, "invalid_page")
			}
			p, err := h.store.ListChunks(ctx, id, start, int(limit))
			if err != nil {
				return err
			}
			writeJSON(w, 200, p)
			return nil
		case "complete", "reset-invalid":
			if r.Method != http.MethodPost {
				return Fail(405, "method_not_allowed")
			}
			if parts[1] == "reset-invalid" {
				if err := h.store.ResetInvalid(ctx, id); err != nil {
					return err
				}
				w.WriteHeader(204)
				return nil
			}
			i, err := h.store.Complete(ctx, id)
			if err != nil {
				return err
			}
			writeJSON(w, 200, i)
			return nil
		}
	}
	if len(parts) == 3 && parts[1] == "chunks" {
		index, err := decimal(parts[2])
		if err != nil {
			return err
		}
		switch r.Method {
		case http.MethodGet:
			c, err := h.store.ReadChunk(ctx, id, index)
			if err != nil {
				return err
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", strconv.Itoa(len(c.Bytes)))
			w.Header().Set("X-Rhizome-Chunk-SHA256", c.SHA256)
			w.WriteHeader(200)
			_, _ = w.Write(c.Bytes)
			return nil
		case http.MethodPut:
			if r.ContentLength > ChunkBytes {
				return Fail(413, "chunk_too_large")
			}
			if r.ContentLength < 0 {
				return Fail(400, "content_length_required")
			}
			if r.Header.Get("Content-Type") != "application/octet-stream" {
				return Fail(400, "invalid_content_type")
			}
			i, err := h.store.Describe(ctx, id)
			if err != nil {
				return err
			}
			length, err := i.ChunkLength(index)
			if err != nil {
				return err
			}
			if r.ContentLength != int64(length) {
				return Fail(400, "invalid_chunk_length")
			}
			b, err := io.ReadAll(io.LimitReader(r.Body, int64(length)+1))
			if err != nil || len(b) != length {
				return Fail(400, "invalid_chunk_length")
			}
			if err := h.store.WriteChunk(ctx, id, index, b, r.Header.Get("X-Rhizome-Chunk-SHA256")); err != nil {
				return err
			}
			w.WriteHeader(204)
			return nil
		default:
			return Fail(405, "method_not_allowed")
		}
	}
	return Fail(404, "not_found")
}

func decimal(s string) (int64, error) {
	if s == "" || (len(s) > 1 && s[0] == '0') {
		return 0, Fail(400, "invalid_integer")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, Fail(400, "invalid_integer")
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, Fail(400, "invalid_integer")
	}
	return v, nil
}
func decodeError(err error) error {
	var max *http.MaxBytesError
	if errors.As(err, &max) {
		return Fail(413, "body_too_large")
	}
	return Fail(400, "invalid_json")
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
