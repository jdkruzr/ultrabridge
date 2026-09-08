// Package bounded owns opt-in row transport limits and wire page construction.
// Hosts own authentication, schema validation, transactions and route activation.
package bounded

import (
	"encoding/json"
	"fmt"
)

type Limits struct {
	MaxOps          int `json:"max_ops"`
	TargetPageBytes int `json:"target_page_bytes"`
	MaxRowBytes     int `json:"max_row_bytes"`
	MaxBodyBytes    int `json:"max_body_bytes"`
}

func Defaults() Limits { return Limits{500, 4 << 20, 8 << 20, 16 << 20} }
func (l Limits) Validate() error {
	if l.MaxOps < 1 || l.MaxOps > 500 || l.MaxBodyBytes < 256 || l.MaxBodyBytes > 16<<20 ||
		l.TargetPageBytes < 1 || l.TargetPageBytes > l.MaxBodyBytes || l.MaxRowBytes < 1 || l.MaxRowBytes > 8<<20 {
		return Fail(400, "invalid_limits")
	}
	return nil
}

type Error struct {
	Status int    `json:"-"`
	Code   string `json:"code"`
	SiteID string `json:"site_id,omitempty"`
	OpSeq  int64  `json:"op_seq,omitempty"`
	Bytes  int64  `json:"encoded_bytes,omitempty"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s/%d (%d bytes)", e.Code, e.SiteID, e.OpSeq, e.Bytes)
}
func Fail(status int, code string) error { return &Error{Status: status, Code: code} }
func Oversized(site string, seq, bytes int64) error {
	return &Error{413, "oversized_op", site, seq, bytes}
}

type Request struct {
	ProtocolVersion int               `json:"protocol_version"`
	SchemaHash      string            `json:"schema_hash"`
	SiteID          string            `json:"site_id"`
	DeviceName      string            `json:"device_name,omitempty"`
	Cursor          int64             `json:"cursor"`
	Ops             []json.RawMessage `json:"ops"`
}
type Response struct {
	ProtocolVersion int               `json:"protocol_version"`
	AcceptedThrough int64             `json:"accepted_through"`
	Rejected        json.RawMessage   `json:"rejected"`
	Ops             []json.RawMessage `json:"ops"`
	Cursor          int64             `json:"cursor"`
	HasMore         bool              `json:"has_more"`
}

func (r Request) ValidateRows(l Limits) error {
	if err := l.Validate(); err != nil {
		return err
	}
	if len(r.Ops) > l.MaxOps {
		return Fail(413, "too_many_ops")
	}
	for _, raw := range r.Ops {
		if len(raw) > l.MaxRowBytes {
			var identity struct {
				SiteID string `json:"site_id"`
				OpSeq  int64  `json:"op_seq"`
			}
			_ = json.Unmarshal(raw, &identity)
			return Oversized(identity.SiteID, identity.OpSeq, int64(len(raw)))
		}
	}
	return nil
}

// Page accumulates at most a single wire page. Call Add in relay order, then
// stop at the first false. The caller's transaction must roll back on ANY error.
type Page struct {
	Response Response
	limits   Limits
	bytes    int
}

func NewPage(cursor, ack int64, rejected json.RawMessage, limits Limits) (*Page, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if rejected == nil || string(rejected) == "null" {
		rejected = json.RawMessage(`[]`)
	}
	p := &Page{Response: Response{ProtocolVersion: 1, AcceptedThrough: ack, Rejected: rejected, Ops: []json.RawMessage{}, Cursor: cursor}, limits: limits}
	b, err := json.Marshal(p.Response)
	if err != nil {
		return nil, err
	}
	if len(b) > limits.MaxBodyBytes {
		return nil, Fail(413, "response_envelope_too_large")
	}
	return p, nil
}

// CountFull lets SQL hosts perform a cheap existence check without fetching the
// 501st payload. Other hosts may pass one lookahead row to Add instead.
func (p *Page) CountFull() bool { return len(p.Response.Ops) >= p.limits.MaxOps }
func (p *Page) Add(seq int64, site string, opSeq int64, encodedBytes int64, raw json.RawMessage) (bool, error) {
	if p.CountFull() {
		p.Response.HasMore = true
		return false, nil
	}
	base := p.Response
	base.Ops = []json.RawMessage{}
	base.Cursor = seq
	base.HasMore = false
	envelope, err := json.Marshal(base)
	if err != nil {
		return false, err
	}
	if encodedBytes > int64(p.limits.MaxRowBytes) || encodedBytes+int64(len(envelope)) > int64(p.limits.MaxBodyBytes) {
		if len(p.Response.Ops) > 0 {
			p.Response.HasMore = true
			return false, nil
		}
		return false, Oversized(site, opSeq, encodedBytes)
	}
	if raw == nil || int64(len(raw)) != encodedBytes {
		return false, Fail(500, "invalid_relay_payload")
	}
	comma := 0
	if len(p.Response.Ops) > 0 {
		comma = 1
	}
	next := len(envelope) + p.bytes + comma + len(raw)
	if len(p.Response.Ops) > 0 && (next > p.limits.TargetPageBytes || next > p.limits.MaxBodyBytes) {
		p.Response.HasMore = true
		return false, nil
	}
	p.Response.Ops = append(p.Response.Ops, raw)
	p.Response.Cursor = seq
	p.bytes += comma + len(raw)
	return true, nil
}
func (p *Page) Encode() ([]byte, error) {
	b, err := json.Marshal(p.Response)
	if err != nil {
		return nil, err
	}
	if len(b) > p.limits.MaxBodyBytes {
		return nil, Fail(413, "response_body_too_large")
	}
	return b, nil
}
