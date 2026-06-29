# Search, RAG, OCR, And Chat

UltraBridge combines OCR text, FTS5 search, optional embeddings, and local chat into one retrieval surface across enabled sources.

## OCR

OCR settings live in **Settings -> AI & Processing**:

- Provider format: Anthropic-style or OpenAI-compatible.
- API URL and key.
- Model.
- Concurrency and max-file size.
- Source-specific prompt overrides.

Supernote, Boox, ForestNote, and reMarkable feed text into the index through source-specific paths.

## Keyword Search

The Search tab uses FTS5 keyword search and source filters. Result badges identify source type:

- `SN`: Supernote
- `B`: Boox
- `FN`: ForestNote
- `RM`: reMarkable

## Embeddings And RAG

When embeddings are enabled, UltraBridge sends page text to Ollama and stores vectors in SQLite. Retrieval combines keyword and vector results with reciprocal rank fusion.

Default embedding model:

```bash
ollama pull nomic-embed-text:v1.5
```

Then enable embeddings in Settings and run the backfill if you already have indexed notes.

If Ollama is unavailable, OCR and keyword indexing continue; RAG/vector search degrades until the embedding service is restored.

## Chat

The Chat tab uses an OpenAI-compatible chat endpoint, such as vLLM. UltraBridge retrieves relevant pages, builds a context prompt, streams the response, and renders citations back to source pages.

Example local server:

```bash
vllm serve Qwen/Qwen3-8B
```

Then set the chat API URL and model in **Settings -> AI & Processing**.

## Common Failure Modes

- No OCR text: reprocess the source item and inspect the Files detail page.
- No vector results: check Ollama URL/model and run backfill.
- Chat errors: check model ID, API URL, and Logs tab.
