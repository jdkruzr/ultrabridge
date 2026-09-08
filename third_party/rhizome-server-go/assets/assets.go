// Package assets implements the opt-in immutable binary channel. It does not
// register routes, advertise capabilities, or change row-sync acknowledgements.
package assets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
)

const ChunkBytes = 262144
const PageEntries = 256

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type Descriptor struct {
	ID         string `json:"asset_id"`
	ByteLength int64  `json:"byte_length,string"`
	ChunkBytes int    `json:"chunk_bytes"`
}

func ValidID(id string) bool { return digestPattern.MatchString(id) }
func Digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func (d Descriptor) Validate() error {
	if !ValidID(d.ID) || d.ByteLength < 0 || d.ChunkBytes != ChunkBytes {
		return Fail(400, "invalid_descriptor")
	}
	return nil
}

// Division before addition avoids overflow at MaxInt64.
func (d Descriptor) ChunkCount() int64 {
	n := d.ByteLength / ChunkBytes
	if d.ByteLength%ChunkBytes != 0 {
		n++
	}
	return n
}

func (d Descriptor) ChunkLength(index int64) (int, error) {
	if index < 0 || index >= d.ChunkCount() {
		return 0, Fail(400, "invalid_index")
	}
	remaining := d.ByteLength - index*ChunkBytes
	if remaining < ChunkBytes {
		return int(remaining), nil
	}
	return ChunkBytes, nil
}

type Info struct {
	Descriptor
	State string `json:"state"`
}
type Entry struct {
	Index      int64   `json:"index"`
	SHA256     *string `json:"sha256"`
	ByteLength int     `json:"byte_length"`
}
type Page struct {
	Entries   []Entry `json:"entries"`
	NextStart *int64  `json:"next_start"`
}
type Chunk struct {
	Bytes  []byte
	SHA256 string
}

type Error struct {
	Status int
	Code   string
}

func (e *Error) Error() string           { return fmt.Sprintf("asset: %s (%d)", e.Code, e.Status) }
func Fail(status int, code string) error { return &Error{status, code} }

type Store interface {
	Describe(context.Context, string) (Info, error)
	Stage(context.Context, Descriptor) (Info, bool, error)
	ListChunks(context.Context, string, int64, int) (Page, error)
	ReadChunk(context.Context, string, int64) (Chunk, error)
	WriteChunk(context.Context, string, int64, []byte, string) error
	Complete(context.Context, string) (Info, error)
	ResetInvalid(context.Context, string) error
}
