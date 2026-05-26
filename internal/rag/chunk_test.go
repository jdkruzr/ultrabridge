package rag

import (
	"strings"
	"testing"
)

func TestChunkText_ShortReturnsSingle(t *testing.T) {
	got := ChunkText("hello world")
	if len(got) != 1 || got[0] != "hello world" {
		t.Fatalf("short text should be one chunk, got %v", got)
	}
}

func TestChunkText_EmptyReturnsNone(t *testing.T) {
	if got := ChunkText("   \n  "); len(got) != 0 {
		t.Fatalf("blank text should yield no chunks, got %v", got)
	}
}

func TestChunkText_SplitsLongTextUnderBudget(t *testing.T) {
	// 10k chars of words → multiple chunks, each within the char budget.
	text := strings.TrimSpace(strings.Repeat("lorem ipsum dolor sit amet ", 400)) // ~10.8k chars
	chunks := ChunkText(text)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if len(c) > chunkMaxChars {
			t.Errorf("chunk %d over budget: %d > %d", i, len(c), chunkMaxChars)
		}
		if strings.TrimSpace(c) == "" {
			t.Errorf("chunk %d is blank", i)
		}
	}
	// No content lost: concatenated non-space content matches.
	join := strings.Join(chunks, " ")
	if strings.Fields(join)[0] != "lorem" || len(strings.Fields(join)) != len(strings.Fields(text)) {
		t.Errorf("chunking dropped/duplicated words: %d vs %d", len(strings.Fields(join)), len(strings.Fields(text)))
	}
}

func TestChunkText_HardSplitsUnbrokenRun(t *testing.T) {
	// A single token longer than the budget must still be split (no infinite loop,
	// no over-budget chunk).
	text := strings.Repeat("x", chunkMaxChars*2+50)
	chunks := ChunkText(text)
	if len(chunks) < 3 {
		t.Fatalf("expected the unbroken run to hard-split, got %d", len(chunks))
	}
	for i, c := range chunks {
		if len(c) > chunkMaxChars {
			t.Errorf("chunk %d over budget: %d", i, len(c))
		}
	}
}
