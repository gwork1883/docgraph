package scheduler

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/docgraph/docgraph/internal/storage"
	"github.com/docgraph/docgraph/internal/syncschedule"
)

type Runner struct {
	store        storage.Store
	logger       *slog.Logger
	pollInterval time.Duration
	now          func() time.Time
}

func NewRunner(store storage.Store, logger *slog.Logger) *Runner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{
		store:        store,
		logger:       logger,
		pollInterval: time.Minute,
		now:          time.Now,
	}
}

func (r *Runner) Run(ctx context.Context) {
	r.RunOnce(ctx)
	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.RunOnce(ctx)
		}
	}
}

func (r *Runner) RunOnce(ctx context.Context) {
	sources, err := r.store.ListSources(ctx)
	if err != nil {
		r.logger.Warn("list sources for scheduled sync failed", "error", err)
		return
	}
	now := r.now()
	for _, source := range sources {
		due, err := r.due(ctx, source, now)
		if err != nil {
			r.logger.Warn("evaluate source sync schedule failed", "source_id", source.ID, "error", err)
			continue
		}
		if !due {
			continue
		}
		if _, err := r.store.CreateSyncJobIfIdle(ctx, source.ID); err != nil {
			r.logger.Warn("scheduled source sync enqueue failed", "source_id", source.ID, "error", err)
		}
	}
}

func (r *Runner) due(ctx context.Context, source storage.Source, now time.Time) (bool, error) {
	if strings.TrimSpace(source.SyncStatus) == "paused" {
		return false, nil
	}
	interval, enabled, err := syncschedule.Parse(source.SyncSchedule)
	if err != nil || !enabled {
		return false, err
	}
	jobs, err := r.store.ListSyncJobs(ctx, source.ID, 1)
	if err != nil {
		return false, err
	}
	if len(jobs) == 0 {
		return true, nil
	}
	if jobs[0].Status == "queued" || jobs[0].Status == "running" {
		return false, nil
	}
	last, ok := parseJobTime(jobs[0].UpdatedAt)
	if !ok {
		last, ok = parseJobTime(jobs[0].CreatedAt)
	}
	if !ok {
		return true, nil
	}
	return !last.Add(interval).After(now), nil
}

func parseJobTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if parsed, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}
