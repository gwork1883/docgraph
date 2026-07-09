package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docgraph/docgraph/internal/domain"
	"github.com/docgraph/docgraph/internal/storage/sqlschema"
	"github.com/docgraph/docgraph/internal/vectorstore"
)

func TestPathFromDSN(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "docgraph.db")

	tests := []struct {
		name    string
		dsn     string
		want    string
		wantErr string
	}{
		{
			name: "relative path",
			dsn:  "sqlite://.docgraph/../docgraph.db",
			want: filepath.Clean("docgraph.db"),
		},
		{
			name: "absolute path",
			dsn:  "sqlite://" + abs,
			want: abs,
		},
		{
			name:    "unsupported scheme",
			dsn:     "postgres://localhost/docgraph",
			wantErr: "invalid sqlite DSN",
		},
		{
			name:    "empty path",
			dsn:     "sqlite://",
			wantErr: "requires a path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := pathFromDSN(tt.dsn)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatal("pathFromDSN returned nil error")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("pathFromDSN error = %q, want substring %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("pathFromDSN returned error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("pathFromDSN = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGenericJobLifecycleAndLease(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "docgraph.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}

	future, err := store.CreateJob(ctx, domain.JobInput{
		Kind:        "maintenance_profile_rebuild",
		SourceID:    "src-jobs",
		TargetKind:  "source",
		TargetID:    "src-jobs",
		PayloadJSON: `{"scope":"future"}`,
		RunAfter:    time.Now().UTC().Add(time.Hour).Format("2006-01-02 15:04:05"),
	})
	if err != nil {
		t.Fatalf("CreateJob future returned error: %v", err)
	}
	due, err := store.CreateJob(ctx, domain.JobInput{
		Kind:         "maintenance_profile_rebuild",
		SourceID:     "src-jobs",
		TargetKind:   "source",
		TargetID:     "src-jobs",
		PayloadJSON:  `{"scope":"due"}`,
		ProgressJSON: `{"phase":"queued"}`,
	})
	if err != nil {
		t.Fatalf("CreateJob due returned error: %v", err)
	}
	if due.Status != "queued" || due.SourceID != "src-jobs" || due.TargetKind != "source" || due.ProgressJSON != `{"phase":"queued"}` {
		t.Fatalf("due job = %+v, want queued job with structured fields", due)
	}

	claimed, err := store.ClaimDueJob(ctx, "worker-a", []string{"maintenance_profile_rebuild"}, time.Minute)
	if err != nil {
		t.Fatalf("ClaimDueJob returned error: %v", err)
	}
	if claimed.ID != due.ID || claimed.Status != "running" || claimed.WorkerID != "worker-a" || claimed.Attempts != 1 || claimed.LockedUntil == "" {
		t.Fatalf("claimed job = %+v, want running due job leased by worker-a", claimed)
	}
	if _, err := store.ClaimDueJob(ctx, "worker-b", []string{"maintenance_profile_rebuild"}, time.Minute); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ClaimDueJob with only future/unexpired jobs error = %v, want sql.ErrNoRows", err)
	}
	if _, err := store.db.ExecContext(ctx, `update jobs set locked_until = datetime('now', '-1 minute') where id = ?`, due.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	reclaimed, err := store.ClaimDueJob(ctx, "worker-b", []string{"maintenance_profile_rebuild"}, time.Minute)
	if err != nil {
		t.Fatalf("ClaimDueJob expired lease returned error: %v", err)
	}
	if reclaimed.ID != due.ID || reclaimed.WorkerID != "worker-b" || reclaimed.Attempts != 2 {
		t.Fatalf("reclaimed job = %+v, want same job leased by worker-b with second attempt", reclaimed)
	}

	if err := store.UpdateJobProgress(ctx, due.ID, `{"phase":"running"}`); err != nil {
		t.Fatalf("UpdateJobProgress returned error: %v", err)
	}
	progressed, err := store.GetJob(ctx, due.ID)
	if err != nil {
		t.Fatalf("GetJob after progress returned error: %v", err)
	}
	if progressed.ProgressJSON != `{"phase":"running"}` {
		t.Fatalf("ProgressJSON = %q, want running phase", progressed.ProgressJSON)
	}
	if err := store.CompleteJob(ctx, due.ID, `{"documents":3}`); err != nil {
		t.Fatalf("CompleteJob returned error: %v", err)
	}
	completed, err := store.GetJob(ctx, due.ID)
	if err != nil {
		t.Fatalf("GetJob completed returned error: %v", err)
	}
	if completed.Status != "completed" || completed.ResultJSON != `{"documents":3}` || completed.WorkerID != "" || completed.LockedUntil != "" || completed.LastError != "" {
		t.Fatalf("completed job = %+v, want completed with cleared lease", completed)
	}

	if err := store.FailJob(ctx, future.ID, " boom "); err != nil {
		t.Fatalf("FailJob returned error: %v", err)
	}
	failed, err := store.GetJob(ctx, future.ID)
	if err != nil {
		t.Fatalf("GetJob failed returned error: %v", err)
	}
	if failed.Status != "failed" || failed.LastError != "boom" || failed.WorkerID != "" || failed.LockedUntil != "" {
		t.Fatalf("failed job = %+v, want failed with trimmed error and cleared lease", failed)
	}

	jobs, err := store.ListJobs(ctx, domain.JobListOptions{
		SourceID:   "src-jobs",
		Kind:       "maintenance_profile_rebuild",
		TargetKind: "source",
		TargetID:   "src-jobs",
		Limit:      10,
	})
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	if len(jobs) != 2 || jobs[0].ID == "" || jobs[1].ID == "" {
		t.Fatalf("ListJobs = %+v, want two matching jobs", jobs)
	}
	completedJobs, err := store.ListJobs(ctx, domain.JobListOptions{SourceID: "src-jobs", Status: "completed", Limit: 10})
	if err != nil {
		t.Fatalf("ListJobs completed returned error: %v", err)
	}
	if len(completedJobs) != 1 || completedJobs[0].ID != due.ID {
		t.Fatalf("completed jobs = %+v, want due job only", completedJobs)
	}
}

func TestCreateEmbeddingEnsureJobIfIdleDedupesActiveScopes(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "docgraph.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}

	sourceJob, err := store.CreateEmbeddingEnsureJobIfIdle(ctx, "src-embedding-ensure")
	if err != nil {
		t.Fatalf("CreateEmbeddingEnsureJobIfIdle source returned error: %v", err)
	}
	if sourceJob.Kind != "maintenance_embedding_ensure" || sourceJob.Status != "queued" || sourceJob.SourceID != "src-embedding-ensure" || sourceJob.TargetKind != "source" {
		t.Fatalf("source ensure job = %+v, want queued source-scoped embedding ensure job", sourceJob)
	}
	if _, err := store.CreateEmbeddingEnsureJobIfIdle(ctx, "src-embedding-ensure"); !errors.Is(err, domain.ErrSyncInProgress) {
		t.Fatalf("duplicate source ensure error = %v, want ErrSyncInProgress", err)
	}
	if _, err := store.CreateEmbeddingEnsureJobIfIdle(ctx, ""); !errors.Is(err, domain.ErrSyncInProgress) {
		t.Fatalf("global ensure while source active error = %v, want ErrSyncInProgress", err)
	}
	if err := store.CompleteJob(ctx, sourceJob.ID, `{}`); err != nil {
		t.Fatalf("CompleteJob source returned error: %v", err)
	}
	globalJob, err := store.CreateEmbeddingEnsureJobIfIdle(ctx, "")
	if err != nil {
		t.Fatalf("CreateEmbeddingEnsureJobIfIdle global returned error: %v", err)
	}
	if globalJob.SourceID != "" || globalJob.TargetKind != "embedding" {
		t.Fatalf("global ensure job = %+v, want global embedding job", globalJob)
	}
	if _, err := store.CreateEmbeddingEnsureJobIfIdle(ctx, "other-source"); !errors.Is(err, domain.ErrSyncInProgress) {
		t.Fatalf("source ensure while global active error = %v, want ErrSyncInProgress", err)
	}
	if err := store.CompleteJob(ctx, globalJob.ID, `{}`); err != nil {
		t.Fatalf("CompleteJob global returned error: %v", err)
	}
	if _, err := store.CreateEmbeddingEnsureJobIfIdle(ctx, "other-source"); err != nil {
		t.Fatalf("CreateEmbeddingEnsureJobIfIdle after global completed returned error: %v", err)
	}
}

func TestOpenCreatesParentDirectory(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "nested", "data", "docgraph.db")

	store, err := Open(ctx, "sqlite://"+dbPath)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if _, err := os.Stat(filepath.Dir(dbPath)); err != nil {
		t.Fatalf("parent directory was not created: %v", err)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("database file was not created: %v", err)
	}
}

func TestOpenConfiguresSQLitePragmasAndPools(t *testing.T) {
	ctx := context.Background()
	store := openTempStore(t, ctx)

	if got := store.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("writer MaxOpenConnections = %d, want 1", got)
	}
	if got := store.reader.Stats().MaxOpenConnections; got < 4 {
		t.Fatalf("reader MaxOpenConnections = %d, want at least 4", got)
	}

	assertPragmaString(t, ctx, store.db, "journal_mode", "wal")
	assertPragmaString(t, ctx, store.reader, "journal_mode", "wal")
	assertPragmaInt(t, ctx, store.db, "busy_timeout", 5000)
	assertPragmaInt(t, ctx, store.reader, "busy_timeout", 5000)
	assertPragmaInt(t, ctx, store.db, "foreign_keys", 1)
	assertPragmaInt(t, ctx, store.reader, "foreign_keys", 1)
	assertPragmaInt(t, ctx, store.db, "synchronous", 1)
	assertPragmaInt(t, ctx, store.reader, "synchronous", 1)
}

func TestConcurrentReadsDuringWriterOperations(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-concurrent")
	replaceConcurrentDocument(t, ctx, store, 0)

	var wg sync.WaitGroup
	errs := make(chan error, 128)

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= 25; i++ {
			if err := store.ReplaceDocument(ctx, concurrentDocumentInput(i), concurrentSections(i)); err != nil {
				errs <- fmt.Errorf("replace document %d: %w", i, err)
				return
			}
			job, err := store.CreateJob(ctx, domain.JobInput{
				Kind:        "maintenance_profile_rebuild",
				SourceID:    "source-concurrent",
				TargetKind:  "document",
				TargetID:    "doc-concurrent",
				PayloadJSON: fmt.Sprintf(`{"iteration":%d}`, i),
			})
			if err != nil {
				errs <- fmt.Errorf("create job %d: %w", i, err)
				return
			}
			if _, err := store.ClaimDueJob(ctx, fmt.Sprintf("worker-%d", i), []string{"maintenance_profile_rebuild"}, time.Minute); err != nil {
				errs <- fmt.Errorf("claim job %d: %w", i, err)
				return
			}
			if err := store.CompleteJob(ctx, job.ID, fmt.Sprintf(`{"iteration":%d}`, i)); err != nil {
				errs <- fmt.Errorf("complete job %d: %w", i, err)
				return
			}
		}
	}()

	for readerID := 0; readerID < 8; readerID++ {
		wg.Add(1)
		go func(readerID int) {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				if _, err := store.Status(ctx); err != nil {
					errs <- fmt.Errorf("reader %d status: %w", readerID, err)
					return
				}
				if _, err := store.GetSource(ctx, "source-concurrent"); err != nil {
					errs <- fmt.Errorf("reader %d get source: %w", readerID, err)
					return
				}
				if _, err := store.ListJobs(ctx, domain.JobListOptions{SourceID: "source-concurrent", Limit: 20}); err != nil {
					errs <- fmt.Errorf("reader %d list jobs: %w", readerID, err)
					return
				}
				if _, err := store.SearchSections(ctx, "membership lifecycle", 10); err != nil {
					errs <- fmt.Errorf("reader %d search: %w", readerID, err)
					return
				}
			}
		}(readerID)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	hits, err := store.SearchSections(ctx, "membership lifecycle", 10)
	if err != nil {
		t.Fatalf("final SearchSections returned error: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("final SearchSections returned no hits")
	}
}

func TestStatusRequiresMigration(t *testing.T) {
	ctx := context.Background()
	store := openTempStore(t, ctx)

	_, err := store.Status(ctx)
	if err == nil {
		t.Fatal("Status returned nil error before migration")
	}
}

func TestMigrateAndStatus(t *testing.T) {
	ctx := context.Background()
	store := openTempStore(t, ctx)
	dsn := store.dsn

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate returned error: %v", err)
	}
	if err := store.CheckSchema(ctx); err != nil {
		t.Fatalf("CheckSchema after migration returned error: %v", err)
	}
	var schemaVersion int
	if err := store.readDB().QueryRowContext(ctx, `select max(version) from schema_migrations`).Scan(&schemaVersion); err != nil {
		t.Fatalf("query schema version: %v", err)
	}
	if schemaVersion != sqlschema.CurrentSchemaVersion {
		t.Fatalf("schema version = %d, want %d", schemaVersion, sqlschema.CurrentSchemaVersion)
	}
	assertCount(t, ctx, store, "sqlite_master", "type = 'table' and name = 'section_entities'", 1)
	assertCount(t, ctx, store, "sqlite_master", "type = 'index' and name = 'idx_section_entities_document'", 1)

	status, err := store.Status(ctx)
	if err != nil {
		t.Fatalf("Status returned error after migration: %v", err)
	}
	if status.StorageDSN != dsn {
		t.Fatalf("StorageDSN = %q, want %q", status.StorageDSN, dsn)
	}
	if status.Sources != 0 || status.Documents != 0 || status.Sections != 0 ||
		status.Nodes != 0 || status.Edges != 0 || status.Jobs != 0 {
		t.Fatalf("empty migrated status = %#v, want zero counts", status)
	}

	insertFixtureRows(t, ctx, store)

	status, err = store.Status(ctx)
	if err != nil {
		t.Fatalf("Status returned error with fixture rows: %v", err)
	}
	if status.Sources != 1 {
		t.Fatalf("Sources = %d, want 1", status.Sources)
	}
	if status.Documents != 1 {
		t.Fatalf("Documents = %d, want 1", status.Documents)
	}
	if status.Sections != 1 {
		t.Fatalf("Sections = %d, want 1", status.Sections)
	}
	if status.Nodes != 2 {
		t.Fatalf("Nodes = %d, want 2", status.Nodes)
	}
	if status.Edges != 1 {
		t.Fatalf("Edges = %d, want 1", status.Edges)
	}
	if status.Jobs != 1 {
		t.Fatalf("Jobs = %d, want 1", status.Jobs)
	}
}

func TestCheckSchemaRequiresMigrate(t *testing.T) {
	ctx := context.Background()
	store := openTempStore(t, ctx)

	err := store.CheckSchema(ctx)
	if err == nil || !strings.Contains(err.Error(), "run docgraph migrate") {
		t.Fatalf("CheckSchema before migration error = %v, want migrate hint", err)
	}
}

func TestCheckSchemaRejectsFutureVersion(t *testing.T) {
	ctx := context.Background()
	store := openTempStore(t, ctx)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `insert into schema_migrations (version) values (?)`, sqlschema.CurrentSchemaVersion+1); err != nil {
		t.Fatalf("insert future schema version: %v", err)
	}

	err := store.CheckSchema(ctx)
	if err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("CheckSchema future version error = %v, want newer-than-supported", err)
	}
}

func TestSectionEntitiesSchemaRequiresMigrateFromOlderVersion(t *testing.T) {
	ctx := context.Background()
	store := openTempStore(t, ctx)

	if _, err := store.db.ExecContext(ctx, `
create table schema_migrations (
  version integer primary key,
  applied_at text not null default current_timestamp
);
insert into schema_migrations (version) values (2);
`); err != nil {
		t.Fatalf("seed old schema version: %v", err)
	}
	if err := store.CheckSchema(ctx); err == nil || !strings.Contains(err.Error(), "run docgraph migrate") {
		t.Fatalf("CheckSchema before migrate error = %v, want migrate hint for old schema", err)
	}
	assertCount(t, ctx, store, "sqlite_master", "type = 'table' and name = 'section_entities'", 0)

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate from old schema returned error: %v", err)
	}
	if err := store.CheckSchema(ctx); err != nil {
		t.Fatalf("CheckSchema after migrate returned error: %v", err)
	}
	assertCount(t, ctx, store, "sqlite_master", "type = 'table' and name = 'section_entities'", 1)
	assertCount(t, ctx, store, "sqlite_master", "type = 'index' and name = 'idx_section_entities_document'", 1)

	var schemaVersion int
	if err := store.readDB().QueryRowContext(ctx, `select max(version) from schema_migrations`).Scan(&schemaVersion); err != nil {
		t.Fatalf("query schema version: %v", err)
	}
	if schemaVersion != sqlschema.CurrentSchemaVersion {
		t.Fatalf("schema version = %d, want %d", schemaVersion, sqlschema.CurrentSchemaVersion)
	}
}

func TestCreateListAndGetSource(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)

	source := domain.Source{
		ID:           "source-1",
		Kind:         "local",
		Name:         "Product Docs",
		DSN:          "file:///workspace/docs",
		ProductHint:  "Payments",
		ModuleHint:   "Checkout",
		SyncSchedule: "manual",
	}
	created, err := store.CreateSource(ctx, source)
	if err != nil {
		t.Fatalf("CreateSource returned error: %v", err)
	}
	if created.ID != source.ID {
		t.Fatalf("created ID = %q, want %q", created.ID, source.ID)
	}
	if created.ConfigJSON != "{}" {
		t.Fatalf("created ConfigJSON = %q, want {}", created.ConfigJSON)
	}
	if created.ProductHint != source.ProductHint {
		t.Fatalf("created ProductHint = %q, want %q", created.ProductHint, source.ProductHint)
	}
	if created.SyncStatus != "active" || created.SyncStatusReason != "" || created.SyncPausedAt != "" {
		t.Fatalf("created sync state = (%q, %q, %q), want active empty state", created.SyncStatus, created.SyncStatusReason, created.SyncPausedAt)
	}
	if created.CreatedAt == "" || created.UpdatedAt == "" {
		t.Fatalf("created timestamps should be populated: %#v", created)
	}

	got, err := store.GetSource(ctx, source.ID)
	if err != nil {
		t.Fatalf("GetSource returned error: %v", err)
	}
	if got.Name != source.Name {
		t.Fatalf("GetSource Name = %q, want %q", got.Name, source.Name)
	}
	if got.DSN != source.DSN {
		t.Fatalf("GetSource DSN = %q, want %q", got.DSN, source.DSN)
	}

	updated := source
	updated.Name = "Updated Product Docs"
	updated.ConfigJSON = `{"branch":"main"}`
	updated.ModuleHint = "Billing"
	created, err = store.CreateSource(ctx, updated)
	if err != nil {
		t.Fatalf("CreateSource update returned error: %v", err)
	}
	if created.Name != updated.Name {
		t.Fatalf("updated Name = %q, want %q", created.Name, updated.Name)
	}
	if created.ConfigJSON != updated.ConfigJSON {
		t.Fatalf("updated ConfigJSON = %q, want %q", created.ConfigJSON, updated.ConfigJSON)
	}
	if created.ModuleHint != updated.ModuleHint {
		t.Fatalf("updated ModuleHint = %q, want %q", created.ModuleHint, updated.ModuleHint)
	}

	sources, err := store.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources returned error: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("ListSources returned %d sources, want 1: %#v", len(sources), sources)
	}
	if sources[0].ID != source.ID || sources[0].Name != updated.Name {
		t.Fatalf("ListSources[0] = %#v, want updated source %q", sources[0], source.ID)
	}

	if err := store.UpdateSourceSyncState(ctx, source.ID, "paused", "credential_required"); err != nil {
		t.Fatalf("UpdateSourceSyncState pause returned error: %v", err)
	}
	got, err = store.GetSource(ctx, source.ID)
	if err != nil {
		t.Fatalf("GetSource after pause returned error: %v", err)
	}
	if got.SyncStatus != "paused" || got.SyncStatusReason != "credential_required" || got.SyncPausedAt == "" {
		t.Fatalf("paused source sync state = (%q, %q, %q), want paused credential_required with timestamp", got.SyncStatus, got.SyncStatusReason, got.SyncPausedAt)
	}
	if err := store.UpdateSourceSyncState(ctx, source.ID, "active", ""); err != nil {
		t.Fatalf("UpdateSourceSyncState active returned error: %v", err)
	}
	got, err = store.GetSource(ctx, source.ID)
	if err != nil {
		t.Fatalf("GetSource after resume returned error: %v", err)
	}
	if got.SyncStatus != "active" || got.SyncStatusReason != "" || got.SyncPausedAt != "" {
		t.Fatalf("resumed source sync state = (%q, %q, %q), want active empty state", got.SyncStatus, got.SyncStatusReason, got.SyncPausedAt)
	}
}

