package vectorstore

import (
	"context"
	"strings"
	"testing"
)

func TestRedactDSN(t *testing.T) {
	got := RedactDSN("pgvector://postgres:secret@example.test/doc?sslmode=disable")
	want := "pgvector://postgres:xxxxx@example.test/doc?sslmode=disable"
	if got != want {
		t.Fatalf("RedactDSN = %q, want %q", got, want)
	}
}

func TestRedactDSNPreservesUsernameOnlyDSN(t *testing.T) {
	got := RedactDSN("pgvector://postgres@example.test/doc")
	want := "pgvector://postgres@example.test/doc"
	if got != want {
		t.Fatalf("RedactDSN = %q, want %q", got, want)
	}
}

func TestOpenPGVectorChecksConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	store, err := OpenPGVector(ctx, "pgvector://postgres@example.test/doc?sslmode=disable")
	if err == nil {
		if store != nil {
			_ = store.Close()
		}
		t.Fatal("OpenPGVector returned nil error for canceled context")
	}
}

func TestANNIndexStatementUsesDimensionScopedHNSWExpression(t *testing.T) {
	stmt := annIndexStatement(768)
	for _, want := range []string{
		"idx_embedding_chunks_hnsw_cosine_d_768",
		"using hnsw",
		"(embedding::vector(768)) vector_cosine_ops",
		"where dimensions = 768",
	} {
		if !strings.Contains(stmt, want) {
			t.Fatalf("annIndexStatement = %q, want %q", stmt, want)
		}
	}
}

func TestChunkVectorSearchQueryMatchesDimensionScopedIndexExpression(t *testing.T) {
	query := chunkVectorSearchQuery(768)
	for _, want := range []string{
		"embedding::vector(768) <=> $1::vector(768)",
		"dimensions = $3",
		"order by (embedding::vector(768) <=> $1::vector(768))",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("chunkVectorSearchQuery = %q, want %q", query, want)
		}
	}
}

func TestNormalizeEmbeddingInventoryOptions(t *testing.T) {
	t.Run("source active plan", func(t *testing.T) {
		opts := normalizeEmbeddingInventoryOptions(EmbeddingInventoryOptions{
			SourceID:         " source-a ",
			Model:            " model-a ",
			GeneratorVersion: " generator-a ",
			Tokenizer:        " tokenizer-a ",
			ChunkStrategy:    " structural ",
			Limit:            25,
			Offset:           10,
		})
		if opts.SourceID != "source-a" || opts.Model != "model-a" || opts.GeneratorVersion != "generator-a" || opts.Tokenizer != "tokenizer-a" || opts.ChunkStrategy != "structural" {
			t.Fatalf("normalized options = %+v", opts)
		}
		if opts.Limit != 25 || opts.Offset != 10 {
			t.Fatalf("normalized pagination = limit %d offset %d", opts.Limit, opts.Offset)
		}
	})

	t.Run("all plans clears plan filters", func(t *testing.T) {
		opts := normalizeEmbeddingInventoryOptions(EmbeddingInventoryOptions{
			SourceID:         "source-a",
			Model:            "model-a",
			GeneratorVersion: "generator-a",
			Tokenizer:        "tokenizer-a",
			ChunkStrategy:    "structural",
			IncludeAllPlans:  true,
		})
		if opts.Model != "" || opts.GeneratorVersion != "" || opts.Tokenizer != "" || opts.ChunkStrategy != "" {
			t.Fatalf("all-plan options retained filters: %+v", opts)
		}
	})

	t.Run("global and default pagination", func(t *testing.T) {
		opts := normalizeEmbeddingInventoryOptions(EmbeddingInventoryOptions{Limit: 1001, Offset: -1})
		if opts.SourceID != "" {
			t.Fatalf("global source id = %q, want empty", opts.SourceID)
		}
		if opts.Limit != 200 || opts.Offset != 0 {
			t.Fatalf("normalized pagination = limit %d offset %d, want 200/0", opts.Limit, opts.Offset)
		}
	})
}

func TestUniqueEmbeddingSectionIDs(t *testing.T) {
	got := uniqueEmbeddingSectionIDs([]string{" section-a ", "", "section-b", "section-a", "  ", "section-b"})
	want := []string{"section-a", "section-b"}
	if len(got) != len(want) {
		t.Fatalf("unique ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unique ids = %v, want %v", got, want)
		}
	}
}

func TestSourceEmbeddingReconciliationQueriesKeepSourceBoundary(t *testing.T) {
	inventoryQuery, args := embeddingChunkInventoryQuery(normalizeEmbeddingInventoryOptions(EmbeddingInventoryOptions{
		SourceID:        "source-a",
		IncludeAllPlans: true,
		Limit:           10,
	}))
	inventoryQuery = strings.Join(strings.Fields(inventoryQuery), " ")
	if !strings.Contains(inventoryQuery, "where source_id = $1") {
		t.Fatalf("source inventory query lacks source predicate: %q", inventoryQuery)
	}
	if len(args) != 3 || args[0] != "source-a" || args[1] != 10 || args[2] != 0 {
		t.Fatalf("source inventory args = %#v, want source/limit/offset", args)
	}

	globalQuery, globalArgs := embeddingChunkInventoryQuery(normalizeEmbeddingInventoryOptions(EmbeddingInventoryOptions{
		Model:            "model-a",
		GeneratorVersion: "generator-a",
		Limit:            10,
	}))
	globalQuery = strings.Join(strings.Fields(globalQuery), " ")
	if strings.Contains(globalQuery, "source_id =") || !strings.Contains(globalQuery, "where model = $1 and generator_version = $2") {
		t.Fatalf("global inventory query has wrong filters: %q", globalQuery)
	}
	if len(globalArgs) != 4 || globalArgs[0] != "model-a" || globalArgs[1] != "generator-a" || globalArgs[2] != 10 || globalArgs[3] != 0 {
		t.Fatalf("global inventory args = %#v, want plan/limit/offset", globalArgs)
	}
	deleteQuery := strings.Join(strings.Fields(deleteEmbeddingChunksBySectionIDsQuery), " ")
	for _, want := range []string{"where source_id = $1", "section_id = any($2::text[])"} {
		if !strings.Contains(deleteQuery, want) {
			t.Fatalf("delete query = %q, want %q", deleteQuery, want)
		}
	}
}

func TestDeleteEmbeddingChunksBySectionIDsValidatesSourceAndEmptyBatch(t *testing.T) {
	store := &PGVectorStore{}
	if _, err := store.DeleteEmbeddingChunksBySectionIDs(context.Background(), " ", []string{"section-a"}); err == nil {
		t.Fatal("DeleteEmbeddingChunksBySectionIDs accepted an empty source id")
	}
	deleted, err := store.DeleteEmbeddingChunksBySectionIDs(context.Background(), "source-a", []string{"", " "})
	if err != nil {
		t.Fatalf("empty DeleteEmbeddingChunksBySectionIDs returned error: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("empty DeleteEmbeddingChunksBySectionIDs deleted %d rows, want 0", deleted)
	}
}
