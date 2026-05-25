# Source Abstraction Package

Last verified: 2026-04-10

## Purpose

Platform-neutral source abstraction layer. Each note-ingestion device (Supernote, Boox) is a "source" with its own lifecycle, config, and processing pipeline. Sources are stored as database rows and instantiated at startup via a factory registry.

## Contracts

### Source Interface

```go
type Source interface {
    Type() string
    Name() string
    Start(ctx context.Context) error
    Stop()
}
```

### SourceRow

Database model for the `sources` table:
- `ID`, `Type` ("supernote" | "boox"), `Name`, `Enabled`, `ConfigJSON`, `CreatedAt`, `UpdatedAt`

### Registry

- `NewRegistry()` -- creates empty registry
- `Register(typeName, factory)` -- registers a factory for a source type
- `Create(db, row, deps)` -- looks up factory by `row.Type`, calls it, returns Source

### CRUD Functions (package-level)

- `ListSources(ctx, db)` -- all source rows ordered by ID
- `ListEnabledSources(ctx, db)` -- only enabled rows
- `GetSource(ctx, db, id)` -- single row by ID
- `AddSource(ctx, db, row)` -- insert, returns assigned ID
- `UpdateSource(ctx, db, row)` -- update name/enabled/config_json
- `RemoveSource(ctx, db, id)` -- hard delete by ID

### SharedDeps

Bundles infrastructure shared across all source adapters: `Indexer`, `Embedder`, `EmbedModel`, `EmbedStore`, `OCRClient`, `OCRMaxFileMB`, `Logger`.

### Sentinel Errors

- `ErrSourceNotFound` -- returned by Get/Update/Remove when row missing
- `ErrUnknownType` -- returned by Registry.Create for unregistered types

## Dependencies

- **Uses**: `database/sql`, `internal/processor` (Indexer, OCRClient), `internal/rag` (Embedder, EmbedStore)
- **Used by**: `cmd/ultrabridge` (registry setup, source lifecycle), `internal/web` (CRUD API), `internal/source/supernote`, `internal/source/boox`

## Sub-packages

### supernote/
Supernote source adapter. Parses `Config` from `config_json` (NotesPath, BackupPath, JIIXEnabled). Creates notestore, processor, and pipeline internally on `Start()` from SharedDeps alone (the legacy mariaDB/Engine.IO deps were removed with the SPC client 2026-05-25).

### boox/
Boox source adapter. Parses `Config` from `config_json` (NotesPath, ImportPath). Creates booxpipeline.Processor internally on `Start()`. Accepts Boox-specific deps (ContentDeleter, OnTodosFound) beyond SharedDeps.

## Key Decisions

- Factory closures in main.go capture extra type-specific deps (e.g. BooxDeps) and present a uniform Factory signature to the registry
- Sources table uses millisecond UTC timestamps for created_at/updated_at
- Enabled stored as integer (0/1) in SQLite, mapped to bool in Go

## Invariants

- Each source row has a unique ID (autoincrement)
- ConfigJSON must be valid JSON (validated at API layer, parsed at adapter layer)
- Source.Start() is idempotent within a process lifecycle; Stop() releases all resources
