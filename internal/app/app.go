package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/docgraph/docgraph/internal/blobstore"
	"github.com/docgraph/docgraph/internal/config"
	"github.com/docgraph/docgraph/internal/embedding"
	"github.com/docgraph/docgraph/internal/jobs"
	"github.com/docgraph/docgraph/internal/mcp"
	"github.com/docgraph/docgraph/internal/query"
	"github.com/docgraph/docgraph/internal/scheduler"
	"github.com/docgraph/docgraph/internal/server"
	"github.com/docgraph/docgraph/internal/storage"
	syncsvc "github.com/docgraph/docgraph/internal/sync"
	"github.com/docgraph/docgraph/internal/vectorstore"
)

const vectorBackendStartupTimeout = 10 * time.Second

const (
	blobGarbageCollectionInterval = time.Hour
	blobGarbageCollectionGrace    = 24 * time.Hour
)

var (
	openVectorBackend    = vectorstore.Open
	migrateVectorBackend = vectorstore.Migrate
)

func Init(ctx context.Context, cfg config.Config) error {
	return Migrate(ctx, cfg)
}

func Migrate(ctx context.Context, cfg config.Config) error {
	store, err := storage.Open(ctx, cfg.Storage.DSN)
	if err != nil {
		return err
	}
	defer store.Close()

	if err := store.Migrate(ctx); err != nil {
		return err
	}
	if cfg.VectorSearch.Enabled {
		if err := migrateVectorSearch(ctx, cfg); err != nil {
			return err
		}
	}
	return store.CheckSchema(ctx)
}

func Serve(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	store, err := storage.OpenExisting(ctx, cfg.Storage.DSN)
	if err != nil {
		return err
	}
	defer store.Close()

	if err := store.CheckSchema(ctx); err != nil {
		return err
	}
	blobs, err := blobstore.New(cfg.Server.DataDir)
	if err != nil {
		return fmt.Errorf("initialize BlobStore: %w", err)
	}

	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	embedder := embedding.NewNoOpEmbedder()
	generatorVersion := cfg.VectorSearch.Embedding.GeneratorVersion
	limits := embedding.ResolveEmbeddingLimits(cfg.VectorSearch.Embedding, embedding.ProviderCapabilities{})
	if cfg.VectorSearch.Enabled {
		configuredEmbedder, vectorBackend := configureVectorSearch(ctx, store, cfg, logger)
		if configuredEmbedder != nil && vectorBackend != nil {
			embedder = configuredEmbedder
			limits = embedding.ResolveEmbeddingLimitsForEmbedder(cfg.VectorSearch.Embedding, embedder)
			if setter, ok := store.(vectorstore.Setter); ok {
				setter.SetVectorBackend(vectorBackend)
			} else {
				_ = vectorBackend.Close()
			}
			if runtimeSetter, ok := store.(vectorstore.SearchRuntimeSetter); ok {
				runtimeSetter.SetVectorSearchRuntime(vectorstore.SearchRuntime{
					Embedder:         configuredEmbedder,
					SearchWeight:     cfg.VectorSearch.SearchWeight,
					VectorCandidates: 60,
					MinSimilarity:    0,
					GeneratorVersion: generatorVersion,
					Tokenizer:        cfg.VectorSearch.Embedding.Tokenizer,
					ChunkStrategy:    cfg.VectorSearch.Embedding.ChunkStrategy,
					IntentRouteWeights: vectorstore.IntentRouteWeights{
						EntityText:       cfg.VectorSearch.IntentRouteEntityText,
						EntityVector:     cfg.VectorSearch.IntentRouteEntityVector,
						ConceptualText:   cfg.VectorSearch.IntentRouteConceptualText,
						ConceptualVector: cfg.VectorSearch.IntentRouteConceptualVector,
						GeneralText:      cfg.VectorSearch.IntentRouteGeneralText,
						GeneralVector:    cfg.VectorSearch.IntentRouteGeneralVector,
						RRFK:             cfg.VectorSearch.RRFK,
					},
				})
			}
		}
	}
	runner := jobs.NewRunner(store, jobs.NewDefaultRegistryWithEmbedding(store, embedder, jobs.EmbeddingOptions{
		BatchSize:          limits.BatchSize,
		ContextTokens:      limits.ContextTokens,
		ChunkTargetTokens:  limits.ChunkTargetTokens,
		ChunkOverlapTokens: limits.ChunkOverlapTokens,
		MaxBatchTokens:     limits.MaxBatchTokens,
		Concurrency:        cfg.VectorSearch.Embedding.Concurrency,
		Limits:             limits,
		Tokenizer:          cfg.VectorSearch.Embedding.Tokenizer,
		ChunkStrategy:      cfg.VectorSearch.Embedding.ChunkStrategy,
		GeneratorVersion:   generatorVersion,
		SyncOptions: syncsvc.ServiceOptions{
			DataDir:   cfg.Server.DataDir,
			BlobStore: blobs,
		},
	}), logger)
	runner.SetWorkerCount(cfg.Server.JobWorkers)
	srv := server.NewWithAuthAndPrefixAndJobsAndEmbedding(addr, store, logger, cfg.Auth, cfg.Server.WebPrefix, runner, embedder, generatorVersion, limits.ChunkTargetTokens, cfg.VectorSearch.SearchWeight)
	srv.SetEmbeddingChunkOptions(cfg.VectorSearch.Embedding.Tokenizer, cfg.VectorSearch.Embedding.ChunkStrategy)
	srv.SetEmbeddingLimits(limits)
	srv.SetBlobStore(blobs)
	go runner.Run(ctx)
	go scheduler.NewRunner(store, logger).Run(ctx)
	go runBlobGarbageCollector(ctx, store, blobs, logger)
	return srv.Run(ctx)
}

