package vectorstore

import (
	"context"
	"os"
	"testing"
	"time"
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
