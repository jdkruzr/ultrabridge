package rag

// FCIS: Functional Core — pure text chunking, no I/O.

import "strings"

// chunkMaxChars bounds one embedding chunk. nomic-embed-text's context is 2048
// tokens; at a worst-case ~1 char/token (dense OCR with numbers/symbols) 1500
// chars stays safely under it, and typical prose (~4 chars/token) packs ~375
// tokens. We chunk on the char budget rather than tokens because we have no
// tokenizer and the server rejects (does not truncate) over-context input.
// See memory project_ollama_embedding_cpu_and_chunking.
const chunkMaxChars = 1500

// ChunkText splits text into embedding-sized chunks, each at most chunkMaxChars,
// preferring to break on whitespace so words stay intact. Blank input yields no
// chunks; short input yields a single chunk. A single unbroken run longer than
// the budget is hard-split. No content is dropped.
func ChunkText(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if len(text) <= chunkMaxChars {
		return []string{text}
	}

	var chunks []string
	rest := text
	for len(rest) > chunkMaxChars {
		// Prefer the last whitespace boundary within the budget so words aren't
		// split; fall back to a hard cut for an unbroken run.
		cut := strings.LastIndexAny(rest[:chunkMaxChars+1], " \t\n\r")
		if cut <= 0 {
			cut = chunkMaxChars
		}
		if c := strings.TrimSpace(rest[:cut]); c != "" {
			chunks = append(chunks, c)
		}
		rest = strings.TrimLeft(rest[cut:], " \t\n\r")
	}
	if c := strings.TrimSpace(rest); c != "" {
		chunks = append(chunks, c)
	}
	return chunks
}
