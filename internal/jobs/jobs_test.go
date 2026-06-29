package jobs

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/docgraph/docgraph/internal/domain"
	"github.com/docgraph/docgraph/internal/storage/sqlite"
)

func TestRunnerClaimsAndRunsRegisteredHandler(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "docgraph.db"))
	if err != nil {
		t.Fatalf("sqlite.Open returned error: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}
	job, err := store.CreateJob(ctx, domain.JobInput{Kind: "test_job", PayloadJSON: `{"ok":true}`})
	if err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}

	registry := NewRegistry()
	registry.Register("test_job", func(ctx context.Context, job domain.Job) error {
		return store.CompleteJob(ctx, job.ID, `{"handled":true}`)
	})
	runner := NewRunner(store, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !runner.RunOnce(ctx) {
		t.Fatal("RunOnce returned false, want handled job")
	}
	if runner.RunOnce(ctx) {
		t.Fatal("second RunOnce returned true, want no due jobs")
	}
	handled, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if handled.Status != "completed" || handled.ResultJSON != `{"handled":true}` || handled.Attempts != 1 {
		t.Fatalf("handled job = %+v, want completed handled job", handled)
	}
}
