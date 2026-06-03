// Package schemaevo is the §I.9 schema-evolution reconcile rule, mirroring the Kotlin
// SchemaEvolution. It is the canonical reference for a multi-master replica's response to a
// synced-schema-hash change; a host's local store applies the same rule against its own cursor.
package schemaevo

// Reconcile decides the §I.9 outcome for a replica whose stored synced-schema hash is storedHash
// and whose current schema hash is currentHash. When they differ — a schema-generation change, or
// a never-reconciled marker (empty string) — the cursor resets to 0 so the next session re-pulls
// the entire relay log once and re-materializes every row under the new schema; otherwise the
// cursor is unchanged. The stored hash always advances to currentHash, so the reset fires AT MOST
// ONCE per generation (the re-pull is idempotent under LWW). An empty storedHash (a device that has
// never reconciled — e.g. the first launch after a cutover) is treated as a change.
func Reconcile(storedHash, currentHash string, cursor int64) (newCursor int64, newStored string) {
	if storedHash == currentHash {
		return cursor, currentHash
	}
	return 0, currentHash
}
