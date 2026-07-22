package embedding

import (
	"slices"
	"strings"
	"testing"

	"github.com/docgraph/docgraph/internal/domain"
)

func TestEvaluateEmbeddingReconciliationABCToCDE(t *testing.T) {
	plan := reconciliationTestPlan()
	sectionA := reconciliationTestSection("a", "hash-a")
	sectionB := reconciliationTestSection("b", "hash-b")
	sectionC := reconciliationTestSection("c", "hash-c")
	sectionD := reconciliationTestSection("d", "hash-d")
	sectionE := reconciliationTestSection("e", "hash-e")
	chunkA := reconciliationTestChunk(sectionA, "chunk-a", 0)
	chunkB := reconciliationTestChunk(sectionB, "chunk-b", 0)
	chunkC := reconciliationTestChunk(sectionC, "chunk-c", 0)

	result := EvaluateEmbeddingReconciliation(
		[]ExpectedSectionEmbedding{
			{Section: sectionE, Chunks: []EmbeddingChunk{reconciliationTestChunk(sectionE, "chunk-e", 0)}},
			{Section: sectionC, Chunks: []EmbeddingChunk{chunkC}},
			{Section: sectionD, Chunks: []EmbeddingChunk{reconciliationTestChunk(sectionD, "chunk-d", 0)}},
		},
		[]domain.EmbeddingChunkHash{
			reconciliationStoredChunk(chunkB, plan),
			reconciliationStoredChunk(chunkC, plan),
			reconciliationStoredChunk(chunkA, plan),
		},
		plan,
	)

	assertStringSliceEqual(t, result.ReadySectionIDs, []string{"c"})
	assertStringSliceEqual(t, result.PendingSectionIDs, []string{"d", "e"})
	assertStringSliceEqual(t, result.StaleSectionIDs, nil)
	assertStringSliceEqual(t, result.OrphanSectionIDs, []string{"a", "b"})
	if result.OrphanChunkCount != 2 || result.ActivePlanOrphanChunkCount != 2 || result.LegacyPlanOrphanChunkCount != 0 {
		t.Fatalf("orphan counts = total:%d active:%d legacy:%d, want 2/2/0", result.OrphanChunkCount, result.ActivePlanOrphanChunkCount, result.LegacyPlanOrphanChunkCount)
	}
}

func TestEvaluateSectionEmbeddingClassifiesHashMismatchAndPartialChunksStale(t *testing.T) {
	plan := reconciliationTestPlan()

	t.Run("hash mismatch", func(t *testing.T) {
		section := reconciliationTestSection("section-hash", "hash-new")
		expected := reconciliationTestChunk(section, "chunk-new", 0)
		stored := reconciliationStoredChunk(expected, plan)
		stored.SectionContentHash = "hash-old"

		evaluation := EvaluateSectionEmbedding(section, []EmbeddingChunk{expected}, []domain.EmbeddingChunkHash{stored}, plan)

		if evaluation.State != SectionEmbeddingStale || evaluation.Reason != "expected_chunks_incomplete" {
			t.Fatalf("evaluation = %+v, want stale expected_chunks_incomplete", evaluation)
		}
		if evaluation.ActivePlanChunks != 1 || evaluation.MissingExpectedChunks != 1 || evaluation.CurrentExpectedChunks != 0 {
			t.Fatalf("evaluation counts = %+v, want active=1 missing=1 current=0", evaluation)
		}
	})

	t.Run("partial chunks", func(t *testing.T) {
		section := reconciliationTestSection("section-partial", "hash-partial")
		expected := []EmbeddingChunk{
			reconciliationTestChunk(section, "chunk-partial-0", 0),
			reconciliationTestChunk(section, "chunk-partial-1", 1),
		}
		stored := reconciliationStoredChunk(expected[0], plan)

		evaluation := EvaluateSectionEmbedding(section, expected, []domain.EmbeddingChunkHash{stored}, plan)

		if evaluation.State != SectionEmbeddingStale || evaluation.ExpectedChunks != 2 || evaluation.CurrentExpectedChunks != 1 || evaluation.MissingExpectedChunks != 1 {
			t.Fatalf("evaluation = %+v, want stale with one of two expected chunks missing", evaluation)
		}
	})

	t.Run("unexpected active chunk", func(t *testing.T) {
		section := reconciliationTestSection("section-extra", "hash-extra")
		expected := reconciliationTestChunk(section, "chunk-extra-current", 0)
		extra := reconciliationTestChunk(section, "chunk-extra-obsolete", 1)

		evaluation := EvaluateSectionEmbedding(section, []EmbeddingChunk{expected}, []domain.EmbeddingChunkHash{
			reconciliationStoredChunk(expected, plan),
			reconciliationStoredChunk(extra, plan),
		}, plan)

		if evaluation.State != SectionEmbeddingStale || evaluation.Reason != "unexpected_active_chunks" || evaluation.UnexpectedActiveChunks != 1 {
			t.Fatalf("evaluation = %+v, want one unexpected active chunk to make the section stale", evaluation)
		}
	})
}

