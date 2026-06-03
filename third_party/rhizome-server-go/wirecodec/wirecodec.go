// Package wirecodec encodes/decodes a single column value to/from its JSON wire form per the
// column's registry.ColumnType. It mirrors the Kotlin WireCodec and MUST agree with it — pinned by
// the `wire-codec` conformance vectors. The Go server carries relayed cols as opaque json.RawMessage
// and so does not decode on the sync path; this codec backs the optional server-side mirror (which
// materializes typed columns) and proves cross-language codec agreement.
//
// Native Go types: Text→string, Int/Timestamp→int64, Real→float64, Bool→bool, ColorInt→int32
// (a signed ARGB value), Blob→[]byte. A null wire value decodes to nil and encodes back to JSON null.
package wirecodec

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/jdkruzr/rhizome/server-go/registry"
)

var jsonNull = json.RawMessage("null")

// Decode parses a wire value into the native Go representation for column type t.
func Decode(t registry.ColumnType, wire json.RawMessage) (any, error) {
	if isNull(wire) {
		return nil, nil
	}
	switch t {
	case registry.Text:
		var s string
		return s, json.Unmarshal(wire, &s)
	case registry.Int, registry.Timestamp:
		var n int64
		return n, json.Unmarshal(wire, &n)
	case registry.Real:
		var f float64
		return f, json.Unmarshal(wire, &f)
	case registry.Bool:
		var b bool
		return b, json.Unmarshal(wire, &b)
	case registry.ColorInt:
		// unsigned int64 wire color -> low 32 bits as a signed int32 (the native ARGB value).
		var u uint32
		if err := json.Unmarshal(wire, &u); err != nil {
			return nil, err
		}
		return int32(u), nil
	case registry.Blob:
		var s string
		if err := json.Unmarshal(wire, &s); err != nil {
			return nil, err
		}
		return base64.StdEncoding.DecodeString(s)
	default:
		return nil, fmt.Errorf("wirecodec: unknown column type %q", t)
	}
}

// Encode renders a native value into its JSON wire form for column type t.
func Encode(t registry.ColumnType, v any) (json.RawMessage, error) {
	if v == nil {
		return jsonNull, nil
	}
	switch t {
	case registry.Text:
		return marshal(v.(string))
	case registry.Int, registry.Timestamp:
		return marshal(v.(int64))
	case registry.Real:
		return marshal(v.(float64))
	case registry.Bool:
		return marshal(v.(bool))
	case registry.ColorInt:
		// signed ARGB int32 -> unsigned int64 on the wire.
		return marshal(uint64(uint32(v.(int32))))
	case registry.Blob:
		return marshal(base64.StdEncoding.EncodeToString(v.([]byte)))
	default:
		return nil, fmt.Errorf("wirecodec: unknown column type %q", t)
	}
}

func marshal(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	return json.RawMessage(b), err
}

func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), jsonNull)
}
