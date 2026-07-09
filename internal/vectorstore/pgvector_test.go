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
