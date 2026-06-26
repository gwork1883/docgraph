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
	"strings"
	"time"
	"unicode"

	_ "modernc.org/sqlite"

	"github.com/docgraph/docgraph/internal/domain"
	"github.com/docgraph/docgraph/internal/ids"
	"github.com/docgraph/docgraph/internal/searchtoken"
	"github.com/docgraph/docgraph/internal/storage/sqlschema"
)

type Store struct {
	db     *sql.DB
	reader *sql.DB
	dsn    string
}

const (
	sectionTokenIndexVersion    = "gse-v1"
	sectionTokenIndexRebuilding = sectionTokenIndexVersion + ":rebuilding"
	legacyNodeIndexVersion      = "v1"
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
	if s.reader == nil {
		return s.db.Close()
	}
	if err := s.reader.Close(); err != nil {
		_ = s.db.Close()
		return err
	}
	return s.db.Close()
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
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
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
	if _, err := s.db.ExecContext(ctx, `
create index if not exists idx_jobs_kind_status_run_after
  on jobs(kind, status, run_after);
create index if not exists idx_jobs_source_kind_status
  on jobs(source_id, kind, status, updated_at);
create index if not exists idx_jobs_target
  on jobs(target_kind, target_id, kind, status);
create index if not exists idx_feedback_target
  on feedback_events(target_kind, target_id, feedback_kind);
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
`); err != nil {
		return err
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
	if err := s.EnsureSearchIndexes(ctx); err != nil {
		return err
	}
	return s.recordSchemaVersion(ctx)
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
	if !needsTokenBackfill {
		return nil
	}
	if err := s.backfillSectionTokenIndexes(ctx); err != nil {
		return err
	}
	if err := s.setSearchIndexVersion(ctx, "section_tokens", sectionTokenIndexVersion); err != nil {
		return err
	}
	return nil
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
select s.id, s.document_id, d.title, s.heading_path, s.title, s.content
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
		content       string
	}
	pending := make([]rowData, 0)
	for rows.Next() {
		var row rowData
		if err := rows.Scan(&row.sectionID, &row.documentID, &row.documentTitle, &row.headingPath, &row.sectionTitle, &row.content); err != nil {
			return err
		}
		pending = append(pending, row)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, row := range pending {
		titleTokens, headingTokens, symbolTokens, contentTokens := buildSectionIndexTexts(row.documentTitle, row.sectionTitle, row.headingPath, row.content)
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
	if _, err := tx.ExecContext(ctx, `delete from nodes where metadata_json like ?`, sourceIDLike(id)); err != nil {
		return err
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
	nodes, err := s.ListSourceNodes(ctx, sourceID, limit, offset)
	if err != nil {
		return domain.SourceArtifacts{}, err
	}
	edges, err := s.ListSourceEdges(ctx, sourceID, limit, offset)
	if err != nil {
		return domain.SourceArtifacts{}, err
	}
	return domain.SourceArtifacts{
		SourceID:  sourceID,
		Counts:    counts,
		Documents: docs,
		Sections:  sections,
		Nodes:     nodes,
		Edges:     edges,
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
	if err := s.readDB().QueryRowContext(ctx, `select count(*) from nodes where metadata_json like ?`, sourceIDLike(sourceID)).Scan(&counts.Nodes); err != nil {
		return counts, err
	}
	if err := s.readDB().QueryRowContext(ctx, `
select count(distinct e.id)
from edges e
join nodes src on src.id = e.src_id
join nodes dst on dst.id = e.dst_id
left join sections sec on sec.id = e.evidence_section_id
left join documents doc on doc.id = sec.document_id
where src.metadata_json like ?
   or dst.metadata_json like ?
   or doc.source_id = ?
`, sourceIDLike(sourceID), sourceIDLike(sourceID), sourceID).Scan(&counts.Edges); err != nil {
		return counts, err
	}
	return counts, nil
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

func (s *Store) ListSourceSections(ctx context.Context, sourceID string, limit, offset int) ([]domain.SectionSummary, error) {
	sourceID, limit, offset, err := normalizeSourceArtifactPage(sourceID, limit, offset)
	if err != nil {
		return nil, err
	}
	if _, err := s.GetSource(ctx, sourceID); err != nil {
		return nil, err
	}
	rows, err := s.readDB().QueryContext(ctx, `
select sections.id, sections.document_id, documents.title, sections.title, sections.heading_path,
       substr(replace(replace(sections.content, char(10), ' '), char(13), ' '), 1, 220),
       sections.ordinal
from sections
join documents on documents.id = sections.document_id
where documents.source_id = ?
order by documents.title asc, sections.ordinal asc
limit ? offset ?
`, sourceID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sections []domain.SectionSummary
	for rows.Next() {
		var section domain.SectionSummary
		if err := rows.Scan(&section.ID, &section.DocumentID, &section.DocumentTitle, &section.Title, &section.HeadingPath, &section.ContentSnippet, &section.Ordinal); err != nil {
			return nil, err
		}
		section.NodeID = stableSectionNodeID(section.ID)
		sections = append(sections, section)
	}
	return sections, rows.Err()
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
select id, kind, name, canonical_name, metadata_json, confidence, created_at, updated_at
from nodes
where metadata_json like ?
order by kind asc, name asc
limit ? offset ?
`, sourceIDLike(sourceID), limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	nodes := make([]domain.Node, 0)
	for rows.Next() {
		var node domain.Node
		if err := rows.Scan(&node.ID, &node.Kind, &node.Name, &node.CanonicalName, &node.MetadataJSON, &node.Confidence, &node.CreatedAt, &node.UpdatedAt); err != nil {
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
select distinct e.id, e.src_id, src.name, src.kind, e.dst_id, dst.name, dst.kind,
       e.kind, e.confidence, e.provenance, coalesce(e.evidence_section_id, '')
from edges e
join nodes src on src.id = e.src_id
join nodes dst on dst.id = e.dst_id
left join sections sec on sec.id = e.evidence_section_id
left join documents doc on doc.id = sec.document_id
where src.metadata_json like ?
   or dst.metadata_json like ?
   or doc.source_id = ?
order by e.kind asc, src.name asc, dst.name asc
limit ? offset ?
`, sourceIDLike(sourceID), sourceIDLike(sourceID), sourceID, limit, offset)
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
insert into documents (id, source_id, external_id, title, url, version, content_hash, indexed_at)
values (?, ?, ?, ?, ?, ?, ?, current_timestamp)
on conflict(id) do update set
  source_id = excluded.source_id,
  external_id = excluded.external_id,
  title = excluded.title,
  url = excluded.url,
  version = excluded.version,
  content_hash = excluded.content_hash,
  updated_at = current_timestamp,
  indexed_at = current_timestamp
`, doc.ID, doc.SourceID, doc.ExternalID, doc.Title, doc.URL, doc.Version, doc.ContentHash)
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `delete from fts_section_tokens where document_id = ?`, doc.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `delete from fts_section_tokens_trigram where document_id = ?`, doc.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `delete from sections where document_id = ?`, doc.ID); err != nil {
		return err
	}

	for _, section := range sections {
		_, err = tx.ExecContext(ctx, `
insert into sections (id, document_id, heading_path, title, content, content_hash, ordinal)
values (?, ?, ?, ?, ?, ?, ?)
`, section.ID, doc.ID, section.HeadingPath, section.Title, section.Content, section.ContentHash, section.Ordinal)
		if err != nil {
			return err
		}
		titleTokens, headingTokens, symbolTokens, contentTokens := buildSectionIndexTexts(doc.Title, section.Title, section.HeadingPath, section.Content)
		_, err = tx.ExecContext(ctx, `
insert into fts_section_tokens (title_tokens, section_heading_tokens, symbol_tokens, content_tokens, section_id, document_id)
values (?, ?, ?, ?, ?, ?)
`, titleTokens, headingTokens, symbolTokens, contentTokens, section.ID, doc.ID)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
insert into fts_section_tokens_trigram (title_tokens, section_heading_tokens, symbol_tokens, content_tokens, section_id, document_id)
values (?, ?, ?, ?, ?, ?)
`, titleTokens, headingTokens, symbolTokens, contentTokens, section.ID, doc.ID)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
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
		if _, err := tx.ExecContext(ctx, `delete from fts_section_tokens where document_id = ?`, docID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `delete from fts_section_tokens_trigram where document_id = ?`, docID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `delete from fts_nodes where node_id in (select id from nodes where metadata_json like ?)`, documentIDLike(docID)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `delete from nodes where metadata_json like ?`, documentIDLike(docID)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `delete from documents where id = ?`, docID); err != nil {
			return err
		}
	}
	return tx.Commit()
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
	if err := s.ensureDocumentProfile(ctx, documentID); err != nil {
		return domain.DocumentProfile{}, err
	}
	return s.getDocumentProfile(ctx, documentID)
}

func (s *Store) UpdateDocumentProfileDesc(ctx context.Context, input domain.DocumentProfileInput) (domain.DocumentProfile, error) {
	input.DocumentID = strings.TrimSpace(input.DocumentID)
	if input.DocumentID == "" {
		return domain.DocumentProfile{}, fmt.Errorf("document id is required")
	}
	if err := s.ensureDocumentProfile(ctx, input.DocumentID); err != nil {
		return domain.DocumentProfile{}, err
	}
	_, err := s.db.ExecContext(ctx, `
update document_profiles
set "desc" = ?, updated_at = current_timestamp
where document_id = ?
`, input.Desc, input.DocumentID)
	if err != nil {
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
	_, err := s.db.ExecContext(ctx, `
insert into document_profiles (document_id, retrieval_profile_json, generated_from_hash, generated_at)
values (?, ?, ?, current_timestamp)
on conflict(document_id) do update set
  retrieval_profile_json = excluded.retrieval_profile_json,
  generated_from_hash = excluded.generated_from_hash,
  generated_at = excluded.generated_at,
  updated_at = current_timestamp
`, input.DocumentID, input.RetrievalProfileJSON, input.GeneratedFromHash)
	if err != nil {
		return domain.DocumentProfile{}, err
	}
	return s.getDocumentProfile(ctx, input.DocumentID)
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
`, strings.TrimSpace(errText), id)
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.SyncJob{}, err
	}
	defer tx.Rollback()

	var running string
	err = tx.QueryRowContext(ctx, `
select id
from jobs
where kind = 'sync_source'
  and status in ('queued', 'running')
  and (source_id = ? or payload_json like ?)
order by rowid desc
limit 1
`, sourceID, sourceIDLike(sourceID)).Scan(&running)
	if err == nil {
		return domain.SyncJob{}, domain.ErrSyncInProgress
	}
	if err != sql.ErrNoRows {
		return domain.SyncJob{}, err
	}

	jobID := ids.Random("job", 12)
	payload := syncJobPayload(sourceID, domain.ResultPayload{})
	if _, err := tx.ExecContext(ctx, `
insert into jobs (id, kind, status, source_id, payload_json, progress_json, result_json)
values (?, 'sync_source', 'queued', ?, ?, '{}', '{}')
`, jobID, sourceID, payload); err != nil {
		return domain.SyncJob{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.SyncJob{}, err
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
	sourceID := job.SourceID
	if sourceID == "" {
		sourceID = sourceIDFromPayload(job.PayloadJSON)
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
`, sourceID, payload, payload, id)
	if err != nil {
		return err
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
  and (source_id = ? or payload_json like ?)
order by rowid desc
limit ?
`, sourceID, sourceIDLike(sourceID), limit)
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
	result, err := s.db.ExecContext(ctx, `
delete from jobs
where id = ?
  and kind = 'sync_source'
  and (source_id = ? or payload_json like ?)
`, jobID, sourceID, sourceIDLike(sourceID))
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
		SourceID    string              `json:"source_id"`
		Documents   int                 `json:"documents"`
		BrokenLinks []domain.BrokenLink `json:"broken_links,omitempty"`
	}{
		SourceID:    sourceID,
		Documents:   result.Documents,
		BrokenLinks: result.BrokenLinks,
	})
	if err != nil {
		return "{}"
	}
	return string(data)
}

func sourceIDFromPayload(payload string) string {
	var value struct {
		SourceID string `json:"source_id"`
	}
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return ""
	}
	return value.SourceID
}

func sourceIDLike(sourceID string) string {
	return `%` + jsonField("source_id", sourceID) + `%`
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

func documentIDLike(documentID string) string {
	return `%` + jsonField("document_id", documentID) + `%`
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

func jsonField(key string, value string) string {
	data, err := json.Marshal(map[string]string{key: value})
	if err != nil {
		return `"` + key + `":"` + value + `"`
	}
	field := strings.TrimPrefix(strings.TrimSuffix(string(data), "}"), "{")
	return field
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
		return err
	}
	if _, err := tx.ExecContext(ctx, `delete from fts_nodes where node_id = ?`, node.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
insert into fts_nodes (kind, name, canonical_name, metadata_json, node_id)
values (?, ?, ?, ?, ?)
`, node.Kind, node.Name, node.CanonicalName, node.MetadataJSON, node.ID); err != nil {
		return err
	}
	return tx.Commit()
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

	_, err := s.db.ExecContext(ctx, `
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
	return err
}

func (s *Store) SearchNodes(ctx context.Context, query string, limit int) ([]domain.Node, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	like := "%" + strings.ToLower(query) + "%"
	ftsQuery := nodeFTSQuery(query)
	args := []any{query, query, query}
	ftsCandidate := ""
	if ftsQuery != "" {
		ftsCandidate = `
  union all
  select n.id, 2
  from fts_nodes f
  join nodes n on n.id = f.node_id
  where fts_nodes match ?
`
		args = append(args, ftsQuery)
	}
	args = append(args, like, like, like, like, like, limit)
	rows, err := s.readDB().QueryContext(ctx, `
with candidates as (
  select id, 0 as source_rank from nodes where id = ?
  union all
  select id, 1 from nodes where lower(name) = lower(?) or lower(canonical_name) = lower(?)
`+ftsCandidate+`
  union all
  select id, 3
  from nodes
  where lower(id) like ?
     or lower(kind) like ?
     or lower(name) like ?
     or lower(canonical_name) like ?
     or lower(metadata_json) like ?
),
ranked as (
  select id, min(source_rank) as source_rank
  from candidates
  group by id
)
select n.id, n.kind, n.name, n.canonical_name, n.metadata_json, n.confidence, n.created_at, n.updated_at
from ranked r
join nodes n on n.id = r.id
order by r.source_rank, n.kind asc, n.name asc
limit ?
`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	nodes := make([]domain.Node, 0)
	for rows.Next() {
		var node domain.Node
		if err := rows.Scan(&node.ID, &node.Kind, &node.Name, &node.CanonicalName, &node.MetadataJSON, &node.Confidence, &node.CreatedAt, &node.UpdatedAt); err != nil {
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
select id, kind, name, canonical_name, metadata_json, confidence, created_at, updated_at
from nodes
where id = ?
`, id).Scan(&node.ID, &node.Kind, &node.Name, &node.CanonicalName, &node.MetadataJSON, &node.Confidence, &node.CreatedAt, &node.UpdatedAt)
	if err != nil {
		return domain.Node{}, err
	}
	return node, nil
}

func (s *Store) GetSection(ctx context.Context, id string) (domain.SectionContent, error) {
	var sc domain.SectionContent
	err := s.readDB().QueryRowContext(ctx, `
select s.id, s.document_id, d.title, d.url, s.title, s.heading_path, s.content
from sections s
join documents d on s.document_id = d.id
where s.id = ?
`, id).Scan(&sc.SectionID, &sc.DocumentID, &sc.DocumentTitle, &sc.DocumentURL, &sc.Title, &sc.HeadingPath, &sc.Content)
	if err != nil {
		return domain.SectionContent{}, err
	}
	return sc, nil
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
  n.id, n.kind, n.name, n.canonical_name, n.metadata_json, n.confidence, n.created_at, n.updated_at,
  e.id, e.src_id, e.dst_id, e.kind, e.confidence, e.provenance, e.evidence_section_id, e.source_revision, e.metadata_json, e.created_at, e.updated_at
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
  n.id, n.kind, n.name, n.canonical_name, n.metadata_json, n.confidence, n.created_at, n.updated_at,
  e.id, e.src_id, e.dst_id, e.kind, e.confidence, e.provenance, e.evidence_section_id, e.source_revision, e.metadata_json, e.created_at, e.updated_at
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
			&item.Node.ID, &item.Node.Kind, &item.Node.Name, &item.Node.CanonicalName, &item.Node.MetadataJSON, &item.Node.Confidence, &item.Node.CreatedAt, &item.Node.UpdatedAt,
			&item.Edge.ID, &item.Edge.SrcID, &item.Edge.DstID, &item.Edge.Kind, &item.Edge.Confidence, &item.Edge.Provenance, &evidence, &item.Edge.SourceRevision, &item.Edge.MetadataJSON, &item.Edge.CreatedAt, &item.Edge.UpdatedAt,
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
	return err
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
	return err
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
	if opts.RelationDepth <= 0 || opts.RelationDepth > 1 {
		opts.RelationDepth = 1
	}

	collector := newSearchCollector(opts)
	attempts := make([]domain.SearchAttempt, 0, opts.MaxSearches)

	runAttempt := func(kind string, query string, terms []string, fn func() ([]domain.SearchHit, error)) error {
		if len(attempts) >= opts.MaxSearches {
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
	unicodeTerms := strongQueryTermTexts(queryTerms)
	tokenQuery := matchQueryFromTerms(unicodeTerms, 16)
	if err := runAttempt("unicode61", tokenQuery, unicodeTerms, func() ([]domain.SearchHit, error) {
		return s.searchSectionsTokenFTS(ctx, tokenQuery, opts.Limit*3, opts)
	}); err != nil {
		return domain.SearchResult{}, err
	}

	trigramQuery := trigramFTSQuery(opts.Query)
	if err := runAttempt("trigram", trigramQuery, nil, func() ([]domain.SearchHit, error) {
		return s.searchSectionsTokenTrigram(ctx, opts.Query, opts.Limit*3, opts)
	}); err != nil {
		return domain.SearchResult{}, err
	}

	if len(collector.hits) == 0 {
		profileTerms := profileSearchTerms(opts.Query)
		if len(profileTerms) > 0 {
			if err := runAttempt("profile_fallback", "", profileTerms, func() ([]domain.SearchHit, error) {
				return s.searchSectionsProfile(ctx, profileTerms, opts.Limit*2, opts)
			}); err != nil {
				return domain.SearchResult{}, err
			}
		}
	}
	if len(collector.hits) == 0 {
		terms := substringSearchTerms(opts.Query)
		if len(terms) > 0 {
			if err := runAttempt("like_fallback", "", terms, func() ([]domain.SearchHit, error) {
				return s.searchSectionsLike(ctx, terms, opts.Limit*2, opts)
			}); err != nil {
				return domain.SearchResult{}, err
			}
		}
	}

	hits := collector.results()
	if opts.UseRelationExpansion && len(hits) > 0 {
		var err error
		hits, err = s.expandSearchHitsWithRelations(ctx, hits, opts)
		if err != nil {
			return domain.SearchResult{}, err
		}
	}
	return domain.SearchResult{
		Query:        opts.Query,
		SearchesUsed: len(attempts),
		Attempts:     attempts,
		Hits:         hits,
	}, nil
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
	terms := strongQueryTermTexts(classifyQueryTerms(opts.Query))
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
			hit.QueryMatch.ScoreExplanation = weightedScoreExplanation(hit.QueryMatch.MatchedFields, breakdown)
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

func (s *Store) searchSectionsLike(ctx context.Context, terms []string, limit int, opts domain.SearchOptions) ([]domain.SearchHit, error) {
	return s.searchSectionsByTerms(ctx, terms, limit, opts, false)
}

func (s *Store) searchSectionsProfile(ctx context.Context, terms []string, limit int, opts domain.SearchOptions) ([]domain.SearchHit, error) {
	return s.searchSectionsByTerms(ctx, terms, limit, opts, true)
}

func (s *Store) searchSectionsByTerms(ctx context.Context, terms []string, limit int, opts domain.SearchOptions, profileOnly bool) ([]domain.SearchHit, error) {
	seen := map[string]bool{}
	hits := make([]domain.SearchHit, 0)
	for _, term := range terms {
		if len(hits) >= limit {
			break
		}
		pattern := "%" + escapeLike(term) + "%"
		where := `
where not exists (
    select 1 from feedback_events fe
    where fe.target_kind = 'document'
      and fe.target_id = documents.id
      and fe.feedback_kind = 'document_stale'
  )
`
		if profileOnly {
			where += `  and (document_profiles."desc" like ? escape '\' or document_profiles.retrieval_profile_json like ? escape '\')`
		} else {
			where += `  and (documents.title like ? escape '\' or sections.title like ? escape '\' or sections.heading_path like ? escape '\' or sections.content like ? escape '\')`
		}
		query := `
select sections.id, sections.document_id, documents.title, documents.url,
       coalesce(document_profiles."desc", ''),
       coalesce(document_profiles.retrieval_profile_json, '{}'),
       exists (
         select 1 from feedback_events fe
         where fe.target_kind = 'document'
           and fe.target_id = documents.id
           and fe.feedback_kind = 'document_canonical'
       ) as canonical,
       sections.title, sections.heading_path, sections.content, sections.ordinal
from sections
join documents on documents.id = sections.document_id
left join document_profiles on document_profiles.document_id = documents.id
` + where + `
order by canonical desc, documents.title asc, sections.ordinal asc
limit ?
`
		var rows *sql.Rows
		var err error
		if profileOnly {
			rows, err = s.readDB().QueryContext(ctx, query, pattern, pattern, limit)
		} else {
			rows, err = s.readDB().QueryContext(ctx, query, pattern, pattern, pattern, pattern, limit)
		}
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var hit domain.SearchHit
			var profileJSON string
			var ordinal int
			if err := rows.Scan(&hit.SectionID, &hit.DocumentID, &hit.DocumentTitle, &hit.DocumentURL, &hit.Desc, &profileJSON, &hit.Canonical, &hit.Title, &hit.HeadingPath, &hit.Content, &ordinal); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if seen[hit.SectionID] {
				continue
			}
			seen[hit.SectionID] = true
			hit.Snippet = snippetForTerm(hit.Content, term)
			hit.Rank = float64(1000 + ordinal)
			if profileOnly {
				enrichSearchHit(&hit, opts, profileJSON, []string{term}, "profile")
			} else {
				enrichSearchHit(&hit, opts, profileJSON, []string{term}, "content")
			}
			breakdown := weightedScoreBreakdown(hit, opts.Query, []string{term}, 1, "fallback")
			hit.Rank = breakdown.Total
			hit.ScoreBreakdown = &breakdown
			if hit.QueryMatch != nil {
				hit.QueryMatch.ScoreExplanation = weightedScoreExplanation(hit.QueryMatch.MatchedFields, breakdown)
			}
			hits = append(hits, hit)
			if len(hits) >= limit {
				break
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}
	return hits, nil
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
		perDoc[hit.DocumentID]++
		result = append(result, hit)
	}
	return result
}

func sortSearchHits(hits []domain.SearchHit) {
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Canonical != hits[j].Canonical {
			return hits[i].Canonical
		}
		if hits[i].Rank == hits[j].Rank {
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
		UnicodeBM25Boost: left.UnicodeBM25Boost + right.UnicodeBM25Boost,
		TrigramBM25Boost: left.TrigramBM25Boost + right.TrigramBM25Boost,
		TitleBoost:       maxFloat(left.TitleBoost, right.TitleBoost),
		SectionBoost:     maxFloat(left.SectionBoost, right.SectionBoost),
		SymbolBoost:      maxFloat(left.SymbolBoost, right.SymbolBoost),
		ExactMatchBoost:  maxFloat(left.ExactMatchBoost, right.ExactMatchBoost),
		CanonicalBoost:   maxFloat(left.CanonicalBoost, right.CanonicalBoost),
		CoverageBoost:    maxFloat(left.CoverageBoost, right.CoverageBoost),
		FallbackBoost:    maxFloat(left.FallbackBoost, right.FallbackBoost),
		MatchedFields:    uniqueStrings(append(left.MatchedFields, right.MatchedFields...)),
		MatchedTerms:     uniqueStrings(append(left.MatchedTerms, right.MatchedTerms...)),
		MatchedSymbols:   uniqueStrings(append(left.MatchedSymbols, right.MatchedSymbols...)),
	}
	merged.Total = merged.UnicodeBM25Boost +
		merged.TrigramBM25Boost +
		merged.TitleBoost +
		merged.SectionBoost +
		merged.SymbolBoost +
		merged.ExactMatchBoost +
		merged.CanonicalBoost +
		merged.CoverageBoost +
		merged.FallbackBoost
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
	if exactHit(hit, query) {
		breakdown.ExactMatchBoost = 100
	}
	breakdown.CoverageBoost = coverageBoost(hit, terms)
	if hit.QueryMatch != nil {
		breakdown.MatchedFields = hit.QueryMatch.MatchedFields
	}
	breakdown.Total = breakdown.UnicodeBM25Boost +
		breakdown.TrigramBM25Boost +
		breakdown.TitleBoost +
		breakdown.SectionBoost +
		breakdown.SymbolBoost +
		breakdown.ExactMatchBoost +
		breakdown.CanonicalBoost +
		breakdown.CoverageBoost +
		breakdown.FallbackBoost
	return breakdown
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
