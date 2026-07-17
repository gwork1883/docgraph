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
	latestJobs, err := r.store.ListLatestSyncJobs(ctx)
	if err != nil {
		r.logger.Warn("list latest sync jobs for scheduler failed", "error", err)
		return
	}
	latestJobsBySourceID := make(map[string]storage.SyncJob, len(latestJobs))
	for _, job := range latestJobs {
		latestJobsBySourceID[job.SourceID] = job
	}
	now := r.now()
	for _, source := range sources {
		var latestJob *storage.SyncJob
		if job, ok := latestJobsBySourceID[source.ID]; ok {
			latestJob = &job
		}
		due, err := r.due(source, latestJob, now)
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

func (r *Runner) due(source storage.Source, latestJob *storage.SyncJob, now time.Time) (bool, error) {
	if strings.TrimSpace(source.SyncStatus) == "paused" {
		return false, nil
	}
	interval, enabled, err := syncschedule.Parse(source.SyncSchedule)
	if err != nil || !enabled {
		return false, err
	}
	if latestJob == nil {
		return true, nil
	}
	if latestJob.Status == "queued" || latestJob.Status == "running" || latestJob.Status == "canceling" {
		return false, nil
	}
	last, ok := parseJobTime(latestJob.UpdatedAt)
	if !ok {
		last, ok = parseJobTime(latestJob.CreatedAt)
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
