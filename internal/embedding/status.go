package embedding

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/docgraph/docgraph/internal/domain"
	"github.com/docgraph/docgraph/internal/embeddingchunk"
	"github.com/docgraph/docgraph/internal/storage"
)

// AuditSourceEmbeddingStatus compares the authoritative current Section
// snapshot with stored chunk metadata. It performs no writes.
func AuditSourceEmbeddingStatus(ctx context.Context, store storage.Store, sourceID string, model string, opts EnsureOptions) (domain.EmbeddingStatus, error) {
	if store == nil {
		return domain.EmbeddingStatus{}, fmt.Errorf("store is required")
	}
	sourceID = strings.TrimSpace(sourceID)
	model = strings.TrimSpace(model)
	if sourceID == "" {
		return domain.EmbeddingStatus{}, fmt.Errorf("source id is required")
	}
	status := domain.EmbeddingStatus{SourceID: sourceID, Model: model}
	totalSections, err := store.CountSectionsForEmbedding(ctx, sourceID)
	if err != nil {
		return domain.EmbeddingStatus{}, err
	}
	status.TotalSections = totalSections
	if model == "" {
		status.Status = "disabled"
		status.Reason = "embedding_not_configured"
		status.PendingSections = totalSections
		return status, nil
	}

	limits := normalizeEnsureLimits(opts)
	generatorVersion := strings.TrimSpace(opts.GeneratorVersion)
	if generatorVersion == "" {
		generatorVersion = DefaultGeneratorVersion
	}
	measurer := NewTextMeasurer(opts.Tokenizer, model)
	_, strategyInfo, err := embeddingchunk.ResolveChunkStrategy(opts.ChunkStrategy)
	if err != nil {
		return domain.EmbeddingStatus{}, err
	}
	chunker := Chunker{
		Measurer:           measurer,
		Strategy:           strategyInfo.Name,
		ContextTokens:      limits.ContextTokens,
		ChunkTargetTokens:  limits.ChunkTargetTokens,
		ChunkOverlapTokens: limits.ChunkOverlapTokens,
	}.Normalized()
	plan := EmbeddingPlan{
		Model:            model,
		GeneratorVersion: generatorVersion,
		Tokenizer:        measurer.Name(),
		ChunkStrategy:    strategyInfo.Name,
	}

	inventory, err := loadEmbeddingInventory(ctx, store, sourceID, model)
	if err != nil {
		return domain.EmbeddingStatus{}, fmt.Errorf("load source embedding inventory: %w", err)
	}
	current := make([]ExpectedSectionEmbedding, 0, totalSections)
	for offset := 0; ; {
		sections, err := store.ListSectionsForEmbedding(ctx, sourceID, embeddingInventoryPageSize, offset)
		if err != nil {
			return domain.EmbeddingStatus{}, err
		}
		for _, section := range sections {
			chunks, err := chunker.BuildChunks(section, model, generatorVersion)
			if err != nil {
				return domain.EmbeddingStatus{}, err
			}
			current = append(current, ExpectedSectionEmbedding{Section: section, Chunks: chunks})
		}
		if len(sections) < embeddingInventoryPageSize {
			break
		}
		offset += len(sections)
	}

	reconciliation := EvaluateEmbeddingReconciliation(current, inventory.Hashes, plan)
	status.Enabled = true
	status.Backend = "vector"
	if inventory.Reconciler != nil {
		status.Backend = "pgvector"
	}
	status.GeneratorVersion = generatorVersion
	status.Tokenizer = plan.Tokenizer
	status.ChunkStrategy = plan.ChunkStrategy
	status.ReadySections = len(reconciliation.ReadySectionIDs)
	status.EmbeddedSections = status.ReadySections
	status.PendingSections = len(reconciliation.PendingSectionIDs)
	status.StaleSections = len(reconciliation.StaleSectionIDs)
	status.OrphanSections = len(reconciliation.OrphanSectionIDs)
	status.OrphanChunks = reconciliation.OrphanChunkCount
	status.CleanupRequired = status.OrphanSections > 0 || status.OrphanChunks > 0
	status.AuditedAt = time.Now().UTC().Format(time.RFC3339)
	for _, evaluation := range reconciliation.Sections {
		status.ExpectedChunks += evaluation.ExpectedChunks
		status.ReadyChunks += evaluation.CurrentExpectedChunks
		switch evaluation.State {
		case SectionEmbeddingPending:
			status.PendingChunks += evaluation.MissingExpectedChunks
		case SectionEmbeddingStale:
			// A replaced chunk is both a missing expected chunk and an
			// unexpected stored chunk. Count that repair once while still
			// surfacing genuinely extra stored chunks.
			status.StaleChunks += max(evaluation.MissingExpectedChunks, evaluation.UnexpectedActiveChunks)
		}
	}
	status.TotalChunks = status.ExpectedChunks
	status.EmbeddedChunks = status.ReadyChunks

	switch {
	case status.PendingSections > 0:
		status.Status = "indexing"
	case status.StaleSections > 0:
		status.Status = "stale"
	case status.CleanupRequired:
		status.Status = "cleanup_required"
	default:
		status.Status = "ready"
	}
	return status, nil
}