func TestEvaluateSectionEmbeddingAcceptsDeterministicRetrySplitChunks(t *testing.T) {
	plan := reconciliationTestPlan()
	section := reconciliationTestSection("section-retry", "hash-retry")
	expected := reconciliationTestChunk(section, "chunk-retry-original", 0)
	expected.Text = strings.Repeat("context exceeded text ", 40)
	expected.TextHash = HashText(expected.Text)
	expected.ChunkTokenCount = 200

	pending := domain.EmbeddingChunkInput{
		ChunkID:            expected.ChunkID,
		SectionID:          expected.SectionID,
		DocumentID:         expected.DocumentID,
		SourceID:           expected.SourceID,
		ChunkOrdinal:       expected.ChunkOrdinal,
		ChunkStartToken:    expected.ChunkStartToken,
		ChunkTokenCount:    expected.ChunkTokenCount,
		ChunkText:          expected.Text,
		Model:              plan.Model,
		SectionContentHash: expected.SectionContentHash,
		ChunkTextHash:      expected.TextHash,
		Tokenizer:          expected.Tokenizer,
		ChunkStrategy:      expected.ChunkStrategy,
		GeneratorVersion:   plan.GeneratorVersion,
	}
	split := splitPendingChunkForRetry(pending)
	if len(split) != 2 {
		t.Fatalf("retry split count = %d, want 2", len(split))
	}
	stored := make([]domain.EmbeddingChunkHash, 0, len(split))
	for _, part := range split {
		stored = append(stored, domain.EmbeddingChunkHash{
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
		})
	}

	evaluation := EvaluateSectionEmbedding(section, []EmbeddingChunk{expected}, stored, plan)

	if evaluation.State != SectionEmbeddingReady || evaluation.ExpectedChunks != 1 || evaluation.CurrentExpectedChunks != 1 || evaluation.ActivePlanChunks != 2 {
		t.Fatalf("evaluation = %+v, want retry-split chunks to satisfy one expected chunk", evaluation)
	}
}

