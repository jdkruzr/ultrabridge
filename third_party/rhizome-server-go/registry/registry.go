// Package registry is the single source of truth for a synced data shape. The merge knownCols,
// the schema hash, server-side validation, and the optional mirror's dynamic SQL are all derived
// from it. This Go registry mirrors the Kotlin client Registry; both MUST produce the same
// canonical schema string and the same schema hash (spec/schema-registry.md).
package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// ColumnType is the wire/SQLite type of a synced column. The string values match the Kotlin
// ColumnType enum names so registries and vectors read identically across languages.
type ColumnType string

const (
	Text      ColumnType = "Text"
	Int       ColumnType = "Int"
	Real      ColumnType = "Real"
	Bool      ColumnType = "Bool"
	Timestamp ColumnType = "Timestamp"
	Blob      ColumnType = "Blob"
	ColorInt  ColumnType = "ColorInt"
)

// Column is a synced non-PK column. The PK and tombstone column are named on Table, not here.
type Column struct {
	Name     string
	Type     ColumnType
	Nullable bool
}

// Table is one synced table. Columns are the NON-pk columns that travel in an op's cols map.
// Tombstone names the nullable column whose non-null value marks a soft delete. A
// ServerAuthoredOnly table is applied by clients but never captured by them.
type Table struct {
	Name               string
	PK                 string
	Tombstone          string
	Columns            []Column
	ServerAuthoredOnly bool
}

// Registry is an ordered set of table descriptors — everything else is derived from it.
type Registry struct {
	Tables []Table
}

// ByName indexes the tables by name.
func (r Registry) ByName() map[string]Table {
	out := make(map[string]Table, len(r.Tables))
	for _, t := range r.Tables {
		out[t.Name] = t
	}
	return out
}

// KnownCols maps each table to its column names sorted alphabetically — the basis for Normalize
// and the schema hash.
func (r Registry) KnownCols() map[string][]string {
	out := make(map[string][]string, len(r.Tables))
	for _, t := range r.Tables {
		out[t.Name] = sortedColNames(t)
	}
	return out
}

// Canonical builds the deterministic schema string: tables alphabetical; within each, columns
// alphabetical; "table:col,col;table:...". Byte-identical to the Kotlin Registry.canonical().
func (r Registry) Canonical() string {
	tables := append([]Table(nil), r.Tables...)
	sort.Slice(tables, func(i, j int) bool { return tables[i].Name < tables[j].Name })
	parts := make([]string, len(tables))
	for i, t := range tables {
		parts[i] = t.Name + ":" + strings.Join(sortedColNames(t), ",")
	}
	return strings.Join(parts, ";")
}

// SchemaHash is the lowercase hex SHA-256 of Canonical — the schema-hash gate (spec/protocol.md).
func (r Registry) SchemaHash() string {
	sum := sha256.Sum256([]byte(r.Canonical()))
	return hex.EncodeToString(sum[:])
}

func sortedColNames(t Table) []string {
	names := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		names[i] = c.Name
	}
	sort.Strings(names)
	return names
}