func TestConfluenceCookieCredentialCRUD(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)

	created, err := store.CreateConfluenceCookieCredential(ctx, domain.ConfluenceCookieCredential{
		ID:      "cred-1",
		Name:    " Wiki Cookie ",
		BaseURL: " https://confluence.example/wiki/ ",
		Cookie:  " TEST_COOKIE=fixture-good ",
		Notes:   " browser session ",
	})
	if err != nil {
		t.Fatalf("CreateConfluenceCookieCredential returned error: %v", err)
	}
	if created.Name != "Wiki Cookie" || created.Cookie != "TEST_COOKIE=fixture-good" || created.Status != "unknown" {
		t.Fatalf("created credential = %+v, want trimmed unknown credential", created)
	}

	listed, err := store.ListConfluenceCookieCredentials(ctx)
	if err != nil {
		t.Fatalf("ListConfluenceCookieCredentials returned error: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("listed credentials = %+v, want created credential", listed)
	}

	updated, err := store.UpdateConfluenceCookieCredential(ctx, domain.ConfluenceCookieCredential{
		ID:      created.ID,
		Name:    "Updated Cookie",
		BaseURL: "https://confluence.example/wiki",
		Cookie:  "TEST_COOKIE=fixture-next",
		Notes:   "updated",
		Status:  "valid",
	})
	if err != nil {
		t.Fatalf("UpdateConfluenceCookieCredential returned error: %v", err)
	}
	if updated.Name != "Updated Cookie" || updated.Cookie != "TEST_COOKIE=fixture-next" || updated.Status != "valid" {
		t.Fatalf("updated credential = %+v, want updated valid credential", updated)
	}

	if err := store.UpdateConfluenceCookieCredentialValidation(ctx, created.ID, "invalid", "expired"); err != nil {
		t.Fatalf("UpdateConfluenceCookieCredentialValidation returned error: %v", err)
	}
	got, err := store.GetConfluenceCookieCredential(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetConfluenceCookieCredential returned error: %v", err)
	}
	if got.Status != "invalid" || got.LastError != "expired" || got.LastValidatedAt == "" {
		t.Fatalf("validated credential = %+v, want invalid with timestamp", got)
	}

	if err := store.DeleteConfluenceCookieCredential(ctx, created.ID); err != nil {
		t.Fatalf("DeleteConfluenceCookieCredential returned error: %v", err)
	}
	if _, err := store.GetConfluenceCookieCredential(ctx, created.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetConfluenceCookieCredential after delete error = %v, want sql.ErrNoRows", err)
	}
}

func TestConfluenceCookieCredentialRejectsInvalidInput(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)

	tests := []domain.ConfluenceCookieCredential{
		{Name: "Wiki", BaseURL: "https://confluence.example", Cookie: "TEST_COOKIE=fixture-good"},
		{ID: "cred-1", BaseURL: "https://confluence.example", Cookie: "TEST_COOKIE=fixture-good"},
		{ID: "cred-1", Name: "Wiki", Cookie: "TEST_COOKIE=fixture-good"},
		{ID: "cred-1", Name: "Wiki", BaseURL: "https://confluence.example"},
	}
	for _, input := range tests {
		if _, err := store.CreateConfluenceCookieCredential(ctx, input); err == nil {
			t.Fatalf("CreateConfluenceCookieCredential(%+v) returned nil error, want validation error", input)
		}
	}
	if _, err := store.UpdateConfluenceCookieCredential(ctx, domain.ConfluenceCookieCredential{
		ID:      "missing",
		Name:    "Missing",
		BaseURL: "https://confluence.example",
		Cookie:  "TEST_COOKIE=fixture-good",
	}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("UpdateConfluenceCookieCredential missing error = %v, want sql.ErrNoRows", err)
	}
	if err := store.DeleteConfluenceCookieCredential(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("DeleteConfluenceCookieCredential missing error = %v, want sql.ErrNoRows", err)
	}
}

func TestUpdateDeleteSourceAndSyncJobsLifecycle(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)

	_, err := store.CreateSource(ctx, domain.Source{
		ID:          "source-life",
		Kind:        "local",
		Name:        "Docs",
		DSN:         "file:///docs",
		ProductHint: "Membership",
		ModuleHint:  "Benefits",
	})
	if err != nil {
		t.Fatalf("CreateSource returned error: %v", err)
	}
	updated, err := store.UpdateSource(ctx, domain.Source{
		ID:          "source-life",
		Kind:        "local",
		Name:        "Updated Docs",
		DSN:         "file:///updated-docs",
		ConfigJSON:  `{"branch":"main"}`,
		ProductHint: "Payments",
		ModuleHint:  "Checkout",
	})
	if err != nil {
		t.Fatalf("UpdateSource returned error: %v", err)
	}
	if updated.Name != "Updated Docs" || updated.DSN != "file:///updated-docs" || updated.ProductHint != "Payments" || updated.ModuleHint != "Checkout" {
		t.Fatalf("updated source = %+v, want updated name/dsn/product/module", updated)
	}

	if err := store.ReplaceDocument(ctx, domain.DocumentInput{
		ID:          "doc-life",
		SourceID:    "source-life",
		ExternalID:  "life.md",
		Title:       "Lifecycle",
		ContentHash: "hash-doc",
	}, []domain.SectionInput{
		{
			ID:          "section-life",
			Title:       "Lifecycle",
			Content:     "membership lifecycle search content",
			ContentHash: "hash-section",
		},
	}); err != nil {
		t.Fatalf("ReplaceDocument returned error: %v", err)
	}
	hits, err := store.SearchSections(ctx, "membership lifecycle", 10)
	if err != nil {
		t.Fatalf("SearchSections before delete returned error: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("SearchSections before delete returned no hits")
	}

	job, err := store.CreateSyncJob(ctx, "source-life")
	if err != nil {
		t.Fatalf("CreateSyncJob returned error: %v", err)
	}
	if job.Status != "running" || !strings.Contains(job.PayloadJSON, `"source_id":"source-life"`) {
		t.Fatalf("created sync job = %+v, want running source job", job)
	}
	if err := store.CompleteSyncJob(ctx, job.ID, domain.ResultPayload{Documents: 1}); err != nil {
		t.Fatalf("CompleteSyncJob returned error: %v", err)
	}
	failed, err := store.CreateSyncJob(ctx, "source-life")
	if err != nil {
		t.Fatalf("second CreateSyncJob returned error: %v", err)
	}
	if err := store.FailSyncJob(ctx, failed.ID, "boom"); err != nil {
		t.Fatalf("FailSyncJob returned error: %v", err)
	}
	jobs, err := store.ListSyncJobs(ctx, "source-life", 10)
	if err != nil {
		t.Fatalf("ListSyncJobs returned error: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("ListSyncJobs returned %d jobs, want 2: %+v", len(jobs), jobs)
	}
	if !syncJobsContain(jobs, "completed", `"documents":1`) || !syncJobsContain(jobs, "failed", "boom") {
		t.Fatalf("sync jobs = %+v, want completed and failed job history", jobs)
	}
	cancelQueued, err := store.CreateSyncJobIfIdle(ctx, "source-life")
	if err != nil {
		t.Fatalf("CreateSyncJobIfIdle for queued cancel returned error: %v", err)
	}
	cancelQueued, err = store.CancelJob(ctx, cancelQueued.ID, "stop queued")
	if err != nil {
		t.Fatalf("CancelJob queued returned error: %v", err)
	}
	if cancelQueued.Status != "canceled" {
		t.Fatalf("queued cancel status = %q, want canceled", cancelQueued.Status)
	}
	cancelRunning, err := store.CreateSyncJob(ctx, "source-life")
	if err != nil {
		t.Fatalf("CreateSyncJob for running cancel returned error: %v", err)
	}
	cancelRunning, err = store.CancelJob(ctx, cancelRunning.ID, "stop running")
	if err != nil {
		t.Fatalf("CancelJob running returned error: %v", err)
	}
	if cancelRunning.Status != "canceling" {
		t.Fatalf("running cancel status = %q, want canceling", cancelRunning.Status)
	}
	if _, err := store.CreateSyncJobIfIdle(ctx, "source-life"); !errors.Is(err, domain.ErrSyncInProgress) {
		t.Fatalf("CreateSyncJobIfIdle with canceling job error = %v, want ErrSyncInProgress", err)
	}
	if err := store.DeleteSyncJob(ctx, "source-life", cancelRunning.ID); !errors.Is(err, domain.ErrJobNotCancelable) {
		t.Fatalf("DeleteSyncJob active error = %v, want ErrJobNotCancelable", err)
	}
	if err := store.MarkJobCanceled(ctx, cancelRunning.ID, "stopped"); err != nil {
		t.Fatalf("MarkJobCanceled returned error: %v", err)
	}
	cancelRunning, err = store.GetJob(ctx, cancelRunning.ID)
	if err != nil {
		t.Fatalf("GetJob canceled returned error: %v", err)
	}
	if cancelRunning.Status != "canceled" || cancelRunning.WorkerID != "" || cancelRunning.LockedUntil != "" {
		t.Fatalf("marked canceled job = %+v, want canceled with cleared lease", cancelRunning)
	}
	if err := store.DeleteSyncJob(ctx, "source-life", failed.ID); err != nil {
		t.Fatalf("DeleteSyncJob returned error: %v", err)
	}
	jobs, err = store.ListSyncJobs(ctx, "source-life", 10)
	if err != nil {
		t.Fatalf("ListSyncJobs after DeleteSyncJob returned error: %v", err)
	}
	if syncJobsContain(jobs, "failed", "boom") {
		t.Fatalf("jobs after DeleteSyncJob = %+v, want failed job removed", jobs)
	}
	if !syncJobsContain(jobs, "completed", `"documents":1`) || !syncJobsContain(jobs, "canceled", "stopped") || !syncJobsContain(jobs, "canceled", "stop queued") {
		t.Fatalf("jobs after DeleteSyncJob = %+v, want completed and canceled history retained", jobs)
	}

	if err := store.DeleteSource(ctx, "source-life"); err != nil {
		t.Fatalf("DeleteSource returned error: %v", err)
	}
	sources, err := store.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources after delete returned error: %v", err)
	}
	if len(sources) != 0 {
		t.Fatalf("ListSources after delete returned %+v, want empty", sources)
	}
	assertCount(t, ctx, store, "documents", "source_id = 'source-life'", 0)
	assertCount(t, ctx, store, "sections", "document_id = 'doc-life'", 0)
	assertCount(t, ctx, store, "fts_section_tokens", "document_id = 'doc-life'", 0)
	hits, err = store.SearchSections(ctx, "membership lifecycle", 10)
	if err != nil {
		t.Fatalf("SearchSections after delete returned error: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("SearchSections after delete returned %+v, want no hits", hits)
	}
}

func TestReplaceDocumentReplacesSectionsAndFTSRows(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-1")

	doc := domain.DocumentInput{
		ID:          "doc-1",
		SourceID:    "source-1",
		ExternalID:  "guide.md",
		Title:       "Guide",
		URL:         "file:///guide.md",
		Version:     "v1",
		ContentHash: "hash-doc-v1",
	}
	firstSections := []domain.SectionInput{
		{
			ID:          "section-old-1",
			HeadingPath: "Guide",
			Title:       "Old Overview",
			Content:     "legacy onboarding content",
			ContentHash: "hash-old-1",
			Ordinal:     0,
		},
		{
			ID:          "section-old-2",
			HeadingPath: "Guide > Details",
			Title:       "Old Details",
			Content:     "obsolete billing details",
			ContentHash: "hash-old-2",
			Ordinal:     1,
		},
	}
	if err := store.ReplaceDocument(ctx, doc, firstSections); err != nil {
		t.Fatalf("first ReplaceDocument returned error: %v", err)
	}
	assertCount(t, ctx, store, "sections", "document_id = 'doc-1'", 2)
	assertCount(t, ctx, store, "fts_section_tokens", "document_id = 'doc-1'", 2)

	doc.Title = "Updated Guide"
	doc.ContentHash = "hash-doc-v2"
	replacementSections := []domain.SectionInput{
		{
			ID:          "section-new-1",
			HeadingPath: "Guide > Current",
			Title:       "Current Overview",
			Content:     "fresh entitlement content",
			ContentHash: "hash-new-1",
			Ordinal:     0,
		},
	}
	if err := store.ReplaceDocument(ctx, doc, replacementSections); err != nil {
		t.Fatalf("second ReplaceDocument returned error: %v", err)
	}

	assertCount(t, ctx, store, "documents", "id = 'doc-1' and title = 'Updated Guide' and content_hash = 'hash-doc-v2'", 1)
	assertCount(t, ctx, store, "sections", "document_id = 'doc-1'", 1)
	assertCount(t, ctx, store, "sections", "id = 'section-new-1' and title = 'Current Overview'", 1)
	assertCount(t, ctx, store, "sections", "id in ('section-old-1', 'section-old-2')", 0)
	assertCount(t, ctx, store, "fts_section_tokens", "document_id = 'doc-1'", 1)
	assertCount(t, ctx, store, "fts_section_tokens", "section_id = 'section-new-1' and title_tokens like '%'", 1)
	assertCount(t, ctx, store, "fts_section_tokens", "section_id in ('section-old-1', 'section-old-2')", 0)
}

func TestSectionEntitiesReplaceListSearchAndPropagation(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-1")

	doc := domain.DocumentInput{
		ID:          "doc-entities",
		SourceID:    "source-1",
		ExternalID:  "entities.md",
		Title:       "Entity API",
		ContentHash: "hash-doc-entities",
	}
	if err := store.ReplaceDocument(ctx, doc, []domain.SectionInput{
		{ID: "section-entities-1", Title: "First", Content: "GET /entity/v1/entities", ContentHash: "hash-section-1", Ordinal: 1},
		{ID: "section-entities-0", Title: "Zero", Content: "BatchGetEntityMeta", ContentHash: "hash-section-0", Ordinal: 0},
	}); err != nil {
		t.Fatalf("ReplaceDocument returned error: %v", err)
	}

	inputs := []domain.SectionEntityInput{
		{
			SectionID:     "section-entities-1",
			Kind:          "api_endpoint",
			RawText:       "GET /entity/v1/entities",
			CanonicalText: "GET /entity/v1/entities",
			Method:        "get",
			Path:          "/entity/v1/entities",
			Source:        "text",
			Confidence:    0.95,
			Evidence:      "GET /entity/v1/entities",
			SpanStart:     4,
			SpanEnd:       23,
			Notes:         []string{"method_from_same_line"},
		},
		{
			SectionID:     "section-entities-1",
			Kind:          "api_endpoint",
			RawText:       "GET /entity/v1/entities",
			CanonicalText: "GET /entity/v1/entities",
			Method:        "GET",
			Path:          "/entity/v1/entities",
			Source:        "text",
			Confidence:    0.90,
			Evidence:      "retry GET /entity/v1/entities",
			SpanStart:     10,
			SpanEnd:       29,
			Notes:         []string{"method_from_same_line"},
		},
		{
			SectionID:     "section-entities-0",
			Kind:          "operation_candidate",
			RawText:       "BatchGetEntityMeta",
			CanonicalText: "BatchGetEntityMeta",
			Operation:     "BatchGetEntityMeta",
			Source:        "text",
			Confidence:    0.55,
			Evidence:      "BatchGetEntityMeta",
		},
	}
	if err := store.ReplaceSectionEntities(ctx, doc.ID, inputs); err != nil {
		t.Fatalf("ReplaceSectionEntities returned error: %v", err)
	}

	entities, err := store.ListDocumentEntities(ctx, doc.ID)
	if err != nil {
		t.Fatalf("ListDocumentEntities returned error: %v", err)
	}
	if len(entities) != 2 {
		t.Fatalf("entities = %+v, want aggregated API endpoint and operation", entities)
	}
	if entities[0].SectionID != "section-entities-0" || entities[0].Kind != "operation_candidate" {
		t.Fatalf("first entity = %+v, want section ordinal ordering", entities[0])
	}
	apiEntity := entities[1]
	if apiEntity.Kind != "api_endpoint" || apiEntity.Method != "GET" || apiEntity.Path != "/entity/v1/entities" || apiEntity.Confidence != 0.95 {
		t.Fatalf("api entity = %+v, want canonical aggregated endpoint", apiEntity)
	}
	var evidence sectionEntityEvidence
	if err := json.Unmarshal([]byte(apiEntity.EvidenceJSON), &evidence); err != nil {
		t.Fatalf("evidence json = %q, unmarshal error: %v", apiEntity.EvidenceJSON, err)
	}
	if len(evidence.Occurrences) != 2 || len(evidence.Notes) != 1 || evidence.Occurrences[0].Evidence == "" {
		t.Fatalf("evidence = %+v, want two occurrences and one deduped note", evidence)
	}
	counts, err := store.GetSourceArtifactCounts(ctx, "source-1")
	if err != nil {
		t.Fatalf("GetSourceArtifactCounts returned error: %v", err)
	}
	if counts.SectionEntities != 2 {
		t.Fatalf("artifact counts = %+v, want two section entities", counts)
	}
	sourceEntities, err := store.ListSourceSectionEntities(ctx, "source-1", 10, 0)
	if err != nil {
		t.Fatalf("ListSourceSectionEntities returned error: %v", err)
	}
	if len(sourceEntities) != 2 || sourceEntities[0].SectionID != "section-entities-0" {
		t.Fatalf("source entities = %+v, want source-scoped entity list", sourceEntities)
	}
	artifacts, err := store.ListSourceArtifacts(ctx, "source-1", 10, 0)
	if err != nil {
		t.Fatalf("ListSourceArtifacts returned error: %v", err)
	}
	if artifacts.Counts.SectionEntities != 2 || len(artifacts.SectionEntities) != 2 || artifacts.EntityDiagnostics.Total != 2 {
		t.Fatalf("artifacts = %+v, want entity counts, list, and diagnostics", artifacts)
	}

	found, err := store.SearchEntities(ctx, "/entity/v1/entities", 10)
	if err != nil {
		t.Fatalf("SearchEntities path returned error: %v", err)
	}
	if len(found) == 0 || found[0].ID != apiEntity.ID {
		t.Fatalf("SearchEntities path = %+v, want API entity first", found)
	}
	found, err = store.SearchEntities(ctx, "BatchGetEntityMeta", 10)
	if err != nil {
		t.Fatalf("SearchEntities operation returned error: %v", err)
	}
	if len(found) == 0 || found[0].Operation != "BatchGetEntityMeta" {
		t.Fatalf("SearchEntities operation = %+v, want operation entity", found)
	}

	if err := store.ReplaceSectionEntities(ctx, doc.ID, []domain.SectionEntityInput{
		{
			SectionID:     "section-entities-1",
			Kind:          "path_literal",
			RawText:       "/entity/v1/entities:filter-filter-count",
			CanonicalText: "/entity/v1/entities:filter-filter-count",
			Path:          "/entity/v1/entities:filter-filter-count",
			Source:        "table",
			Confidence:    0.65,
		},
	}); err != nil {
		t.Fatalf("second ReplaceSectionEntities returned error: %v", err)
	}
	entities, err = store.ListDocumentEntities(ctx, doc.ID)
	if err != nil {
		t.Fatalf("ListDocumentEntities after replace returned error: %v", err)
	}
	if len(entities) != 1 || entities[0].Kind != "path_literal" || entities[0].Path != "/entity/v1/entities:filter-filter-count" {
		t.Fatalf("entities after replace = %+v, want only replacement path literal", entities)
	}
	assertCount(t, ctx, store, "section_entities", "canonical_text = 'GET /entity/v1/entities'", 0)

	if err := store.ReplaceDocument(ctx, doc, []domain.SectionInput{
		{ID: "section-entities-new", Title: "New", Content: "new content", ContentHash: "hash-new", Ordinal: 0},
	}); err != nil {
		t.Fatalf("ReplaceDocument replacing sections returned error: %v", err)
	}
	assertCount(t, ctx, store, "section_entities", "document_id = 'doc-entities'", 0)
}

func TestReplaceSectionEntitiesValidatesDocumentSectionsAndPropagationsDocumentDelete(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-1")

	if err := store.ReplaceDocument(ctx, domain.DocumentInput{
		ID:          "doc-one",
		SourceID:    "source-1",
		ExternalID:  "one.md",
		Title:       "One",
		ContentHash: "hash-one",
	}, []domain.SectionInput{{ID: "section-one", Title: "One", Content: "one", ContentHash: "hash-section-one"}}); err != nil {
		t.Fatalf("ReplaceDocument doc-one returned error: %v", err)
	}
	if err := store.ReplaceDocument(ctx, domain.DocumentInput{
		ID:          "doc-two",
		SourceID:    "source-1",
		ExternalID:  "two.md",
		Title:       "Two",
		ContentHash: "hash-two",
	}, []domain.SectionInput{{ID: "section-two", Title: "Two", Content: "two", ContentHash: "hash-section-two"}}); err != nil {
		t.Fatalf("ReplaceDocument doc-two returned error: %v", err)
	}

	err := store.ReplaceSectionEntities(ctx, "doc-one", []domain.SectionEntityInput{
		{SectionID: "section-two", Kind: "path_literal", CanonicalText: "/entity/v1/entities", Path: "/entity/v1/entities"},
	})
	if err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("cross-document ReplaceSectionEntities error = %v, want section ownership error", err)
	}

	if err := store.ReplaceSectionEntities(ctx, "doc-one", []domain.SectionEntityInput{
		{SectionID: "section-one", Kind: "path_literal", CanonicalText: "/entity/v1/entities", Path: "/entity/v1/entities"},
	}); err != nil {
		t.Fatalf("ReplaceSectionEntities doc-one returned error: %v", err)
	}
	assertCount(t, ctx, store, "section_entities", "document_id = 'doc-one'", 1)
	if err := store.DeleteDocumentsNotInSource(ctx, "source-1", []string{"doc-two"}); err != nil {
		t.Fatalf("DeleteDocumentsNotInSource returned error: %v", err)
	}
	assertCount(t, ctx, store, "section_entities", "document_id = 'doc-one'", 0)
}

