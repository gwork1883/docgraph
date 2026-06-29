package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"strings"
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
	pollInterval time.Duration
	lease        time.Duration
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
		pollInterval: time.Second,
		lease:        10 * time.Minute,
	}
}

func (r *Runner) Run(ctx context.Context) {
	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()
	for {
		worked := r.RunOnce(ctx)
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
	kinds := r.registry.Kinds()
	if len(kinds) == 0 {
		return false
	}
	job, err := r.store.ClaimDueJob(ctx, r.workerID, kinds, r.lease)
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
	if err := handler(ctx, job); err != nil {
		r.logger.Warn("job failed", "job_id", job.ID, "kind", job.Kind, "error", err)
	}
	return true
}
