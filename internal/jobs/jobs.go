package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/docgraph/docgraph/internal/config"
	"github.com/docgraph/docgraph/internal/embedding"
	"github.com/docgraph/docgraph/internal/storage"
	syncsvc "github.com/docgraph/docgraph/internal/sync"
)

type Handler func(context.Context, storage.Job) error

type Registry struct {
	handlers map[string]Handler
}

func NewRegistry() *Registry {
	return &Registry{handlers: map[string]Handler{}}
}

func NewDefaultRegistry(store storage.Store) *Registry {
	return NewDefaultRegistryWithEmbedding(store, embedding.NewNoOpEmbedder(), EmbeddingOptions{BatchSize: 64, GeneratorVersion: embedding.DefaultGeneratorVersion})
}

type EmbeddingOptions struct {
	BatchSize          int
	ContextTokens      int
	ChunkTargetTokens  int
	ChunkOverlapTokens int
	MaxBatchTokens     int
	Concurrency        int
	Limits             embedding.ResolvedEmbeddingLimits
	Tokenizer          string
	ChunkStrategy      string
	GeneratorVersion   string
}

func NewDefaultRegistryWithEmbedding(store storage.Store, embedder embedding.Embedder, opts EmbeddingOptions) *Registry {
	registry := NewRegistry()
	generatorVersion := strings.TrimSpace(opts.GeneratorVersion)
	if generatorVersion == "" {
		generatorVersion = embedding.DefaultGeneratorVersion
	}
	limits := resolvedLimitsFromOptions(opts)
	registry.Register("sync_source", func(ctx context.Context, job storage.Job) error {
		_, err := syncsvc.NewService(store).RunSyncJob(ctx, job)
		if err != nil {
			return err
		}
		if embedder != nil && strings.TrimSpace(embedder.Model()) != "" && strings.TrimSpace(job.SourceID) != "" {
			err = enqueueEmbeddingEnsureJob(ctx, store, job.SourceID)
		}
		return err
	})
	registry.Register("maintenance_embedding_ensure", func(ctx context.Context, job storage.Job) error {
		sourceID := strings.TrimSpace(job.SourceID)
		model := ""
		if embedder != nil {
			model = embedder.Model()
		}
		totalSections := 0
		if strings.TrimSpace(model) != "" {
			if count, err := store.CountSectionsForEmbedding(ctx, sourceID); err == nil {
				totalSections = count
			}
		}
		_ = store.UpdateJobProgress(ctx, job.ID, embeddingProgressJSON(sourceID, "running", embedding.EnsureProgress{}, totalSections, opts))
		result, err := embedding.Ensure(ctx, store, embedder, embedding.EnsureOptions{
			SourceID:           sourceID,
			BatchSize:          opts.BatchSize,
			ContextTokens:      opts.ContextTokens,
			ChunkTargetTokens:  opts.ChunkTargetTokens,
			ChunkOverlapTokens: opts.ChunkOverlapTokens,
			MaxBatchTokens:     opts.MaxBatchTokens,
			Concurrency:        opts.Concurrency,
			Limits:             limits,
			Tokenizer:          opts.Tokenizer,
			ChunkStrategy:      opts.ChunkStrategy,
			GeneratorVersion:   generatorVersion,
			OnProgress: func(progress embedding.EnsureProgress) error {
				return store.UpdateJobProgress(ctx, job.ID, embeddingProgressJSON(sourceID, "running", progress, totalSections, opts))
			},
		})
		if err != nil {
			_ = store.FailJob(ctx, job.ID, err.Error())
			return err
		}
		retryJobID := ""
		if result.DeferredChunks > 0 {
			if retryJob, retryErr := createDeferredEmbeddingRetryJob(ctx, store, sourceID); retryErr == nil {
				retryJobID = retryJob.ID
			} else {
				_ = store.UpdateJobProgress(ctx, job.ID, embeddingProgressJSON(sourceID, "retry_enqueue_failed", embedding.EnsureProgress{
					ScannedSections:  result.ScannedSections,
					EmbeddedSections: result.EmbeddedSections,
					SkippedSections:  result.SkippedSections,
					DeferredSections: result.DeferredSections,
					ScannedChunks:    result.ScannedChunks,
					EmbeddedChunks:   result.EmbeddedChunks,
					SkippedChunks:    result.SkippedChunks,
					DeferredChunks:   result.DeferredChunks,
				}, totalSections, opts))
				return retryErr
			}
		}
		return store.CompleteJob(ctx, job.ID, fmt.Sprintf(`{"source_id":%q,"embedded_sections":%d,"skipped_sections":%d,"deferred_sections":%d,"scanned_sections":%d,"embedded_chunks":%d,"skipped_chunks":%d,"deferred_chunks":%d,"scanned_chunks":%d,"total_sections":%d,"tokenizer":%q,"chunk_strategy":%q,"generator_version":%q,"retry_job_id":%q}`, sourceID, result.EmbeddedSections, result.SkippedSections, result.DeferredSections, result.ScannedSections, result.EmbeddedChunks, result.SkippedChunks, result.DeferredChunks, result.ScannedChunks, totalSections, opts.Tokenizer, opts.ChunkStrategy, generatorVersion, retryJobID))
	})
	return registry
}

