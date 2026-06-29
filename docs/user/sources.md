# Sources And Sync Models

UltraBridge treats each device family as a source. Sources are configured in **Settings -> Devices** and appear as separate Files tabs.

## Source Types

| Source | Direction | How Data Arrives | Notes |
| --- | --- | --- | --- |
| Supernote | Two-way | Built-in SPC server on `:8089` | Files, tasks, and digests sync through the Supernote protocol surface. |
| Boox | One-way in | WebDAV uploads to `/webdav/` | UltraBridge receives files; device deletes/renames do not propagate. |
| ForestNote | Two-way | `/sync/v1` relay | Mirrors notes, folders, text boxes, page text, and task provenance. |
| reMarkable | Shared protocol | Device-facing sync routes on the main app | Stores documents/blobs locally and surfaces render/OCR/search state. |

## Settings

- **Settings -> Devices** owns source rows, source paths, sync controls, and device registries.
- **Settings -> AI & Processing** owns OCR provider settings and source-specific OCR prompt overrides.
- **Settings -> Integrations** owns CalDAV and MCP configuration.
- **Settings -> System** owns auth and logging.

## Search And RAG

All indexed sources feed the same FTS5 index and optional embedding store. Source badges in Search show where each result came from:

- `SN`: Supernote
- `B`: Boox
- `FN`: ForestNote
- `RM`: reMarkable

## Deletes

- Supernote and ForestNote deletes are recoverable tombstones that converge through their sync models.
- Boox deletes in UltraBridge remove UltraBridge's file/catalog copy; the Boox device remains the exporter.
- reMarkable deletes follow the hosted reMarkable protocol surface.