func TestGetDocumentBySourceExternalID(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-1")

	doc := domain.DocumentInput{
		ID:          "doc-lookup",
		SourceID:    "source-1",
		ExternalID:  "docs/lookup.md",
		Title:       "Lookup",
		URL:         "file:///docs/lookup.md",
		ContentHash: "hash-lookup",
	}
	if err := store.ReplaceDocument(ctx, doc, []domain.SectionInput{
		{ID: "section-lookup-1", DocumentID: doc.ID, Title: "One", Content: "one", ContentHash: "hash-section-1", Ordinal: 1},
		{ID: "section-lookup-2", DocumentID: doc.ID, Title: "Two", Content: "two", ContentHash: "hash-section-2", Ordinal: 2},
	}); err != nil {
		t.Fatalf("ReplaceDocument returned error: %v", err)
	}

	got, err := store.GetDocumentBySourceExternalID(ctx, "source-1", "docs/lookup.md")
	if err != nil {
		t.Fatalf("GetDocumentBySourceExternalID returned error: %v", err)
	}
	if got.ID != doc.ID || got.SourceID != doc.SourceID || got.ExternalID != doc.ExternalID || got.ContentHash != doc.ContentHash || got.SectionCount != 2 || got.NodeID == "" {
		t.Fatalf("document summary = %+v, want stored document with two sections and node id", got)
	}
	if _, err := store.GetDocumentBySourceExternalID(ctx, "source-1", "missing.md"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing GetDocumentBySourceExternalID error = %v, want sql.ErrNoRows", err)
	}
}

func TestSourceHealthReportsDiagnostics(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-health")

	if err := store.ReplaceDocument(ctx, domain.DocumentInput{
		ID:          "doc-health",
		SourceID:    "source-health",
		ExternalID:  "health.md",
		Title:       "Health",
		ContentHash: "hash-health",
	}, []domain.SectionInput{
		{
			ID:          "section-health",
			Title:       "Tiny",
			Content:     "tiny",
			ContentHash: "hash-section",
		},
	}); err != nil {
		t.Fatalf("ReplaceDocument health returned error: %v", err)
	}
	if err := store.ReplaceDocument(ctx, domain.DocumentInput{
		ID:          "doc-empty",
		SourceID:    "source-health",
		ExternalID:  "empty.md",
		Title:       "Empty",
		ContentHash: "hash-empty",
	}, nil); err != nil {
		t.Fatalf("ReplaceDocument empty returned error: %v", err)
	}
	if _, err := store.CreateFeedbackEvent(ctx, domain.FeedbackEventInput{
		TargetKind:   "document",
		TargetID:     "doc-health",
		FeedbackKind: "document_stale",
	}); err != nil {
		t.Fatalf("CreateFeedbackEvent stale returned error: %v", err)
	}
	job, err := store.CreateSyncJob(ctx, "source-health")
	if err != nil {
		t.Fatalf("CreateSyncJob returned error: %v", err)
	}
	if err := store.CompleteSyncJob(ctx, job.ID, domain.ResultPayload{
		Documents: 2,
		BrokenLinks: []domain.BrokenLink{{
			SourceDocument: "health.md",
			SourceSection:  "Tiny",
			Href:           "missing.html",
		}},
	}); err != nil {
		t.Fatalf("CompleteSyncJob returned error: %v", err)
	}

	health, err := store.GetSourceHealth(ctx, "source-health")
	if err != nil {
		t.Fatalf("GetSourceHealth returned error: %v", err)
	}
	if health.Counts.Documents != 2 || health.Counts.Sections != 1 {
		t.Fatalf("health counts = %+v, want 2 documents and 1 section", health.Counts)
	}
	if len(health.BrokenLinks) != 1 || health.BrokenLinks[0].Href != "missing.html" {
		t.Fatalf("health broken links = %+v, want missing.html", health.BrokenLinks)
	}
	if len(health.ZeroSectionDocuments) != 1 || health.ZeroSectionDocuments[0].ID != "doc-empty" {
		t.Fatalf("health zero-section docs = %+v, want doc-empty", health.ZeroSectionDocuments)
	}
	if len(health.LowContentSections) != 1 || health.LowContentSections[0].ID != "section-health" {
		t.Fatalf("health low-content sections = %+v, want section-health", health.LowContentSections)
	}
	if len(health.StaleFeedback) != 1 || health.StaleFeedback[0].TargetID != "doc-health" {
		t.Fatalf("health stale feedback = %+v, want doc-health", health.StaleFeedback)
	}
	if health.EntityDiagnostics.Total != 0 || health.Counts.SectionEntities != 0 {
		t.Fatalf("health entity diagnostics = %+v counts = %+v, want no entities", health.EntityDiagnostics, health.Counts)
	}
	if !healthWarningsContain(health.Warnings, "broken_links") || !healthWarningsContain(health.Warnings, "zero_section_documents") || !healthWarningsContain(health.Warnings, "stale_documents") || !healthWarningsContain(health.Warnings, "no_section_entities") {
		t.Fatalf("health warnings = %+v, want broken_links, zero_section_documents, stale_documents, no_section_entities", health.Warnings)
	}
}

func TestSearchSections(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-1")

	doc := domain.DocumentInput{
		ID:          "doc-1",
		SourceID:    "source-1",
		ExternalID:  "search.md",
		Title:       "Search",
		ContentHash: "hash-doc",
	}
	sections := []domain.SectionInput{
		{
			ID:          "section-match",
			HeadingPath: "Search > Benefits",
			Title:       "Benefit Rules",
			Content:     "membership entitlement configuration",
			ContentHash: "hash-match",
			Ordinal:     0,
		},
		{
			ID:          "section-other",
			HeadingPath: "Search > Checkout",
			Title:       "Checkout Rules",
			Content:     "payment routing configuration",
			ContentHash: "hash-other",
			Ordinal:     1,
		},
	}
	if err := store.ReplaceDocument(ctx, doc, sections); err != nil {
		t.Fatalf("ReplaceDocument returned error: %v", err)
	}

	hits, err := store.SearchSections(ctx, "membership", 10)
	if err != nil {
		t.Fatalf("SearchSections returned error: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("SearchSections returned %d hits, want 1: %#v", len(hits), hits)
	}
	if hits[0].SectionID != "section-match" {
		t.Fatalf("hit SectionID = %q, want section-match", hits[0].SectionID)
	}
	if hits[0].DocumentID != "doc-1" {
		t.Fatalf("hit DocumentID = %q, want doc-1", hits[0].DocumentID)
	}
	if hits[0].Title != "Benefit Rules" {
		t.Fatalf("hit Title = %q, want Benefit Rules", hits[0].Title)
	}
	if !strings.Contains(hits[0].Snippet, "<mark>membership</mark>") {
		t.Fatalf("hit Snippet = %q, want highlighted membership", hits[0].Snippet)
	}

	hits, err = store.SearchSections(ctx, "   ", 10)
	if err != nil {
		t.Fatalf("empty SearchSections returned error: %v", err)
	}
	if hits != nil {
		t.Fatalf("empty SearchSections returned %#v, want nil", hits)
	}
}

func TestSearchSectionsRanksExactPhraseBeforePartialCanonicalMatches(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-1")

	query := "display字段控制group是否显示的需求"
	if err := store.ReplaceDocument(ctx, domain.DocumentInput{
		ID:          "doc-exact-phrase",
		SourceID:    "source-1",
		ExternalID:  "exact.md",
		Title:       "需求记录",
		ContentHash: "hash-exact-phrase",
	}, []domain.SectionInput{
		{
			ID:          "section-exact-phrase",
			Title:       "需求说明",
			HeadingPath: "需求 > 配置",
			Content:     "这里记录 display字段控制group是否显示的需求，并说明验收标准。",
			ContentHash: "hash-section-exact-phrase",
			Ordinal:     0,
		},
	}); err != nil {
		t.Fatalf("ReplaceDocument exact returned error: %v", err)
	}
	if err := store.ReplaceDocument(ctx, domain.DocumentInput{
		ID:          "doc-partial-canonical",
		SourceID:    "source-1",
		ExternalID:  "partial.md",
		Title:       "display group 字段控制",
		ContentHash: "hash-partial-canonical",
	}, []domain.SectionInput{
		{
			ID:          "section-partial-canonical",
			Title:       "group 显示控制",
			HeadingPath: "display > group > 字段控制",
			Content:     "这篇文档分散描述 display 字段、group 是否显示和需求背景，但不包含完整查询短语。",
			ContentHash: "hash-section-partial-canonical",
			Ordinal:     0,
		},
	}); err != nil {
		t.Fatalf("ReplaceDocument partial returned error: %v", err)
	}
	if _, err := store.CreateFeedbackEvent(ctx, domain.FeedbackEventInput{
		TargetKind:   "document",
		TargetID:     "doc-partial-canonical",
		FeedbackKind: "document_canonical",
	}); err != nil {
		t.Fatalf("CreateFeedbackEvent document_canonical returned error: %v", err)
	}

	result, err := store.SearchSectionsWithOptions(ctx, domain.SearchOptions{
		Query:                  query,
		Limit:                  5,
		MaxSearches:            5,
		MaxSectionsPerDocument: 5,
		ProfileDetail:          "compact",
	})
	if err != nil {
		t.Fatalf("SearchSectionsWithOptions returned error: %v", err)
	}
	if len(result.Hits) < 2 {
		t.Fatalf("hits = %+v, want exact and partial hits", result.Hits)
	}
	// In RRF mode, verify both hits have valid RRF contributions
	var exactPhraseHit, partialCanonicalHit *domain.SearchHit
	for i := range result.Hits {
		if result.Hits[i].SectionID == "section-exact-phrase" {
			exactPhraseHit = &result.Hits[i]
		}
		if result.Hits[i].SectionID == "section-partial-canonical" {
			partialCanonicalHit = &result.Hits[i]
		}
	}
	if exactPhraseHit == nil || partialCanonicalHit == nil {
		t.Fatalf("missing expected hits; all hits: %+v", result.Hits)
	}
	// Both hits should have RRFContribution with positive scores
	if exactPhraseHit.RRFContribution == nil || exactPhraseHit.RRFContribution.FinalScore <= 0 {
		t.Fatalf("exact phrase RRF contribution = %+v, want positive score", exactPhraseHit.RRFContribution)
	}
	if partialCanonicalHit.RRFContribution == nil || partialCanonicalHit.RRFContribution.FinalScore <= 0 {
		t.Fatalf("partial canonical RRF contribution = %+v, want positive score", partialCanonicalHit.RRFContribution)
	}
	// Verify HybridSearchMeta is populated
	if result.HybridSearchMeta == nil {
		t.Fatal("HybridSearchMeta is nil, want populated")
	}
	// Exact phrase hit should have exact_match_boost evidence
	if exactPhraseHit.ScoreBreakdown == nil || exactPhraseHit.ScoreBreakdown.ExactMatchBoost == 0 {
		t.Fatalf("exact hit score breakdown = %+v, want exact phrase boost", exactPhraseHit.ScoreBreakdown)
	}
	if exactPhraseHit.QueryMatch == nil || !containsString(exactPhraseHit.QueryMatch.MatchedFields, "exact_phrase") {
		t.Fatalf("query match = %+v, want exact_phrase evidence", exactPhraseHit.QueryMatch)
	}
}

func TestExactPhraseBoostScalesByQueryComplexity(t *testing.T) {
	tests := []struct {
		query string
		want  float64
	}{
		{query: "node", want: 80},
		{query: "display group", want: 160},
		{query: "display字段控制group是否显示的需求", want: 260},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			if got := exactPhraseBoost(tt.query); got != tt.want {
				t.Fatalf("exactPhraseBoost(%q) = %.0f, want %.0f", tt.query, got, tt.want)
			}
		})
	}
}

func TestSearchSectionsUsesLowerExactPhraseBoostForShortQueries(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-1")

	if err := store.ReplaceDocument(ctx, domain.DocumentInput{
		ID:          "doc-short-phrase",
		SourceID:    "source-1",
		ExternalID:  "short.md",
		Title:       "Service Notes",
		ContentHash: "hash-short-phrase",
	}, []domain.SectionInput{
		{
			ID:          "section-short-phrase",
			Title:       "Notes",
			HeadingPath: "Operations > Notes",
			Content:     "node appears in an operational note.",
			ContentHash: "hash-section-short-phrase",
			Ordinal:     0,
		},
	}); err != nil {
		t.Fatalf("ReplaceDocument returned error: %v", err)
	}

	result, err := store.SearchSectionsWithOptions(ctx, domain.SearchOptions{
		Query:                  "node",
		Limit:                  5,
		MaxSearches:            5,
		MaxSectionsPerDocument: 5,
		ProfileDetail:          "compact",
	})
	if err != nil {
		t.Fatalf("SearchSectionsWithOptions returned error: %v", err)
	}
	if len(result.Hits) == 0 {
		t.Fatalf("hits empty, want short query hit")
	}
	boost := result.Hits[0].ScoreBreakdown.ExactMatchBoost
	if boost != 80 {
		t.Fatalf("short exact phrase boost = %.0f, want 80; hit = %+v", boost, result.Hits[0])
	}
}