func resolvedLimitsFromOptions(opts EmbeddingOptions) embedding.ResolvedEmbeddingLimits {
	if opts.Limits.ChunkTargetTokens > 0 && opts.Limits.BatchSize > 0 && opts.Limits.MaxBatchTokens > 0 {
		return opts.Limits
	}
	return embedding.ResolveEmbeddingLimits(configFromOptions(opts), embedding.ProviderCapabilities{})
}

func configFromOptions(opts EmbeddingOptions) config.EmbeddingConfig {
	return config.EmbeddingConfig{
		ContextTokens:      opts.ContextTokens,
		ChunkTargetTokens:  opts.ChunkTargetTokens,
		ChunkOverlapTokens: opts.ChunkOverlapTokens,
		MaxBatchTokens:     opts.MaxBatchTokens,
		BatchSize:          opts.BatchSize,
	}
}

func enqueueEmbeddingEnsureJob(ctx context.Context, store storage.Store, sourceID string) error {
	_, err := store.CreateEmbeddingEnsureJobIfIdle(ctx, sourceID)
	if err != nil {
		if errors.Is(err, storage.ErrSyncInProgress) {
			return nil
		}
		return err
	}
	return nil
}

func createDeferredEmbeddingRetryJob(ctx context.Context, store storage.Store, sourceID string) (storage.Job, error) {
	sourceID = strings.TrimSpace(sourceID)
	targetKind := "embedding"
	payload := `{"mode":"ensure_retry","reason":"deferred_embedding_batch"}`
	if sourceID != "" {
		targetKind = "source"
		payload = fmt.Sprintf(`{"source_id":%q,"mode":"ensure_retry","reason":"deferred_embedding_batch"}`, sourceID)
	}
	return store.CreateJob(ctx, storage.JobInput{
		Kind:        "maintenance_embedding_ensure",
		SourceID:    sourceID,
		TargetKind:  targetKind,
		TargetID:    sourceID,
		PayloadJSON: payload,
		RunAfter:    time.Now().UTC().Add(2 * time.Minute).Format("2006-01-02 15:04:05"),
	})
}

func embeddingProgressJSON(sourceID string, phase string, progress embedding.EnsureProgress, totalSections int, opts EmbeddingOptions) string {
	return fmt.Sprintf(`{"source_id":%q,"phase":%q,"scanned_sections":%d,"embedded_sections":%d,"skipped_sections":%d,"deferred_sections":%d,"pending_batch_sections":%d,"scanned_chunks":%d,"embedded_chunks":%d,"skipped_chunks":%d,"deferred_chunks":%d,"pending_batch_chunks":%d,"total_sections":%d,"tokenizer":%q,"chunk_strategy":%q,"generator_version":%q}`, sourceID, phase, progress.ScannedSections, progress.EmbeddedSections, progress.SkippedSections, progress.DeferredSections, progress.PendingSections, progress.ScannedChunks, progress.EmbeddedChunks, progress.SkippedChunks, progress.DeferredChunks, progress.PendingChunks, totalSections, opts.Tokenizer, opts.ChunkStrategy, opts.GeneratorVersion)
}

func (r *Registry) Register(kind string, handler Handler) {
	kind = strings.TrimSpace(kind)
	if kind == "" || handler == nil {
		return
	}
	r.handlers[kind] = handler
}

