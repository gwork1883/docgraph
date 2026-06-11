package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	_ "modernc.org/sqlite"

	"github.com/docgraph/docgraph/internal/domain"
	"github.com/docgraph/docgraph/internal/ids"
	"github.com/docgraph/docgraph/internal/storage/sqlschema"
)

type Store struct {
	db  *sql.DB
	dsn string
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	path, err := pathFromDSN(dsn)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(time.Hour)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &Store{db: db, dsn: dsn}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, sqlschema.Schema); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
update jobs
set status = 'failed',
    last_error = 'Previous run interrupted (process exited while job was running)',
    updated_at = current_timestamp
where status = 'running'
`)
	return err
}

func (s *Store) Status(ctx context.Context) (sqlschema.Status, error) {
	count := func(table string) (int64, error) {
		var n int64
		err := s.db.QueryRowContext(ctx, "select count(*) from "+table).Scan(&n)
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

func (s *Store) CreateSource(ctx context.Context, source domain.Source) (domain.Source, error) {
	if source.ConfigJSON == "" {
		source.ConfigJSON = "{}"
	}
	_, err := s.db.ExecContext(ctx, `
insert into sources (id, kind, name, dsn, config_json, product_hint, module_hint, sync_schedule)
values (?, ?, ?, ?, ?, ?, ?, ?)
on conflict(id) do update set
  kind = excluded.kind,
  name = excluded.name,
  dsn = excluded.dsn,
  config_json = excluded.config_json,
  product_hint = excluded.product_hint,
  module_hint = excluded.module_hint,
  sync_schedule = excluded.sync_schedule,
  updated_at = current_timestamp
`, source.ID, source.Kind, source.Name, source.DSN, source.ConfigJSON, source.ProductHint, source.ModuleHint, source.SyncSchedule)
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
delete from fts_sections
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
	rows, err := s.db.QueryContext(ctx, `
select id, kind, name, dsn, config_json, product_hint, module_hint, sync_schedule, created_at, updated_at
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
		if err := rows.Scan(&source.ID, &source.Kind, &source.Name, &source.DSN, &source.ConfigJSON, &source.ProductHint, &source.ModuleHint, &source.SyncSchedule, &source.CreatedAt, &source.UpdatedAt); err != nil {
			return nil, err
		}
		sources = append(sources, source)
	}
	return sources, rows.Err()
}

func (s *Store) GetSource(ctx context.Context, id string) (domain.Source, error) {
	var source domain.Source
	err := s.db.QueryRowContext(ctx, `
select id, kind, name, dsn, config_json, product_hint, module_hint, sync_schedule, created_at, updated_at
from sources
where id = ?
`, id).Scan(&source.ID, &source.Kind, &source.Name, &source.DSN, &source.ConfigJSON, &source.ProductHint, &source.ModuleHint, &source.SyncSchedule, &source.CreatedAt, &source.UpdatedAt)
	if err != nil {
		return domain.Source{}, err
	}
	return source, nil
}