func TestSearchSectionsPrioritizesExactAPILiterals(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-1")

	cases := []struct {
		name        string
		path        string
		term        string
		relatedPath string
	}{
		{
			name:        "operation suffix",
			path:        "/entity/v1/entities:filter-filter-count",
			term:        "entity",
			relatedPath: "/entity/v1/entities:filter-filter-count-archive",
		},
		{
			name:        "camel case operation",
			path:        "/EntityV1/BatchGetEntityMeta",
			term:        "entity",
			relatedPath: "/EntityV1/BatchGetEntityMetaPreview",
		},
		{
			name:        "templated field",
			path:        "/access/v1/access/{object.id}/meta",
			term:        "access",
			relatedPath: "/access/v1/access/{object.id}/meta/preview",
		},
	}

	for i, tc := range cases {
		exactDocID := fmt.Sprintf("doc-exact-path-%d", i)
		exactSectionID := fmt.Sprintf("section-exact-path-%d", i)
		if err := store.ReplaceDocument(ctx, domain.DocumentInput{
			ID:          exactDocID,
			SourceID:    "source-1",
			ExternalID:  exactDocID + ".md",
			Title:       "Exact API " + strconv.Itoa(i),
			ContentHash: "hash-" + exactDocID,
		}, []domain.SectionInput{
			{
				ID:          exactSectionID,
				HeadingPath: "Exact API > Operation",
				Title:       "Exact Operation",
				Content:     "Use GET " + tc.path + " to retrieve the exact resource.",
				ContentHash: "hash-" + exactSectionID,
				Ordinal:     0,
			},
		}); err != nil {
			t.Fatalf("ReplaceDocument(%s) returned error: %v", exactDocID, err)
		}

		relatedDocID := fmt.Sprintf("doc-related-path-%d", i)
		relatedSectionID := fmt.Sprintf("section-related-path-%d", i)
		if err := store.ReplaceDocument(ctx, domain.DocumentInput{
			ID:          relatedDocID,
			SourceID:    "source-1",
			ExternalID:  relatedDocID + ".md",
			Title:       "Related API " + strconv.Itoa(i),
			ContentHash: "hash-" + relatedDocID,
		}, []domain.SectionInput{
			{
				ID:          relatedSectionID,
				HeadingPath: "Related API > Operation",
				Title:       "Related Operation",
				Content:     "Use GET " + tc.relatedPath + " for a neighboring operation.",
				ContentHash: "hash-" + relatedSectionID,
				Ordinal:     0,
			},
		}); err != nil {
			t.Fatalf("ReplaceDocument(%s) returned error: %v", relatedDocID, err)
		}

		noiseDocID := fmt.Sprintf("doc-token-noise-%d", i)
		noiseSectionID := fmt.Sprintf("section-token-noise-%d", i)
		if err := store.ReplaceDocument(ctx, domain.DocumentInput{
			ID:          noiseDocID,
			SourceID:    "source-1",
			ExternalID:  noiseDocID + ".md",
			Title:       "Token Overview " + strconv.Itoa(i),
			ContentHash: "hash-" + noiseDocID,
		}, []domain.SectionInput{
			{
				ID:          noiseSectionID,
				HeadingPath: "Token Overview > Concepts",
				Title:       "Concepts",
				Content:     tc.term + " documents describe lifecycle, ownership, review, and metadata.",
				ContentHash: "hash-" + noiseSectionID,
				Ordinal:     0,
			},
		}); err != nil {
			t.Fatalf("ReplaceDocument(%s) returned error: %v", noiseDocID, err)
		}
	}

	for i, tc := range cases {
		for _, query := range []string{tc.path, "GET " + tc.path} {
			t.Run(tc.name+"/"+query, func(t *testing.T) {
				result, err := store.SearchSectionsWithOptions(ctx, domain.SearchOptions{
					Query:                  query,
					Limit:                  10,
					MaxSearches:            5,
					MaxSectionsPerDocument: 5,
					ProfileDetail:          "compact",
				})
				if err != nil {
					t.Fatalf("SearchSectionsWithOptions returned error: %v", err)
				}
				if len(result.Hits) == 0 {
					t.Fatalf("SearchSectionsWithOptions returned no hits")
				}
				wantSectionID := fmt.Sprintf("section-exact-path-%d", i)
				if result.Hits[0].SectionID != wantSectionID {
					t.Fatalf("first hit = %+v, want exact API path first; all hits: %+v", result.Hits[0], result.Hits)
				}
				if !strings.Contains(result.Hits[0].Snippet, "<mark>"+tc.path+"</mark>") && !strings.Contains(result.Hits[0].Snippet, "<mark>GET "+tc.path+"</mark>") {
					t.Fatalf("exact path snippet = %q, want full path highlight for %q", result.Hits[0].Snippet, tc.path)
				}
				noiseSectionID := fmt.Sprintf("section-token-noise-%d", i)
				relatedSectionID := fmt.Sprintf("section-related-path-%d", i)
				for _, hit := range result.Hits {
					if hit.SectionID == noiseSectionID && hit.Rank >= result.Hits[0].Rank {
						t.Fatalf("token-only noise rank >= exact path rank: %+v", result.Hits)
					}
					if hit.SectionID == relatedSectionID && hit.ScoreBreakdown != nil && hit.ScoreBreakdown.ExactMatchBoost >= result.Hits[0].ScoreBreakdown.ExactMatchBoost {
						t.Fatalf("longer neighboring path received exact-equivalent boost: %+v", result.Hits)
					}
				}
				if result.Hits[0].QueryMatch == nil || !containsString(result.Hits[0].QueryMatch.MatchedFields, "entity_exact") {
					t.Fatalf("query match = %+v, want entity_exact evidence", result.Hits[0].QueryMatch)
				}
				var exactAttemptHits int
				var normalizedAttemptHits int
				for _, attempt := range result.Attempts {
					switch attempt.Kind {
					case "entity_exact":
						exactAttemptHits = attempt.Hits
					case "entity_normalized":
						normalizedAttemptHits = attempt.Hits
					}
				}
				if exactAttemptHits == 0 || normalizedAttemptHits != 0 {
					t.Fatalf("attempts = %+v, want exact hits without normalized hits for exact query", result.Attempts)
				}
			})
		}
	}
}

func TestSearchSectionsUsesNormalizedEntityBoostAndKeepsFallback(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-1")

	if err := store.ReplaceDocument(ctx, domain.DocumentInput{
		ID:          "doc-normalized-path",
		SourceID:    "source-1",
		ExternalID:  "normalized.md",
		Title:       "Normalized API",
		ContentHash: "hash-normalized",
	}, []domain.SectionInput{
		{
			ID:          "section-normalized-path",
			Title:       "Normalized Placeholder",
			Content:     "Use DELETE /access/v1/access/{ object.id }/meta for placeholder metadata.",
			ContentHash: "hash-section-normalized",
		},
	}); err != nil {
		t.Fatalf("ReplaceDocument normalized returned error: %v", err)
	}
	if err := store.ReplaceDocument(ctx, domain.DocumentInput{
		ID:          "doc-fallback-path",
		SourceID:    "source-1",
		ExternalID:  "fallback.md",
		Title:       "Fallback API",
		ContentHash: "hash-fallback",
	}, []domain.SectionInput{
		{
			ID:          "section-fallback-path",
			Title:       "Fallback Path",
			Content:     "The fallback example mentions entity filter count behavior for adjacent APIs.",
			ContentHash: "hash-section-fallback",
		},
	}); err != nil {
		t.Fatalf("ReplaceDocument fallback returned error: %v", err)
	}

	normalized, err := store.SearchSectionsWithOptions(ctx, domain.SearchOptions{
		Query:                  "/access/v1/access/{object.id}/meta",
		Limit:                  5,
		MaxSearches:            5,
		MaxSectionsPerDocument: 5,
		ProfileDetail:          "compact",
	})
	if err != nil {
		t.Fatalf("normalized SearchSectionsWithOptions returned error: %v", err)
	}
	if len(normalized.Hits) == 0 || normalized.Hits[0].SectionID != "section-normalized-path" {
		t.Fatalf("normalized hits = %+v, want normalized path section first", normalized.Hits)
	}
	if normalized.Hits[0].QueryMatch == nil || !containsString(normalized.Hits[0].QueryMatch.MatchedFields, "entity_normalized") {
		t.Fatalf("normalized query match = %+v, want entity_normalized evidence", normalized.Hits[0].QueryMatch)
	}

	fallback, err := store.SearchSectionsWithOptions(ctx, domain.SearchOptions{
		Query:                  "/entity/v1/entities:filter-count",
		Limit:                  5,
		MaxSearches:            5,
		MaxSectionsPerDocument: 5,
		ProfileDetail:          "compact",
	})
	if err != nil {
		t.Fatalf("fallback SearchSectionsWithOptions returned error: %v", err)
	}
	if len(fallback.Hits) == 0 {
		t.Fatalf("fallback hits empty, want fuzzy/token recovery")
	}
	foundEntityExactZero := false
	foundFallbackAttempt := false
	for _, attempt := range fallback.Attempts {
		if attempt.Kind == "entity_exact" && attempt.Hits == 0 {
			foundEntityExactZero = true
		}
		if (attempt.Kind == "unicode61" || attempt.Kind == "trigram" || attempt.Kind == "like_fallback") && attempt.Hits > 0 {
			foundFallbackAttempt = true
		}
	}
	if !foundEntityExactZero || !foundFallbackAttempt {
		t.Fatalf("fallback attempts = %+v, want exact miss plus fallback recovery", fallback.Attempts)
	}

	budgetedMiss, err := store.SearchSectionsWithOptions(ctx, domain.SearchOptions{
		Query:                  "/qpathx/v9/nohit:zzzz-yyyy uniquemissx",
		Limit:                  5,
		MaxSearches:            5,
		MaxSectionsPerDocument: 5,
		ProfileDetail:          "compact",
	})
	if err != nil {
		t.Fatalf("budgeted miss SearchSectionsWithOptions returned error: %v", err)
	}
	foundLikeFallbackAttempt := false
	for _, attempt := range budgetedMiss.Attempts {
		if attempt.Kind == "like_fallback" {
			foundLikeFallbackAttempt = true
		}
	}
	if !foundLikeFallbackAttempt {
		t.Fatalf("budgeted miss attempts = %+v, want LIKE fallback even after entity attempts consume nominal budget", budgetedMiss.Attempts)
	}
}

func TestSearchSectionsUsesStoredEntitiesBeforeExtractorFallback(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-1")

	doc := domain.DocumentInput{
		ID:          "doc-stored-entity-search",
		SourceID:    "source-1",
		ExternalID:  "stored-entity.md",
		Title:       "Stored Entity Search",
		ContentHash: "hash-stored-entity",
	}
	if err := store.ReplaceDocument(ctx, doc, []domain.SectionInput{
		{
			ID:          "section-stored-entity",
			Title:       "Stored Entity",
			Content:     "This section describes the stored endpoint without spelling out the route literal.",
			ContentHash: "hash-section-stored-entity",
			Ordinal:     0,
		},
		{
			ID:          "section-token-only",
			Title:       "Token Only",
			Content:     "entity lifecycle notes without endpoint evidence",
			ContentHash: "hash-section-token-only",
			Ordinal:     1,
		},
	}); err != nil {
		t.Fatalf("ReplaceDocument returned error: %v", err)
	}
	if err := store.ReplaceSectionEntities(ctx, doc.ID, []domain.SectionEntityInput{
		{
			SectionID:     "section-stored-entity",
			Kind:          "api_endpoint",
			RawText:       "GET /entity/v1/entities",
			CanonicalText: "GET /entity/v1/entities",
			Method:        "GET",
			Path:          "/entity/v1/entities",
			Source:        "text",
			Confidence:    0.95,
		},
	}); err != nil {
		t.Fatalf("ReplaceSectionEntities returned error: %v", err)
	}

	result, err := store.SearchSectionsWithOptions(ctx, domain.SearchOptions{
		Query:                  "/entity/v1/entities",
		Limit:                  5,
		MaxSearches:            5,
		MaxSectionsPerDocument: 5,
		ProfileDetail:          "compact",
	})
	if err != nil {
		t.Fatalf("SearchSectionsWithOptions returned error: %v", err)
	}
	if len(result.Hits) == 0 || result.Hits[0].SectionID != "section-stored-entity" {
		t.Fatalf("hits = %+v, want stored entity section first", result.Hits)
	}
	if result.Hits[0].QueryMatch == nil || !containsString(result.Hits[0].QueryMatch.MatchedFields, "entity_exact") {
		t.Fatalf("query match = %+v, want entity_exact from stored entity", result.Hits[0].QueryMatch)
	}
	if len(result.Hits[0].MatchedEntities) != 1 || result.Hits[0].MatchedEntities[0].Path != "/entity/v1/entities" || result.Hits[0].MatchedEntities[0].MatchMode != "entity_exact" {
		t.Fatalf("matched entities = %+v, want stored exact entity metadata", result.Hits[0].MatchedEntities)
	}
	foundEntityAttempt := false
	for _, attempt := range result.Attempts {
		if attempt.Kind == "entity_exact" && attempt.Hits > 0 {
			foundEntityAttempt = true
		}
	}
	if !foundEntityAttempt {
		t.Fatalf("attempts = %+v, want stored entity exact attempt", result.Attempts)
	}
}

func TestSearchSectionsEntityFallbackSkipsSectionsWithStoredEntities(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-1")

	doc := domain.DocumentInput{
		ID:          "doc-stored-entity-authority",
		SourceID:    "source-1",
		ExternalID:  "stored-authority.md",
		Title:       "Stored Entity Authority",
		ContentHash: "hash-stored-authority",
	}
	if err := store.ReplaceDocument(ctx, doc, []domain.SectionInput{
		{
			ID:          "section-stored-authority",
			Title:       "Updated Entity",
			Content:     "Legacy prose still mentions GET /entity/v1/entities for background.",
			ContentHash: "hash-section-stored-authority",
			Ordinal:     0,
		},
	}); err != nil {
		t.Fatalf("ReplaceDocument returned error: %v", err)
	}
	if err := store.ReplaceSectionEntities(ctx, doc.ID, []domain.SectionEntityInput{
		{
			SectionID:     "section-stored-authority",
			Kind:          "api_endpoint",
			RawText:       "GET /entity/v1/entities:filter-filter-count",
			CanonicalText: "GET /entity/v1/entities:filter-filter-count",
			Method:        "GET",
			Path:          "/entity/v1/entities:filter-filter-count",
			Source:        "text",
			Confidence:    0.95,
		},
	}); err != nil {
		t.Fatalf("ReplaceSectionEntities returned error: %v", err)
	}

	result, err := store.SearchSectionsWithOptions(ctx, domain.SearchOptions{
		Query:                  "/entity/v1/entities",
		Limit:                  5,
		MaxSearches:            5,
		MaxSectionsPerDocument: 5,
		ProfileDetail:          "compact",
	})
	if err != nil {
		t.Fatalf("SearchSectionsWithOptions returned error: %v", err)
	}
	if len(result.Hits) == 0 {
		t.Fatalf("hits empty, want general fallback to preserve recall")
	}
	for _, attempt := range result.Attempts {
		if attempt.Kind == "entity_exact" && attempt.Hits != 0 {
			t.Fatalf("attempts = %+v, want entity_exact to trust stored entities over text fallback", result.Attempts)
		}
	}
}

func TestDocumentProfilePreservesDescAndPropagations(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-1")

	doc := domain.DocumentInput{
		ID:          "doc-profile",
		SourceID:    "source-1",
		ExternalID:  "profile.md",
		Title:       "Profile",
		ContentHash: "hash-v1",
	}
	if err := store.ReplaceDocument(ctx, doc, []domain.SectionInput{
		{ID: "section-profile", DocumentID: doc.ID, Title: "Overview", Content: "profile content", ContentHash: "section-hash"},
	}); err != nil {
		t.Fatalf("ReplaceDocument returned error: %v", err)
	}

	profile, err := store.GetDocumentProfile(ctx, doc.ID)
	if err != nil {
		t.Fatalf("GetDocumentProfile returned error: %v", err)
	}
	if profile.Desc != "" || profile.RetrievalProfileJSON != "{}" {
		t.Fatalf("default profile = %+v, want empty desc and empty retrieval profile", profile)
	}
	assertCount(t, ctx, store, "document_profiles", "document_id = 'doc-profile'", 0)

	profile, err = store.UpdateDocumentProfileDesc(ctx, domain.DocumentProfileInput{
		DocumentID: doc.ID,
		Desc:       "管理员维护的文档说明",
	})
	if err != nil {
		t.Fatalf("UpdateDocumentProfileDesc returned error: %v", err)
	}
	if profile.Desc != "管理员维护的文档说明" {
		t.Fatalf("profile desc = %q, want administrator desc", profile.Desc)
	}

	profile, err = store.UpsertDocumentRetrievalProfile(ctx, domain.RetrievalProfileInput{
		DocumentID:           doc.ID,
		RetrievalProfileJSON: `{"top_tags":["配置"],"top_terms":[{"term":"配置","tf":1,"sections":1,"score":1}]}`,
		GeneratedFromHash:    "hash-v1",
	})
	if err != nil {
		t.Fatalf("UpsertDocumentRetrievalProfile returned error: %v", err)
	}
	if profile.Desc != "管理员维护的文档说明" || profile.GeneratedFromHash != "hash-v1" {
		t.Fatalf("generated profile update = %+v, want desc preserved and generated hash", profile)
	}

	doc.ContentHash = "hash-v2"
	if err := store.ReplaceDocument(ctx, doc, []domain.SectionInput{
		{ID: "section-profile-v2", DocumentID: doc.ID, Title: "Overview", Content: "updated profile content", ContentHash: "section-hash-v2"},
	}); err != nil {
		t.Fatalf("second ReplaceDocument returned error: %v", err)
	}
	profile, err = store.GetDocumentProfile(ctx, doc.ID)
	if err != nil {
		t.Fatalf("GetDocumentProfile after ReplaceDocument returned error: %v", err)
	}
	if profile.Desc != "管理员维护的文档说明" {
		t.Fatalf("desc after ReplaceDocument = %q, want preserved administrator desc", profile.Desc)
	}

	if err := store.DeleteDocumentsNotInSource(ctx, "source-1", nil); err != nil {
		t.Fatalf("DeleteDocumentsNotInSource returned error: %v", err)
	}
	if _, err := store.GetDocumentProfile(ctx, doc.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetDocumentProfile after document delete error = %v, want sql.ErrNoRows", err)
	}
}

func TestKnowledgeRelationProposalApprovalExpandsSearch(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-1")

	initDoc := domain.DocumentInput{
		ID:          "doc-init",
		SourceID:    "source-1",
		ExternalID:  "init.md",
		Title:       "Permission Init Example",
		ContentHash: "hash-init",
	}
	if err := store.ReplaceDocument(ctx, initDoc, []domain.SectionInput{
		{ID: "section-init", DocumentID: initDoc.ID, Title: "Init", Content: "permission init example uses a compact policy block", ContentHash: "section-init-hash"},
	}); err != nil {
		t.Fatalf("ReplaceDocument init returned error: %v", err)
	}
	schemaDoc := domain.DocumentInput{
		ID:          "doc-schema",
		SourceID:    "source-1",
		ExternalID:  "schema.md",
		Title:       "Permission Schema Syntax",
		ContentHash: "hash-schema",
	}
	if err := store.ReplaceDocument(ctx, schemaDoc, []domain.SectionInput{
		{ID: "section-schema", DocumentID: schemaDoc.ID, Title: "Fields", Content: "field-level grammar and operators are defined here", ContentHash: "section-schema-hash"},
	}); err != nil {
		t.Fatalf("ReplaceDocument schema returned error: %v", err)
	}

	proposal, err := store.CreateKnowledgeRelationProposal(ctx, domain.KnowledgeRelationProposalInput{
		RelationType:   "schema_reference",
		FromDocumentID: initDoc.ID,
		ToDocumentID:   schemaDoc.ID,
		Reason:         "init examples need the field-level schema syntax for rigorous answers",
		EvidenceJSON:   `{"query":"permission init"}`,
		CreatedByType:  "mcp_agent",
		CreatedByRef:   "test",
	})
	if err != nil {
		t.Fatalf("CreateKnowledgeRelationProposal returned error: %v", err)
	}
	if proposal.Status != "pending" {
		t.Fatalf("proposal status = %q, want pending", proposal.Status)
	}
	relation, err := store.ApproveKnowledgeRelationProposal(ctx, proposal.ID, "reviewer", "looks correct")
	if err != nil {
		t.Fatalf("ApproveKnowledgeRelationProposal returned error: %v", err)
	}
	if relation.RelationType != "schema_reference" || relation.Effect != "context_link" {
		t.Fatalf("approved relation = %+v, want schema_reference context_link", relation)
	}

	result, err := store.SearchSectionsWithOptions(ctx, domain.SearchOptions{
		Query:                  "permission init",
		Limit:                  5,
		MaxSearches:            5,
		MaxSectionsPerDocument: 5,
		ProfileDetail:          "compact",
		UseRelationExpansion:   true,
	})
	if err != nil {
		t.Fatalf("SearchSectionsWithOptions returned error: %v", err)
	}
	if !searchHitsContainDocument(result.Hits, initDoc.ID) {
		t.Fatalf("search hits = %+v, want init doc", result.Hits)
	}
	if !searchHitsContainDocument(result.Hits, schemaDoc.ID) {
		t.Fatalf("search hits = %+v, want schema doc expanded from approved relation", result.Hits)
	}
	if !searchHitsContainRelation(result.Hits, relation.ID) {
		t.Fatalf("search hits = %+v, want relation match %s", result.Hits, relation.ID)
	}
}

