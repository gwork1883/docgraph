package jobs

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

func TestRunnerRunsJobsConcurrently(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := sqlite.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "docgraph.db"))
	if err != nil {
		t.Fatalf("sqlite.Open returned error: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := store.CreateJob(ctx, domain.JobInput{Kind: "parallel_job"}); err != nil {
			t.Fatalf("CreateJob %d returned error: %v", i, err)
		}
	}

	var active int32
	var completed int32
	var once sync.Once
	bothRunning := make(chan struct{})
	release := make(chan struct{})
	registry := NewRegistry()
	registry.Register("parallel_job", func(ctx context.Context, job domain.Job) error {
		if atomic.AddInt32(&active, 1) == 2 {
			once.Do(func() { close(bothRunning) })
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
		}
		atomic.AddInt32(&active, -1)
		atomic.AddInt32(&completed, 1)
		return store.CompleteJob(ctx, job.ID, `{"ok":true}`)
	})
	runner := NewRunner(store, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.SetWorkerCount(2)
	done := make(chan struct{})
	go func() {
		runner.Run(ctx)
		close(done)
	}()
	select {
	case <-bothRunning:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not run two jobs concurrently")
	}
	close(release)
	for deadline := time.After(2 * time.Second); atomic.LoadInt32(&completed) < 2; {
		select {
		case <-deadline:
			t.Fatalf("completed jobs = %d, want 2", completed)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not stop after context cancel")
	}
}

func TestRunnerCancelJobCancelsHandlerContext(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "docgraph.db"))
	if err != nil {
		t.Fatalf("sqlite.Open returned error: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}
	job, err := store.CreateJob(ctx, domain.JobInput{Kind: "cancel_job"})
	if err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}

	started := make(chan struct{})
	done := make(chan struct{})
	registry := NewRegistry()
	registry.Register("cancel_job", func(ctx context.Context, job domain.Job) error {
		close(started)
		<-ctx.Done()
		close(done)
		return ctx.Err()
	})
	runner := NewRunner(store, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
	go runner.RunOnce(ctx)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not start")
	}
	if _, err := runner.CancelJob(ctx, job.ID, "stop test"); err != nil {
		t.Fatalf("CancelJob returned error: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler context was not canceled")
	}
	for deadline := time.After(2 * time.Second); ; {
		canceled, err := store.GetJob(ctx, job.ID)
		if err != nil {
			t.Fatalf("GetJob returned error: %v", err)
		}
		if canceled.Status == "canceled" {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("job status = %q, want canceled", canceled.Status)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}
