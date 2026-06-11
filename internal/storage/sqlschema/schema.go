package sqlschema

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
  payload_json text not null default '{}',
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

create virtual table if not exists fts_sections using fts5(
  title,
  heading_path,
  content,
  section_id unindexed,
  document_id unindexed
);

create index if not exists idx_documents_source on documents(source_id);
create index if not exists idx_sections_document on sections(document_id);
create index if not exists idx_document_profiles_generated_hash on document_profiles(generated_from_hash);
create index if not exists idx_nodes_kind_name on nodes(kind, canonical_name);
create index if not exists idx_edges_src_kind on edges(src_id, kind);
create index if not exists idx_edges_dst_kind on edges(dst_id, kind);
create index if not exists idx_jobs_status_run_after on jobs(status, run_after);
`