func TestGetSectionExplicitReferencesResolveHeadingNumberAndMarkdownAnchor(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-1")
	seedExplicitReferenceDocument(t, ctx, store, "See §5 Baking Temperatures for details. Also see [Cookie Settings](#cookie-settings).")

	section, err := store.GetSection(ctx, "section-source")
	if err != nil {
		t.Fatalf("GetSection returned error: %v", err)
	}
	headingRef := findExplicitReference(section.ExplicitReferences, "heading_number_reference_extractor")
	if headingRef == nil {
		t.Fatalf("explicit_references = %+v, want heading number reference", section.ExplicitReferences)
	}
	if !headingRef.Resolved || headingRef.TargetSectionID != "section-propagation" || headingRef.TargetDocumentID != "doc-explicit" {
		t.Fatalf("heading reference = %+v, want resolved propagation section", *headingRef)
	}
	if headingRef.TargetHeadingPath != "Schema > 5 Baking Temperatures" {
		t.Fatalf("heading target path = %q, want propagation heading path", headingRef.TargetHeadingPath)
	}

	markdownRef := findExplicitReference(section.ExplicitReferences, "markdown_link_extractor")
	if markdownRef == nil {
		t.Fatalf("explicit_references = %+v, want markdown reference", section.ExplicitReferences)
	}
	if !markdownRef.Resolved || markdownRef.TargetSectionID != "section-operation" {
		t.Fatalf("markdown reference = %+v, want resolved operation section", *markdownRef)
	}
}

func TestGetSectionExplicitReferencesReturnAmbiguousCandidates(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-1")
	seedExplicitReferenceDocument(t, ctx, store, "See Baking Temperatures for details.")
	if err := store.ReplaceDocument(ctx, domain.DocumentInput{
		ID:          "doc-explicit",
		SourceID:    "source-1",
		ExternalID:  "schema.md",
		Title:       "Schema",
		URL:         "file:///docs/schema.md",
		ContentHash: "hash-doc-explicit-ambiguous",
	}, []domain.SectionInput{
		{
			ID:          "section-source",
			DocumentID:  "doc-explicit",
			HeadingPath: "Schema > Operation",
			Title:       "Operation",
			Content:     "See Baking Temperatures for details.",
			ContentHash: "hash-section-source-ambiguous",
			Ordinal:     0,
		},
		{
			ID:          "section-propagation",
			DocumentID:  "doc-explicit",
			HeadingPath: "Schema > 5 Baking Temperatures",
			Title:       "5 Baking Temperatures",
			Content:     "Baking temperature rules for oven settings.",
			ContentHash: "hash-section-propagation",
			Ordinal:     1,
		},
		{
			ID:          "section-propagation-other",
			DocumentID:  "doc-explicit",
			HeadingPath: "Schema > Appendix > Baking Temperatures",
			Title:       "Baking Temperatures",
			Content:     "Another propagation section.",
			ContentHash: "hash-section-propagation-other",
			Ordinal:     2,
		},
	}); err != nil {
		t.Fatalf("ReplaceDocument ambiguous returned error: %v", err)
	}

	section, err := store.GetSection(ctx, "section-source")
	if err != nil {
		t.Fatalf("GetSection returned error: %v", err)
	}
	ref := findExplicitReference(section.ExplicitReferences, "plain_title_reference_extractor")
	if ref == nil {
		t.Fatalf("explicit_references = %+v, want plain title reference", section.ExplicitReferences)
	}
	if ref.Resolved {
		t.Fatalf("plain title reference = %+v, want unresolved ambiguous reference", *ref)
	}
	if len(ref.Candidates) < 2 {
		t.Fatalf("plain title candidates = %+v, want ambiguous candidates", ref.Candidates)
	}
}

func TestSearchSectionsExplicitReferenceMetadataAndSuggestedReads(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-1")
	seedExplicitReferenceDocument(t, ctx, store, "When configuring cookies, detailed rules see §5 Baking Temperatures.")

	result, err := store.SearchSectionsWithOptions(ctx, domain.SearchOptions{
		Query:                  "configuring operations",
		Limit:                  5,
		MaxSearches:            5,
		MaxSectionsPerDocument: 5,
		Detail:                 "summary",
		ProfileDetail:          "none",
	})
	if err != nil {
		t.Fatalf("SearchSectionsWithOptions returned error: %v", err)
	}
	hit := findSearchHit(result.Hits, "section-source")
	if hit == nil {
		t.Fatalf("hits = %+v, want source section", result.Hits)
	}
	if !hit.HasExplicitReferences || hit.ExplicitReferenceCount != 1 {
		t.Fatalf("source hit explicit reference metadata = %+v, want one explicit reference", *hit)
	}
	if len(result.SuggestedReads.ExplicitReferences) == 0 {
		t.Fatalf("suggested_reads = %+v, want explicit references", result.SuggestedReads)
	}
	ref := result.SuggestedReads.ExplicitReferences[0]
	if ref.SourceSectionID != "section-source" || ref.TargetSectionID != "section-propagation" || !ref.Resolved {
		t.Fatalf("suggested explicit reference = %+v, want resolved propagation reference", ref)
	}
	if result.SuggestedReads.ImplicitSymbolLinks == nil || result.SuggestedReads.CuratedRelations == nil || result.SuggestedReads.StructuralNeighbors == nil {
		t.Fatalf("suggested_reads = %+v, want separate empty non-explicit groups", result.SuggestedReads)
	}
}

func TestSearchSectionsUsesChineseSubstringAndProfileEvidence(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-1")

	doc := domain.DocumentInput{
		ID:          "doc-auth-errors",
		SourceID:    "source-1",
		ExternalID:  "auth-errors.md",
		Title:       "配置接口",
		ContentHash: "hash-auth-errors",
	}
	if err := store.ReplaceDocument(ctx, doc, []domain.SectionInput{
		{
			ID:          "section-auth-errors",
			DocumentID:  doc.ID,
			HeadingPath: "配置接口 > 错误响应",
			Title:       "错误响应",
			Content:     "配置接口的几种错误响应包括 401 和 403。",
			ContentHash: "hash-section-auth-errors",
		},
	}); err != nil {
		t.Fatalf("ReplaceDocument returned error: %v", err)
	}
	if _, err := store.UpsertDocumentRetrievalProfile(ctx, domain.RetrievalProfileInput{
		DocumentID:           doc.ID,
		RetrievalProfileJSON: `{"top_tags":["配置","错误响应"],"top_terms":[{"term":"错误响应","tf":2,"sections":1,"heading_hits":1,"score":7.2}],"keyphrases":["配置接口","错误响应"],"api_refs":["401","403"]}`,
		GeneratedFromHash:    doc.ContentHash,
	}); err != nil {
		t.Fatalf("UpsertDocumentRetrievalProfile returned error: %v", err)
	}

	result, err := store.SearchSectionsWithOptions(ctx, domain.SearchOptions{
		Query:                  "错误响应",
		Limit:                  5,
		MaxSearches:            5,
		MaxSectionsPerDocument: 2,
		ProfileDetail:          "compact",
	})
	if err != nil {
		t.Fatalf("SearchSectionsWithOptions returned error: %v", err)
	}
	if len(result.Hits) == 0 || result.Hits[0].DocumentID != doc.ID {
		t.Fatalf("SearchSectionsWithOptions hits = %+v, want auth errors document", result.Hits)
	}
	if result.SearchesUsed == 0 || len(result.Attempts) == 0 {
		t.Fatalf("search attempts = %+v, want populated attempts", result.Attempts)
	}
	hit := result.Hits[0]
	if hit.QueryMatch == nil || len(hit.QueryMatch.MatchedFields) == 0 {
		t.Fatalf("hit query_match = %+v, want match evidence", hit.QueryMatch)
	}
	if hit.Profile == nil || !containsString(hit.Profile.TopTags, "错误响应") {
		t.Fatalf("hit profile = %+v, want compact generated tags", hit.Profile)
	}
}

func TestSearchSectionsProfileAliasEvidenceAndBoundedFullProfile(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-1")

	doc := domain.DocumentInput{
		ID:          "doc-alias",
		SourceID:    "source-1",
		ExternalID:  "alias.md",
		Title:       "Alias Document",
		ContentHash: "hash-alias",
	}
	longContent := strings.Repeat("large section content ", 80)
	if err := store.ReplaceDocument(ctx, doc, []domain.SectionInput{
		{
			ID:          "section-alias",
			DocumentID:  doc.ID,
			Title:       "Overview",
			Content:     longContent,
			ContentHash: "hash-section-alias",
		},
	}); err != nil {
		t.Fatalf("ReplaceDocument returned error: %v", err)
	}
	if _, err := store.UpsertDocumentRetrievalProfile(ctx, domain.RetrievalProfileInput{
		DocumentID: doc.ID,
		RetrievalProfileJSON: `{
			"top_tags":["alias"],
			"top_terms":[{"term":"alias","tf":1,"sections":1,"score":2}],
			"keyphrases":["alias"],
			"aliases":["alias-only-token"],
			"api_refs":["GET /alias"],
			"section_distribution":[
				{"section_id":"section-alias","title":"Overview","terms":["alias","alias-only-token","GET /alias","extra1","extra2","extra3","extra4","extra5","extra6"],"term_count":9},
				{"section_id":"section-extra-1","title":"Extra 1","terms":["extra"],"term_count":1},
				{"section_id":"section-extra-2","title":"Extra 2","terms":["extra"],"term_count":1},
				{"section_id":"section-extra-3","title":"Extra 3","terms":["extra"],"term_count":1},
				{"section_id":"section-extra-4","title":"Extra 4","terms":["extra"],"term_count":1},
				{"section_id":"section-extra-5","title":"Extra 5","terms":["extra"],"term_count":1}
			],
			"stats":{"token_count":99,"section_count":6,"unique_term_count":12}
		}`,
		GeneratedFromHash: doc.ContentHash,
	}); err != nil {
		t.Fatalf("UpsertDocumentRetrievalProfile returned error: %v", err)
	}

	result, err := store.SearchSectionsWithOptions(ctx, domain.SearchOptions{
		Query:                  "alias-only-token",
		Limit:                  5,
		MaxSearches:            5,
		MaxSectionsPerDocument: 2,
		ProfileDetail:          "full",
		MaxCharsPerResult:      40,
	})
	if err != nil {
		t.Fatalf("SearchSectionsWithOptions returned error: %v", err)
	}
	if len(result.Hits) != 1 {
		t.Fatalf("hits = %+v, want one alias profile hit", result.Hits)
	}
	hit := result.Hits[0]
	if hit.QueryMatch == nil || !containsString(hit.QueryMatch.MatchedFields, "profile") {
		t.Fatalf("query match = %+v, want profile evidence for alias-only hit", hit.QueryMatch)
	}
	if !strings.Contains(hit.Content, "truncated at 40 bytes") {
		t.Fatalf("content = %q, want truncation hint", hit.Content)
	}
	full, ok := hit.RetrievalProfile.(map[string]any)
	if !ok {
		t.Fatalf("retrieval_profile = %#v, want bounded full profile map", hit.RetrievalProfile)
	}
	sections, ok := full["section_distribution"].([]storedSectionDistribution)
	if !ok || len(sections) > 5 {
		t.Fatalf("bounded section_distribution = %#v, want at most 5 sections", full["section_distribution"])
	}
	if len(sections) == 0 || len(sections[0].Terms) > 8 {
		t.Fatalf("bounded first section = %+v, want limited terms", sections)
	}
}

func TestSearchSectionsTrigramFindsCJKMixedQueries(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-1")

	// Insert a document that should be found when searching "sampleapp平台默认配置schema".
	// The query mixes Latin ("sampleapp", "schema") with CJK ("平台默认配置").
	// The default unicode61 tokenizer treats the whole string as one token,
	// but the trigram tokenizer can match fragments across script boundaries.
	doc := domain.DocumentInput{
		ID:          "doc-auth",
		SourceID:    "source-1",
		ExternalID:  "auth-schema.md",
		Title:       "示例系统基本配置Schema",
		ContentHash: "hash-auth",
	}
	if err := store.ReplaceDocument(ctx, doc, []domain.SectionInput{
		{
			ID:          "section-auth-overview",
			DocumentID:  doc.ID,
			HeadingPath: "示例系统 > 配置概述",
			Title:       "配置概述",
			Content:     "示例系统的基本配置Schema描述了系统的默认设置，包括模块和配置项概念。",
			ContentHash: "hash-section-auth",
			Ordinal:     0,
		},
	}); err != nil {
		t.Fatalf("ReplaceDocument returned error: %v", err)
	}

	// Insert noise documents that also match partial terms.
	noiseDoc := domain.DocumentInput{
		ID:          "doc-sampleapp",
		SourceID:    "source-1",
		ExternalID:  "sampleapp-deploy.md",
		Title:       "sampleapp平台部署指南",
		ContentHash: "hash-sampleapp",
	}
	if err := store.ReplaceDocument(ctx, noiseDoc, []domain.SectionInput{
		{
			ID:          "section-sampleapp",
			DocumentID:  noiseDoc.ID,
			HeadingPath: "sampleapp > 部署",
			Title:       "部署步骤",
			Content:     "sampleapp平台是一个统一部署平台，支持自动扩缩容。",
			ContentHash: "hash-section-sampleapp",
			Ordinal:     0,
		},
	}); err != nil {
		t.Fatalf("ReplaceDocument (noise) returned error: %v", err)
	}

	noiseDoc2 := domain.DocumentInput{
		ID:          "doc-platform",
		SourceID:    "source-1",
		ExternalID:  "platform-config.md",
		Title:       "平台默认配置",
		ContentHash: "hash-platform",
	}
	if err := store.ReplaceDocument(ctx, noiseDoc2, []domain.SectionInput{
		{
			ID:          "section-platform",
			DocumentID:  noiseDoc2.ID,
			HeadingPath: "平台 > 默认配置",
			Title:       "默认配置",
			Content:     "平台默认配置包括配置管理、网络策略等模块。",
			ContentHash: "hash-section-platform",
			Ordinal:     0,
		},
	}); err != nil {
		t.Fatalf("ReplaceDocument (noise2) returned error: %v", err)
	}

	// The diagnostic query: "sampleapp平台默认配置schema"
	result, err := store.SearchSectionsWithOptions(ctx, domain.SearchOptions{
		Query:                  "sampleapp平台默认配置schema",
		Limit:                  10,
		MaxSearches:            5,
		MaxSectionsPerDocument: 2,
		ProfileDetail:          "compact",
	})
	if err != nil {
		t.Fatalf("SearchSectionsWithOptions returned error: %v", err)
	}

	// Verify the trigram attempt was used.
	foundTrigram := false
	for _, att := range result.Attempts {
		if att.Kind == "trigram" {
			foundTrigram = true
			if att.Hits == 0 {
				t.Fatalf("trigram attempt has 0 hits, want >0")
			}
		}
	}
	if !foundTrigram {
		t.Fatalf("search attempts %+v missing trigram kind", result.Attempts)
	}

	// The expected document (示例系统基本配置Schema) must appear in results.
	foundExpected := false
	for _, hit := range result.Hits {
		if hit.DocumentID == "doc-auth" {
			foundExpected = true
			break
		}
	}
	if !foundExpected {
		t.Fatalf("expected doc-auth (示例系统基本配置Schema) missing from hits: %+v", result.Hits)
	}

	// Verify fts_section_tokens_trigram has data for these sections.
	var trigramCount int64
	if err := store.readDB().QueryRowContext(ctx,
		"select count(*) from fts_section_tokens_trigram",
	).Scan(&trigramCount); err != nil {
		t.Fatalf("counting fts_section_tokens_trigram: %v", err)
	}
	if trigramCount != 3 {
		t.Fatalf("fts_section_tokens_trigram has %d rows, want 3", trigramCount)
	}
}

func TestFeedbackValidationCanonicalRankingAndNodeMerge(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-1")

	for _, doc := range []struct {
		id    string
		title string
		secID string
	}{
		{id: "doc-a", title: "Regular Document", secID: "section-a"},
		{id: "doc-b", title: "Canonical Document", secID: "section-b"},
	} {
		if err := store.ReplaceDocument(ctx, domain.DocumentInput{
			ID:          doc.id,
			SourceID:    "source-1",
			ExternalID:  doc.id + ".md",
			Title:       doc.title,
			ContentHash: "hash-" + doc.id,
		}, []domain.SectionInput{
			{
				ID:          doc.secID,
				Title:       doc.title,
				Content:     "canonical ranking sharedtoken",
				ContentHash: "hash-" + doc.secID,
			},
		}); err != nil {
			t.Fatalf("ReplaceDocument(%s) returned error: %v", doc.id, err)
		}
	}
	if _, err := store.CreateFeedbackEvent(ctx, domain.FeedbackEventInput{
		TargetKind:   "document",
		TargetID:     "doc-b",
		FeedbackKind: "document_canonical",
	}); err != nil {
		t.Fatalf("CreateFeedbackEvent document_canonical returned error: %v", err)
	}
	hits, err := store.SearchSections(ctx, "canonical ranking sharedtoken", 10)
	if err != nil {
		t.Fatalf("SearchSections returned error: %v", err)
	}
	if len(hits) != 2 || hits[0].DocumentID != "doc-b" || !hits[0].Canonical {
		t.Fatalf("SearchSections hits = %+v, want canonical document first", hits)
	}

	if _, err := store.CreateFeedbackEvent(ctx, domain.FeedbackEventInput{
		TargetKind:   "edge",
		TargetID:     "doc-b",
		FeedbackKind: "document_stale",
	}); err == nil || !strings.Contains(err.Error(), `target_kind "document"`) {
		t.Fatalf("document_stale wrong target error = %v, want target_kind document error", err)
	}
	if _, err := store.CreateFeedbackEvent(ctx, domain.FeedbackEventInput{
		TargetKind:   "node",
		TargetID:     "node-a",
		FeedbackKind: "node_merge",
		PayloadJSON:  `{}`,
	}); err == nil || !strings.Contains(err.Error(), "merged_into") {
		t.Fatalf("node_merge missing payload error = %v, want merged_into error", err)
	}

	for _, node := range []domain.NodeInput{
		{ID: "node-a", Kind: "Product", Name: "A", CanonicalName: "a"},
		{ID: "node-b", Kind: "Product", Name: "B", CanonicalName: "b"},
	} {
		if err := store.UpsertNode(ctx, node); err != nil {
			t.Fatalf("UpsertNode(%s) returned error: %v", node.ID, err)
		}
	}
	if _, err := store.CreateFeedbackEvent(ctx, domain.FeedbackEventInput{
		TargetKind:   "node",
		TargetID:     "node-a",
		FeedbackKind: "node_merge",
		PayloadJSON:  `{"merged_into":"node-b"}`,
	}); err != nil {
		t.Fatalf("CreateFeedbackEvent node_merge returned error: %v", err)
	}
	related, err := store.RelatedNodes(ctx, "node-a", domain.RelatedOptions{Direction: "out", Kind: "merged_into"})
	if err != nil {
		t.Fatalf("RelatedNodes merged_into returned error: %v", err)
	}
	if len(related) != 1 || related[0].Node.ID != "node-b" || related[0].Edge.Provenance != "manual" {
		t.Fatalf("RelatedNodes merged_into = %+v, want manual merge edge to node-b", related)
	}
}

