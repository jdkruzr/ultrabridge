package readercontract

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jdkruzr/rhizome/server-go/registry"
)

// WireOp deliberately retains JSON numbers. Routing through map[string]any's
// float64 decoder would lose int64 timestamps, sequence numbers and virtual geometry.
type WireOp struct {
	Table  string                     `json:"table"`
	PK     string                     `json:"pk"`
	SiteID string                     `json:"site_id"`
	OpTS   int64                      `json:"op_ts"`
	OpSeq  int64                      `json:"op_seq"`
	Cols   map[string]json.RawMessage `json:"cols"`
}
type Version struct {
	OpTS, OpSeq int64
	SiteID      string
}
type Record struct {
	ID      string
	Columns map[string]any
	Version *Version
}

var tables = Registry().ByName()
var sitePattern = regexp.MustCompile(`^[0-7][0-9A-HJKMNP-TV-Z]{25}$`)
var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (r Record) text(n string) string  { return r.Columns[n].(string) }
func (r Record) number(n string) int64 { return r.Columns[n].(int64) }
func (r Record) ink() Ink {
	var dynamics []byte
	if r.Columns["point_dynamics"] != nil {
		dynamics = r.Columns["point_dynamics"].([]byte)
	}
	return Ink{r.ID, r.text("brush_kind"), r.number("color"), r.number("pen_width_min"), r.number("pen_width_max"), r.number("brush_version"), r.number("brush_seed"), r.Columns["points"].([]byte), dynamics}
}

func textJSON(raw []byte) (string, error) {
	var s string
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '"' || !utf8.Valid(raw) || !validStringEscapes(raw) {
		return "", fmt.Errorf("invalid Unicode string")
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", err
	}
	return s, nil
}

// DecodeJSON is the lossless ingress boundary. Production does not call it yet.
func DecodeJSON(data []byte) (WireOp, Record, error) {
	var op WireOp
	if !utf8.Valid(data) {
		return op, Record{}, fmt.Errorf("invalid UTF-8")
	}
	if err := json.Unmarshal(data, &op); err != nil {
		return op, Record{}, err
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return op, Record{}, err
	}
	if len(envelope) != 6 {
		return op, Record{}, fmt.Errorf("incomplete/unknown operation fields")
	}
	for _, field := range []string{"table", "pk", "site_id", "op_ts", "op_seq", "cols"} {
		raw, exists := envelope[field]
		if !exists || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return op, Record{}, fmt.Errorf("missing operation field %s", field)
		}
	}
	for _, field := range []string{"table", "pk", "site_id"} {
		if _, err := textJSON(envelope[field]); err != nil {
			return op, Record{}, err
		}
	}
	r, err := Decode(op)
	return op, r, err
}

func Decode(op WireOp) (Record, error) {
	r := Record{op.PK, map[string]any{}, &Version{op.OpTS, op.OpSeq, op.SiteID}}
	t, ok := tables[op.Table]
	if !ok || !sitePattern.MatchString(op.SiteID) || op.OpTS < 0 || op.OpSeq <= 0 || len(op.Cols) != len(t.Columns) {
		return r, fmt.Errorf("invalid operation")
	}
	for _, c := range t.Columns {
		raw, ok := op.Cols[c.Name]
		if !ok {
			return r, fmt.Errorf("missing column %s", c.Name)
		}
		raw = bytes.TrimSpace(raw)
		if !json.Valid(raw) {
			return r, fmt.Errorf("invalid column JSON")
		}
		if bytes.Equal(raw, []byte("null")) {
			r.Columns[c.Name] = nil
			continue
		}
		switch c.Type {
		case registry.Text, registry.Blob:
			s, err := textJSON(raw)
			if err != nil {
				return r, err
			}
			if c.Type == registry.Text {
				r.Columns[c.Name] = s
				continue
			}
			if strings.ContainsAny(s, "\r\n") {
				return r, fmt.Errorf("invalid base64 whitespace")
			}
			codec := base64.StdEncoding
			if !strings.Contains(s, "=") {
				codec = base64.RawStdEncoding
			}
			b, err := codec.DecodeString(s)
			if err != nil {
				return r, err
			}
			r.Columns[c.Name] = b
		default:
			// Decimal integers only: no string coercion, fractional or exponent rounding.
			n, err := strconv.ParseInt(string(raw), 10, 64)
			if err != nil {
				return r, err
			}
			if c.Type == registry.ColorInt {
				if n < 0 || n > 0xffffffff {
					return r, fmt.Errorf("invalid wire color")
				}
				n = int64(int32(n))
			}
			r.Columns[c.Name] = n
		}
	}
	return r, Validate(op.Table, r)
}

