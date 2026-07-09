package embedding

import (
	"strings"
	"testing"

	"github.com/docgraph/docgraph/internal/domain"
)

func TestChunkerSplitsLongSectionIntoMultipleChunks(t *testing.T) {
	section := domain.EmbeddingSection{
		SectionID:     "section-long",
		DocumentID:    "doc-long",
		SourceID:      "source-long",
		SourceName:    "Docs",
		DocumentTitle: "Long Section",
		Title:         "Chunking",
		Content:       strings.Repeat("长段落内容需要切分。\n\n", 120),
		ContentHash:   "hash-long",
	}

	chunks, err := Chunker{
		Measurer:           ConservativeTextMeasurer{},
		ChunkTargetTokens:  120,
		ChunkOverlapTokens: 12,
	}.BuildChunks(section, "test-model", "generator-v1")
	if err != nil {
		t.Fatalf("BuildChunks returned error: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("chunk count = %d, want multiple chunks", len(chunks))
	}
	for i, chunk := range chunks {
		if chunk.ChunkID == "" || chunk.SectionID != section.SectionID || chunk.DocumentID != section.DocumentID || chunk.SourceID != section.SourceID {
			t.Fatalf("chunk[%d] trace = %+v, want stable source/document/section metadata", i, chunk)
		}
		if chunk.ChunkOrdinal != i {
			t.Fatalf("chunk[%d].ChunkOrdinal = %d, want %d", i, chunk.ChunkOrdinal, i)
		}
		if chunk.Tokenizer != "conservative" {
			t.Fatalf("chunk[%d].Tokenizer = %q, want conservative", i, chunk.Tokenizer)
		}
		if chunk.TextHash == "" || chunk.SectionContentHash != section.ContentHash {
			t.Fatalf("chunk[%d] hashes = %+v, want text and section content hashes", i, chunk)
		}
	}
}

func TestPlanEmbeddingChunkBatchesRespectsMaxBatchTokens(t *testing.T) {
	chunks := []domain.EmbeddingChunkInput{
		{ChunkID: "c1", ChunkTokenCount: 40},
		{ChunkID: "c2", ChunkTokenCount: 35},
		{ChunkID: "c3", ChunkTokenCount: 50},
		{ChunkID: "c4", ChunkTokenCount: 20},
	}

	batches := planEmbeddingChunkBatches(chunks, 64, 80)

	if len(batches) != 2 {
		t.Fatalf("batch count = %d, want 2: %+v", len(batches), batches)
	}
	for _, batch := range batches {
		if batch.Tokens > 80 {
			t.Fatalf("batch %+v exceeds max_batch_tokens=80", batch)
		}
		if got := batch.End - batch.Start; got > 64 {
			t.Fatalf("batch %+v exceeds batch_size=64 with %d chunks", batch, got)
		}
	}
	if batches[0].Start != 0 || batches[0].End != 2 || batches[0].Tokens != 75 {
		t.Fatalf("first batch = %+v, want c1+c2 tokens=75", batches[0])
	}
	if batches[1].Start != 2 || batches[1].End != 4 || batches[1].Tokens != 70 {
		t.Fatalf("second batch = %+v, want c3+c4 tokens=70", batches[1])
	}
}

func TestSplitPendingChunkForRetryKeepsTraceableChunkMetadata(t *testing.T) {
	chunk := domain.EmbeddingChunkInput{
		ChunkID:            "chunk-original",
		SectionID:          "section-1",
		DocumentID:         "doc-1",
		SourceID:           "source-1",
		ChunkOrdinal:       3,
		ChunkStartToken:    100,
		ChunkTokenCount:    200,
		ChunkText:          strings.Repeat("context exceeded text ", 40),
		Model:              "test-model",
		SectionContentHash: "section-hash",
		ChunkTextHash:      "old-hash",
		Tokenizer:          "conservative",
		ChunkStrategy:      "adaptive",
		GeneratorVersion:   "generator-v1",
	}

	split := splitPendingChunkForRetry(chunk)

	if len(split) != 2 {
		t.Fatalf("split count = %d, want 2", len(split))
	}
	for i, item := range split {
		if item.ChunkID == "" || item.ChunkID == chunk.ChunkID {
			t.Fatalf("split[%d].ChunkID = %q, want new stable id", i, item.ChunkID)
		}
		if item.SectionID != chunk.SectionID || item.DocumentID != chunk.DocumentID || item.SourceID != chunk.SourceID {
			t.Fatalf("split[%d] trace = %+v, want original source/document/section", i, item)
		}
		if item.SectionContentHash != chunk.SectionContentHash || item.ChunkTextHash == "" || item.ChunkTextHash == chunk.ChunkTextHash {
			t.Fatalf("split[%d] hashes = %+v, want section hash and new chunk hash", i, item)
		}
		if item.ChunkTokenCount <= 0 {
			t.Fatalf("split[%d].ChunkTokenCount = %d, want positive", i, item.ChunkTokenCount)
		}
		if item.ChunkStrategy != chunk.ChunkStrategy {
			t.Fatalf("split[%d].ChunkStrategy = %q, want %q", i, item.ChunkStrategy, chunk.ChunkStrategy)
		}
	}
	if split[0].ChunkOrdinal == chunk.ChunkOrdinal || split[1].ChunkOrdinal == chunk.ChunkOrdinal || split[0].ChunkOrdinal == split[1].ChunkOrdinal {
		t.Fatalf("split ordinals = %d/%d from original %d, want distinct retry ordinals", split[0].ChunkOrdinal, split[1].ChunkOrdinal, chunk.ChunkOrdinal)
	}
}

func TestRetrySplitChunksCountAsCurrentForEnsure(t *testing.T) {
	chunk := EmbeddingChunk{
		ChunkID:            "chunk-original",
		SectionID:          "section-1",
		DocumentID:         "doc-1",
		SourceID:           "source-1",
		ChunkOrdinal:       0,
		ChunkStartToken:    0,
		ChunkTokenCount:    200,
		Text:               strings.Repeat("context exceeded text ", 40),
		TextHash:           "old-hash",
		SectionContentHash: "section-hash",
		Tokenizer:          "conservative",
		ChunkStrategy:      "auto",
	}
	pending := domain.EmbeddingChunkInput{
		ChunkID:            chunk.ChunkID,
		SectionID:          chunk.SectionID,
		DocumentID:         chunk.DocumentID,
		SourceID:           chunk.SourceID,
		ChunkOrdinal:       chunk.ChunkOrdinal,
		ChunkStartToken:    chunk.ChunkStartToken,
		ChunkTokenCount:    chunk.ChunkTokenCount,
		ChunkText:          chunk.Text,
		Model:              "test-model",
		SectionContentHash: chunk.SectionContentHash,
		ChunkTextHash:      chunk.TextHash,
		Tokenizer:          chunk.Tokenizer,
		ChunkStrategy:      chunk.ChunkStrategy,
		GeneratorVersion:   "generator-v1",
	}
	split := splitPendingChunkForRetry(pending)
	existing := map[string]domain.EmbeddingChunkHash{}
	for _, part := range split {
		existing[part.ChunkID] = domain.EmbeddingChunkHash{
			ChunkID:            part.ChunkID,
			SectionID:          part.SectionID,
			DocumentID:         part.DocumentID,
			SourceID:           part.SourceID,
			ChunkOrdinal:       part.ChunkOrdinal,
			Model:              part.Model,
			SectionContentHash: part.SectionContentHash,
			ChunkTextHash:      part.ChunkTextHash,
			Tokenizer:          part.Tokenizer,
			ChunkStrategy:      part.ChunkStrategy,
			GeneratorVersion:   part.GeneratorVersion,
		}
	}
	section := domain.EmbeddingSection{
		SectionID:   chunk.SectionID,
		DocumentID:  chunk.DocumentID,
		SourceID:    chunk.SourceID,
		ContentHash: chunk.SectionContentHash,
	}

	if !retrySplitChunksCurrent(existing, chunk, section, "test-model", "generator-v1") {
		t.Fatalf("retrySplitChunksCurrent returned false, want existing deterministic retry chunks to count as current")
	}
}
