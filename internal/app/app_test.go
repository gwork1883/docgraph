package app

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docgraph/docgraph/internal/config"
	"github.com/docgraph/docgraph/internal/domain"
	"github.com/docgraph/docgraph/internal/vectorstore"
)

func TestMigrateMigratesVectorBackendWhenEnabled(t *testing.T) {
	oldMigrateVectorBackend := migrateVectorBackend
	t.Cleanup(func() {
		migrateVectorBackend = oldMigrateVectorBackend
	})

	var called bool
	migrateVectorBackend = func(ctx context.Context, dsn string) error {
		called = true
		if dsn != "pgvector://postgres:secret@example.test/doc" {
			t.Fatalf("vector dsn = %q, want configured dsn", dsn)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("vector migration context has no deadline")
		}
		return nil
	}

	cfg := config.Default()
	cfg.Storage.DSN = "sqlite://" + filepath.ToSlash(filepath.Join(t.TempDir(), "docgraph.db"))
	cfg.VectorSearch.Enabled = true
	cfg.VectorSearch.VectorDB.DSN = "pgvector://postgres:secret@example.test/doc"

	if err := Migrate(context.Background(), cfg); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}
	if !called {
		t.Fatal("vector migration was not called")
	}
}

func TestConfigureVectorSearchUsesTimeoutAndRedactedLogs(t *testing.T) {
	oldOpenVectorBackend := openVectorBackend
	t.Cleanup(func() {
		openVectorBackend = oldOpenVectorBackend
	})

	var gotDeadline bool
	openVectorBackend = func(ctx context.Context, dsn string) (vectorstore.Store, error) {
		if dsn != "pgvector://postgres:secret@example.test/doc" {
			t.Fatalf("vector dsn = %q, want configured dsn", dsn)
		}
		deadline, ok := ctx.Deadline()
		gotDeadline = ok && time.Until(deadline) <= vectorBackendStartupTimeout
		return fakeVectorStore{}, nil
	}

	cfg := config.Default()
	cfg.VectorSearch.Enabled = true
	cfg.VectorSearch.Embedding.Provider = "openai-compatible"
	cfg.VectorSearch.Embedding.Model = "test-embedding"
	cfg.VectorSearch.Embedding.Dimensions = 2
	cfg.VectorSearch.VectorDB.DSN = "pgvector://postgres:secret@example.test/doc"

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	_, backend := configureVectorSearch(context.Background(), nil, cfg, logger)
	if backend == nil {
		t.Fatal("backend = nil, want configured backend")
	}
	if !gotDeadline {
		t.Fatal("vector backend open context did not include startup timeout")
	}
	logText := logs.String()
	if strings.Contains(logText, "secret") {
		t.Fatalf("logs contain unredacted password: %s", logText)
	}
	if !strings.Contains(logText, "xxxxx") {
		t.Fatalf("logs = %q, want redacted password marker", logText)
	}
}

func TestConfigureVectorSearchWarnsWithRedactedDSNWhenBackendUnavailable(t *testing.T) {
	oldOpenVectorBackend := openVectorBackend
	t.Cleanup(func() {
		openVectorBackend = oldOpenVectorBackend
	})

	openVectorBackend = func(ctx context.Context, dsn string) (vectorstore.Store, error) {
		return nil, errors.New("dial timeout")
	}

	cfg := config.Default()
	cfg.VectorSearch.Enabled = true
	cfg.VectorSearch.Embedding.Provider = "openai-compatible"
	cfg.VectorSearch.Embedding.Model = "test-embedding"
	cfg.VectorSearch.Embedding.Dimensions = 2
	cfg.VectorSearch.VectorDB.DSN = "pgvector://postgres:secret@example.test/doc"

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	_, backend := configureVectorSearch(context.Background(), nil, cfg, logger)
	if backend != nil {
		t.Fatal("backend configured after open failure")
	}
	logText := logs.String()
	if !strings.Contains(logText, "vector search disabled: vector database is not configured") {
		t.Fatalf("logs = %q, want vector backend warning", logText)
	}
	if strings.Contains(logText, "secret") {
		t.Fatalf("logs contain unredacted password: %s", logText)
	}
	if !strings.Contains(logText, "xxxxx") {
		t.Fatalf("logs = %q, want redacted password marker", logText)
	}
}

type fakeVectorStore struct{}

func (fakeVectorStore) UpsertSectionEmbedding(ctx context.Context, input domain.SectionEmbeddingInput) error {
	return nil
}

func (fakeVectorStore) GetSectionEmbedding(ctx context.Context, sectionID string, model string) (domain.VectorSearchHit, []float32, error) {
	return domain.VectorSearchHit{}, nil, sql.ErrNoRows
}

func (fakeVectorStore) DeleteSectionEmbeddings(ctx context.Context, sectionID string) error {
	return nil
}

func (fakeVectorStore) SearchSectionsByVector(ctx context.Context, embedding []float32, model string, limit int, minSimilarity float64, plan vectorstore.EmbeddingPlanFilter) ([]domain.VectorSearchHit, error) {
	return nil, nil
}

func (fakeVectorStore) ListSectionEmbeddingHashes(ctx context.Context, model string, limit, offset int) ([]domain.SectionEmbeddingHash, error) {
	return nil, nil
}

func (fakeVectorStore) UpsertEmbeddingChunk(ctx context.Context, input domain.EmbeddingChunkInput) error {
	return nil
}

func (fakeVectorStore) GetEmbeddingChunk(ctx context.Context, chunkID string, model string) (domain.VectorSearchHit, []float32, error) {
	return domain.VectorSearchHit{}, nil, sql.ErrNoRows
}

func (fakeVectorStore) DeleteEmbeddingChunksBySection(ctx context.Context, sectionID string, model string, generatorVersion string, tokenizer string, chunkStrategy string) error {
	return nil
}

func (fakeVectorStore) SearchChunksByVector(ctx context.Context, embedding []float32, model string, limit int, minSimilarity float64, plan vectorstore.EmbeddingPlanFilter) ([]domain.VectorSearchHit, error) {
	return nil, nil
}

func (fakeVectorStore) ListEmbeddingChunkHashes(ctx context.Context, model string, limit, offset int) ([]domain.EmbeddingChunkHash, error) {
	return nil, nil
}

func (fakeVectorStore) GetEmbeddingCoverage(ctx context.Context, sourceID string, model string, generatorVersion string, tokenizer string, chunkStrategy string) (domain.EmbeddingCoverage, error) {
	return domain.EmbeddingCoverage{}, nil
}

func (fakeVectorStore) Close() error {
	return nil
}
