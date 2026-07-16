package scheduler

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/docgraph/docgraph/internal/domain"
	"github.com/docgraph/docgraph/internal/storage"
	"github.com/docgraph/docgraph/internal/storage/sqlite"
)

func TestRunOnceSyncsDueSource(t *testing.T) {
	ctx := context.Background()
	store := openSchedulerStore(t, ctx)
	docsDir := t.TempDir()
	writeSchedulerFile(t, filepath.Join(docsDir, "guide.md"), "# Guide\n\nscheduled sync content.\n")
	_, err := store.CreateSource(ctx, domain.Source{
		ID:           "src_due",
		Kind:         "local",
		Name:         "Due Docs",
		DSN:          docsDir,
		SyncSchedule: "hourly",
	})
	if err != nil {
		t.Fatalf("CreateSource returned error: %v", err)
	}

	newTestRunner(store).RunOnce(ctx)

	jobs, err := store.ListSyncJobs(ctx, "src_due", 10)
	if err != nil {
		t.Fatalf("ListSyncJobs returned error: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Status != "queued" {
		t.Fatalf("jobs = %+v, want one queued scheduled sync", jobs)
	}
}

func TestRunOnceSkipsPausedAndRunningSources(t *testing.T) {
	ctx := context.Background()
	store := openSchedulerStore(t, ctx)
	docsDir := t.TempDir()
	writeSchedulerFile(t, filepath.Join(docsDir, "guide.md"), "# Guide\n\nscheduled skip content.\n")
	for _, source := range []domain.Source{
		{ID: "src_paused", Kind: "local", Name: "Paused Docs", DSN: docsDir, SyncSchedule: "hourly"},
		{ID: "src_running", Kind: "local", Name: "Running Docs", DSN: docsDir, SyncSchedule: "hourly"},
	} {
		if _, err := store.CreateSource(ctx, source); err != nil {
			t.Fatalf("CreateSource(%s) returned error: %v", source.ID, err)
		}
	}
	if err := store.UpdateSourceSyncState(ctx, "src_paused", "paused", "credential_required"); err != nil {
		t.Fatalf("UpdateSourceSyncState returned error: %v", err)
	}
	if _, err := store.CreateSyncJob(ctx, "src_running"); err != nil {
		t.Fatalf("CreateSyncJob returned error: %v", err)
	}

	newTestRunner(store).RunOnce(ctx)

	pausedJobs, err := store.ListSyncJobs(ctx, "src_paused", 10)
	if err != nil {
		t.Fatalf("ListSyncJobs paused returned error: %v", err)
	}
	if len(pausedJobs) != 0 {
		t.Fatalf("paused jobs = %+v, want none", pausedJobs)
	}
	runningJobs, err := store.ListSyncJobs(ctx, "src_running", 10)
	if err != nil {
		t.Fatalf("ListSyncJobs running returned error: %v", err)
	}
	if len(runningJobs) != 1 || runningJobs[0].Status != "running" {
		t.Fatalf("running jobs = %+v, want only original running job", runningJobs)
	}
}

func TestRunOnceLoadsLatestSyncJobsInOneBatch(t *testing.T) {
	ctx := context.Background()
	store := openSchedulerStore(t, ctx)
	docsDir := t.TempDir()
	for _, source := range []domain.Source{
		{ID: "src_batch_due_a", Kind: "local", Name: "Batch Due A", DSN: docsDir, SyncSchedule: "hourly"},
		{ID: "src_batch_due_b", Kind: "local", Name: "Batch Due B", DSN: docsDir, SyncSchedule: "daily"},
	} {
		if _, err := store.CreateSource(ctx, source); err != nil {
			t.Fatalf("CreateSource(%s) returned error: %v", source.ID, err)
		}
	}
	countingStore := &syncJobCountingStore{Store: store}
	newTestRunner(countingStore).RunOnce(ctx)
	if countingStore.listLatestCalls != 1 || countingStore.listCalls != 0 {
		t.Fatalf("sync job reads = latest:%d list:%d, want one batch latest and no per-source lists", countingStore.listLatestCalls, countingStore.listCalls)
	}
}

type syncJobCountingStore struct {
	storage.Store
	listCalls       int
	listLatestCalls int
}

func (s *syncJobCountingStore) ListSyncJobs(ctx context.Context, sourceID string, limit int) ([]storage.SyncJob, error) {
	s.listCalls++
	return s.Store.ListSyncJobs(ctx, sourceID, limit)
}

func (s *syncJobCountingStore) ListLatestSyncJobs(ctx context.Context) ([]storage.SyncJob, error) {
	s.listLatestCalls++
	return s.Store.ListLatestSyncJobs(ctx)
}

func openSchedulerStore(t *testing.T, ctx context.Context) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "docgraph.db"))
	if err != nil {
		t.Fatalf("sqlite.Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close returned error: %v", err)
		}
	})
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}
	return store
}

func newTestRunner(store storage.Store) *Runner {
	runner := NewRunner(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.now = func() time.Time { return time.Now().Add(time.Hour) }
	return runner
}

func writeSchedulerFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
}
