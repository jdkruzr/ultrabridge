# Note RAG: Retrieval-Augmented Generation over Handwritten Notes

Brainstorming document for building insight generation from the collected
OCR'd note content in UltraBridge's SQLite database.

## Goal

Let users ask natural language questions about their notes and get
synthesized answers grounded in actual note content. Examples:

- "What were the key decisions from my steering committee meetings?"
- "Summarize what happened with the CCC project in Q4 2025"
- "What open items did I discuss with Deepak in the last 3 months?"
- "Find all mentions of budget concerns across my meeting notes"

## What We Have

### Data Layer (already built)
- `note_content` table: per-page OCR text with `note_path`, `page`, `body_text`, `title_text`, `keywords`, `source`
- FTS5 index (`note_fts`): keyword search with BM25 scoring and snippet extraction
- `boox_notes` / `notes` tables: metadata (device model, folder, timestamps, page count)
- Unified across Supernote and Boox pipelines
- ~12,000+ pages of handwritten meeting notes, personal notes, etc.

### Infrastructure (already running)
- Qwen3-VL-8B on RTX 5060 Ti (16GB) — local, no API costs, ~10s/page for OCR
- SQLite with WAL mode, single-writer
- Web UI with tabs, Settings, SSE log streaming

## Architecture Options

### Option A: Hybrid FTS5 + LLM (No Embeddings)

Leverage existing FTS5 index for retrieval, LLM for query understanding and synthesis.

```
User Question
    |
    v
[Query Expansion LLM Call]
    "What decisions were made in steering committee?"
    -> generates: ["steering committee decision", "HPC steering action items",
                   "steering committee resolution", "committee approved"]
    |
    v
[FTS5 Search] x N queries
    -> top-K pages per query, deduplicated, ranked by BM25
    |
    v
[Context Assembly]
    - Page text + metadata (date, folder, device, title)
    - Sorted by date or relevance
    - Truncated to fit context window
    |
    v
[Synthesis LLM Call]
    "Given these note excerpts, answer the user's question..."
    -> Grounded answer with citations (note title, date, page)
```

**Pros:**
- No new infrastructure (FTS5 already exists)
- No embedding model needed
- Works today with minimal code
- Keyword search is actually decent for meeting notes (people write topic names)

