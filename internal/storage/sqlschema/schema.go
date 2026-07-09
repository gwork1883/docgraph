package sqlschema

const CurrentSchemaVersion = 4

type Status struct {
	StorageDSN string `json:"storage_dsn"`
	Sources    int64  `json:"sources"`
	Documents  int64  `json:"documents"`
	Sections   int64  `json:"sections"`
	Nodes      int64  `json:"nodes"`
	Edges      int64  `json:"edges"`
	Jobs       int64  `json:"jobs"`
}

const Schema = `
pragma journal_mode = wal;
pragma foreign_keys = on;

create table if not exists schema_migrations (
  version integer primary key,
  applied_at text not null default current_timestamp
);

create table if not exists sources (
  id text primary key,
  kind text not null,
  name text not null,
  dsn text not null,
  config_json text not null default '{}',
  product_hint text not null default '',
  module_hint text not null default '',
  sync_schedule text not null default '',
  sync_status text not null default 'active',
  sync_status_reason text not null default '',
  sync_paused_at text not null default '',
  created_at text not null default current_timestamp,
  updated_at text not null default current_timestamp
);

create table if not exists confluence_cookie_credentials (
  id text primary key,
  name text not null,
  base_url text not null,
  cookie text not null default '',
  notes text not null default '',
  status text not null default 'unknown',
  last_validated_at text not null default '',
  last_error text not null default '',
  created_at text not null default current_timestamp,
  updated_at text not null default current_timestamp
);

create table if not exists documents (
  id text primary key,
  source_id text not null references sources(id) on delete cascade,
  external_id text not null,
  title text not null,
  url text not null default '',
  version text not null default '',
  content_hash text not null,
  metadata_json text not null default '{}',
  acl_json text not null default '{}',
  updated_at text not null default current_timestamp,
  indexed_at text not null default current_timestamp,
  unique(source_id, external_id)
);

create table if not exists sections (
  id text primary key,
  document_id text not null references documents(id) on delete cascade,
  heading_path text not null default '',
  title text not null default '',
  content text not null default '',
  content_hash text not null,
  ordinal integer not null default 0,
  metadata_json text not null default '{}',
  created_at text not null default current_timestamp,
  updated_at text not null default current_timestamp
);

create table if not exists document_profiles (
  document_id text primary key references documents(id) on delete cascade,
  "desc" text not null default '',
  retrieval_profile_json text not null default '{}',
  generated_from_hash text not null default '',
  generated_at text not null default '',
  created_at text not null default current_timestamp,
  updated_at text not null default current_timestamp
);

create table if not exists section_entities (
  id text primary key,
  section_id text not null references sections(id) on delete cascade,
  document_id text not null references documents(id) on delete cascade,
  kind text not null,
  raw_text text not null default '',
  canonical_text text not null default '',
  method text not null default '',
  path text not null default '',
  operation text not null default '',
  source text not null default '',
  confidence real not null default 0,
  evidence_json text not null default '{}',
  created_at text not null default current_timestamp
);

create table if not exists knowledge_relation_proposals (
  id text primary key,
  relation_type text not null,
  from_document_id text not null references documents(id) on delete cascade,
  from_anchor text not null default '',
  to_document_id text not null references documents(id) on delete cascade,
  to_anchor text not null default '',
  direction text not null default 'directed',
  reason text not null,
  evidence_json text not null default '{}',
  proposed_effect text not null default 'context_link',
  confidence real not null default 0,
  created_by_type text not null default '',
  created_by_ref text not null default '',
  status text not null default 'pending',
  reviewed_by text not null default '',
  review_note text not null default '',
  reviewed_at text not null default '',
  created_at text not null default current_timestamp,
  updated_at text not null default current_timestamp
);

create table if not exists knowledge_relations (
  id text primary key,
  relation_type text not null,
  from_document_id text not null references documents(id) on delete cascade,
  from_anchor text not null default '',
  to_document_id text not null references documents(id) on delete cascade,
  to_anchor text not null default '',
  direction text not null default 'directed',
  effect text not null default 'context_link',
  weight real not null default 1.0,
  reason text not null,
  evidence_json text not null default '{}',
  approved_from_proposal_id text references knowledge_relation_proposals(id) on delete set null,
  created_by text not null default '',
  disabled_at text not null default '',
  created_at text not null default current_timestamp,
  updated_at text not null default current_timestamp
);

create table if not exists nodes (
  id text primary key,
  kind text not null,
  name text not null,
  canonical_name text not null,
  metadata_json text not null default '{}',
  confidence real not null default 1.0,
  created_at text not null default current_timestamp,
  updated_at text not null default current_timestamp
);

create table if not exists edges (
  id text primary key,
  src_id text not null references nodes(id) on delete cascade,
  dst_id text not null references nodes(id) on delete cascade,
  kind text not null,
  confidence real not null default 1.0,
  provenance text not null default 'rule',
  evidence_section_id text references sections(id) on delete set null,
  source_revision text not null default '',
  metadata_json text not null default '{}',
  created_at text not null default current_timestamp,
  updated_at text not null default current_timestamp
);

create table if not exists aliases (
  id text primary key,
  node_id text not null references nodes(id) on delete cascade,
  alias text not null,
  source text not null default '',
  unique(node_id, alias)
);

create table if not exists section_nodes (
  section_id text not null references sections(id) on delete cascade,
  node_id text not null references nodes(id) on delete cascade,
  role text not null default 'mentions',
  confidence real not null default 1.0,
  primary key(section_id, node_id, role)
);

create table if not exists jobs (
  id text primary key,
  kind text not null,
  status text not null,
  source_id text not null default '',
  target_kind text not null default '',
  target_id text not null default '',
  payload_json text not null default '{}',
  progress_json text not null default '{}',
  result_json text not null default '{}',
  worker_id text not null default '',
  attempts integer not null default 0,
  run_after text not null default current_timestamp,
  locked_until text,
  last_error text not null default '',
  created_at text not null default current_timestamp,
  updated_at text not null default current_timestamp
);

create table if not exists feedback_events (
  id text primary key,
  target_kind text not null,
  target_id text not null,
  feedback_kind text not null,
  payload_json text not null default '{}',
  actor text not null default '',
  created_at text not null default current_timestamp
);

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

create virtual table if not exists fts_nodes using fts5(
  kind,
  name,
  canonical_name,
  metadata_json,
  node_id unindexed
);

create index if not exists idx_documents_source on documents(source_id);
create index if not exists idx_sections_document on sections(document_id);
create index if not exists idx_document_profiles_generated_hash on document_profiles(generated_from_hash);
create index if not exists idx_section_entities_document on section_entities(document_id, kind, canonical_text);
create index if not exists idx_section_entities_section on section_entities(section_id, kind, canonical_text);
create index if not exists idx_section_entities_path on section_entities(kind, method, path, confidence);
create index if not exists idx_relation_proposals_status_created on knowledge_relation_proposals(status, created_at);
create index if not exists idx_relation_proposals_from on knowledge_relation_proposals(from_document_id, status);
create index if not exists idx_relation_proposals_to on knowledge_relation_proposals(to_document_id, status);
create index if not exists idx_knowledge_relations_from on knowledge_relations(from_document_id, relation_type, disabled_at);
create index if not exists idx_knowledge_relations_to on knowledge_relations(to_document_id, relation_type, disabled_at);
create index if not exists idx_knowledge_relations_type on knowledge_relations(relation_type, disabled_at);
create index if not exists idx_nodes_kind_name on nodes(kind, canonical_name);
create index if not exists idx_edges_src_kind on edges(src_id, kind);
create index if not exists idx_edges_dst_kind on edges(dst_id, kind);
create index if not exists idx_jobs_status_run_after on jobs(status, run_after);
create index if not exists idx_feedback_target on feedback_events(target_kind, target_id, feedback_kind);
create index if not exists idx_query_events_hash_created on query_events(query_hash, created_at);
create index if not exists idx_search_result_events_query on search_result_events(query_event_id, rank);
`