func versioned(s string) (map[string]json.RawMessage, int64, error) {
	var o map[string]json.RawMessage
	if !utf8.ValidString(s) || len(s) > 1024*1024 {
		return nil, 0, fmt.Errorf("invalid JSON payload")
	}
	if err := json.Unmarshal([]byte(s), &o); err != nil {
		return nil, 0, err
	}
	v, err := jsonInteger(o["version"])
	if err != nil || v <= 0 || v > 2147483647 {
		return nil, 0, fmt.Errorf("versioned JSON required")
	}
	return o, v, nil
}
func jsonInteger(raw []byte) (int64, error) {
	return strconv.ParseInt(string(bytes.TrimSpace(raw)), 10, 64)
}
func anchor(s string) bool {
	o, v, err := versioned(s)
	if err != nil {
		return false
	}
	if v != 1 {
		return true
	}
	section, e1 := jsonInteger(o["section"])
	start, e2 := jsonInteger(o["start"])
	end, e3 := jsonInteger(o["end"])
	if e1 != nil || e2 != nil || e3 != nil || section < 0 || start < 0 || end < start {
		return false
	}
	for _, k := range []string{"quote", "prefix", "suffix"} {
		raw := bytes.TrimSpace(o[k])
		if len(raw) == 0 || raw[0] != '"' {
			return false
		}
	}
	return true
}
func property(name, s string) bool {
	if name != "anchor" && name != "height" && name != "highlight_present" {
		return false
	}
	o, v, err := versioned(s)
	if err != nil {
		return false
	}
	if v != 1 {
		return true
	}
	switch name {
	case "anchor":
		return anchor(s)
	case "height":
		n, err := jsonInteger(o["height"])
		return err == nil && n >= 0
	default:
		p := string(bytes.TrimSpace(o["present"]))
		return p == "true" || p == "false"
	}
}

// Validate checks complete typed storage rows (including signed ARGB), not dependencies.
func Validate(table string, r Record) error {
	return validateShape(table, r, true)
}

// Reduction inspects ink only after cancellation/erase masking, like Kotlin.
// Unsupported bytes in an invisible stroke must not poison the surviving annotation.
func validateShape(table string, r Record, inspectInk bool) error {
	t, ok := tables[table]
	if !ok || !validID(r.ID) || len(r.Columns) != len(t.Columns) {
		return fmt.Errorf("invalid row identity/columns")
	}
	for _, c := range t.Columns {
		value, exists := r.Columns[c.Name]
		if !exists || value == nil && !c.Nullable {
			return fmt.Errorf("missing %s", c.Name)
		}
		if value == nil {
			continue
		}
		switch c.Type {
		case registry.Text:
			s, ok := value.(string)
			if !ok || !utf8.ValidString(s) || len(s) > 1024*1024 {
				return fmt.Errorf("invalid text %s", c.Name)
			}
		case registry.Blob:
			if _, ok := value.([]byte); !ok {
				return fmt.Errorf("invalid blob %s", c.Name)
			}
		default:
			if _, ok := value.(int64); !ok {
				return fmt.Errorf("invalid integer %s", c.Name)
			}
		}
	}
	id := func(k string) bool { return validID(r.text(k)) }
	bit := func(k string) bool { return r.number(k) == 0 || r.number(k) == 1 }
	jsonOK := func(k string) bool { _, _, err := versioned(r.text(k)); return err == nil }
	composite := func(keys ...string) bool {
		p := make([]string, len(keys))
		for i, k := range keys {
			p[i] = r.text(k)
		}
		s, err := CompositeID(p...)
		return err == nil && s == r.ID
	}
	valid := false
	switch table {
	case "reader_book":
		valid = digestPattern.MatchString(r.ID) && r.text("asset_id") == r.ID && r.number("byte_length") >= 0 && (r.text("media_type") == "application/epub+zip" || r.text("media_type") == "application/x-mobipocket-ebook") && jsonOK("metadata_json")
	case "reader_book_title":
		valid = !kotlinBlank(r.text("title")) && utf16Length(r.text("title")) <= 4096
	case "reader_annotation":
		valid = digestPattern.MatchString(r.text("book_id")) && r.number("canvas_width") > 0 && r.number("initial_height") >= 0 && id("creator_session_id") && anchor(r.text("initial_anchor_json"))
	case "reader_edit_session":
		owner, _ := r.Columns["owner_site"].(string)
		valid = id("annotation_id") && (r.text("kind") == "interactive" && sitePattern.MatchString(owner) && (r.text("state") == "open" || r.text("state") == "finished" || r.text("state") == "cancelled") || r.text("kind") == "import" && r.Columns["owner_site"] == nil && r.text("state") == "finished")
	case "reader_stroke":
		valid = id("annotation_id") && id("session_id") && r.number("paint_order") > 0 && (r.text("paint_site") == "import" || sitePattern.MatchString(r.text("paint_site"))) && r.number("color") >= -2147483648 && r.number("color") <= 2147483647
		if inspectInk {
			_, err := r.ink().Bottom()
			valid = valid && err == nil
		}
	case "reader_erase_claim":
		valid = id("session_id") && id("stroke_id") && bit("active") && composite("session_id", "stroke_id")
	case "reader_annotation_value":
		valid = id("session_id") && composite("session_id", "property") && property(r.text("property"), r.text("value_json"))
	case "reader_position":
		valid = digestPattern.MatchString(r.text("book_id")) && sitePattern.MatchString(r.text("site_id")) && jsonOK("locator_json") && composite("book_id", "site_id")
	case "reader_recognition":
		valid = id("annotation_id") && id("producer_id") && digestPattern.MatchString(r.text("input_hash")) && composite("annotation_id", "producer_id") && !kotlinBlank(r.text("engine")) && (r.text("status") == "ready" || r.text("status") == "failed" || r.text("status") == "unavailable")
	case "content_anchor":
		valid = jsonOK("selector_json")
	case "content_reference":
		valid = id("source_anchor_id") && id("target_anchor_id")
	default:
		valid = strings.HasSuffix(table, "_lifecycle") && bit("deleted") && r.number("changed_at") >= 0
	}
	if !valid {
		return fmt.Errorf("invalid %s shape", table)
	}
	return nil
}
