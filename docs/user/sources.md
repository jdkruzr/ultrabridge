# Sources And Sync Models

UltraBridge treats each device family as a source. Sources are configured in **Settings -> Devices** and appear as separate Files tabs. Each Files tab carries a banner stating the source's sync model; the table below uses the same labels.

## Source Types

| Source | Sync model | Authority | How Data Arrives |
| --- | --- | --- | --- |
| Supernote | Two-way sync | Shared (UB-hosted) | Built-in SPC server on `:8089`. Files, tasks, and digests sync through the Supernote protocol surface. |
| Boox | Receive-only | Device | WebDAV uploads to `/webdav/`. UltraBridge receives files; device deletes/renames do not propagate. |
| ForestNote | Live mirror (two-way) | Shared (row-level LWW) | `/sync/v1` relay. Mirrors notes, folders, text boxes, and page text. |
| reMarkable | Two-way sync | Shared (reMarkable protocol) | Device-facing sync routes on the main app. Stores documents/blobs locally, surfaces render/OCR/search state, and writes back: upload PDF/EPUB into any folder, download the original file, delete to the tablet's trash, and create/rename/move folders from the Files tab or `/api/v1/remarkable/`. Server-authored changes reach connected tablets within seconds. |

Only Supernote and Boox run a global processing worker with start/stop controls in the pipeline status bar; ForestNote re-OCR is per-notebook and reMarkable reprocessing is per-document, so those two report status only.

## Settings

- **Settings -> Devices** owns source rows, source paths, sync controls, and device registries. For ForestNote and reMarkable devices you can also assign your own name to each device — it is stored separately from whatever the device reports about itself and is never overwritten by a sync; clear it to fall back to the reported name.
- **Settings -> AI & Processing** owns OCR provider settings and source-specific OCR prompt overrides.
- **Settings -> Integrations** owns CalDAV and MCP configuration.
- **Settings -> System** owns web credentials and verbose API logging.

## Search And RAG

All indexed sources feed the same FTS5 index and optional embedding store. Source badges in Search show where each result came from:

- `SN`: Supernote
- `B`: Boox
- `FN`: ForestNote
- `RM`: reMarkable
- `DIG`: Supernote digest

## Deletes

- Supernote and ForestNote deletes are recoverable tombstones that converge through their sync models.
- Boox deletes in UltraBridge remove UltraBridge's file/catalog copy; the Boox device remains the exporter.
- reMarkable deletes from UltraBridge move the item into the tablet's own trash — recoverable from the device's trash screen — and it disappears from UltraBridge's listing and search. Non-empty folders cannot be deleted; empty them first. Documents trashed on the device are likewise invisible in UltraBridge.
