# Boox Note Parser

Last verified: 2026-07-27 (Empty-notebook handling: `Open` now returns the exported sentinel `ErrEmptyNotebook` when note_info declares pages but the archive carries no data for ANY of them — a notebook created on the device and never drawn in. A PARTIALLY missing archive is a different, non-sentinel error and stays loud. `booxpipeline` skips the former and fails the latter.)

## Purpose
Parses Boox .note files (ZIP archives containing protobuf metadata, nested shape ZIPs, and binary point files) into an in-memory model of pages, shapes, and stroke points.

## Contracts
- **Exposes**: `Open(r io.ReaderAt, size int64) (*Note, error)`, `Note` (NoteID, Title, Pages), `Page` (PageID, Width, Height, Shapes), `Shape` (UniqueID, ShapeType, Color, FillColor, Thickness, ZOrder, BoundingRect, MatrixValues, Text, Points), `TinyPoint` (X, Y, Size, Pressure, Time), `Rect`
- **Guarantees**: Produces fully correlated model -- shapes have their point data attached via UUID matching. Pages ordered by orderIndex from note_info. Shapes sorted by ZOrder. Blank pages (no shape ZIP) return empty shape slice, not error.
- **Empty vs truncated**: in the pageNames fallback branch a missing VirtualPage
  entry is not fatal — the page falls through to the backfill loop, which can
  rebuild it from shape/point data. After backfill, pages still unresolved are
  judged: ALL declared pages unresolved (and no pages materialized at all) →
  `ErrEmptyNotebook`, which callers should treat as "nothing to process";
  SOME unresolved while others carry real content → a plain error naming the
  count, because that is a truncated/corrupt archive and must not be silently
  skipped. `booxpipeline.Processor.handleJobError` maps the sentinel to a
  `skipped` job with reason "empty notebook (no page data)".
- **Expects**: Valid Boox .note ZIP with standard directory layout: `{noteId}/note/pb/note_info`, `{noteId}/virtual/page/pb/{pageId}`, `{noteId}/shape/{pageId}#...zip`, `{noteId}/point/{pageId}/{shapeId}`

## Dependencies
- **Uses**: `archive/zip`, `google.golang.org/protobuf/proto`, `internal/booxnote/proto` (generated protobuf types)
- **Used by**: `booxpipeline` (processor parses .note before rendering), `booxnote/testutil` (test builder)
- **Boundary**: Pure parser -- no filesystem I/O, no rendering, no database. Caller provides `io.ReaderAt`.

## Key Types
- **Note**: Top-level container with NoteID (ZIP root directory name), Title (from note_info protobuf), and ordered Pages.
- **Page**: Single page with dimensions parsed from VirtualPage protobuf (`pageSize` field, supports "WxH" and JSON formats).
- **Shape**: Stroke or geometric shape. ShapeType determines rendering behavior (scribble vs geometric in booxrender).
- **TinyPoint**: 16-byte binary point sample: X/Y position (float32), Size/Pressure (int16), Time (uint32). Big-endian encoding.

## ZIP Structure
```
{noteId}/
  note/pb/note_info          -- NoteInfo protobuf (title, pageNameList JSON)
  virtual/page/pb/{pageId}   -- VirtualPage protobuf (pageSize, pageId)
  shape/{pageId}#...zip      -- Nested ZIP containing ShapeInfoProtoList protobuf
  point/{pageId}/{shapeId}   -- Binary point file (V1 format: header + xref + point blocks)
```

## Point File Format (V1)
- 76-byte header
- Point data blocks: 4-byte attrs + N x 16-byte TinyPoints (big-endian)
- Xref table at end: 44-byte entries (36-byte UUID + 4-byte offset + 4-byte length)
- Last 4 bytes: xref table offset (big-endian uint32)

## Gotchas
- Shape ZIP is nested: outer ZIP entry contains another ZIP with the protobuf
- BoundingRect and MatrixValues are JSON strings in the protobuf -- parsed best-effort (silently skipped on error)
- Points come from two sources: inline `pointList` in the protobuf OR separate point files. Point files take precedence only when inline points are empty.
- Page size format varies by device firmware: "WxH" string or JSON `{"width":N,"height":N}`

## Subpackages
- `proto/` -- Generated protobuf code (NoteInfo, VirtualPage, ShapeInfoProto, ShapeInfoProtoList). Regenerate with `protoc` via `generate.go`.
- `testutil/` -- Exported `BuildTestNoteFile(t, tmpDir, opts)` helper that constructs valid .note ZIPs for testing. Used by `booxpipeline` and `booxrender` tests.