**Cons:**
- Misses semantic matches ("budget concerns" won't find "cost overruns")
- OCR noise reduces keyword recall
- Query expansion adds an extra LLM round-trip

### Option B: Vector Embeddings + FTS5 Hybrid

Add semantic search alongside keyword search.

```
[Indexing Pipeline]
    On OCR completion:
        body_text -> embedding model -> vector stored in DB

[Query Time]
    User question -> embedding -> vector similarity search
                  -> FTS5 keyword search (parallel)
                  -> merge & re-rank results
                  -> synthesis LLM call
```

**Embedding model options:**
- Local: `nomic-embed-text` or `bge-small-en` via Ollama/vLLM (~100M params, runs on CPU)
- API: OpenAI `text-embedding-3-small` (cheap but external)

**Vector storage options:**
- `sqlite-vec` (pure C, single-file, integrates with existing DB)
  - Caveat: need to verify compatibility with modernc.org/sqlite (pure-Go)
  - May need CGO or a separate process
- Separate SQLite DB with a Go vector search library
- Qdrant/ChromaDB (separate service, more ops burden)

**Pros:**
- Semantic matching ("budget" finds "cost", "funding", "expenditure")
- More tolerant of OCR noise
- Industry-standard RAG approach

**Cons:**
- Embedding model infrastructure (even small ones need hosting)
- Reindex all existing content
- More complex query pipeline
- Diminishing returns for meeting notes (which tend to use literal topic names)

### Option C: Full-Context Approach (No Retrieval)

For a small enough corpus, skip retrieval entirely — stuff everything into a
large-context LLM.

At ~200 tokens/page average (handwritten notes are sparse), 12,000 pages is
~2.4M tokens. That's within Gemini 2.5 Pro's context window (1M+) if you
summarize first, or could work with a two-stage approach:

1. Pre-compute per-note summaries (stored in DB)
2. Load all summaries (~50 tokens each = 600K tokens) into context
3. LLM identifies relevant notes, then load full pages for those

**Pros:**
- No retrieval pipeline to build or tune
- LLM sees everything, no recall issues
- Simplest code

**Cons:**
- Requires large-context model (not local Qwen)
- API costs per query
- Slow (processing millions of tokens per question)
- Not practical for real-time chat

## Recommended Approach: Start with A, Add B Later

### Phase 1: Hybrid FTS5 + LLM (Option A)

Minimal new code. Could be built in a day:

1. New `internal/rag/` package:
   - `QueryExpander` — takes user question, returns N search queries
   - `Retriever` — runs FTS5 queries, deduplicates, ranks, assembles context
   - `Synthesizer` — sends context + question to LLM, returns answer

2. New web routes:
   - `GET /chat` — chat UI tab
   - `POST /chat/ask` — accepts question, returns streamed answer (SSE)
   - `GET /chat/history` — previous Q&A pairs (stored in SQLite)

3. LLM routing:
   - Query expansion: local Qwen (fast, cheap, good enough for keyword generation)
   - Synthesis: configurable — local Qwen for simple queries, or external API
     (Claude, GPT-4) for complex multi-note synthesis
   - Could use same OCR client infrastructure (OpenAI-compatible API)

### Phase 2: Add Embeddings (Option B)

Once Phase 1 is working and we understand retrieval gaps:

1. Add embedding generation to the OCR pipeline (after indexing)
2. Store embeddings alongside note_content
3. Hybrid retrieval: FTS5 + vector similarity, merge results
4. Evaluate: does semantic search actually improve answers?

## Open Questions

- **LLM for synthesis**: Local Qwen3-8B can do simple Q&A but may struggle
  with complex multi-document synthesis. Is an external API acceptable for
  this use case, or must everything stay local?

- **Context window management**: With dense meeting notes, top-20 pages could
  be 4000+ tokens. Local 8B model with 16K context can handle ~12K tokens of
  note content + prompt. Is that enough, or do we need summarization?

- **Chat history**: Should the system remember previous questions in a session
  to enable follow-ups ("tell me more about the second point")?

- **Date awareness**: Many queries will be time-scoped ("last month", "Q4 2025").
  The retriever needs to filter by note date. FTS5 doesn't support date
  filters natively — need to JOIN with boox_notes/notes and filter in SQL.

- **Folder/source scoping**: "What did I write on my Palma about work?"
  needs device + folder filtering. Already have this metadata.

- **Incremental updates**: New notes get indexed automatically. RAG index
  stays current with no extra work (FTS5 triggers handle it). Embeddings
  would need a similar auto-index hook.

## Decision: Embedding Model

**Choice: `nomic-embed-text-v1.5`** via Ollama on the UltraBridge host CPU.

### Why This Model

| Model | Params | Dims | MTEB Score | CPU Speed |
|-------|--------|------|------------|-----------|
| all-MiniLM-L6-v2 | 22M | 384 | 56.3 | ~30ms |
| bge-small-en-v1.5 | 33M | 384 | 62.2 | ~50ms |
| gte-small | 33M | 384 | 61.4 | ~50ms |
| **nomic-embed-text-v1.5** | **137M** | **768** | **65.5** | **~150ms** |

Nomic wins on quality while staying CPU-feasible. 768 dims is more expressive
than 384 at the cost of 2x storage per vector — still only ~3KB per page.

Matryoshka representation support means we can truncate to 256 dims later
if needed, with minimal quality loss.

### Storage Math

- 768 floats * 4 bytes = 3,072 bytes per page embedding
- 12,000 pages = ~36 MB in memory for brute-force search
- Brute-force cosine similarity over all vectors: <5ms per query
- No vector index needed at this scale (ANN indices help at 100K+ docs)

Store as BLOBs in a `note_embeddings` SQLite table alongside existing data.
Load into memory on startup. No CGO, no extensions, no external vector DB.

### Pipeline Integration

Add embedding generation after OCR indexing in the worker:

```
OCR -> index body_text in note_content -> embed body_text -> store in note_embeddings
```

At 150ms/chunk on CPU, this adds ~2% to a 7s/page pipeline. Negligible.
Runs on CPU so it doesn't compete with the GPU VLM.

### Reindexing Cost

If the model changes: 12K pages * 150ms = 30 minutes of background CPU work.
Non-event at this scale. Would need millions of documents before model
migration becomes painful.

### Hosting

Ollama on the UltraBridge host (192.168.9.52):
- `ollama pull nomic-embed-text`
- Ollama exposes an embedding API at `http://localhost:11434/api/embeddings`
- No GPU needed, runs on CPU
- Same Docker network, container can reach host via `host.docker.internal`
  or the host IP

### Why Not Wait

The system's purpose is patching executive function for an ADHD brain.
RAG over notes is core to that mission, not a nice-to-have. Embeddings
meaningfully improve retrieval when the user asks conceptual questions
("what were the key decisions?") rather than keyword queries. The
infrastructure cost is minimal, so there's no reason to defer.

### Future-Proofing

Small embedding models have plateaued. Gains in the last 2 years came from
training techniques (contrastive learning, hard negatives), not architecture.
A 33-137M model is near the ceiling for its parameter budget. Dramatic leaps
require 7B+ models (GPU-only). Incremental improvements (MTEB 65 -> 68)
won't invalidate the approach — and reindexing is cheap at our scale anyway.

## Decision: Retrieval Strategy

**Hybrid: FTS5 keyword search + vector similarity, merged results.**

Both retrieval paths run in parallel at query time:
1. FTS5 keyword search (existing) — catches exact matches, handles topic names
2. Vector cosine similarity (new) — catches semantic matches, tolerates OCR noise

Results are merged by reciprocal rank fusion (RRF) or simple interleaving,
deduplicated by (note_path, page), and fed to the synthesis LLM.

This avoids the "query expansion" step from Option A (saving an LLM round-trip)
while covering both lexical and semantic retrieval.

## Decision: Generation Interface

Two generation models, each suited to different query types:

### 1. Local Qwen3-VL-8B (existing infrastructure)

- Already running on RTX 5060 Ti for OCR
- 16K context window — fits ~15-20 retrieved pages + prompt
- Good for: quick factual lookups, single-topic queries
  - "When was my last meeting with Deepak?"
  - "What did I write about the Box renewal?"
- Free, fast, fully private
- Interface: chat tab in UltraBridge web UI

### 2. Claude via MCP (Model Context Protocol)

The more powerful option. UltraBridge exposes an MCP server that Claude
can call to search and retrieve notes. Claude drives the retrieval
iteratively and reasons across results.

**MCP Tools exposed by UltraBridge:**

```
search_notes(query, folder?, device?, date_range?)
  -> returns matching pages: text, metadata (date, device, folder, title, page)

get_note_pages(note_path)
  -> returns all pages of a specific note with full OCR text

get_note_image(note_path, page)
  -> returns rendered JPEG of the handwritten page
```

**Example conversation:**

> User: "Did I ever commit to a timeline for the CCC migration?"
>
> Claude calls search_notes("CCC migration timeline commit")
> -> gets 5 matching pages across 3 notes
>
> Claude: "Yes, in three places:
> - 2025-03-14, Deepak Change Mgmt (Palma2, Moffitt): Q3 target for phase 1
> - 2025-12-05, Demand Notes (Palma2, Moffitt): Q3 slipped to Q4
> - 2026-03-30, CCC Legal (Palma2, Moffitt): confirmed completion, under budget
>
> Want me to pull up the actual pages?"
>
> User: "Yeah, show me the legal one"
>
> Claude calls get_note_image(".../20260330 CCC Legal.pdf", 0)
> -> renders the handwritten page inline

**Why MCP over a simple API:**

- **Iterative retrieval**: Claude can refine searches based on what it finds.
  "That's not quite right, let me also search for budget discussions" — without
  the user manually re-querying.
- **Conversation context**: Claude remembers prior searches within a session.
  Can cross-reference findings, spot patterns, build narrative across months
  of meeting notes.
- **Multimodal verification**: Claude can view the actual handwritten page
  image to catch details the OCR missed or misread.
- **No prompt engineering on the user's part**: The user asks natural questions.
  Claude decides what to search for, how many results to pull, whether to
  drill deeper.

This is the real executive function patch — not "find this note" but "help me
understand the arc of what happened across 6 months of scattered meeting notes."

### MCP Server Implementation

The MCP server would be a lightweight addition to UltraBridge:

- Built-in MCP endpoint at `/mcp`
- Tools call into existing infrastructure:
  - `search_notes` -> hybrid FTS5 + vector retrieval (same as chat tab)
  - `get_note_pages` -> `search.GetContent()` (already exists)
  - `get_note_image` -> `/files/boox/render` endpoint (already exists, just needs
    to return bytes instead of serving HTTP)
- Registered in Claude Desktop / claude.ai via MCP config
- Runs inside the main UltraBridge process

No new database work — the MCP tools are a thin interface over retrieval
and rendering capabilities that already exist.

### Which to build first

Build the retrieval pipeline (embeddings + hybrid search) first — both
interfaces use it. Then:

1. **MCP server** — higher impact, more natural UX, leverages Claude's
   reasoning for complex queries
2. **Chat tab** — simpler to build, works offline/fully local, good for
   quick lookups when you don't need Claude's reasoning depth

Both can coexist. Quick factual lookups in the web UI, deep synthesis
via Claude.

## Data Quality Considerations

- OCR of handwriting is inherently noisy. Expect 80-95% accuracy depending
  on handwriting quality and pen type.
- Red ink / light strokes may OCR poorly.
- Abbreviations and shorthand are common in meeting notes — the LLM needs
  to handle "SC" = "Scientific Computing", "RIT" = "Research IT", etc.
- Could add a user-configurable glossary/abbreviation map to the prompt.
- Pages with diagrams/drawings will produce garbage OCR — consider a
  "content type" classifier to skip non-text pages.
