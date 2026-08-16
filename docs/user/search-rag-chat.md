# Search, RAG, OCR, And Chat

UltraBridge combines OCR text, FTS5 search, optional embeddings, and local chat into one retrieval surface across enabled sources.

## OCR

OCR settings live in **Settings -> AI & Processing**:

- Provider format: Anthropic-style or OpenAI-compatible.
- API URL and key.
- Model.
- Concurrency and max-file size.
- Source-specific prompt overrides for Supernote, ForestNote, and Boox. (reMarkable has no prompt override.)

Supernote, Boox, ForestNote, and reMarkable feed text into the index through source-specific paths; Supernote digests are a fifth searchable surface.

## Search

The Search tab combines a query box, a **sort** (Relevance / Most recent / Earliest first — **Most recent** is the default), a **mode** (Keyword or Semantic), per-source checkboxes, a Location facet, and created/modified date ranges. Result badges identify source type:

- `SN`: Supernote
- `B`: Boox
- `FN`: ForestNote
- `RM`: reMarkable
- `DIG`: Supernote digest

Results link to the canonical in-tab detail page for each note; MCP search results carry the same canonical links plus native-opener links where a source has them.

## Embeddings And RAG

When embeddings are enabled, UltraBridge sends page text to Ollama and stores vectors in SQLite. Retrieval combines keyword and vector results with reciprocal rank fusion; keyword hits are weighted above vector-only hits, and filtered searches (source/device/date/location) fetch a larger candidate set before filtering so recall doesn't collapse.

Default embedding model:

```bash
ollama pull nomic-embed-text:v1.5
```

Then enable embeddings in Settings and run the backfill if you already have indexed notes.

If Ollama is unavailable, OCR and keyword indexing continue; RAG/vector search degrades until the embedding service is restored.

## Chat

The Chat tab uses an OpenAI-compatible chat endpoint, such as vLLM or Ollama. UltraBridge retrieves relevant pages, builds a context prompt, streams the response, and renders citations back to source pages.

Example local server:

```bash
vllm serve Qwen/Qwen3-8B
```

Then set the chat API URL and model in **Settings -> AI & Processing**.

## Common Failure Modes

- No OCR text: reprocess the source item and inspect the Files detail page. Note that reMarkable PDFs are only OCR'd on manual reprocess, and EPUBs can't be rendered or OCR'd at all.
- No vector results: check Ollama URL/model and run backfill.
- Chat errors: check model ID, API URL, and Logs tab.