func (s *Store) ListSourceArtifacts(ctx context.Context, sourceID string, limit, offset int) (domain.SourceArtifacts, error) {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return domain.SourceArtifacts{}, fmt.Errorf("source id is required")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	if _, err := s.GetSource(ctx, sourceID); err != nil {
		return domain.SourceArtifacts{}, err
	}

	counts, err := s.sourceArtifactCounts(ctx, sourceID)
	if err != nil {
		return domain.SourceArtifacts{}, err
	}
	docs, err := s.listSourceDocuments(ctx, sourceID, limit, offset)
	if err != nil {
		return domain.SourceArtifacts{}, err
	}
	sections, err := s.listSourceSections(ctx, sourceID, limit, offset)
	if err != nil {
		return domain.SourceArtifacts{}, err
	}
	nodes, err := s.listSourceNodes(ctx, sourceID, limit, offset)
	if err != nil {
		return domain.SourceArtifacts{}, err
	}
	edges, err := s.listSourceEdges(ctx, sourceID, limit, offset)
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

func (s *Store) sourceArtifactCounts(ctx context.Context, sourceID string) (domain.SourceArtifactCounts, error) {
	var counts domain.SourceArtifactCounts
	if err := s.db.QueryRowContext(ctx, `select count(*) from documents where source_id = ?`, sourceID).Scan(&counts.Documents); err != nil {
		return counts, err
	}
	if err := s.db.QueryRowContext(ctx, `
select count(*)
from sections
join documents on documents.id = sections.document_id
where documents.source_id = ?
`, sourceID).Scan(&counts.Sections); err != nil {
		return counts, err
	}
	if err := s.db.QueryRowContext(ctx, `select count(*) from nodes where metadata_json like ?`, sourceIDLike(sourceID)).Scan(&counts.Nodes); err != nil {
		return counts, err
	}
	if err := s.db.QueryRowContext(ctx, `
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

func (s *Store) listSourceDocuments(ctx context.Context, sourceID string, limit, offset int) ([]domain.DocumentSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
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

func (s *Store) listSourceSections(ctx context.Context, sourceID string, limit, offset int) ([]domain.SectionSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
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

func (s *Store) listSourceNodes(ctx context.Context, sourceID string, limit, offset int) ([]domain.Node, error) {
	rows, err := s.db.QueryContext(ctx, `
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

func (s *Store) listSourceEdges(ctx context.Context, sourceID string, limit, offset int) ([]domain.EdgeSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
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

	if _, err := tx.ExecContext(ctx, `delete from fts_sections where document_id = ?`, doc.ID); err != nil {
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
		_, err = tx.ExecContext(ctx, `
insert into fts_sections (title, heading_path, content, section_id, document_id)
values (?, ?, ?, ?, ?)
`, section.Title, section.HeadingPath, section.Content, section.ID, doc.ID)
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
		if _, err := tx.ExecContext(ctx, `delete from fts_sections where document_id = ?`, docID); err != nil {
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
	err := s.db.QueryRowContext(ctx, `
select document_id, "desc", retrieval_profile_json, generated_from_hash, generated_at, created_at, updated_at
from document_profiles
where document_id = ?
`, documentID).Scan(&profile.DocumentID, &profile.Desc, &profile.RetrievalProfileJSON, &profile.GeneratedFromHash, &profile.GeneratedAt, &profile.CreatedAt, &profile.UpdatedAt)
	if err != nil {
		return domain.DocumentProfile{}, err
	}
	return profile, nil
}

func (s *Store) CreateSyncJob(ctx context.Context, sourceID string) (domain.SyncJob, error) {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return domain.SyncJob{}, fmt.Errorf("source id is required")
	}
	jobID := ids.Random("job", 12)
	payload := syncJobPayload(sourceID, domain.ResultPayload{})
	_, err := s.db.ExecContext(ctx, `
insert into jobs (id, kind, status, payload_json, attempts)
values (?, 'sync_source', 'running', ?, 1)
`, jobID, payload)
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
  and status = 'running'
  and payload_json like ?
order by rowid desc
limit 1
`, sourceIDLike(sourceID)).Scan(&running)
	if err == nil {
		return domain.SyncJob{}, domain.ErrSyncInProgress
	}
	if err != sql.ErrNoRows {
		return domain.SyncJob{}, err
	}

	jobID := ids.Random("job", 12)
	payload := syncJobPayload(sourceID, domain.ResultPayload{})
	if _, err := tx.ExecContext(ctx, `
insert into jobs (id, kind, status, payload_json, attempts)
values (?, 'sync_source', 'running', ?, 1)
`, jobID, payload); err != nil {
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
	sourceID := sourceIDFromPayload(job.PayloadJSON)
	payload := syncJobPayload(sourceID, resultPayload)
	_, err = s.db.ExecContext(ctx, `
update jobs
set status = 'completed',
    payload_json = ?,
    last_error = '',
    updated_at = current_timestamp
where id = ?
`, payload, id)
	return err
}

func (s *Store) FailSyncJob(ctx context.Context, id string, errText string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("job id is required")
	}
	_, err := s.db.ExecContext(ctx, `
update jobs
set status = 'failed',
    last_error = ?,
    updated_at = current_timestamp
where id = ?
`, strings.TrimSpace(errText), id)
	return err
}

func (s *Store) ListSyncJobs(ctx context.Context, sourceID string, limit int) ([]domain.SyncJob, error) {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return nil, fmt.Errorf("source id is required")
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
select id, kind, status, payload_json, attempts, run_after, coalesce(locked_until, ''), last_error, created_at, updated_at
from jobs
where kind = 'sync_source' and payload_json like ?
order by rowid desc
limit ?
`, sourceIDLike(sourceID), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []domain.SyncJob
	for rows.Next() {
		var job domain.SyncJob
		if err := rows.Scan(&job.ID, &job.Kind, &job.Status, &job.PayloadJSON, &job.Attempts, &job.RunAfter, &job.LockedUntil, &job.LastError, &job.CreatedAt, &job.UpdatedAt); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
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
  and payload_json like ?
`, jobID, sourceIDLike(sourceID))
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
	var job domain.SyncJob
	err := s.db.QueryRowContext(ctx, `
select id, kind, status, payload_json, attempts, run_after, coalesce(locked_until, ''), last_error, created_at, updated_at
from jobs
where id = ?
`, id).Scan(&job.ID, &job.Kind, &job.Status, &job.PayloadJSON, &job.Attempts, &job.RunAfter, &job.LockedUntil, &job.LastError, &job.CreatedAt, &job.UpdatedAt)
	if err != nil {
		return domain.SyncJob{}, err
	}
	return job, nil
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

	_, err := s.db.ExecContext(ctx, `
insert into nodes (id, kind, name, canonical_name, metadata_json, confidence)
values (?, ?, ?, ?, ?, ?)
on conflict(id) do update set
  kind = excluded.kind,
  name = excluded.name,
  canonical_name = excluded.canonical_name,
  metadata_json = excluded.metadata_json,
  confidence = excluded.confidence,
  updated_at = current_timestamp
`, node.ID, node.Kind, node.Name, node.CanonicalName, node.MetadataJSON, node.Confidence)
	return err
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
	rows, err := s.db.QueryContext(ctx, `
select id, kind, name, canonical_name, metadata_json, confidence, created_at, updated_at
from nodes
where lower(id) like ?
   or lower(kind) like ?
   or lower(name) like ?
   or lower(canonical_name) like ?
   or lower(metadata_json) like ?
order by
  case when id = ? then 0 when lower(name) = lower(?) then 1 else 2 end,
  kind asc,
  name asc
limit ?
`, like, like, like, like, like, query, query, limit)
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

func (s *Store) GetNode(ctx context.Context, id string) (domain.Node, error) {
	var node domain.Node
	err := s.db.QueryRowContext(ctx, `
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
	err := s.db.QueryRowContext(ctx, `
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
	rows, err := s.db.QueryContext(ctx, query, id, kind, kind, limit)
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
	err := s.db.QueryRowContext(ctx, `
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

	rows, err := s.db.QueryContext(ctx, `
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
		opts.MaxSearches = 3
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

	phraseQuery := quoteMatchQuery(opts.Query)
	if err := runAttempt("phrase", phraseQuery, nil, func() ([]domain.SearchHit, error) {
		return s.searchSectionsMatch(ctx, phraseQuery, opts.Limit*2, opts)
	}); err != nil {
		return domain.SearchResult{}, err
	}

	tokenQuery := fallbackMatchQuery(opts.Query)
	if tokenQuery != "" && tokenQuery != phraseQuery {
		if err := runAttempt("token", tokenQuery, nil, func() ([]domain.SearchHit, error) {
			return s.searchSectionsMatch(ctx, tokenQuery, opts.Limit*2, opts)
		}); err != nil {
			return domain.SearchResult{}, err
		}
	}

	if hasHan(opts.Query) {
		terms := substringSearchTerms(opts.Query)
		if len(terms) > 0 {
			if err := runAttempt("ngram", "", terms, func() ([]domain.SearchHit, error) {
				return s.searchSectionsLike(ctx, terms, opts.Limit*2, opts)
			}); err != nil {
				return domain.SearchResult{}, err
			}
		}
	}

	profileTerms := profileSearchTerms(opts.Query)
	if len(profileTerms) > 0 {
		if err := runAttempt("profile", "", profileTerms, func() ([]domain.SearchHit, error) {
			return s.searchSectionsProfile(ctx, profileTerms, opts.Limit*2, opts)
		}); err != nil {
			return domain.SearchResult{}, err
		}
	}

	hits := collector.results()
	return domain.SearchResult{
		Query:        opts.Query,
		SearchesUsed: len(attempts),
		Attempts:     attempts,
		Hits:         hits,
	}, nil
}

func (s *Store) searchSectionsMatch(ctx context.Context, matchQuery string, limit int, opts domain.SearchOptions) ([]domain.SearchHit, error) {
	rows, err := s.db.QueryContext(ctx, `
select fts_sections.section_id, fts_sections.document_id, documents.title, documents.url,
       coalesce(document_profiles."desc", ''),
       coalesce(document_profiles.retrieval_profile_json, '{}'),
       exists (
         select 1 from feedback_events fe
         where fe.target_kind = 'document'
           and fe.target_id = documents.id
           and fe.feedback_kind = 'document_canonical'
       ) as canonical,
       sections.title, sections.heading_path, sections.content,
       snippet(fts_sections, 2, '<mark>', '</mark>', '...', 16), rank
from fts_sections
join sections on sections.id = fts_sections.section_id
join documents on documents.id = fts_sections.document_id
left join document_profiles on document_profiles.document_id = documents.id
where fts_sections match ?
  and not exists (
    select 1 from feedback_events fe
    where fe.target_kind = 'document'
      and fe.target_id = documents.id
      and fe.feedback_kind = 'document_stale'
  )
order by canonical desc, rank
limit ?
`, matchQuery, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hits []domain.SearchHit
	for rows.Next() {
		var hit domain.SearchHit
		var profileJSON string
		if err := rows.Scan(&hit.SectionID, &hit.DocumentID, &hit.DocumentTitle, &hit.DocumentURL, &hit.Desc, &profileJSON, &hit.Canonical, &hit.Title, &hit.HeadingPath, &hit.Content, &hit.Snippet, &hit.Rank); err != nil {
			return nil, err
		}
		enrichSearchHit(&hit, opts, profileJSON, searchTerms(opts.Query), "fts")
		hits = append(hits, hit)
	}
	return hits, rows.Err()
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
			rows, err = s.db.QueryContext(ctx, query, pattern, pattern, limit)
		} else {
			rows, err = s.db.QueryContext(ctx, query, pattern, pattern, pattern, pattern, limit)
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
			// LLM should use doc_get_section to retrieve full content of relevant sections.
			hit.Content = ""
			hit.Desc = ""
			hit.Profile = nil
			hit.RetrievalProfile = nil
			hit.QueryMatch = nil
		} else {
			// Content mode (default behavior): include full content with truncation.
			if c.opts.MaxCharsPerResult > 0 {
				hit.Content = truncateBytes(hit.Content, c.opts.MaxCharsPerResult)
			}
			if c.opts.ProfileDetail == "none" {
				hit.Desc = ""
				hit.Profile = nil
				hit.RetrievalProfile = nil
			}
		}
		if idx, ok := c.seen[hit.SectionID]; ok {
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
