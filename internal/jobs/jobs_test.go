package jobs

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docgraph/docgraph/internal/domain"
	"github.com/docgraph/docgraph/internal/embedding"
	"github.com/docgraph/docgraph/internal/storage/sqlite"
	"github.com/docgraph/docgraph/internal/vectorstore"
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

func TestEmbeddingEnsureJobRecordsProgressResultAndSkipsCurrentEmbeddings(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "docgraph.db"))
	if err != nil {
		t.Fatalf("sqlite.Open returned error: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}
	backend := newJobsFakeVectorBackend()
	store.SetVectorBackend(backend)
	source, err := store.CreateSource(ctx, domain.Source{ID: "source-embedding-job", Kind: "local", Name: "Docs", DSN: "file:///docs"})
	if err != nil {
		t.Fatalf("CreateSource returned error: %v", err)
	}
	if err := store.ReplaceDocument(ctx, domain.DocumentInput{ID: "doc-embedding-job", SourceID: source.ID, ExternalID: "doc.md", Title: "Doc", ContentHash: "hash-doc"}, []domain.SectionInput{
		{ID: "section-embedding-job-1", Content: "first section", ContentHash: "hash-section-1"},
		{ID: "section-embedding-job-2", Content: "second section", ContentHash: "hash-section-2"},
	}); err != nil {
		t.Fatalf("ReplaceDocument returned error: %v", err)
	}
	backend.hashes["orphan-chunk"] = domain.EmbeddingChunkHash{
		ChunkID:            "orphan-chunk",
		SectionID:          "deleted-section",
		DocumentID:         "deleted-document",
		SourceID:           source.ID,
		Model:              "legacy-model",
		SectionContentHash: "deleted-hash",
		ChunkTextHash:      "deleted-text-hash",
		Tokenizer:          "legacy-tokenizer",
		ChunkStrategy:      "legacy-strategy",
		GeneratorVersion:   "legacy-generator",
	}
	job, err := store.CreateJob(ctx, domain.JobInput{
		Kind:     "maintenance_embedding_ensure",
		SourceID: source.ID,
	})
	if err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}

	runner := NewRunner(store, NewDefaultRegistryWithEmbedding(store, jobsFakeEmbedder{model: "test-embedding"}, EmbeddingOptions{BatchSize: 1, ChunkTargetTokens: 1200, GeneratorVersion: "generator-v1"}), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !runner.RunOnce(ctx) {
		t.Fatal("RunOnce returned false, want handled embedding job")
	}
	handled, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if handled.Status != "completed" {
		t.Fatalf("handled status = %q, want completed; error=%s", handled.Status, handled.LastError)
	}
	for _, want := range []string{`"phase":"running"`, `"scanned_sections":2`, `"embedded_sections":2`, `"pending_batch_sections":0`, `"total_sections":2`, `"chunk_strategy":""`} {
		if !strings.Contains(handled.ProgressJSON, want) {
			t.Fatalf("ProgressJSON = %q, want %s", handled.ProgressJSON, want)
		}
	}
	for _, want := range []string{`"embedded_sections":2`, `"detected_orphan_sections":1`, `"detected_orphan_chunks":1`, `"deleted_orphan_sections":1`, `"deleted_orphan_chunks":1`, `"total_sections":2`, `"generator_version":"generator-v1"`} {
		if !strings.Contains(handled.ResultJSON, want) {
			t.Fatalf("ResultJSON = %q, want %s", handled.ResultJSON, want)
		}
	}
	if _, exists := backend.hashes["orphan-chunk"]; exists {
		t.Fatal("orphan chunk still exists after embedding ensure")
	}

	secondJob, err := store.CreateJob(ctx, domain.JobInput{
		Kind:     "maintenance_embedding_ensure",
		SourceID: source.ID,
	})
	if err != nil {
		t.Fatalf("CreateJob second returned error: %v", err)
	}
	if !runner.RunOnce(ctx) {
		t.Fatal("RunOnce second returned false, want handled embedding job")
	}
	secondHandled, err := store.GetJob(ctx, secondJob.ID)
	if err != nil {
		t.Fatalf("GetJob second returned error: %v", err)
	}
	if secondHandled.Status != "completed" {
		t.Fatalf("second status = %q, want completed; error=%s", secondHandled.Status, secondHandled.LastError)
	}
	for _, want := range []string{`"embedded_sections":0`, `"skipped_sections":2`, `"scanned_sections":2`, `"total_sections":2`} {
		if !strings.Contains(secondHandled.ResultJSON, want) {
			t.Fatalf("second ResultJSON = %q, want %s", secondHandled.ResultJSON, want)
		}
	}
}

func TestEmbeddingReconciliationCleansOrphansAfterSectionRewrite(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "docgraph.db"))
	if err != nil {
		t.Fatalf("sqlite.Open returned error: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}
	backend := newJobsFakeVectorBackend()
	store.SetVectorBackend(backend)
	source, err := store.CreateSource(ctx, domain.Source{ID: "source-section-rewrite", Kind: "local", Name: "Docs", DSN: "file:///docs"})
	if err != nil {
		t.Fatalf("CreateSource returned error: %v", err)
	}
	document := domain.DocumentInput{ID: "doc-section-rewrite", SourceID: source.ID, ExternalID: "rewrite.md", Title: "Rewrite", ContentHash: "hash-v1"}
	if err := store.ReplaceDocument(ctx, document, []domain.SectionInput{
		{ID: "a", Content: "section a", ContentHash: "hash-a", Ordinal: 0},
		{ID: "b", Content: "section b", ContentHash: "hash-b", Ordinal: 1},
		{ID: "c", Content: "section c", ContentHash: "hash-c", Ordinal: 2},
	}); err != nil {
		t.Fatalf("ReplaceDocument initial returned error: %v", err)
	}
	opts := embedding.EnsureOptions{
		SourceID:          source.ID,
		BatchSize:         2,
		ChunkTargetTokens: 1200,
		GeneratorVersion:  "generator-v1",
	}
	embedder := jobsFakeEmbedder{model: "test-embedding"}
	if _, err := embedding.Ensure(ctx, store, embedder, opts); err != nil {
		t.Fatalf("Ensure initial returned error: %v", err)
	}

	document.ContentHash = "hash-v2"
	if err := store.ReplaceDocument(ctx, document, []domain.SectionInput{
		{ID: "c", Content: "section c", ContentHash: "hash-c", Ordinal: 0},
		{ID: "d", Content: "section d", ContentHash: "hash-d", Ordinal: 1},
		{ID: "e", Content: "section e", ContentHash: "hash-e", Ordinal: 2},
	}); err != nil {
		t.Fatalf("ReplaceDocument rewritten returned error: %v", err)
	}

	before, err := embedding.AuditSourceEmbeddingStatus(ctx, store, source.ID, embedder.Model(), opts)
	if err != nil {
		t.Fatalf("AuditSourceEmbeddingStatus before cleanup returned error: %v", err)
	}
	if before.TotalSections != 3 || before.ReadySections != 1 || before.PendingSections != 2 || before.StaleSections != 0 || before.OrphanSections != 2 || before.OrphanChunks != 2 || !before.CleanupRequired || before.Status != "indexing" {
		t.Fatalf("status before cleanup = %+v, want ready=1 pending=2 orphan=2", before)
	}

	result, err := embedding.Ensure(ctx, store, embedder, opts)
	if err != nil {
		t.Fatalf("Ensure rewritten returned error: %v", err)
	}
	if result.EmbeddedSections != 2 || result.SkippedSections != 1 || result.DetectedPendingSections != 2 || result.DetectedOrphanSections != 2 || result.DetectedOrphanChunks != 2 || result.DeletedOrphanSections != 2 || result.DeletedOrphanChunks != 2 {
		t.Fatalf("Ensure rewritten result = %+v, want rebuild d/e and delete a/b", result)
	}

	after, err := embedding.AuditSourceEmbeddingStatus(ctx, store, source.ID, embedder.Model(), opts)
	if err != nil {
		t.Fatalf("AuditSourceEmbeddingStatus after cleanup returned error: %v", err)
	}
	if after.TotalSections != 3 || after.ReadySections != 3 || after.PendingSections != 0 || after.StaleSections != 0 || after.OrphanSections != 0 || after.OrphanChunks != 0 || after.CleanupRequired || after.Status != "ready" {
		t.Fatalf("status after cleanup = %+v, want exactly 3 ready sections", after)
	}
	for _, removedID := range []string{"a", "b"} {
		for _, hash := range backend.hashes {
			if hash.SectionID == removedID {
				t.Fatalf("orphan section %q still has chunk %q", removedID, hash.ChunkID)
			}
		}
	}
}

func TestEmbeddingStatusClassifiesReadyPendingStaleAndOrphan(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "docgraph.db"))
	if err != nil {
		t.Fatalf("sqlite.Open returned error: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}
	backend := newJobsFakeVectorBackend()
	store.SetVectorBackend(backend)
	source, err := store.CreateSource(ctx, domain.Source{ID: "source-status-classification", Kind: "local", Name: "Docs", DSN: "file:///docs"})
	if err != nil {
		t.Fatalf("CreateSource returned error: %v", err)
	}
	if err := store.ReplaceDocument(ctx, domain.DocumentInput{ID: "doc-status-classification", SourceID: source.ID, ExternalID: "status.md", Title: "Status", ContentHash: "hash-doc"}, []domain.SectionInput{
		{ID: "ready", Content: "ready section", ContentHash: "hash-ready", Ordinal: 0},
		{ID: "stale", Content: "stale section", ContentHash: "hash-stale", Ordinal: 1},
		{ID: "pending", Content: "pending section", ContentHash: "hash-pending", Ordinal: 2},
	}); err != nil {
		t.Fatalf("ReplaceDocument returned error: %v", err)
	}
	opts := embedding.EnsureOptions{SourceID: source.ID, BatchSize: 2, ChunkTargetTokens: 1200, GeneratorVersion: "generator-v1"}
	embedder := jobsFakeEmbedder{model: "test-embedding"}
	if _, err := embedding.Ensure(ctx, store, embedder, opts); err != nil {
		t.Fatalf("Ensure returned error: %v", err)
	}
	for chunkID, hash := range backend.hashes {
		switch hash.SectionID {
		case "stale":
			hash.SectionContentHash = "old-content-hash"
			backend.hashes[chunkID] = hash
		case "pending":
			delete(backend.hashes, chunkID)
		}
	}
	backend.hashes["orphan-status-chunk"] = domain.EmbeddingChunkHash{
		ChunkID:            "orphan-status-chunk",
		SectionID:          "removed",
		DocumentID:         "removed-document",
		SourceID:           source.ID,
		Model:              "legacy-model",
		SectionContentHash: "removed-hash",
		ChunkTextHash:      "removed-text-hash",
		Tokenizer:          "legacy-tokenizer",
		ChunkStrategy:      "legacy-strategy",
		GeneratorVersion:   "legacy-generator",
	}

	status, err := embedding.AuditSourceEmbeddingStatus(ctx, store, source.ID, embedder.Model(), opts)
	if err != nil {
		t.Fatalf("AuditSourceEmbeddingStatus returned error: %v", err)
	}
	if status.TotalSections != 3 || status.ReadySections != 1 || status.PendingSections != 1 || status.StaleSections != 1 || status.OrphanSections != 1 {
		t.Fatalf("section status = %+v, want one ready/pending/stale/orphan", status)
	}
	if status.ExpectedChunks != 3 || status.ReadyChunks != 1 || status.PendingChunks != 1 || status.StaleChunks != 1 || status.OrphanChunks != 1 {
		t.Fatalf("chunk status = %+v, want expected=3 and one ready/pending/stale/orphan", status)
	}
	if status.EmbeddedSections != status.ReadySections || status.TotalChunks != status.ExpectedChunks || status.EmbeddedChunks != status.ReadyChunks {
		t.Fatalf("compatibility aliases do not match exact status: %+v", status)
	}
	if !status.CleanupRequired || status.Status != "indexing" || status.AuditedAt == "" {
		t.Fatalf("status metadata = %+v, want cleanup_required indexing audit", status)
	}
}

func TestEmbeddingEnsureJobDefersTransientEmbeddingFailuresAndQueuesRetry(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "docgraph.db"))
	if err != nil {
		t.Fatalf("sqlite.Open returned error: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}
	backend := newJobsFakeVectorBackend()
	store.SetVectorBackend(backend)
	source, err := store.CreateSource(ctx, domain.Source{ID: "source-embedding-defer", Kind: "local", Name: "Docs", DSN: "file:///docs"})
	if err != nil {
		t.Fatalf("CreateSource returned error: %v", err)
	}
	if err := store.ReplaceDocument(ctx, domain.DocumentInput{ID: "doc-embedding-defer", SourceID: source.ID, ExternalID: "doc.md", Title: "Doc", ContentHash: "hash-doc"}, []domain.SectionInput{
		{ID: "section-embedding-defer-1", Content: "first section", ContentHash: "hash-section-1", Ordinal: 0},
		{ID: "section-embedding-defer-2", Content: "second section", ContentHash: "hash-section-2", Ordinal: 1},
	}); err != nil {
		t.Fatalf("ReplaceDocument returned error: %v", err)
	}
	job, err := store.CreateJob(ctx, domain.JobInput{
		Kind:     "maintenance_embedding_ensure",
		SourceID: source.ID,
	})
	if err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}

	runner := NewRunner(store, NewDefaultRegistryWithEmbedding(store, &transientOnceJobsEmbedder{model: "test-embedding"}, EmbeddingOptions{BatchSize: 1, ChunkTargetTokens: 1200, GeneratorVersion: "generator-v1"}), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !runner.RunOnce(ctx) {
		t.Fatal("RunOnce returned false, want handled embedding job")
	}
	handled, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if handled.Status != "completed" {
		t.Fatalf("handled status = %q, want completed; error=%s", handled.Status, handled.LastError)
	}
	for _, want := range []string{`"embedded_sections":1`, `"deferred_sections":1`, `"embedded_chunks":1`, `"deferred_chunks":1`, `"retry_job_id":"job_`} {
		if !strings.Contains(handled.ResultJSON, want) {
			t.Fatalf("ResultJSON = %q, want %s", handled.ResultJSON, want)
		}
	}
	if len(backend.hashes) != 1 {
		t.Fatalf("stored chunks = %d, want only successful batch stored before retry", len(backend.hashes))
	}
	queued, err := store.ListJobs(ctx, domain.JobListOptions{Kind: "maintenance_embedding_ensure", Status: "queued", SourceID: source.ID, Limit: 10})
	if err != nil {
		t.Fatalf("ListJobs retry returned error: %v", err)
	}
	if len(queued) != 1 || !strings.Contains(queued[0].PayloadJSON, `"mode":"ensure_retry"`) {
		t.Fatalf("queued retry jobs = %+v, want one deferred retry job", queued)
	}
	if runner.RunOnce(ctx) {
		t.Fatal("RunOnce returned true for future retry job, want retry to wait for run_after")
	}
}

func TestEmbeddingEnsureQueueDedupesAgainstGlobalJob(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "docgraph.db"))
	if err != nil {
		t.Fatalf("sqlite.Open returned error: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}
	source, err := store.CreateSource(ctx, domain.Source{ID: "source-embedding-dedupe", Kind: "local", Name: "Docs", DSN: "file:///tmp/missing"})
	if err != nil {
		t.Fatalf("CreateSource returned error: %v", err)
	}
	if _, err := store.CreateEmbeddingEnsureJobIfIdle(ctx, ""); err != nil {
		t.Fatalf("CreateEmbeddingEnsureJobIfIdle global returned error: %v", err)
	}
	if _, err := store.CreateEmbeddingEnsureJobIfIdle(ctx, source.ID); !errors.Is(err, domain.ErrSyncInProgress) {
		t.Fatalf("CreateEmbeddingEnsureJobIfIdle source error = %v, want ErrSyncInProgress", err)
	}
	count, err := store.CountJobs(ctx, domain.JobListOptions{Kind: "maintenance_embedding_ensure"})
	if err != nil {
		t.Fatalf("CountJobs returned error: %v", err)
	}
	if count != 1 {
		t.Fatalf("embedding job count = %d, want 1", count)
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

type jobsFakeEmbedder struct {
	model string
}

func (e jobsFakeEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	vectors := make([][]float32, len(texts))
	for i := range texts {
		vectors[i] = []float32{1, float32(i)}
	}
	return vectors, nil
}

func (e jobsFakeEmbedder) Model() string {
	return e.model
}

func (e jobsFakeEmbedder) Dimensions() int {
	return 2
}

type transientOnceJobsEmbedder struct {
	model string
	calls int
}

func (e *transientOnceJobsEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	e.calls++
	if e.calls == 1 {
		return nil, context.DeadlineExceeded
	}
	vectors := make([][]float32, len(texts))
	for i := range texts {
		vectors[i] = []float32{1, float32(i)}
	}
	return vectors, nil
}

func (e *transientOnceJobsEmbedder) Model() string {
	return e.model
}

func (e *transientOnceJobsEmbedder) Dimensions() int {
	return 2
}

type jobsFakeVectorBackend struct {
	hashes map[string]domain.EmbeddingChunkHash
}

func newJobsFakeVectorBackend() *jobsFakeVectorBackend {
	return &jobsFakeVectorBackend{hashes: map[string]domain.EmbeddingChunkHash{}}
}

func (f *jobsFakeVectorBackend) UpsertSectionEmbedding(ctx context.Context, input domain.SectionEmbeddingInput) error {
	return f.UpsertEmbeddingChunk(ctx, domain.EmbeddingChunkInput{
		ChunkID:            "chunk-" + input.SectionID,
		SectionID:          input.SectionID,
		DocumentID:         input.DocumentID,
		SourceID:           input.SourceID,
		ChunkOrdinal:       0,
		Model:              input.Model,
		SectionContentHash: input.ContentHash,
		ChunkTextHash:      input.EmbeddingTextHash,
		Tokenizer:          "conservative",
		ChunkStrategy:      "structural",
		GeneratorVersion:   input.GeneratorVersion,
	})
}

func (f *jobsFakeVectorBackend) GetSectionEmbedding(ctx context.Context, sectionID string, model string) (domain.VectorSearchHit, []float32, error) {
	return domain.VectorSearchHit{}, nil, sql.ErrNoRows
}

func (f *jobsFakeVectorBackend) DeleteSectionEmbeddings(ctx context.Context, sectionID string) error {
	return f.DeleteEmbeddingChunksBySection(ctx, sectionID, "", "", "", "")
}

func (f *jobsFakeVectorBackend) SearchSectionsByVector(ctx context.Context, embedding []float32, model string, limit int, minSimilarity float64, plan vectorstore.EmbeddingPlanFilter) ([]domain.VectorSearchHit, error) {
	return nil, nil
}

func (f *jobsFakeVectorBackend) UpsertEmbeddingChunk(ctx context.Context, input domain.EmbeddingChunkInput) error {
	if input.ChunkID == "" {
		input.ChunkID = "chunk-" + input.SectionID
	}
	f.hashes[input.ChunkID] = domain.EmbeddingChunkHash{
		ChunkID:            input.ChunkID,
		SectionID:          input.SectionID,
		DocumentID:         input.DocumentID,
		SourceID:           input.SourceID,
		ChunkOrdinal:       input.ChunkOrdinal,
		Model:              input.Model,
		SectionContentHash: input.SectionContentHash,
		ChunkTextHash:      input.ChunkTextHash,
		Tokenizer:          input.Tokenizer,
		ChunkStrategy:      input.ChunkStrategy,
		GeneratorVersion:   input.GeneratorVersion,
		GeneratedAt:        "2026-07-03T00:00:00Z",
	}
	return nil
}

func (f *jobsFakeVectorBackend) GetEmbeddingChunk(ctx context.Context, chunkID string, model string) (domain.VectorSearchHit, []float32, error) {
	return domain.VectorSearchHit{}, nil, sql.ErrNoRows
}

func (f *jobsFakeVectorBackend) DeleteEmbeddingChunksBySection(ctx context.Context, sectionID string, model string, generatorVersion string, tokenizer string, chunkStrategy string) error {
	for chunkID, hash := range f.hashes {
		if hash.SectionID != sectionID {
			continue
		}
		if model != "" && hash.Model != model {
			continue
		}
		if generatorVersion != "" && hash.GeneratorVersion != generatorVersion {
			continue
		}
		if tokenizer != "" && hash.Tokenizer != tokenizer {
			continue
		}
		if chunkStrategy != "" && hash.ChunkStrategy != chunkStrategy {
			continue
		}
		delete(f.hashes, chunkID)
	}
	return nil
}

func (f *jobsFakeVectorBackend) SearchChunksByVector(ctx context.Context, embedding []float32, model string, limit int, minSimilarity float64, plan vectorstore.EmbeddingPlanFilter) ([]domain.VectorSearchHit, error) {
	return nil, nil
}

func (f *jobsFakeVectorBackend) ListSectionEmbeddingHashes(ctx context.Context, model string, limit, offset int) ([]domain.SectionEmbeddingHash, error) {
	items := make([]domain.SectionEmbeddingHash, 0, len(f.hashes))
	for _, hash := range f.hashes {
		if hash.Model == model {
			items = append(items, domain.SectionEmbeddingHash{
				SectionID:         hash.SectionID,
				DocumentID:        hash.DocumentID,
				SourceID:          hash.SourceID,
				Model:             hash.Model,
				ContentHash:       hash.SectionContentHash,
				EmbeddingTextHash: hash.ChunkTextHash,
				GeneratorVersion:  hash.GeneratorVersion,
				GeneratedAt:       hash.GeneratedAt,
			})
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].SectionID < items[j].SectionID
	})
	if offset >= len(items) {
		return nil, nil
	}
	if limit <= 0 || offset+limit > len(items) {
		limit = len(items) - offset
	}
	return items[offset : offset+limit], nil
}

func (f *jobsFakeVectorBackend) ListEmbeddingChunkHashes(ctx context.Context, model string, limit, offset int) ([]domain.EmbeddingChunkHash, error) {
	items := make([]domain.EmbeddingChunkHash, 0, len(f.hashes))
	for _, hash := range f.hashes {
		if hash.Model == model {
			items = append(items, hash)
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].ChunkID < items[j].ChunkID
	})
	if offset >= len(items) {
		return nil, nil
	}
	if limit <= 0 || offset+limit > len(items) {
		limit = len(items) - offset
	}
	return items[offset : offset+limit], nil
}

func (f *jobsFakeVectorBackend) ListEmbeddingChunkInventory(ctx context.Context, opts vectorstore.EmbeddingInventoryOptions) ([]domain.EmbeddingChunkHash, error) {
	items := make([]domain.EmbeddingChunkHash, 0, len(f.hashes))
	for _, hash := range f.hashes {
		if opts.SourceID != "" && hash.SourceID != opts.SourceID {
			continue
		}
		if !opts.IncludeAllPlans {
			if opts.Model != "" && hash.Model != opts.Model {
				continue
			}
			if opts.GeneratorVersion != "" && hash.GeneratorVersion != opts.GeneratorVersion {
				continue
			}
			if opts.Tokenizer != "" && hash.Tokenizer != opts.Tokenizer {
				continue
			}
			if opts.ChunkStrategy != "" && hash.ChunkStrategy != opts.ChunkStrategy {
				continue
			}
		}
		items = append(items, hash)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].SectionID != items[j].SectionID {
			return items[i].SectionID < items[j].SectionID
		}
		if items[i].ChunkOrdinal != items[j].ChunkOrdinal {
			return items[i].ChunkOrdinal < items[j].ChunkOrdinal
		}
		return items[i].ChunkID < items[j].ChunkID
	})
	if opts.Offset >= len(items) {
		return nil, nil
	}
	limit := opts.Limit
	if limit <= 0 || opts.Offset+limit > len(items) {
		limit = len(items) - opts.Offset
	}
	return items[opts.Offset : opts.Offset+limit], nil
}

