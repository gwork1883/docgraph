package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	_ "modernc.org/sqlite"

	"github.com/docgraph/docgraph/internal/domain"
	"github.com/docgraph/docgraph/internal/embeddingchunk"
	"github.com/docgraph/docgraph/internal/ids"
	"github.com/docgraph/docgraph/internal/searchtoken"
	"github.com/docgraph/docgraph/internal/storage/sqlschema"
	"github.com/docgraph/docgraph/internal/technical/extract"
	"github.com/docgraph/docgraph/internal/vectorstore"
)

type Store struct {
	db            *sql.DB
	reader        *sql.DB
	dsn           string
	vectorBackend vectorstore.Store
	vectorRuntime vectorstore.SearchRuntime
}

const (
	sectionTokenIndexVersion     = "gse-v1"
	sectionTokenIndexRebuilding  = sectionTokenIndexVersion + ":rebuilding"
	legacyNodeIndexVersion       = "v1"
	auxiliarySearchIndexVersion  = "v1"
	shortGramSearchIndexVersion  = "v2"
	graphMembershipSchemaVersion = 6
)

const (
	nodeSourceRefMetadata = 1 << iota
	nodeSourceRefOwner
)

const (
	edgeSourceRefOwner = 1 << iota
	edgeSourceRefEvidence
	edgeSourceRefSrcMetadata
	edgeSourceRefDstMetadata
)

func Open(ctx context.Context, dsn string) (*Store, error) {
	return open(ctx, dsn, true)
}

func OpenExisting(ctx context.Context, dsn string) (*Store, error) {
	return open(ctx, dsn, false)
}

func open(ctx context.Context, dsn string, create bool) (*Store, error) {
	path, err := pathFromDSN(dsn)
	if err != nil {
		return nil, err
	}
	if !create {
		if _, err := os.Stat(path); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	db, err := openSQLiteHandle(ctx, path, 1, "writer")
	if err != nil {
		return nil, err
	}
	reader, err := openSQLiteHandle(ctx, path, 8, "reader")
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	return &Store{db: db, reader: reader, dsn: dsn}, nil
}

func (s *Store) Close() error {
	if s.vectorBackend != nil {
		if err := s.vectorBackend.Close(); err != nil {
			if s.reader != nil {
				_ = s.reader.Close()
			}
			_ = s.db.Close()
			return err
		}
	}
	if s.reader == nil {
		return s.db.Close()
	}
	if err := s.reader.Close(); err != nil {
		_ = s.db.Close()
		return err
	}
	return s.db.Close()
}

func (s *Store) SetVectorBackend(backend vectorstore.Store) {
	s.vectorBackend = backend
}

func (s *Store) SetVectorSearchRuntime(runtime vectorstore.SearchRuntime) {
	s.vectorRuntime = runtime
}

func openSQLiteHandle(ctx context.Context, path string, maxOpenConns int, role string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s handle: %w", role, err)
	}
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxOpenConns)
	db.SetConnMaxLifetime(time.Hour)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite %s handle: %w", role, err)
	}
	if err := applyPragmas(ctx, db, role); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func sqliteDSN(path string) string {
	return path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(wal)&_pragma=synchronous(normal)"
}

func applyPragmas(ctx context.Context, db *sql.DB, role string) error {
	for _, stmt := range []string{
		`pragma journal_mode = wal`,
		`pragma busy_timeout = 5000`,
		`pragma foreign_keys = on`,
		`pragma synchronous = normal`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("apply sqlite %s %s: %w", role, stmt, err)
		}
	}
	return nil
}

func (s *Store) readDB() *sql.DB {
	if s.reader != nil {
		return s.reader
	}
	return s.db
}

func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, sqlschema.Schema); err != nil {
		return err
	}
	var previousSchemaVersion int
	if err := s.db.QueryRowContext(ctx, `select coalesce(max(version), 0) from schema_migrations`).Scan(&previousSchemaVersion); err != nil {
		return err
	}
	for _, stmt := range []string{
		`alter table sources add column sync_status text not null default 'active'`,
		`alter table sources add column sync_status_reason text not null default ''`,
		`alter table sources add column sync_paused_at text not null default ''`,
		`alter table jobs add column source_id text not null default ''`,
		`alter table jobs add column target_kind text not null default ''`,
		`alter table jobs add column target_id text not null default ''`,
		`alter table jobs add column progress_json text not null default '{}'`,
		`alter table jobs add column result_json text not null default '{}'`,
		`alter table jobs add column worker_id text not null default ''`,
		`alter table sections add column search_text text not null default ''`,
		`alter table nodes add column owner_source_id text references sources(id) on delete cascade`,
		`alter table nodes add column owner_scope text not null default ''`,
		`alter table edges add column owner_source_id text references sources(id) on delete cascade`,
		`alter table edges add column owner_scope text not null default ''`,
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx, `update sections set search_text = content where search_text = ''`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `
update jobs
set source_id = json_extract(payload_json, '$.source_id')
where source_id = ''
  and json_valid(payload_json)
  and json_extract(payload_json, '$.source_id') is not null
`); err != nil {
		return err
	}
	var unscopedSyncJobs int
	if err := s.db.QueryRowContext(ctx, `select count(*) from jobs where kind = 'sync_source' and trim(source_id) = ''`).Scan(&unscopedSyncJobs); err != nil {
		return err
	}
	if unscopedSyncJobs > 0 {
		return fmt.Errorf("cannot migrate %d sync_source jobs without source_id", unscopedSyncJobs)
	}
	if _, err := s.db.ExecContext(ctx, `
create index if not exists idx_jobs_kind_status_run_after
  on jobs(kind, status, run_after);
create index if not exists idx_jobs_source_kind_status
  on jobs(source_id, kind, status, updated_at);
create index if not exists idx_jobs_sync_source_latest
  on jobs(source_id) where kind = 'sync_source';
create index if not exists idx_jobs_target
  on jobs(target_kind, target_id, kind, status);
create index if not exists idx_feedback_target
  on feedback_events(target_kind, target_id, feedback_kind);
create index if not exists idx_nodes_owner
  on nodes(owner_source_id, owner_scope, id);
create index if not exists idx_edges_owner
  on edges(owner_source_id, owner_scope, id);
create index if not exists idx_nodes_name_nocase
  on nodes(name collate nocase);
create index if not exists idx_nodes_canonical_nocase
  on nodes(canonical_name collate nocase);
create index if not exists idx_section_entities_canonical_nocase
  on section_entities(canonical_text collate nocase);
create index if not exists idx_section_entities_path_nocase
  on section_entities(path collate nocase);
create index if not exists idx_section_entities_operation_nocase
  on section_entities(operation collate nocase);
create index if not exists idx_section_entities_method_path_nocase
  on section_entities((method || ' ' || path) collate nocase);
create table if not exists query_events (
  id text primary key,
  query_hash text not null,
  normalized_query text not null default '',
  query_text text not null default '',
  source text not null default '',
  result_count integer not null default 0,
  latency_ms integer not null default 0,
  cache_hit integer not null default 0,
  created_at text not null default current_timestamp
);
create table if not exists search_result_events (
  id text primary key,
  query_event_id text not null references query_events(id) on delete cascade,
  document_id text not null,
  section_id text not null,
  rank integer not null,
  score real not null default 0,
  clicked integer not null default 0,
  used_in_context integer not null default 0,
  created_at text not null default current_timestamp
);
create table if not exists search_index_meta (
  key text primary key,
  value text not null,
  updated_at text not null default current_timestamp
);
create index if not exists idx_query_events_hash_created
  on query_events(query_hash, created_at);
create index if not exists idx_search_result_events_query
  on search_result_events(query_event_id, rank);
create virtual table if not exists fts_nodes using fts5(
  kind,
  name,
  canonical_name,
  metadata_json,
  node_id unindexed
);
create virtual table if not exists fts_nodes_trigram using fts5(
  id,
  kind,
  name,
  canonical_name,
  metadata_json,
  node_id unindexed,
  tokenize='trigram'
);
create virtual table if not exists fts_nodes_shortgrams using fts5(
  id_grams,
  kind_grams,
  name_grams,
  canonical_name_grams,
  node_id unindexed
);
create virtual table if not exists fts_document_profiles using fts5(
  desc_tokens,
  profile_tokens,
  document_id unindexed
);
create virtual table if not exists fts_document_profiles_trigram using fts5(
  desc_tokens,
  profile_tokens,
  document_id unindexed,
  tokenize='trigram'
);
create virtual table if not exists fts_section_entities using fts5(
  canonical_text,
  raw_text,
  path,
  operation,
  entity_id unindexed
);
create virtual table if not exists fts_section_entities_trigram using fts5(
  canonical_text,
  raw_text,
  path,
  operation,
  entity_id unindexed,
  tokenize='trigram'
);
create virtual table if not exists fts_section_tokens using fts5(
  title_tokens,
  section_heading_tokens,
  symbol_tokens,
  content_tokens,
  section_id unindexed,
  document_id unindexed
);
create virtual table if not exists fts_section_tokens_trigram using fts5(
  title_tokens,
  section_heading_tokens,
  symbol_tokens,
  content_tokens,
  section_id unindexed,
  document_id unindexed,
  tokenize='trigram'
);
create virtual table if not exists fts_section_shortgrams using fts5(
  title_grams,
  section_heading_grams,
  content_grams,
  section_id unindexed,
  document_id unindexed
);
`); err != nil {
		return err
	}
	if previousSchemaVersion < graphMembershipSchemaVersion {
		if err := s.rebuildGraphMemberships(ctx); err != nil {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx, `
update jobs
set status = 'failed',
    last_error = 'Previous run interrupted (process exited while job was running)',
    worker_id = '',
    locked_until = null,
    updated_at = current_timestamp
where status = 'running'
`); err != nil {
		return err
	}
	var duplicateActiveSyncSources int
	if err := s.db.QueryRowContext(ctx, `
select count(*)
from (
  select source_id
  from jobs
  where kind = 'sync_source'
    and status in ('queued', 'running', 'canceling')
  group by source_id
  having count(*) > 1
)
`).Scan(&duplicateActiveSyncSources); err != nil {
		return err
	}
	if duplicateActiveSyncSources > 0 {
		return fmt.Errorf("cannot migrate sync_source jobs: %d sources have multiple active jobs", duplicateActiveSyncSources)
	}
	if _, err := s.db.ExecContext(ctx, `
create unique index if not exists idx_jobs_sync_source_active
  on jobs(source_id)
  where kind = 'sync_source'
    and status in ('queued', 'running', 'canceling')
`); err != nil {
		return err
	}
	if err := s.EnsureSearchIndexes(ctx); err != nil {
		return err
	}
	return s.recordSchemaVersion(ctx)
}

// rebuildGraphMemberships is the one-time compatibility bridge for databases
// that predate the materialized graph membership tables. It is deliberately
// idempotent because `docgraph migrate` may be retried after an interrupted
// run. JSON is inspected here, during migration, instead of on online reads.
func (s *Store) rebuildGraphMemberships(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, table := range []string{"source_edges", "document_nodes", "source_nodes"} {
		if _, err := tx.ExecContext(ctx, "delete from "+table); err != nil {
			return fmt.Errorf("clear %s before graph membership rebuild: %w", table, err)
		}
	}

	statements := []struct {
		query string
		args  []any
	}{
		{`
insert into source_nodes (source_id, node_id, ref_mask)
select sources.id, nodes.id, ?
from nodes join sources on sources.id = nodes.owner_source_id
where true
on conflict(source_id, node_id) do update set ref_mask = source_nodes.ref_mask | excluded.ref_mask
`, []any{nodeSourceRefOwner}},
		{`
insert into source_nodes (source_id, node_id, ref_mask)
select sources.id, nodes.id, ?
from nodes
join sources on sources.id = json_extract(
  case when json_valid(nodes.metadata_json) then nodes.metadata_json else '{}' end,
  '$.source_id'
)
where true
on conflict(source_id, node_id) do update set ref_mask = source_nodes.ref_mask | excluded.ref_mask
`, []any{nodeSourceRefMetadata}},
		{`
insert into document_nodes (document_id, node_id)
select documents.id, nodes.id
from nodes
join documents on documents.id = json_extract(
  case when json_valid(nodes.metadata_json) then nodes.metadata_json else '{}' end,
  '$.document_id'
)
where true
on conflict(document_id, node_id) do nothing
`, nil},
		{`
insert into source_edges (source_id, edge_id, ref_mask)
select sources.id, edges.id, ?
from edges join sources on sources.id = edges.owner_source_id
where true
on conflict(source_id, edge_id) do update set ref_mask = source_edges.ref_mask | excluded.ref_mask
`, []any{edgeSourceRefOwner}},
		{`
insert into source_edges (source_id, edge_id, ref_mask)
select documents.source_id, edges.id, ?
from edges
join sections on sections.id = edges.evidence_section_id
join documents on documents.id = sections.document_id
where true
on conflict(source_id, edge_id) do update set ref_mask = source_edges.ref_mask | excluded.ref_mask
`, []any{edgeSourceRefEvidence}},
		{`
insert into source_edges (source_id, edge_id, ref_mask)
select source_nodes.source_id, edges.id, ?
from edges
join source_nodes on source_nodes.node_id = edges.src_id
where source_nodes.ref_mask & ? != 0
on conflict(source_id, edge_id) do update set ref_mask = source_edges.ref_mask | excluded.ref_mask
`, []any{edgeSourceRefSrcMetadata, nodeSourceRefMetadata}},
		{`
insert into source_edges (source_id, edge_id, ref_mask)
select source_nodes.source_id, edges.id, ?
from edges
join source_nodes on source_nodes.node_id = edges.dst_id
where source_nodes.ref_mask & ? != 0
on conflict(source_id, edge_id) do update set ref_mask = source_edges.ref_mask | excluded.ref_mask
`, []any{edgeSourceRefDstMetadata, nodeSourceRefMetadata}},
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
			return fmt.Errorf("rebuild graph memberships: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit graph membership rebuild: %w", err)
	}
	return nil
}

func (s *Store) CheckSchema(ctx context.Context) error {
	version, err := s.schemaVersion(ctx)
	if err != nil {
		return err
	}
	if version < sqlschema.CurrentSchemaVersion {
		return fmt.Errorf("database schema version %d is older than required version %d; run docgraph migrate", version, sqlschema.CurrentSchemaVersion)
	}
	if version > sqlschema.CurrentSchemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d; upgrade docgraph", version, sqlschema.CurrentSchemaVersion)
	}
	return nil
}

func (s *Store) recordSchemaVersion(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
insert into schema_migrations (version, applied_at)
values (?, current_timestamp)
on conflict(version) do nothing
`, sqlschema.CurrentSchemaVersion)
	return err
}

func (s *Store) schemaVersion(ctx context.Context) (int, error) {
	var version int
	err := s.db.QueryRowContext(ctx, `select coalesce(max(version), 0) from schema_migrations`).Scan(&version)
	if err != nil {
		if strings.Contains(err.Error(), "no such table: schema_migrations") {
			return 0, fmt.Errorf("database is not migrated; run docgraph migrate")
		}
		return 0, err
	}
	if version == 0 {
		return 0, fmt.Errorf("database is not migrated; run docgraph migrate")
	}
	return version, nil
}

func (s *Store) Status(ctx context.Context) (sqlschema.Status, error) {
	count := func(table string) (int64, error) {
		var n int64
		err := s.readDB().QueryRowContext(ctx, "select count(*) from "+table).Scan(&n)
		return n, err
	}

	status := sqlschema.Status{StorageDSN: s.dsn}
	var err error
	if status.Sources, err = count("sources"); err != nil {
		return status, err
	}
	if status.Documents, err = count("documents"); err != nil {
		return status, err
	}
	if status.Sections, err = count("sections"); err != nil {
		return status, err
	}
	if status.Nodes, err = count("nodes"); err != nil {
		return status, err
	}
	if status.Edges, err = count("edges"); err != nil {
		return status, err
	}
	if status.Jobs, err = count("jobs"); err != nil {
		return status, err
	}
	return status, nil
}

func (s *Store) EnsureSearchIndexes(ctx context.Context) error {
	if err := s.ensureLegacyNodeIndex(ctx); err != nil {
		return err
	}
	needsTokenBackfill, err := s.ensureSectionTokenIndexVersion(ctx)
	if err != nil {
		return err
	}
	if needsTokenBackfill {
		if err := s.backfillSectionTokenIndexes(ctx); err != nil {
			return err
		}
		if err := s.setSearchIndexVersion(ctx, "section_tokens", sectionTokenIndexVersion); err != nil {
			return err
		}
	}
	if err := s.ensureAuxiliarySearchIndexes(ctx); err != nil {
		return err
	}
	return s.ensureShortGramSearchIndexes(ctx)
}

func (s *Store) ensureLegacyNodeIndex(ctx context.Context) error {
	if version, err := s.searchIndexVersion(ctx, "nodes"); err != nil {
		return err
	} else if version == legacyNodeIndexVersion {
		return nil
	}
	nodeCount, err := s.countRows(ctx, "nodes")
	if err != nil {
		return err
	}
	ftsCount, err := s.countRows(ctx, "fts_nodes")
	if err != nil {
		return err
	}
	if ftsCount >= nodeCount {
		return s.setSearchIndexVersion(ctx, "nodes", legacyNodeIndexVersion)
	}
	if ftsCount == 0 {
		return s.backfillNodeIndexFromOffset(ctx, 0)
	}
	return s.backfillNodeIndexFromOffset(ctx, ftsCount)
}

func (s *Store) ensureSectionTokenIndexVersion(ctx context.Context) (bool, error) {
	var version string
	err := s.db.QueryRowContext(ctx, `select value from search_index_meta where key = 'section_tokens'`).Scan(&version)
	if err == nil && version == sectionTokenIndexVersion {
		return false, nil
	}
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	sectionCount, err := s.countRows(ctx, "sections")
	if err != nil {
		return false, err
	}
	tokenCount, err := s.countRows(ctx, "fts_section_tokens")
	if err != nil {
		return false, err
	}
	trigramCount, err := s.countRows(ctx, "fts_section_tokens_trigram")
	if err != nil {
		return false, err
	}
	if tokenCount >= sectionCount && trigramCount >= sectionCount {
		return false, s.setSearchIndexVersion(ctx, "section_tokens", sectionTokenIndexVersion)
	}
	if version == sectionTokenIndexRebuilding {
		return true, nil
	}
	if _, err := s.db.ExecContext(ctx, `delete from fts_section_tokens`); err != nil {
		return false, err
	}
	if _, err := s.db.ExecContext(ctx, `delete from fts_section_tokens_trigram`); err != nil {
		return false, err
	}
	if err := s.setSearchIndexVersion(ctx, "section_tokens", sectionTokenIndexRebuilding); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) searchIndexVersion(ctx context.Context, key string) (string, error) {
	var version string
	err := s.db.QueryRowContext(ctx, `select value from search_index_meta where key = ?`, key).Scan(&version)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return version, nil
}

func (s *Store) countRows(ctx context.Context, table string) (int64, error) {
	var count int64
	if err := s.db.QueryRowContext(ctx, "select count(*) from "+table).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) setSearchIndexVersion(ctx context.Context, key string, version string) error {
	_, err := s.db.ExecContext(ctx, `
insert into search_index_meta (key, value, updated_at)
values (?, ?, current_timestamp)
on conflict(key) do update set
  value = excluded.value,
  updated_at = current_timestamp
`, key, version)
	return err
}

func (s *Store) backfillNodeIndexFromOffset(ctx context.Context, offset int64) error {
	if _, err := s.db.ExecContext(ctx, `
insert into fts_nodes (kind, name, canonical_name, metadata_json, node_id)
select nodes.kind, nodes.name, nodes.canonical_name, nodes.metadata_json, nodes.id
from nodes
order by nodes.rowid
limit -1 offset ?
`, offset); err != nil {
		return err
	}
	return s.setSearchIndexVersion(ctx, "nodes", legacyNodeIndexVersion)
}

func (s *Store) backfillSectionTokenIndexes(ctx context.Context) error {
	sectionCount, err := s.countRows(ctx, "sections")
	if err != nil {
		return err
	}
	tokenCount, err := s.countRows(ctx, "fts_section_tokens")
	if err != nil {
		return err
	}
	trigramCount, err := s.countRows(ctx, "fts_section_tokens_trigram")
	if err != nil {
		return err
	}
	offset := minInt64(tokenCount, trigramCount)
	if offset >= sectionCount {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `
select s.id, s.document_id, d.title, s.heading_path, s.title, s.search_text
from sections s
join documents d on d.id = s.document_id
order by s.rowid
limit -1 offset ?
`, offset)
	if err != nil {
		return err
	}
	defer rows.Close()

	type rowData struct {
		sectionID     string
		documentID    string
		documentTitle string
		headingPath   string
		sectionTitle  string
		searchText    string
	}
	pending := make([]rowData, 0)
	for rows.Next() {
		var row rowData
		if err := rows.Scan(&row.sectionID, &row.documentID, &row.documentTitle, &row.headingPath, &row.sectionTitle, &row.searchText); err != nil {
			return err
		}
		pending = append(pending, row)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, row := range pending {
		titleTokens, headingTokens, symbolTokens, contentTokens := buildSectionIndexTexts(row.documentTitle, row.sectionTitle, row.headingPath, row.searchText)
		if _, err := s.db.ExecContext(ctx, `
insert into fts_section_tokens (title_tokens, section_heading_tokens, symbol_tokens, content_tokens, section_id, document_id)
values (?, ?, ?, ?, ?, ?)
`, titleTokens, headingTokens, symbolTokens, contentTokens, row.sectionID, row.documentID); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `
insert into fts_section_tokens_trigram (title_tokens, section_heading_tokens, symbol_tokens, content_tokens, section_id, document_id)
values (?, ?, ?, ?, ?, ?)
`, titleTokens, headingTokens, symbolTokens, contentTokens, row.sectionID, row.documentID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ensureAuxiliarySearchIndexes(ctx context.Context) error {
	version, err := s.searchIndexVersion(ctx, "auxiliary_search")
	if err != nil {
		return err
	}
	tables := []struct {
		base string
		fts  string
	}{
		{"nodes", "fts_nodes_trigram"},
		{"document_profiles", "fts_document_profiles"},
		{"document_profiles", "fts_document_profiles_trigram"},
		{"section_entities", "fts_section_entities"},
		{"section_entities", "fts_section_entities_trigram"},
	}
	complete := version == auxiliarySearchIndexVersion
	for _, table := range tables {
		baseCount, countErr := s.countRows(ctx, table.base)
		if countErr != nil {
			return countErr
		}
		ftsCount, countErr := s.countRows(ctx, table.fts)
		if countErr != nil {
			return countErr
		}
		if baseCount != ftsCount {
			complete = false
		}
	}
	if complete {
		return nil
	}
	return s.rebuildAuxiliarySearchIndexes(ctx)
}

func (s *Store) rebuildAuxiliarySearchIndexes(ctx context.Context) error {
	type profileRow struct {
		documentID string
		desc       string
		profile    string
	}
	rows, err := s.readDB().QueryContext(ctx, `select document_id, "desc", retrieval_profile_json from document_profiles order by document_id`)
	if err != nil {
		return err
	}
	profiles := make([]profileRow, 0)
	for rows.Next() {
		var row profileRow
		if err := rows.Scan(&row.documentID, &row.desc, &row.profile); err != nil {
			rows.Close()
			return err
		}
		profiles = append(profiles, row)
	}
	if err := rows.Close(); err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{
		"fts_nodes_trigram",
		"fts_document_profiles",
		"fts_document_profiles_trigram",
		"fts_section_entities",
		"fts_section_entities_trigram",
	} {
		if _, err := tx.ExecContext(ctx, "delete from "+table); err != nil {
			return fmt.Errorf("clear %s: %w", table, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
insert into fts_nodes_trigram (id, kind, name, canonical_name, metadata_json, node_id)
select id, kind, name, canonical_name, metadata_json, id from nodes
`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
insert into fts_section_entities (canonical_text, raw_text, path, operation, entity_id)
select canonical_text, raw_text, path, operation, id from section_entities
`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
insert into fts_section_entities_trigram (canonical_text, raw_text, path, operation, entity_id)
select canonical_text, raw_text, path, operation, id from section_entities
`); err != nil {
		return err
	}
	for _, profile := range profiles {
		if err := replaceDocumentProfileSearchIndex(ctx, tx, profile.documentID, profile.desc, profile.profile); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
insert into search_index_meta (key, value, updated_at)
values ('auxiliary_search', ?, current_timestamp)
on conflict(key) do update set value = excluded.value, updated_at = current_timestamp
`, auxiliarySearchIndexVersion); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit auxiliary search index rebuild: %w", err)
	}
	return nil
}

func (s *Store) ensureShortGramSearchIndexes(ctx context.Context) error {
	version, err := s.searchIndexVersion(ctx, "shortgrams")
	if err != nil {
		return err
	}
	tables := []struct {
		base string
		fts  string
	}{
		{"sections", "fts_section_shortgrams"},
		{"nodes", "fts_nodes_shortgrams"},
	}
	complete := version == shortGramSearchIndexVersion
	for _, table := range tables {
		baseCount, countErr := s.countRows(ctx, table.base)
		if countErr != nil {
			return countErr
		}
		ftsCount, countErr := s.countRows(ctx, table.fts)
		if countErr != nil {
			return countErr
		}
		if baseCount != ftsCount {
			complete = false
		}
	}
	if complete {
		return nil
	}
	return s.rebuildShortGramSearchIndexes(ctx)
}

func (s *Store) rebuildShortGramSearchIndexes(ctx context.Context) error {
	type sectionRow struct {
		sectionID     string
		documentID    string
		documentTitle string
		sectionTitle  string
		headingPath   string
		searchText    string
	}
	sectionRows, err := s.readDB().QueryContext(ctx, `
select s.id, s.document_id, d.title, s.title, s.heading_path, s.search_text
from sections s
join documents d on d.id = s.document_id
order by s.rowid
`)
	if err != nil {
		return err
	}
	sections := make([]sectionRow, 0)
	for sectionRows.Next() {
		var row sectionRow
		if err := sectionRows.Scan(&row.sectionID, &row.documentID, &row.documentTitle, &row.sectionTitle, &row.headingPath, &row.searchText); err != nil {
			sectionRows.Close()
			return err
		}
		sections = append(sections, row)
	}
	if err := sectionRows.Err(); err != nil {
		sectionRows.Close()
		return err
	}
	if err := sectionRows.Close(); err != nil {
		return err
	}

	type nodeRow struct {
		id            string
		kind          string
		name          string
		canonicalName string
	}
	nodeRows, err := s.readDB().QueryContext(ctx, `select id, kind, name, canonical_name from nodes order by rowid`)
	if err != nil {
		return err
	}
	nodes := make([]nodeRow, 0)
	for nodeRows.Next() {
		var row nodeRow
		if err := nodeRows.Scan(&row.id, &row.kind, &row.name, &row.canonicalName); err != nil {
			nodeRows.Close()
			return err
		}
		nodes = append(nodes, row)
	}
	if err := nodeRows.Err(); err != nil {
		nodeRows.Close()
		return err
	}
	if err := nodeRows.Close(); err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `delete from fts_section_shortgrams`); err != nil {
		return fmt.Errorf("clear fts_section_shortgrams: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `delete from fts_nodes_shortgrams`); err != nil {
		return fmt.Errorf("clear fts_nodes_shortgrams: %w", err)
	}
	// Rebuilding can cover tens of thousands of rows. Keep the online
	// replace-one helpers for normal writes, but batch the one-time backfill so
	// migration does not pay one SQL prepare/execute round trip per row (or two
	// per node after the table has already been cleared).
	const shortGramBackfillBatchSize = 100
	for start := 0; start < len(sections); start += shortGramBackfillBatchSize {
		end := min(start+shortGramBackfillBatchSize, len(sections))
		values := make([]string, 0, end-start)
		args := make([]any, 0, (end-start)*5)
		for _, row := range sections[start:end] {
			titleGrams, headingGrams, contentGrams := buildSectionShortGramIndexTexts(row.documentTitle, row.sectionTitle, row.headingPath, row.searchText)
			values = append(values, "(?, ?, ?, ?, ?)")
			args = append(args, titleGrams, headingGrams, contentGrams, row.sectionID, row.documentID)
		}
		if _, err := tx.ExecContext(ctx, `
insert into fts_section_shortgrams (title_grams, section_heading_grams, content_grams, section_id, document_id)
values `+strings.Join(values, ",")+`
`, args...); err != nil {
			return fmt.Errorf("backfill short-gram index for sections %d-%d: %w", start, end-1, err)
		}
	}
	for start := 0; start < len(nodes); start += shortGramBackfillBatchSize {
		end := min(start+shortGramBackfillBatchSize, len(nodes))
		values := make([]string, 0, end-start)
		args := make([]any, 0, (end-start)*5)
		for _, row := range nodes[start:end] {
			values = append(values, "(?, ?, ?, ?, ?)")
			args = append(args,
				shortGramIndexText(row.id), shortGramIndexText(row.kind),
				shortGramIndexText(row.name), shortGramIndexText(row.canonicalName), row.id,
			)
		}
		if _, err := tx.ExecContext(ctx, `
insert into fts_nodes_shortgrams (id_grams, kind_grams, name_grams, canonical_name_grams, node_id)
values `+strings.Join(values, ",")+`
`, args...); err != nil {
			return fmt.Errorf("backfill short-gram index for nodes %d-%d: %w", start, end-1, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
insert into search_index_meta (key, value, updated_at)
values ('shortgrams', ?, current_timestamp)
on conflict(key) do update set value = excluded.value, updated_at = current_timestamp
`, shortGramSearchIndexVersion); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit short-gram search index rebuild: %w", err)
	}
	return nil
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func (s *Store) CreateSource(ctx context.Context, source domain.Source) (domain.Source, error) {
	if source.ConfigJSON == "" {
		source.ConfigJSON = "{}"
	}
	source.SyncStatus, source.SyncStatusReason, source.SyncPausedAt = normalizeSourceSyncState(source.SyncStatus, source.SyncStatusReason, source.SyncPausedAt)
	_, err := s.db.ExecContext(ctx, `
insert into sources (id, kind, name, dsn, config_json, product_hint, module_hint, sync_schedule, sync_status, sync_status_reason, sync_paused_at)
values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
on conflict(id) do update set
  kind = excluded.kind,
  name = excluded.name,
  dsn = excluded.dsn,
  config_json = excluded.config_json,
  product_hint = excluded.product_hint,
  module_hint = excluded.module_hint,
  sync_schedule = excluded.sync_schedule,
  updated_at = current_timestamp
`, source.ID, source.Kind, source.Name, source.DSN, source.ConfigJSON, source.ProductHint, source.ModuleHint, source.SyncSchedule, source.SyncStatus, source.SyncStatusReason, source.SyncPausedAt)
	if err != nil {
		return domain.Source{}, err
	}
	return s.GetSource(ctx, source.ID)
}

func (s *Store) UpdateSource(ctx context.Context, source domain.Source) (domain.Source, error) {
	source.ID = strings.TrimSpace(source.ID)
	source.Kind = strings.TrimSpace(source.Kind)
	source.Name = strings.TrimSpace(source.Name)
	source.DSN = strings.TrimSpace(source.DSN)
	if source.ID == "" {
		return domain.Source{}, fmt.Errorf("source id is required")
	}
	if source.Kind == "" {
		return domain.Source{}, fmt.Errorf("source kind is required")
	}
	if source.Name == "" {
		return domain.Source{}, fmt.Errorf("source name is required")
	}
	if source.DSN == "" {
		return domain.Source{}, fmt.Errorf("source dsn is required")
	}
	if source.ConfigJSON == "" {
		source.ConfigJSON = "{}"
	}

	result, err := s.db.ExecContext(ctx, `
update sources
set kind = ?, name = ?, dsn = ?, config_json = ?, product_hint = ?, module_hint = ?, sync_schedule = ?, updated_at = current_timestamp
where id = ?
`, source.Kind, source.Name, source.DSN, source.ConfigJSON, source.ProductHint, source.ModuleHint, source.SyncSchedule, source.ID)
	if err != nil {
		return domain.Source{}, err
	}
	if n, err := result.RowsAffected(); err == nil && n == 0 {
		return domain.Source{}, sql.ErrNoRows
	}
	return s.GetSource(ctx, source.ID)
}

const effectiveSourceOwnedNodeIDsSQL = `
select n.id
from nodes n
where n.owner_source_id = ?
union
select n.id
from source_nodes sn
join nodes n on n.id = sn.node_id
where sn.source_id = ?
  and (sn.ref_mask & ?) != 0
  and n.owner_source_id is null
`

const effectiveDocumentOwnedNodeIDsSQL = `
select n.id
from document_nodes dn
join documents d on d.id = dn.document_id
join nodes n on n.id = dn.node_id
where dn.document_id = ?
  and (n.owner_source_id = d.source_id or n.owner_source_id is null)
`

// deleteNodesBySelectorTx removes node search-index rows and their base nodes
// from one shared selector. Explicit owner_source_id is authoritative; legacy
// nodes without an explicit owner retain their historical metadata membership
// lifecycle until all writers populate ownership directly.
func deleteNodesBySelectorTx(ctx context.Context, tx *sql.Tx, nodeIDsSQL string, args ...any) error {
	for _, table := range []string{"fts_nodes", "fts_nodes_trigram", "fts_nodes_shortgrams"} {
		if _, err := tx.ExecContext(ctx, "delete from "+table+" where node_id in ("+nodeIDsSQL+")", args...); err != nil {
			return fmt.Errorf("delete %s for selected nodes: %w", table, err)
		}
	}
	if _, err := tx.ExecContext(ctx, "delete from nodes where id in ("+nodeIDsSQL+")", args...); err != nil {
		return fmt.Errorf("delete selected nodes: %w", err)
	}
	return nil
}

func (s *Store) DeleteSource(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("source id is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
delete from fts_section_tokens
where document_id in (select id from documents where source_id = ?)
`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
delete from fts_section_tokens_trigram
where document_id in (select id from documents where source_id = ?)
`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
delete from fts_section_shortgrams
where document_id in (select id from documents where source_id = ?)
`, id); err != nil {
		return err
	}
	for _, table := range []string{"fts_document_profiles", "fts_document_profiles_trigram"} {
		if _, err := tx.ExecContext(ctx, "delete from "+table+" where document_id in (select id from documents where source_id = ?)", id); err != nil {
			return err
		}
	}
	for _, table := range []string{"fts_section_entities", "fts_section_entities_trigram"} {
		if _, err := tx.ExecContext(ctx, "delete from "+table+" where entity_id in (select se.id from section_entities se join documents d on d.id = se.document_id where d.source_id = ?)", id); err != nil {
			return err
		}
	}
	if err := deleteNodesBySelectorTx(ctx, tx, effectiveSourceOwnedNodeIDsSQL, id, id, nodeSourceRefMetadata); err != nil {
		return fmt.Errorf("delete graph nodes for source %s: %w", id, err)
	}
	result, err := tx.ExecContext(ctx, `delete from sources where id = ?`, id)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

func (s *Store) ListSources(ctx context.Context) ([]domain.Source, error) {
	rows, err := s.readDB().QueryContext(ctx, `
select id, kind, name, dsn, config_json, product_hint, module_hint, sync_schedule, sync_status, sync_status_reason, sync_paused_at, created_at, updated_at
from sources
order by created_at desc, id desc
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sources []domain.Source
	for rows.Next() {
		var source domain.Source
		if err := rows.Scan(&source.ID, &source.Kind, &source.Name, &source.DSN, &source.ConfigJSON, &source.ProductHint, &source.ModuleHint, &source.SyncSchedule, &source.SyncStatus, &source.SyncStatusReason, &source.SyncPausedAt, &source.CreatedAt, &source.UpdatedAt); err != nil {
			return nil, err
		}
		sources = append(sources, source)
	}
	return sources, rows.Err()
}

func (s *Store) GetSource(ctx context.Context, id string) (domain.Source, error) {
	var source domain.Source
	err := s.readDB().QueryRowContext(ctx, `
select id, kind, name, dsn, config_json, product_hint, module_hint, sync_schedule, sync_status, sync_status_reason, sync_paused_at, created_at, updated_at
from sources
where id = ?
`, id).Scan(&source.ID, &source.Kind, &source.Name, &source.DSN, &source.ConfigJSON, &source.ProductHint, &source.ModuleHint, &source.SyncSchedule, &source.SyncStatus, &source.SyncStatusReason, &source.SyncPausedAt, &source.CreatedAt, &source.UpdatedAt)
	if err != nil {
		return domain.Source{}, err
	}
	return source, nil
}

func (s *Store) CreateConfluenceCookieCredential(ctx context.Context, credential domain.ConfluenceCookieCredential) (domain.ConfluenceCookieCredential, error) {
	credential.ID = strings.TrimSpace(credential.ID)
	credential.Name = strings.TrimSpace(credential.Name)
	credential.BaseURL = strings.TrimSpace(credential.BaseURL)
	credential.Cookie = strings.TrimSpace(credential.Cookie)
	credential.Notes = strings.TrimSpace(credential.Notes)
	credential.Status = normalizeCredentialStatus(credential.Status)
	if credential.ID == "" {
		return domain.ConfluenceCookieCredential{}, fmt.Errorf("credential id is required")
	}
	if credential.Name == "" {
		return domain.ConfluenceCookieCredential{}, fmt.Errorf("credential name is required")
	}
	if credential.BaseURL == "" {
		return domain.ConfluenceCookieCredential{}, fmt.Errorf("credential base_url is required")
	}
	if credential.Cookie == "" {
		return domain.ConfluenceCookieCredential{}, fmt.Errorf("credential cookie is required")
	}
	_, err := s.db.ExecContext(ctx, `
insert into confluence_cookie_credentials (id, name, base_url, cookie, notes, status, last_error)
values (?, ?, ?, ?, ?, ?, ?)
`, credential.ID, credential.Name, credential.BaseURL, credential.Cookie, credential.Notes, credential.Status, strings.TrimSpace(credential.LastError))
	if err != nil {
		return domain.ConfluenceCookieCredential{}, err
	}
	return s.GetConfluenceCookieCredential(ctx, credential.ID)
}

func (s *Store) UpdateConfluenceCookieCredential(ctx context.Context, credential domain.ConfluenceCookieCredential) (domain.ConfluenceCookieCredential, error) {
	credential.ID = strings.TrimSpace(credential.ID)
	credential.Name = strings.TrimSpace(credential.Name)
	credential.BaseURL = strings.TrimSpace(credential.BaseURL)
	credential.Cookie = strings.TrimSpace(credential.Cookie)
	credential.Notes = strings.TrimSpace(credential.Notes)
	credential.Status = normalizeCredentialStatus(credential.Status)
	if credential.ID == "" {
		return domain.ConfluenceCookieCredential{}, fmt.Errorf("credential id is required")
	}
	if credential.Name == "" {
		return domain.ConfluenceCookieCredential{}, fmt.Errorf("credential name is required")
	}
	if credential.BaseURL == "" {
		return domain.ConfluenceCookieCredential{}, fmt.Errorf("credential base_url is required")
	}
	if credential.Cookie == "" {
		return domain.ConfluenceCookieCredential{}, fmt.Errorf("credential cookie is required")
	}
	result, err := s.db.ExecContext(ctx, `
update confluence_cookie_credentials
set name = ?, base_url = ?, cookie = ?, notes = ?, status = ?, last_error = ?, updated_at = current_timestamp
where id = ?
`, credential.Name, credential.BaseURL, credential.Cookie, credential.Notes, credential.Status, strings.TrimSpace(credential.LastError), credential.ID)
	if err != nil {
		return domain.ConfluenceCookieCredential{}, err
	}
	if n, err := result.RowsAffected(); err == nil && n == 0 {
		return domain.ConfluenceCookieCredential{}, sql.ErrNoRows
	}
	return s.GetConfluenceCookieCredential(ctx, credential.ID)
}

func (s *Store) UpdateConfluenceCookieCredentialValidation(ctx context.Context, id string, status string, errText string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("credential id is required")
	}
	result, err := s.db.ExecContext(ctx, `
update confluence_cookie_credentials
set status = ?, last_validated_at = current_timestamp, last_error = ?, updated_at = current_timestamp
where id = ?
`, normalizeCredentialStatus(status), strings.TrimSpace(errText), id)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) DeleteConfluenceCookieCredential(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("credential id is required")
	}
	result, err := s.db.ExecContext(ctx, `delete from confluence_cookie_credentials where id = ?`, id)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) ListConfluenceCookieCredentials(ctx context.Context) ([]domain.ConfluenceCookieCredential, error) {
	rows, err := s.readDB().QueryContext(ctx, `
select id, name, base_url, cookie, notes, status, last_validated_at, last_error, created_at, updated_at
from confluence_cookie_credentials
order by updated_at desc, created_at desc, id desc
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var credentials []domain.ConfluenceCookieCredential
	for rows.Next() {
		var credential domain.ConfluenceCookieCredential
		if err := rows.Scan(&credential.ID, &credential.Name, &credential.BaseURL, &credential.Cookie, &credential.Notes, &credential.Status, &credential.LastValidatedAt, &credential.LastError, &credential.CreatedAt, &credential.UpdatedAt); err != nil {
			return nil, err
		}
		credentials = append(credentials, credential)
	}
	return credentials, rows.Err()
}

func (s *Store) GetConfluenceCookieCredential(ctx context.Context, id string) (domain.ConfluenceCookieCredential, error) {
	var credential domain.ConfluenceCookieCredential
	err := s.readDB().QueryRowContext(ctx, `
select id, name, base_url, cookie, notes, status, last_validated_at, last_error, created_at, updated_at
from confluence_cookie_credentials
where id = ?
`, strings.TrimSpace(id)).Scan(&credential.ID, &credential.Name, &credential.BaseURL, &credential.Cookie, &credential.Notes, &credential.Status, &credential.LastValidatedAt, &credential.LastError, &credential.CreatedAt, &credential.UpdatedAt)
	if err != nil {
		return domain.ConfluenceCookieCredential{}, err
	}
	return credential, nil
}

func normalizeCredentialStatus(status string) string {
	switch strings.TrimSpace(status) {
	case "valid", "invalid":
		return strings.TrimSpace(status)
	default:
		return "unknown"
	}
}

func (s *Store) UpdateSourceSyncState(ctx context.Context, id string, status string, reason string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("source id is required")
	}
	status, reason, pausedAt := normalizeSourceSyncState(status, reason, "")
	if status == "paused" && pausedAt == "" {
		result, err := s.db.ExecContext(ctx, `
update sources
set sync_status = ?, sync_status_reason = ?, sync_paused_at = current_timestamp, updated_at = current_timestamp
where id = ?
`, status, reason, id)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err == nil && n == 0 {
			return sql.ErrNoRows
		}
		return nil
	}
	result, err := s.db.ExecContext(ctx, `
update sources
set sync_status = ?, sync_status_reason = ?, sync_paused_at = ?, updated_at = current_timestamp
where id = ?
`, status, reason, pausedAt, id)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) ListSourceArtifacts(ctx context.Context, sourceID string, limit, offset int) (domain.SourceArtifacts, error) {
	sourceID, limit, offset, err := normalizeSourceArtifactPage(sourceID, limit, offset)
	if err != nil {
		return domain.SourceArtifacts{}, err
	}
	counts, err := s.GetSourceArtifactCounts(ctx, sourceID)
	if err != nil {
		return domain.SourceArtifacts{}, err
	}
	docs, err := s.ListSourceDocuments(ctx, sourceID, limit, offset)
	if err != nil {
		return domain.SourceArtifacts{}, err
	}
	sections, err := s.ListSourceSections(ctx, sourceID, limit, offset)
	if err != nil {
		return domain.SourceArtifacts{}, err
	}
	sectionEntities, err := s.ListSourceSectionEntities(ctx, sourceID, limit, offset)
	if err != nil {
		return domain.SourceArtifacts{}, err
	}
	nodes, err := s.ListSourceNodes(ctx, sourceID, limit, offset)
	if err != nil {
		return domain.SourceArtifacts{}, err
	}
	edges, err := s.ListSourceEdges(ctx, sourceID, limit, offset)
	if err != nil {
		return domain.SourceArtifacts{}, err
	}
	entityDiagnostics, err := s.sourceEntityDiagnostics(ctx, sourceID, 20)
	if err != nil {
		return domain.SourceArtifacts{}, err
	}
	var activeSnapshot *domain.SourceSnapshot
	if snapshot, snapshotErr := s.GetActiveSourceSnapshot(ctx, sourceID); snapshotErr == nil {
		activeSnapshot = &snapshot
	} else if !errors.Is(snapshotErr, sql.ErrNoRows) {
		return domain.SourceArtifacts{}, snapshotErr
	}
	featureInventory, err := s.ListSourceFeatureInventory(ctx, sourceID)
	if err != nil {
		return domain.SourceArtifacts{}, err
	}
	return domain.SourceArtifacts{
		SourceID:          sourceID,
		Counts:            counts,
		EntityDiagnostics: entityDiagnostics,
		Documents:         docs,
		Sections:          sections,
		SectionEntities:   sectionEntities,
		Nodes:             nodes,
		Edges:             edges,
		ActiveSnapshot:    activeSnapshot,
		FeatureInventory:  featureInventory,
	}, nil
}

func (s *Store) GetSourceArtifactCounts(ctx context.Context, sourceID string) (domain.SourceArtifactCounts, error) {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return domain.SourceArtifactCounts{}, fmt.Errorf("source id is required")
	}
	if _, err := s.GetSource(ctx, sourceID); err != nil {
		return domain.SourceArtifactCounts{}, err
	}
	var counts domain.SourceArtifactCounts
	if err := s.readDB().QueryRowContext(ctx, `select count(*) from documents where source_id = ?`, sourceID).Scan(&counts.Documents); err != nil {
		return counts, err
	}
	if err := s.readDB().QueryRowContext(ctx, `
select count(*)
from sections
join documents on documents.id = sections.document_id
where documents.source_id = ?
`, sourceID).Scan(&counts.Sections); err != nil {
		return counts, err
	}
	if err := s.readDB().QueryRowContext(ctx, `select count(*) from source_nodes where source_id = ?`, sourceID).Scan(&counts.Nodes); err != nil {
		return counts, err
	}
	if err := s.readDB().QueryRowContext(ctx, `select count(*) from source_edges where source_id = ?`, sourceID).Scan(&counts.Edges); err != nil {
		return counts, err
	}
	if err := s.readDB().QueryRowContext(ctx, `
select count(*)
from section_entities se
join documents d on d.id = se.document_id
where d.source_id = ?
`, sourceID).Scan(&counts.SectionEntities); err != nil {
		return counts, err
	}
	if err := s.readDB().QueryRowContext(ctx, `select count(*) from media_assets where source_id = ?`, sourceID).Scan(&counts.MediaAssets); err != nil {
		return counts, err
	}
	if err := s.readDB().QueryRowContext(ctx, `select count(*) from source_snapshots where source_id = ?`, sourceID).Scan(&counts.Snapshots); err != nil {
		return counts, err
	}
	if err := s.readDB().QueryRowContext(ctx, `select count(*) from media_assets where source_id = ? and status = 'missing'`, sourceID).Scan(&counts.MissingMediaAssets); err != nil {
		return counts, err
	}
	if err := s.readDB().QueryRowContext(ctx, `select count(*) from media_assets where source_id = ? and status = 'rejected'`, sourceID).Scan(&counts.RejectedMediaAssets); err != nil {
		return counts, err
	}
	if err := s.readDB().QueryRowContext(ctx, `
select count(*) from media_assets where source_id = ? and (blob_sha256 is null or status in ('missing', 'rejected', 'external'))
`, sourceID).Scan(&counts.UnavailableMediaAssets); err != nil {
		return counts, err
	}
	if err := s.readDB().QueryRowContext(ctx, `
select coalesce(sum(mb.size_bytes), 0) from source_snapshots ss join media_blobs mb on mb.sha256 = ss.blob_sha256
where ss.source_id = ? and ss.active = 1
`, sourceID).Scan(&counts.SnapshotBytes); err != nil {
		return counts, err
	}
	if err := s.readDB().QueryRowContext(ctx, `select coalesce(sum(size_bytes), 0) from media_assets where source_id = ?`, sourceID).Scan(&counts.ReferencedMediaBytes); err != nil {
		return counts, err
	}
	return counts, nil
}

func (s *Store) GetSourceHealth(ctx context.Context, sourceID string) (domain.SourceHealth, error) {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return domain.SourceHealth{}, fmt.Errorf("source id is required")
	}
	if _, err := s.GetSource(ctx, sourceID); err != nil {
		return domain.SourceHealth{}, err
	}
	counts, err := s.GetSourceArtifactCounts(ctx, sourceID)
	if err != nil {
		return domain.SourceHealth{}, err
	}
	entityDiagnostics, err := s.sourceEntityDiagnostics(ctx, sourceID, 20)
	if err != nil {
		return domain.SourceHealth{}, err
	}
	zeroDocs, err := s.listZeroSectionDocuments(ctx, sourceID, 50)
	if err != nil {
		return domain.SourceHealth{}, err
	}
	lowSections, err := s.listLowContentSections(ctx, sourceID, 50)
	if err != nil {
		return domain.SourceHealth{}, err
	}
	staleFeedback, err := s.listSourceDocumentFeedback(ctx, sourceID, "document_stale", 100)
	if err != nil {
		return domain.SourceHealth{}, err
	}
	jobs, err := s.ListJobs(ctx, domain.JobListOptions{SourceID: sourceID, Kind: "sync_source", Limit: 1})
	if err != nil {
		return domain.SourceHealth{}, err
	}
	var latestJob domain.Job
	var brokenLinks []domain.BrokenLink
	if len(jobs) > 0 {
		latestJob = jobs[0]
		brokenLinks = brokenLinksFromJob(latestJob)
	}
	warnings := sourceHealthWarnings(counts, entityDiagnostics, latestJob, brokenLinks, zeroDocs, lowSections, staleFeedback)
	var activeSnapshot *domain.SourceSnapshot
	if snapshot, snapshotErr := s.GetActiveSourceSnapshot(ctx, sourceID); snapshotErr == nil {
		activeSnapshot = &snapshot
	} else if !errors.Is(snapshotErr, sql.ErrNoRows) {
		return domain.SourceHealth{}, snapshotErr
	}
	featureInventory, err := s.ListSourceFeatureInventory(ctx, sourceID)
	if err != nil {
		return domain.SourceHealth{}, err
	}
	unsupportedFeatures := 0
	rejectedFeatures := 0
	for _, entry := range featureInventory {
		switch entry.CoverageStatus {
		case "unsupported":
			unsupportedFeatures += entry.Count
		case "rejected":
			rejectedFeatures += entry.Count
		}
	}
	if counts.UnavailableMediaAssets > 0 {
		warnings = append(warnings, domain.SourceHealthWarning{Kind: "unavailable_media_assets", Severity: "warn", Message: "Some source media is missing, external, or rejected.", Count: int(counts.UnavailableMediaAssets)})
	}
	if rejectedFeatures > 0 {
		warnings = append(warnings, domain.SourceHealthWarning{Kind: "rejected_source_features", Severity: "warn", Message: "Some source features were rejected with diagnostics.", Count: rejectedFeatures})
	}
	if unsupportedFeatures > 0 {
		warnings = append(warnings, domain.SourceHealthWarning{Kind: "unsupported_source_features", Severity: "warn", Message: "Some source features are preserved but unsupported.", Count: unsupportedFeatures})
	}
	return domain.SourceHealth{
		SourceID:             sourceID,
		Counts:               counts,
		EntityDiagnostics:    entityDiagnostics,
		LatestJob:            latestJob,
		BrokenLinks:          brokenLinks,
		ZeroSectionDocuments: zeroDocs,
		LowContentSections:   lowSections,
		StaleFeedback:        staleFeedback,
		Warnings:             warnings,
		ActiveSnapshot:       activeSnapshot,
		FeatureInventory:     featureInventory,
	}, nil
}

func (s *Store) listZeroSectionDocuments(ctx context.Context, sourceID string, limit int) ([]domain.DocumentSummary, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.readDB().QueryContext(ctx, `
select documents.id, documents.source_id, documents.external_id, documents.title, documents.url,
       documents.content_hash, documents.indexed_at, count(sections.id)
from documents
left join sections on sections.document_id = documents.id
where documents.source_id = ?
group by documents.id
having count(sections.id) = 0
order by documents.indexed_at desc, documents.title asc
limit ?
`, sourceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var docs []domain.DocumentSummary
	for rows.Next() {
		var doc domain.DocumentSummary
		if err := rows.Scan(&doc.ID, &doc.SourceID, &doc.ExternalID, &doc.Title, &doc.URL, &doc.ContentHash, &doc.IndexedAt, &doc.SectionCount); err != nil {
			return nil, err
		}
		doc.NodeID = stableDocumentNodeID(doc.ID)
		docs = append(docs, doc)
	}
	return docs, rows.Err()
}

func (s *Store) listLowContentSections(ctx context.Context, sourceID string, limit int) ([]domain.SectionSummary, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.readDB().QueryContext(ctx, `
select sections.id, sections.document_id, documents.title, sections.title, sections.heading_path,
       substr(replace(replace(sections.content, char(10), ' '), char(13), ' '), 1, 220),
       sections.content_hash, sections.ordinal
from sections
join documents on documents.id = sections.document_id
where documents.source_id = ?
  and length(trim(sections.content)) < 80
order by length(trim(sections.content)) asc, documents.title asc, sections.ordinal asc
limit ?
`, sourceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sections []domain.SectionSummary
	for rows.Next() {
		var section domain.SectionSummary
		if err := rows.Scan(&section.ID, &section.DocumentID, &section.DocumentTitle, &section.Title, &section.HeadingPath, &section.ContentSnippet, &section.ContentHash, &section.Ordinal); err != nil {
			return nil, err
		}
		section.NodeID = stableSectionNodeID(section.ID)
		sections = append(sections, section)
	}
	return sections, rows.Err()
}

func (s *Store) listSourceDocumentFeedback(ctx context.Context, sourceID string, feedbackKind string, limit int) ([]domain.FeedbackEvent, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.readDB().QueryContext(ctx, `
select fe.id, fe.target_kind, fe.target_id, fe.feedback_kind, fe.payload_json, fe.actor, fe.created_at
from feedback_events fe
join documents d on d.id = fe.target_id
where fe.target_kind = 'document'
  and d.source_id = ?
  and (? = '' or fe.feedback_kind = ?)
order by fe.rowid desc
limit ?
`, sourceID, feedbackKind, feedbackKind, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	events := make([]domain.FeedbackEvent, 0)
	for rows.Next() {
		var event domain.FeedbackEvent
		if err := rows.Scan(&event.ID, &event.TargetKind, &event.TargetID, &event.FeedbackKind, &event.PayloadJSON, &event.Actor, &event.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func brokenLinksFromJob(job domain.Job) []domain.BrokenLink {
	raw := strings.TrimSpace(job.ResultJSON)
	if raw == "" || raw == "{}" {
		raw = strings.TrimSpace(job.PayloadJSON)
	}
	var payload domain.ResultPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil
	}
	return payload.BrokenLinks
}

func sourceHealthWarnings(counts domain.SourceArtifactCounts, entityDiagnostics domain.EntityDiagnostics, latestJob domain.Job, brokenLinks []domain.BrokenLink, zeroDocs []domain.DocumentSummary, lowSections []domain.SectionSummary, staleFeedback []domain.FeedbackEvent) []domain.SourceHealthWarning {
	warnings := make([]domain.SourceHealthWarning, 0)
	if latestJob.ID == "" {
		warnings = append(warnings, domain.SourceHealthWarning{Kind: "no_sync_job", Severity: "warn", Message: "Source has no sync job history."})
	} else if latestJob.Status == "failed" {
		warnings = append(warnings, domain.SourceHealthWarning{Kind: "latest_sync_failed", Severity: "error", Message: latestJob.LastError, Count: 1})
	}
	if counts.Documents == 0 {
		warnings = append(warnings, domain.SourceHealthWarning{Kind: "no_documents", Severity: "error", Message: "No documents are indexed for this source."})
	}
	if counts.Sections > 0 && counts.SectionEntities == 0 {
		warnings = append(warnings, domain.SourceHealthWarning{Kind: "no_section_entities", Severity: "info", Message: "No technical entities have been extracted for this source.", Count: int(counts.Sections)})
	}
	if len(brokenLinks) > 0 {
		warnings = append(warnings, domain.SourceHealthWarning{Kind: "broken_links", Severity: "warn", Message: "Latest sync reported broken links.", Count: len(brokenLinks)})
	}
	if len(zeroDocs) > 0 {
		warnings = append(warnings, domain.SourceHealthWarning{Kind: "zero_section_documents", Severity: "warn", Message: "Some documents have no parsed sections.", Count: len(zeroDocs)})
	}
	if len(lowSections) > 0 {
		warnings = append(warnings, domain.SourceHealthWarning{Kind: "low_content_sections", Severity: "warn", Message: "Some parsed sections contain very little text.", Count: len(lowSections)})
	}
	if len(staleFeedback) > 0 {
		warnings = append(warnings, domain.SourceHealthWarning{Kind: "stale_documents", Severity: "info", Message: "Some documents are manually marked stale.", Count: len(staleFeedback)})
	}
	if entityDiagnostics.DocumentsPathOnly > 0 {
		warnings = append(warnings, domain.SourceHealthWarning{Kind: "path_literals_without_api", Severity: "warn", Message: "Some documents contain path literals but no API endpoints.", Count: entityDiagnostics.DocumentsPathOnly})
	}
	return warnings
}

func (s *Store) sourceEntityDiagnostics(ctx context.Context, sourceID string, limit int) (domain.EntityDiagnostics, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	var diagnostics domain.EntityDiagnostics
	err := s.readDB().QueryRowContext(ctx, `
select
  count(*),
  coalesce(sum(case when kind = 'api_endpoint' then 1 else 0 end), 0),
  coalesce(sum(case when kind = 'path_literal' then 1 else 0 end), 0),
  coalesce(sum(case when kind = 'operation_candidate' then 1 else 0 end), 0),
  count(distinct document_id),
  count(distinct case when kind = 'api_endpoint' then document_id end),
  count(distinct section_id)
from section_entities
where document_id in (select id from documents where source_id = ?)
`, sourceID).Scan(&diagnostics.Total, &diagnostics.APIEndpoints, &diagnostics.PathLiterals, &diagnostics.Operations, &diagnostics.DocumentsWithAny, &diagnostics.DocumentsWithAPI, &diagnostics.SectionsWithAny)
	if err != nil {
		return domain.EntityDiagnostics{}, err
	}
	err = s.readDB().QueryRowContext(ctx, `
select count(*)
from (
  select document_id
  from section_entities
  where document_id in (select id from documents where source_id = ?)
  group by document_id
  having sum(case when kind = 'path_literal' then 1 else 0 end) > 0
     and sum(case when kind = 'api_endpoint' then 1 else 0 end) = 0
)
`, sourceID).Scan(&diagnostics.DocumentsPathOnly)
	if err != nil {
		return domain.EntityDiagnostics{}, err
	}
	docs, err := s.listPathOnlyEntityDocuments(ctx, sourceID, limit)
	if err != nil {
		return domain.EntityDiagnostics{}, err
	}
	diagnostics.TopPathOnlyDocs = docs
	return diagnostics, nil
}

func (s *Store) listPathOnlyEntityDocuments(ctx context.Context, sourceID string, limit int) ([]domain.DocumentSummary, error) {
	rows, err := s.readDB().QueryContext(ctx, `
select d.id, d.source_id, d.external_id, d.title, d.url,
       d.content_hash, d.indexed_at, count(distinct sections.id) as section_count
from documents d
join section_entities se on se.document_id = d.id
left join sections on sections.document_id = d.id
where d.source_id = ?
group by d.id
having sum(case when se.kind = 'path_literal' then 1 else 0 end) > 0
   and sum(case when se.kind = 'api_endpoint' then 1 else 0 end) = 0
order by sum(case when se.kind = 'path_literal' then 1 else 0 end) desc, d.indexed_at desc, d.title asc
limit ?
`, sourceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	docs := make([]domain.DocumentSummary, 0)
	for rows.Next() {
		var doc domain.DocumentSummary
		if err := rows.Scan(&doc.ID, &doc.SourceID, &doc.ExternalID, &doc.Title, &doc.URL, &doc.ContentHash, &doc.IndexedAt, &doc.SectionCount); err != nil {
			return nil, err
		}
		doc.NodeID = stableDocumentNodeID(doc.ID)
		docs = append(docs, doc)
	}
	return docs, rows.Err()
}

func (s *Store) ListSourceDocuments(ctx context.Context, sourceID string, limit, offset int) ([]domain.DocumentSummary, error) {
	sourceID, limit, offset, err := normalizeSourceArtifactPage(sourceID, limit, offset)
	if err != nil {
		return nil, err
	}
	if _, err := s.GetSource(ctx, sourceID); err != nil {
		return nil, err
	}
	rows, err := s.readDB().QueryContext(ctx, `
select documents.id, documents.source_id, documents.external_id, documents.title, documents.url,
       documents.content_hash, documents.indexed_at, count(sections.id)
from documents
left join sections on sections.document_id = documents.id
where documents.source_id = ?
group by documents.id
order by documents.indexed_at desc, documents.title asc
limit ? offset ?
`, sourceID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var docs []domain.DocumentSummary
	for rows.Next() {
		var doc domain.DocumentSummary
		if err := rows.Scan(&doc.ID, &doc.SourceID, &doc.ExternalID, &doc.Title, &doc.URL, &doc.ContentHash, &doc.IndexedAt, &doc.SectionCount); err != nil {
			return nil, err
		}
		doc.NodeID = stableDocumentNodeID(doc.ID)
		docs = append(docs, doc)
	}
	return docs, rows.Err()
}

func (s *Store) GetDocument(ctx context.Context, id string) (domain.DocumentSummary, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return domain.DocumentSummary{}, fmt.Errorf("document id is required")
	}
	var doc domain.DocumentSummary
	err := s.readDB().QueryRowContext(ctx, `
select documents.id, documents.source_id, documents.external_id, documents.title, documents.url,
       documents.content_hash, documents.indexed_at, count(sections.id)
from documents
left join sections on sections.document_id = documents.id
where documents.id = ?
group by documents.id
`, id).Scan(&doc.ID, &doc.SourceID, &doc.ExternalID, &doc.Title, &doc.URL, &doc.ContentHash, &doc.IndexedAt, &doc.SectionCount)
	if err != nil {
		return domain.DocumentSummary{}, err
	}
	doc.NodeID = stableDocumentNodeID(doc.ID)
	return doc, nil
}

func (s *Store) GetDocumentBySourceExternalID(ctx context.Context, sourceID string, externalID string) (domain.DocumentSummary, error) {
	sourceID = strings.TrimSpace(sourceID)
	externalID = strings.TrimSpace(externalID)
	if sourceID == "" {
		return domain.DocumentSummary{}, fmt.Errorf("source id is required")
	}
	if externalID == "" {
		return domain.DocumentSummary{}, fmt.Errorf("external id is required")
	}

	var doc domain.DocumentSummary
	err := s.readDB().QueryRowContext(ctx, `
select documents.id, documents.source_id, documents.external_id, documents.title, documents.url,
       documents.content_hash, documents.indexed_at, count(sections.id)
from documents
left join sections on sections.document_id = documents.id
where documents.source_id = ? and documents.external_id = ?
group by documents.id
`, sourceID, externalID).Scan(&doc.ID, &doc.SourceID, &doc.ExternalID, &doc.Title, &doc.URL, &doc.ContentHash, &doc.IndexedAt, &doc.SectionCount)
	if err != nil {
		return domain.DocumentSummary{}, err
	}
	doc.NodeID = stableDocumentNodeID(doc.ID)
	return doc, nil
}

func (s *Store) ListDocumentSections(ctx context.Context, documentID string, limit, offset int) ([]domain.SectionSummary, error) {
	documentID = strings.TrimSpace(documentID)
	if documentID == "" {
		return nil, fmt.Errorf("document id is required")
	}
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	if offset < 0 {
		return nil, fmt.Errorf("offset must be non-negative")
	}
	if _, err := s.GetDocument(ctx, documentID); err != nil {
		return nil, err
	}
	rows, err := s.readDB().QueryContext(ctx, `
select s.id, s.document_id, d.title, s.title, s.heading_path,
       substr(replace(replace(s.content, char(10), ' '), char(13), ' '), 1, 800),
       s.content_hash, s.ordinal,
       coalesce((select sn.node_id from section_nodes sn join nodes n on n.id = sn.node_id
                 where sn.section_id = s.id and n.kind = 'DocSection'
                 order by case when sn.role = 'represents' then 0 else 1 end, sn.node_id limit 1), ''),
       coalesce(ss.parent_section_id, ''), coalesce(ss.source_element_id, ''), coalesce(ss.element_kind, ''),
       coalesce(ss.depth, 0), coalesce(ss.sibling_ordinal, s.ordinal), coalesce(ss.order_path_json, '[]'),
       coalesce(ss.display_number, ''), coalesce(ss.presentation_json, '{}'), s.metadata_json,
       (select count(*) from section_media_refs smr where smr.section_id = s.id)
from sections s
join documents d on d.id = s.document_id
left join section_structures ss on ss.section_id = s.id
where s.document_id = ?
order by s.ordinal asc
limit ? offset ?
`, documentID, limit, offset)
	if err != nil {
		return nil, err
	}
	return scanSectionSummaryRows(rows)
}

func (s *Store) ListSourceSections(ctx context.Context, sourceID string, limit, offset int) ([]domain.SectionSummary, error) {
	sourceID, limit, offset, err := normalizeSourceArtifactPage(sourceID, limit, offset)
	if err != nil {
		return nil, err
	}
	if _, err := s.GetSource(ctx, sourceID); err != nil {
		return nil, err
	}
	rows, err := s.readDB().QueryContext(ctx, `
select s.id, s.document_id, d.title, s.title, s.heading_path,
       substr(replace(replace(s.content, char(10), ' '), char(13), ' '), 1, 220),
       s.content_hash, s.ordinal,
       coalesce((select sn.node_id from section_nodes sn join nodes n on n.id = sn.node_id
                 where sn.section_id = s.id and n.kind = 'DocSection'
                 order by case when sn.role = 'represents' then 0 else 1 end, sn.node_id limit 1), ''),
       coalesce(ss.parent_section_id, ''), coalesce(ss.source_element_id, ''), coalesce(ss.element_kind, ''),
       coalesce(ss.depth, 0), coalesce(ss.sibling_ordinal, s.ordinal), coalesce(ss.order_path_json, '[]'),
       coalesce(ss.display_number, ''), coalesce(ss.presentation_json, '{}'), s.metadata_json,
       (select count(*) from section_media_refs smr where smr.section_id = s.id)
from sections s
join documents d on d.id = s.document_id
left join section_structures ss on ss.section_id = s.id
where d.source_id = ?
order by d.title asc, s.ordinal asc
limit ? offset ?
`, sourceID, limit, offset)
	if err != nil {
		return nil, err
	}
	return scanSectionSummaryRows(rows)
}

func (s *Store) ListSourceNodes(ctx context.Context, sourceID string, limit, offset int) ([]domain.Node, error) {
	sourceID, limit, offset, err := normalizeSourceArtifactPage(sourceID, limit, offset)
	if err != nil {
		return nil, err
	}
	if _, err := s.GetSource(ctx, sourceID); err != nil {
		return nil, err
	}
	rows, err := s.readDB().QueryContext(ctx, `
select n.id, n.kind, n.name, n.canonical_name, n.metadata_json, n.confidence,
       coalesce(n.owner_source_id, ''), n.owner_scope, n.created_at, n.updated_at
from source_nodes sn
join nodes n on n.id = sn.node_id
where sn.source_id = ?
order by n.kind asc, n.name asc, n.id asc
limit ? offset ?
`, sourceID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	nodes := make([]domain.Node, 0)
	for rows.Next() {
		var node domain.Node
		if err := rows.Scan(&node.ID, &node.Kind, &node.Name, &node.CanonicalName, &node.MetadataJSON, &node.Confidence, &node.OwnerSourceID, &node.OwnerScope, &node.CreatedAt, &node.UpdatedAt); err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}

func (s *Store) ListSourceEdges(ctx context.Context, sourceID string, limit, offset int) ([]domain.EdgeSummary, error) {
	sourceID, limit, offset, err := normalizeSourceArtifactPage(sourceID, limit, offset)
	if err != nil {
		return nil, err
	}
	if _, err := s.GetSource(ctx, sourceID); err != nil {
		return nil, err
	}
	rows, err := s.readDB().QueryContext(ctx, `
select e.id, e.src_id, src.name, src.kind, e.dst_id, dst.name, dst.kind,
       e.kind, e.confidence, e.provenance, coalesce(e.evidence_section_id, '')
from source_edges se
join edges e on e.id = se.edge_id
join nodes src on src.id = e.src_id
join nodes dst on dst.id = e.dst_id
where se.source_id = ?
order by e.kind asc, src.name asc, dst.name asc, e.id asc
limit ? offset ?
`, sourceID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	edges := make([]domain.EdgeSummary, 0)
	for rows.Next() {
		var edge domain.EdgeSummary
		if err := rows.Scan(&edge.ID, &edge.SrcID, &edge.SrcName, &edge.SrcKind, &edge.DstID, &edge.DstName, &edge.DstKind, &edge.Kind, &edge.Confidence, &edge.Provenance, &edge.EvidenceSectionID); err != nil {
			return nil, err
		}
		edges = append(edges, edge)
	}
	return edges, rows.Err()
}

func normalizeSourceArtifactPage(sourceID string, limit, offset int) (string, int, int, error) {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return "", 0, 0, fmt.Errorf("source id is required")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	return sourceID, limit, offset, nil
}

func (s *Store) ReplaceDocument(ctx context.Context, doc domain.DocumentInput, sections []domain.SectionInput) error {
	var err error
	if doc.MetadataJSON, err = normalizeJSONObject(doc.MetadataJSON, "document metadata_json"); err != nil {
		return err
	}
	sectionIDs := make(map[string]bool, len(sections))
	for i := range sections {
		section := &sections[i]
		if strings.TrimSpace(section.ID) == "" {
			return fmt.Errorf("section id is required")
		}
		if sectionIDs[section.ID] {
			return fmt.Errorf("duplicate section id %q", section.ID)
		}
		sectionIDs[section.ID] = true
		if section.SearchText == "" {
			section.SearchText = section.Content
		}
		if section.MetadataJSON, err = normalizeJSONObject(section.MetadataJSON, "section "+section.ID+" metadata_json"); err != nil {
			return err
		}
		if section.Structure != nil {
			if section.Structure.PresentationJSON, err = normalizeJSONObject(section.Structure.PresentationJSON, "section "+section.ID+" presentation_json"); err != nil {
				return err
			}
		}
	}
	for _, section := range sections {
		if section.Structure != nil && section.Structure.ParentSectionID != "" && !sectionIDs[section.Structure.ParentSectionID] {
			return fmt.Errorf("section %s parent %s is not in document replacement", section.ID, section.Structure.ParentSectionID)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
insert into documents (id, source_id, external_id, title, url, version, content_hash, metadata_json, indexed_at)
values (?, ?, ?, ?, ?, ?, ?, ?, current_timestamp)
on conflict(id) do update set
  source_id = excluded.source_id,
  external_id = excluded.external_id,
  title = excluded.title,
  url = excluded.url,
  version = excluded.version,
  content_hash = excluded.content_hash,
  metadata_json = excluded.metadata_json,
  updated_at = current_timestamp,
  indexed_at = current_timestamp
`, doc.ID, doc.SourceID, doc.ExternalID, doc.Title, doc.URL, doc.Version, doc.ContentHash, doc.MetadataJSON)
	if err != nil {
		return fmt.Errorf("upsert document %s: %w", doc.ID, err)
	}

	if _, err := tx.ExecContext(ctx, `delete from fts_section_tokens where document_id = ?`, doc.ID); err != nil {
		return fmt.Errorf("delete fts_section_tokens for document %s: %w", doc.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `delete from fts_section_tokens_trigram where document_id = ?`, doc.ID); err != nil {
		return fmt.Errorf("delete fts_section_tokens_trigram for document %s: %w", doc.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `delete from fts_section_shortgrams where document_id = ?`, doc.ID); err != nil {
		return fmt.Errorf("delete fts_section_shortgrams for document %s: %w", doc.ID, err)
	}
	for _, table := range []string{"fts_section_entities", "fts_section_entities_trigram"} {
		if _, err := tx.ExecContext(ctx, "delete from "+table+" where entity_id in (select id from section_entities where document_id = ?)", doc.ID); err != nil {
			return fmt.Errorf("delete %s for document %s: %w", table, doc.ID, err)
		}
	}
	evidenceEdgeIDs, err := evidenceEdgeIDsForDocumentTx(ctx, tx, doc.ID)
	if err != nil {
		return fmt.Errorf("list evidence edges for document %s: %w", doc.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `delete from sections where document_id = ?`, doc.ID); err != nil {
		return fmt.Errorf("delete sections for document %s: %w", doc.ID, err)
	}

	for _, section := range sections {
		_, err = tx.ExecContext(ctx, `
insert into sections (id, document_id, heading_path, title, content, search_text, content_hash, ordinal, metadata_json)
values (?, ?, ?, ?, ?, ?, ?, ?, ?)
`, section.ID, doc.ID, section.HeadingPath, section.Title, section.Content, section.SearchText, section.ContentHash, section.Ordinal, section.MetadataJSON)
		if err != nil {
			return fmt.Errorf("insert section %s for document %s: %w", section.ID, doc.ID, err)
		}
		titleTokens, headingTokens, symbolTokens, contentTokens := buildSectionIndexTexts(doc.Title, section.Title, section.HeadingPath, section.SearchText)
		_, err = tx.ExecContext(ctx, `
insert into fts_section_tokens (title_tokens, section_heading_tokens, symbol_tokens, content_tokens, section_id, document_id)
values (?, ?, ?, ?, ?, ?)
`, titleTokens, headingTokens, symbolTokens, contentTokens, section.ID, doc.ID)
		if err != nil {
			return fmt.Errorf("insert fts_section_tokens for section %s: %w", section.ID, err)
		}
		_, err = tx.ExecContext(ctx, `
insert into fts_section_tokens_trigram (title_tokens, section_heading_tokens, symbol_tokens, content_tokens, section_id, document_id)
values (?, ?, ?, ?, ?, ?)
`, titleTokens, headingTokens, symbolTokens, contentTokens, section.ID, doc.ID)
		if err != nil {
			return fmt.Errorf("insert fts_section_tokens_trigram for section %s: %w", section.ID, err)
		}
		titleGrams, headingGrams, contentGrams := buildSectionShortGramIndexTexts(doc.Title, section.Title, section.HeadingPath, section.SearchText)
		_, err = tx.ExecContext(ctx, `
insert into fts_section_shortgrams (title_grams, section_heading_grams, content_grams, section_id, document_id)
values (?, ?, ?, ?, ?)
`, titleGrams, headingGrams, contentGrams, section.ID, doc.ID)
		if err != nil {
			return fmt.Errorf("insert fts_section_shortgrams for section %s: %w", section.ID, err)
		}
	}
	for _, section := range sections {
		if section.Structure == nil {
			continue
		}
		orderPathJSON, err := json.Marshal(section.Structure.OrderPath)
		if err != nil {
			return fmt.Errorf("marshal order path for section %s: %w", section.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `
insert into section_structures (
  section_id, document_id, parent_section_id, source_element_id, element_kind,
  depth, sibling_ordinal, order_path_json, display_number, presentation_json
) values (?, ?, nullif(?, ''), ?, ?, ?, ?, ?, ?, ?)
`, section.ID, doc.ID, section.Structure.ParentSectionID, section.Structure.SourceElementID, section.Structure.ElementKind,
			section.Structure.Depth, section.Structure.SiblingOrdinal, string(orderPathJSON), section.Structure.DisplayNumber, section.Structure.PresentationJSON); err != nil {
			return fmt.Errorf("insert structure for section %s: %w", section.ID, err)
		}
	}
	for _, edgeID := range evidenceEdgeIDs {
		if err := replaceEdgeMembershipsTx(ctx, tx, edgeID); err != nil {
			return fmt.Errorf("refresh evidence edge %s after replacing document %s: %w", edgeID, doc.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit replace document %s: %w", doc.ID, err)
	}
	return nil
}

func (s *Store) ReplaceWorkbookBundle(ctx context.Context, bundle domain.WorkbookBundle) (domain.WorkbookReplaceResult, error) {
	bundle, err := normalizeWorkbookBundle(bundle)
	if err != nil {
		return domain.WorkbookReplaceResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.WorkbookReplaceResult{}, err
	}
	defer tx.Rollback()

	var sourceExists bool
	if err := tx.QueryRowContext(ctx, `select exists(select 1 from sources where id = ?)`, bundle.SourceID).Scan(&sourceExists); err != nil {
		return domain.WorkbookReplaceResult{}, err
	}
	if !sourceExists {
		return domain.WorkbookReplaceResult{}, fmt.Errorf("source %q not found", bundle.SourceID)
	}

	oldSectionIDs, err := sectionIDsForSourceTx(ctx, tx, bundle.SourceID)
	if err != nil {
		return domain.WorkbookReplaceResult{}, err
	}
	oldDocumentHashes, err := documentHashesForSourceTx(ctx, tx, bundle.SourceID)
	if err != nil {
		return domain.WorkbookReplaceResult{}, err
	}
	newSectionIDs := make(map[string]bool)
	newDocumentIDs := make(map[string]bool)
	documentTitles := make(map[string]string)
	for _, item := range bundle.Documents {
		newDocumentIDs[item.Document.ID] = true
		documentTitles[item.Document.ID] = item.Document.Title
		for _, section := range item.Sections {
			newSectionIDs[section.ID] = true
		}
	}
	staleSectionIDs := make([]string, 0)
	for _, sectionID := range oldSectionIDs {
		if !newSectionIDs[sectionID] {
			staleSectionIDs = append(staleSectionIDs, sectionID)
		}
	}
	sort.Strings(staleSectionIDs)

	for _, blob := range bundle.Blobs {
		if _, err := tx.ExecContext(ctx, `
insert into media_blobs (sha256, size_bytes, sniffed_media_type, storage_key)
values (?, ?, ?, ?)
on conflict(sha256) do update set
  size_bytes = excluded.size_bytes,
  sniffed_media_type = excluded.sniffed_media_type,
  storage_key = excluded.storage_key
`, blob.SHA256, blob.SizeBytes, blob.SniffedMediaType, blob.StorageKey); err != nil {
			return domain.WorkbookReplaceResult{}, fmt.Errorf("upsert media blob %s: %w", blob.SHA256, err)
		}
	}

	// Logical snapshot/media state is replaced in the same transaction. Blob
	// files are immutable and may already have been finalized by the caller.
	if _, err := tx.ExecContext(ctx, `delete from media_assets where source_id = ?`, bundle.SourceID); err != nil {
		return domain.WorkbookReplaceResult{}, fmt.Errorf("delete old media assets: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `delete from source_snapshots where source_id = ?`, bundle.SourceID); err != nil {
		return domain.WorkbookReplaceResult{}, fmt.Errorf("delete old source snapshots: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
insert into source_snapshots (
  id, source_id, source_hash, blob_sha256, format_family, format_version,
  semantic_hash, media_manifest_hash, metadata_json, active
) values (?, ?, ?, ?, ?, ?, ?, ?, ?, 1)
`, bundle.Snapshot.ID, bundle.SourceID, bundle.Snapshot.SourceHash, bundle.Snapshot.BlobSHA256,
		bundle.Snapshot.FormatFamily, bundle.Snapshot.FormatVersion, bundle.Snapshot.SemanticHash,
		bundle.Snapshot.MediaManifestHash, bundle.Snapshot.MetadataJSON); err != nil {
		return domain.WorkbookReplaceResult{}, fmt.Errorf("insert active source snapshot: %w", err)
	}

	for _, item := range bundle.Documents {
		doc := item.Document
		if _, err := tx.ExecContext(ctx, `
insert into documents (id, source_id, external_id, title, url, version, content_hash, metadata_json, indexed_at)
values (?, ?, ?, ?, ?, ?, ?, ?, current_timestamp)
on conflict(id) do update set
  source_id = excluded.source_id,
  external_id = excluded.external_id,
  title = excluded.title,
  url = excluded.url,
  version = excluded.version,
  content_hash = excluded.content_hash,
  metadata_json = excluded.metadata_json,
  updated_at = current_timestamp,
  indexed_at = current_timestamp
`, doc.ID, bundle.SourceID, doc.ExternalID, doc.Title, doc.URL, doc.Version, doc.ContentHash, doc.MetadataJSON); err != nil {
			return domain.WorkbookReplaceResult{}, fmt.Errorf("upsert workbook document %s: %w", doc.ID, err)
		}
		if previousHash, existed := oldDocumentHashes[doc.ID]; existed && previousHash != doc.ContentHash {
			// Admin-authored desc is retained, while generated retrieval/entity
			// state tied to the previous source text is invalidated atomically.
			if _, err := tx.ExecContext(ctx, `
update document_profiles
set retrieval_profile_json = '{}', generated_from_hash = '', generated_at = '', updated_at = current_timestamp
where document_id = ?
`, doc.ID); err != nil {
				return domain.WorkbookReplaceResult{}, fmt.Errorf("invalidate stale workbook profile %s: %w", doc.ID, err)
			}
			var desc string
			if err := tx.QueryRowContext(ctx, `select "desc" from document_profiles where document_id = ?`, doc.ID).Scan(&desc); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return domain.WorkbookReplaceResult{}, fmt.Errorf("read invalidated workbook profile %s: %w", doc.ID, err)
			} else if err == nil {
				if err := replaceDocumentProfileSearchIndex(ctx, tx, doc.ID, desc, "{}"); err != nil {
					return domain.WorkbookReplaceResult{}, err
				}
			}
			for _, table := range []string{"fts_section_entities", "fts_section_entities_trigram"} {
				if _, err := tx.ExecContext(ctx, "delete from "+table+" where entity_id in (select id from section_entities where document_id = ?)", doc.ID); err != nil {
					return domain.WorkbookReplaceResult{}, fmt.Errorf("delete stale workbook %s %s: %w", table, doc.ID, err)
				}
			}
			if _, err := tx.ExecContext(ctx, `delete from section_entities where document_id = ?`, doc.ID); err != nil {
				return domain.WorkbookReplaceResult{}, fmt.Errorf("delete stale workbook entities %s: %w", doc.ID, err)
			}
		}
	}

	// Upsert every Section before deleting stale rows so a Topic can move
	// between Sheet Documents without changing its stable Section ID.
	for _, item := range bundle.Documents {
		for _, section := range item.Sections {
			if _, err := tx.ExecContext(ctx, `
insert into sections (id, document_id, heading_path, title, content, search_text, content_hash, ordinal, metadata_json)
values (?, ?, ?, ?, ?, ?, ?, ?, ?)
on conflict(id) do update set
  document_id = excluded.document_id,
  heading_path = excluded.heading_path,
  title = excluded.title,
  content = excluded.content,
  search_text = excluded.search_text,
  content_hash = excluded.content_hash,
  ordinal = excluded.ordinal,
  metadata_json = excluded.metadata_json,
  updated_at = current_timestamp
`, section.ID, item.Document.ID, section.HeadingPath, section.Title, section.Content, section.SearchText,
				section.ContentHash, section.Ordinal, section.MetadataJSON); err != nil {
				return domain.WorkbookReplaceResult{}, fmt.Errorf("upsert workbook section %s: %w", section.ID, err)
			}
		}
	}

	if _, err := tx.ExecContext(ctx, `
delete from section_structures
where section_id in (
  select sections.id from sections join documents on documents.id = sections.document_id
  where documents.source_id = ?
)
`, bundle.SourceID); err != nil {
		return domain.WorkbookReplaceResult{}, fmt.Errorf("delete old section structures: %w", err)
	}
	for _, item := range bundle.Documents {
		for _, section := range item.Sections {
			structure := section.Structure
			if structure == nil {
				continue
			}
			orderPathJSON, _ := json.Marshal(structure.OrderPath)
			if _, err := tx.ExecContext(ctx, `
insert into section_structures (
  section_id, document_id, parent_section_id, source_element_id, element_kind,
  depth, sibling_ordinal, order_path_json, display_number, presentation_json
) values (?, ?, nullif(?, ''), ?, ?, ?, ?, ?, ?, ?)
`, section.ID, item.Document.ID, structure.ParentSectionID, structure.SourceElementID, structure.ElementKind,
				structure.Depth, structure.SiblingOrdinal, string(orderPathJSON), structure.DisplayNumber, structure.PresentationJSON); err != nil {
				return domain.WorkbookReplaceResult{}, fmt.Errorf("insert structure for section %s: %w", section.ID, err)
			}
		}
	}

	if _, err := tx.ExecContext(ctx, `delete from fts_section_tokens where document_id in (select id from documents where source_id = ?)`, bundle.SourceID); err != nil {
		return domain.WorkbookReplaceResult{}, fmt.Errorf("delete workbook token FTS: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `delete from fts_section_tokens_trigram where document_id in (select id from documents where source_id = ?)`, bundle.SourceID); err != nil {
		return domain.WorkbookReplaceResult{}, fmt.Errorf("delete workbook trigram FTS: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `delete from fts_section_shortgrams where document_id in (select id from documents where source_id = ?)`, bundle.SourceID); err != nil {
		return domain.WorkbookReplaceResult{}, fmt.Errorf("delete workbook short-gram FTS: %w", err)
	}
	for _, item := range bundle.Documents {
		for _, section := range item.Sections {
			titleTokens, headingTokens, symbolTokens, contentTokens := buildSectionIndexTexts(
				documentTitles[item.Document.ID], section.Title, section.HeadingPath, section.SearchText,
			)
			if _, err := tx.ExecContext(ctx, `
insert into fts_section_tokens (title_tokens, section_heading_tokens, symbol_tokens, content_tokens, section_id, document_id)
values (?, ?, ?, ?, ?, ?)
`, titleTokens, headingTokens, symbolTokens, contentTokens, section.ID, item.Document.ID); err != nil {
				return domain.WorkbookReplaceResult{}, fmt.Errorf("insert workbook token FTS for %s: %w", section.ID, err)
			}
			if _, err := tx.ExecContext(ctx, `
insert into fts_section_tokens_trigram (title_tokens, section_heading_tokens, symbol_tokens, content_tokens, section_id, document_id)
values (?, ?, ?, ?, ?, ?)
`, titleTokens, headingTokens, symbolTokens, contentTokens, section.ID, item.Document.ID); err != nil {
				return domain.WorkbookReplaceResult{}, fmt.Errorf("insert workbook trigram FTS for %s: %w", section.ID, err)
			}
			titleGrams, headingGrams, contentGrams := buildSectionShortGramIndexTexts(
				documentTitles[item.Document.ID], section.Title, section.HeadingPath, section.SearchText,
			)
			if _, err := tx.ExecContext(ctx, `
insert into fts_section_shortgrams (title_grams, section_heading_grams, content_grams, section_id, document_id)
values (?, ?, ?, ?, ?)
`, titleGrams, headingGrams, contentGrams, section.ID, item.Document.ID); err != nil {
				return domain.WorkbookReplaceResult{}, fmt.Errorf("insert workbook short-gram FTS for %s: %w", section.ID, err)
			}
		}
	}

	newNodeIDs := make(map[string]bool, len(bundle.Nodes))
	for _, node := range bundle.Nodes {
		newNodeIDs[node.ID] = true
		if _, err := tx.ExecContext(ctx, `
insert into nodes (id, kind, name, canonical_name, metadata_json, confidence, owner_source_id, owner_scope)
values (?, ?, ?, ?, ?, ?, ?, ?)
on conflict(id) do update set
  kind = excluded.kind,
  name = excluded.name,
  canonical_name = excluded.canonical_name,
  metadata_json = excluded.metadata_json,
  confidence = excluded.confidence,
  owner_source_id = excluded.owner_source_id,
  owner_scope = excluded.owner_scope,
  updated_at = current_timestamp
`, node.ID, node.Kind, node.Name, node.CanonicalName, node.MetadataJSON, node.Confidence, bundle.SourceID, bundle.WorkbookKey); err != nil {
			return domain.WorkbookReplaceResult{}, fmt.Errorf("upsert owned node %s: %w", node.ID, err)
		}
		if err := replaceNodeMembershipsTx(ctx, tx, node.ID); err != nil {
			return domain.WorkbookReplaceResult{}, fmt.Errorf("update memberships for owned node %s: %w", node.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `delete from fts_nodes where node_id = ?`, node.ID); err != nil {
			return domain.WorkbookReplaceResult{}, fmt.Errorf("delete owned node FTS %s: %w", node.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `
insert into fts_nodes (kind, name, canonical_name, metadata_json, node_id) values (?, ?, ?, ?, ?)
`, node.Kind, node.Name, node.CanonicalName, node.MetadataJSON, node.ID); err != nil {
			return domain.WorkbookReplaceResult{}, fmt.Errorf("insert owned node FTS %s: %w", node.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `delete from fts_nodes_trigram where node_id = ?`, node.ID); err != nil {
			return domain.WorkbookReplaceResult{}, fmt.Errorf("delete owned node trigram FTS %s: %w", node.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `insert into fts_nodes_trigram (id, kind, name, canonical_name, metadata_json, node_id) values (?, ?, ?, ?, ?, ?)`, node.ID, node.Kind, node.Name, node.CanonicalName, node.MetadataJSON, node.ID); err != nil {
			return domain.WorkbookReplaceResult{}, fmt.Errorf("insert owned node trigram FTS %s: %w", node.ID, err)
		}
		if err := replaceNodeShortGramSearchIndex(ctx, tx, node.ID, node.Kind, node.Name, node.CanonicalName); err != nil {
			return domain.WorkbookReplaceResult{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `
delete from section_nodes
where section_id in (
  select sections.id from sections join documents on documents.id = sections.document_id
  where documents.source_id = ?
)
`, bundle.SourceID); err != nil {
		return domain.WorkbookReplaceResult{}, fmt.Errorf("delete old workbook section-node mappings: %w", err)
	}
	for _, link := range bundle.SectionNodes {
		if _, err := tx.ExecContext(ctx, `
insert into section_nodes (section_id, node_id, role, confidence) values (?, ?, ?, ?)
`, link.SectionID, link.NodeID, link.Role, link.Confidence); err != nil {
			return domain.WorkbookReplaceResult{}, fmt.Errorf("insert section-node mapping %s/%s: %w", link.SectionID, link.NodeID, err)
		}
	}

	newEdgeIDs := make(map[string]bool, len(bundle.Edges))
	for _, edge := range bundle.Edges {
		newEdgeIDs[edge.ID] = true
		if _, err := tx.ExecContext(ctx, `
insert into edges (
  id, src_id, dst_id, kind, confidence, provenance, evidence_section_id,
  source_revision, metadata_json, owner_source_id, owner_scope
) values (?, ?, ?, ?, ?, ?, nullif(?, ''), ?, ?, ?, ?)
on conflict(id) do update set
  src_id = excluded.src_id,
  dst_id = excluded.dst_id,
  kind = excluded.kind,
  confidence = excluded.confidence,
  provenance = excluded.provenance,
  evidence_section_id = excluded.evidence_section_id,
  source_revision = excluded.source_revision,
  metadata_json = excluded.metadata_json,
  owner_source_id = excluded.owner_source_id,
  owner_scope = excluded.owner_scope,
  updated_at = current_timestamp
`, edge.ID, edge.SrcID, edge.DstID, edge.Kind, edge.Confidence, edge.Provenance,
			edge.EvidenceSectionID, edge.SourceRevision, edge.MetadataJSON, bundle.SourceID, bundle.WorkbookKey); err != nil {
			return domain.WorkbookReplaceResult{}, fmt.Errorf("upsert owned edge %s: %w", edge.ID, err)
		}
		if err := replaceEdgeMembershipsTx(ctx, tx, edge.ID); err != nil {
			return domain.WorkbookReplaceResult{}, fmt.Errorf("update memberships for owned edge %s: %w", edge.ID, err)
		}
	}

	oldEdgeIDs, err := ownedGraphIDsTx(ctx, tx, "edges", bundle.SourceID, bundle.WorkbookKey)
	if err != nil {
		return domain.WorkbookReplaceResult{}, err
	}
	for _, edgeID := range oldEdgeIDs {
		if newEdgeIDs[edgeID] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `delete from edges where id = ? and owner_source_id = ? and owner_scope = ?`, edgeID, bundle.SourceID, bundle.WorkbookKey); err != nil {
			return domain.WorkbookReplaceResult{}, fmt.Errorf("delete stale owned edge %s: %w", edgeID, err)
		}
	}
	oldNodeIDs, err := ownedGraphIDsTx(ctx, tx, "nodes", bundle.SourceID, bundle.WorkbookKey)
	if err != nil {
		return domain.WorkbookReplaceResult{}, err
	}
	for _, nodeID := range oldNodeIDs {
		if newNodeIDs[nodeID] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `delete from fts_nodes where node_id = ?`, nodeID); err != nil {
			return domain.WorkbookReplaceResult{}, fmt.Errorf("delete stale owned node FTS %s: %w", nodeID, err)
		}
		if _, err := tx.ExecContext(ctx, `delete from fts_nodes_trigram where node_id = ?`, nodeID); err != nil {
			return domain.WorkbookReplaceResult{}, fmt.Errorf("delete stale owned node trigram FTS %s: %w", nodeID, err)
		}
		if _, err := tx.ExecContext(ctx, `delete from fts_nodes_shortgrams where node_id = ?`, nodeID); err != nil {
			return domain.WorkbookReplaceResult{}, fmt.Errorf("delete stale owned node short-gram FTS %s: %w", nodeID, err)
		}
		if _, err := tx.ExecContext(ctx, `delete from nodes where id = ? and owner_source_id = ? and owner_scope = ?`, nodeID, bundle.SourceID, bundle.WorkbookKey); err != nil {
			return domain.WorkbookReplaceResult{}, fmt.Errorf("delete stale owned node %s: %w", nodeID, err)
		}
	}

	staleEvidenceEdgeIDs := make(map[string]bool)
	for _, sectionID := range staleSectionIDs {
		rows, err := tx.QueryContext(ctx, `select id from edges where evidence_section_id = ?`, sectionID)
		if err != nil {
			return domain.WorkbookReplaceResult{}, fmt.Errorf("list evidence edges for stale workbook section %s: %w", sectionID, err)
		}
		for rows.Next() {
			var edgeID string
			if err := rows.Scan(&edgeID); err != nil {
				rows.Close()
				return domain.WorkbookReplaceResult{}, fmt.Errorf("scan evidence edge for stale workbook section %s: %w", sectionID, err)
			}
			staleEvidenceEdgeIDs[edgeID] = true
		}
		if err := rows.Close(); err != nil {
			return domain.WorkbookReplaceResult{}, err
		}
	}
	for _, sectionID := range staleSectionIDs {
		for _, table := range []string{"fts_section_entities", "fts_section_entities_trigram"} {
			if _, err := tx.ExecContext(ctx, "delete from "+table+" where entity_id in (select id from section_entities where section_id = ?)", sectionID); err != nil {
				return domain.WorkbookReplaceResult{}, fmt.Errorf("delete stale workbook section %s from %s: %w", sectionID, table, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `delete from sections where id = ?`, sectionID); err != nil {
			return domain.WorkbookReplaceResult{}, fmt.Errorf("delete stale workbook section %s: %w", sectionID, err)
		}
	}
	for edgeID := range staleEvidenceEdgeIDs {
		if err := replaceEdgeMembershipsTx(ctx, tx, edgeID); err != nil {
			return domain.WorkbookReplaceResult{}, fmt.Errorf("refresh evidence edge %s after workbook reconciliation: %w", edgeID, err)
		}
	}
	oldDocumentIDs, err := documentIDsForSourceTx(ctx, tx, bundle.SourceID)
	if err != nil {
		return domain.WorkbookReplaceResult{}, err
	}
	for _, documentID := range oldDocumentIDs {
		if newDocumentIDs[documentID] {
			continue
		}
		for _, table := range []string{"fts_document_profiles", "fts_document_profiles_trigram"} {
			if _, err := tx.ExecContext(ctx, "delete from "+table+" where document_id = ?", documentID); err != nil {
				return domain.WorkbookReplaceResult{}, fmt.Errorf("delete stale workbook document %s from %s: %w", documentID, table, err)
			}
		}
		for _, table := range []string{"fts_section_entities", "fts_section_entities_trigram"} {
			if _, err := tx.ExecContext(ctx, "delete from "+table+" where entity_id in (select id from section_entities where document_id = ?)", documentID); err != nil {
				return domain.WorkbookReplaceResult{}, fmt.Errorf("delete stale workbook document %s from %s: %w", documentID, table, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `delete from documents where id = ? and source_id = ?`, documentID, bundle.SourceID); err != nil {
			return domain.WorkbookReplaceResult{}, fmt.Errorf("delete stale workbook document %s: %w", documentID, err)
		}
	}

	for _, asset := range bundle.MediaAssets {
		if _, err := tx.ExecContext(ctx, `
insert into media_assets (
  id, source_id, snapshot_id, document_id, external_id, blob_sha256, kind,
  original_name, media_type, size_bytes, status, metadata_json
) values (?, ?, ?, ?, ?, nullif(?, ''), ?, ?, ?, ?, ?, ?)
`, asset.ID, bundle.SourceID, bundle.Snapshot.ID, asset.DocumentID, asset.ExternalID, asset.BlobSHA256,
			asset.Kind, asset.OriginalName, asset.MediaType, asset.SizeBytes, asset.Status, asset.MetadataJSON); err != nil {
			return domain.WorkbookReplaceResult{}, fmt.Errorf("insert media asset %s: %w", asset.ID, err)
		}
	}
	for _, ref := range bundle.MediaRefs {
		if _, err := tx.ExecContext(ctx, `
insert into section_media_refs (section_id, asset_id, role, ordinal, metadata_json)
values (?, ?, ?, ?, ?)
`, ref.SectionID, ref.AssetID, ref.Role, ref.Ordinal, ref.MetadataJSON); err != nil {
			return domain.WorkbookReplaceResult{}, fmt.Errorf("insert media reference %s/%s: %w", ref.SectionID, ref.AssetID, err)
		}
	}
	for _, feature := range bundle.FeatureInventory {
		if _, err := tx.ExecContext(ctx, `
insert into source_feature_inventory (
  id, snapshot_id, feature_key, coverage_status, element_path, count, metadata_json
) values (?, ?, ?, ?, ?, ?, ?)
`, feature.ID, bundle.Snapshot.ID, feature.FeatureKey, feature.CoverageStatus, feature.ElementPath, feature.Count, feature.MetadataJSON); err != nil {
			return domain.WorkbookReplaceResult{}, fmt.Errorf("insert feature inventory %s: %w", feature.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return domain.WorkbookReplaceResult{}, fmt.Errorf("commit workbook replacement for source %s: %w", bundle.SourceID, err)
	}
	return domain.WorkbookReplaceResult{ActiveSnapshotID: bundle.Snapshot.ID, StaleSectionIDs: staleSectionIDs}, nil
}

func sectionIDsForSourceTx(ctx context.Context, tx *sql.Tx, sourceID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
select sections.id from sections join documents on documents.id = sections.document_id
where documents.source_id = ? order by sections.id
`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func documentIDsForSourceTx(ctx context.Context, tx *sql.Tx, sourceID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `select id from documents where source_id = ? order by id`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func documentHashesForSourceTx(ctx context.Context, tx *sql.Tx, sourceID string) (map[string]string, error) {
	rows, err := tx.QueryContext(ctx, `select id, content_hash from documents where source_id = ?`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hashes := make(map[string]string)
	for rows.Next() {
		var id, hash string
		if err := rows.Scan(&id, &hash); err != nil {
			return nil, err
		}
		hashes[id] = hash
	}
	return hashes, rows.Err()
}

func ownedGraphIDsTx(ctx context.Context, tx *sql.Tx, table string, sourceID string, ownerScope string) ([]string, error) {
	if table != "nodes" && table != "edges" {
		return nil, fmt.Errorf("unsupported graph ownership table %q", table)
	}
	rows, err := tx.QueryContext(ctx, `select id from `+table+` where owner_source_id = ? and owner_scope = ? order by id`, sourceID, ownerScope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func normalizeWorkbookBundle(bundle domain.WorkbookBundle) (domain.WorkbookBundle, error) {
	bundle.SourceID = strings.TrimSpace(bundle.SourceID)
	bundle.WorkbookKey = strings.TrimSpace(bundle.WorkbookKey)
	if bundle.SourceID == "" || bundle.WorkbookKey == "" {
		return bundle, fmt.Errorf("workbook source_id and workbook_key are required")
	}
	bundle.Snapshot.ID = strings.TrimSpace(bundle.Snapshot.ID)
	bundle.Snapshot.SourceID = strings.TrimSpace(bundle.Snapshot.SourceID)
	if bundle.Snapshot.SourceID == "" {
		bundle.Snapshot.SourceID = bundle.SourceID
	}
	if bundle.Snapshot.ID == "" || bundle.Snapshot.SourceID != bundle.SourceID || strings.TrimSpace(bundle.Snapshot.SourceHash) == "" || strings.TrimSpace(bundle.Snapshot.FormatFamily) == "" {
		return bundle, fmt.Errorf("valid snapshot id, source, hash, and format family are required")
	}
	var err error
	if bundle.Snapshot.MetadataJSON, err = normalizeJSONObject(bundle.Snapshot.MetadataJSON, "snapshot metadata_json"); err != nil {
		return bundle, err
	}

	blobIDs := make(map[string]bool, len(bundle.Blobs))
	for i := range bundle.Blobs {
		blob := &bundle.Blobs[i]
		blob.SHA256 = strings.ToLower(strings.TrimSpace(blob.SHA256))
		blob.SniffedMediaType = strings.TrimSpace(blob.SniffedMediaType)
		blob.StorageKey = strings.TrimSpace(blob.StorageKey)
		if !validSHA256(blob.SHA256) || blob.SizeBytes < 0 || blob.SniffedMediaType == "" || blob.StorageKey == "" {
			return bundle, fmt.Errorf("invalid media blob %q", blob.SHA256)
		}
		if blobIDs[blob.SHA256] {
			return bundle, fmt.Errorf("duplicate media blob %s", blob.SHA256)
		}
		blobIDs[blob.SHA256] = true
	}
	bundle.Snapshot.BlobSHA256 = strings.ToLower(strings.TrimSpace(bundle.Snapshot.BlobSHA256))
	if !blobIDs[bundle.Snapshot.BlobSHA256] {
		return bundle, fmt.Errorf("snapshot blob %s is not included in bundle", bundle.Snapshot.BlobSHA256)
	}

	documentIDs := make(map[string]bool)
	externalDocuments := make(map[string]bool)
	sectionIDs := make(map[string]bool)
	sectionDocuments := make(map[string]string)
	sectionDepths := make(map[string]int)
	sourceElementIDs := make(map[string]bool)
	for i := range bundle.Documents {
		item := &bundle.Documents[i]
		doc := &item.Document
		doc.ID = strings.TrimSpace(doc.ID)
		doc.SourceID = strings.TrimSpace(doc.SourceID)
		if doc.SourceID == "" {
			doc.SourceID = bundle.SourceID
		}
		if doc.ID == "" || doc.SourceID != bundle.SourceID || strings.TrimSpace(doc.ExternalID) == "" || strings.TrimSpace(doc.Title) == "" {
			return bundle, fmt.Errorf("invalid workbook document %q", doc.ID)
		}
		if documentIDs[doc.ID] || externalDocuments[doc.ExternalID] {
			return bundle, fmt.Errorf("duplicate workbook document id or external id %q", doc.ID)
		}
		documentIDs[doc.ID] = true
		externalDocuments[doc.ExternalID] = true
		if doc.MetadataJSON, err = normalizeJSONObject(doc.MetadataJSON, "document "+doc.ID+" metadata_json"); err != nil {
			return bundle, err
		}
		for j := range item.Sections {
			section := &item.Sections[j]
			section.ID = strings.TrimSpace(section.ID)
			section.DocumentID = doc.ID
			if section.ID == "" || sectionIDs[section.ID] {
				return bundle, fmt.Errorf("empty or duplicate section id %q", section.ID)
			}
			sectionIDs[section.ID] = true
			sectionDocuments[section.ID] = doc.ID
			if section.SearchText == "" {
				section.SearchText = section.Content
			}
			if section.MetadataJSON, err = normalizeJSONObject(section.MetadataJSON, "section "+section.ID+" metadata_json"); err != nil {
				return bundle, err
			}
			if section.Structure == nil {
				return bundle, fmt.Errorf("section %s has no structure", section.ID)
			}
			structure := section.Structure
			structure.ParentSectionID = strings.TrimSpace(structure.ParentSectionID)
			structure.SourceElementID = strings.TrimSpace(structure.SourceElementID)
			structure.ElementKind = strings.TrimSpace(structure.ElementKind)
			if structure.SourceElementID == "" || sourceElementIDs[structure.SourceElementID] || structure.ElementKind == "" || structure.Depth < 0 || structure.SiblingOrdinal < 0 || len(structure.OrderPath) != structure.Depth+1 {
				return bundle, fmt.Errorf("invalid structure for section %s", section.ID)
			}
			sourceElementIDs[structure.SourceElementID] = true
			sectionDepths[section.ID] = structure.Depth
			if structure.PresentationJSON, err = normalizeJSONObject(structure.PresentationJSON, "section "+section.ID+" presentation_json"); err != nil {
				return bundle, err
			}
		}
	}
	if len(documentIDs) == 0 || len(sectionIDs) == 0 {
		return bundle, fmt.Errorf("workbook bundle must contain documents and sections")
	}
	for _, item := range bundle.Documents {
		for _, section := range item.Sections {
			parentID := section.Structure.ParentSectionID
			if parentID == "" {
				if section.Structure.Depth != 0 {
					return bundle, fmt.Errorf("root section %s must have depth 0", section.ID)
				}
				continue
			}
			if !sectionIDs[parentID] || sectionDocuments[parentID] != item.Document.ID {
				return bundle, fmt.Errorf("section %s has invalid parent %s", section.ID, parentID)
			}
			if sectionDepths[parentID]+1 != section.Structure.Depth {
				return bundle, fmt.Errorf("section %s depth does not follow parent %s", section.ID, parentID)
			}
		}
	}

	nodeIDs := make(map[string]bool, len(bundle.Nodes))
	nodeKinds := make(map[string]string, len(bundle.Nodes))
	for i := range bundle.Nodes {
		node := &bundle.Nodes[i]
		node.ID = strings.TrimSpace(node.ID)
		node.Kind = strings.TrimSpace(node.Kind)
		node.Name = strings.TrimSpace(node.Name)
		if node.ID == "" || node.Kind == "" || node.Name == "" || nodeIDs[node.ID] {
			return bundle, fmt.Errorf("invalid or duplicate node %q", node.ID)
		}
		nodeIDs[node.ID] = true
		nodeKinds[node.ID] = node.Kind
		if strings.TrimSpace(node.CanonicalName) == "" {
			node.CanonicalName = strings.ToLower(node.Name)
		}
		if node.Confidence <= 0 {
			node.Confidence = 1
		}
		if node.MetadataJSON, err = normalizeJSONObject(node.MetadataJSON, "node "+node.ID+" metadata_json"); err != nil {
			return bundle, err
		}
	}
	mappedSections := make(map[string]bool)
	mappingKeys := make(map[string]bool)
	for i := range bundle.SectionNodes {
		link := &bundle.SectionNodes[i]
		link.SectionID = strings.TrimSpace(link.SectionID)
		link.NodeID = strings.TrimSpace(link.NodeID)
		link.Role = strings.TrimSpace(link.Role)
		if link.Role == "" {
			link.Role = "represents"
		}
		if link.Confidence <= 0 {
			link.Confidence = 1
		}
		key := link.SectionID + "\x00" + link.NodeID + "\x00" + link.Role
		if !sectionIDs[link.SectionID] || !nodeIDs[link.NodeID] || mappingKeys[key] {
			return bundle, fmt.Errorf("invalid section-node mapping %s/%s", link.SectionID, link.NodeID)
		}
		if nodeKinds[link.NodeID] == "DocSection" {
			mappedSections[link.SectionID] = true
		}
		mappingKeys[key] = true
	}
	for sectionID := range sectionIDs {
		if !mappedSections[sectionID] {
			return bundle, fmt.Errorf("section %s has no DocSection node mapping", sectionID)
		}
	}

	edgeIDs := make(map[string]bool, len(bundle.Edges))
	for i := range bundle.Edges {
		edge := &bundle.Edges[i]
		edge.ID = strings.TrimSpace(edge.ID)
		edge.SrcID = strings.TrimSpace(edge.SrcID)
		edge.DstID = strings.TrimSpace(edge.DstID)
		edge.Kind = strings.TrimSpace(edge.Kind)
		if edge.ID == "" || edgeIDs[edge.ID] || !nodeIDs[edge.SrcID] || !nodeIDs[edge.DstID] || edge.Kind == "" {
			return bundle, fmt.Errorf("invalid or duplicate edge %q", edge.ID)
		}
		if edge.EvidenceSectionID != "" && !sectionIDs[edge.EvidenceSectionID] {
			return bundle, fmt.Errorf("edge %s has unknown evidence section", edge.ID)
		}
		if edge.Confidence <= 0 {
			edge.Confidence = 1
		}
		if strings.TrimSpace(edge.Provenance) == "" {
			edge.Provenance = "source_authored"
		}
		if edge.MetadataJSON, err = normalizeJSONObject(edge.MetadataJSON, "edge "+edge.ID+" metadata_json"); err != nil {
			return bundle, err
		}
		edgeIDs[edge.ID] = true
	}

	assetIDs := make(map[string]bool, len(bundle.MediaAssets))
	assetExternalKeys := make(map[string]bool)
	for i := range bundle.MediaAssets {
		asset := &bundle.MediaAssets[i]
		asset.ID = strings.TrimSpace(asset.ID)
		asset.SourceID = bundle.SourceID
		asset.SnapshotID = bundle.Snapshot.ID
		asset.DocumentID = strings.TrimSpace(asset.DocumentID)
		asset.ExternalID = strings.TrimSpace(asset.ExternalID)
		asset.BlobSHA256 = strings.ToLower(strings.TrimSpace(asset.BlobSHA256))
		asset.Kind = strings.TrimSpace(asset.Kind)
		asset.Status = strings.TrimSpace(asset.Status)
		key := asset.DocumentID + "\x00" + asset.ExternalID
		if asset.ID == "" || assetIDs[asset.ID] || !documentIDs[asset.DocumentID] || asset.ExternalID == "" || assetExternalKeys[key] || asset.Kind == "" || asset.Status == "" || asset.SizeBytes < 0 {
			return bundle, fmt.Errorf("invalid or duplicate media asset %q", asset.ID)
		}
		if asset.BlobSHA256 != "" && !blobIDs[asset.BlobSHA256] {
			return bundle, fmt.Errorf("media asset %s references unknown blob", asset.ID)
		}
		if asset.MetadataJSON, err = normalizeJSONObject(asset.MetadataJSON, "media asset "+asset.ID+" metadata_json"); err != nil {
			return bundle, err
		}
		assetIDs[asset.ID] = true
		assetExternalKeys[key] = true
	}
	refKeys := make(map[string]bool)
	for i := range bundle.MediaRefs {
		ref := &bundle.MediaRefs[i]
		ref.SectionID = strings.TrimSpace(ref.SectionID)
		ref.AssetID = strings.TrimSpace(ref.AssetID)
		ref.Role = strings.TrimSpace(ref.Role)
		key := fmt.Sprintf("%s\x00%s\x00%s\x00%d", ref.SectionID, ref.AssetID, ref.Role, ref.Ordinal)
		if !sectionIDs[ref.SectionID] || !assetIDs[ref.AssetID] || ref.Role == "" || ref.Ordinal < 0 || refKeys[key] {
			return bundle, fmt.Errorf("invalid or duplicate media reference %s/%s", ref.SectionID, ref.AssetID)
		}
		if ref.MetadataJSON, err = normalizeJSONObject(ref.MetadataJSON, "media reference metadata_json"); err != nil {
			return bundle, err
		}
		refKeys[key] = true
	}
	featureIDs := make(map[string]bool)
	for i := range bundle.FeatureInventory {
		feature := &bundle.FeatureInventory[i]
		feature.SnapshotID = bundle.Snapshot.ID
		feature.FeatureKey = strings.TrimSpace(feature.FeatureKey)
		feature.CoverageStatus = strings.TrimSpace(feature.CoverageStatus)
		if feature.ID == "" {
			feature.ID = ids.Stable("feature", bundle.Snapshot.ID, feature.FeatureKey, feature.CoverageStatus, feature.ElementPath)
		}
		if featureIDs[feature.ID] || feature.FeatureKey == "" || !validCoverageStatus(feature.CoverageStatus) {
			return bundle, fmt.Errorf("invalid or duplicate feature inventory %q", feature.ID)
		}
		if feature.Count <= 0 {
			feature.Count = 1
		}
		if feature.MetadataJSON, err = normalizeJSONObject(feature.MetadataJSON, "feature inventory metadata_json"); err != nil {
			return bundle, err
		}
		featureIDs[feature.ID] = true
	}
	return bundle, nil
}

func normalizeJSONObject(value string, field string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "{}", nil
	}
	if !json.Valid([]byte(value)) {
		return "", fmt.Errorf("%s must be valid JSON", field)
	}
	return value, nil
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validCoverageStatus(value string) bool {
	switch value {
	case "indexed", "preserved", "rejected", "unsupported":
		return true
	default:
		return false
	}
}

func (s *Store) DeleteDocumentsNotInSource(ctx context.Context, sourceID string, keepDocumentIDs []string) error {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return fmt.Errorf("source id is required")
	}

	staleIDs, err := s.staleDocumentIDs(ctx, sourceID, keepDocumentIDs)
	if err != nil {
		return err
	}
	if len(staleIDs) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, docID := range staleIDs {
		evidenceEdgeIDs, err := evidenceEdgeIDsForDocumentTx(ctx, tx, docID)
		if err != nil {
			return fmt.Errorf("list evidence edges for stale document %s: %w", docID, err)
		}
		if _, err := tx.ExecContext(ctx, `delete from fts_section_tokens where document_id = ?`, docID); err != nil {
			return fmt.Errorf("delete stale fts_section_tokens for document %s: %w", docID, err)
		}
		if _, err := tx.ExecContext(ctx, `delete from fts_section_tokens_trigram where document_id = ?`, docID); err != nil {
			return fmt.Errorf("delete stale fts_section_tokens_trigram for document %s: %w", docID, err)
		}
		if _, err := tx.ExecContext(ctx, `delete from fts_section_shortgrams where document_id = ?`, docID); err != nil {
			return fmt.Errorf("delete stale fts_section_shortgrams for document %s: %w", docID, err)
		}
		for _, table := range []string{"fts_document_profiles", "fts_document_profiles_trigram"} {
			if _, err := tx.ExecContext(ctx, "delete from "+table+" where document_id = ?", docID); err != nil {
				return fmt.Errorf("delete stale %s for document %s: %w", table, docID, err)
			}
		}
		for _, table := range []string{"fts_section_entities", "fts_section_entities_trigram"} {
			if _, err := tx.ExecContext(ctx, "delete from "+table+" where entity_id in (select id from section_entities where document_id = ?)", docID); err != nil {
				return fmt.Errorf("delete stale %s for document %s: %w", table, docID, err)
			}
		}
		if err := deleteNodesBySelectorTx(ctx, tx, effectiveDocumentOwnedNodeIDsSQL, docID); err != nil {
			return fmt.Errorf("delete stale graph nodes for document %s: %w", docID, err)
		}
		if _, err := tx.ExecContext(ctx, `delete from documents where id = ? and source_id = ?`, docID, sourceID); err != nil {
			return fmt.Errorf("delete stale document %s: %w", docID, err)
		}
		for _, edgeID := range evidenceEdgeIDs {
			if err := replaceEdgeMembershipsTx(ctx, tx, edgeID); err != nil {
				return fmt.Errorf("refresh evidence edge %s after deleting document %s: %w", edgeID, docID, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete stale documents for source %s: %w", sourceID, err)
	}
	return nil
}

func evidenceEdgeIDsForDocumentTx(ctx context.Context, tx *sql.Tx, documentID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
select e.id
from sections s
join edges e on e.evidence_section_id = s.id
where s.document_id = ?
`, documentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var edgeIDs []string
	for rows.Next() {
		var edgeID string
		if err := rows.Scan(&edgeID); err != nil {
			return nil, err
		}
		edgeIDs = append(edgeIDs, edgeID)
	}
	return edgeIDs, rows.Err()
}

func (s *Store) staleDocumentIDs(ctx context.Context, sourceID string, keepDocumentIDs []string) ([]string, error) {
	keep := make(map[string]bool, len(keepDocumentIDs))
	for _, id := range keepDocumentIDs {
		id = strings.TrimSpace(id)
		if id != "" {
			keep[id] = true
		}
	}

	rows, err := s.db.QueryContext(ctx, `select id from documents where source_id = ?`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stale []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if !keep[id] {
			stale = append(stale, id)
		}
	}
	return stale, rows.Err()
}

func (s *Store) GetDocumentProfile(ctx context.Context, documentID string) (domain.DocumentProfile, error) {
	documentID = strings.TrimSpace(documentID)
	if documentID == "" {
		return domain.DocumentProfile{}, fmt.Errorf("document id is required")
	}
	if err := s.ensureDocumentExists(ctx, documentID); err != nil {
		return domain.DocumentProfile{}, err
	}
	profile, err := s.getDocumentProfile(ctx, documentID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.DocumentProfile{
			DocumentID:           documentID,
			RetrievalProfileJSON: "{}",
		}, nil
	}
	if err != nil {
		return domain.DocumentProfile{}, err
	}
	return profile, nil
}

func (s *Store) UpdateDocumentProfileDesc(ctx context.Context, input domain.DocumentProfileInput) (domain.DocumentProfile, error) {
	input.DocumentID = strings.TrimSpace(input.DocumentID)
	if input.DocumentID == "" {
		return domain.DocumentProfile{}, fmt.Errorf("document id is required")
	}
	if err := s.ensureDocumentExists(ctx, input.DocumentID); err != nil {
		return domain.DocumentProfile{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.DocumentProfile{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
insert into document_profiles (document_id)
values (?)
on conflict(document_id) do nothing
`, input.DocumentID); err != nil {
		return domain.DocumentProfile{}, err
	}
	if _, err := tx.ExecContext(ctx, `
update document_profiles
set "desc" = ?, updated_at = current_timestamp
where document_id = ?
`, input.Desc, input.DocumentID); err != nil {
		return domain.DocumentProfile{}, err
	}
	var profileJSON string
	if err := tx.QueryRowContext(ctx, `select retrieval_profile_json from document_profiles where document_id = ?`, input.DocumentID).Scan(&profileJSON); err != nil {
		return domain.DocumentProfile{}, err
	}
	if err := replaceDocumentProfileSearchIndex(ctx, tx, input.DocumentID, input.Desc, profileJSON); err != nil {
		return domain.DocumentProfile{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.DocumentProfile{}, err
	}
	return s.getDocumentProfile(ctx, input.DocumentID)
}

func (s *Store) UpsertDocumentRetrievalProfile(ctx context.Context, input domain.RetrievalProfileInput) (domain.DocumentProfile, error) {
	input.DocumentID = strings.TrimSpace(input.DocumentID)
	input.GeneratedFromHash = strings.TrimSpace(input.GeneratedFromHash)
	input.RetrievalProfileJSON = strings.TrimSpace(input.RetrievalProfileJSON)
	if input.DocumentID == "" {
		return domain.DocumentProfile{}, fmt.Errorf("document id is required")
	}
	if input.RetrievalProfileJSON == "" {
		input.RetrievalProfileJSON = "{}"
	}
	if !json.Valid([]byte(input.RetrievalProfileJSON)) {
		return domain.DocumentProfile{}, fmt.Errorf("retrieval_profile_json must be valid JSON")
	}
	if err := s.ensureDocumentExists(ctx, input.DocumentID); err != nil {
		return domain.DocumentProfile{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.DocumentProfile{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
insert into document_profiles (document_id, retrieval_profile_json, generated_from_hash, generated_at)
values (?, ?, ?, current_timestamp)
on conflict(document_id) do update set
  retrieval_profile_json = excluded.retrieval_profile_json,
  generated_from_hash = excluded.generated_from_hash,
	  generated_at = excluded.generated_at,
	  updated_at = current_timestamp
`, input.DocumentID, input.RetrievalProfileJSON, input.GeneratedFromHash); err != nil {
		return domain.DocumentProfile{}, err
	}
	var desc string
	if err := tx.QueryRowContext(ctx, `select "desc" from document_profiles where document_id = ?`, input.DocumentID).Scan(&desc); err != nil {
		return domain.DocumentProfile{}, err
	}
	if err := replaceDocumentProfileSearchIndex(ctx, tx, input.DocumentID, desc, input.RetrievalProfileJSON); err != nil {
		return domain.DocumentProfile{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.DocumentProfile{}, err
	}
	return s.getDocumentProfile(ctx, input.DocumentID)
}

type searchIndexExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func replaceDocumentProfileSearchIndex(ctx context.Context, exec searchIndexExecer, documentID string, desc string, profileJSON string) error {
	descTokens, profileTokens := documentProfileIndexTexts(desc, profileJSON)
	for _, table := range []string{"fts_document_profiles", "fts_document_profiles_trigram"} {
		if _, err := exec.ExecContext(ctx, "delete from "+table+" where document_id = ?", documentID); err != nil {
			return fmt.Errorf("delete %s for document %s: %w", table, documentID, err)
		}
		if _, err := exec.ExecContext(ctx, "insert into "+table+" (desc_tokens, profile_tokens, document_id) values (?, ?, ?)", descTokens, profileTokens, documentID); err != nil {
			return fmt.Errorf("insert %s for document %s: %w", table, documentID, err)
		}
	}
	return nil
}

func replaceNodeShortGramSearchIndex(ctx context.Context, exec searchIndexExecer, nodeID string, kind string, name string, canonicalName string) error {
	if _, err := exec.ExecContext(ctx, `delete from fts_nodes_shortgrams where node_id = ?`, nodeID); err != nil {
		return fmt.Errorf("delete short-gram index for node %s: %w", nodeID, err)
	}
	if _, err := exec.ExecContext(ctx, `
insert into fts_nodes_shortgrams (id_grams, kind_grams, name_grams, canonical_name_grams, node_id)
values (?, ?, ?, ?, ?)
`, shortGramIndexText(nodeID), shortGramIndexText(kind), shortGramIndexText(name), shortGramIndexText(canonicalName), nodeID); err != nil {
		return fmt.Errorf("insert short-gram index for node %s: %w", nodeID, err)
	}
	return nil
}

func documentProfileIndexTexts(desc string, profileJSON string) (string, string) {
	profile := parseStoredProfile(profileJSON)
	values := make([]string, 0)
	for _, term := range profile.TopTerms {
		values = append(values, term.Term)
	}
	values = append(values, profile.TopTags...)
	values = append(values, profile.Keyphrases...)
	values = append(values, profile.Aliases...)
	values = append(values, profile.APIRefs...)
	for _, section := range profile.SectionDistribution {
		values = append(values, section.Title)
		values = append(values, section.Terms...)
	}
	return tokenizedIndexText(desc), tokenizedIndexText(strings.Join(uniqueStrings(nonEmptyStrings(values)), "\n"))
}

func (s *Store) ensureDocumentProfile(ctx context.Context, documentID string) error {
	if err := s.ensureDocumentExists(ctx, documentID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
insert into document_profiles (document_id)
values (?)
on conflict(document_id) do nothing
`, documentID)
	return err
}

func (s *Store) ensureDocumentExists(ctx context.Context, documentID string) error {
	var found string
	err := s.db.QueryRowContext(ctx, `select id from documents where id = ?`, documentID).Scan(&found)
	return err
}

func (s *Store) getDocumentProfile(ctx context.Context, documentID string) (domain.DocumentProfile, error) {
	var profile domain.DocumentProfile
	err := s.readDB().QueryRowContext(ctx, `
select document_id, "desc", retrieval_profile_json, generated_from_hash, generated_at, created_at, updated_at
from document_profiles
where document_id = ?
`, documentID).Scan(&profile.DocumentID, &profile.Desc, &profile.RetrievalProfileJSON, &profile.GeneratedFromHash, &profile.GeneratedAt, &profile.CreatedAt, &profile.UpdatedAt)
	if err != nil {
		return domain.DocumentProfile{}, err
	}
	return profile, nil
}

type sectionEntityEvidence struct {
	Occurrences []sectionEntityOccurrence `json:"occurrences,omitempty"`
	Notes       []string                  `json:"notes,omitempty"`
}

type sectionEntityOccurrence struct {
	Raw       string `json:"raw,omitempty"`
	Source    string `json:"source,omitempty"`
	SpanStart int    `json:"span_start,omitempty"`
	SpanEnd   int    `json:"span_end,omitempty"`
	Evidence  string `json:"evidence,omitempty"`
}

func (s *Store) ReplaceSectionEntities(ctx context.Context, documentID string, entities []domain.SectionEntityInput) error {
	documentID = strings.TrimSpace(documentID)
	if documentID == "" {
		return fmt.Errorf("document id is required")
	}
	if err := s.ensureDocumentExists(ctx, documentID); err != nil {
		return err
	}
	sectionIDs, err := s.documentSectionIDSet(ctx, documentID)
	if err != nil {
		return err
	}
	aggregated, err := aggregateSectionEntityInputs(documentID, sectionIDs, entities)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, table := range []string{"fts_section_entities", "fts_section_entities_trigram"} {
		if _, err := tx.ExecContext(ctx, "delete from "+table+" where entity_id in (select id from section_entities where document_id = ?)", documentID); err != nil {
			return fmt.Errorf("delete %s for document %s: %w", table, documentID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `delete from section_entities where document_id = ?`, documentID); err != nil {
		return fmt.Errorf("delete section entities for document %s: %w", documentID, err)
	}
	for _, entity := range aggregated {
		_, err := tx.ExecContext(ctx, `
insert into section_entities (
  id, section_id, document_id, kind, raw_text, canonical_text,
  method, path, operation, source, confidence, evidence_json
) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, entity.ID, entity.SectionID, entity.DocumentID, entity.Kind, entity.RawText, entity.CanonicalText, entity.Method, entity.Path, entity.Operation, entity.Source, entity.Confidence, entity.EvidenceJSON)
		if err != nil {
			return fmt.Errorf("insert section entity %s: %w", entity.ID, err)
		}
		if err := insertSectionEntitySearchIndex(ctx, tx, entity); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit replace section entities for document %s: %w", documentID, err)
	}
	return nil
}

func (s *Store) ListDocumentEntities(ctx context.Context, documentID string) ([]domain.SectionEntity, error) {
	documentID = strings.TrimSpace(documentID)
	if documentID == "" {
		return nil, fmt.Errorf("document id is required")
	}
	rows, err := s.readDB().QueryContext(ctx, `
select id, section_id, document_id, kind, raw_text, canonical_text,
       method, path, operation, source, confidence, evidence_json, created_at
from section_entities
where document_id = ?
order by (
  select sections.ordinal from sections where sections.id = section_entities.section_id
) asc, kind asc, method asc, canonical_text asc, id asc
`, documentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSectionEntities(rows)
}

func (s *Store) ListSourceSectionEntities(ctx context.Context, sourceID string, limit, offset int) ([]domain.SectionEntity, error) {
	sourceID, limit, offset, err := normalizeSourceArtifactPage(sourceID, limit, offset)
	if err != nil {
		return nil, err
	}
	if _, err := s.GetSource(ctx, sourceID); err != nil {
		return nil, err
	}
	rows, err := s.readDB().QueryContext(ctx, `
select se.id, se.section_id, se.document_id, se.kind, se.raw_text, se.canonical_text,
       se.method, se.path, se.operation, se.source, se.confidence, se.evidence_json, se.created_at
from section_entities se
join documents d on d.id = se.document_id
left join sections s on s.id = se.section_id
where d.source_id = ?
order by d.indexed_at desc, d.title asc, coalesce(s.ordinal, 0) asc, se.kind asc, se.method asc, se.canonical_text asc, se.id asc
limit ? offset ?
`, sourceID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSectionEntities(rows)
}

func (s *Store) SearchEntities(ctx context.Context, query string, limit int) ([]domain.SectionEntity, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	seen := map[string]bool{}
	results := make([]domain.SectionEntity, 0, limit)
	add := func(entities []domain.SectionEntity) {
		for _, entity := range entities {
			if len(results) >= limit {
				return
			}
			if seen[entity.ID] {
				continue
			}
			seen[entity.ID] = true
			results = append(results, entity)
		}
	}

	exactTerms := append([]string{query}, entitySearchTerms(extract.ExtractSection(extract.SectionInput{Content: query}))...)
	for _, term := range uniqueStrings(nonEmptyStrings(exactTerms)) {
		entities, err := s.searchEntitiesExact(ctx, term, limit-len(results))
		if err != nil {
			return nil, err
		}
		add(entities)
		if len(results) >= limit {
			return results, nil
		}
	}

	entities, err := s.searchEntitiesFTS(ctx, matchQueryFromTerms([]string{query}, 1), false, limit-len(results))
	if err != nil {
		return nil, err
	}
	add(entities)
	if len(results) < limit {
		entities, err = s.searchEntitiesFTS(ctx, trigramFTSQuery(query), true, limit-len(results))
		if err != nil {
			return nil, err
		}
		add(entities)
	}
	return results, nil
}

func (s *Store) searchEntitiesExact(ctx context.Context, term string, limit int) ([]domain.SectionEntity, error) {
	term = strings.TrimSpace(term)
	if term == "" || limit <= 0 {
		return nil, nil
	}
	rows, err := s.readDB().QueryContext(ctx, `
select id, section_id, document_id, kind, raw_text, canonical_text,
       method, path, operation, source, confidence, evidence_json, created_at
from section_entities
where canonical_text = ? collate nocase
   or path = ? collate nocase
   or (method || ' ' || path) = ? collate nocase
   or operation = ? collate nocase
order by confidence desc, kind asc, canonical_text asc, id asc
limit ?
`, term, term, term, term, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSectionEntities(rows)
}

func (s *Store) searchEntitiesFTS(ctx context.Context, matchQuery string, trigram bool, limit int) ([]domain.SectionEntity, error) {
	if limit <= 0 {
		return nil, nil
	}
	table := "fts_section_entities"
	if trigram {
		table = "fts_section_entities_trigram"
	}
	query := `
select se.id, se.section_id, se.document_id, se.kind, se.raw_text, se.canonical_text,
       se.method, se.path, se.operation, se.source, se.confidence, se.evidence_json, se.created_at
from ` + table + ` f
join section_entities se on se.id = f.entity_id
where ` + table + ` match ?
order by se.confidence desc, se.kind asc, se.canonical_text asc, se.id asc
limit ?
`
	rows, err := s.readDB().QueryContext(ctx, query, matchQuery, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSectionEntities(rows)
}

func insertSectionEntitySearchIndex(ctx context.Context, exec searchIndexExecer, entity domain.SectionEntity) error {
	for _, table := range []string{"fts_section_entities", "fts_section_entities_trigram"} {
		if _, err := exec.ExecContext(ctx, "insert into "+table+" (canonical_text, raw_text, path, operation, entity_id) values (?, ?, ?, ?, ?)", entity.CanonicalText, entity.RawText, entity.Path, entity.Operation, entity.ID); err != nil {
			return fmt.Errorf("insert %s for entity %s: %w", table, entity.ID, err)
		}
	}
	return nil
}

func scanSectionEntities(rows *sql.Rows) ([]domain.SectionEntity, error) {
	entities := make([]domain.SectionEntity, 0)
	for rows.Next() {
		var entity domain.SectionEntity
		if err := rows.Scan(
			&entity.ID,
			&entity.SectionID,
			&entity.DocumentID,
			&entity.Kind,
			&entity.RawText,
			&entity.CanonicalText,
			&entity.Method,
			&entity.Path,
			&entity.Operation,
			&entity.Source,
			&entity.Confidence,
			&entity.EvidenceJSON,
			&entity.CreatedAt,
		); err != nil {
			return nil, err
		}
		entities = append(entities, entity)
	}
	return entities, rows.Err()
}

func (s *Store) documentSectionIDSet(ctx context.Context, documentID string) (map[string]bool, error) {
	rows, err := s.readDB().QueryContext(ctx, `select id from sections where document_id = ?`, documentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sectionIDs := map[string]bool{}
	for rows.Next() {
		var sectionID string
		if err := rows.Scan(&sectionID); err != nil {
			return nil, err
		}
		sectionIDs[sectionID] = true
	}
	return sectionIDs, rows.Err()
}

func aggregateSectionEntityInputs(documentID string, sectionIDs map[string]bool, inputs []domain.SectionEntityInput) ([]domain.SectionEntity, error) {
	type aggregate struct {
		entity    domain.SectionEntity
		evidence  sectionEntityEvidence
		notesSeen map[string]bool
	}
	byID := map[string]*aggregate{}
	for _, input := range inputs {
		normalized, err := normalizeSectionEntityInput(documentID, sectionIDs, input)
		if err != nil {
			return nil, err
		}
		if normalized.Kind == "" {
			continue
		}
		id := ids.Stable("entity", normalized.SectionID, normalized.Kind, normalized.Method, normalized.CanonicalText)
		item := byID[id]
		if item == nil {
			normalized.ID = id
			normalized.EvidenceJSON = "{}"
			item = &aggregate{
				entity:    normalized,
				notesSeen: map[string]bool{},
			}
			byID[id] = item
		}
		if normalized.Confidence > item.entity.Confidence {
			item.entity.Confidence = normalized.Confidence
			if normalized.RawText != "" {
				item.entity.RawText = normalized.RawText
			}
			if normalized.Source != "" {
				item.entity.Source = normalized.Source
			}
		}
		if item.entity.RawText == "" && normalized.RawText != "" {
			item.entity.RawText = normalized.RawText
		}
		if item.entity.Source == "" && normalized.Source != "" {
			item.entity.Source = normalized.Source
		}
		if item.entity.Operation == "" && normalized.Operation != "" {
			item.entity.Operation = normalized.Operation
		}
		item.evidence.Occurrences = append(item.evidence.Occurrences, sectionEntityOccurrence{
			Raw:       normalized.RawText,
			Source:    normalized.Source,
			SpanStart: input.SpanStart,
			SpanEnd:   input.SpanEnd,
			Evidence:  strings.TrimSpace(input.Evidence),
		})
		for _, note := range input.Notes {
			note = strings.TrimSpace(note)
			if note == "" || item.notesSeen[note] {
				continue
			}
			item.notesSeen[note] = true
			item.evidence.Notes = append(item.evidence.Notes, note)
		}
	}

	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]domain.SectionEntity, 0, len(ids))
	for _, id := range ids {
		item := byID[id]
		data, err := json.Marshal(item.evidence)
		if err != nil {
			return nil, err
		}
		item.entity.EvidenceJSON = string(data)
		result = append(result, item.entity)
	}
	return result, nil
}

func normalizeSectionEntityInput(documentID string, sectionIDs map[string]bool, input domain.SectionEntityInput) (domain.SectionEntity, error) {
	sectionID := strings.TrimSpace(input.SectionID)
	if sectionID == "" {
		return domain.SectionEntity{}, fmt.Errorf("section id is required")
	}
	if !sectionIDs[sectionID] {
		return domain.SectionEntity{}, fmt.Errorf("section %s does not belong to document %s", sectionID, documentID)
	}
	kind := strings.TrimSpace(input.Kind)
	canonical := strings.TrimSpace(input.CanonicalText)
	method := strings.ToUpper(strings.TrimSpace(input.Method))
	path := strings.TrimSpace(input.Path)
	operation := strings.TrimSpace(input.Operation)
	if canonical == "" {
		switch {
		case method != "" && path != "":
			canonical = method + " " + path
		case path != "":
			canonical = path
		default:
			canonical = operation
		}
	}
	if kind == "" || canonical == "" {
		return domain.SectionEntity{}, nil
	}
	return domain.SectionEntity{
		SectionID:     sectionID,
		DocumentID:    documentID,
		Kind:          kind,
		RawText:       strings.TrimSpace(input.RawText),
		CanonicalText: canonical,
		Method:        method,
		Path:          path,
		Operation:     operation,
		Source:        strings.TrimSpace(input.Source),
		Confidence:    input.Confidence,
	}, nil
}

func (s *Store) CreateKnowledgeRelationProposal(ctx context.Context, input domain.KnowledgeRelationProposalInput) (domain.KnowledgeRelationProposal, error) {
	input = normalizeKnowledgeRelationProposalInput(input)
	if input.ID == "" {
		input.ID = ids.Random("krp", 12)
	}
	if err := validateKnowledgeRelationProposalInput(ctx, s, input); err != nil {
		return domain.KnowledgeRelationProposal{}, err
	}
	_, err := s.db.ExecContext(ctx, `
insert into knowledge_relation_proposals (
  id, relation_type, from_document_id, from_anchor, to_document_id, to_anchor,
  direction, reason, evidence_json, proposed_effect, confidence, created_by_type, created_by_ref
) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, input.ID, input.RelationType, input.FromDocumentID, input.FromAnchor, input.ToDocumentID, input.ToAnchor,
		input.Direction, input.Reason, input.EvidenceJSON, input.ProposedEffect, input.Confidence, input.CreatedByType, input.CreatedByRef)
	if err != nil {
		return domain.KnowledgeRelationProposal{}, err
	}
	return s.GetKnowledgeRelationProposal(ctx, input.ID)
}

func (s *Store) ListKnowledgeRelationProposals(ctx context.Context, opts domain.KnowledgeRelationProposalListOptions) ([]domain.KnowledgeRelationProposal, error) {
	opts.Status = strings.TrimSpace(opts.Status)
	opts.DocumentID = strings.TrimSpace(opts.DocumentID)
	if opts.Limit <= 0 || opts.Limit > 200 {
		opts.Limit = 50
	}
	if opts.Offset < 0 {
		opts.Offset = 0
	}
	where := []string{"1 = 1"}
	args := make([]any, 0, 4)
	if opts.Status != "" && opts.Status != "all" {
		where = append(where, "status = ?")
		args = append(args, opts.Status)
	}
	if opts.DocumentID != "" {
		where = append(where, "(from_document_id = ? or to_document_id = ?)")
		args = append(args, opts.DocumentID, opts.DocumentID)
	}
	args = append(args, opts.Limit, opts.Offset)
	rows, err := s.readDB().QueryContext(ctx, `
select id, relation_type, from_document_id, from_anchor, to_document_id, to_anchor,
       direction, reason, evidence_json, proposed_effect, confidence, created_by_type, created_by_ref,
       status, reviewed_by, review_note, reviewed_at, created_at, updated_at
from knowledge_relation_proposals
where `+strings.Join(where, " and ")+`
order by created_at desc, id desc
limit ? offset ?
`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanKnowledgeRelationProposals(rows)
}

func (s *Store) GetKnowledgeRelationProposal(ctx context.Context, id string) (domain.KnowledgeRelationProposal, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return domain.KnowledgeRelationProposal{}, fmt.Errorf("proposal id is required")
	}
	row := s.readDB().QueryRowContext(ctx, `
select id, relation_type, from_document_id, from_anchor, to_document_id, to_anchor,
       direction, reason, evidence_json, proposed_effect, confidence, created_by_type, created_by_ref,
       status, reviewed_by, review_note, reviewed_at, created_at, updated_at
from knowledge_relation_proposals
where id = ?
`, id)
	return scanKnowledgeRelationProposal(row)
}

func (s *Store) ApproveKnowledgeRelationProposal(ctx context.Context, id string, reviewedBy string, reviewNote string) (domain.KnowledgeRelation, error) {
	id = strings.TrimSpace(id)
	reviewedBy = strings.TrimSpace(reviewedBy)
	reviewNote = strings.TrimSpace(reviewNote)
	if id == "" {
		return domain.KnowledgeRelation{}, fmt.Errorf("proposal id is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.KnowledgeRelation{}, err
	}
	defer tx.Rollback()

	proposal, err := getKnowledgeRelationProposalTx(ctx, tx, id)
	if err != nil {
		return domain.KnowledgeRelation{}, err
	}
	if proposal.Status != "pending" {
		return domain.KnowledgeRelation{}, fmt.Errorf("proposal %q is %s", id, proposal.Status)
	}
	relationID := ids.Random("kr", 12)
	_, err = tx.ExecContext(ctx, `
update knowledge_relation_proposals
set status = 'approved',
    reviewed_by = ?,
    review_note = ?,
    reviewed_at = current_timestamp,
    updated_at = current_timestamp
where id = ? and status = 'pending'
`, reviewedBy, reviewNote, id)
	if err != nil {
		return domain.KnowledgeRelation{}, err
	}
	_, err = tx.ExecContext(ctx, `
insert into knowledge_relations (
  id, relation_type, from_document_id, from_anchor, to_document_id, to_anchor,
  direction, effect, weight, reason, evidence_json, approved_from_proposal_id, created_by
) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, relationID, proposal.RelationType, proposal.FromDocumentID, proposal.FromAnchor, proposal.ToDocumentID, proposal.ToAnchor,
		proposal.Direction, proposal.ProposedEffect, relationWeightForEffect(proposal.ProposedEffect), proposal.Reason, proposal.EvidenceJSON, proposal.ID, reviewedBy)
	if err != nil {
		return domain.KnowledgeRelation{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.KnowledgeRelation{}, err
	}
	return s.getKnowledgeRelation(ctx, relationID)
}

func (s *Store) RejectKnowledgeRelationProposal(ctx context.Context, id string, reviewedBy string, reviewNote string) (domain.KnowledgeRelationProposal, error) {
	return s.updateKnowledgeRelationProposalStatus(ctx, id, "rejected", reviewedBy, reviewNote)
}

func (s *Store) CancelKnowledgeRelationProposal(ctx context.Context, id string, reviewedBy string, reviewNote string) (domain.KnowledgeRelationProposal, error) {
	return s.updateKnowledgeRelationProposalStatus(ctx, id, "cancelled", reviewedBy, reviewNote)
}

func (s *Store) updateKnowledgeRelationProposalStatus(ctx context.Context, id string, status string, reviewedBy string, reviewNote string) (domain.KnowledgeRelationProposal, error) {
	id = strings.TrimSpace(id)
	reviewedBy = strings.TrimSpace(reviewedBy)
	reviewNote = strings.TrimSpace(reviewNote)
	if id == "" {
		return domain.KnowledgeRelationProposal{}, fmt.Errorf("proposal id is required")
	}
	result, err := s.db.ExecContext(ctx, `
update knowledge_relation_proposals
set status = ?,
    reviewed_by = ?,
    review_note = ?,
    reviewed_at = current_timestamp,
    updated_at = current_timestamp
where id = ? and status = 'pending'
`, status, reviewedBy, reviewNote, id)
	if err != nil {
		return domain.KnowledgeRelationProposal{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return domain.KnowledgeRelationProposal{}, err
	}
	if changed == 0 {
		proposal, getErr := s.GetKnowledgeRelationProposal(ctx, id)
		if getErr != nil {
			return domain.KnowledgeRelationProposal{}, getErr
		}
		return domain.KnowledgeRelationProposal{}, fmt.Errorf("proposal %q is %s", id, proposal.Status)
	}
	return s.GetKnowledgeRelationProposal(ctx, id)
}

func (s *Store) ListKnowledgeRelations(ctx context.Context, opts domain.KnowledgeRelationListOptions) ([]domain.KnowledgeRelation, error) {
	opts.DocumentID = strings.TrimSpace(opts.DocumentID)
	if opts.Limit <= 0 || opts.Limit > 200 {
		opts.Limit = 50
	}
	where := []string{"1 = 1"}
	args := make([]any, 0, 6)
	if opts.DocumentID != "" {
		where = append(where, "(from_document_id = ? or to_document_id = ?)")
		args = append(args, opts.DocumentID, opts.DocumentID)
	}
	if len(opts.RelationTypes) > 0 {
		placeholders := make([]string, 0, len(opts.RelationTypes))
		for _, relationType := range opts.RelationTypes {
			relationType = strings.TrimSpace(relationType)
			if relationType == "" {
				continue
			}
			placeholders = append(placeholders, "?")
			args = append(args, relationType)
		}
		if len(placeholders) > 0 {
			where = append(where, "relation_type in ("+strings.Join(placeholders, ",")+")")
		}
	}
	if !opts.IncludeDisabled {
		where = append(where, "disabled_at = ''")
	}
	args = append(args, opts.Limit)
	rows, err := s.readDB().QueryContext(ctx, `
select id, relation_type, from_document_id, from_anchor, to_document_id, to_anchor,
       direction, effect, weight, reason, evidence_json, coalesce(approved_from_proposal_id, ''),
       created_by, disabled_at, created_at, updated_at
from knowledge_relations
where `+strings.Join(where, " and ")+`
order by created_at desc, id desc
limit ?
`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanKnowledgeRelations(rows)
}

func (s *Store) DisableKnowledgeRelation(ctx context.Context, id string, reviewedBy string, note string) (domain.KnowledgeRelation, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return domain.KnowledgeRelation{}, fmt.Errorf("relation id is required")
	}
	result, err := s.db.ExecContext(ctx, `
update knowledge_relations
set disabled_at = current_timestamp,
    updated_at = current_timestamp
where id = ? and disabled_at = ''
`, id)
	if err != nil {
		return domain.KnowledgeRelation{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return domain.KnowledgeRelation{}, err
	}
	if changed == 0 {
		if _, err := s.getKnowledgeRelation(ctx, id); err != nil {
			return domain.KnowledgeRelation{}, err
		}
	}
	return s.getKnowledgeRelation(ctx, id)
}

func (s *Store) ReenableKnowledgeRelation(ctx context.Context, id string, reviewedBy string, note string) (domain.KnowledgeRelation, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return domain.KnowledgeRelation{}, fmt.Errorf("relation id is required")
	}
	_, err := s.db.ExecContext(ctx, `
update knowledge_relations
set disabled_at = '',
    updated_at = current_timestamp
where id = ?
`, id)
	if err != nil {
		return domain.KnowledgeRelation{}, err
	}
	return s.getKnowledgeRelation(ctx, id)
}

func (s *Store) getKnowledgeRelation(ctx context.Context, id string) (domain.KnowledgeRelation, error) {
	row := s.readDB().QueryRowContext(ctx, `
select id, relation_type, from_document_id, from_anchor, to_document_id, to_anchor,
       direction, effect, weight, reason, evidence_json, coalesce(approved_from_proposal_id, ''),
       created_by, disabled_at, created_at, updated_at
from knowledge_relations
where id = ?
`, id)
	return scanKnowledgeRelation(row)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func normalizeKnowledgeRelationProposalInput(input domain.KnowledgeRelationProposalInput) domain.KnowledgeRelationProposalInput {
	input.ID = strings.TrimSpace(input.ID)
	input.RelationType = strings.TrimSpace(input.RelationType)
	input.FromDocumentID = strings.TrimSpace(input.FromDocumentID)
	input.FromAnchor = strings.TrimSpace(input.FromAnchor)
	input.ToDocumentID = strings.TrimSpace(input.ToDocumentID)
	input.ToAnchor = strings.TrimSpace(input.ToAnchor)
	input.Direction = strings.TrimSpace(input.Direction)
	input.Reason = strings.TrimSpace(input.Reason)
	input.EvidenceJSON = defaultJSONObject(input.EvidenceJSON)
	input.ProposedEffect = strings.TrimSpace(input.ProposedEffect)
	input.CreatedByType = strings.TrimSpace(input.CreatedByType)
	input.CreatedByRef = strings.TrimSpace(input.CreatedByRef)
	if input.Direction == "" {
		input.Direction = "directed"
	}
	if input.ProposedEffect == "" {
		input.ProposedEffect = effectForRelationType(input.RelationType)
	}
	if input.Confidence < 0 {
		input.Confidence = 0
	}
	if input.Confidence > 1 {
		input.Confidence = 1
	}
	return input
}

func validateKnowledgeRelationProposalInput(ctx context.Context, s *Store, input domain.KnowledgeRelationProposalInput) error {
	if input.RelationType == "" {
		return fmt.Errorf("relation_type is required")
	}
	if !allowedKnowledgeRelationType(input.RelationType) {
		return fmt.Errorf("unsupported relation_type %q", input.RelationType)
	}
	if input.FromDocumentID == "" {
		return fmt.Errorf("from_document_id is required")
	}
	if input.ToDocumentID == "" {
		return fmt.Errorf("to_document_id is required")
	}
	if input.FromDocumentID == input.ToDocumentID {
		return fmt.Errorf("from_document_id and to_document_id must differ")
	}
	if input.Direction != "directed" && input.Direction != "undirected" {
		return fmt.Errorf("direction must be directed or undirected")
	}
	if input.Reason == "" {
		return fmt.Errorf("reason is required")
	}
	if !allowedKnowledgeRelationEffect(input.ProposedEffect) {
		return fmt.Errorf("unsupported proposed_effect %q", input.ProposedEffect)
	}
	if !json.Valid([]byte(input.EvidenceJSON)) {
		return fmt.Errorf("evidence_json must be valid JSON")
	}
	if err := s.ensureDocumentExists(ctx, input.FromDocumentID); err != nil {
		return fmt.Errorf("from_document_id %q not found: %w", input.FromDocumentID, err)
	}
	if err := s.ensureDocumentExists(ctx, input.ToDocumentID); err != nil {
		return fmt.Errorf("to_document_id %q not found: %w", input.ToDocumentID, err)
	}
	return nil
}

func allowedKnowledgeRelationType(value string) bool {
	switch value {
	case "related_to", "schema_reference", "deprecated_by", "should_ignore":
		return true
	default:
		return false
	}
}

func allowedKnowledgeRelationEffect(value string) bool {
	switch value {
	case "context_link", "boost", "demote", "ignore":
		return true
	default:
		return false
	}
}

func effectForRelationType(relationType string) string {
	switch relationType {
	case "deprecated_by":
		return "demote"
	case "should_ignore":
		return "ignore"
	case "related_to":
		return "boost"
	default:
		return "context_link"
	}
}

func relationWeightForEffect(effect string) float64 {
	switch effect {
	case "boost":
		return 0.5
	case "demote":
		return -0.8
	case "ignore":
		return -1
	default:
		return 1
	}
}

func scanKnowledgeRelationProposal(row rowScanner) (domain.KnowledgeRelationProposal, error) {
	var proposal domain.KnowledgeRelationProposal
	err := row.Scan(&proposal.ID, &proposal.RelationType, &proposal.FromDocumentID, &proposal.FromAnchor, &proposal.ToDocumentID, &proposal.ToAnchor,
		&proposal.Direction, &proposal.Reason, &proposal.EvidenceJSON, &proposal.ProposedEffect, &proposal.Confidence, &proposal.CreatedByType, &proposal.CreatedByRef,
		&proposal.Status, &proposal.ReviewedBy, &proposal.ReviewNote, &proposal.ReviewedAt, &proposal.CreatedAt, &proposal.UpdatedAt)
	if err != nil {
		return domain.KnowledgeRelationProposal{}, err
	}
	return proposal, nil
}

func scanKnowledgeRelationProposals(rows *sql.Rows) ([]domain.KnowledgeRelationProposal, error) {
	proposals := make([]domain.KnowledgeRelationProposal, 0)
	for rows.Next() {
		proposal, err := scanKnowledgeRelationProposal(rows)
		if err != nil {
			return nil, err
		}
		proposals = append(proposals, proposal)
	}
	return proposals, rows.Err()
}

func getKnowledgeRelationProposalTx(ctx context.Context, tx *sql.Tx, id string) (domain.KnowledgeRelationProposal, error) {
	row := tx.QueryRowContext(ctx, `
select id, relation_type, from_document_id, from_anchor, to_document_id, to_anchor,
       direction, reason, evidence_json, proposed_effect, confidence, created_by_type, created_by_ref,
       status, reviewed_by, review_note, reviewed_at, created_at, updated_at
from knowledge_relation_proposals
where id = ?
`, id)
	return scanKnowledgeRelationProposal(row)
}

func scanKnowledgeRelation(row rowScanner) (domain.KnowledgeRelation, error) {
	var relation domain.KnowledgeRelation
	err := row.Scan(&relation.ID, &relation.RelationType, &relation.FromDocumentID, &relation.FromAnchor, &relation.ToDocumentID, &relation.ToAnchor,
		&relation.Direction, &relation.Effect, &relation.Weight, &relation.Reason, &relation.EvidenceJSON, &relation.ApprovedFromProposalID,
		&relation.CreatedBy, &relation.DisabledAt, &relation.CreatedAt, &relation.UpdatedAt)
	if err != nil {
		return domain.KnowledgeRelation{}, err
	}
	return relation, nil
}

func scanKnowledgeRelations(rows *sql.Rows) ([]domain.KnowledgeRelation, error) {
	relations := make([]domain.KnowledgeRelation, 0)
	for rows.Next() {
		relation, err := scanKnowledgeRelation(rows)
		if err != nil {
			return nil, err
		}
		relations = append(relations, relation)
	}
	return relations, rows.Err()
}

func (s *Store) CreateJob(ctx context.Context, input domain.JobInput) (domain.Job, error) {
	input.Kind = strings.TrimSpace(input.Kind)
	input.SourceID = strings.TrimSpace(input.SourceID)
	input.TargetKind = strings.TrimSpace(input.TargetKind)
	input.TargetID = strings.TrimSpace(input.TargetID)
	input.PayloadJSON = defaultJSONObject(input.PayloadJSON)
	input.ProgressJSON = defaultJSONObject(input.ProgressJSON)
	input.ResultJSON = defaultJSONObject(input.ResultJSON)
	input.RunAfter = strings.TrimSpace(input.RunAfter)
	if input.Kind == "" {
		return domain.Job{}, fmt.Errorf("job kind is required")
	}
	if input.Kind == "sync_source" && input.SourceID == "" {
		return domain.Job{}, fmt.Errorf("source id is required for sync_source jobs")
	}
	jobID := ids.Random("job", 12)
	if input.RunAfter == "" {
		_, err := s.db.ExecContext(ctx, `
insert into jobs (id, kind, status, source_id, target_kind, target_id, payload_json, progress_json, result_json)
values (?, ?, 'queued', ?, ?, ?, ?, ?, ?)
`, jobID, input.Kind, input.SourceID, input.TargetKind, input.TargetID, input.PayloadJSON, input.ProgressJSON, input.ResultJSON)
		if err != nil {
			return domain.Job{}, err
		}
	} else {
		_, err := s.db.ExecContext(ctx, `
insert into jobs (id, kind, status, source_id, target_kind, target_id, payload_json, progress_json, result_json, run_after)
values (?, ?, 'queued', ?, ?, ?, ?, ?, ?, ?)
`, jobID, input.Kind, input.SourceID, input.TargetKind, input.TargetID, input.PayloadJSON, input.ProgressJSON, input.ResultJSON, input.RunAfter)
		if err != nil {
			return domain.Job{}, err
		}
	}
	return s.GetJob(ctx, jobID)
}

func (s *Store) ClaimDueJob(ctx context.Context, workerID string, kinds []string, lease time.Duration) (domain.Job, error) {
	workerID = strings.TrimSpace(workerID)
	if workerID == "" {
		return domain.Job{}, fmt.Errorf("worker id is required")
	}
	kinds = normalizeKinds(kinds)
	if len(kinds) == 0 {
		return domain.Job{}, fmt.Errorf("at least one job kind is required")
	}
	if lease <= 0 {
		lease = time.Minute
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Job{}, err
	}
	defer tx.Rollback()

	placeholders := strings.TrimRight(strings.Repeat("?,", len(kinds)), ",")
	args := make([]any, 0, len(kinds))
	for _, kind := range kinds {
		args = append(args, kind)
	}
	var id string
	query := fmt.Sprintf(`
select id
from jobs
where kind in (%s)
  and (
    (status = 'queued' and run_after <= current_timestamp)
    or (status = 'running' and locked_until is not null and locked_until <= current_timestamp)
  )
order by run_after asc, rowid asc
limit 1
`, placeholders)
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&id); err != nil {
		return domain.Job{}, err
	}
	leaseUntil := time.Now().UTC().Add(lease).Format("2006-01-02 15:04:05")
	result, err := tx.ExecContext(ctx, `
update jobs
set status = 'running',
    attempts = attempts + 1,
    worker_id = ?,
    locked_until = ?,
    last_error = '',
    updated_at = current_timestamp
where id = ?
  and (
    status = 'queued'
    or (status = 'running' and locked_until is not null and locked_until <= current_timestamp)
  )
`, workerID, leaseUntil, id)
	if err != nil {
		return domain.Job{}, err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return domain.Job{}, err
	}
	if updated == 0 {
		return domain.Job{}, sql.ErrNoRows
	}
	if err := tx.Commit(); err != nil {
		return domain.Job{}, err
	}
	return s.GetJob(ctx, id)
}

func (s *Store) UpdateJobProgress(ctx context.Context, id string, progressJSON string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("job id is required")
	}
	progressJSON = defaultJSONObject(progressJSON)
	result, err := s.db.ExecContext(ctx, `
update jobs
set progress_json = ?,
    updated_at = current_timestamp
where id = ?
`, progressJSON, id)
	if err != nil {
		return err
	}
	return requireRowsAffected(result)
}

func (s *Store) CompleteJob(ctx context.Context, id string, resultJSON string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("job id is required")
	}
	resultJSON = defaultJSONObject(resultJSON)
	result, err := s.db.ExecContext(ctx, `
update jobs
set status = 'completed',
    result_json = ?,
    worker_id = '',
    locked_until = null,
    last_error = '',
    updated_at = current_timestamp
where id = ?
  and status not in ('canceled', 'canceling')
`, resultJSON, id)
	if err != nil {
		return err
	}
	return requireRowsAffected(result)
}

func (s *Store) FailJob(ctx context.Context, id string, errText string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("job id is required")
	}
	result, err := s.db.ExecContext(ctx, `
update jobs
set status = 'failed',
    worker_id = '',
    locked_until = null,
    last_error = ?,
    updated_at = current_timestamp
where id = ?
  and status not in ('canceled', 'canceling')
`, strings.TrimSpace(errText), id)
	if err != nil {
		return err
	}
	return requireRowsAffected(result)
}

func (s *Store) CancelJob(ctx context.Context, id string, reason string) (domain.Job, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return domain.Job{}, fmt.Errorf("job id is required")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "canceled by user"
	}
	job, err := s.GetJob(ctx, id)
	if err != nil {
		return domain.Job{}, err
	}
	switch job.Status {
	case "queued":
		result, err := s.db.ExecContext(ctx, `
update jobs
set status = 'canceled',
    worker_id = '',
    locked_until = null,
    last_error = ?,
    updated_at = current_timestamp
where id = ?
  and status = 'queued'
`, reason, id)
		if err != nil {
			return domain.Job{}, err
		}
		if err := requireRowsAffected(result); err != nil {
			return domain.Job{}, err
		}
		return s.GetJob(ctx, id)
	case "running":
		result, err := s.db.ExecContext(ctx, `
update jobs
set status = 'canceling',
    last_error = ?,
    updated_at = current_timestamp
where id = ?
  and status = 'running'
`, reason, id)
		if err != nil {
			return domain.Job{}, err
		}
		if err := requireRowsAffected(result); err != nil {
			return domain.Job{}, err
		}
		return s.GetJob(ctx, id)
	case "canceling", "canceled":
		return job, nil
	default:
		return job, domain.ErrJobNotCancelable
	}
}

func (s *Store) MarkJobCanceled(ctx context.Context, id string, reason string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("job id is required")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "canceled"
	}
	result, err := s.db.ExecContext(ctx, `
update jobs
set status = 'canceled',
    worker_id = '',
    locked_until = null,
    last_error = ?,
    updated_at = current_timestamp
where id = ?
  and status in ('running', 'canceling')
`, reason, id)
	if err != nil {
		return err
	}
	return requireRowsAffected(result)
}

func (s *Store) ListJobs(ctx context.Context, opts domain.JobListOptions) ([]domain.Job, error) {
	if opts.Limit <= 0 || opts.Limit > 100 {
		opts.Limit = 20
	}
	if opts.Offset < 0 {
		opts.Offset = 0
	}
	where, args := jobWhere(opts)
	args = append(args, opts.Limit, opts.Offset)
	rows, err := s.readDB().QueryContext(ctx, `
select id, kind, status, source_id, target_kind, target_id, payload_json, progress_json, result_json, worker_id, attempts, run_after, coalesce(locked_until, ''), last_error, created_at, updated_at
from jobs
where `+where+`
order by rowid desc
limit ? offset ?
`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanJobs(rows)
}

func (s *Store) CountJobs(ctx context.Context, opts domain.JobListOptions) (int, error) {
	where, args := jobWhere(opts)
	var count int
	if err := s.readDB().QueryRowContext(ctx, `select count(*) from jobs where `+where, args...).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) GetJob(ctx context.Context, id string) (domain.Job, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return domain.Job{}, fmt.Errorf("job id is required")
	}
	var job domain.Job
	err := s.readDB().QueryRowContext(ctx, `
select id, kind, status, source_id, target_kind, target_id, payload_json, progress_json, result_json, worker_id, attempts, run_after, coalesce(locked_until, ''), last_error, created_at, updated_at
from jobs
where id = ?
`, id).Scan(&job.ID, &job.Kind, &job.Status, &job.SourceID, &job.TargetKind, &job.TargetID, &job.PayloadJSON, &job.ProgressJSON, &job.ResultJSON, &job.WorkerID, &job.Attempts, &job.RunAfter, &job.LockedUntil, &job.LastError, &job.CreatedAt, &job.UpdatedAt)
	if err != nil {
		return domain.Job{}, err
	}
	return job, nil
}

func jobWhere(opts domain.JobListOptions) (string, []any) {
	clauses := []string{"1 = 1"}
	args := []any{}
	if value := strings.TrimSpace(opts.SourceID); value != "" {
		clauses = append(clauses, "source_id = ?")
		args = append(args, value)
	}
	if value := strings.TrimSpace(opts.Kind); value != "" {
		clauses = append(clauses, "kind = ?")
		args = append(args, value)
	}
	if value := strings.TrimSpace(opts.Status); value != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, value)
	}
	if value := strings.TrimSpace(opts.TargetKind); value != "" {
		clauses = append(clauses, "target_kind = ?")
		args = append(args, value)
	}
	if value := strings.TrimSpace(opts.TargetID); value != "" {
		clauses = append(clauses, "target_id = ?")
		args = append(args, value)
	}
	return strings.Join(clauses, " and "), args
}

func (s *Store) CreateEmbeddingEnsureJobIfIdle(ctx context.Context, sourceID string) (domain.Job, error) {
	sourceID = strings.TrimSpace(sourceID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Job{}, err
	}
	defer tx.Rollback()

	var running string
	args := []any{}
	scopeClause := "1 = 1"
	if sourceID != "" {
		scopeClause = "(source_id = '' or source_id = ?)"
		args = append(args, sourceID)
	}
	query := `
select id
from jobs
where kind = 'maintenance_embedding_ensure'
  and status in ('queued', 'running', 'canceling')
  and ` + scopeClause + `
order by rowid desc
limit 1
`
	err = tx.QueryRowContext(ctx, query, args...).Scan(&running)
	if err == nil {
		return domain.Job{}, domain.ErrSyncInProgress
	}
	if err != sql.ErrNoRows {
		return domain.Job{}, err
	}

	jobID := ids.Random("job", 12)
	targetKind := "embedding"
	payload := `{"mode":"ensure"}`
	if sourceID != "" {
		targetKind = "source"
		payload = fmt.Sprintf(`{"source_id":%q,"mode":"ensure"}`, sourceID)
	}
	if _, err := tx.ExecContext(ctx, `
insert into jobs (id, kind, status, source_id, target_kind, target_id, payload_json, progress_json, result_json)
values (?, 'maintenance_embedding_ensure', 'queued', ?, ?, ?, ?, '{}', '{}')
`, jobID, sourceID, targetKind, sourceID, payload); err != nil {
		return domain.Job{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Job{}, err
	}
	return s.GetJob(ctx, jobID)
}

func (s *Store) CreateSyncJob(ctx context.Context, sourceID string) (domain.SyncJob, error) {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return domain.SyncJob{}, fmt.Errorf("source id is required")
	}
	jobID := ids.Random("job", 12)
	payload := syncJobPayload(sourceID, domain.ResultPayload{})
	_, err := s.db.ExecContext(ctx, `
insert into jobs (id, kind, status, source_id, payload_json, progress_json, result_json, attempts)
values (?, 'sync_source', 'running', ?, ?, '{}', '{}', 1)
`, jobID, sourceID, payload)
	if err != nil {
		return domain.SyncJob{}, err
	}
	return s.getSyncJob(ctx, jobID)
}

func (s *Store) CreateSyncJobIfIdle(ctx context.Context, sourceID string) (domain.SyncJob, error) {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return domain.SyncJob{}, fmt.Errorf("source id is required")
	}
	jobID := ids.Random("job", 12)
	payload := syncJobPayload(sourceID, domain.ResultPayload{})
	result, err := s.db.ExecContext(ctx, `
insert into jobs (id, kind, status, source_id, payload_json, progress_json, result_json)
values (?, 'sync_source', 'queued', ?, ?, '{}', '{}')
	on conflict(source_id) where kind = 'sync_source' and status in ('queued', 'running', 'canceling')
do nothing
`, jobID, sourceID, payload)
	if err != nil {
		return domain.SyncJob{}, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return domain.SyncJob{}, err
	}
	if inserted == 0 {
		return domain.SyncJob{}, domain.ErrSyncInProgress
	}
	return s.getSyncJob(ctx, jobID)
}

func (s *Store) CompleteSyncJob(ctx context.Context, id string, resultPayload domain.ResultPayload) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("job id is required")
	}
	job, err := s.getSyncJob(ctx, id)
	if err != nil {
		return err
	}
	if job.Kind != "sync_source" {
		return fmt.Errorf("job %s is not a sync_source job", id)
	}
	sourceID := job.SourceID
	if sourceID == "" {
		return fmt.Errorf("sync_source job %s is missing source_id", id)
	}
	payload := syncJobPayload(sourceID, resultPayload)
	result, err := s.db.ExecContext(ctx, `
update jobs
set status = 'completed',
    source_id = ?,
    payload_json = ?,
    result_json = ?,
    worker_id = '',
    locked_until = null,
    last_error = '',
    updated_at = current_timestamp
where id = ?
  and status not in ('canceled', 'canceling')
`, sourceID, payload, payload, id)
	if err != nil {
		return fmt.Errorf("complete sync job %s: %w", id, err)
	}
	return requireRowsAffected(result)
}

func (s *Store) FailSyncJob(ctx context.Context, id string, errText string) error {
	return s.FailJob(ctx, id, errText)
}

func (s *Store) ListSyncJobs(ctx context.Context, sourceID string, limit int) ([]domain.SyncJob, error) {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return nil, fmt.Errorf("source id is required")
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.readDB().QueryContext(ctx, `
select id, kind, status, source_id, target_kind, target_id, payload_json, progress_json, result_json, worker_id, attempts, run_after, coalesce(locked_until, ''), last_error, created_at, updated_at
from jobs
where kind = 'sync_source'
  and source_id = ?
order by rowid desc
limit ?
`, sourceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanJobs(rows)
}

func (s *Store) ListLatestSyncJobs(ctx context.Context) ([]domain.SyncJob, error) {
	rows, err := s.readDB().QueryContext(ctx, `
select latest.id, latest.kind, latest.status, latest.source_id, latest.target_kind, latest.target_id,
       latest.payload_json, latest.progress_json, latest.result_json, latest.worker_id, latest.attempts,
       latest.run_after, coalesce(latest.locked_until, ''), latest.last_error, latest.created_at, latest.updated_at
from sources source
join jobs latest on latest.rowid = (
  select max(candidate.rowid)
  from jobs candidate
  where candidate.kind = 'sync_source'
    and candidate.source_id = source.id
)
order by source.id
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanJobs(rows)
}

func (s *Store) DeleteSyncJob(ctx context.Context, sourceID string, jobID string) error {
	sourceID = strings.TrimSpace(sourceID)
	jobID = strings.TrimSpace(jobID)
	if sourceID == "" {
		return fmt.Errorf("source id is required")
	}
	if jobID == "" {
		return fmt.Errorf("job id is required")
	}
	job, err := s.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if job.Kind != "sync_source" || job.SourceID != sourceID {
		return sql.ErrNoRows
	}
	if job.Status == "queued" || job.Status == "running" || job.Status == "canceling" {
		return domain.ErrJobNotCancelable
	}
	result, err := s.db.ExecContext(ctx, `
delete from jobs
where id = ?
  and kind = 'sync_source'
  and source_id = ?
`, jobID, sourceID)
	if err != nil {
		return err
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if deleted == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) getSyncJob(ctx context.Context, id string) (domain.SyncJob, error) {
	return s.GetJob(ctx, id)
}

func defaultJSONObject(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "{}"
	}
	if !json.Valid([]byte(value)) {
		return "{}"
	}
	return value
}

func normalizeKinds(kinds []string) []string {
	seen := map[string]bool{}
	var normalized []string
	for _, kind := range kinds {
		kind = strings.TrimSpace(kind)
		if kind == "" || seen[kind] {
			continue
		}
		seen[kind] = true
		normalized = append(normalized, kind)
	}
	return normalized
}

func requireRowsAffected(result sql.Result) error {
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func scanJobs(rows *sql.Rows) ([]domain.Job, error) {
	var jobs []domain.Job
	for rows.Next() {
		var job domain.Job
		if err := rows.Scan(&job.ID, &job.Kind, &job.Status, &job.SourceID, &job.TargetKind, &job.TargetID, &job.PayloadJSON, &job.ProgressJSON, &job.ResultJSON, &job.WorkerID, &job.Attempts, &job.RunAfter, &job.LockedUntil, &job.LastError, &job.CreatedAt, &job.UpdatedAt); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func syncJobPayload(sourceID string, result domain.ResultPayload) string {
	data, err := json.Marshal(struct {
		SourceID             string                       `json:"source_id"`
		Documents            int                          `json:"documents"`
		EntityDiagnostics    domain.EntityDiagnostics     `json:"entity_diagnostics,omitempty"`
		BrokenLinks          []domain.BrokenLink          `json:"broken_links,omitempty"`
		ConnectorDiagnostics *domain.ConnectorDiagnostics `json:"connector_diagnostics,omitempty"`
	}{
		SourceID:             sourceID,
		Documents:            result.Documents,
		EntityDiagnostics:    result.EntityDiagnostics,
		BrokenLinks:          result.BrokenLinks,
		ConnectorDiagnostics: result.ConnectorDiagnostics,
	})
	if err != nil {
		return "{}"
	}
	return string(data)
}

func normalizeSourceSyncState(status string, reason string, pausedAt string) (string, string, string) {
	status = strings.TrimSpace(status)
	reason = strings.TrimSpace(reason)
	pausedAt = strings.TrimSpace(pausedAt)
	if status == "" {
		status = "active"
	}
	if status != "active" && status != "paused" {
		status = "active"
		reason = ""
		pausedAt = ""
	}
	if status == "active" {
		reason = ""
		pausedAt = ""
	}
	if status == "paused" && reason == "" {
		reason = "credential_required"
	}
	return status, reason, pausedAt
}

func stableDocumentNodeID(documentID string) string {
	return stableArtifactID("node", "document", documentID)
}

func stableSectionNodeID(sectionID string) string {
	return stableArtifactID("node", "section", sectionID)
}

func stableArtifactID(prefix string, parts ...string) string {
	return ids.Stable(prefix, parts...)
}

func replaceNodeMembershipsTx(ctx context.Context, tx *sql.Tx, nodeID string) error {
	oldMetadataSources := make(map[string]bool)
	rows, err := tx.QueryContext(ctx, `
select source_id from source_nodes where node_id = ? and ref_mask & ? != 0
`, nodeID, nodeSourceRefMetadata)
	if err != nil {
		return err
	}
	for rows.Next() {
		var sourceID string
		if err := rows.Scan(&sourceID); err != nil {
			rows.Close()
			return err
		}
		oldMetadataSources[sourceID] = true
	}
	if err := rows.Close(); err != nil {
		return err
	}

	var ownerSourceID sql.NullString
	var metadataJSON string
	if err := tx.QueryRowContext(ctx, `select owner_source_id, metadata_json from nodes where id = ?`, nodeID).Scan(&ownerSourceID, &metadataJSON); err != nil {
		return err
	}
	var metadata struct {
		SourceID   string `json:"source_id"`
		DocumentID string `json:"document_id"`
	}
	if json.Valid([]byte(metadataJSON)) {
		_ = json.Unmarshal([]byte(metadataJSON), &metadata)
	}
	metadata.SourceID = strings.TrimSpace(metadata.SourceID)
	metadata.DocumentID = strings.TrimSpace(metadata.DocumentID)

	desiredSources := make(map[string]int)
	if ownerSourceID.Valid && strings.TrimSpace(ownerSourceID.String) != "" {
		desiredSources[strings.TrimSpace(ownerSourceID.String)] |= nodeSourceRefOwner
	}
	if metadata.SourceID != "" {
		var exists bool
		if err := tx.QueryRowContext(ctx, `select exists(select 1 from sources where id = ?)`, metadata.SourceID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			desiredSources[metadata.SourceID] |= nodeSourceRefMetadata
		}
	}
	if _, err := tx.ExecContext(ctx, `delete from source_nodes where node_id = ?`, nodeID); err != nil {
		return err
	}
	for sourceID, refMask := range desiredSources {
		if _, err := tx.ExecContext(ctx, `insert into source_nodes (source_id, node_id, ref_mask) values (?, ?, ?)`, sourceID, nodeID, refMask); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, `delete from document_nodes where node_id = ?`, nodeID); err != nil {
		return err
	}
	if metadata.DocumentID != "" {
		if _, err := tx.ExecContext(ctx, `
insert into document_nodes (document_id, node_id)
select id, ? from documents where id = ?
`, nodeID, metadata.DocumentID); err != nil {
			return err
		}
	}

	newMetadataSources := make(map[string]bool)
	for sourceID, refMask := range desiredSources {
		if refMask&nodeSourceRefMetadata != 0 {
			newMetadataSources[sourceID] = true
		}
	}
	if stringSetEqual(oldMetadataSources, newMetadataSources) {
		return nil
	}
	edgeRows, err := tx.QueryContext(ctx, `
select id from edges where src_id = ?
union
select id from edges where dst_id = ?
`, nodeID, nodeID)
	if err != nil {
		return err
	}
	var edgeIDs []string
	for edgeRows.Next() {
		var edgeID string
		if err := edgeRows.Scan(&edgeID); err != nil {
			edgeRows.Close()
			return err
		}
		edgeIDs = append(edgeIDs, edgeID)
	}
	if err := edgeRows.Close(); err != nil {
		return err
	}
	for _, edgeID := range edgeIDs {
		if err := replaceEdgeMembershipsTx(ctx, tx, edgeID); err != nil {
			return err
		}
	}
	return nil
}

func replaceEdgeMembershipsTx(ctx context.Context, tx *sql.Tx, edgeID string) error {
	var ownerSourceID sql.NullString
	var evidenceSourceID sql.NullString
	var srcID, dstID string
	err := tx.QueryRowContext(ctx, `
select e.owner_source_id,
       (select d.source_id from sections s join documents d on d.id = s.document_id where s.id = e.evidence_section_id),
       e.src_id, e.dst_id
from edges e
where e.id = ?
`, edgeID).Scan(&ownerSourceID, &evidenceSourceID, &srcID, &dstID)
	if errors.Is(err, sql.ErrNoRows) {
		_, deleteErr := tx.ExecContext(ctx, `delete from source_edges where edge_id = ?`, edgeID)
		return deleteErr
	}
	if err != nil {
		return err
	}

	desired := make(map[string]int)
	if ownerSourceID.Valid && strings.TrimSpace(ownerSourceID.String) != "" {
		desired[strings.TrimSpace(ownerSourceID.String)] |= edgeSourceRefOwner
	}
	if evidenceSourceID.Valid && strings.TrimSpace(evidenceSourceID.String) != "" {
		desired[strings.TrimSpace(evidenceSourceID.String)] |= edgeSourceRefEvidence
	}
	for _, endpoint := range []struct {
		nodeID string
		mask   int
	}{{srcID, edgeSourceRefSrcMetadata}, {dstID, edgeSourceRefDstMetadata}} {
		rows, err := tx.QueryContext(ctx, `
select source_id from source_nodes where node_id = ? and ref_mask & ? != 0
`, endpoint.nodeID, nodeSourceRefMetadata)
		if err != nil {
			return err
		}
		for rows.Next() {
			var sourceID string
			if err := rows.Scan(&sourceID); err != nil {
				rows.Close()
				return err
			}
			desired[sourceID] |= endpoint.mask
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, `delete from source_edges where edge_id = ?`, edgeID); err != nil {
		return err
	}
	for sourceID, refMask := range desired {
		if _, err := tx.ExecContext(ctx, `insert into source_edges (source_id, edge_id, ref_mask) values (?, ?, ?)`, sourceID, edgeID, refMask); err != nil {
			return err
		}
	}
	return nil
}

func stringSetEqual(left, right map[string]bool) bool {
	if len(left) != len(right) {
		return false
	}
	for value := range left {
		if !right[value] {
			return false
		}
	}
	return true
}

func (s *Store) UpsertNode(ctx context.Context, node domain.NodeInput) error {
	node.ID = strings.TrimSpace(node.ID)
	node.Kind = strings.TrimSpace(node.Kind)
	node.Name = strings.TrimSpace(node.Name)
	node.CanonicalName = strings.TrimSpace(node.CanonicalName)
	if node.ID == "" {
		return fmt.Errorf("node id is required")
	}
	if node.Kind == "" {
		return fmt.Errorf("node kind is required")
	}
	if node.Name == "" {
		return fmt.Errorf("node name is required")
	}
	if node.CanonicalName == "" {
		node.CanonicalName = strings.ToLower(node.Name)
	}
	if node.MetadataJSON == "" {
		node.MetadataJSON = "{}"
	}
	if node.Confidence <= 0 {
		node.Confidence = 1
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
insert into nodes (id, kind, name, canonical_name, metadata_json, confidence)
values (?, ?, ?, ?, ?, ?)
on conflict(id) do update set
  kind = excluded.kind,
  name = excluded.name,
  canonical_name = excluded.canonical_name,
  metadata_json = excluded.metadata_json,
  confidence = excluded.confidence,
  updated_at = current_timestamp
`, node.ID, node.Kind, node.Name, node.CanonicalName, node.MetadataJSON, node.Confidence); err != nil {
		return fmt.Errorf("upsert node %s: %w", node.ID, err)
	}
	if err := replaceNodeMembershipsTx(ctx, tx, node.ID); err != nil {
		return fmt.Errorf("update memberships for node %s: %w", node.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `delete from fts_nodes where node_id = ?`, node.ID); err != nil {
		return fmt.Errorf("delete fts node %s: %w", node.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `
insert into fts_nodes (kind, name, canonical_name, metadata_json, node_id)
values (?, ?, ?, ?, ?)
`, node.Kind, node.Name, node.CanonicalName, node.MetadataJSON, node.ID); err != nil {
		return fmt.Errorf("insert fts node %s: %w", node.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `delete from fts_nodes_trigram where node_id = ?`, node.ID); err != nil {
		return fmt.Errorf("delete trigram fts node %s: %w", node.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `
insert into fts_nodes_trigram (id, kind, name, canonical_name, metadata_json, node_id)
values (?, ?, ?, ?, ?, ?)
`, node.ID, node.Kind, node.Name, node.CanonicalName, node.MetadataJSON, node.ID); err != nil {
		return fmt.Errorf("insert trigram fts node %s: %w", node.ID, err)
	}
	if err := replaceNodeShortGramSearchIndex(ctx, tx, node.ID, node.Kind, node.Name, node.CanonicalName); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit upsert node %s: %w", node.ID, err)
	}
	return nil
}

func (s *Store) UpsertEdge(ctx context.Context, edge domain.EdgeInput) error {
	edge.ID = strings.TrimSpace(edge.ID)
	edge.SrcID = strings.TrimSpace(edge.SrcID)
	edge.DstID = strings.TrimSpace(edge.DstID)
	edge.Kind = strings.TrimSpace(edge.Kind)
	if edge.ID == "" {
		return fmt.Errorf("edge id is required")
	}
	if edge.SrcID == "" {
		return fmt.Errorf("edge src id is required")
	}
	if edge.DstID == "" {
		return fmt.Errorf("edge dst id is required")
	}
	if edge.Kind == "" {
		return fmt.Errorf("edge kind is required")
	}
	if edge.Confidence <= 0 {
		edge.Confidence = 1
	}
	if edge.Provenance == "" {
		edge.Provenance = "rule"
	}
	if edge.MetadataJSON == "" {
		edge.MetadataJSON = "{}"
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
insert into edges (id, src_id, dst_id, kind, confidence, provenance, evidence_section_id, source_revision, metadata_json)
values (?, ?, ?, ?, ?, ?, nullif(?, ''), ?, ?)
on conflict(id) do update set
  src_id = excluded.src_id,
  dst_id = excluded.dst_id,
  kind = excluded.kind,
  confidence = excluded.confidence,
  provenance = excluded.provenance,
  evidence_section_id = excluded.evidence_section_id,
  source_revision = excluded.source_revision,
  metadata_json = excluded.metadata_json,
  updated_at = current_timestamp
`, edge.ID, edge.SrcID, edge.DstID, edge.Kind, edge.Confidence, edge.Provenance, edge.EvidenceSectionID, edge.SourceRevision, edge.MetadataJSON)
	if err != nil {
		return fmt.Errorf("upsert edge %s src=%s dst=%s evidence_section=%s: %w", edge.ID, edge.SrcID, edge.DstID, edge.EvidenceSectionID, err)
	}
	if err := replaceEdgeMembershipsTx(ctx, tx, edge.ID); err != nil {
		return fmt.Errorf("update memberships for edge %s: %w", edge.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit upsert edge %s: %w", edge.ID, err)
	}
	return nil
}

func (s *Store) SearchNodes(ctx context.Context, query string, limit int) ([]domain.Node, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	ftsQuery := nodeFTSQuery(query)
	queryRuneLength := len([]rune(query))
	args := []any{query, query, query}
	indexedCandidates := ""
	if ftsQuery != "" && queryRuneLength != 2 {
		indexedCandidates += `
  union all
  select n.id, 2
  from fts_nodes f
  join nodes n on n.id = f.node_id
  where fts_nodes match ?
`
		args = append(args, ftsQuery)
	}
	if queryRuneLength <= 2 {
		indexedCandidates += `
  union all
  select n.id, 3
  from fts_nodes_shortgrams f
  join nodes n on n.id = f.node_id
  where fts_nodes_shortgrams match ?
`
		args = append(args, shortGramFTSQuery(query))
	}
	if queryRuneLength >= 3 {
		indexedCandidates += `
  union all
  select n.id, 4
  from fts_nodes_trigram f
  join nodes n on n.id = f.node_id
  where fts_nodes_trigram match ?
`
		args = append(args, trigramFTSQuery(query))
	}
	args = append(args, limit)
	rows, err := s.readDB().QueryContext(ctx, `
with candidates as (
  select id, 0 as source_rank from nodes where id = ?
  union all
  select id, 1 from nodes where name = ? collate nocase or canonical_name = ? collate nocase
`+indexedCandidates+`
),
ranked as (
  select id, min(source_rank) as source_rank
  from candidates
  group by id
)
select n.id, n.kind, n.name, n.canonical_name, n.metadata_json, n.confidence,
       coalesce(n.owner_source_id, ''), n.owner_scope, n.created_at, n.updated_at
from ranked r
join nodes n on n.id = r.id
order by r.source_rank, n.kind asc, n.name asc
limit ?
`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	nodes, err := scanNodes(rows)
	if err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(nodes) > 0 || queryRuneLength != 1 {
		return nodes, nil
	}
	return s.searchNodesSingleRuneLike(ctx, query, limit)
}

func (s *Store) searchNodesSingleRuneLike(ctx context.Context, query string, limit int) ([]domain.Node, error) {
	pattern := "%" + escapeLike(strings.ToLower(query)) + "%"
	rows, err := s.readDB().QueryContext(ctx, `
select id, kind, name, canonical_name, metadata_json, confidence,
       coalesce(owner_source_id, ''), owner_scope, created_at, updated_at
from nodes
where lower(id) like ? escape '\'
   or lower(kind) like ? escape '\'
   or lower(name) like ? escape '\'
   or lower(canonical_name) like ? escape '\'
order by kind asc, name asc
limit ?
`, pattern, pattern, pattern, pattern, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

func scanNodes(rows *sql.Rows) ([]domain.Node, error) {
	nodes := make([]domain.Node, 0)
	for rows.Next() {
		var node domain.Node
		if err := rows.Scan(&node.ID, &node.Kind, &node.Name, &node.CanonicalName, &node.MetadataJSON, &node.Confidence, &node.OwnerSourceID, &node.OwnerScope, &node.CreatedAt, &node.UpdatedAt); err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}

func nodeFTSQuery(query string) string {
	terms := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '/' || r == '-' || r == '.')
	})
	seen := map[string]bool{}
	cleaned := make([]string, 0, len(terms))
	for _, term := range terms {
		term = strings.Trim(term, `"'`)
		if term == "" || seen[term] {
			continue
		}
		seen[term] = true
		cleaned = append(cleaned, `"`+strings.ReplaceAll(term, `"`, `""`)+`"`)
	}
	return strings.Join(cleaned, " OR ")
}

func (s *Store) GetNode(ctx context.Context, id string) (domain.Node, error) {
	var node domain.Node
	err := s.readDB().QueryRowContext(ctx, `
select id, kind, name, canonical_name, metadata_json, confidence,
       coalesce(owner_source_id, ''), owner_scope, created_at, updated_at
from nodes
where id = ?
`, id).Scan(&node.ID, &node.Kind, &node.Name, &node.CanonicalName, &node.MetadataJSON, &node.Confidence, &node.OwnerSourceID, &node.OwnerScope, &node.CreatedAt, &node.UpdatedAt)
	if err != nil {
		return domain.Node{}, err
	}
	return node, nil
}

func (s *Store) GetSection(ctx context.Context, id string) (domain.SectionContent, error) {
	var sc domain.SectionContent
	var parentID, sourceElementID, elementKind, orderPathJSON, displayNumber, presentationJSON sql.NullString
	var structureDocumentID sql.NullString
	var depth, siblingOrdinal sql.NullInt64
	err := s.readDB().QueryRowContext(ctx, `
select s.id,
	   coalesce((select sn.node_id from section_nodes sn join nodes n on n.id = sn.node_id
	             where sn.section_id = s.id and n.kind = 'DocSection'
	             order by case when sn.role in ('represents', 'primary') then 0 else 1 end, sn.node_id limit 1), ''),
	   s.document_id, d.title, d.url, s.title, s.heading_path, s.content, s.search_text, s.metadata_json,
       ss.document_id, ss.parent_section_id, ss.source_element_id, ss.element_kind,
       ss.depth, ss.sibling_ordinal, ss.order_path_json, ss.display_number, ss.presentation_json
from sections s
join documents d on s.document_id = d.id
left join section_structures ss on ss.section_id = s.id
where s.id = ?
`, id).Scan(&sc.SectionID, &sc.NodeID, &sc.DocumentID, &sc.DocumentTitle, &sc.DocumentURL, &sc.Title, &sc.HeadingPath, &sc.Content, &sc.SearchText, &sc.MetadataJSON,
		&structureDocumentID, &parentID, &sourceElementID, &elementKind, &depth, &siblingOrdinal, &orderPathJSON, &displayNumber, &presentationJSON)
	if err != nil {
		return domain.SectionContent{}, err
	}
	if structureDocumentID.Valid {
		structure := domain.SectionStructure{
			SectionID:        sc.SectionID,
			DocumentID:       structureDocumentID.String,
			ParentSectionID:  parentID.String,
			SourceElementID:  sourceElementID.String,
			ElementKind:      elementKind.String,
			Depth:            int(depth.Int64),
			SiblingOrdinal:   int(siblingOrdinal.Int64),
			DisplayNumber:    displayNumber.String,
			PresentationJSON: presentationJSON.String,
		}
		_ = json.Unmarshal([]byte(orderPathJSON.String), &structure.OrderPath)
		sc.Structure = &structure
	}
	if err := s.attachExplicitReferences(ctx, &sc); err != nil {
		return domain.SectionContent{}, err
	}
	media, err := s.ListSectionMediaAssets(ctx, []string{sc.SectionID})
	if err != nil {
		return domain.SectionContent{}, err
	}
	sc.MediaAssets = media[sc.SectionID]
	return sc, nil
}

func (s *Store) GetActiveSourceSnapshot(ctx context.Context, sourceID string) (domain.SourceSnapshot, error) {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return domain.SourceSnapshot{}, fmt.Errorf("source id is required")
	}
	var snapshot domain.SourceSnapshot
	var active int
	err := s.readDB().QueryRowContext(ctx, `
select id, source_id, source_hash, blob_sha256, format_family, format_version,
       semantic_hash, media_manifest_hash, metadata_json, active, created_at
from source_snapshots where source_id = ? and active = 1
`, sourceID).Scan(&snapshot.ID, &snapshot.SourceID, &snapshot.SourceHash, &snapshot.BlobSHA256,
		&snapshot.FormatFamily, &snapshot.FormatVersion, &snapshot.SemanticHash, &snapshot.MediaManifestHash,
		&snapshot.MetadataJSON, &active, &snapshot.CreatedAt)
	snapshot.Active = active != 0
	return snapshot, err
}

func (s *Store) GetMediaAsset(ctx context.Context, assetID string) (domain.MediaAsset, error) {
	assetID = strings.TrimSpace(assetID)
	if assetID == "" {
		return domain.MediaAsset{}, fmt.Errorf("media asset id is required")
	}
	var asset domain.MediaAsset
	var blob sql.NullString
	err := s.readDB().QueryRowContext(ctx, `
select id, source_id, snapshot_id, document_id, external_id, blob_sha256, kind,
       original_name, media_type, size_bytes, status, metadata_json, created_at, updated_at
from media_assets where id = ?
`, assetID).Scan(&asset.ID, &asset.SourceID, &asset.SnapshotID, &asset.DocumentID, &asset.ExternalID,
		&blob, &asset.Kind, &asset.OriginalName, &asset.MediaType, &asset.SizeBytes, &asset.Status,
		&asset.MetadataJSON, &asset.CreatedAt, &asset.UpdatedAt)
	if blob.Valid {
		asset.BlobSHA256 = blob.String
	}
	return asset, err
}

func (s *Store) ListDocumentMediaAssets(ctx context.Context, documentID string, limit, offset int) ([]domain.MediaAsset, error) {
	documentID = strings.TrimSpace(documentID)
	if documentID == "" {
		return nil, fmt.Errorf("document id is required")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.readDB().QueryContext(ctx, `
select id, source_id, snapshot_id, document_id, external_id, blob_sha256, kind,
       original_name, media_type, size_bytes, status, metadata_json, created_at, updated_at
from media_assets where document_id = ? order by original_name, id limit ? offset ?
`, documentID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	assets := make([]domain.MediaAsset, 0)
	for rows.Next() {
		var asset domain.MediaAsset
		var blob sql.NullString
		if err := rows.Scan(&asset.ID, &asset.SourceID, &asset.SnapshotID, &asset.DocumentID, &asset.ExternalID,
			&blob, &asset.Kind, &asset.OriginalName, &asset.MediaType, &asset.SizeBytes, &asset.Status,
			&asset.MetadataJSON, &asset.CreatedAt, &asset.UpdatedAt); err != nil {
			return nil, err
		}
		if blob.Valid {
			asset.BlobSHA256 = blob.String
		}
		assets = append(assets, asset)
	}
	return assets, rows.Err()
}

func (s *Store) ListSectionMediaAssets(ctx context.Context, sectionIDs []string) (map[string][]domain.MediaAssetSummary, error) {
	result := make(map[string][]domain.MediaAssetSummary, len(sectionIDs))
	ids := uniqueStrings(sectionIDs)
	if len(ids) == 0 {
		return result, nil
	}
	if len(ids) > 500 {
		return nil, fmt.Errorf("too many section ids: %d", len(ids))
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
		result[id] = []domain.MediaAssetSummary{}
	}
	rows, err := s.readDB().QueryContext(ctx, `
select smr.section_id, ma.id, ma.kind, ma.original_name, ma.media_type, ma.size_bytes,
       ma.status, smr.role, smr.ordinal, ma.metadata_json, smr.metadata_json
from section_media_refs smr
join media_assets ma on ma.id = smr.asset_id
where smr.section_id in (`+placeholders+`)
order by smr.section_id, smr.ordinal, ma.id
`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sectionID string
		var asset domain.MediaAssetSummary
		if err := rows.Scan(&sectionID, &asset.ID, &asset.Kind, &asset.OriginalName, &asset.MediaType,
			&asset.SizeBytes, &asset.Status, &asset.Role, &asset.Ordinal, &asset.MetadataJSON, &asset.ReferenceMetadataJSON); err != nil {
			return nil, err
		}
		result[sectionID] = append(result[sectionID], asset)
	}
	return result, rows.Err()
}

func (s *Store) ListSourceFeatureInventory(ctx context.Context, sourceID string) ([]domain.FeatureInventoryEntry, error) {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return nil, fmt.Errorf("source id is required")
	}
	rows, err := s.readDB().QueryContext(ctx, `
select fi.id, fi.snapshot_id, fi.feature_key, fi.coverage_status, fi.element_path, fi.count, fi.metadata_json
from source_feature_inventory fi
join source_snapshots ss on ss.id = fi.snapshot_id
where ss.source_id = ? and ss.active = 1
order by fi.coverage_status, fi.feature_key, fi.element_path, fi.id
`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := make([]domain.FeatureInventoryEntry, 0)
	for rows.Next() {
		var entry domain.FeatureInventoryEntry
		if err := rows.Scan(&entry.ID, &entry.SnapshotID, &entry.FeatureKey, &entry.CoverageStatus,
			&entry.ElementPath, &entry.Count, &entry.MetadataJSON); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func (s *Store) IsMediaBlobReferenced(ctx context.Context, digest string) (bool, error) {
	digest = strings.ToLower(strings.TrimSpace(digest))
	if !validSHA256(digest) {
		return false, fmt.Errorf("invalid sha256 digest")
	}
	var referenced bool
	err := s.readDB().QueryRowContext(ctx, `
select exists(select 1 from source_snapshots where blob_sha256 = ?)
    or exists(select 1 from media_assets where blob_sha256 = ?)
`, digest, digest).Scan(&referenced)
	return referenced, err
}

func (s *Store) ListUnreferencedMediaBlobs(ctx context.Context, olderThan string, limit int) ([]domain.MediaBlob, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.readDB().QueryContext(ctx, `
select mb.sha256, mb.size_bytes, mb.sniffed_media_type, mb.storage_key, mb.created_at
from media_blobs mb
where (? = '' or mb.created_at < ?)
  and not exists(select 1 from source_snapshots ss where ss.blob_sha256 = mb.sha256)
  and not exists(select 1 from media_assets ma where ma.blob_sha256 = mb.sha256)
order by mb.created_at, mb.sha256 limit ?
`, olderThan, olderThan, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	blobs := make([]domain.MediaBlob, 0)
	for rows.Next() {
		var blob domain.MediaBlob
		if err := rows.Scan(&blob.SHA256, &blob.SizeBytes, &blob.SniffedMediaType, &blob.StorageKey, &blob.CreatedAt); err != nil {
			return nil, err
		}
		blobs = append(blobs, blob)
	}
	return blobs, rows.Err()
}

func (s *Store) DeleteMediaBlobIfUnreferenced(ctx context.Context, digest string, olderThan string) (bool, error) {
	digest = strings.ToLower(strings.TrimSpace(digest))
	if !validSHA256(digest) {
		return false, fmt.Errorf("invalid sha256 digest")
	}
	result, err := s.db.ExecContext(ctx, `
delete from media_blobs
where sha256 = ? and (? = '' or created_at < ?)
  and not exists(select 1 from source_snapshots where blob_sha256 = ?)
  and not exists(select 1 from media_assets where blob_sha256 = ?)
`, digest, olderThan, olderThan, digest, digest)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count > 0, err
}

func (s *Store) ListDocumentOutline(ctx context.Context, documentID string, opts domain.OutlineOptions) (domain.DocumentOutline, error) {
	documentID = strings.TrimSpace(documentID)
	opts.ParentSectionID = strings.TrimSpace(opts.ParentSectionID)
	if documentID == "" {
		return domain.DocumentOutline{}, fmt.Errorf("document id is required")
	}
	if opts.Limit <= 0 || opts.Limit > 500 {
		opts.Limit = 100
	}
	if opts.Offset < 0 {
		return domain.DocumentOutline{}, fmt.Errorf("offset must be non-negative")
	}
	rows, err := s.readDB().QueryContext(ctx, `
select s.id, s.document_id, d.title, s.title, s.heading_path,
       substr(replace(replace(s.content, char(10), ' '), char(13), ' '), 1, 800),
       s.content_hash, s.ordinal,
       coalesce((select sn.node_id from section_nodes sn join nodes n on n.id = sn.node_id
                 where sn.section_id = s.id and n.kind = 'DocSection'
                 order by case when sn.role = 'represents' then 0 else 1 end, sn.node_id limit 1), ''),
       coalesce(ss.parent_section_id, ''), coalesce(ss.source_element_id, ''), coalesce(ss.element_kind, ''),
       coalesce(ss.depth, 0), coalesce(ss.sibling_ordinal, s.ordinal), coalesce(ss.order_path_json, '[]'),
       coalesce(ss.display_number, ''), coalesce(ss.presentation_json, '{}'), s.metadata_json,
       (select count(*) from section_media_refs smr where smr.section_id = s.id)
from sections s
join documents d on d.id = s.document_id
left join section_structures ss on ss.section_id = s.id
where s.document_id = ?
  and ((? = '' and ss.parent_section_id is null) or (? <> '' and ss.parent_section_id = ?))
order by coalesce(ss.sibling_ordinal, s.ordinal), s.id
limit ? offset ?
`, documentID, opts.ParentSectionID, opts.ParentSectionID, opts.ParentSectionID, opts.Limit+1, opts.Offset)
	if err != nil {
		return domain.DocumentOutline{}, err
	}
	sections, err := scanSectionSummaryRows(rows)
	if err != nil {
		return domain.DocumentOutline{}, err
	}
	hasMore := len(sections) > opts.Limit
	if hasMore {
		sections = sections[:opts.Limit]
	}
	return domain.DocumentOutline{
		DocumentID:      documentID,
		ParentSectionID: opts.ParentSectionID,
		Sections:        sections,
		Limit:           opts.Limit,
		Offset:          opts.Offset,
		HasMore:         hasMore,
	}, nil
}

func scanSectionSummaryRows(rows *sql.Rows) ([]domain.SectionSummary, error) {
	defer rows.Close()
	sections := make([]domain.SectionSummary, 0)
	for rows.Next() {
		var section domain.SectionSummary
		var orderPathJSON string
		if err := rows.Scan(&section.ID, &section.DocumentID, &section.DocumentTitle, &section.Title,
			&section.HeadingPath, &section.ContentSnippet, &section.ContentHash, &section.Ordinal, &section.NodeID,
			&section.ParentSectionID, &section.SourceElementID, &section.ElementKind, &section.Depth,
			&section.SiblingOrdinal, &orderPathJSON, &section.DisplayNumber, &section.PresentationJSON,
			&section.MetadataJSON, &section.MediaCount); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(orderPathJSON), &section.OrderPath)
		if section.NodeID == "" && section.ElementKind == "" {
			section.NodeID = stableSectionNodeID(section.ID)
		}
		sections = append(sections, section)
	}
	return sections, rows.Err()
}

func (s *Store) GetSectionContext(ctx context.Context, sectionID string, childLimit int) (domain.SectionContext, error) {
	sectionID = strings.TrimSpace(sectionID)
	if sectionID == "" {
		return domain.SectionContext{}, fmt.Errorf("section id is required")
	}
	if childLimit <= 0 || childLimit > 500 {
		childLimit = 100
	}
	section, err := s.GetSection(ctx, sectionID)
	if err != nil {
		return domain.SectionContext{}, err
	}
	outline, err := s.ListDocumentOutline(ctx, section.DocumentID, domain.OutlineOptions{ParentSectionID: sectionID, Limit: childLimit})
	if err != nil {
		return domain.SectionContext{}, err
	}
	ancestorRows, err := s.readDB().QueryContext(ctx, `
with recursive ancestor_ids(id) as (
  select parent_section_id from section_structures where section_id = ? and parent_section_id is not null
  union all
  select ss.parent_section_id from section_structures ss join ancestor_ids a on a.id = ss.section_id
  where ss.parent_section_id is not null
)
select s.id, s.document_id, d.title, s.title, s.heading_path,
       substr(replace(replace(s.content, char(10), ' '), char(13), ' '), 1, 800),
       s.content_hash, s.ordinal,
       coalesce((select sn.node_id from section_nodes sn join nodes n on n.id = sn.node_id
                 where sn.section_id = s.id and n.kind = 'DocSection'
                 order by case when sn.role = 'represents' then 0 else 1 end, sn.node_id limit 1), ''),
       coalesce(ss.parent_section_id, ''), coalesce(ss.source_element_id, ''), coalesce(ss.element_kind, ''),
       coalesce(ss.depth, 0), coalesce(ss.sibling_ordinal, s.ordinal), coalesce(ss.order_path_json, '[]'),
       coalesce(ss.display_number, ''), coalesce(ss.presentation_json, '{}'), s.metadata_json,
       (select count(*) from section_media_refs smr where smr.section_id = s.id)
from ancestor_ids a
join sections s on s.id = a.id
join documents d on d.id = s.document_id
left join section_structures ss on ss.section_id = s.id
order by ss.depth, ss.sibling_ordinal, s.id
`, sectionID)
	if err != nil {
		return domain.SectionContext{}, err
	}
	ancestors, err := scanSectionSummaryRows(ancestorRows)
	if err != nil {
		return domain.SectionContext{}, err
	}
	var sectionNodeID string
	err = s.readDB().QueryRowContext(ctx, `
select sn.node_id from section_nodes sn join nodes n on n.id = sn.node_id
where sn.section_id = ? and n.kind = 'DocSection'
order by case when sn.role = 'represents' then 0 else 1 end, sn.node_id limit 1
`, sectionID).Scan(&sectionNodeID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return domain.SectionContext{}, err
	}
	var authored []domain.RelatedNode
	if sectionNodeID != "" {
		for _, kind := range []string{"related_to", "links_to", "summarizes", "annotates", "groups"} {
			if len(authored) >= 100 {
				break
			}
			related, relatedErr := s.RelatedNodes(ctx, sectionNodeID, domain.RelatedOptions{Direction: "both", Kind: kind, Limit: 100 - len(authored)})
			if relatedErr != nil && !errors.Is(relatedErr, sql.ErrNoRows) {
				return domain.SectionContext{}, relatedErr
			}
			for _, relation := range related {
				if relation.Edge.Provenance == "source_authored" {
					authored = append(authored, relation)
				}
			}
		}
	}
	var sourceID string
	if err := s.readDB().QueryRowContext(ctx, `select source_id from documents where id = ?`, section.DocumentID).Scan(&sourceID); err != nil {
		return domain.SectionContext{}, err
	}
	var snapshot *domain.SourceSnapshot
	active, err := s.GetActiveSourceSnapshot(ctx, sourceID)
	if err == nil {
		snapshot = &active
	} else if !errors.Is(err, sql.ErrNoRows) {
		return domain.SectionContext{}, err
	}
	return domain.SectionContext{
		Section:           section,
		Structure:         section.Structure,
		Ancestors:         ancestors,
		Children:          outline.Sections,
		ChildrenPage:      outline,
		AuthoredRelations: authored,
		MediaAssets:       section.MediaAssets,
		Snapshot:          snapshot,
	}, nil
}

func (s *Store) UpsertSectionEmbedding(ctx context.Context, input domain.SectionEmbeddingInput) error {
	if s.vectorBackend != nil {
		return s.vectorBackend.UpsertSectionEmbedding(ctx, input)
	}
	return fmt.Errorf("vector search is disabled")
}

func (s *Store) GetSectionEmbedding(ctx context.Context, sectionID string, model string) (domain.VectorSearchHit, []float32, error) {
	if s.vectorBackend != nil {
		return s.vectorBackend.GetSectionEmbedding(ctx, sectionID, model)
	}
	return domain.VectorSearchHit{}, nil, sql.ErrNoRows
}

func (s *Store) DeleteSectionEmbeddings(ctx context.Context, sectionID string) error {
	if s.vectorBackend != nil {
		return s.vectorBackend.DeleteSectionEmbeddings(ctx, sectionID)
	}
	return nil
}

func (s *Store) SearchSectionsByVector(ctx context.Context, embedding []float32, model string, limit int, minSimilarity float64, plan vectorstore.EmbeddingPlanFilter) ([]domain.VectorSearchHit, error) {
	if s.vectorBackend != nil {
		return s.vectorBackend.SearchSectionsByVector(ctx, embedding, model, limit, minSimilarity, plan)
	}
	return nil, nil
}

func (s *Store) ListSectionEmbeddingHashes(ctx context.Context, model string, limit, offset int) ([]domain.SectionEmbeddingHash, error) {
	if s.vectorBackend != nil {
		return s.vectorBackend.ListSectionEmbeddingHashes(ctx, model, limit, offset)
	}
	return nil, nil
}

func (s *Store) UpsertEmbeddingChunk(ctx context.Context, input domain.EmbeddingChunkInput) error {
	if s.vectorBackend != nil {
		return s.vectorBackend.UpsertEmbeddingChunk(ctx, input)
	}
	return fmt.Errorf("vector search is disabled")
}

func (s *Store) GetEmbeddingChunk(ctx context.Context, chunkID string, model string) (domain.VectorSearchHit, []float32, error) {
	if s.vectorBackend != nil {
		return s.vectorBackend.GetEmbeddingChunk(ctx, chunkID, model)
	}
	return domain.VectorSearchHit{}, nil, sql.ErrNoRows
}

func (s *Store) DeleteEmbeddingChunksBySection(ctx context.Context, sectionID string, model string, generatorVersion string, tokenizer string, chunkStrategy string) error {
	if s.vectorBackend != nil {
		return s.vectorBackend.DeleteEmbeddingChunksBySection(ctx, sectionID, model, generatorVersion, tokenizer, chunkStrategy)
	}
	return nil
}

func (s *Store) SearchChunksByVector(ctx context.Context, embedding []float32, model string, limit int, minSimilarity float64, plan vectorstore.EmbeddingPlanFilter) ([]domain.VectorSearchHit, error) {
	if s.vectorBackend != nil {
		return s.vectorBackend.SearchChunksByVector(ctx, embedding, model, limit, minSimilarity, plan)
	}
	return nil, nil
}

func (s *Store) ListEmbeddingChunkHashes(ctx context.Context, model string, limit, offset int) ([]domain.EmbeddingChunkHash, error) {
	if s.vectorBackend != nil {
		return s.vectorBackend.ListEmbeddingChunkHashes(ctx, model, limit, offset)
	}
	return nil, nil
}

func (s *Store) GetSourceEmbeddingStatus(ctx context.Context, sourceID string, model string, generatorVersion string, tokenizer string, chunkStrategy string, chunkTargetTokens int) (domain.EmbeddingStatus, error) {
	sourceID = strings.TrimSpace(sourceID)
	model = strings.TrimSpace(model)
	generatorVersion = strings.TrimSpace(generatorVersion)
	tokenizer = strings.TrimSpace(tokenizer)
	chunkStrategy = strings.TrimSpace(chunkStrategy)
	if sourceID == "" {
		return domain.EmbeddingStatus{}, fmt.Errorf("source id is required")
	}
	status := domain.EmbeddingStatus{SourceID: sourceID, Model: model, GeneratorVersion: generatorVersion}
	err := s.readDB().QueryRowContext(ctx, `
select count(*)
from sections
join documents on documents.id = sections.document_id
where documents.source_id = ?
`, sourceID).Scan(&status.TotalSections)
	if err != nil {
		return domain.EmbeddingStatus{}, err
	}
	if model == "" || s.vectorBackend == nil {
		status.Enabled = false
		status.Status = "disabled"
		if model == "" {
			status.Reason = "embedding_not_configured"
		} else {
			status.Reason = "vector_db_not_configured"
		}
		status.PendingSections = status.TotalSections
		return status, nil
	}
	status.Enabled = true
	if tokenizer == "" {
		tokenizer = "auto"
	}
	if chunkStrategy == "" {
		chunkStrategy = "auto"
	}
	resolvedTokenizer := embeddingchunk.NewTextMeasurer(tokenizer, model).Name()
	status.Tokenizer = resolvedTokenizer
	status.ChunkStrategy = chunkStrategy

	coverage, err := s.vectorBackend.GetEmbeddingCoverage(ctx, sourceID, model, generatorVersion, resolvedTokenizer, chunkStrategy)
	if err != nil {
		return domain.EmbeddingStatus{}, fmt.Errorf("query embedding coverage: %w", err)
	}
	status.EmbeddedSections = coverage.EmbeddedSections
	status.EmbeddedChunks = coverage.EmbeddedChunks
	status.TotalChunks = coverage.EmbeddedChunks // best known count; pending chunks unknown until they're built
	status.PendingSections = status.TotalSections - status.EmbeddedSections
	if status.PendingSections < 0 {
		status.PendingSections = 0
	}
	if status.PendingSections > 0 {
		status.Status = "indexing"
	} else {
		status.Status = "ready"
	}
	return status, nil
}

func (s *Store) GetSectionForEmbedding(ctx context.Context, sectionID string) (domain.EmbeddingSection, error) {
	sectionID = strings.TrimSpace(sectionID)
	if sectionID == "" {
		return domain.EmbeddingSection{}, fmt.Errorf("section id is required")
	}
	var section domain.EmbeddingSection
	err := s.readDB().QueryRowContext(ctx, `
select sections.id, sections.document_id, documents.source_id, sources.name, sources.product_hint, sources.module_hint,
	   documents.title, sections.heading_path, sections.title,
	   case when sections.search_text <> '' then sections.search_text else sections.content end,
	   sections.search_text, sections.content_hash
from sections
join documents on documents.id = sections.document_id
join sources on sources.id = documents.source_id
where sections.id = ?
`, sectionID).Scan(&section.SectionID, &section.DocumentID, &section.SourceID, &section.SourceName, &section.ProductHint, &section.ModuleHint, &section.DocumentTitle, &section.HeadingPath, &section.Title, &section.Content, &section.SearchText, &section.ContentHash)
	if err != nil {
		return domain.EmbeddingSection{}, err
	}
	return section, nil
}

func (s *Store) ListSectionsForEmbedding(ctx context.Context, sourceID string, limit, offset int) ([]domain.EmbeddingSection, error) {
	sourceID = strings.TrimSpace(sourceID)
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	where := ""
	args := []any{}
	if sourceID != "" {
		where = "where sources.id = ?"
		args = append(args, sourceID)
	}
	args = append(args, limit, offset)
	rows, err := s.readDB().QueryContext(ctx, `
select sections.id, sections.document_id, documents.source_id, sources.name, sources.product_hint, sources.module_hint,
	   documents.title, sections.heading_path, sections.title,
	   case when sections.search_text <> '' then sections.search_text else sections.content end,
	   sections.search_text, sections.content_hash
from sections
join documents on documents.id = sections.document_id
join sources on sources.id = documents.source_id
`+where+`
order by documents.title asc, sections.ordinal asc, sections.id asc
limit ? offset ?
`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sections := make([]domain.EmbeddingSection, 0)
	for rows.Next() {
		var section domain.EmbeddingSection
		if err := rows.Scan(&section.SectionID, &section.DocumentID, &section.SourceID, &section.SourceName, &section.ProductHint, &section.ModuleHint, &section.DocumentTitle, &section.HeadingPath, &section.Title, &section.Content, &section.SearchText, &section.ContentHash); err != nil {
			return nil, err
		}
		sections = append(sections, section)
	}
	return sections, rows.Err()
}

func (s *Store) CountSectionsForEmbedding(ctx context.Context, sourceID string) (int, error) {
	sourceID = strings.TrimSpace(sourceID)
	where := ""
	args := []any{}
	if sourceID != "" {
		where = "where sources.id = ?"
		args = append(args, sourceID)
	}
	var count int
	err := s.readDB().QueryRowContext(ctx, `
select count(*)
from sections
join documents on documents.id = sections.document_id
join sources on sources.id = documents.source_id
`+where, args...).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) RelatedNodes(ctx context.Context, id string, opts domain.RelatedOptions) ([]domain.RelatedNode, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("node id is required")
	}
	direction := strings.ToLower(strings.TrimSpace(opts.Direction))
	if direction == "" {
		direction = "both"
	}
	if direction != "both" && direction != "out" && direction != "in" {
		return nil, fmt.Errorf("unsupported related direction %q", opts.Direction)
	}
	limit := opts.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}

	related := make([]domain.RelatedNode, 0)
	if direction == "both" || direction == "out" {
		rows, err := s.relatedRows(ctx, relatedOutQuery, id, opts.Kind, limit)
		if err != nil {
			return nil, err
		}
		related = append(related, rows...)
	}
	if len(related) < limit && (direction == "both" || direction == "in") {
		rows, err := s.relatedRows(ctx, relatedInQuery, id, opts.Kind, limit-len(related))
		if err != nil {
			return nil, err
		}
		related = append(related, rows...)
	}
	return related, nil
}

func (s *Store) ListAuthoredRelationsForSections(ctx context.Context, sectionIDs []string, perSectionLimit int) (map[string][]domain.RelatedNode, error) {
	unique := make([]string, 0, len(sectionIDs))
	seen := make(map[string]bool, len(sectionIDs))
	for _, id := range sectionIDs {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	result := make(map[string][]domain.RelatedNode, len(unique))
	if len(unique) == 0 {
		return result, nil
	}
	if perSectionLimit <= 0 || perSectionLimit > 100 {
		perSectionLimit = 5
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(unique)), ",")
	args := make([]any, 0, len(unique)+1)
	for _, id := range unique {
		args = append(args, id)
	}
	args = append(args, perSectionLimit)
	rows, err := s.readDB().QueryContext(ctx, `
with hit_nodes as (
  select distinct sn.section_id, sn.node_id
  from section_nodes sn join nodes source_node on source_node.id = sn.node_id
  where sn.section_id in (`+placeholders+`) and source_node.kind = 'DocSection'
), ranked as (
  select h.section_id,
         counterpart.id as node_id, counterpart.kind as node_kind, counterpart.name as node_name,
         counterpart.canonical_name as node_canonical_name, counterpart.metadata_json as node_metadata_json,
         counterpart.confidence as node_confidence, coalesce(counterpart.owner_source_id, '') as node_owner_source_id,
         counterpart.owner_scope as node_owner_scope, counterpart.created_at as node_created_at, counterpart.updated_at as node_updated_at,
         e.id as edge_id, e.src_id, e.dst_id, e.kind as edge_kind, e.confidence as edge_confidence,
         e.provenance, e.evidence_section_id, e.source_revision, e.metadata_json as edge_metadata_json,
         coalesce(e.owner_source_id, '') as edge_owner_source_id, e.owner_scope as edge_owner_scope,
         e.created_at as edge_created_at, e.updated_at as edge_updated_at,
         case when e.src_id = h.node_id then 'out' else 'in' end as direction,
         row_number() over (partition by h.section_id order by e.kind, e.id) as relation_rank
  from hit_nodes h
  join edges e on e.src_id = h.node_id or e.dst_id = h.node_id
  join nodes counterpart on counterpart.id = case when e.src_id = h.node_id then e.dst_id else e.src_id end
  where e.provenance = 'source_authored'
    and e.kind in ('related_to', 'links_to')
    and not exists (
      select 1 from feedback_events fe
      where fe.target_kind = 'edge' and fe.target_id = e.id and fe.feedback_kind = 'relationship_wrong'
    )
)
select section_id,
       node_id, node_kind, node_name, node_canonical_name, node_metadata_json, node_confidence,
       node_owner_source_id, node_owner_scope, node_created_at, node_updated_at,
       edge_id, src_id, dst_id, edge_kind, edge_confidence, provenance, evidence_section_id,
       source_revision, edge_metadata_json, edge_owner_source_id, edge_owner_scope, edge_created_at, edge_updated_at,
       direction
from ranked where relation_rank <= ?
order by section_id, relation_rank
`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sectionID string
		var item domain.RelatedNode
		var evidence sql.NullString
		if err := rows.Scan(
			&sectionID,
			&item.Node.ID, &item.Node.Kind, &item.Node.Name, &item.Node.CanonicalName, &item.Node.MetadataJSON, &item.Node.Confidence,
			&item.Node.OwnerSourceID, &item.Node.OwnerScope, &item.Node.CreatedAt, &item.Node.UpdatedAt,
			&item.Edge.ID, &item.Edge.SrcID, &item.Edge.DstID, &item.Edge.Kind, &item.Edge.Confidence, &item.Edge.Provenance, &evidence,
			&item.Edge.SourceRevision, &item.Edge.MetadataJSON, &item.Edge.OwnerSourceID, &item.Edge.OwnerScope, &item.Edge.CreatedAt, &item.Edge.UpdatedAt,
			&item.Direction,
		); err != nil {
			return nil, err
		}
		if evidence.Valid {
			item.Edge.EvidenceSectionID = evidence.String
		}
		result[sectionID] = append(result[sectionID], item)
	}
	return result, rows.Err()
}

func (s *Store) Impact(ctx context.Context, id string, opts domain.ImpactOptions) (domain.ImpactResult, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return domain.ImpactResult{}, fmt.Errorf("node id is required")
	}
	start, err := s.GetNode(ctx, id)
	if err != nil {
		return domain.ImpactResult{}, err
	}
	direction := strings.ToLower(strings.TrimSpace(opts.Direction))
	if direction == "" {
		direction = "out"
	}
	if direction != "both" && direction != "out" && direction != "in" {
		return domain.ImpactResult{}, fmt.Errorf("unsupported impact direction %q", opts.Direction)
	}
	maxDepth := opts.MaxDepth
	if maxDepth <= 0 || maxDepth > 5 {
		maxDepth = 2
	}
	limit := opts.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	type candidate struct {
		nodes   []domain.Node
		edges   []domain.Edge
		visited map[string]bool
	}

	result := domain.ImpactResult{
		StartNode: start,
		Paths:     make([]domain.ImpactPath, 0),
	}
	queue := []candidate{{
		nodes:   []domain.Node{start},
		edges:   nil,
		visited: map[string]bool{start.ID: true},
	}}

	for len(queue) > 0 && len(result.Paths) < limit {
		current := queue[0]
		queue = queue[1:]
		if len(current.edges) >= maxDepth {
			continue
		}
		from := current.nodes[len(current.nodes)-1]
		related, err := s.RelatedNodes(ctx, from.ID, domain.RelatedOptions{
			Direction: direction,
			Kind:      opts.Kind,
			Limit:     limit,
		})
		if err != nil {
			return domain.ImpactResult{}, err
		}
		for _, rel := range related {
			if current.visited[rel.Node.ID] {
				continue
			}

			nextNodes := append(append([]domain.Node{}, current.nodes...), rel.Node)
			nextEdges := append(append([]domain.Edge{}, current.edges...), rel.Edge)
			path := domain.ImpactPath{Nodes: nextNodes, Edges: nextEdges}
			result.Paths = append(result.Paths, path)
			if len(result.Paths) >= limit {
				break
			}
			if len(nextEdges) < maxDepth {
				nextVisited := make(map[string]bool, len(current.visited)+1)
				for nodeID, ok := range current.visited {
					nextVisited[nodeID] = ok
				}
				nextVisited[rel.Node.ID] = true
				queue = append(queue, candidate{
					nodes:   nextNodes,
					edges:   nextEdges,
					visited: nextVisited,
				})
			}
		}
	}

	return result, nil
}

const relatedOutQuery = `
select
  n.id, n.kind, n.name, n.canonical_name, n.metadata_json, n.confidence, coalesce(n.owner_source_id, ''), n.owner_scope, n.created_at, n.updated_at,
  e.id, e.src_id, e.dst_id, e.kind, e.confidence, e.provenance, e.evidence_section_id, e.source_revision, e.metadata_json, coalesce(e.owner_source_id, ''), e.owner_scope, e.created_at, e.updated_at
from edges e
join nodes n on n.id = e.dst_id
where e.src_id = ? and (? = '' or e.kind = ?)
  and not exists (
    select 1 from feedback_events fe
    where fe.target_kind = 'edge'
      and fe.target_id = e.id
      and fe.feedback_kind = 'relationship_wrong'
  )
order by e.kind, n.kind, n.canonical_name
limit ?
`

const relatedInQuery = `
select
  n.id, n.kind, n.name, n.canonical_name, n.metadata_json, n.confidence, coalesce(n.owner_source_id, ''), n.owner_scope, n.created_at, n.updated_at,
  e.id, e.src_id, e.dst_id, e.kind, e.confidence, e.provenance, e.evidence_section_id, e.source_revision, e.metadata_json, coalesce(e.owner_source_id, ''), e.owner_scope, e.created_at, e.updated_at
from edges e
join nodes n on n.id = e.src_id
where e.dst_id = ? and (? = '' or e.kind = ?)
  and not exists (
    select 1 from feedback_events fe
    where fe.target_kind = 'edge'
      and fe.target_id = e.id
      and fe.feedback_kind = 'relationship_wrong'
  )
order by e.kind, n.kind, n.canonical_name
limit ?
`

func (s *Store) relatedRows(ctx context.Context, query string, id string, kind string, limit int) ([]domain.RelatedNode, error) {
	kind = strings.TrimSpace(kind)
	rows, err := s.readDB().QueryContext(ctx, query, id, kind, kind, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	related := make([]domain.RelatedNode, 0)
	for rows.Next() {
		var item domain.RelatedNode
		var evidence sql.NullString
		if err := rows.Scan(
			&item.Node.ID, &item.Node.Kind, &item.Node.Name, &item.Node.CanonicalName, &item.Node.MetadataJSON, &item.Node.Confidence, &item.Node.OwnerSourceID, &item.Node.OwnerScope, &item.Node.CreatedAt, &item.Node.UpdatedAt,
			&item.Edge.ID, &item.Edge.SrcID, &item.Edge.DstID, &item.Edge.Kind, &item.Edge.Confidence, &item.Edge.Provenance, &evidence, &item.Edge.SourceRevision, &item.Edge.MetadataJSON, &item.Edge.OwnerSourceID, &item.Edge.OwnerScope, &item.Edge.CreatedAt, &item.Edge.UpdatedAt,
		); err != nil {
			return nil, err
		}
		if evidence.Valid {
			item.Edge.EvidenceSectionID = evidence.String
		}
		if item.Edge.SrcID == id {
			item.Direction = "out"
		} else {
			item.Direction = "in"
		}
		related = append(related, item)
	}
	return related, rows.Err()
}

func (s *Store) CreateFeedbackEvent(ctx context.Context, input domain.FeedbackEventInput) (domain.FeedbackEvent, error) {
	input.ID = strings.TrimSpace(input.ID)
	input.TargetKind = strings.TrimSpace(input.TargetKind)
	input.TargetID = strings.TrimSpace(input.TargetID)
	input.FeedbackKind = strings.TrimSpace(input.FeedbackKind)
	input.Actor = strings.TrimSpace(input.Actor)
	input.PayloadJSON = strings.TrimSpace(input.PayloadJSON)
	if input.ID == "" {
		input.ID = ids.Random("fb", 12)
	}
	if input.TargetKind == "" {
		return domain.FeedbackEvent{}, fmt.Errorf("feedback target_kind is required")
	}
	if input.TargetID == "" {
		return domain.FeedbackEvent{}, fmt.Errorf("feedback target_id is required")
	}
	if !supportedFeedbackKind(input.FeedbackKind) {
		return domain.FeedbackEvent{}, fmt.Errorf("unsupported feedback_kind %q", input.FeedbackKind)
	}
	if err := validateFeedbackTarget(input); err != nil {
		return domain.FeedbackEvent{}, err
	}
	if input.PayloadJSON == "" {
		input.PayloadJSON = "{}"
	}
	if !json.Valid([]byte(input.PayloadJSON)) {
		return domain.FeedbackEvent{}, fmt.Errorf("feedback payload_json must be valid JSON")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.FeedbackEvent{}, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
insert into feedback_events (id, target_kind, target_id, feedback_kind, payload_json, actor)
values (?, ?, ?, ?, ?, ?)
`, input.ID, input.TargetKind, input.TargetID, input.FeedbackKind, input.PayloadJSON, input.Actor); err != nil {
		return domain.FeedbackEvent{}, err
	}
	if input.FeedbackKind == "relationship_add" {
		if err := applyRelationshipAdd(ctx, tx, input); err != nil {
			return domain.FeedbackEvent{}, err
		}
	}
	if input.FeedbackKind == "node_merge" {
		if err := applyNodeMerge(ctx, tx, input); err != nil {
			return domain.FeedbackEvent{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.FeedbackEvent{}, err
	}
	return s.getFeedbackEvent(ctx, input.ID)
}

func supportedFeedbackKind(kind string) bool {
	switch kind {
	case "document_canonical", "document_stale", "relationship_wrong", "relationship_add", "node_merge":
		return true
	default:
		return false
	}
}

func validateFeedbackTarget(input domain.FeedbackEventInput) error {
	want := ""
	switch input.FeedbackKind {
	case "document_canonical", "document_stale":
		want = "document"
	case "relationship_wrong":
		want = "edge"
	case "relationship_add":
		want = "node"
	case "node_merge":
		want = "node"
	}
	if want != "" && input.TargetKind != want {
		return fmt.Errorf("%s feedback requires target_kind %q", input.FeedbackKind, want)
	}
	return nil
}

func applyRelationshipAdd(ctx context.Context, tx *sql.Tx, input domain.FeedbackEventInput) error {
	var payload struct {
		EdgeID            string  `json:"edge_id"`
		SrcID             string  `json:"src_id"`
		DstID             string  `json:"dst_id"`
		Kind              string  `json:"kind"`
		Confidence        float64 `json:"confidence"`
		EvidenceSectionID string  `json:"evidence_section_id"`
		SourceRevision    string  `json:"source_revision"`
		MetadataJSON      string  `json:"metadata_json"`
	}
	if err := json.Unmarshal([]byte(input.PayloadJSON), &payload); err != nil {
		return fmt.Errorf("parse relationship_add payload: %w", err)
	}
	payload.EdgeID = strings.TrimSpace(payload.EdgeID)
	payload.SrcID = strings.TrimSpace(payload.SrcID)
	payload.DstID = strings.TrimSpace(payload.DstID)
	payload.Kind = strings.TrimSpace(payload.Kind)
	if payload.SrcID == "" || payload.DstID == "" || payload.Kind == "" {
		return fmt.Errorf("relationship_add payload requires src_id, dst_id, and kind")
	}
	if payload.EdgeID == "" {
		payload.EdgeID = ids.Stable("edge", "manual", payload.SrcID, payload.Kind, payload.DstID)
	}
	if payload.Confidence <= 0 {
		payload.Confidence = 1
	}
	if strings.TrimSpace(payload.MetadataJSON) == "" {
		payload.MetadataJSON = "{}"
	}
	if !json.Valid([]byte(payload.MetadataJSON)) {
		return fmt.Errorf("relationship_add metadata_json must be valid JSON")
	}

	_, err := tx.ExecContext(ctx, `
insert into edges (id, src_id, dst_id, kind, confidence, provenance, evidence_section_id, source_revision, metadata_json)
values (?, ?, ?, ?, ?, 'manual', nullif(?, ''), ?, ?)
on conflict(id) do update set
  src_id = excluded.src_id,
  dst_id = excluded.dst_id,
  kind = excluded.kind,
  confidence = excluded.confidence,
  provenance = excluded.provenance,
  evidence_section_id = excluded.evidence_section_id,
  source_revision = excluded.source_revision,
  metadata_json = excluded.metadata_json,
  updated_at = current_timestamp
`, payload.EdgeID, payload.SrcID, payload.DstID, payload.Kind, payload.Confidence, payload.EvidenceSectionID, payload.SourceRevision, payload.MetadataJSON)
	if err != nil {
		return err
	}
	return replaceEdgeMembershipsTx(ctx, tx, payload.EdgeID)
}

func applyNodeMerge(ctx context.Context, tx *sql.Tx, input domain.FeedbackEventInput) error {
	var payload struct {
		MergedInto string `json:"merged_into"`
	}
	if err := json.Unmarshal([]byte(input.PayloadJSON), &payload); err != nil {
		return fmt.Errorf("parse node_merge payload: %w", err)
	}
	payload.MergedInto = strings.TrimSpace(payload.MergedInto)
	if payload.MergedInto == "" {
		return fmt.Errorf("node_merge payload requires merged_into")
	}
	if payload.MergedInto == input.TargetID {
		return fmt.Errorf("node_merge merged_into must be different from target_id")
	}
	edgeID := ids.Stable("edge", "manual", input.TargetID, "merged_into", payload.MergedInto)
	_, err := tx.ExecContext(ctx, `
insert into edges (id, src_id, dst_id, kind, confidence, provenance, metadata_json)
values (?, ?, ?, 'merged_into', 1, 'manual', ?)
on conflict(id) do update set
  src_id = excluded.src_id,
  dst_id = excluded.dst_id,
  kind = excluded.kind,
  confidence = excluded.confidence,
  provenance = excluded.provenance,
  metadata_json = excluded.metadata_json,
  updated_at = current_timestamp
`, edgeID, input.TargetID, payload.MergedInto, `{"feedback_kind":"node_merge"}`)
	if err != nil {
		return err
	}
	return replaceEdgeMembershipsTx(ctx, tx, edgeID)
}

func (s *Store) getFeedbackEvent(ctx context.Context, id string) (domain.FeedbackEvent, error) {
	var event domain.FeedbackEvent
	err := s.readDB().QueryRowContext(ctx, `
select id, target_kind, target_id, feedback_kind, payload_json, actor, created_at
from feedback_events
where id = ?
`, id).Scan(&event.ID, &event.TargetKind, &event.TargetID, &event.FeedbackKind, &event.PayloadJSON, &event.Actor, &event.CreatedAt)
	if err != nil {
		return domain.FeedbackEvent{}, err
	}
	return event, nil
}

// DeleteFeedbackEvent removes a feedback event by ID.
func (s *Store) DeleteFeedbackEvent(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, "delete from feedback_events where id = ?", id)
	if err != nil {
		return fmt.Errorf("delete feedback event %q: %w", id, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete feedback event %q: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("feedback event %q not found", id)
	}
	return nil
}

func (s *Store) ListFeedbackEvents(ctx context.Context, opts domain.FeedbackListOptions) ([]domain.FeedbackEvent, error) {
	targetKind := strings.TrimSpace(opts.TargetKind)
	targetID := strings.TrimSpace(opts.TargetID)
	feedbackKind := strings.TrimSpace(opts.FeedbackKind)
	limit := opts.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}

	rows, err := s.readDB().QueryContext(ctx, `
select id, target_kind, target_id, feedback_kind, payload_json, actor, created_at
from feedback_events
where (? = '' or target_kind = ?)
  and (? = '' or target_id = ?)
  and (? = '' or feedback_kind = ?)
order by rowid desc
limit ?
`, targetKind, targetKind, targetID, targetID, feedbackKind, feedbackKind, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	events := make([]domain.FeedbackEvent, 0)
	for rows.Next() {
		var event domain.FeedbackEvent
		if err := rows.Scan(&event.ID, &event.TargetKind, &event.TargetID, &event.FeedbackKind, &event.PayloadJSON, &event.Actor, &event.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) RecordQueryObservation(ctx context.Context, input domain.QueryObservationInput) error {
	input.QueryText = strings.TrimSpace(input.QueryText)
	input.NormalizedQuery = strings.TrimSpace(input.NormalizedQuery)
	if input.NormalizedQuery == "" {
		input.NormalizedQuery = strings.ToLower(input.QueryText)
	}
	if input.Source == "" {
		input.Source = "api"
	}
	if input.ID == "" {
		input.ID = ids.Random("qry", 12)
	}
	queryHash := queryHash(input.NormalizedQuery)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
insert into query_events (id, query_hash, normalized_query, query_text, source, result_count, latency_ms, cache_hit)
values (?, ?, ?, ?, ?, ?, ?, ?)
`, input.ID, queryHash, input.NormalizedQuery, input.QueryText, input.Source, input.ResultCount, input.LatencyMS, boolInt(input.CacheHit)); err != nil {
		return err
	}
	for i, result := range input.Results {
		if strings.TrimSpace(result.DocumentID) == "" || strings.TrimSpace(result.SectionID) == "" {
			continue
		}
		rank := result.Rank
		if rank <= 0 {
			rank = i + 1
		}
		if _, err := tx.ExecContext(ctx, `
insert into search_result_events (id, query_event_id, document_id, section_id, rank, score)
values (?, ?, ?, ?, ?, ?)
`, ids.Random("qres", 12), input.ID, result.DocumentID, result.SectionID, rank, result.Score); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func queryHash(query string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(query))))
	return hex.EncodeToString(sum[:])
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (s *Store) SearchSections(ctx context.Context, query string, limit int) ([]domain.SearchHit, error) {
	result, err := s.SearchSectionsWithOptions(ctx, domain.SearchOptions{
		Query:                  query,
		Limit:                  limit,
		MaxSearches:            5,
		MaxSectionsPerDocument: 5,
		ProfileDetail:          "compact",
	})
	if err != nil {
		return nil, err
	}
	return result.Hits, nil
}

func (s *Store) SearchSectionsWithOptions(ctx context.Context, opts domain.SearchOptions) (domain.SearchResult, error) {
	opts.Query = strings.TrimSpace(opts.Query)
	if opts.Query == "" {
		return domain.SearchResult{Query: opts.Query}, nil
	}
	exactTerms := normalizeExactTerms(opts.ExactTerms)
	opts.OriginalQuery = strings.TrimSpace(opts.OriginalQuery)
	if opts.OriginalQuery == "" {
		opts.OriginalQuery = opts.Query
	}
	if opts.Limit <= 0 || opts.Limit > 200 {
		opts.Limit = 20
	}
	if opts.MaxSearches <= 0 {
		opts.MaxSearches = 5
	}
	if opts.MaxSearches > 5 {
		opts.MaxSearches = 5
	}
	if opts.MaxSectionsPerDocument <= 0 {
		opts.MaxSectionsPerDocument = 2
	}
	if opts.MaxSectionsPerDocument > 5 {
		opts.MaxSectionsPerDocument = 5
	}
	if opts.ProfileDetail == "" {
		opts.ProfileDetail = "compact"
	}
	if opts.ProfileDetail != "none" && opts.ProfileDetail != "compact" && opts.ProfileDetail != "full" {
		opts.ProfileDetail = "compact"
	}
	if opts.MaxCharsPerResult < 0 {
		opts.MaxCharsPerResult = 0
	}
	if opts.MaxCharsPerResult > 4000 {
		opts.MaxCharsPerResult = 4000
	}
	if opts.MaxMediaAssetsPerResult < 0 {
		opts.MaxMediaAssetsPerResult = 0
	}
	if opts.MaxMediaAssetsTotal < 0 {
		opts.MaxMediaAssetsTotal = 0
	}
	opts.MediaDetail = strings.ToLower(strings.TrimSpace(opts.MediaDetail))
	if opts.MediaDetail == "" {
		opts.MediaDetail = "full"
	}
	if opts.MediaDetail != "full" && opts.MediaDetail != "compact" && opts.MediaDetail != "none" {
		opts.MediaDetail = "full"
	}
	if opts.RelationDepth <= 0 || opts.RelationDepth > 1 {
		opts.RelationDepth = 1
	}

	// ===== Phase 1: Intent routing =====
	weights := s.vectorRuntime.IntentRouteWeights
	intent := RouteIntent(opts.OriginalQuery, opts.SemanticIntents, weights)
	if !s.canUseVectorSearch() {
		intent.WVector = 0
	}

	// ===== Phase 2: Text lane retrieval (existing cascade logic) =====
	collector := newSearchCollector(opts)
	attempts := make([]domain.SearchAttempt, 0, opts.MaxSearches)

	runAttempt := func(kind string, query string, terms []string, required bool, fn func() ([]domain.SearchHit, error)) error {
		if !required && len(attempts) >= opts.MaxSearches {
			return nil
		}
		hits, err := fn()
		if err != nil {
			return err
		}
		attempts = append(attempts, domain.SearchAttempt{Kind: kind, Query: query, Terms: terms, Hits: len(hits)})
		collector.add(kind, hits)
		return nil
	}

	queryTerms := classifyQueryTerms(opts.Query)
	queryEntities := extractQueryEntities(opts.Query)
	entityExactTerms := entitySearchTerms(queryEntities)
	if len(entityExactTerms) > 0 {
		entityQuery := matchQueryFromTerms(entityExactTerms, 16)
		if err := runAttempt("entity_exact", entityQuery, entityExactTerms, false, func() ([]domain.SearchHit, error) {
			return s.searchSectionsEntity(ctx, entityQuery, opts.Limit*3, opts, entityMatchExact)
		}); err != nil {
			return domain.SearchResult{}, err
		}
		if err := runAttempt("entity_normalized", entityQuery, entityExactTerms, false, func() ([]domain.SearchHit, error) {
			return s.searchSectionsEntity(ctx, entityQuery, opts.Limit*3, opts, entityMatchNormalized)
		}); err != nil {
			return domain.SearchResult{}, err
		}
	}
	unicodeTerms := strongQueryTermTexts(queryTerms)
	tokenQuery := matchQueryFromTerms(unicodeTerms, 16)
	if err := runAttempt("unicode61", tokenQuery, unicodeTerms, false, func() ([]domain.SearchHit, error) {
		return s.searchSectionsTokenFTS(ctx, tokenQuery, opts.Limit*3, opts)
	}); err != nil {
		return domain.SearchResult{}, err
	}

	trigramQuery := trigramFTSQuery(opts.Query)
	if err := runAttempt("trigram", trigramQuery, nil, false, func() ([]domain.SearchHit, error) {
		return s.searchSectionsTokenTrigram(ctx, opts.Query, opts.Limit*3, opts)
	}); err != nil {
		return domain.SearchResult{}, err
	}

	if !strings.EqualFold(opts.OriginalQuery, opts.Query) {
		originalTerms := strongQueryTermTexts(classifyQueryTerms(opts.OriginalQuery))
		originalTokenQuery := matchQueryFromTerms(originalTerms, 16)
		if err := runAttempt("original_unicode61", originalTokenQuery, originalTerms, true, func() ([]domain.SearchHit, error) {
			originalOpts := opts
			originalOpts.Query = opts.OriginalQuery
			return s.searchSectionsTokenFTS(ctx, originalTokenQuery, opts.Limit*3, originalOpts)
		}); err != nil {
			return domain.SearchResult{}, err
		}
		if err := runAttempt("original_trigram", trigramFTSQuery(opts.OriginalQuery), nil, true, func() ([]domain.SearchHit, error) {
			originalOpts := opts
			originalOpts.Query = opts.OriginalQuery
			return s.searchSectionsTokenTrigram(ctx, opts.OriginalQuery, opts.Limit*3, originalOpts)
		}); err != nil {
			return domain.SearchResult{}, err
		}
	}
	if len(exactTerms) > 0 {
		if err := runAttempt("exact_terms", "", exactTerms, true, func() ([]domain.SearchHit, error) {
			return s.searchSectionsExactTerms(ctx, exactTerms, opts.Limit*3, opts)
		}); err != nil {
			return domain.SearchResult{}, err
		}
	}

	if len(collector.hits) == 0 {
		profileTerms := profileSearchTerms(opts.Query)
		if len(profileTerms) > 0 {
			if err := runAttempt("profile_fallback", "", profileTerms, false, func() ([]domain.SearchHit, error) {
				return s.searchSectionsProfile(ctx, profileTerms, opts.Limit*2, opts)
			}); err != nil {
				return domain.SearchResult{}, err
			}
		}
	}
	if len(collector.hits) == 0 {
		terms := substringSearchTerms(opts.Query)
		if len(terms) > 0 {
			if err := runAttempt("substring_fallback", "", terms, true, func() ([]domain.SearchHit, error) {
				return s.searchSectionsExactTerms(ctx, terms, opts.Limit*2, opts)
			}); err != nil {
				return domain.SearchResult{}, err
			}
		}
	}
	// ===== Phase 2b: Vector lane retrieval =====
	var vectorHits []domain.SearchHit
	var vectorAttempt domain.SearchAttempt
	vectorCandidates := 0
	if s.canUseVectorSearch() && intent.WVector > 0 {
		vectorHits, vectorAttempt = s.searchSectionsVector(ctx, opts)
		vectorCandidates = vectorAttempt.Hits
		attempts = append(attempts, vectorAttempt)
	}

	// ===== Phase 3: Two-lane independent ranking =====
	textResults := collector.results()
	textRanked := assignRanks(textResults,
		func(h domain.SearchHit) float64 {
			if h.ScoreBreakdown != nil {
				return h.ScoreBreakdown.Total
			}
			return h.Rank
		},
		func(h domain.SearchHit) []string {
			if h.QueryMatch != nil {
				return h.QueryMatch.SearchAttempts
			}
			return nil
		})

	vectorRanked := assignRanks(vectorHits,
		func(h domain.SearchHit) float64 {
			if h.ScoreBreakdown != nil {
				return h.ScoreBreakdown.VectorBoost // raw similarity in RRF mode
			}
			return 0
		},
		func(_ domain.SearchHit) []string { return []string{"vector"} })

	// ===== Phase 4: Weighted RRF fusion =====
	rrfK := weights.RRFK
	if rrfK <= 0 {
		rrfK = 60
	}
	contribs := weightedRRFFusion(textRanked, vectorRanked, intent.WText, intent.WVector, rrfK)

	// ===== Phase 5: Business multiplier calibration =====
	// Build lookup maps for merging hits
	textHitsBySection := map[string]*domain.SearchHit{}
	for i := range textResults {
		textHitsBySection[textResults[i].SectionID] = &textResults[i]
	}
	vectorHitsBySection := map[string]*domain.SearchHit{}
	for i := range vectorHits {
		vectorHitsBySection[vectorHits[i].SectionID] = &vectorHits[i]
	}

	// Set DocumentID on RRFContribution for per-doc cap enforcement
	for sectionID, contrib := range contribs {
		hit := mergeHitForSection(sectionID, textHitsBySection, vectorHitsBySection)
		contrib.DocumentID = hit.DocumentID
	}

	applyMultiplierCalibration(contribs, textHitsBySection) // canonical/stale multiplier

	// ===== Phase 6: Build final hits =====
	hits := buildFinalHits(contribs, textHitsBySection, vectorHitsBySection, opts.Limit, opts.MaxSectionsPerDocument)

	if opts.UseRelationExpansion && len(hits) > 0 {
		var err error
		hits, err = s.expandSearchHitsWithRelations(ctx, hits, opts)
		if err != nil {
			return domain.SearchResult{}, err
		}
	}
	mediaSummary, err := s.hydrateSearchHitWorkbookFields(ctx, hits, opts)
	if err != nil {
		return domain.SearchResult{}, err
	}

	result := domain.SearchResult{
		Query:        opts.Query,
		SearchesUsed: len(attempts),
		Attempts:     attempts,
		Hits:         hits,
		MediaSummary: &mediaSummary,
		HybridSearchMeta: &domain.HybridSearchMeta{
			IntentRoute:         intent.Route,
			WText:               intent.WText,
			WVector:             intent.WVector,
			RRFK:                rrfK,
			TextCandidates:      len(textRanked),
			VectorCandidates:    vectorCandidates,
			VectorMinSimilarity: s.vectorRuntime.MinSimilarity,
		},
		SuggestedReads: domain.SuggestedReads{
			ExplicitReferences:  []domain.ExplicitReference{},
			ImplicitSymbolLinks: []domain.SuggestedRead{},
			CuratedRelations:    []domain.SuggestedRead{},
			StructuralNeighbors: []domain.SuggestedRead{},
		},
	}
	if err := s.enrichSearchResultExplicitReferences(ctx, &result); err != nil {
		return domain.SearchResult{}, err
	}
	return result, nil
}

func (s *Store) hydrateSearchHitWorkbookFields(ctx context.Context, hits []domain.SearchHit, opts domain.SearchOptions) (domain.SearchMediaSummary, error) {
	summary := domain.SearchMediaSummary{
		PerResultLimit: opts.MaxMediaAssetsPerResult,
		TotalLimit:     opts.MaxMediaAssetsTotal,
		Detail:         opts.MediaDetail,
	}
	if len(hits) == 0 {
		return summary, nil
	}
	sectionIDs := make([]string, 0, len(hits))
	bySection := make(map[string][]int, len(hits))
	for i := range hits {
		if hits[i].SectionID == "" {
			continue
		}
		if len(bySection[hits[i].SectionID]) == 0 {
			sectionIDs = append(sectionIDs, hits[i].SectionID)
		}
		bySection[hits[i].SectionID] = append(bySection[hits[i].SectionID], i)
	}
	if len(sectionIDs) == 0 {
		return summary, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(sectionIDs)), ",")
	args := make([]any, len(sectionIDs))
	for i, id := range sectionIDs {
		args[i] = id
	}
	rows, err := s.readDB().QueryContext(ctx, `
select s.id, d.source_id,
		       coalesce((select sn.node_id from section_nodes sn join nodes n on n.id = sn.node_id
		                 where sn.section_id = s.id and n.kind = 'DocSection'
		                 order by case when sn.role = 'represents' then 0 else 1 end, sn.node_id limit 1), ''),
		       coalesce(ss.element_kind, ''), coalesce(ss.display_number, ''), s.metadata_json, s.search_text,
		       (select count(distinct e.id)
		        from section_nodes authored_sn
		        join edges e on e.src_id = authored_sn.node_id or e.dst_id = authored_sn.node_id
		        where authored_sn.section_id = s.id and e.provenance = 'source_authored'
		          and e.kind in ('related_to', 'links_to')),
		       coalesce((select group_concat(e.metadata_json, char(10))
		        from section_nodes authored_sn
		        join edges e on e.src_id = authored_sn.node_id or e.dst_id = authored_sn.node_id
		        where authored_sn.section_id = s.id and e.provenance = 'source_authored'
		          and e.kind in ('related_to', 'links_to')), '')
from sections s join documents d on d.id = s.document_id
left join section_structures ss on ss.section_id = s.id
where s.id in (`+placeholders+`)
`, args...)
	if err != nil {
		return summary, err
	}
	for rows.Next() {
		var sectionID, sourceID, nodeID, elementKind, displayNumber, metadataJSON, searchText, authoredMetadata string
		var authoredRelationCount int
		if err := rows.Scan(&sectionID, &sourceID, &nodeID, &elementKind, &displayNumber, &metadataJSON, &searchText, &authoredRelationCount, &authoredMetadata); err != nil {
			_ = rows.Close()
			return summary, err
		}
		for _, idx := range bySection[sectionID] {
			hits[idx].SourceID = sourceID
			hits[idx].NodeID = nodeID
			hits[idx].ElementKind = elementKind
			hits[idx].DisplayNumber = displayNumber
			hits[idx].AuthoredRelationCount = authoredRelationCount
			hits[idx].EvidenceKind = classifySearchHitEvidence(opts.Query, hits[idx], metadataJSON, searchText, authoredMetadata, nil, false)
		}
	}
	if err := rows.Close(); err != nil {
		return summary, err
	}
	ancestorRows, err := s.readDB().QueryContext(ctx, `
with recursive ancestry(origin_id, section_id) as (
  select section_id, parent_section_id from section_structures
  where section_id in (`+placeholders+`) and parent_section_id is not null
  union all
  select a.origin_id, ss.parent_section_id
  from ancestry a join section_structures ss on ss.section_id = a.section_id
  where ss.parent_section_id is not null
)
select a.origin_id, s.id, s.title, coalesce(ss.display_number, ''), coalesce(ss.depth, 0)
from ancestry a join sections s on s.id = a.section_id
left join section_structures ss on ss.section_id = s.id
order by a.origin_id, ss.depth, ss.sibling_ordinal, s.id
`, args...)
	if err != nil {
		return summary, err
	}
	ancestry := make(map[string][]domain.SectionBreadcrumb)
	for ancestorRows.Next() {
		var originID string
		var item domain.SectionBreadcrumb
		if err := ancestorRows.Scan(&originID, &item.SectionID, &item.Title, &item.DisplayNumber, &item.Depth); err != nil {
			_ = ancestorRows.Close()
			return summary, err
		}
		ancestry[originID] = append(ancestry[originID], item)
	}
	if err := ancestorRows.Close(); err != nil {
		return summary, err
	}
	media, mediaTotals, mediaMatches, err := s.searchSectionMediaAssets(ctx, sectionIDs, opts)
	if err != nil {
		return summary, err
	}
	for _, sectionID := range sectionIDs {
		summary.Total += mediaTotals[sectionID]
		summary.Returned += len(media[sectionID])
	}
	summary.Truncated = summary.Returned < summary.Total
	for sectionID, indices := range bySection {
		for _, idx := range indices {
			hits[idx].Ancestry = ancestry[sectionID]
			hits[idx].MediaAssets = media[sectionID]
			hits[idx].MediaAssetsTotal = mediaTotals[sectionID]
			hits[idx].MediaAssetsTruncated = len(media[sectionID]) < mediaTotals[sectionID]
			if evidence := classifySearchHitEvidence(opts.Query, hits[idx], "", "", "", media[sectionID], mediaMatches[sectionID]); evidence == "media_metadata" {
				hits[idx].EvidenceKind = evidence
			}
		}
	}
	return summary, nil
}

func (s *Store) searchSectionMediaAssets(ctx context.Context, sectionIDs []string, opts domain.SearchOptions) (map[string][]domain.MediaAssetSummary, map[string]int, map[string]bool, error) {
	assets := make(map[string][]domain.MediaAssetSummary, len(sectionIDs))
	totals := make(map[string]int, len(sectionIDs))
	matches := make(map[string]bool, len(sectionIDs))
	ids := uniqueStrings(sectionIDs)
	if len(ids) == 0 {
		return assets, totals, matches, nil
	}
	if len(ids) > 500 {
		return nil, nil, nil, fmt.Errorf("too many section ids: %d", len(ids))
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	query := strings.ToLower(strings.TrimSpace(opts.Query))
	aggregateArgs := make([]any, 0, len(ids)+1)
	aggregateArgs = append(aggregateArgs, query)
	for _, id := range ids {
		aggregateArgs = append(aggregateArgs, id)
		assets[id] = []domain.MediaAssetSummary{}
	}
	rows, err := s.readDB().QueryContext(ctx, `
with search(term) as (values (?))
select smr.section_id, count(*),
       max(case when search.term <> '' and (
         instr(lower(ma.original_name), search.term) > 0
         or instr(lower(ma.media_type), search.term) > 0
         or instr(lower(ma.metadata_json), search.term) > 0
         or instr(lower(smr.metadata_json), search.term) > 0
       ) then 1 else 0 end)
from section_media_refs smr
join media_assets ma on ma.id = smr.asset_id
cross join search
where smr.section_id in (`+placeholders+`)
group by smr.section_id
`, aggregateArgs...)
	if err != nil {
		return nil, nil, nil, err
	}
	for rows.Next() {
		var sectionID string
		var total, matched int
		if err := rows.Scan(&sectionID, &total, &matched); err != nil {
			_ = rows.Close()
			return nil, nil, nil, err
		}
		totals[sectionID] = total
		matches[sectionID] = matched != 0
	}
	if err := rows.Close(); err != nil {
		return nil, nil, nil, err
	}
	if opts.MediaDetail == "none" {
		return assets, totals, matches, nil
	}

	requestedRows := make([]string, len(ids))
	assetArgs := make([]any, 0, len(ids)*2+4)
	for i, id := range ids {
		requestedRows[i] = "(?, ?)"
		assetArgs = append(assetArgs, id, i)
	}
	assetMetadata := "''"
	referenceMetadata := "''"
	if opts.MediaDetail == "full" {
		assetMetadata = "ma.metadata_json"
		referenceMetadata = "smr.metadata_json"
	}
	assetArgs = append(assetArgs,
		opts.MaxMediaAssetsPerResult, opts.MaxMediaAssetsPerResult,
		opts.MaxMediaAssetsTotal, opts.MaxMediaAssetsTotal,
	)
	assetRows, err := s.readDB().QueryContext(ctx, `
with requested(section_id, priority) as (values `+strings.Join(requestedRows, ",")+`),
ranked as (
  select requested.priority, smr.section_id, ma.id, ma.kind, ma.original_name, ma.media_type,
         ma.size_bytes, ma.status, smr.role, smr.ordinal,
         `+assetMetadata+` as asset_metadata_json,
         `+referenceMetadata+` as reference_metadata_json,
         row_number() over (
           partition by smr.section_id
           order by smr.ordinal, ma.id, smr.role
         ) as per_result_rank
  from requested
  join section_media_refs smr on smr.section_id = requested.section_id
  join media_assets ma on ma.id = smr.asset_id
),
per_result as (
  select * from ranked where ? = 0 or per_result_rank <= ?
),
globally_ranked as (
  select per_result.*,
         row_number() over (
           order by priority, per_result_rank, section_id, id, role
         ) as global_rank
  from per_result
)
select section_id, id, kind, original_name, media_type, size_bytes, status,
       role, ordinal, asset_metadata_json, reference_metadata_json
from globally_ranked
where ? = 0 or global_rank <= ?
order by priority, per_result_rank, section_id, id, role
`, assetArgs...)
	if err != nil {
		return nil, nil, nil, err
	}
	defer assetRows.Close()
	for assetRows.Next() {
		var sectionID string
		var asset domain.MediaAssetSummary
		if err := assetRows.Scan(&sectionID, &asset.ID, &asset.Kind, &asset.OriginalName, &asset.MediaType,
			&asset.SizeBytes, &asset.Status, &asset.Role, &asset.Ordinal, &asset.MetadataJSON, &asset.ReferenceMetadataJSON); err != nil {
			return nil, nil, nil, err
		}
		assets[sectionID] = append(assets[sectionID], asset)
	}
	if err := assetRows.Err(); err != nil {
		return nil, nil, nil, err
	}
	return assets, totals, matches, nil
}

func classifySearchHitEvidence(query string, hit domain.SearchHit, metadataJSON, searchText, authoredMetadata string, media []domain.MediaAssetSummary, mediaMatched bool) string {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return ""
	}
	contains := func(value string) bool { return strings.Contains(strings.ToLower(value), query) }
	if contains(hit.Title) {
		return "title"
	}
	if contains(hit.DisplayNumber) {
		return "numbering"
	}
	if mediaMatched {
		return "media_metadata"
	}
	for _, asset := range media {
		if contains(asset.OriginalName) || contains(asset.MediaType) || contains(asset.MetadataJSON) || contains(asset.ReferenceMetadataJSON) {
			return "media_metadata"
		}
	}
	if contains(authoredMetadata) {
		return "relationship_label"
	}
	if metadataJSON != "" {
		var metadata map[string]json.RawMessage
		if json.Unmarshal([]byte(metadataJSON), &metadata) == nil {
			for _, item := range []struct {
				key      string
				evidence string
			}{
				{key: "notes", evidence: "note"},
				{key: "labels", evidence: "label"},
				{key: "markers", evidence: "marker"},
				{key: "task", evidence: "task"},
				{key: "links", evidence: "link"},
			} {
				if contains(string(metadata[item.key])) {
					return item.evidence
				}
			}
		}
	}
	if contains(hit.HeadingPath) {
		return "ancestor_path"
	}
	if contains(hit.Content) || contains(searchText) {
		return "source_text"
	}
	return "search_match"
}

type entityMatchMode string

const (
	entityMatchExact      entityMatchMode = "entity_exact"
	entityMatchNormalized entityMatchMode = "entity_normalized"
)

func (s *Store) searchSectionsEntity(ctx context.Context, matchQuery string, limit int, opts domain.SearchOptions, mode entityMatchMode) ([]domain.SearchHit, error) {
	hits, err := s.searchSectionsStoredEntities(ctx, opts, mode, limit)
	if err != nil {
		return nil, err
	}
	if len(hits) > 0 {
		return hits, nil
	}
	return s.searchSectionsEntityFTS(ctx, matchQuery, limit, opts, mode)
}

func (s *Store) canUseVectorSearch() bool {
	return s.vectorRuntime.Embedder != nil && strings.TrimSpace(s.vectorRuntime.Embedder.Model()) != ""
}

func (s *Store) searchSectionsVector(ctx context.Context, opts domain.SearchOptions) ([]domain.SearchHit, domain.SearchAttempt) {
	attempt := domain.SearchAttempt{Kind: "vector", Query: vectorQueryLabel(opts)}
	weight := s.vectorRuntime.SearchWeight
	if weight <= 0 {
		weight = 0.4
	}
	if weight > 1 {
		weight = 1
	}
	limit := s.vectorRuntime.VectorCandidates
	if limit <= 0 {
		limit = opts.Limit * 3
	}
	if limit <= 0 {
		limit = 60
	}
	text := strings.Join(nonEmptyStrings(append([]string{opts.Query}, opts.SemanticIntents...)), "\n")
	if strings.TrimSpace(text) == "" {
		return nil, attempt
	}
	vectors, err := s.vectorRuntime.Embedder.Embed(ctx, []string{text})
	if err != nil {
		attempt.Error = "embedding_failed: " + truncateForSearchAttempt(err.Error(), 160)
		return nil, attempt
	}
	if len(vectors) == 0 || len(vectors[0]) == 0 {
		attempt.Error = "embedding_empty"
		return nil, attempt
	}
	model := s.vectorRuntime.Embedder.Model()
	vectorHits, err := s.SearchChunksByVector(ctx, vectors[0], model, limit, s.vectorRuntime.MinSimilarity, vectorstore.EmbeddingPlanFilter{
		GeneratorVersion: strings.TrimSpace(s.vectorRuntime.GeneratorVersion),
		Tokenizer:        embeddingchunk.NewTextMeasurer(s.vectorRuntime.Tokenizer, model).Name(),
		ChunkStrategy:    strings.TrimSpace(s.vectorRuntime.ChunkStrategy),
	})
	if err != nil {
		attempt.Error = "vector_search_failed: " + truncateForSearchAttempt(err.Error(), 160)
		return nil, attempt
	}
	hits := make([]domain.SearchHit, 0, len(vectorHits))
	seenSections := map[string]bool{}
	for _, vectorHit := range vectorHits {
		if seenSections[vectorHit.SectionID] {
			continue
		}
		hit, ok, err := s.hydrateAndValidateVectorHit(ctx, vectorHit, opts, weight)
		if err != nil {
			attempt.Error = "vector_hydrate_failed: " + truncateForSearchAttempt(err.Error(), 160)
			return hits, attempt
		}
		if !ok {
			continue
		}
		seenSections[vectorHit.SectionID] = true
		hits = append(hits, hit)
	}
	sortSearchHits(hits)
	attempt.Hits = len(hits)
	return hits, attempt
}

func (s *Store) hydrateAndValidateVectorHit(ctx context.Context, vectorHit domain.VectorSearchHit, opts domain.SearchOptions, weight float64) (domain.SearchHit, bool, error) {
	var hit domain.SearchHit
	var profileJSON string
	err := s.readDB().QueryRowContext(ctx, `
select sections.id, sections.document_id, documents.source_id, documents.title, documents.url,
       sections.content_hash,
       coalesce(document_profiles."desc", ''),
       coalesce(document_profiles.retrieval_profile_json, '{}'),
       exists (
         select 1 from feedback_events fe
         where fe.target_kind = 'document'
           and fe.target_id = documents.id
           and fe.feedback_kind = 'document_canonical'
       ) as canonical,
       sections.title, sections.heading_path, sections.content
from sections
join documents on documents.id = sections.document_id
join sources on sources.id = documents.source_id
left join document_profiles on document_profiles.document_id = documents.id
where sections.id = ?
  and documents.id = ?
  and sources.id = ?
  and not exists (
    select 1 from feedback_events fe
    where fe.target_kind = 'document'
      and fe.target_id = documents.id
      and fe.feedback_kind = 'document_stale'
  )
`, vectorHit.SectionID, vectorHit.DocumentID, vectorHit.SourceID).Scan(&hit.SectionID, &hit.DocumentID, &hit.SourceID, &hit.DocumentTitle, &hit.DocumentURL, &hit.ContentHash, &hit.Desc, &profileJSON, &hit.Canonical, &hit.Title, &hit.HeadingPath, &hit.Content)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.SearchHit{}, false, nil
	}
	if err != nil {
		return domain.SearchHit{}, false, err
	}
	contentHash := vectorHit.SectionContentHash
	if contentHash == "" {
		contentHash = vectorHit.ContentHash
	}
	if contentHash != hit.ContentHash {
		return domain.SearchHit{}, false, nil
	}
	terms := searchTerms(opts.OriginalQuery)
	if len(terms) == 0 {
		terms = searchTerms(opts.Query)
	}
	if strings.TrimSpace(vectorHit.ChunkText) != "" {
		hit.Snippet = bestSnippet(vectorHit.ChunkText, terms)
	} else {
		hit.Snippet = bestSnippet(hit.Content, terms)
	}
	enrichSearchHit(&hit, opts, profileJSON, terms, "vector")
	breakdown := weightedScoreBreakdown(hit, opts.OriginalQuery, terms, 0, "vector")
	breakdown.VectorBoost = vectorHit.Similarity // RRF mode: store raw similarity for transparency
	breakdown.MatchedFields = uniqueStrings(append(breakdown.MatchedFields, "vector"))
	breakdown.StaleEmbeddingPenalty = 0
	breakdown.Total = scoreBreakdownTotal(breakdown)
	hit.Rank = breakdown.Total
	hit.ScoreBreakdown = &breakdown
	hit.Trace = &domain.SearchHitTrace{
		SourceID:             vectorHit.SourceID,
		DocumentID:           vectorHit.DocumentID,
		SectionID:            vectorHit.SectionID,
		ChunkID:              vectorHit.ChunkID,
		ChunkOrdinal:         vectorHit.ChunkOrdinal,
		ContentHash:          contentHash,
		EmbeddingModel:       vectorHit.Model,
		EmbeddingTextHash:    vectorHit.EmbeddingTextHash,
		ChunkTextHash:        vectorHit.ChunkTextHash,
		EmbeddingGeneratedAt: vectorHit.GeneratedAt,
		GeneratorVersion:     vectorHit.GeneratorVersion,
		Tokenizer:            vectorHit.Tokenizer,
		ChunkStrategy:        vectorHit.ChunkStrategy,
		VectorTraceValid:     true,
	}
	hit.EvidenceLevel = evidenceLevelForBreakdown(breakdown)
	if hit.QueryMatch != nil {
		applyScoreBreakdownToQueryMatch(hit.QueryMatch, breakdown)
	}
	return hit, true, nil
}

func (s *Store) searchSectionsStoredEntities(ctx context.Context, opts domain.SearchOptions, mode entityMatchMode, limit int) ([]domain.SearchHit, error) {
	queryEntities := extractQueryEntities(opts.Query)
	if len(queryEntities) == 0 || limit <= 0 {
		return nil, nil
	}
	terms := entitySearchTerms(queryEntities)
	seen := map[string]bool{}
	hits := make([]domain.SearchHit, 0)
	for _, term := range terms {
		entities, err := s.searchEntitiesExact(ctx, term, limit)
		if err != nil {
			return nil, err
		}
		for _, entity := range entities {
			if len(hits) >= limit {
				return hits, nil
			}
			if seen[entity.SectionID] {
				continue
			}
			match := storedEntityMatchForQuery(entity, queryEntities)
			if mode == entityMatchExact && !match.Exact {
				continue
			}
			if mode == entityMatchNormalized && !match.Normalized {
				continue
			}
			hit, err := s.sectionHitForStoredEntity(ctx, entity, opts, mode, match)
			if err != nil {
				return nil, err
			}
			seen[entity.SectionID] = true
			hits = append(hits, hit)
		}
	}
	sortSearchHits(hits)
	return hits, nil
}

func (s *Store) sectionHitForStoredEntity(ctx context.Context, entity domain.SectionEntity, opts domain.SearchOptions, mode entityMatchMode, match entityMatchResult) (domain.SearchHit, error) {
	var hit domain.SearchHit
	var profileJSON string
	err := s.readDB().QueryRowContext(ctx, `
select sections.id, sections.document_id, documents.title, documents.url,
       coalesce(document_profiles."desc", ''),
       coalesce(document_profiles.retrieval_profile_json, '{}'),
       exists (
         select 1 from feedback_events fe
         where fe.target_kind = 'document'
           and fe.target_id = documents.id
           and fe.feedback_kind = 'document_canonical'
       ) as canonical,
       sections.title, sections.heading_path, sections.content
from sections
join documents on documents.id = sections.document_id
left join document_profiles on document_profiles.document_id = documents.id
where sections.id = ?
  and not exists (
    select 1 from feedback_events fe
    where fe.target_kind = 'document'
      and fe.target_id = documents.id
      and fe.feedback_kind = 'document_stale'
  )
`, entity.SectionID).Scan(&hit.SectionID, &hit.DocumentID, &hit.DocumentTitle, &hit.DocumentURL, &hit.Desc, &profileJSON, &hit.Canonical, &hit.Title, &hit.HeadingPath, &hit.Content)
	if err != nil {
		return domain.SearchHit{}, err
	}
	terms := match.Terms
	if len(terms) == 0 {
		terms = []string{sectionEntityDisplayTerm(entity)}
	}
	hit.Snippet = bestSnippet(hit.Content, terms)
	enrichSearchHit(&hit, opts, profileJSON, terms, string(mode))
	breakdown := domain.ScoreBreakdown{
		MatchedFields: []string{string(mode)},
		MatchedTerms:  uniqueStrings(terms),
	}
	if mode == entityMatchExact {
		breakdown.ExactMatchBoost = 260
	} else {
		breakdown.ExactMatchBoost = 180
	}
	if hit.Canonical {
		breakdown.CanonicalBoost = 30
	}
	hit.MatchedEntities = []domain.MatchedEntity{matchedEntityFromStored(entity, string(mode))}
	breakdown.Total = breakdown.ExactMatchBoost + breakdown.CanonicalBoost
	hit.Rank = breakdown.Total
	hit.ScoreBreakdown = &breakdown
	if hit.QueryMatch != nil {
		applyScoreBreakdownToQueryMatch(hit.QueryMatch, breakdown)
	}
	return hit, nil
}

func (s *Store) searchSectionsEntityFTS(ctx context.Context, matchQuery string, limit int, opts domain.SearchOptions, mode entityMatchMode) ([]domain.SearchHit, error) {
	hits, err := s.searchSectionsTokenFTS(ctx, matchQuery, limit, opts)
	if err != nil {
		return nil, err
	}
	filtered := hits[:0]
	for _, hit := range hits {
		match := entityMatchForHit(hit, opts.Query)
		matched := mode == entityMatchExact && match.Exact
		matched = matched || mode == entityMatchNormalized && match.Normalized
		if !matched {
			continue
		}
		hasStoredEntities, err := s.sectionHasStoredEntities(ctx, hit.SectionID)
		if err != nil {
			return nil, err
		}
		if hasStoredEntities {
			continue
		}
		hit.MatchedEntities = matchedEntitiesWithMode(match.Entities, string(mode))
		filtered = append(filtered, hit)
	}
	return filtered, nil
}

func (s *Store) sectionHasStoredEntities(ctx context.Context, sectionID string) (bool, error) {
	var exists bool
	err := s.readDB().QueryRowContext(ctx, `
select exists (
  select 1 from section_entities
  where section_id = ?
)
`, sectionID).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

func (s *Store) expandSearchHitsWithRelations(ctx context.Context, hits []domain.SearchHit, opts domain.SearchOptions) ([]domain.SearchHit, error) {
	if len(hits) == 0 {
		return hits, nil
	}
	byDocument := map[string]int{}
	for i, hit := range hits {
		if hit.DocumentID != "" {
			byDocument[hit.DocumentID] = i
		}
	}
	documents := make([]string, 0, len(byDocument))
	for documentID := range byDocument {
		documents = append(documents, documentID)
	}
	sort.Strings(documents)

	relationsByDocument, err := s.knowledgeRelationsForDocuments(ctx, documents, opts.RelationTypes)
	if err != nil {
		return nil, err
	}
	for _, documentID := range documents {
		sourceIdx := byDocument[documentID]
		for _, relation := range relationsByDocument[documentID] {
			targetDocumentID, direction, ok := relationTargetForDocument(relation, documentID)
			if !ok {
				continue
			}
			match := domain.RelationMatch{
				RelationID:       relation.ID,
				RelationType:     relation.RelationType,
				SourceDocumentID: documentID,
				TargetDocumentID: targetDocumentID,
				Direction:        direction,
				Effect:           relation.Effect,
				Weight:           relation.Weight,
				Reason:           relation.Reason,
			}
			hits[sourceIdx].RelationMatches = append(hits[sourceIdx].RelationMatches, match)
			if relation.Effect == "ignore" || relation.RelationType == "should_ignore" {
				continue
			}
			if _, exists := byDocument[targetDocumentID]; exists {
				continue
			}
			if len(hits) >= opts.Limit {
				continue
			}
			relatedHit, err := s.firstSectionHitForDocument(ctx, targetDocumentID, opts)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				return nil, err
			}
			relatedHit.RelationMatches = []domain.RelationMatch{match}
			relatedHit.Rank = hits[sourceIdx].Rank + relation.Weight
			if relatedHit.Snippet == "" {
				relatedHit.Snippet = bestSnippet(relatedHit.Content, searchTerms(opts.Query))
			}
			applySearchDetailOptions(&relatedHit, opts)
			byDocument[targetDocumentID] = len(hits)
			hits = append(hits, relatedHit)
		}
	}
	sortSearchHits(hits)
	if len(hits) > opts.Limit {
		hits = hits[:opts.Limit]
	}
	return hits, nil
}

func (s *Store) knowledgeRelationsForDocuments(ctx context.Context, documentIDs []string, relationTypes []string) (map[string][]domain.KnowledgeRelation, error) {
	result := make(map[string][]domain.KnowledgeRelation, len(documentIDs))
	if len(documentIDs) == 0 {
		return result, nil
	}
	allowedTypes := map[string]bool{}
	for _, relationType := range relationTypes {
		relationType = strings.TrimSpace(relationType)
		if relationType != "" {
			allowedTypes[relationType] = true
		}
	}
	for _, documentID := range documentIDs {
		relations, err := s.ListKnowledgeRelations(ctx, domain.KnowledgeRelationListOptions{
			DocumentID:    documentID,
			RelationTypes: relationTypes,
			Limit:         50,
		})
		if err != nil {
			return nil, err
		}
		if len(allowedTypes) > 0 {
			filtered := relations[:0]
			for _, relation := range relations {
				if allowedTypes[relation.RelationType] {
					filtered = append(filtered, relation)
				}
			}
			relations = filtered
		}
		result[documentID] = relations
	}
	return result, nil
}

func relationTargetForDocument(relation domain.KnowledgeRelation, documentID string) (string, string, bool) {
	if relation.FromDocumentID == documentID {
		return relation.ToDocumentID, "out", true
	}
	if relation.ToDocumentID == documentID && relation.Direction == "undirected" {
		return relation.FromDocumentID, "in", true
	}
	return "", "", false
}

func (s *Store) firstSectionHitForDocument(ctx context.Context, documentID string, opts domain.SearchOptions) (domain.SearchHit, error) {
	var hit domain.SearchHit
	var profileJSON string
	err := s.readDB().QueryRowContext(ctx, `
select sections.id, sections.document_id, documents.title, documents.url,
       coalesce(document_profiles."desc", ''),
       coalesce(document_profiles.retrieval_profile_json, '{}'),
       exists (
         select 1 from feedback_events fe
         where fe.target_kind = 'document'
           and fe.target_id = documents.id
           and fe.feedback_kind = 'document_canonical'
       ) as canonical,
       sections.title, sections.heading_path, sections.content
from sections
join documents on documents.id = sections.document_id
left join document_profiles on document_profiles.document_id = documents.id
where sections.document_id = ?
order by sections.ordinal asc, sections.id asc
limit 1
`, documentID).Scan(&hit.SectionID, &hit.DocumentID, &hit.DocumentTitle, &hit.DocumentURL, &hit.Desc, &profileJSON, &hit.Canonical, &hit.Title, &hit.HeadingPath, &hit.Content)
	if err != nil {
		return domain.SearchHit{}, err
	}
	enrichSearchHit(&hit, opts, profileJSON, searchTerms(opts.Query), "relation")
	return hit, nil
}

func applySearchDetailOptions(hit *domain.SearchHit, opts domain.SearchOptions) {
	if opts.Detail == "summary" {
		hit.Content = fmt.Sprintf("[content omitted; use doc_get_section(\"%s\") to read full section]", hit.SectionID)
		hit.Desc = ""
		hit.Profile = nil
		hit.RetrievalProfile = nil
		hit.QueryMatch = nil
		return
	}
	if opts.MaxCharsPerResult > 0 {
		before := len(hit.Content)
		hit.Content = truncateBytes(hit.Content, opts.MaxCharsPerResult)
		if len(hit.Content) < before {
			hit.Content += fmt.Sprintf("\n[truncated at %d bytes; use doc_get_section(\"%s\") for full content]", opts.MaxCharsPerResult, hit.SectionID)
		}
	}
	if opts.ProfileDetail == "none" {
		hit.Desc = ""
		hit.Profile = nil
		hit.RetrievalProfile = nil
	}
}

func (s *Store) searchSectionsTokenFTS(ctx context.Context, matchQuery string, limit int, opts domain.SearchOptions) ([]domain.SearchHit, error) {
	rows, err := s.readDB().QueryContext(ctx, `
select fts_section_tokens.section_id, fts_section_tokens.document_id, documents.title, documents.url,
       coalesce(document_profiles."desc", ''),
       coalesce(document_profiles.retrieval_profile_json, '{}'),
       exists (
         select 1 from feedback_events fe
         where fe.target_kind = 'document'
           and fe.target_id = documents.id
           and fe.feedback_kind = 'document_canonical'
       ) as canonical,
       sections.title, sections.heading_path, sections.content,
       -bm25(fts_section_tokens, 8.0, 6.0, 7.0, 1.0) as score
from fts_section_tokens
join sections on sections.id = fts_section_tokens.section_id
join documents on documents.id = fts_section_tokens.document_id
left join document_profiles on document_profiles.document_id = documents.id
where fts_section_tokens match ?
  and not exists (
    select 1 from feedback_events fe
    where fe.target_kind = 'document'
      and fe.target_id = documents.id
      and fe.feedback_kind = 'document_stale'
  )
order by canonical desc, score desc
limit ?
`, matchQuery, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanWeightedSearchHits(rows, opts, "fts_unicode")
}

func (s *Store) searchSectionsTokenTrigram(ctx context.Context, query string, limit int, opts domain.SearchOptions) ([]domain.SearchHit, error) {
	trigramQuery := trigramFTSQuery(query)

	rows, err := s.readDB().QueryContext(ctx, `
select fts_section_tokens_trigram.section_id, fts_section_tokens_trigram.document_id, documents.title, documents.url,
       coalesce(document_profiles."desc", ''),
       coalesce(document_profiles.retrieval_profile_json, '{}'),
       exists (
         select 1 from feedback_events fe
         where fe.target_kind = 'document'
           and fe.target_id = documents.id
           and fe.feedback_kind = 'document_canonical'
       ) as canonical,
       sections.title, sections.heading_path, sections.content,
       -bm25(fts_section_tokens_trigram, 8.0, 6.0, 7.0, 1.0) as score
from fts_section_tokens_trigram
join sections on sections.id = fts_section_tokens_trigram.section_id
join documents on documents.id = fts_section_tokens_trigram.document_id
left join document_profiles on document_profiles.document_id = documents.id
where fts_section_tokens_trigram match ?
  and not exists (
    select 1 from feedback_events fe
    where fe.target_kind = 'document'
      and fe.target_id = documents.id
      and fe.feedback_kind = 'document_stale'
  )
order by canonical desc, score desc
limit ?
`, trigramQuery, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanWeightedSearchHits(rows, opts, "fts_trigram")
}

func (s *Store) searchSectionsShortGram(ctx context.Context, term string, limit int, opts domain.SearchOptions) ([]domain.SearchHit, error) {
	matchQuery := shortGramFTSQuery(term)
	if matchQuery == "" {
		return nil, nil
	}
	rows, err := s.readDB().QueryContext(ctx, `
select fts_section_shortgrams.section_id, fts_section_shortgrams.document_id, documents.title, documents.url,
       coalesce(document_profiles."desc", ''),
       coalesce(document_profiles.retrieval_profile_json, '{}'),
       exists (
         select 1 from feedback_events fe
         where fe.target_kind = 'document'
           and fe.target_id = documents.id
           and fe.feedback_kind = 'document_canonical'
       ) as canonical,
       sections.title, sections.heading_path, sections.content,
       -bm25(fts_section_shortgrams, 8.0, 6.0, 1.0) as score
from fts_section_shortgrams
join sections on sections.id = fts_section_shortgrams.section_id
join documents on documents.id = fts_section_shortgrams.document_id
left join document_profiles on document_profiles.document_id = documents.id
where fts_section_shortgrams match ?
  and not exists (
    select 1 from feedback_events fe
    where fe.target_kind = 'document'
      and fe.target_id = documents.id
      and fe.feedback_kind = 'document_stale'
  )
order by canonical desc, score desc
limit ?
`, matchQuery, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanWeightedSearchHits(rows, opts, "fts_shortgram")
}

func scanWeightedSearchHits(rows *sql.Rows, opts domain.SearchOptions, field string) ([]domain.SearchHit, error) {
	type pendingHit struct {
		hit         domain.SearchHit
		profileJSON string
		rawScore    float64
	}
	pending := make([]pendingHit, 0)
	maxRawScore := 0.0
	for rows.Next() {
		var hit domain.SearchHit
		var profileJSON string
		var rawScore float64
		if err := rows.Scan(&hit.SectionID, &hit.DocumentID, &hit.DocumentTitle, &hit.DocumentURL, &hit.Desc, &profileJSON, &hit.Canonical, &hit.Title, &hit.HeadingPath, &hit.Content, &rawScore); err != nil {
			return nil, err
		}
		if rawScore > maxRawScore {
			maxRawScore = rawScore
		}
		pending = append(pending, pendingHit{hit: hit, profileJSON: profileJSON, rawScore: rawScore})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	hits := make([]domain.SearchHit, 0, len(pending))
	queryTerms := classifyQueryTerms(opts.Query)
	terms := uniqueStrings(append(entitySearchTerms(extractQueryEntities(opts.Query)), strongQueryTermTexts(queryTerms)...))
	if len(terms) == 0 {
		terms = searchTerms(opts.Query)
	}
	for _, row := range pending {
		hit := row.hit
		baseScore := normalizedBM25Boost(row.rawScore, maxRawScore, field)
		hit.Snippet = bestSnippet(hit.Content, terms)
		enrichSearchHit(&hit, opts, row.profileJSON, terms, field)
		breakdown := weightedScoreBreakdown(hit, opts.Query, terms, baseScore, field)
		hit.Rank = breakdown.Total
		hit.ScoreBreakdown = &breakdown
		if hit.QueryMatch != nil {
			applyScoreBreakdownToQueryMatch(hit.QueryMatch, breakdown)
		}
		hits = append(hits, hit)
	}
	return hits, nil
}

func normalizedBM25Boost(rawScore float64, maxRawScore float64, field string) float64 {
	if rawScore <= 0 || maxRawScore <= 0 {
		return 0
	}
	switch field {
	case "fts_unicode":
		return rawScore / maxRawScore * 100
	case "fts_trigram":
		return rawScore / maxRawScore * 40
	default:
		return rawScore
	}
}

func (s *Store) searchSectionsProfile(ctx context.Context, terms []string, limit int, opts domain.SearchOptions) ([]domain.SearchHit, error) {
	seen := map[string]bool{}
	hits := make([]domain.SearchHit, 0, limit)
	matchQuery := matchQueryFromTerms(terms, 16)
	for _, table := range []string{"fts_document_profiles", "fts_document_profiles_trigram"} {
		rows, err := s.readDB().QueryContext(ctx, `
select sections.id, sections.document_id, documents.title, documents.url,
       document_profiles."desc", document_profiles.retrieval_profile_json,
       exists (
         select 1 from feedback_events fe
         where fe.target_kind = 'document'
           and fe.target_id = documents.id
           and fe.feedback_kind = 'document_canonical'
       ) as canonical,
       sections.title, sections.heading_path, sections.content, sections.ordinal
from `+table+` profile_fts
join document_profiles on document_profiles.document_id = profile_fts.document_id
join documents on documents.id = document_profiles.document_id
join sections on sections.document_id = documents.id
where `+table+` match ?
  and not exists (
    select 1 from feedback_events fe
    where fe.target_kind = 'document'
      and fe.target_id = documents.id
      and fe.feedback_kind = 'document_stale'
  )
order by canonical desc, documents.title asc, sections.ordinal asc
limit ?
`, matchQuery, limit)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var hit domain.SearchHit
			var profileJSON string
			var ordinal int
			if err := rows.Scan(&hit.SectionID, &hit.DocumentID, &hit.DocumentTitle, &hit.DocumentURL, &hit.Desc, &profileJSON, &hit.Canonical, &hit.Title, &hit.HeadingPath, &hit.Content, &ordinal); err != nil {
				rows.Close()
				return nil, err
			}
			if seen[hit.SectionID] {
				continue
			}
			seen[hit.SectionID] = true
			hit.Snippet = bestSnippet(hit.Content, terms)
			enrichSearchHit(&hit, opts, profileJSON, terms, "profile")
			breakdown := weightedScoreBreakdown(hit, opts.Query, terms, 1, "profile")
			hit.Rank = breakdown.Total
			hit.ScoreBreakdown = &breakdown
			if hit.QueryMatch != nil {
				applyScoreBreakdownToQueryMatch(hit.QueryMatch, breakdown)
			}
			hits = append(hits, hit)
			if len(hits) >= limit {
				break
			}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if len(hits) >= limit {
			break
		}
	}
	return hits, nil
}

func (s *Store) searchSectionsExactTerms(ctx context.Context, terms []string, limit int, opts domain.SearchOptions) ([]domain.SearchHit, error) {
	seen := map[string]bool{}
	hits := make([]domain.SearchHit, 0)
	for _, term := range terms {
		if len(hits) >= limit {
			break
		}
		termOpts := opts
		termOpts.Query = term
		candidates, err := s.searchSectionsShortGram(ctx, term, limit*2, termOpts)
		if err != nil {
			return nil, err
		}
		for _, hit := range candidates {
			if seen[hit.SectionID] {
				continue
			}
			matched, err := s.sectionContainsLiteral(ctx, hit.SectionID, term)
			if err != nil {
				return nil, err
			}
			if !matched {
				continue
			}
			seen[hit.SectionID] = true
			hit.Snippet = snippetForTerm(hit.Content, term)
			breakdown := weightedScoreBreakdown(hit, opts.Query, []string{term}, 1, "fallback")
			hit.Rank = breakdown.Total
			hit.ScoreBreakdown = &breakdown
			if hit.QueryMatch != nil {
				applyScoreBreakdownToQueryMatch(hit.QueryMatch, breakdown)
			}
			hits = append(hits, hit)
			if len(hits) >= limit {
				break
			}
		}
	}
	return hits, nil
}

func (s *Store) sectionContainsLiteral(ctx context.Context, sectionID string, term string) (bool, error) {
	var documentTitle, sectionTitle, headingPath, searchText string
	if err := s.readDB().QueryRowContext(ctx, `
select documents.title, sections.title, sections.heading_path, sections.search_text
from sections join documents on documents.id = sections.document_id
where sections.id = ?
`, sectionID).Scan(&documentTitle, &sectionTitle, &headingPath, &searchText); err != nil {
		return false, err
	}
	term = strings.ToLower(strings.TrimSpace(term))
	text := strings.ToLower(strings.Join([]string{documentTitle, sectionTitle, headingPath, searchText}, "\n"))
	return term != "" && strings.Contains(text, term), nil
}

type searchCollector struct {
	opts       domain.SearchOptions
	queryTerms []string
	hits       []domain.SearchHit
	seen       map[string]int
}

func newSearchCollector(opts domain.SearchOptions) *searchCollector {
	return &searchCollector{
		opts:       opts,
		queryTerms: searchTerms(opts.Query),
		seen:       map[string]int{},
	}
}

func (c *searchCollector) add(attempt string, hits []domain.SearchHit) {
	for _, hit := range hits {
		if c.opts.Detail == "summary" {
			// Summary mode: strip content and verbose metadata for lightweight scanning.
			// Replace with a hint so LLMs know to use doc_get_section for full retrieval.
			hit.Content = fmt.Sprintf("[content omitted; use doc_get_section(\"%s\") to read full section]", hit.SectionID)
			hit.Desc = ""
			hit.Profile = nil
			hit.RetrievalProfile = nil
			hit.QueryMatch = nil
		} else {
			// Content mode (default behavior): include full content with truncation.
			if c.opts.MaxCharsPerResult > 0 {
				before := len(hit.Content)
				hit.Content = truncateBytes(hit.Content, c.opts.MaxCharsPerResult)
				if len(hit.Content) < before {
					hit.Content += fmt.Sprintf("\n[truncated at %d bytes; use doc_get_section(\"%s\") for full content]", c.opts.MaxCharsPerResult, hit.SectionID)
				}
			}
			if c.opts.ProfileDetail == "none" {
				hit.Desc = ""
				hit.Profile = nil
				hit.RetrievalProfile = nil
			}
		}
		if idx, ok := c.seen[hit.SectionID]; ok {
			oldRank := c.hits[idx].Rank
			if c.hits[idx].ScoreBreakdown != nil && hit.ScoreBreakdown != nil {
				merged := mergeScoreBreakdowns(*c.hits[idx].ScoreBreakdown, *hit.ScoreBreakdown)
				c.hits[idx].ScoreBreakdown = &merged
				c.hits[idx].Rank = merged.Total
			} else if hit.Rank > c.hits[idx].Rank {
				c.hits[idx].Rank = hit.Rank
				c.hits[idx].ScoreBreakdown = hit.ScoreBreakdown
			}
			if hit.Rank > oldRank {
				c.hits[idx].Snippet = hit.Snippet
			}
			if hit.QueryMatch != nil {
				mergeQueryMatch(c.hits[idx].QueryMatch, hit.QueryMatch, attempt)
			}
			c.hits[idx].MatchedEntities = mergeMatchedEntities(c.hits[idx].MatchedEntities, hit.MatchedEntities)
			if c.hits[idx].Trace == nil && hit.Trace != nil {
				c.hits[idx].Trace = hit.Trace
			}
			if hit.EvidenceLevel != "" && (c.hits[idx].EvidenceLevel == "" || c.hits[idx].EvidenceLevel == "weak_vector_only") {
				c.hits[idx].EvidenceLevel = hit.EvidenceLevel
			}
			continue
		}
		if hit.QueryMatch != nil {
			hit.QueryMatch.SearchAttempts = uniqueStrings(append(hit.QueryMatch.SearchAttempts, attempt))
		}
		c.seen[hit.SectionID] = len(c.hits)
		c.hits = append(c.hits, hit)
	}
}

func (c *searchCollector) results() []domain.SearchHit {
	sortSearchHits(c.hits)
	result := make([]domain.SearchHit, 0, minInt(c.opts.Limit, len(c.hits)))
	perDoc := map[string]int{}
	for _, hit := range c.hits {
		if len(result) >= c.opts.Limit {
			break
		}
		if perDoc[hit.DocumentID] >= c.opts.MaxSectionsPerDocument {
			continue
		}
		if hit.EvidenceLevel == "" && hit.ScoreBreakdown != nil {
			hit.EvidenceLevel = evidenceLevelForBreakdown(*hit.ScoreBreakdown)
		}
		perDoc[hit.DocumentID]++
		result = append(result, hit)
	}
	return result
}

func sortSearchHits(hits []domain.SearchHit) {
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Rank == hits[j].Rank {
			if hits[i].Canonical != hits[j].Canonical {
				return hits[i].Canonical
			}
			if hits[i].DocumentTitle == hits[j].DocumentTitle {
				return hits[i].Title < hits[j].Title
			}
			return hits[i].DocumentTitle < hits[j].DocumentTitle
		}
		return hits[i].Rank > hits[j].Rank
	})
}

func mergeScoreBreakdowns(left domain.ScoreBreakdown, right domain.ScoreBreakdown) domain.ScoreBreakdown {
	merged := domain.ScoreBreakdown{
		UnicodeBM25Boost:      left.UnicodeBM25Boost + right.UnicodeBM25Boost,
		TrigramBM25Boost:      left.TrigramBM25Boost + right.TrigramBM25Boost,
		TitleBoost:            maxFloat(left.TitleBoost, right.TitleBoost),
		SectionBoost:          maxFloat(left.SectionBoost, right.SectionBoost),
		SymbolBoost:           maxFloat(left.SymbolBoost, right.SymbolBoost),
		ExactMatchBoost:       maxFloat(left.ExactMatchBoost, right.ExactMatchBoost),
		CanonicalBoost:        maxFloat(left.CanonicalBoost, right.CanonicalBoost),
		CoverageBoost:         maxFloat(left.CoverageBoost, right.CoverageBoost),
		FallbackBoost:         maxFloat(left.FallbackBoost, right.FallbackBoost),
		VectorBoost:           maxFloat(left.VectorBoost, right.VectorBoost),
		VectorOnlyPenalty:     maxFloat(left.VectorOnlyPenalty, right.VectorOnlyPenalty),
		StaleEmbeddingPenalty: maxFloat(left.StaleEmbeddingPenalty, right.StaleEmbeddingPenalty),
		MatchedFields:         uniqueStrings(append(left.MatchedFields, right.MatchedFields...)),
		MatchedTerms:          uniqueStrings(append(left.MatchedTerms, right.MatchedTerms...)),
		MatchedSymbols:        uniqueStrings(append(left.MatchedSymbols, right.MatchedSymbols...)),
	}
	if hasNonVectorEvidence(merged) {
		merged.VectorOnlyPenalty = 0
	}
	merged.Total = scoreBreakdownTotal(merged)
	return merged
}

func maxFloat(left float64, right float64) float64 {
	if left > right {
		return left
	}
	return right
}

func mergeQueryMatch(dst *domain.QueryMatch, src *domain.QueryMatch, attempt string) {
	if dst == nil {
		return
	}
	dst.SearchAttempts = uniqueStrings(append(dst.SearchAttempts, attempt))
	if src == nil {
		return
	}
	dst.MatchedTerms = uniqueStrings(append(dst.MatchedTerms, src.MatchedTerms...))
	dst.MatchedFields = uniqueStrings(append(dst.MatchedFields, src.MatchedFields...))
	dst.SearchAttempts = uniqueStrings(append(dst.SearchAttempts, src.SearchAttempts...))
	if dst.ScoreExplanation == "" {
		dst.ScoreExplanation = src.ScoreExplanation
	}
}

type storedRetrievalProfile struct {
	TopTerms            []domain.ProfileTerm        `json:"top_terms"`
	TopTags             []string                    `json:"top_tags"`
	Keyphrases          []string                    `json:"keyphrases"`
	Aliases             []string                    `json:"aliases"`
	APIRefs             []string                    `json:"api_refs"`
	SectionDistribution []storedSectionDistribution `json:"section_distribution"`
	Stats               map[string]any              `json:"stats"`
}

type storedSectionDistribution struct {
	SectionID string   `json:"section_id"`
	Title     string   `json:"title"`
	Terms     []string `json:"terms"`
	TermCount int      `json:"term_count"`
}

func enrichSearchHit(hit *domain.SearchHit, opts domain.SearchOptions, profileJSON string, terms []string, field string) {
	profile := parseStoredProfile(profileJSON)
	matchedTerms := matchedTermsForHit(*hit, profile, terms)
	matchedFields := matchedFieldsForHit(*hit, profile, matchedTerms, field)
	if len(matchedTerms) == 0 {
		matchedTerms = terms
	}
	hit.QueryMatch = &domain.QueryMatch{
		MatchedTerms:     uniqueStrings(matchedTerms),
		MatchedFields:    uniqueStrings(matchedFields),
		ScoreExplanation: scoreExplanation(uniqueStrings(matchedFields)),
	}
	if opts.ProfileDetail == "compact" || opts.ProfileDetail == "full" {
		hit.Profile = compactProfile(profile, matchedTerms)
	}
	if opts.ProfileDetail == "full" {
		hit.RetrievalProfile = boundedFullProfile(profile, hit.SectionID, matchedTerms)
	}
}

func parseStoredProfile(profileJSON string) storedRetrievalProfile {
	var profile storedRetrievalProfile
	if strings.TrimSpace(profileJSON) == "" {
		return profile
	}
	_ = json.Unmarshal([]byte(profileJSON), &profile)
	return profile
}

func compactProfile(profile storedRetrievalProfile, matchedTerms []string) *domain.SearchHitProfile {
	topTerms := profile.TopTerms
	if len(topTerms) > 5 {
		topTerms = topTerms[:5]
	}
	topTags := profile.TopTags
	if len(topTags) > 8 {
		topTags = topTags[:8]
	}
	return &domain.SearchHitProfile{
		TopTags:           topTags,
		TopTerms:          topTerms,
		MatchedTerms:      uniqueStrings(matchedTerms),
		MatchedKeyphrases: matchedValues(profile.Keyphrases, matchedTerms),
	}
}

func matchedTermsForHit(hit domain.SearchHit, profile storedRetrievalProfile, terms []string) []string {
	text := strings.ToLower(strings.Join([]string{hit.DocumentTitle, hit.Desc, hit.Title, hit.HeadingPath, hit.Content}, "\n"))
	matched := make([]string, 0, len(terms))
	for _, term := range terms {
		term = strings.TrimSpace(strings.ToLower(term))
		if term == "" {
			continue
		}
		if strings.Contains(text, term) || profileContainsTerm(profile, term) {
			matched = append(matched, term)
		}
	}
	return uniqueStrings(matched)
}

func matchedFieldsForHit(hit domain.SearchHit, profile storedRetrievalProfile, matchedTerms []string, fallback string) []string {
	fields := make([]string, 0, 4)
	for _, term := range matchedTerms {
		if containsFold(hit.DocumentTitle, term) {
			fields = append(fields, "title")
		}
		if containsFold(hit.Desc, term) {
			fields = append(fields, "desc")
		}
		if containsFold(hit.Title, term) || containsFold(hit.HeadingPath, term) {
			fields = append(fields, "heading")
		}
		if containsFold(hit.Content, term) {
			fields = append(fields, "content")
		}
		if profileContainsTerm(profile, term) {
			fields = append(fields, "profile")
		}
	}
	if len(fields) == 0 && fallback != "" {
		fields = append(fields, fallback)
	}
	return uniqueStrings(fields)
}

func profileContainsTerm(profile storedRetrievalProfile, term string) bool {
	for _, profileTerm := range profile.TopTerms {
		if strings.Contains(strings.ToLower(profileTerm.Term), term) || strings.Contains(term, strings.ToLower(profileTerm.Term)) {
			return true
		}
	}
	for _, value := range append(append(append(append([]string{}, profile.TopTags...), profile.Keyphrases...), profile.Aliases...), profile.APIRefs...) {
		if strings.Contains(strings.ToLower(value), term) || strings.Contains(term, strings.ToLower(value)) {
			return true
		}
	}
	return false
}

func boundedFullProfile(profile storedRetrievalProfile, sectionID string, matchedTerms []string) map[string]any {
	result := map[string]any{
		"top_terms":     limitProfileTerms(profile.TopTerms, 20),
		"top_tags":      limitStrings(profile.TopTags, 12),
		"keyphrases":    limitStrings(profile.Keyphrases, 20),
		"aliases":       limitStrings(profile.Aliases, 10),
		"api_refs":      limitStrings(profile.APIRefs, 20),
		"matched_terms": limitStrings(uniqueStrings(matchedTerms), 20),
	}
	if len(profile.Stats) > 0 {
		result["stats"] = profile.Stats
	}
	sections := boundedSectionDistribution(profile.SectionDistribution, sectionID, matchedTerms)
	if len(sections) > 0 {
		result["section_distribution"] = sections
	}
	return result
}

func boundedSectionDistribution(sections []storedSectionDistribution, sectionID string, matchedTerms []string) []storedSectionDistribution {
	selected := make([]storedSectionDistribution, 0, 5)
	add := func(section storedSectionDistribution) {
		if len(selected) >= 5 {
			return
		}
		for _, existing := range selected {
			if existing.SectionID == section.SectionID {
				return
			}
		}
		section.Terms = limitStrings(section.Terms, 8)
		selected = append(selected, section)
	}
	for _, section := range sections {
		if section.SectionID == sectionID {
			add(section)
		}
	}
	for _, section := range sections {
		if sectionContainsMatchedTerm(section, matchedTerms) {
			add(section)
		}
	}
	for _, section := range sections {
		add(section)
	}
	return selected
}

func sectionContainsMatchedTerm(section storedSectionDistribution, matchedTerms []string) bool {
	text := strings.ToLower(section.Title + " " + strings.Join(section.Terms, " "))
	for _, term := range matchedTerms {
		if strings.Contains(text, strings.ToLower(term)) {
			return true
		}
	}
	return false
}

func limitProfileTerms(terms []domain.ProfileTerm, limit int) []domain.ProfileTerm {
	if len(terms) <= limit {
		return terms
	}
	return terms[:limit]
}

func limitStrings(values []string, limit int) []string {
	values = uniqueStrings(values)
	if len(values) <= limit {
		return values
	}
	return values[:limit]
}

func matchedValues(values []string, terms []string) []string {
	matched := make([]string, 0)
	for _, value := range values {
		for _, term := range terms {
			if containsFold(value, term) || containsFold(term, value) {
				matched = append(matched, value)
				break
			}
		}
	}
	if len(matched) > 5 {
		matched = matched[:5]
	}
	return uniqueStrings(matched)
}

func scoreExplanation(fields []string) string {
	if len(fields) == 0 {
		return ""
	}
	return "matched " + strings.Join(fields, ", ")
}

func quoteMatchQuery(query string) string {
	return `"` + strings.ReplaceAll(strings.TrimSpace(query), `"`, `""`) + `"`
}

func matchQueryFromTerms(terms []string, limit int) string {
	quoted := make([]string, 0, minInt(limit, len(terms)))
	for _, term := range terms {
		term = strings.TrimSpace(term)
		if term == "" {
			continue
		}
		quoted = append(quoted, `"`+strings.ReplaceAll(term, `"`, `""`)+`"`)
		if len(quoted) >= limit {
			break
		}
	}
	if len(quoted) == 0 {
		return `""`
	}
	return strings.Join(quoted, " OR ")
}

// trigramFTSQuery prepares a query for the trigram FTS5 index.
// It splits the query at CJK/Latin script boundaries so each script
// forms its own search term. This allows the trigram tokenizer to
// match fragments within each script independently.
// Example: "sampleapp平台默认配置schema" → `"sampleapp" OR "平台默认配置" OR "schema"`
func trigramFTSQuery(query string) string {
	var parts []string
	var current strings.Builder
	flush := func() {
		s := strings.TrimSpace(current.String())
		if s != "" {
			parts = append(parts, `"`+strings.ReplaceAll(s, `"`, `""`)+`"`)
		}
		current.Reset()
	}
	for _, r := range query {
		if current.Len() > 0 {
			runes := []rune(current.String())
			prevCJK := isCJK(runes[len(runes)-1])
			curCJK := isCJK(r)
			if prevCJK != curCJK {
				flush()
			}
		}
		current.WriteRune(r)
	}
	flush()
	if len(parts) == 0 {
		return `"` + strings.ReplaceAll(strings.TrimSpace(query), `"`, `""`) + `"`
	}
	return strings.Join(parts, " OR ")
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r)
}

func fallbackMatchQuery(query string) string {
	fields := strings.Fields(query)
	if len(fields) <= 1 {
		return ""
	}
	quoted := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.Trim(field, `"'.,;:!?()[]{}<>`)
		if field == "" {
			continue
		}
		quoted = append(quoted, `"`+strings.ReplaceAll(field, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, " OR ")
}

func searchTerms(query string) []string {
	terms := []string{}
	query = strings.TrimSpace(query)
	if query != "" {
		terms = append(terms, strings.ToLower(query))
	}
	for _, field := range strings.Fields(query) {
		field = strings.Trim(strings.ToLower(field), `"'.,;:!?()[]{}<>`)
		if field != "" {
			terms = append(terms, field)
		}
	}
	terms = append(terms, chineseNgrams(query, 2, 4)...)
	return uniqueStrings(terms)
}

func expandedSearchTerms(query string) []string {
	return queryTermTexts(classifyQueryTerms(query), true)
}

type queryTerm struct {
	Text     string
	Source   string
	Strength string
	Weight   float64
}

func classifyQueryTerms(query string) []queryTerm {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}
	seen := map[string]bool{}
	terms := make([]queryTerm, 0)
	add := func(text string, source string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		key := strings.ToLower(text)
		if seen[key] {
			return
		}
		seen[key] = true
		strength, weight := classifyTermStrength(text, source)
		terms = append(terms, queryTerm{Text: text, Source: source, Strength: strength, Weight: weight})
	}

	for _, term := range searchtoken.Default().QueryTerms(query) {
		add(term.Text, term.Source)
	}
	sort.SliceStable(terms, func(i, j int) bool {
		if terms[i].Weight == terms[j].Weight {
			if len([]rune(terms[i].Text)) == len([]rune(terms[j].Text)) {
				return terms[i].Text < terms[j].Text
			}
			return len([]rune(terms[i].Text)) > len([]rune(terms[j].Text))
		}
		return terms[i].Weight > terms[j].Weight
	})
	return terms
}

func classifyTermStrength(term string, source string) (string, float64) {
	runeLen := len([]rune(term))
	if source == "symbol" {
		return "strong", 3.0
	}
	if source == "raw" {
		return "strong", 2.4
	}
	if source == "gse" && (hasASCII(term) || runeLen >= 2) {
		return "strong", 2.0
	}
	if runeLen <= 1 {
		return "weak", 0.4
	}
	return "neutral", 1.4
}

func queryTermTexts(terms []queryTerm, includeWeak bool) []string {
	values := make([]string, 0, len(terms))
	for _, term := range terms {
		if !includeWeak && term.Strength == "weak" {
			continue
		}
		values = append(values, term.Text)
	}
	return uniqueStrings(nonEmptyStrings(values))
}

func strongQueryTermTexts(terms []queryTerm) []string {
	values := make([]string, 0, len(terms))
	for _, term := range terms {
		if term.Strength != "strong" {
			continue
		}
		values = append(values, term.Text)
	}
	return uniqueStrings(nonEmptyStrings(values))
}

func hasASCII(value string) bool {
	for _, r := range value {
		if r <= unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			return true
		}
	}
	return false
}

func substringSearchTerms(query string) []string {
	terms := []string{strings.TrimSpace(query)}
	terms = append(terms, chineseNgrams(query, 2, 4)...)
	return uniqueStrings(nonEmptyStrings(terms))
}

func vectorQueryLabel(opts domain.SearchOptions) string {
	parts := []string{strings.TrimSpace(opts.OriginalQuery)}
	parts = append(parts, opts.SemanticIntents...)
	parts = uniqueStrings(nonEmptyStrings(parts))
	return strings.Join(parts, " | ")
}

func truncateForSearchAttempt(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}

func profileSearchTerms(query string) []string {
	terms := searchTerms(query)
	filtered := make([]string, 0, len(terms))
	for _, term := range terms {
		if len([]rune(term)) >= 2 || strings.Contains(term, "/") || strings.Contains(term, "_") || strings.Contains(term, "-") {
			filtered = append(filtered, term)
		}
	}
	return uniqueStrings(filtered)
}

func chineseNgrams(query string, minN int, maxN int) []string {
	runs := hanRuns(query)
	terms := make([]string, 0)
	for _, run := range runs {
		for n := minN; n <= maxN; n++ {
			if len(run) < n {
				continue
			}
			for i := 0; i+n <= len(run); i++ {
				terms = append(terms, string(run[i:i+n]))
			}
		}
	}
	return terms
}

func hanRuns(query string) [][]rune {
	var runs [][]rune
	var current []rune
	for _, r := range query {
		if unicode.Is(unicode.Han, r) {
			current = append(current, r)
			continue
		}
		if len(current) > 0 {
			runs = append(runs, current)
			current = nil
		}
	}
	if len(current) > 0 {
		runs = append(runs, current)
	}
	return runs
}

func hasHan(value string) bool {
	for _, r := range value {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func escapeLike(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	value = strings.ReplaceAll(value, `_`, `\_`)
	return value
}

func snippetForTerm(content string, term string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	lowerContent := strings.ToLower(content)
	lowerTerm := strings.ToLower(term)
	idx := strings.Index(lowerContent, lowerTerm)
	if idx < 0 {
		return truncateBytes(content, 220)
	}
	start := idx - 70
	if start < 0 {
		start = 0
	}
	end := idx + len(term) + 90
	if end > len(content) {
		end = len(content)
	}
	prefix := ""
	if start > 0 {
		prefix = "..."
	}
	suffix := ""
	if end < len(content) {
		suffix = "..."
	}
	return prefix + content[start:idx] + "<mark>" + content[idx:idx+len(term)] + "</mark>" + content[idx+len(term):end] + suffix
}

func bestSnippet(content string, terms []string) string {
	for _, term := range terms {
		if strings.TrimSpace(term) != "" && strings.Contains(strings.ToLower(content), strings.ToLower(term)) {
			return snippetForTerm(content, term)
		}
	}
	return truncateBytes(strings.TrimSpace(content), 220)
}

func buildSectionIndexTexts(documentTitle string, sectionTitle string, headingPath string, content string) (string, string, string, string) {
	titleText := tokenizedIndexText(documentTitle)
	headingText := tokenizedIndexText(sectionTitle + "\n" + headingPath)
	symbolText := strings.Join(searchtoken.Default().SymbolTerms(documentTitle+"\n"+sectionTitle+"\n"+headingPath+"\n"+content), " ")
	contentText := tokenizedIndexText(content)
	return titleText, headingText, symbolText, contentText
}

func buildSectionShortGramIndexTexts(documentTitle string, sectionTitle string, headingPath string, content string) (string, string, string) {
	return shortGramIndexText(documentTitle), shortGramIndexText(sectionTitle + "\n" + headingPath), shortGramIndexText(content)
}

func shortGramIndexText(text string) string {
	runes := []rune(strings.ToLower(text))
	if len(runes) == 0 {
		return ""
	}
	var builder strings.Builder
	builder.Grow(len(runes) * 12)
	written := false
	for _, current := range runes {
		writeShortGramToken(&builder, 'u', current, 0, false, &written)
	}
	for i := 0; i+1 < len(runes); i++ {
		writeShortGramToken(&builder, 'g', runes[i], runes[i+1], true, &written)
	}
	return builder.String()
}

func shortGramFTSQuery(term string) string {
	runes := []rune(strings.ToLower(strings.TrimSpace(term)))
	if len(runes) == 0 {
		return ""
	}
	var builder strings.Builder
	written := false
	if len(runes) == 1 {
		writeShortGramToken(&builder, 'u', runes[0], 0, false, &written)
	} else {
		for i := 0; i+1 < len(runes); i++ {
			writeShortGramToken(&builder, 'g', runes[i], runes[i+1], true, &written)
		}
	}
	return `"` + builder.String() + `"`
}

// writeShortGramToken emits tokenizer-safe, collision-free rune encodings.
// Unigrams and bigrams use distinct prefixes; the non-hex 'z' separator keeps
// variable-width rune code points unambiguous without the expansion of UTF-8
// byte-wise hex encoding.
func writeShortGramToken(builder *strings.Builder, prefix byte, first rune, second rune, hasSecond bool, written *bool) {
	if *written {
		builder.WriteByte(' ')
	}
	*written = true
	builder.WriteByte(prefix)
	var scratch [16]byte
	builder.Write(strconv.AppendInt(scratch[:0], int64(first), 16))
	if hasSecond {
		builder.WriteByte('z')
		builder.Write(strconv.AppendInt(scratch[:0], int64(second), 16))
	}
}

func tokenizedIndexText(text string) string {
	return strings.Join(searchtoken.Default().IndexTerms(text), " ")
}

func weightedScoreBreakdown(hit domain.SearchHit, query string, terms []string, baseScore float64, source string) domain.ScoreBreakdown {
	breakdown := domain.ScoreBreakdown{
		MatchedTerms: uniqueStrings(nonEmptyStrings(terms)),
	}
	switch source {
	case "fts_unicode":
		breakdown.UnicodeBM25Boost = baseScore
	case "fts_trigram":
		breakdown.TrigramBM25Boost = baseScore
	case "fallback", "content", "profile":
		breakdown.FallbackBoost = baseScore
	default:
		breakdown.FallbackBoost = baseScore
	}
	if hit.Canonical {
		breakdown.CanonicalBoost = 30
	}
	if containsFold(hit.DocumentTitle, query) || containsFold(hit.Title, query) || strongTermHit([]string{hit.DocumentTitle, hit.Title}, terms) {
		breakdown.TitleBoost = 80
	}
	if containsFold(hit.HeadingPath, query) || containsFold(hit.Title, query) || strongTermHit([]string{hit.HeadingPath, hit.Title}, terms) {
		breakdown.SectionBoost = 60
	}
	breakdown.MatchedSymbols = matchedSymbols(hit, terms)
	if len(breakdown.MatchedSymbols) > 0 {
		breakdown.SymbolBoost = 70
	}
	entityMatch := entityMatchForHit(hit, query)
	if entityMatch.Exact {
		breakdown.ExactMatchBoost = 260
		breakdown.MatchedFields = append(breakdown.MatchedFields, string(entityMatchExact))
		breakdown.MatchedTerms = append(breakdown.MatchedTerms, entityMatch.Terms...)
	} else if entityMatch.Normalized {
		breakdown.ExactMatchBoost = 160
		breakdown.MatchedFields = append(breakdown.MatchedFields, string(entityMatchNormalized))
		breakdown.MatchedTerms = append(breakdown.MatchedTerms, entityMatch.Terms...)
	} else if exactPhraseHit(hit, query) {
		breakdown.ExactMatchBoost = exactPhraseBoost(query)
		breakdown.MatchedFields = append(breakdown.MatchedFields, "exact_phrase")
	} else if exactHit(hit, query) {
		breakdown.ExactMatchBoost = 100
	}
	breakdown.CoverageBoost = coverageBoost(hit, terms)
	if hit.QueryMatch != nil {
		breakdown.MatchedFields = uniqueStrings(append(hit.QueryMatch.MatchedFields, breakdown.MatchedFields...))
	}
	breakdown.MatchedTerms = uniqueStrings(breakdown.MatchedTerms)
	breakdown.Total = scoreBreakdownTotal(breakdown)
	return breakdown
}

func scoreBreakdownTotal(breakdown domain.ScoreBreakdown) float64 {
	return breakdown.UnicodeBM25Boost +
		breakdown.TrigramBM25Boost +
		breakdown.TitleBoost +
		breakdown.SectionBoost +
		breakdown.SymbolBoost +
		breakdown.ExactMatchBoost +
		breakdown.CanonicalBoost +
		breakdown.CoverageBoost +
		breakdown.FallbackBoost +
		breakdown.VectorBoost -
		breakdown.VectorOnlyPenalty -
		breakdown.StaleEmbeddingPenalty
}

func vectorOnlyPenalty(breakdown domain.ScoreBreakdown) float64 {
	if hasNonVectorEvidence(breakdown) {
		return 0
	}
	return 30
}

func hasNonVectorEvidence(breakdown domain.ScoreBreakdown) bool {
	return breakdown.UnicodeBM25Boost > 0 ||
		breakdown.TrigramBM25Boost > 0 ||
		breakdown.TitleBoost > 0 ||
		breakdown.SectionBoost > 0 ||
		breakdown.SymbolBoost > 0 ||
		breakdown.ExactMatchBoost > 0 ||
		breakdown.CoverageBoost > 0 ||
		breakdown.FallbackBoost > 0 ||
		len(breakdown.MatchedSymbols) > 0
}

func evidenceLevelForBreakdown(breakdown domain.ScoreBreakdown) string {
	if breakdown.ExactMatchBoost >= 160 || breakdown.SymbolBoost > 0 {
		return "strong_exact"
	}
	if breakdown.VectorBoost > 0 && hasNonVectorEvidence(breakdown) {
		return "strong_hybrid"
	}
	if breakdown.VectorBoost > 0 {
		return "weak_vector_only"
	}
	return "medium_semantic"
}

func coverageBoost(hit domain.SearchHit, terms []string) float64 {
	coreTerms := make([]string, 0)
	for _, term := range terms {
		term = strings.TrimSpace(term)
		if term == "" || len([]rune(term)) < 2 && !hasASCII(term) {
			continue
		}
		coreTerms = append(coreTerms, term)
	}
	coreTerms = uniqueStrings(coreTerms)
	if len(coreTerms) == 0 {
		return 0
	}
	text := strings.ToLower(strings.Join([]string{hit.DocumentTitle, hit.Title, hit.HeadingPath, hit.Content}, "\n"))
	matched := 0
	for _, term := range coreTerms {
		if strings.Contains(text, strings.ToLower(term)) {
			matched++
		}
	}
	return float64(matched) / float64(len(coreTerms)) * 80
}

func matchedSymbols(hit domain.SearchHit, terms []string) []string {
	symbols := searchtoken.Default().SymbolTerms(strings.Join([]string{hit.DocumentTitle, hit.Title, hit.HeadingPath, hit.Content}, "\n"))
	matched := make([]string, 0)
	for _, symbol := range symbols {
		for _, term := range terms {
			if strings.EqualFold(symbol, term) {
				matched = append(matched, symbol)
				break
			}
		}
	}
	return uniqueStrings(matched)
}

type entityMatchResult struct {
	Exact      bool
	Normalized bool
	Terms      []string
	Entities   []domain.MatchedEntity
}

func extractQueryEntities(query string) []extract.EntityCandidate {
	return extract.ExtractSection(extract.SectionInput{Content: query})
}

func extractHitEntities(hit domain.SearchHit) []extract.EntityCandidate {
	return extract.ExtractSection(extract.SectionInput{
		DocumentID:  hit.DocumentID,
		SectionID:   hit.SectionID,
		Title:       hit.DocumentTitle + "\n" + hit.Title,
		HeadingPath: hit.HeadingPath,
		Content:     hit.Content,
	})
}

func entitySearchTerms(entities []extract.EntityCandidate) []string {
	terms := make([]string, 0, len(entities)*2)
	for _, entity := range entities {
		switch entity.Kind {
		case extract.EntityAPIEndpoint:
			terms = append(terms, entity.Canonical, entity.Path)
		case extract.EntityPathLiteral:
			terms = append(terms, entity.Path)
		case extract.EntityOperationCandidate:
			terms = append(terms, entity.Operation)
		}
	}
	return uniqueStrings(nonEmptyStrings(terms))
}

func entityMatchForHit(hit domain.SearchHit, query string) entityMatchResult {
	queryEntities := extractQueryEntities(query)
	if len(queryEntities) == 0 {
		return entityMatchResult{}
	}
	queryHasPath := false
	for _, queryEntity := range queryEntities {
		if queryEntity.Path != "" {
			queryHasPath = true
			break
		}
	}
	hitEntities := extractHitEntities(hit)
	if len(hitEntities) == 0 {
		return entityMatchResult{}
	}
	hitText := strings.Join([]string{hit.DocumentTitle, hit.Title, hit.HeadingPath, hit.Content}, "\n")
	result := entityMatchResult{}
	for _, queryEntity := range queryEntities {
		for _, hitEntity := range hitEntities {
			if queryHasPath && queryEntity.Path == "" {
				continue
			}
			if queryEntity.Operation != "" && hitEntity.Operation != "" && strings.EqualFold(queryEntity.Operation, hitEntity.Operation) {
				if queryEntity.Operation == hitEntity.Operation && strings.Contains(hitText, queryEntity.Operation) {
					result.Exact = true
				} else {
					result.Normalized = true
				}
				result.Terms = append(result.Terms, queryEntity.Operation)
				result.Entities = append(result.Entities, matchedEntityFromCandidate(hitEntity, ""))
				continue
			}
			if queryEntity.Path == "" || hitEntity.Path == "" {
				continue
			}
			methodCompatibleForExact := queryEntity.Method == "" || (hitEntity.Method != "" && queryEntity.Method == hitEntity.Method)
			if queryEntity.Path == hitEntity.Path && methodCompatibleForExact && hitContainsEntityLiteral(hitText, queryEntity) {
				result.Exact = true
				result.Terms = append(result.Terms, entityDisplayTerm(queryEntity))
				result.Entities = append(result.Entities, matchedEntityFromCandidate(hitEntity, ""))
				continue
			}
			if strings.EqualFold(queryEntity.Path, hitEntity.Path) && methodsCompatibleForNormalized(queryEntity.Method, hitEntity.Method) {
				result.Normalized = true
				result.Terms = append(result.Terms, entityDisplayTerm(queryEntity))
				result.Entities = append(result.Entities, matchedEntityFromCandidate(hitEntity, ""))
			}
		}
	}
	result.Terms = uniqueStrings(nonEmptyStrings(result.Terms))
	result.Entities = uniqueMatchedEntities(result.Entities)
	if result.Exact {
		result.Normalized = false
	}
	return result
}

func storedEntityMatchForQuery(entity domain.SectionEntity, queryEntities []extract.EntityCandidate) entityMatchResult {
	result := entityMatchResult{}
	for _, queryEntity := range queryEntities {
		if queryEntity.Operation != "" && entity.Operation != "" && strings.EqualFold(queryEntity.Operation, entity.Operation) {
			if queryEntity.Operation == entity.Operation {
				result.Exact = true
			} else {
				result.Normalized = true
			}
			result.Terms = append(result.Terms, queryEntity.Operation)
			result.Entities = append(result.Entities, matchedEntityFromStored(entity, ""))
			continue
		}
		if queryEntity.Path == "" || entity.Path == "" {
			continue
		}
		if queryEntity.Path == entity.Path && methodsCompatibleForExact(queryEntity.Method, entity.Method) {
			result.Exact = true
			result.Terms = append(result.Terms, entityDisplayTerm(queryEntity))
			result.Entities = append(result.Entities, matchedEntityFromStored(entity, ""))
			continue
		}
		if strings.EqualFold(queryEntity.Path, entity.Path) && methodsCompatibleForNormalized(queryEntity.Method, entity.Method) {
			result.Normalized = true
			result.Terms = append(result.Terms, entityDisplayTerm(queryEntity))
			result.Entities = append(result.Entities, matchedEntityFromStored(entity, ""))
		}
	}
	result.Terms = uniqueStrings(nonEmptyStrings(result.Terms))
	result.Entities = uniqueMatchedEntities(result.Entities)
	if result.Exact {
		result.Normalized = false
	}
	return result
}

func methodsCompatibleForNormalized(queryMethod string, hitMethod string) bool {
	return queryMethod == "" || hitMethod == "" || queryMethod == hitMethod
}

func methodsCompatibleForExact(queryMethod string, hitMethod string) bool {
	return queryMethod == "" || (hitMethod != "" && queryMethod == hitMethod)
}

func hitContainsEntityLiteral(hitText string, entity extract.EntityCandidate) bool {
	if entity.Path != "" && strings.Contains(hitText, entity.Path) {
		return true
	}
	if entity.Raw != "" && strings.Contains(hitText, entity.Raw) {
		return true
	}
	return false
}

func entityDisplayTerm(entity extract.EntityCandidate) string {
	if entity.Method != "" && entity.Path != "" {
		return entity.Method + " " + entity.Path
	}
	if entity.Path != "" {
		return entity.Path
	}
	return entity.Operation
}

func sectionEntityDisplayTerm(entity domain.SectionEntity) string {
	if entity.Method != "" && entity.Path != "" {
		return entity.Method + " " + entity.Path
	}
	if entity.Path != "" {
		return entity.Path
	}
	if entity.Operation != "" {
		return entity.Operation
	}
	return entity.CanonicalText
}

func matchedEntityFromStored(entity domain.SectionEntity, mode string) domain.MatchedEntity {
	return domain.MatchedEntity{
		ID:            entity.ID,
		SectionID:     entity.SectionID,
		DocumentID:    entity.DocumentID,
		Kind:          entity.Kind,
		CanonicalText: entity.CanonicalText,
		Method:        entity.Method,
		Path:          entity.Path,
		Operation:     entity.Operation,
		Source:        entity.Source,
		Confidence:    entity.Confidence,
		MatchMode:     mode,
	}
}

func matchedEntityFromCandidate(candidate extract.EntityCandidate, mode string) domain.MatchedEntity {
	return domain.MatchedEntity{
		SectionID:     candidate.SectionID,
		DocumentID:    candidate.DocumentID,
		Kind:          string(candidate.Kind),
		CanonicalText: candidate.Canonical,
		Method:        candidate.Method,
		Path:          candidate.Path,
		Operation:     candidate.Operation,
		Source:        string(candidate.Source),
		Confidence:    candidate.Confidence,
		MatchMode:     mode,
	}
}

func matchedEntitiesWithMode(entities []domain.MatchedEntity, mode string) []domain.MatchedEntity {
	out := make([]domain.MatchedEntity, 0, len(entities))
	for _, entity := range entities {
		entity.MatchMode = mode
		out = append(out, entity)
	}
	return uniqueMatchedEntities(out)
}

func mergeMatchedEntities(existing []domain.MatchedEntity, incoming []domain.MatchedEntity) []domain.MatchedEntity {
	merged := make([]domain.MatchedEntity, 0, len(existing)+len(incoming))
	merged = append(merged, existing...)
	merged = append(merged, incoming...)
	return uniqueMatchedEntities(merged)
}

func uniqueMatchedEntities(entities []domain.MatchedEntity) []domain.MatchedEntity {
	seen := map[string]bool{}
	out := make([]domain.MatchedEntity, 0, len(entities))
	for _, entity := range entities {
		if entity.Kind == "" && entity.CanonicalText == "" && entity.Path == "" && entity.Operation == "" {
			continue
		}
		key := strings.Join([]string{entity.ID, entity.SectionID, entity.Kind, entity.CanonicalText, entity.Method, entity.Path, entity.Operation, entity.MatchMode}, "\x00")
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, entity)
	}
	return out
}

func applyScoreBreakdownToQueryMatch(match *domain.QueryMatch, breakdown domain.ScoreBreakdown) {
	match.MatchedFields = uniqueStrings(append(match.MatchedFields, breakdown.MatchedFields...))
	match.MatchedTerms = uniqueStrings(append(match.MatchedTerms, breakdown.MatchedTerms...))
	match.ScoreExplanation = weightedScoreExplanation(match.MatchedFields, breakdown)
}

func strongTermHit(fields []string, terms []string) bool {
	for _, field := range fields {
		for _, term := range terms {
			term = strings.TrimSpace(term)
			if term == "" {
				continue
			}
			if !hasASCII(term) && len([]rune(term)) < 2 {
				continue
			}
			if containsFold(field, term) {
				return true
			}
		}
	}
	return false
}

func exactHit(hit domain.SearchHit, query string) bool {
	query = strings.TrimSpace(query)
	if query == "" {
		return false
	}
	return containsFold(hit.DocumentTitle, query) || containsFold(hit.Title, query) || containsFold(hit.HeadingPath, query) || containsFold(hit.Content, query)
}

func exactPhraseHit(hit domain.SearchHit, query string) bool {
	query = strings.TrimSpace(query)
	if query == "" {
		return false
	}
	return containsExactPhraseFold(hit.DocumentTitle, query) || containsExactPhraseFold(hit.Title, query) || containsExactPhraseFold(hit.HeadingPath, query) || containsExactPhraseFold(hit.Content, query)
}

func exactPhraseBoost(query string) float64 {
	switch classifyExactPhraseQuery(query) {
	case phraseQueryLong:
		return 260
	case phraseQueryMedium:
		return 160
	default:
		return 80
	}
}

type phraseQueryClass int

const (
	phraseQueryShort phraseQueryClass = iota
	phraseQueryMedium
	phraseQueryLong
)

func classifyExactPhraseQuery(query string) phraseQueryClass {
	runeLen := phraseQueryLength(query)
	strongCount := 0
	for _, term := range classifyQueryTerms(query) {
		if term.Strength == "strong" {
			strongCount++
		}
	}
	switch {
	case runeLen >= 18 || strongCount >= 5:
		return phraseQueryLong
	case runeLen >= 7 || strongCount >= 2:
		return phraseQueryMedium
	default:
		return phraseQueryShort
	}
}

func phraseQueryLength(query string) int {
	count := 0
	for _, r := range query {
		if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			continue
		}
		count++
	}
	return count
}

func containsExactPhraseFold(value string, query string) bool {
	valueRunes := []rune(strings.ToLower(value))
	queryRunes := []rune(strings.ToLower(strings.TrimSpace(query)))
	if len(queryRunes) == 0 || len(queryRunes) > len(valueRunes) {
		return false
	}
	for i := 0; i+len(queryRunes) <= len(valueRunes); i++ {
		matched := true
		for j, r := range queryRunes {
			if valueRunes[i+j] != r {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		beforeOK := i == 0 || !isExactPhraseContinuation(valueRunes[i-1])
		afterIdx := i + len(queryRunes)
		afterOK := afterIdx == len(valueRunes) || !isExactPhraseContinuation(valueRunes[afterIdx])
		if beforeOK && afterOK {
			return true
		}
	}
	return false
}

func isExactPhraseContinuation(r rune) bool {
	if unicode.IsLetter(r) || unicode.IsDigit(r) {
		return true
	}
	return strings.ContainsRune("_./:{}-", r)
}

func weightedScoreExplanation(fields []string, breakdown domain.ScoreBreakdown) string {
	base := scoreExplanation(fields)
	if base == "" {
		base = "matched indexed tokens"
	}
	return fmt.Sprintf("%s; weighted score %.2f from unicode %.2f, trigram %.2f, title %.0f, section %.0f, symbol %.0f, exact %.0f, coverage %.0f",
		base,
		breakdown.Total,
		breakdown.UnicodeBM25Boost,
		breakdown.TrigramBM25Boost,
		breakdown.TitleBoost,
		breakdown.SectionBoost,
		breakdown.SymbolBoost,
		breakdown.ExactMatchBoost,
		breakdown.CoverageBoost,
	)
}

func containsFold(value string, term string) bool {
	return strings.Contains(strings.ToLower(value), strings.ToLower(term))
}

func nonEmptyStrings(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}

func normalizeExactTerms(values []string) []string {
	return uniqueStrings(nonEmptyStrings(values))
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func truncateBytes(value string, max int) string {
	if max <= 0 || len(value) <= max {
		return value
	}
	return truncateParagraphs(value, max)
}

// truncateParagraphs truncates text at paragraph boundaries (double newline),
// keeping as many complete paragraphs as fit within max bytes.
// Falls back to rune-level truncation only if the first paragraph exceeds max.
func truncateParagraphs(value string, max int) string {
	paragraphs := splitParagraphs(value)
	var b strings.Builder
	for _, p := range paragraphs {
		if b.Len()+len(p) > max {
			// If we haven't added any paragraph yet and the first one is too long,
			// fall back to rune-level truncation within it.
			if b.Len() == 0 {
				return truncateRunes(p, max)
			}
			break
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(p)
	}
	return b.String()
}

// splitParagraphs splits text on double-newline boundaries into non-empty paragraphs.
func splitParagraphs(value string) []string {
	parts := strings.Split(value, "\n\n")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func truncateRunes(value string, max int) string {
	if max <= 0 || len(value) <= max {
		return value
	}
	var b strings.Builder
	b.Grow(max)
	for _, r := range value {
		if b.Len()+len(string(r)) > max {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

func minInt(a int, b int) int {
	if a < b {
		return a
	}
	return b
}

func pathFromDSN(dsn string) (string, error) {
	const prefix = "sqlite://"
	if !strings.HasPrefix(dsn, prefix) {
		return "", fmt.Errorf("invalid sqlite DSN %q", dsn)
	}
	path := strings.TrimPrefix(dsn, prefix)
	if path == "" {
		return "", fmt.Errorf("sqlite DSN requires a path")
	}
	if strings.HasPrefix(path, "/") {
		return path, nil
	}
	return filepath.Clean(path), nil
}
