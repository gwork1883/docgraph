package embedding

import (
	"context"

	"github.com/docgraph/docgraph/internal/domain"
	"github.com/docgraph/docgraph/internal/storage"
	"github.com/docgraph/docgraph/internal/vectorstore"
)

const embeddingInventoryPageSize = 500

type embeddingInventorySnapshot struct {
	Hashes     []domain.EmbeddingChunkHash
	ByChunkID  map[string]domain.EmbeddingChunkHash
	Reconciler storage.SourceEmbeddingReconcileStore
}

func loadEmbeddingInventory(ctx context.Context, store storage.Store, sourceID string, model string) (embeddingInventorySnapshot, error) {
	snapshot := embeddingInventorySnapshot{ByChunkID: map[string]domain.EmbeddingChunkHash{}}
	if reconciler, ok := store.(storage.SourceEmbeddingReconcileStore); ok && reconciler.SupportsSourceEmbeddingReconciliation() {
		snapshot.Reconciler = reconciler
		for offset := 0; ; {
			page, err := reconciler.ListEmbeddingChunkInventory(ctx, vectorstore.EmbeddingInventoryOptions{
				SourceID:        sourceID,
				Model:           model,
				IncludeAllPlans: true,
				Limit:           embeddingInventoryPageSize,
				Offset:          offset,
			})
			if err != nil {
				return embeddingInventorySnapshot{}, err
			}
			for _, hash := range page {
				snapshot.Hashes = append(snapshot.Hashes, hash)
				snapshot.ByChunkID[hash.ChunkID] = hash
			}
			if len(page) < embeddingInventoryPageSize {
				return snapshot, nil
			}
			offset += len(page)
		}
	}

	for offset := 0; ; {
		page, err := store.ListEmbeddingChunkHashes(ctx, model, embeddingInventoryPageSize, offset)
		if err != nil {
			return embeddingInventorySnapshot{}, err
		}
		for _, hash := range page {
			if sourceID != "" && hash.SourceID != sourceID {
				continue
			}
			snapshot.Hashes = append(snapshot.Hashes, hash)
			snapshot.ByChunkID[hash.ChunkID] = hash
		}
		if len(page) < embeddingInventoryPageSize {
			return snapshot, nil
		}
		offset += len(page)
	}
}
