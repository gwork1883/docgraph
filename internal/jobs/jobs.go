package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

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
	registry := NewRegistry()
	registry.Register("sync_source", func(ctx context.Context, job storage.Job) error {
		_, err := syncsvc.NewService(store).RunSyncJob(ctx, job)
		return err
	})
	return registry
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