func TestEvaluateEmbeddingReconciliationSeparatesLegacyPlansAndOrphans(t *testing.T) {
	activePlan := reconciliationTestPlan()
	legacyPlan := activePlan
	legacyPlan.GeneratorVersion = "generator-v1"
	sectionC := reconciliationTestSection("c", "hash-c")
	sectionD := reconciliationTestSection("d", "hash-d")
	sectionX := reconciliationTestSection("x", "hash-x")
	activeC := reconciliationTestChunk(sectionC, "chunk-c-active", 0)
	legacyC := reconciliationTestChunk(sectionC, "chunk-c-legacy", 0)
	legacyD := reconciliationTestChunk(sectionD, "chunk-d-legacy", 0)
	activeX := reconciliationTestChunk(sectionX, "chunk-x-active", 0)
	legacyX := reconciliationTestChunk(sectionX, "chunk-x-legacy", 0)

	result := EvaluateEmbeddingReconciliation(
		[]ExpectedSectionEmbedding{
			{Section: sectionD, Chunks: []EmbeddingChunk{reconciliationTestChunk(sectionD, "chunk-d-active", 0)}},
			{Section: sectionC, Chunks: []EmbeddingChunk{activeC}},
		},
		[]domain.EmbeddingChunkHash{
			reconciliationStoredChunk(legacyX, legacyPlan),
			reconciliationStoredChunk(activeC, activePlan),
			reconciliationStoredChunk(legacyD, legacyPlan),
			reconciliationStoredChunk(activeX, activePlan),
			reconciliationStoredChunk(legacyC, legacyPlan),
		},
		activePlan,
	)

	assertStringSliceEqual(t, result.ReadySectionIDs, []string{"c"})
	assertStringSliceEqual(t, result.PendingSectionIDs, []string{"d"})
	assertStringSliceEqual(t, result.StaleSectionIDs, nil)
	assertStringSliceEqual(t, result.LegacyPlanSectionIDs, []string{"c", "d"})
	assertStringSliceEqual(t, result.OrphanSectionIDs, []string{"x"})
	if result.CurrentLegacyPlanChunkCount != 2 {
		t.Fatalf("CurrentLegacyPlanChunkCount = %d, want 2", result.CurrentLegacyPlanChunkCount)
	}
	if result.OrphanChunkCount != 2 || result.ActivePlanOrphanChunkCount != 1 || result.LegacyPlanOrphanChunkCount != 1 {
		t.Fatalf("orphan counts = total:%d active:%d legacy:%d, want 2/1/1", result.OrphanChunkCount, result.ActivePlanOrphanChunkCount, result.LegacyPlanOrphanChunkCount)
	}
}

func reconciliationTestPlan() EmbeddingPlan {
	return EmbeddingPlan{
		Model:            "test-model",
		GeneratorVersion: "generator-v2",
		Tokenizer:        "conservative",
		ChunkStrategy:    "structural",
	}
}

func reconciliationTestSection(id string, contentHash string) domain.EmbeddingSection {
	return domain.EmbeddingSection{
		SectionID:   id,
		DocumentID:  "doc-" + id,
		SourceID:    "source-test",
		ContentHash: contentHash,
	}
}

func reconciliationTestChunk(section domain.EmbeddingSection, chunkID string, ordinal int) EmbeddingChunk {
	return EmbeddingChunk{
		ChunkID:            chunkID,
		SectionID:          section.SectionID,
		DocumentID:         section.DocumentID,
		SourceID:           section.SourceID,
		ChunkOrdinal:       ordinal,
		ChunkTokenCount:    20,
		Text:               "chunk text " + chunkID,
		TextHash:           "text-hash-" + chunkID,
		SectionContentHash: section.ContentHash,
		Tokenizer:          "conservative",
		ChunkStrategy:      "structural",
	}
}

func reconciliationStoredChunk(chunk EmbeddingChunk, plan EmbeddingPlan) domain.EmbeddingChunkHash {
	return domain.EmbeddingChunkHash{
		ChunkID:            chunk.ChunkID,
		SectionID:          chunk.SectionID,
		DocumentID:         chunk.DocumentID,
		SourceID:           chunk.SourceID,
		ChunkOrdinal:       chunk.ChunkOrdinal,
		Model:              plan.Model,
		SectionContentHash: chunk.SectionContentHash,
		ChunkTextHash:      chunk.TextHash,
		Tokenizer:          plan.Tokenizer,
		ChunkStrategy:      plan.ChunkStrategy,
		GeneratorVersion:   plan.GeneratorVersion,
	}
}

func assertStringSliceEqual(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("values = %v, want %v", got, want)
	}
}
