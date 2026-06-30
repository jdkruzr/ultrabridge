# MCP Auth

Last verified: 2026-04-09

## Purpose
Bearer token management for MCP server authentication. Stores SHA-256 hashed tokens in shared SQLite notedb. Provides CRUD operations and validation for token-based auth.

## Contracts
- **Exposes**: `Migrate(ctx, db)`, `CreateToken(ctx, db, label)`, `ValidateToken(ctx, db, rawToken)`, `ListTokens(ctx, db)`, `RevokeToken(ctx, db, tokenHash)`
- **Guarantees**: Raw tokens never stored -- only SHA-256 hash persisted. `Migrate` is idempotent (CREATE TABLE IF NOT EXISTS). `RevokeToken` is idempotent. `ValidateToken` updates `last_used` timestamp on success.
- **Expects**: Opened `*sql.DB` (shared notedb). `Migrate` must be called before other functions (called at ultrabridge startup).

## Dependencies
- **Uses**: `database/sql`, `crypto/sha256`, `crypto/rand`, `encoding/base64`
- **Used by**: `cmd/ultrabridge` (migration at startup and `/mcp` auth), `internal/web` (create/revoke UI)
- **Boundary**: Stateless functions operating on shared DB. No internal state, no goroutines.

## Key Decisions
- SHA-256 hashing (not bcrypt): tokens are high-entropy random bytes, so fast hashing is appropriate and enables efficient DB lookups
- Token format: 32 random bytes, base64url-encoded (no padding)
- Hash as primary key: `token_hash` TEXT is the PK, used as identifier for revocation
- Schema owned here, not in notedb: `mcpauth.Migrate` creates `mcp_tokens` table separately from notedb's migrations

## Invariants
- `token_hash` is PRIMARY KEY (SHA-256 hex of raw token)
- Raw token returned only from `CreateToken`, never retrievable afterward
- `ErrInvalidToken` sentinel error for missing/revoked tokens
- Timestamps: millisecond UTC unix, 0 = never used

## Key Files
- `mcpauth.go` -- All types, migration, and CRUD functions
- `mcpauth_test.go` -- Unit tests with in-memory SQLite