func (r *Registry) Kinds() []string {
	kinds := make([]string, 0, len(r.handlers))
	for kind := range r.handlers {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}

func (r *Registry) Handler(kind string) (Handler, bool) {
	handler, ok := r.handlers[strings.TrimSpace(kind)]
	return handler, ok
}

type Runner struct {
	store        storage.Store
	registry     *Registry
	logger       *slog.Logger
	workerID     string
	workers      int
	pollInterval time.Duration
	cancelPoll   time.Duration
	lease        time.Duration
	mu           sync.Mutex
	running      map[string]context.CancelFunc
}

func NewRunner(store storage.Store, registry *Registry, logger *slog.Logger) *Runner {
	if registry == nil {
		registry = NewDefaultRegistry(store)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{
		store:        store,
		registry:     registry,
		logger:       logger,
		workerID:     fmt.Sprintf("worker-%d", time.Now().UnixNano()),
		workers:      1,
		pollInterval: time.Second,
		cancelPoll:   time.Second,
		lease:        10 * time.Minute,
		running:      map[string]context.CancelFunc{},
	}
}

func (r *Runner) SetWorkerCount(workers int) {
	if workers < 1 {
		workers = 1
	}
	r.workers = workers
}

func (r *Runner) Run(ctx context.Context) {
	if r.workers <= 1 {
		r.runLoop(ctx, r.workerID)
		return
	}
	var wg sync.WaitGroup
	for i := 0; i < r.workers; i++ {
		workerID := fmt.Sprintf("%s-%d", r.workerID, i+1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.runLoop(ctx, workerID)
		}()
	}
	wg.Wait()
}

func (r *Runner) runLoop(ctx context.Context, workerID string) {
	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()
	for {
		worked := r.runOnce(ctx, workerID)
		if worked {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Runner) RunOnce(ctx context.Context) bool {
	return r.runOnce(ctx, r.workerID)
}

func (r *Runner) runOnce(ctx context.Context, workerID string) bool {
	kinds := r.registry.Kinds()
	if len(kinds) == 0 {
		return false
	}
	job, err := r.store.ClaimDueJob(ctx, workerID, kinds, r.lease)
	if err != nil {
		if err != sql.ErrNoRows {
			r.logger.Warn("claim job failed", "error", err)
		}
		return false
	}
	handler, ok := r.registry.Handler(job.Kind)
	if !ok {
		_ = r.store.FailJob(ctx, job.ID, fmt.Sprintf("no handler registered for job kind %q", job.Kind))
		return true
	}
	jobCtx, cancel := context.WithCancel(ctx)
	r.register(job.ID, cancel)
	defer r.unregister(job.ID)
	defer cancel()
	stopMonitor := r.monitorCancelRequests(jobCtx, job.ID, cancel)
	defer stopMonitor()
	if err := handler(jobCtx, job); err != nil {
		r.logger.Warn("job failed", "job_id", job.ID, "kind", job.Kind, "error", err)
	}
	r.finalizeCanceledJob(ctx, job.ID)
	return true
}

func (r *Runner) CancelJob(ctx context.Context, id string, reason string) (storage.Job, error) {
	job, err := r.store.CancelJob(ctx, id, reason)
	if err != nil {
		return job, err
	}
	r.mu.Lock()
	cancel := r.running[strings.TrimSpace(id)]
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return job, nil
}

func (r *Runner) register(id string, cancel context.CancelFunc) {
	id = strings.TrimSpace(id)
	if id == "" || cancel == nil {
		return
	}
	r.mu.Lock()
	r.running[id] = cancel
	r.mu.Unlock()
}

func (r *Runner) unregister(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	r.mu.Lock()
	delete(r.running, id)
	r.mu.Unlock()
}

func (r *Runner) monitorCancelRequests(ctx context.Context, id string, cancel context.CancelFunc) func() {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(r.cancelPoll)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				job, err := r.store.GetJob(ctx, id)
				if err != nil {
					continue
				}
				if job.Status == "canceling" || job.Status == "canceled" {
					cancel()
					return
				}
			}
		}
	}()
	return func() { close(done) }
}

func (r *Runner) finalizeCanceledJob(ctx context.Context, id string) {
	job, err := r.store.GetJob(ctx, id)
	if err != nil || job.Status != "canceling" {
		return
	}
	if err := r.store.MarkJobCanceled(ctx, id, job.LastError); err != nil {
		r.logger.Warn("mark job canceled failed", "job_id", id, "error", err)
	}
}
