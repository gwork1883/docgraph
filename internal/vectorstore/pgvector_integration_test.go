package vectorstore

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/docgraph/docgraph/internal/domain"
)

func TestPGVectorEnsureANNIndexIntegration(t *testing.T) {
	dsn := os.Getenv("DOCGRAPH_PGVECTOR_TEST_DSN")
	if dsn == "" {
		t.Skip("DOCGRAPH_PGVECTOR_TEST_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	store, err := OpenPGVector(ctx, dsn)
	if err != nil {
		t.Skipf("pgvector backend is not available: %v", err)
	}
	defer store.Close()

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}
	if err := store.EnsureANNIndex(ctx, "integration-test-model", 3); err != nil {
		t.Fatalf("EnsureANNIndex returned error: %v", err)
	}
}

func TestPGVectorSourceEmbeddingReconcilerIntegration(t *testing.T) {
	dsn := os.Getenv("DOCGRAPH_PGVECTOR_TEST_DSN")
	if dsn == "" {
		t.Skip("DOCGRAPH_PGVECTOR_TEST_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store, err := OpenPGVector(ctx, dsn)
	if err != nil {
		t.Skipf("pgvector backend is not available: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}

	suffix := fmt.Sprint(time.Now().UnixNano())
	sourceA := "integration-reconcile-source-a-" + suffix
	sourceB := "integration-reconcile-source-b-" + suffix
	sectionA := "integration-reconcile-section-a-" + suffix
	sectionB := "integration-reconcile-section-b-" + suffix
	foreignSection := "integration-reconcile-section-foreign-" + suffix
	model := "integration-reconcile-model-" + suffix
	const (
		activeGenerator = "integration-generator-active"
		legacyGenerator = "integration-generator-legacy"
		tokenizer       = "integration-tokenizer"
		strategy        = "structural"
	)
	allSectionIDs := []string{sectionA, sectionB, foreignSection}
	defer func() {
		_, _ = store.DeleteEmbeddingChunksBySectionIDs(context.Background(), sourceA, allSectionIDs)
		_, _ = store.DeleteEmbeddingChunksBySectionIDs(context.Background(), sourceB, allSectionIDs)
	}()

	inputs := []domain.EmbeddingChunkInput{
		reconciliationIntegrationChunk(sourceA, sectionA, model, activeGenerator, tokenizer, strategy, 0),
		reconciliationIntegrationChunk(sourceA, sectionA, model, legacyGenerator, tokenizer, strategy, 0),
		reconciliationIntegrationChunk(sourceA, sectionB, model, activeGenerator, tokenizer, strategy, 0),
		reconciliationIntegrationChunk(sourceB, foreignSection, model, activeGenerator, tokenizer, strategy, 0),
	}
	for _, input := range inputs {
		if err := store.UpsertEmbeddingChunk(ctx, input); err != nil {
			t.Fatalf("UpsertEmbeddingChunk(%s) returned error: %v", input.ChunkID, err)
		}
	}

	allPlans, err := store.ListEmbeddingChunkInventory(ctx, EmbeddingInventoryOptions{
		SourceID:        sourceA,
		Model:           "ignored-by-all-plans",
		IncludeAllPlans: true,
		Limit:           100,
	})
	if err != nil {
		t.Fatalf("ListEmbeddingChunkInventory(all plans) returned error: %v", err)
	}
	if len(allPlans) != 3 {
		t.Fatalf("all-plan inventory = %d rows, want 3: %+v", len(allPlans), allPlans)
	}
	for _, item := range allPlans {
		if item.SourceID != sourceA {
			t.Fatalf("all-plan inventory crossed source boundary: %+v", item)
		}
	}

	activePlan, err := store.ListEmbeddingChunkInventory(ctx, EmbeddingInventoryOptions{
		SourceID:         sourceA,
		Model:            model,
		GeneratorVersion: activeGenerator,
		Tokenizer:        tokenizer,
		ChunkStrategy:    strategy,
		Limit:            100,
	})
	if err != nil {
		t.Fatalf("ListEmbeddingChunkInventory(active plan) returned error: %v", err)
	}
	if len(activePlan) != 2 {
		t.Fatalf("active-plan inventory = %d rows, want 2: %+v", len(activePlan), activePlan)
	}
	globalActivePlan, err := store.ListEmbeddingChunkInventory(ctx, EmbeddingInventoryOptions{
		Model:            model,
		GeneratorVersion: activeGenerator,
		Tokenizer:        tokenizer,
		ChunkStrategy:    strategy,
		Limit:            100,
	})
	if err != nil {
		t.Fatalf("ListEmbeddingChunkInventory(global active plan) returned error: %v", err)
	}
	if len(globalActivePlan) != 3 {
		t.Fatalf("global active-plan inventory = %d rows, want 3 across both sources: %+v", len(globalActivePlan), globalActivePlan)
	}

	firstPage, err := store.ListEmbeddingChunkInventory(ctx, EmbeddingInventoryOptions{SourceID: sourceA, IncludeAllPlans: true, Limit: 2})
	if err != nil {
		t.Fatalf("ListEmbeddingChunkInventory(first page) returned error: %v", err)
	}
	secondPage, err := store.ListEmbeddingChunkInventory(ctx, EmbeddingInventoryOptions{SourceID: sourceA, IncludeAllPlans: true, Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("ListEmbeddingChunkInventory(second page) returned error: %v", err)
	}
	if len(firstPage) != 2 || len(secondPage) != 1 {
		t.Fatalf("inventory pages = %d/%d rows, want 2/1", len(firstPage), len(secondPage))
	}

	deleted, err := store.DeleteEmbeddingChunksBySectionIDs(ctx, sourceA, []string{" " + sectionA + " ", sectionA, foreignSection})
	if err != nil {
		t.Fatalf("DeleteEmbeddingChunksBySectionIDs returned error: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted rows = %d, want 2 active+legacy chunks", deleted)
	}
	deleted, err = store.DeleteEmbeddingChunksBySectionIDs(ctx, sourceA, []string{sectionA, foreignSection})
	if err != nil {
		t.Fatalf("idempotent DeleteEmbeddingChunksBySectionIDs returned error: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("idempotent deleted rows = %d, want 0", deleted)
	}

	remainingA, err := store.ListEmbeddingChunkInventory(ctx, EmbeddingInventoryOptions{SourceID: sourceA, IncludeAllPlans: true, Limit: 100})
	if err != nil {
		t.Fatalf("ListEmbeddingChunkInventory(source A after delete) returned error: %v", err)
	}
	if len(remainingA) != 1 || remainingA[0].SectionID != sectionB {
		t.Fatalf("source A remaining inventory = %+v, want only section B", remainingA)
	}
	remainingB, err := store.ListEmbeddingChunkInventory(ctx, EmbeddingInventoryOptions{SourceID: sourceB, IncludeAllPlans: true, Limit: 100})
	if err != nil {
		t.Fatalf("ListEmbeddingChunkInventory(source B after delete) returned error: %v", err)
	}
	if len(remainingB) != 1 || remainingB[0].SectionID != foreignSection {
		t.Fatalf("source B inventory = %+v, source-scoped delete removed foreign row", remainingB)
	}
}

func reconciliationIntegrationChunk(sourceID, sectionID, model, generatorVersion, tokenizer, strategy string, ordinal int) domain.EmbeddingChunkInput {
	textHash := "text-hash-" + generatorVersion
	return domain.EmbeddingChunkInput{
		ChunkID:            domainChunkID(sectionID, ordinal, model, generatorVersion, tokenizer, strategy, textHash),
		SectionID:          sectionID,
		DocumentID:         "document-" + sectionID,
		SourceID:           sourceID,
		ChunkOrdinal:       ordinal,
		ChunkTokenCount:    3,
		ChunkText:          "integration reconciliation chunk",
		Model:              model,
		Dimensions:         3,
		Embedding:          []float32{1, 0, 0},
		SectionContentHash: "content-hash-" + sectionID,
		ChunkTextHash:      textHash,
		Tokenizer:          tokenizer,
		ChunkStrategy:      strategy,
		GeneratorVersion:   generatorVersion,
	}
}