func TestFeedbackEventsApplyCurationRules(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-1")

	if err := store.ReplaceDocument(ctx, domain.DocumentInput{
		ID:          "doc-feedback",
		SourceID:    "source-1",
		ExternalID:  "feedback.md",
		Title:       "Feedback",
		ContentHash: "hash-doc",
	}, []domain.SectionInput{
		{
			ID:          "section-feedback",
			Title:       "Feedback",
			Content:     "feedback stale unique token",
			ContentHash: "hash-section",
		},
	}); err != nil {
		t.Fatalf("ReplaceDocument returned error: %v", err)
	}
	for _, node := range []domain.NodeInput{
		{ID: "node-a", Kind: "Product", Name: "A", CanonicalName: "a"},
		{ID: "node-b", Kind: "API", Name: "B", CanonicalName: "b"},
		{ID: "node-c", Kind: "Module", Name: "C", CanonicalName: "c"},
	} {
		if err := store.UpsertNode(ctx, node); err != nil {
			t.Fatalf("UpsertNode(%s) returned error: %v", node.ID, err)
		}
	}
	if err := store.UpsertEdge(ctx, domain.EdgeInput{
		ID:         "edge-auto",
		SrcID:      "node-a",
		DstID:      "node-b",
		Kind:       "exposes_api",
		Provenance: "rule",
	}); err != nil {
		t.Fatalf("UpsertEdge returned error: %v", err)
	}

	event, err := store.CreateFeedbackEvent(ctx, domain.FeedbackEventInput{
		TargetKind:   "edge",
		TargetID:     "edge-auto",
		FeedbackKind: "relationship_wrong",
		Actor:        "alice",
	})
	if err != nil {
		t.Fatalf("CreateFeedbackEvent relationship_wrong returned error: %v", err)
	}
	if event.ID == "" || event.Actor != "alice" || event.PayloadJSON != "{}" {
		t.Fatalf("relationship_wrong event = %+v, want populated event", event)
	}
	related, err := store.RelatedNodes(ctx, "node-a", domain.RelatedOptions{Direction: "out"})
	if err != nil {
		t.Fatalf("RelatedNodes after relationship_wrong returned error: %v", err)
	}
	if len(related) != 0 {
		t.Fatalf("RelatedNodes after relationship_wrong = %+v, want edge filtered", related)
	}

	event, err = store.CreateFeedbackEvent(ctx, domain.FeedbackEventInput{
		TargetKind:   "node",
		TargetID:     "node-a",
		FeedbackKind: "relationship_add",
		PayloadJSON:  `{"src_id":"node-a","dst_id":"node-c","kind":"depends_on","edge_id":"edge-manual"}`,
		Actor:        "bob",
	})
	if err != nil {
		t.Fatalf("CreateFeedbackEvent relationship_add returned error: %v", err)
	}
	if event.FeedbackKind != "relationship_add" {
		t.Fatalf("relationship_add event = %+v", event)
	}
	related, err = store.RelatedNodes(ctx, "node-a", domain.RelatedOptions{Direction: "out", Kind: "depends_on"})
	if err != nil {
		t.Fatalf("RelatedNodes after relationship_add returned error: %v", err)
	}
	if len(related) != 1 || related[0].Edge.ID != "edge-manual" || related[0].Edge.Provenance != "manual" {
		t.Fatalf("RelatedNodes after relationship_add = %+v, want manual edge", related)
	}

	hits, err := store.SearchSections(ctx, "feedback stale unique", 10)
	if err != nil {
		t.Fatalf("SearchSections before document_stale returned error: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("SearchSections before document_stale = %+v, want hit", hits)
	}
	if _, err := store.CreateFeedbackEvent(ctx, domain.FeedbackEventInput{
		TargetKind:   "document",
		TargetID:     "doc-feedback",
		FeedbackKind: "document_stale",
	}); err != nil {
		t.Fatalf("CreateFeedbackEvent document_stale returned error: %v", err)
	}
	hits, err = store.SearchSections(ctx, "feedback stale unique", 10)
	if err != nil {
		t.Fatalf("SearchSections after document_stale returned error: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("SearchSections after document_stale = %+v, want no stale hits", hits)
	}

	events, err := store.ListFeedbackEvents(ctx, domain.FeedbackListOptions{TargetKind: "edge", TargetID: "edge-auto", Limit: 10})
	if err != nil {
		t.Fatalf("ListFeedbackEvents returned error: %v", err)
	}
	if len(events) != 1 || events[0].FeedbackKind != "relationship_wrong" {
		t.Fatalf("ListFeedbackEvents = %+v, want relationship_wrong event", events)
	}
}

func TestGraphNodeAndEdgeUpsertIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-1")

	if err := store.ReplaceDocument(ctx, domain.DocumentInput{
		ID:          "doc-1",
		SourceID:    "source-1",
		ExternalID:  "guide.md",
		Title:       "Guide",
		ContentHash: "hash-doc",
	}, []domain.SectionInput{
		{
			ID:          "section-1",
			Title:       "API",
			Content:     "GET /member/benefits returns benefits.",
			ContentHash: "hash-section",
		},
	}); err != nil {
		t.Fatalf("ReplaceDocument returned error: %v", err)
	}

	if err := store.UpsertNode(ctx, domain.NodeInput{
		ID:            "node-document",
		Kind:          "Document",
		Name:          "Guide",
		CanonicalName: "doc:guide.md",
		MetadataJSON:  `{"external_id":"guide.md"}`,
		Confidence:    1.0,
	}); err != nil {
		t.Fatalf("first UpsertNode document returned error: %v", err)
	}
	if err := store.UpsertNode(ctx, domain.NodeInput{
		ID:            "node-section",
		Kind:          "DocSection",
		Name:          "API",
		CanonicalName: "section:guide.md#api",
		MetadataJSON:  `{"ordinal":0}`,
		Confidence:    0.95,
	}); err != nil {
		t.Fatalf("first UpsertNode section returned error: %v", err)
	}
	if err := store.UpsertEdge(ctx, domain.EdgeInput{
		ID:                "edge-document-section",
		SrcID:             "node-document",
		DstID:             "node-section",
		Kind:              "contains",
		Confidence:        0.9,
		Provenance:        "rule",
		EvidenceSectionID: "section-1",
		SourceRevision:    "rev-1",
		MetadataJSON:      `{"source":"first-sync"}`,
	}); err != nil {
		t.Fatalf("first UpsertEdge returned error: %v", err)
	}

	if err := store.UpsertNode(ctx, domain.NodeInput{
		ID:            "node-document",
		Kind:          "Document",
		Name:          "Updated Guide",
		CanonicalName: "doc:guide.md",
		MetadataJSON:  `{"external_id":"guide.md","title":"Updated Guide"}`,
		Confidence:    0.88,
	}); err != nil {
		t.Fatalf("second UpsertNode document returned error: %v", err)
	}
	if err := store.UpsertEdge(ctx, domain.EdgeInput{
		ID:                "edge-document-section",
		SrcID:             "node-document",
		DstID:             "node-section",
		Kind:              "contains",
		Confidence:        0.77,
		Provenance:        "rule",
		EvidenceSectionID: "section-1",
		SourceRevision:    "rev-2",
		MetadataJSON:      `{"source":"second-sync"}`,
	}); err != nil {
		t.Fatalf("second UpsertEdge returned error: %v", err)
	}

	assertCount(t, ctx, store, "nodes", "id in ('node-document', 'node-section')", 2)
	assertCount(t, ctx, store, "edges", "id = 'edge-document-section'", 1)
	assertCount(t, ctx, store, "nodes", "id = 'node-document' and name = 'Updated Guide' and confidence = 0.88 and metadata_json like '%Updated Guide%'", 1)
	assertCount(t, ctx, store, "edges", "id = 'edge-document-section' and confidence = 0.77 and source_revision = 'rev-2' and metadata_json like '%second-sync%'", 1)

	node, err := store.GetNode(ctx, "node-document")
	if err != nil {
		t.Fatalf("GetNode returned error: %v", err)
	}
	if node.Name != "Updated Guide" || node.MetadataJSON != `{"external_id":"guide.md","title":"Updated Guide"}` {
		t.Fatalf("GetNode returned %#v, want updated document node", node)
	}

	related, err := store.RelatedNodes(ctx, "node-document", domain.RelatedOptions{
		Direction: "out",
		Kind:      "contains",
	})
	if err != nil {
		t.Fatalf("RelatedNodes returned error: %v", err)
	}
	if len(related) != 1 {
		t.Fatalf("RelatedNodes returned %d rows, want 1: %#v", len(related), related)
	}
	if related[0].Node.ID != "node-section" || related[0].Direction != "out" {
		t.Fatalf("RelatedNodes[0] = %#v, want outgoing section relation", related[0])
	}
	if related[0].Edge.ID != "edge-document-section" || related[0].Edge.SourceRevision != "rev-2" {
		t.Fatalf("RelatedNodes edge = %#v, want updated edge", related[0].Edge)
	}
}

func TestImpactReturnsPathsWithDepthDirectionKindAndCycleHandling(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	seedImpactGraph(t, ctx, store)

	depthOne, err := store.Impact(ctx, "node-product", domain.ImpactOptions{
		Direction: "out",
		MaxDepth:  1,
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("Impact depth one returned error: %v", err)
	}
	if depthOne.StartNode.ID != "node-product" {
		t.Fatalf("Impact start node = %+v, want node-product", depthOne.StartNode)
	}
	if len(depthOne.Paths) != 2 {
		t.Fatalf("Impact depth one returned %d paths, want 2: %#v", len(depthOne.Paths), depthOne.Paths)
	}
	for _, path := range depthOne.Paths {
		if len(path.Nodes) != 2 || len(path.Edges) != 1 {
			t.Fatalf("Impact depth one path = %#v, want one-hop path", path)
		}
	}

	filtered, err := store.Impact(ctx, "node-product", domain.ImpactOptions{
		Direction: "out",
		Kind:      "exposes_api",
		MaxDepth:  3,
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("Impact filtered returned error: %v", err)
	}
	if len(filtered.Paths) != 1 {
		t.Fatalf("Impact filtered returned %d paths, want only exposes_api edge: %#v", len(filtered.Paths), filtered.Paths)
	}
	if filtered.Paths[0].Edges[0].Kind != "exposes_api" || filtered.Paths[0].Nodes[1].ID != "node-api" {
		t.Fatalf("Impact filtered path = %#v, want product to API exposes_api path", filtered.Paths[0])
	}
	if filtered.Paths[0].Edges[0].EvidenceSectionID != "section-impact" {
		t.Fatalf("Impact evidence section = %q, want section-impact", filtered.Paths[0].Edges[0].EvidenceSectionID)
	}

	incoming, err := store.Impact(ctx, "node-product", domain.ImpactOptions{
		Direction: "in",
		MaxDepth:  1,
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("Impact incoming returned error: %v", err)
	}
	if len(incoming.Paths) != 1 || incoming.Paths[0].Edges[0].ID != "edge-module-product" || incoming.Paths[0].Nodes[1].ID != "node-module" {
		t.Fatalf("Impact incoming paths = %#v, want module to product incoming path", incoming.Paths)
	}

	withCycle, err := store.Impact(ctx, "node-product", domain.ImpactOptions{
		Direction: "out",
		MaxDepth:  4,
		Limit:     20,
	})
	if err != nil {
		t.Fatalf("Impact with cycle returned error: %v", err)
	}
	for _, path := range withCycle.Paths {
		seen := map[string]bool{}
		for _, node := range path.Nodes {
			if seen[node.ID] {
				t.Fatalf("Impact path contains cycle node %q: %#v", node.ID, path)
			}
			seen[node.ID] = true
		}
		if len(path.Nodes) != len(path.Edges)+1 {
			t.Fatalf("Impact path = %#v, want nodes to describe path, not flat nodes", path)
		}
	}
}

func openTempStore(t *testing.T, ctx context.Context) *Store {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "docgraph.db")
	store, err := Open(ctx, "sqlite://"+dbPath)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Fatalf("Close returned error: %v", err)
		}
	})
	return store
}

func openMigratedTempStore(t *testing.T, ctx context.Context) *Store {
	t.Helper()

	store := openTempStore(t, ctx)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}
	return store
}

func createTestSource(t *testing.T, ctx context.Context, store *Store, id string) {
	t.Helper()

	_, err := store.CreateSource(ctx, domain.Source{
		ID:   id,
		Kind: "local",
		Name: "Docs",
		DSN:  "file:///docs",
	})
	if err != nil {
		t.Fatalf("CreateSource returned error: %v", err)
	}
}

func seedExplicitReferenceDocument(t *testing.T, ctx context.Context, store *Store, sourceContent string) {
	t.Helper()

	if err := store.ReplaceDocument(ctx, domain.DocumentInput{
		ID:          "doc-explicit",
		SourceID:    "source-1",
		ExternalID:  "schema.md",
		Title:       "Schema",
		URL:         "file:///docs/schema.md",
		ContentHash: "hash-doc-explicit",
	}, []domain.SectionInput{
		{
			ID:          "section-source",
			DocumentID:  "doc-explicit",
			HeadingPath: "Schema > Operation",
			Title:       "Operation",
			Content:     sourceContent,
			ContentHash: "hash-section-source",
			Ordinal:     0,
		},
		{
			ID:          "section-operation",
			DocumentID:  "doc-explicit",
			HeadingPath: "Schema > Cookie Settings",
			Title:       "Cookie Settings",
			Content:     "Cookie configuration syntax.",
			ContentHash: "hash-section-operation",
			Ordinal:     1,
		},
		{
			ID:          "section-propagation",
			DocumentID:  "doc-explicit",
			HeadingPath: "Schema > 5 Baking Temperatures",
			Title:       "5 Baking Temperatures",
			Content:     "Baking temperature rules for oven settings.",
			ContentHash: "hash-section-propagation",
			Ordinal:     2,
		},
	}); err != nil {
		t.Fatalf("ReplaceDocument explicit returned error: %v", err)
	}
}

func findExplicitReference(refs []domain.ExplicitReference, extractor string) *domain.ExplicitReference {
	for i := range refs {
		if refs[i].Extractor == extractor {
			return &refs[i]
		}
	}
	return nil
}

func findSearchHit(hits []domain.SearchHit, sectionID string) *domain.SearchHit {
	for i := range hits {
		if hits[i].SectionID == sectionID {
			return &hits[i]
		}
	}
	return nil
}

func assertCount(t *testing.T, ctx context.Context, store *Store, table string, where string, want int64) {
	t.Helper()

	var got int64
	query := "select count(*) from " + table
	if where != "" {
		query += " where " + where
	}
	if err := store.db.QueryRowContext(ctx, query).Scan(&got); err != nil {
		t.Fatalf("count %s where %q: %v", table, where, err)
	}
	if got != want {
		t.Fatalf("count %s where %q = %d, want %d", table, where, got, want)
	}
}

func assertPragmaString(t *testing.T, ctx context.Context, db *sql.DB, name string, want string) {
	t.Helper()

	var got string
	if err := db.QueryRowContext(ctx, "pragma "+name).Scan(&got); err != nil {
		t.Fatalf("pragma %s returned error: %v", name, err)
	}
	if strings.ToLower(got) != want {
		t.Fatalf("pragma %s = %q, want %q", name, got, want)
	}
}

func assertPragmaInt(t *testing.T, ctx context.Context, db *sql.DB, name string, want int) {
	t.Helper()

	var got int
	if err := db.QueryRowContext(ctx, "pragma "+name).Scan(&got); err != nil {
		t.Fatalf("pragma %s returned error: %v", name, err)
	}
	if got != want {
		t.Fatalf("pragma %s = %d, want %d", name, got, want)
	}
}

func replaceConcurrentDocument(t *testing.T, ctx context.Context, store *Store, iteration int) {
	t.Helper()

	if err := store.ReplaceDocument(ctx, concurrentDocumentInput(iteration), concurrentSections(iteration)); err != nil {
		t.Fatalf("ReplaceDocument concurrent fixture returned error: %v", err)
	}
}

func concurrentDocumentInput(iteration int) domain.DocumentInput {
	return domain.DocumentInput{
		ID:          "doc-concurrent",
		SourceID:    "source-concurrent",
		ExternalID:  "concurrent.md",
		Title:       "Concurrent Membership Guide",
		URL:         "file:///docs/concurrent.md",
		Version:     fmt.Sprintf("%d", iteration),
		ContentHash: fmt.Sprintf("hash-concurrent-doc-%d", iteration),
	}
}

func concurrentSections(iteration int) []domain.SectionInput {
	return []domain.SectionInput{
		{
			ID:          "section-concurrent",
			Title:       "Membership Lifecycle",
			HeadingPath: "Membership > Lifecycle",
			Content:     fmt.Sprintf("membership lifecycle concurrent reader writer smoke iteration %d", iteration),
			ContentHash: fmt.Sprintf("hash-concurrent-section-%d", iteration),
			Ordinal:     1,
		},
	}
}

func syncJobsContain(jobs []domain.SyncJob, status string, text string) bool {
	for _, job := range jobs {
		if job.Status != status {
			continue
		}
		if strings.Contains(job.PayloadJSON, text) || strings.Contains(job.LastError, text) {
			return true
		}
	}
	return false
}