func runBlobGarbageCollector(ctx context.Context, store storage.Store, blobs *blobstore.Store, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	collect := func() {
		mediaStore, ok := store.(storage.MediaStore)
		if !ok || blobs == nil {
			return
		}
		cutoff := time.Now().Add(-blobGarbageCollectionGrace)
		result, err := blobs.CollectGarbage(ctx, cutoff, mediaStore.IsMediaBlobReferenced)
		if err != nil {
			if ctx.Err() == nil {
				logger.Warn("Blob garbage collection deferred", "error", err)
			}
			return
		}
		cutoffText := cutoff.UTC().Format("2006-01-02 15:04:05")
		for _, digest := range result.DeletedDigests {
			if _, err := mediaStore.DeleteMediaBlobIfUnreferenced(ctx, digest, cutoffText); err != nil && ctx.Err() == nil {
				logger.Warn("Blob metadata garbage collection deferred", "sha256", digest, "error", err)
			}
		}
		if candidates, err := mediaStore.ListUnreferencedMediaBlobs(ctx, cutoffText, 1000); err == nil {
			for _, candidate := range candidates {
				if _, err := mediaStore.DeleteMediaBlobIfUnreferenced(ctx, candidate.SHA256, cutoffText); err != nil && ctx.Err() == nil {
					logger.Warn("orphan Blob metadata cleanup deferred", "sha256", candidate.SHA256, "error", err)
				}
			}
		} else if ctx.Err() == nil {
			logger.Warn("orphan Blob metadata scan deferred", "error", err)
		}
		if _, err := blobs.CleanupStaging(cutoff); err != nil && ctx.Err() == nil {
			logger.Warn("Blob staging cleanup deferred", "error", err)
		}
	}
	collect()
	ticker := time.NewTicker(blobGarbageCollectionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			collect()
		}
	}
}

func Status(ctx context.Context, cfg config.Config) (storage.Status, error) {
	store, err := storage.OpenExisting(ctx, cfg.Storage.DSN)
	if err != nil {
		return storage.Status{}, err
	}
	defer store.Close()

	if err := store.CheckSchema(ctx); err != nil {
		return storage.Status{}, err
	}
	return store.Status(ctx)
}

