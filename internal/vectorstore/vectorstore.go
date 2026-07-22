package vectorstore

import (
	"context"
	"fmt"
	"strings"

	"github.com/docgraph/docgraph/internal/domain"
)

type Store interface {
	UpsertSectionEmbedding(ctx context.Context, input domain.SectionEmbeddingInput) error
	GetSectionEmbedding(ctx context.Context, sectionID string, model string) (domain.VectorSearchHit, []float32, error)
	DeleteSectionEmbeddings(ctx context.Context, sectionID string) error
	SearchSectionsByVector(ctx context.Context, embedding []float32, model string, limit int, minSimilarity float64, plan EmbeddingPlanFilter) ([]domain.VectorSearchHit, error)
	ListSectionEmbeddingHashes(ctx context.Context, model string, limit, offset int) ([]domain.SectionEmbeddingHash, error)
	UpsertEmbeddingChunk(ctx context.Context, input domain.EmbeddingChunkInput) error
	GetEmbeddingChunk(ctx context.Context, chunkID string, model string) (domain.VectorSearchHit, []float32, error)
	DeleteEmbeddingChunksBySection(ctx context.Context, sectionID string, model string, generatorVersion string, tokenizer string, chunkStrategy string) error
	SearchChunksByVector(ctx context.Context, embedding []float32, model string, limit int, minSimilarity float64, plan EmbeddingPlanFilter) ([]domain.VectorSearchHit, error)
	ListEmbeddingChunkHashes(ctx context.Context, model string, limit, offset int) ([]domain.EmbeddingChunkHash, error)
	GetEmbeddingCoverage(ctx context.Context, sourceID string, model string, generatorVersion string, tokenizer string, chunkStrategy string) (domain.EmbeddingCoverage, error)
	Close() error
}

// SourceEmbeddingReconciler exposes inventory and source-scoped cleanup without
// making reconciliation support mandatory for every Store implementation.
// Callers should discover it with a type assertion. An empty inventory SourceID
// requests global pagination; deletion always requires a non-empty source ID.
type SourceEmbeddingReconciler interface {
	ListEmbeddingChunkInventory(ctx context.Context, opts EmbeddingInventoryOptions) ([]domain.EmbeddingChunkHash, error)
	DeleteEmbeddingChunksBySectionIDs(ctx context.Context, sourceID string, sectionIDs []string) (deleted int64, err error)
}

type EmbeddingInventoryOptions struct {
	SourceID         string
	Model            string
	GeneratorVersion string
	Tokenizer        string
	ChunkStrategy    string
	IncludeAllPlans  bool
	Limit            int
	Offset           int
}

type EmbeddingPlanFilter struct {
	GeneratorVersion string
	Tokenizer        string
	ChunkStrategy    string
}

type Setter interface {
	SetVectorBackend(Store)
}

type Migrator interface {
	Migrate(ctx context.Context) error
}

type DimensionValidator interface {
	ValidateModelDimensions(ctx context.Context, model string, dimensions int) error
}

type ANNIndexer interface {
	EnsureANNIndex(ctx context.Context, model string, dimensions int) error
}

type QueryEmbedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Model() string
}

type SearchRuntime struct {
	Embedder           QueryEmbedder
	SearchWeight       float64
	VectorCandidates   int
	MinSimilarity      float64
	GeneratorVersion   string
	Tokenizer          string
	ChunkStrategy      string
	IntentRouteWeights IntentRouteWeights
}

type IntentRouteWeights struct {
	EntityText       float64
	EntityVector     float64
	ConceptualText   float64
	ConceptualVector float64
	GeneralText      float64
	GeneralVector    float64
	RRFK             float64
}

type SearchRuntimeSetter interface {
	SetVectorSearchRuntime(SearchRuntime)
}

func Open(ctx context.Context, dsn string) (Store, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" || dsn == "none://" {
		return nil, nil
	}
	scheme := dsn
	if i := strings.Index(scheme, "://"); i >= 0 {
		scheme = scheme[:i]
	}
	switch scheme {
	case "pgvector", "postgres", "postgresql":
		return OpenPGVector(ctx, dsn)
	default:
		return nil, fmt.Errorf("unsupported vector backend scheme %q", scheme)
	}
}

func Migrate(ctx context.Context, dsn string) error {
	store, err := Open(ctx, dsn)
	if err != nil {
		return err
	}
	if store == nil {
		return nil
	}
	defer store.Close()
	migrator, ok := store.(Migrator)
	if !ok {
		return nil
	}
	return migrator.Migrate(ctx)
}