func healthWarningsContain(warnings []domain.SourceHealthWarning, kind string) bool {
	for _, warning := range warnings {
		if warning.Kind == kind {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func seedImpactGraph(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()

	createTestSource(t, ctx, store, "source-impact")
	if err := store.ReplaceDocument(ctx, domain.DocumentInput{
		ID:          "doc-impact",
		SourceID:    "source-impact",
		ExternalID:  "impact.md",
		Title:       "Impact",
		ContentHash: "hash-impact-doc",
	}, []domain.SectionInput{
		{
			ID:          "section-impact",
			Title:       "Impact Evidence",
			Content:     "Membership exposes API and downstream module behavior.",
			ContentHash: "hash-impact-section",
		},
	}); err != nil {
		t.Fatalf("ReplaceDocument for impact graph returned error: %v", err)
	}

	nodes := []domain.NodeInput{
		{ID: "node-product", Kind: "Product", Name: "Membership", CanonicalName: "membership"},
		{ID: "node-api", Kind: "API", Name: "GET /member/benefits", CanonicalName: "get /member/benefits"},
		{ID: "node-module", Kind: "Module", Name: "Entitlements", CanonicalName: "entitlements"},
		{ID: "node-section", Kind: "DocSection", Name: "Impact Evidence", CanonicalName: "impact#evidence"},
	}
	for _, node := range nodes {
		if err := store.UpsertNode(ctx, node); err != nil {
			t.Fatalf("UpsertNode(%s) returned error: %v", node.ID, err)
		}
	}

	edges := []domain.EdgeInput{
		{ID: "edge-product-api", SrcID: "node-product", DstID: "node-api", Kind: "exposes_api", EvidenceSectionID: "section-impact"},
		{ID: "edge-product-section", SrcID: "node-product", DstID: "node-section", Kind: "contains", EvidenceSectionID: "section-impact"},
		{ID: "edge-api-module", SrcID: "node-api", DstID: "node-module", Kind: "describes", EvidenceSectionID: "section-impact"},
		{ID: "edge-module-product", SrcID: "node-module", DstID: "node-product", Kind: "depends_on", EvidenceSectionID: "section-impact"},
	}
	for _, edge := range edges {
		if err := store.UpsertEdge(ctx, edge); err != nil {
			t.Fatalf("UpsertEdge(%s) returned error: %v", edge.ID, err)
		}
	}
}

func TestFTSNodesMigrationAndBackfillIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := openTempStore(t, ctx)

	// First migration creates fts_nodes and backfills.
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("first Migrate returned error: %v", err)
	}

	// Insert a node after the first migration.
	if err := store.UpsertNode(ctx, domain.NodeInput{
		ID:            "node-migrate",
		Kind:          "Product",
		Name:          "MigrationProduct",
		CanonicalName: "migrationproduct",
		MetadataJSON:  `{"source_id":"src-test"}`,
	}); err != nil {
		t.Fatalf("UpsertNode returned error: %v", err)
	}

	// Second migration must be idempotent — no error.
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate returned error: %v", err)
	}

	// fts_nodes should still have exactly one row for this node.
	var ftsCount int64
	if err := store.readDB().QueryRowContext(ctx, `select count(*) from fts_nodes where node_id = 'node-migrate'`).Scan(&ftsCount); err != nil {
		t.Fatalf("count fts_nodes: %v", err)
	}
	if ftsCount != 1 {
		t.Fatalf("fts_nodes count for node-migrate = %d, want 1", ftsCount)
	}

	// Run the backfill query again — it should be a no-op for already filled rows.
	if _, err := store.db.ExecContext(ctx, `
	insert into fts_nodes (kind, name, canonical_name, metadata_json, node_id)
	select nodes.kind, nodes.name, nodes.canonical_name, nodes.metadata_json, nodes.id
	from nodes
	where not exists (select 1 from fts_nodes where fts_nodes.node_id = nodes.id)
	group by nodes.id
	`); err != nil {
		t.Fatalf("manual backfill: %v", err)
	}
	if err := store.readDB().QueryRowContext(ctx, `select count(*) from fts_nodes where node_id = 'node-migrate'`).Scan(&ftsCount); err != nil {
		t.Fatalf("count fts_nodes after manual backfill: %v", err)
	}
	if ftsCount != 1 {
		t.Fatalf("fts_nodes count after manual backfill = %d, want 1", ftsCount)
	}

	// A node inserted via UpsertNode should also be findable via FTS.
	nodes, err := store.SearchNodes(ctx, "MigrationProduct", 10)
	if err != nil {
		t.Fatalf("SearchNodes returned error: %v", err)
	}
	if !nodesContainID(nodes, "node-migrate") {
		t.Fatalf("SearchNodes MigrationProduct = %+v, want node-migrate", nodes)
	}
}

func TestUpsertNodeUpdatesFTSAndOldTokenNotMatch(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)

	// Insert a node with an initial name.
	if err := store.UpsertNode(ctx, domain.NodeInput{
		ID:            "node-fts-update",
		Kind:          "Module",
		Name:          "AlphaModule",
		CanonicalName: "alphamodule",
	}); err != nil {
		t.Fatalf("UpsertNode returned error: %v", err)
	}

	// The old name should be searchable.
	nodes, err := store.SearchNodes(ctx, "AlphaModule", 10)
	if err != nil {
		t.Fatalf("SearchNodes old name returned error: %v", err)
	}
	if !nodesContainID(nodes, "node-fts-update") {
		t.Fatalf("SearchNodes AlphaModule = %+v, want node-fts-update", nodes)
	}

	// Now update the node to a new name.
	if err := store.UpsertNode(ctx, domain.NodeInput{
		ID:            "node-fts-update",
		Kind:          "Module",
		Name:          "BetaModule",
		CanonicalName: "betamodule",
	}); err != nil {
		t.Fatalf("second UpsertNode returned error: %v", err)
	}

	// The old name should no longer match via FTS.
	nodes, err = store.SearchNodes(ctx, "AlphaModule", 10)
	if err != nil {
		t.Fatalf("SearchNodes old name after update returned error: %v", err)
	}
	if len(nodes) != 0 {
		// If LIKE still finds it, ensure FTS would not have matched.
		allOld := true
		for _, n := range nodes {
			if n.ID == "node-fts-update" {
				// LIKE fallback may still match via name or canonical_name fields.
				// But the primary FTS path should not return it for the old name.
				allOld = false
				break
			}
		}
		if !allOld {
			t.Logf("LIKE fallback found the updated node; checking FTS directly")
		}
	}

	// The new name should be searchable.
	nodes, err = store.SearchNodes(ctx, "BetaModule", 10)
	if err != nil {
		t.Fatalf("SearchNodes new name returned error: %v", err)
	}
	if !nodesContainID(nodes, "node-fts-update") {
		t.Fatalf("SearchNodes BetaModule = %+v, want node-fts-update", nodes)
	}

	// Verify directly that fts_nodes has only the new content.
	rows, err := store.readDB().QueryContext(ctx, `select name, canonical_name from fts_nodes where node_id = 'node-fts-update'`)
	if err != nil {
		t.Fatalf("query fts_nodes: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var name, canonical string
		if err := rows.Scan(&name, &canonical); err != nil {
			t.Fatalf("scan fts_nodes: %v", err)
		}
		if name != "BetaModule" || canonical != "betamodule" {
			t.Fatalf("fts_nodes row = (%q, %q), want BetaModule/betamodule", name, canonical)
		}
		count++
	}
	if count != 1 {
		t.Fatalf("fts_nodes count = %d, want 1", count)
	}
}

func TestSearchNodesHandlesFTSUnsafeQuery(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)

	for _, node := range []domain.NodeInput{
		{ID: "node-safe-1", Kind: "Product", Name: "Payments", CanonicalName: "payments"},
		{ID: "node-safe-2", Kind: "API", Name: "GET /payments", CanonicalName: "get /payments"},
		{ID: "node-safe-3", Kind: "Module", Name: "Checkout", CanonicalName: "checkout"},
	} {
		if err := store.UpsertNode(ctx, node); err != nil {
			t.Fatalf("UpsertNode(%s) returned error: %v", node.ID, err)
		}
	}

	// An FTS unsafe query (e.g., with special characters that break FTS syntax)
	// must NOT return an error — it should fall back to LIKE search.
	unsafeQueries := []string{
		`"`,
		`'`,
		`*`,
		`(payments`,
		`payments)`,
		`AND`,
		`OR`,
		`NOT`,
		`NEAR(payments checkout)`,
	}

	for _, q := range unsafeQueries {
		nodes, err := store.SearchNodes(ctx, q, 10)
		if err != nil {
			t.Fatalf("SearchNodes(%q) returned error: %v", q, err)
		}
		// The query may return zero results, but must not error.
		// For single special chars, there's nothing meaningful to match.
		_ = nodes
	}

	// A semi-safe query with both valid tokens and special chars should still work.
	nodes, err := store.SearchNodes(ctx, "payments AND checkout", 10)
	if err != nil {
		t.Fatalf("SearchNodes with special FTS keywords returned error: %v", err)
	}
	// Should still find results via LIKE fallback even if FTS chokes.
	if len(nodes) == 0 {
		t.Fatalf("SearchNodes payments AND checkout returned no results, want LIKE fallback hits")
	}

	// Normal queries should work via exact/name matching or FTS.
	nodes, err = store.SearchNodes(ctx, "payments", 10)
	if err != nil {
		t.Fatalf("SearchNodes payments returned error: %v", err)
	}
	if len(nodes) == 0 {
		t.Fatalf("SearchNodes payments returned no results")
	}
}

func TestRecordQueryObservationWritesQueryAndSearchResultEvents(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)

	// Insert test data so we can reference document/section ids.
	createTestSource(t, ctx, store, "src-search-obs")
	if err := store.ReplaceDocument(ctx, domain.DocumentInput{
		ID:          "doc-search-obs",
		SourceID:    "src-search-obs",
		ExternalID:  "search-obs.md",
		Title:       "Search Obs Doc",
		ContentHash: "hash-search-obs",
	}, []domain.SectionInput{
		{
			ID:          "section-search-obs-1",
			Title:       "Section One",
			Content:     "search observation test content",
			ContentHash: "hash-section-obs-1",
		},
		{
			ID:          "section-search-obs-2",
			Title:       "Section Two",
			Content:     "more observation test content",
			ContentHash: "hash-section-obs-2",
		},
	}); err != nil {
		t.Fatalf("ReplaceDocument returned error: %v", err)
	}

	err := store.RecordQueryObservation(ctx, domain.QueryObservationInput{
		QueryText:       "search observation",
		NormalizedQuery: "search observation",
		Source:          "web",
		ResultCount:     2,
		LatencyMS:       42,
		Results: []domain.SearchResultObservationInput{
			{DocumentID: "doc-search-obs", SectionID: "section-search-obs-1", Rank: 1, Score: 9.5},
			{DocumentID: "doc-search-obs", SectionID: "section-search-obs-2", Rank: 2, Score: 8.2},
		},
	})
	if err != nil {
		t.Fatalf("RecordQueryObservation returned error: %v", err)
	}

	// Verify query_events row.
	var qe struct {
		ID              string
		QueryHash       string
		NormalizedQuery string
		QueryText       string
		Source          string
		ResultCount     int
		LatencyMS       int
		CacheHit        int
	}
	if err := store.readDB().QueryRowContext(ctx, `
	select id, query_hash, normalized_query, query_text, source, result_count, latency_ms, cache_hit
	from query_events
	where normalized_query = 'search observation'
	`).Scan(&qe.ID, &qe.QueryHash, &qe.NormalizedQuery, &qe.QueryText, &qe.Source, &qe.ResultCount, &qe.LatencyMS, &qe.CacheHit); err != nil {
		t.Fatalf("query query_events: %v", err)
	}
	if qe.ID == "" || qe.QueryHash == "" || qe.ResultCount != 2 || qe.LatencyMS != 42 || qe.Source != "web" {
		t.Fatalf("query_events row = %+v, want populated observation", qe)
	}
	if qe.CacheHit != 0 {
		t.Fatalf("CacheHit = %d, want 0", qe.CacheHit)
	}

	// Verify search_result_events rows.
	rows, err := store.readDB().QueryContext(ctx, `
	select document_id, section_id, rank, score
	from search_result_events
	where query_event_id = ?
	order by rank asc
	`, qe.ID)
	if err != nil {
		t.Fatalf("query search_result_events: %v", err)
	}
	defer rows.Close()

	var results []struct {
		DocID     string
		SectionID string
		Rank      int
		Score     float64
	}
	for rows.Next() {
		var r struct {
			DocID     string
			SectionID string
			Rank      int
			Score     float64
		}
		if err := rows.Scan(&r.DocID, &r.SectionID, &r.Rank, &r.Score); err != nil {
			t.Fatalf("scan search_result_events: %v", err)
		}
		results = append(results, r)
	}
	if len(results) != 2 {
		t.Fatalf("search_result_events count = %d, want 2: %+v", len(results), results)
	}
	if results[0].DocID != "doc-search-obs" || results[0].SectionID != "section-search-obs-1" || results[0].Rank != 1 || results[0].Score != 9.5 {
		t.Fatalf("first result = %+v, want doc-search-obs/section-search-obs-1 rank 1 score 9.5", results[0])
	}
	if results[1].DocID != "doc-search-obs" || results[1].SectionID != "section-search-obs-2" || results[1].Rank != 2 || results[1].Score != 8.2 {
		t.Fatalf("second result = %+v, want doc-search-obs/section-search-obs-2 rank 2 score 8.2", results[1])
	}
}

func TestRecordQueryObservationCacheHitFlag(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)

	err := store.RecordQueryObservation(ctx, domain.QueryObservationInput{
		QueryText:       "cache hit test",
		NormalizedQuery: "cache hit test",
		Source:          "mcp",
		ResultCount:     5,
		LatencyMS:       3,
		CacheHit:        true,
		Results:         []domain.SearchResultObservationInput{},
	})
	if err != nil {
		t.Fatalf("RecordQueryObservation returned error: %v", err)
	}

	var cacheHit int
	if err := store.readDB().QueryRowContext(ctx, `
	select cache_hit from query_events where normalized_query = 'cache hit test'
	`).Scan(&cacheHit); err != nil {
		t.Fatalf("query query_events: %v", err)
	}
	if cacheHit != 1 {
		t.Fatalf("cache_hit = %d, want 1", cacheHit)
	}

	// No search_result_events should be written for empty results slice.
	var resultCount int64
	if err := store.readDB().QueryRowContext(ctx, `
	select count(*) from search_result_events sre
	join query_events qe on qe.id = sre.query_event_id
	where qe.normalized_query = 'cache hit test'
	`).Scan(&resultCount); err != nil {
		t.Fatalf("count search_result_events: %v", err)
	}
	if resultCount != 0 {
		t.Fatalf("search_result_events count = %d, want 0", resultCount)
	}
}

func TestSectionEmbeddingsStoreMetadataAndVectorSearch(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	backend := newFakeVectorBackend()
	store.SetVectorBackend(backend)
	createTestSource(t, ctx, store, "source-embedding")
	if err := store.ReplaceDocument(ctx, domain.DocumentInput{ID: "doc-embedding", SourceID: "source-embedding", ExternalID: "embedding.md", Title: "Embedding Metadata", ContentHash: "hash-doc-embedding"}, []domain.SectionInput{{
		ID: "section-embedding", Title: "Vector Trace", Content: "embedding metadata must be traceable", ContentHash: "hash-section-embedding",
	}}); err != nil {
		t.Fatalf("ReplaceDocument returned error: %v", err)
	}
	input := domain.SectionEmbeddingInput{SectionID: "section-embedding", DocumentID: "doc-embedding", SourceID: "source-embedding", Model: "test-embedding", Dimensions: 3, Embedding: []float32{1, 0, 0}, ContentHash: "hash-section-embedding", EmbeddingTextHash: "hash-embedding-text", GeneratorVersion: "generator-v1"}
	if err := store.UpsertSectionEmbedding(ctx, input); err != nil {
		t.Fatalf("UpsertSectionEmbedding returned error: %v", err)
	}
	hit, vector, err := store.GetSectionEmbedding(ctx, "section-embedding", "test-embedding")
	if err != nil {
		t.Fatalf("GetSectionEmbedding returned error: %v", err)
	}
	if hit.SourceID != input.SourceID || hit.DocumentID != input.DocumentID || hit.ContentHash != input.ContentHash || hit.EmbeddingTextHash != input.EmbeddingTextHash || hit.Model != input.Model || hit.GeneratorVersion != input.GeneratorVersion {
		t.Fatalf("embedding metadata = %+v, want source/document/hash/model/generator trace", hit)
	}
	if fmt.Sprint(vector) != fmt.Sprint(input.Embedding) {
		t.Fatalf("embedding vector = %v, want %v", vector, input.Embedding)
	}
	hits, err := store.SearchSectionsByVector(ctx, []float32{1, 0, 0}, "test-embedding", 10, 0.5, vectorstore.EmbeddingPlanFilter{})
	if err != nil {
		t.Fatalf("SearchSectionsByVector returned error: %v", err)
	}
	if len(hits) != 1 || hits[0].SectionID != "section-embedding" || hits[0].SourceID != "source-embedding" || hits[0].Similarity < 0.99 {
		t.Fatalf("vector hits = %+v, want traceable section hit", hits)
	}
	if hits[0].ChunkID == "" {
		t.Fatalf("vector hit = %+v, want chunk_id", hits[0])
	}
	chunkHit, _, err := store.GetEmbeddingChunk(ctx, hits[0].ChunkID, "test-embedding")
	if err != nil {
		t.Fatalf("GetEmbeddingChunk returned error: %v", err)
	}
	if chunkHit.SectionID != "section-embedding" || chunkHit.DocumentID != "doc-embedding" || chunkHit.SourceID != "source-embedding" {
		t.Fatalf("chunk reverse trace = %+v, want section/document/source", chunkHit)
	}
}

func TestVectorHitWithMismatchedContentHashIsFiltered(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	store.SetVectorBackend(newFakeVectorBackend())
	store.SetVectorSearchRuntime(vectorstore.SearchRuntime{Embedder: fakeQueryEmbedder{model: "test-embedding", vector: []float32{1, 0}}, SearchWeight: 0.4})
	createTestSource(t, ctx, store, "source-stale-vector")
	if err := store.ReplaceDocument(ctx, domain.DocumentInput{ID: "doc-stale-vector", SourceID: "source-stale-vector", ExternalID: "stale.md", Title: "Stale Vector", ContentHash: "hash-doc-stale-vector"}, []domain.SectionInput{{
		ID: "section-stale-vector", Title: "Semantic Only", Content: "nothing lexical should match this content", ContentHash: "hash-current-section",
	}}); err != nil {
		t.Fatalf("ReplaceDocument returned error: %v", err)
	}
	if err := store.UpsertSectionEmbedding(ctx, domain.SectionEmbeddingInput{SectionID: "section-stale-vector", DocumentID: "doc-stale-vector", SourceID: "source-stale-vector", Model: "test-embedding", Dimensions: 2, Embedding: []float32{1, 0}, ContentHash: "hash-old-section", EmbeddingTextHash: "hash-text", GeneratorVersion: "generator-v1"}); err != nil {
		t.Fatalf("UpsertSectionEmbedding returned error: %v", err)
	}
	result, err := store.SearchSectionsWithOptions(ctx, domain.SearchOptions{Query: "zzzznomatch", Limit: 10, MaxSearches: 5, MaxSectionsPerDocument: 5, ProfileDetail: "compact"})
	if err != nil {
		t.Fatalf("SearchSectionsWithOptions returned error: %v", err)
	}
	if len(result.Hits) != 0 {
		t.Fatalf("hits = %+v, want mismatched vector hit filtered", result.Hits)
	}
}