func MCP(ctx context.Context, cfg config.Config, in io.Reader, out io.Writer) error {
	store, err := storage.OpenExisting(ctx, cfg.Storage.DSN)
	if err != nil {
		return err
	}
	defer store.Close()

	if err := store.CheckSchema(ctx); err != nil {
		return err
	}
	embedder := embedding.NewNoOpEmbedder()
	generatorVersion := cfg.VectorSearch.Embedding.GeneratorVersion
	if cfg.VectorSearch.Enabled {
		configuredEmbedder, vectorBackend := configureVectorSearch(ctx, store, cfg, nil)
		if configuredEmbedder != nil && vectorBackend != nil {
			embedder = configuredEmbedder
			if setter, ok := store.(vectorstore.Setter); ok {
				setter.SetVectorBackend(vectorBackend)
			} else {
				_ = vectorBackend.Close()
			}
			if runtimeSetter, ok := store.(vectorstore.SearchRuntimeSetter); ok {
				runtimeSetter.SetVectorSearchRuntime(vectorstore.SearchRuntime{
					Embedder:         configuredEmbedder,
					SearchWeight:     cfg.VectorSearch.SearchWeight,
					VectorCandidates: 60,
					MinSimilarity:    0,
					GeneratorVersion: generatorVersion,
					Tokenizer:        cfg.VectorSearch.Embedding.Tokenizer,
					ChunkStrategy:    cfg.VectorSearch.Embedding.ChunkStrategy,
					IntentRouteWeights: vectorstore.IntentRouteWeights{
						EntityText:       cfg.VectorSearch.IntentRouteEntityText,
						EntityVector:     cfg.VectorSearch.IntentRouteEntityVector,
						ConceptualText:   cfg.VectorSearch.IntentRouteConceptualText,
						ConceptualVector: cfg.VectorSearch.IntentRouteConceptualVector,
						GeneralText:      cfg.VectorSearch.IntentRouteGeneralText,
						GeneralVector:    cfg.VectorSearch.IntentRouteGeneralVector,
						RRFK:             cfg.VectorSearch.RRFK,
					},
				})
			}
		}
	}
	mcpServer := mcp.NewServerWithStoreAndEmbeddingAndPlan(query.NewService(store), store, embedder, generatorVersion, cfg.VectorSearch.SearchWeight, cfg.VectorSearch.Embedding.Tokenizer, cfg.VectorSearch.Embedding.ChunkStrategy, in, out)
	mcpServer.SetAssetURIBasePath(cfg.Server.WebPrefix)
	return mcpServer.Run(ctx)
}

func configureVectorSearch(ctx context.Context, _ storage.Store, cfg config.Config, logger *slog.Logger) (embedding.Embedder, vectorstore.Store) {
	embedder, err := embedding.NewEmbedder(cfg.VectorSearch.Embedding)
	if err != nil || embedder.Model() == "" {
		if logger != nil {
			logger.Warn("vector search disabled: embedding is not configured", "error", err)
		}
		return nil, nil
	}
	if prober, ok := embedder.(interface {
		Probe(context.Context) (embedding.ProviderCapabilities, error)
	}); ok && cfg.VectorSearch.Embedding.Dimensions <= 0 {
		probeCtx, cancel := context.WithTimeout(ctx, vectorBackendStartupTimeout)
		caps, probeErr := prober.Probe(probeCtx)
		cancel()
		if probeErr != nil || caps.Dimensions <= 0 {
			if logger != nil {
				logger.Warn("vector search disabled: embedding capability probe failed", "model", embedder.Model(), "error", probeErr)
			}
			return nil, nil
		}
		if logger != nil {
			logger.Info("embedding provider probed", "model", caps.Model, "dimensions", caps.Dimensions)
		}
	}
	if logger != nil {
		logger.Info("configuring vector search", "dsn", vectorstore.RedactDSN(cfg.VectorSearch.VectorDB.DSN))
	}
	startupCtx, cancel := context.WithTimeout(ctx, vectorBackendStartupTimeout)
	defer cancel()
	backend, err := openVectorBackend(startupCtx, cfg.VectorSearch.VectorDB.DSN)
	if err != nil || backend == nil {
		if logger != nil {
			logger.Warn("vector search disabled: vector database is not configured", "dsn", vectorstore.RedactDSN(cfg.VectorSearch.VectorDB.DSN), "error", err)
		}
		return nil, nil
	}
	if validator, ok := backend.(vectorstore.DimensionValidator); ok && embedder.Dimensions() > 0 {
		if err := validator.ValidateModelDimensions(startupCtx, embedder.Model(), embedder.Dimensions()); err != nil {
			if logger != nil {
				logger.Warn("vector search disabled: vector dimension validation failed", "model", embedder.Model(), "dimensions", embedder.Dimensions(), "error", err)
			}
			_ = backend.Close()
			return nil, nil
		}
	}
	if indexer, ok := backend.(vectorstore.ANNIndexer); ok && embedder.Dimensions() > 0 {
		if err := indexer.EnsureANNIndex(startupCtx, embedder.Model(), embedder.Dimensions()); err != nil {
			if logger != nil {
				logger.Warn("pgvector ANN index was not created; vector search will continue without ANN index", "model", embedder.Model(), "dimensions", embedder.Dimensions(), "error", err)
			}
		}
	}
	return embedder, backend
}

func migrateVectorSearch(ctx context.Context, cfg config.Config) error {
	startupCtx, cancel := context.WithTimeout(ctx, vectorBackendStartupTimeout)
	defer cancel()
	if err := migrateVectorBackend(startupCtx, cfg.VectorSearch.VectorDB.DSN); err != nil {
		return fmt.Errorf("migrate vector search backend: %w", err)
	}
	return nil
}