func (f *jobsFakeVectorBackend) DeleteEmbeddingChunksBySectionIDs(ctx context.Context, sourceID string, sectionIDs []string) (int64, error) {
	sections := map[string]bool{}
	for _, sectionID := range sectionIDs {
		sections[strings.TrimSpace(sectionID)] = true
	}
	var deleted int64
	for chunkID, hash := range f.hashes {
		if hash.SourceID == sourceID && sections[hash.SectionID] {
			delete(f.hashes, chunkID)
			deleted++
		}
	}
	return deleted, nil
}

func (f *jobsFakeVectorBackend) GetEmbeddingCoverage(ctx context.Context, sourceID string, model string, generatorVersion string, tokenizer string, chunkStrategy string) (domain.EmbeddingCoverage, error) {
	coverage := domain.EmbeddingCoverage{}
	seenSections := map[string]bool{}
	for _, hash := range f.hashes {
		if sourceID != "" && hash.SourceID != sourceID {
			continue
		}
		if model != "" && hash.Model != model {
			continue
		}
		if generatorVersion != "" && hash.GeneratorVersion != generatorVersion {
			continue
		}
		if tokenizer != "" && hash.Tokenizer != tokenizer {
			continue
		}
		if chunkStrategy != "" && hash.ChunkStrategy != chunkStrategy {
			continue
		}
		coverage.EmbeddedChunks++
		if !seenSections[hash.SectionID] {
			seenSections[hash.SectionID] = true
			coverage.EmbeddedSections++
		}
	}
	return coverage, nil
}

func (f *jobsFakeVectorBackend) Close() error {
	return nil
}