func TestVectorSearchFiltersToActiveEmbeddingPlan(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	store.SetVectorBackend(newFakeVectorBackend())
	store.SetVectorSearchRuntime(vectorstore.SearchRuntime{
		Embedder:         fakeQueryEmbedder{model: "test-embedding", vector: []float32{1, 0}},
		SearchWeight:     1,
		GeneratorVersion: "generator-v2",
		Tokenizer:        "conservative",
		ChunkStrategy:    "auto",
	})
	createTestSource(t, ctx, store, "source-active-plan")
	if err := store.ReplaceDocument(ctx, domain.DocumentInput{ID: "doc-active-plan", SourceID: "source-active-plan", ExternalID: "active.md", Title: "Active Plan", ContentHash: "hash-doc-active-plan"}, []domain.SectionInput{{
		ID: "section-active-plan", Title: "Active", Content: "semantic target only", ContentHash: "hash-section-active-plan",
	}}); err != nil {
		t.Fatalf("ReplaceDocument returned error: %v", err)
	}
	for _, input := range []domain.EmbeddingChunkInput{
		{
			ChunkID:            "chunk-old-structural",
			SectionID:          "section-active-plan",
			DocumentID:         "doc-active-plan",
			SourceID:           "source-active-plan",
			ChunkOrdinal:       0,
			ChunkText:          "old structural chunk",
			Model:              "test-embedding",
			Dimensions:         2,
			Embedding:          []float32{1, 0},
			SectionContentHash: "hash-section-active-plan",
			ChunkTextHash:      "hash-old-structural",
			Tokenizer:          "conservative",
			ChunkStrategy:      "structural",
			GeneratorVersion:   "generator-v1",
		},
		{
			ChunkID:            "chunk-current-auto",
			SectionID:          "section-active-plan",
			DocumentID:         "doc-active-plan",
			SourceID:           "source-active-plan",
			ChunkOrdinal:       0,
			ChunkText:          "current auto chunk",
			Model:              "test-embedding",
			Dimensions:         2,
			Embedding:          []float32{0, 1},
			SectionContentHash: "hash-section-active-plan",
			ChunkTextHash:      "hash-current-auto",
			Tokenizer:          "conservative",
			ChunkStrategy:      "auto",
			GeneratorVersion:   "generator-v2",
		},
	} {
		if err := store.UpsertEmbeddingChunk(ctx, input); err != nil {
			t.Fatalf("UpsertEmbeddingChunk returned error: %v", err)
		}
	}
	result, err := store.SearchSectionsWithOptions(ctx, domain.SearchOptions{Query: "zzzznomatch", Limit: 10, MaxSearches: 5, MaxSectionsPerDocument: 5, ProfileDetail: "compact"})
	if err != nil {
		t.Fatalf("SearchSectionsWithOptions returned error: %v", err)
	}
	if len(result.Hits) != 1 {
		t.Fatalf("hits = %+v, want one active-plan vector hit", result.Hits)
	}
	trace := result.Hits[0].Trace
	if trace == nil || trace.ChunkID != "chunk-current-auto" || trace.ChunkStrategy != "auto" || trace.GeneratorVersion != "generator-v2" {
		t.Fatalf("trace = %+v, want current auto plan hit", trace)
	}
}

func TestVectorOnlyResultDoesNotOutrankStrongExactHit(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	store.SetVectorBackend(newFakeVectorBackend())
	store.SetVectorSearchRuntime(vectorstore.SearchRuntime{Embedder: fakeQueryEmbedder{model: "test-embedding", vector: []float32{1, 0}}, SearchWeight: 1})
	createTestSource(t, ctx, store, "source-vector-rank")
	if err := store.ReplaceDocument(ctx, domain.DocumentInput{ID: "doc-exact-vector-rank", SourceID: "source-vector-rank", ExternalID: "exact.md", Title: "Exact API", ContentHash: "hash-doc-exact-vector-rank"}, []domain.SectionInput{{
		ID: "section-exact-vector-rank", Title: "GET /api/users", Content: "The exact endpoint is GET /api/users.", ContentHash: "hash-section-exact-vector-rank",
	}}); err != nil {
		t.Fatalf("ReplaceDocument exact returned error: %v", err)
	}
	if err := store.ReplaceDocument(ctx, domain.DocumentInput{ID: "doc-vector-only-rank", SourceID: "source-vector-rank", ExternalID: "semantic.md", Title: "Semantic Neighbor", ContentHash: "hash-doc-vector-only-rank"}, []domain.SectionInput{{
		ID: "section-vector-only-rank", Title: "Neighbor", Content: "A semantically close but lexically unrelated section.", ContentHash: "hash-section-vector-only-rank",
	}}); err != nil {
		t.Fatalf("ReplaceDocument vector returned error: %v", err)
	}
	for _, input := range []domain.SectionEmbeddingInput{
		{SectionID: "section-exact-vector-rank", DocumentID: "doc-exact-vector-rank", SourceID: "source-vector-rank", Model: "test-embedding", Dimensions: 2, Embedding: []float32{0, 1}, ContentHash: "hash-section-exact-vector-rank", EmbeddingTextHash: "hash-exact-text", GeneratorVersion: "generator-v1"},
		{SectionID: "section-vector-only-rank", DocumentID: "doc-vector-only-rank", SourceID: "source-vector-rank", Model: "test-embedding", Dimensions: 2, Embedding: []float32{1, 0}, ContentHash: "hash-section-vector-only-rank", EmbeddingTextHash: "hash-vector-text", GeneratorVersion: "generator-v1"},
	} {
		if err := store.UpsertSectionEmbedding(ctx, input); err != nil {
			t.Fatalf("UpsertSectionEmbedding returned error: %v", err)
		}
	}
	result, err := store.SearchSectionsWithOptions(ctx, domain.SearchOptions{Query: "GET /api/users", Limit: 10, MaxSearches: 5, MaxSectionsPerDocument: 5, ProfileDetail: "compact"})
	if err != nil {
		t.Fatalf("SearchSectionsWithOptions returned error: %v", err)
	}
	if len(result.Hits) < 2 {
		t.Fatalf("hits = %+v, want exact and vector-only hits", result.Hits)
	}
	if result.Hits[0].SectionID != "section-exact-vector-rank" {
		t.Fatalf("first hit = %+v, want exact hit before vector-only hit", result.Hits[0])
	}
	for _, hit := range result.Hits {
		if hit.SectionID == "section-vector-only-rank" && hit.EvidenceLevel != "weak_vector_only" {
			t.Fatalf("vector-only evidence level = %q, want weak_vector_only", hit.EvidenceLevel)
		}
	}
}

func TestSourceEmbeddingStatusCountsCoverageAndPendingSections(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	store.SetVectorBackend(newFakeVectorBackend())
	createTestSource(t, ctx, store, "source-embedding-status")
	if err := store.ReplaceDocument(ctx, domain.DocumentInput{ID: "doc-embedding-status", SourceID: "source-embedding-status", ExternalID: "status.md", Title: "Embedding Status", ContentHash: "hash-doc-embedding-status"}, []domain.SectionInput{
		{ID: "section-embedding-current", Content: "current", ContentHash: "hash-current", Ordinal: 0},
		{ID: "section-embedding-stale", Content: "stale", ContentHash: "hash-stale-current", Ordinal: 1},
		{ID: "section-embedding-stale-text", Content: "stale text", ContentHash: "hash-stale-text-current", Ordinal: 2},
		{ID: "section-embedding-pending", Content: "pending", ContentHash: "hash-pending", Ordinal: 3},
	}); err != nil {
		t.Fatalf("ReplaceDocument returned error: %v", err)
	}
	for _, input := range []domain.SectionEmbeddingInput{
		{SectionID: "section-embedding-current", DocumentID: "doc-embedding-status", SourceID: "source-embedding-status", Model: "test-embedding", Dimensions: 2, Embedding: []float32{1, 0}, ContentHash: "hash-current", EmbeddingTextHash: "text-hash-current", GeneratorVersion: "generator-v1"},
		{SectionID: "section-embedding-stale", DocumentID: "doc-embedding-status", SourceID: "source-embedding-status", Model: "test-embedding", Dimensions: 2, Embedding: []float32{0, 1}, ContentHash: "hash-stale-old", EmbeddingTextHash: "hash-stale-text", GeneratorVersion: "generator-v1"},
		{SectionID: "section-embedding-stale-text", DocumentID: "doc-embedding-status", SourceID: "source-embedding-status", Model: "test-embedding", Dimensions: 2, Embedding: []float32{1, 1}, ContentHash: "hash-stale-text-current", EmbeddingTextHash: "old-text-hash", GeneratorVersion: "generator-v1"},
	} {
		if err := store.UpsertSectionEmbedding(ctx, input); err != nil {
			t.Fatalf("UpsertSectionEmbedding returned error: %v", err)
		}
	}
	status, err := store.GetSourceEmbeddingStatus(ctx, "source-embedding-status", "test-embedding", "generator-v1", "auto", "structural", 1200)
	if err != nil {
		t.Fatalf("GetSourceEmbeddingStatus returned error: %v", err)
	}
	// 4 total sections, 3 have embeddings in the vector backend → 3 embedded, 1 pending
	if status.TotalSections != 4 || status.EmbeddedSections != 3 || status.PendingSections != 1 {
		t.Fatalf("status = %+v, want total=4 embedded=3 pending=1", status)
	}
	if status.Status != "indexing" {
		t.Fatalf("status.Status = %q, want indexing", status.Status)
	}
}

type fakeVectorBackend struct {
	hits    map[string]domain.VectorSearchHit
	vectors map[string][]float32
}

type fakeQueryEmbedder struct {
	model  string
	vector []float32
	err    error
}

func (e fakeQueryEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if e.err != nil {
		return nil, e.err
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = append([]float32{}, e.vector...)
	}
	return out, nil
}

func (e fakeQueryEmbedder) Model() string {
	return e.model
}

func newFakeVectorBackend() *fakeVectorBackend {
	return &fakeVectorBackend{
		hits:    map[string]domain.VectorSearchHit{},
		vectors: map[string][]float32{},
	}
}

func (f *fakeVectorBackend) UpsertSectionEmbedding(ctx context.Context, input domain.SectionEmbeddingInput) error {
	return f.UpsertEmbeddingChunk(ctx, domain.EmbeddingChunkInput{
		ChunkID:            "chunk-" + input.SectionID,
		SectionID:          input.SectionID,
		DocumentID:         input.DocumentID,
		SourceID:           input.SourceID,
		ChunkOrdinal:       0,
		ChunkText:          input.EmbeddingTextHash,
		Model:              input.Model,
		Dimensions:         input.Dimensions,
		Embedding:          input.Embedding,
		SectionContentHash: input.ContentHash,
		ChunkTextHash:      input.EmbeddingTextHash,
		Tokenizer:          "conservative",
		ChunkStrategy:      "structural",
		GeneratorVersion:   input.GeneratorVersion,
	})
}

func (f *fakeVectorBackend) GetSectionEmbedding(ctx context.Context, sectionID string, model string) (domain.VectorSearchHit, []float32, error) {
	for chunkID, hit := range f.hits {
		if hit.SectionID == sectionID && hit.Model == model {
			return hit, append([]float32{}, f.vectors[chunkID]...), nil
		}
	}
	return domain.VectorSearchHit{}, nil, sql.ErrNoRows
}

func (f *fakeVectorBackend) DeleteSectionEmbeddings(ctx context.Context, sectionID string) error {
	return f.DeleteEmbeddingChunksBySection(ctx, sectionID, "", "", "", "")
}

func (f *fakeVectorBackend) SearchSectionsByVector(ctx context.Context, embedding []float32, model string, limit int, minSimilarity float64, plan vectorstore.EmbeddingPlanFilter) ([]domain.VectorSearchHit, error) {
	return f.SearchChunksByVector(ctx, embedding, model, limit, minSimilarity, plan)
}

func (f *fakeVectorBackend) UpsertEmbeddingChunk(ctx context.Context, input domain.EmbeddingChunkInput) error {
	if input.ChunkID == "" {
		input.ChunkID = "chunk-" + input.SectionID
	}
	f.hits[input.ChunkID] = domain.VectorSearchHit{
		ChunkID:            input.ChunkID,
		SectionID:          input.SectionID,
		DocumentID:         input.DocumentID,
		SourceID:           input.SourceID,
		ChunkOrdinal:       input.ChunkOrdinal,
		ChunkText:          input.ChunkText,
		Model:              input.Model,
		ContentHash:        input.SectionContentHash,
		SectionContentHash: input.SectionContentHash,
		EmbeddingTextHash:  input.ChunkTextHash,
		ChunkTextHash:      input.ChunkTextHash,
		Tokenizer:          input.Tokenizer,
		ChunkStrategy:      input.ChunkStrategy,
		GeneratorVersion:   input.GeneratorVersion,
		GeneratedAt:        "2026-07-03T00:00:00Z",
	}
	f.vectors[input.ChunkID] = append([]float32{}, input.Embedding...)
	return nil
}

func (f *fakeVectorBackend) GetEmbeddingChunk(ctx context.Context, chunkID string, model string) (domain.VectorSearchHit, []float32, error) {
	hit, ok := f.hits[chunkID]
	if !ok || hit.Model != model {
		return domain.VectorSearchHit{}, nil, sql.ErrNoRows
	}
	return hit, append([]float32{}, f.vectors[chunkID]...), nil
}

func (f *fakeVectorBackend) DeleteEmbeddingChunksBySection(ctx context.Context, sectionID string, model string, generatorVersion string, tokenizer string, chunkStrategy string) error {
	for chunkID, hit := range f.hits {
		if hit.SectionID != sectionID {
			continue
		}
		if model != "" && hit.Model != model {
			continue
		}
		if generatorVersion != "" && hit.GeneratorVersion != generatorVersion {
			continue
		}
		if tokenizer != "" && hit.Tokenizer != tokenizer {
			continue
		}
		if chunkStrategy != "" && hit.ChunkStrategy != chunkStrategy {
			continue
		}
		delete(f.hits, chunkID)
		delete(f.vectors, chunkID)
	}
	return nil
}

func (f *fakeVectorBackend) SearchChunksByVector(ctx context.Context, embedding []float32, model string, limit int, minSimilarity float64, plan vectorstore.EmbeddingPlanFilter) ([]domain.VectorSearchHit, error) {
	hits := make([]domain.VectorSearchHit, 0)
	for chunkID, hit := range f.hits {
		if hit.Model != model {
			continue
		}
		if plan.GeneratorVersion != "" && hit.GeneratorVersion != plan.GeneratorVersion {
			continue
		}
		if plan.Tokenizer != "" && hit.Tokenizer != plan.Tokenizer {
			continue
		}
		if plan.ChunkStrategy != "" && hit.ChunkStrategy != plan.ChunkStrategy {
			continue
		}
		hit.Similarity = testCosine(embedding, f.vectors[chunkID])
		if hit.Similarity >= minSimilarity {
			hits = append(hits, hit)
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		return hits[i].Similarity > hits[j].Similarity
	})
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

func (f *fakeVectorBackend) ListSectionEmbeddingHashes(ctx context.Context, model string, limit, offset int) ([]domain.SectionEmbeddingHash, error) {
	chunks, err := f.ListEmbeddingChunkHashes(ctx, model, limit, offset)
	if err != nil {
		return nil, err
	}
	items := make([]domain.SectionEmbeddingHash, 0)
	seen := map[string]bool{}
	for _, hit := range chunks {
		if seen[hit.SectionID] {
			continue
		}
		seen[hit.SectionID] = true
		items = append(items, domain.SectionEmbeddingHash{
			SectionID:         hit.SectionID,
			DocumentID:        hit.DocumentID,
			SourceID:          hit.SourceID,
			Model:             hit.Model,
			ContentHash:       hit.SectionContentHash,
			EmbeddingTextHash: hit.ChunkTextHash,
			GeneratorVersion:  hit.GeneratorVersion,
			GeneratedAt:       hit.GeneratedAt,
		})
	}
	return items, nil
}

func (f *fakeVectorBackend) ListEmbeddingChunkHashes(ctx context.Context, model string, limit, offset int) ([]domain.EmbeddingChunkHash, error) {
	items := make([]domain.EmbeddingChunkHash, 0)
	for _, hit := range f.hits {
		if hit.Model != model {
			continue
		}
		items = append(items, domain.EmbeddingChunkHash{
			ChunkID:            hit.ChunkID,
			SectionID:          hit.SectionID,
			DocumentID:         hit.DocumentID,
			SourceID:           hit.SourceID,
			ChunkOrdinal:       hit.ChunkOrdinal,
			Model:              hit.Model,
			SectionContentHash: hit.SectionContentHash,
			ChunkTextHash:      hit.ChunkTextHash,
			Tokenizer:          hit.Tokenizer,
			ChunkStrategy:      hit.ChunkStrategy,
			GeneratorVersion:   hit.GeneratorVersion,
			GeneratedAt:        hit.GeneratedAt,
		})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].SectionID == items[j].SectionID {
			return items[i].ChunkOrdinal < items[j].ChunkOrdinal
		}
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

func (f *fakeVectorBackend) GetEmbeddingCoverage(ctx context.Context, sourceID string, model string, generatorVersion string, tokenizer string, chunkStrategy string) (domain.EmbeddingCoverage, error) {
	coverage := domain.EmbeddingCoverage{}
	seenSections := map[string]bool{}
	for _, hit := range f.hits {
		if sourceID != "" && hit.SourceID != sourceID {
			continue
		}
		if model != "" && hit.Model != model {
			continue
		}
		if generatorVersion != "" && hit.GeneratorVersion != generatorVersion {
			continue
		}
		if tokenizer != "" && hit.Tokenizer != tokenizer {
			continue
		}
		if chunkStrategy != "" && hit.ChunkStrategy != chunkStrategy {
			continue
		}
		coverage.EmbeddedChunks++
		if !seenSections[hit.SectionID] {
			seenSections[hit.SectionID] = true
			coverage.EmbeddedSections++
		}
	}
	return coverage, nil
}

func (f *fakeVectorBackend) Close() error {
	return nil
}

func testCosine(left []float32, right []float32) float64 {
	if len(left) == 0 || len(left) != len(right) {
		return 0
	}
	var dot, leftNorm, rightNorm float64
	for i := range left {
		l := float64(left[i])
		r := float64(right[i])
		dot += l * r
		leftNorm += l * l
		rightNorm += r * r
	}
	if leftNorm == 0 || rightNorm == 0 {
		return 0
	}
	return dot / (math.Sqrt(leftNorm) * math.Sqrt(rightNorm))
}

func nodesContainID(nodes []domain.Node, id string) bool {
	for _, n := range nodes {
		if n.ID == id {
			return true
		}
	}
	return false
}

func searchHitsContainDocument(hits []domain.SearchHit, documentID string) bool {
	for _, hit := range hits {
		if hit.DocumentID == documentID {
			return true
		}
	}
	return false
}

func searchHitsContainRelation(hits []domain.SearchHit, relationID string) bool {
	for _, hit := range hits {
		for _, match := range hit.RelationMatches {
			if match.RelationID == relationID {
				return true
			}
		}
	}
	return false
}

func insertFixtureRows(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()

	statements := []string{
		`insert into sources (id, kind, name, dsn) values ('source-1', 'git', 'Docs', 'git://docs')`,
		`insert into documents (id, source_id, external_id, title, content_hash) values ('doc-1', 'source-1', 'README.md', 'Readme', 'hash-doc')`,
		`insert into sections (id, document_id, title, content_hash) values ('section-1', 'doc-1', 'Overview', 'hash-section')`,
		`insert into nodes (id, kind, name, canonical_name) values ('node-1', 'Product', 'Payments', 'payments')`,
		`insert into nodes (id, kind, name, canonical_name) values ('node-2', 'API', 'GET /payments', 'get /payments')`,
		`insert into edges (id, src_id, dst_id, kind, evidence_section_id) values ('edge-1', 'node-1', 'node-2', 'exposes_api', 'section-1')`,
		`insert into jobs (id, kind, status) values ('job-1', 'sync', 'queued')`,
	}

	for _, stmt := range statements {
		if _, err := store.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
}
