# DocGraph Architecture Design

## 1. Architecture Principles

DocGraph should be boring to deploy and useful quickly.

Core principles:

- Single static binary by default.
- Docker is optional, not required.
- SQLite is the default storage engine.
- PostgreSQL and external indexes are optional future extensions.
- Configuration uses DSN strings for backend extensibility.
- Web UI, REST API, MCP server, and background worker run inside one process for MVP.
- The knowledge model and query API are more important than early database complexity.
- All returned knowledge should be traceable to source evidence.
- Permission filtering must happen before data is returned to users or AI agents.

## 2. Runtime Shape

Default command:

```bash
docgraph serve
```

Default runtime:

```text
docgraph binary
  Web UI
  REST API
  MCP HTTP endpoint
  Background worker
  Scheduler
  Connector runtime
  Query engine
  SQLite storage
```

Optional stdio MCP command:

```bash
docgraph mcp
```

CLI commands:

```bash
docgraph init
docgraph serve
docgraph mcp
docgraph source add
docgraph source list
docgraph sync
docgraph index
docgraph status
docgraph export
```

## 3. Technology Choice

Recommended language: Go.

Rationale:

- Produces simple static binaries.
- Good standard library for HTTP, CLI, concurrency, and file handling.
- Easy to embed Web assets.
- Lower maintenance cost than Rust for this kind of internal platform.
- Enough performance for indexing, search orchestration, APIs, and MCP.

Rust remains a strong option for lower-level engines, but DocGraph's early complexity is product integration, connectors, search, permissions, and context assembly. Go is the better default for fast and maintainable delivery.

## 4. High-Level Architecture

```text
Sources
  Git Markdown / Local Dir / Confluence / HTML / OpenAPI
      |
      v
Connectors
  Fetch, auth, incremental change detection
      |
      v
Normalizer
  Convert to canonical document and section model
      |
      v
Extractor
  Entities, links, APIs, owners, aliases, relationships
      |
      v
Index Writer
  Documents, sections, nodes, edges, evidence, FTS
      |
      v
Query Engine
  Search, related, impact, context packing
      |
      v
Interfaces
  Web UI / REST API / MCP
```

## 5. Process Components

### 5.1 Web UI

Embedded static assets served by the binary.

Responsibilities:

- Source configuration.
- Sync status and errors.
- Search and browse.
- Node detail.
- Related documents.
- Basic impact view.
- Manual curation.
- MCP/API setup help.

### 5.2 REST API

Serves Web UI and external integrations.

API authentication is configured at process startup. MVP supports two modes:

- `none`: no API token check, intended for localhost development.
- `token`: every non-health HTTP route requires a shared API token.

`GET /api/health` is intentionally exempt from token auth so local process managers,
load balancers, and scripts can check liveness without holding DocGraph credentials.
All other API and Web routes are protected when `auth.mode` is `token`.

Clients may provide the token with either header:

```text
X-DocGraph-Token: <token>
Authorization: Bearer <token>
```

Initial endpoints:

```text
GET  /api/health
GET  /api/status

GET  /api/sources
POST /api/sources
GET  /api/sources/{id}
PUT  /api/sources/{id}
POST /api/sources/{id}/sync

POST /api/search
POST /api/context
GET  /api/nodes/{id}
GET  /api/nodes/{id}/related
POST /api/impact

POST /api/feedback
```

### 5.3 MCP Server

MCP exposes DocGraph to AI agents.

MVP tools:

```text
doc_search(query, product?, limit?)
doc_context(task, product?, max_tokens?)
doc_get_node(node_id)
doc_related(node_id, relation_types?, depth?)
doc_impact(node_id, depth?)
product_owner(entity)
```

Potential resources:

```text
product://node/{node_id}
product://doc/{document_id}
product://section/{section_id}
```

MCP results should return structured content plus source evidence. The AI agent can synthesize an answer, but DocGraph should provide traceable facts.

### 5.4 Background Worker

Runs inside the same binary for MVP.

Responsibilities:

- Source sync jobs.
- Document normalization jobs.
- Extraction jobs.
- Index update jobs.
- Retry and error recording.
- Scheduled sync.

Jobs are stored in SQLite to avoid an external queue dependency.

### 5.5 Connector Runtime

Connectors implement a common interface:

```go
type Connector interface {
    Test(ctx context.Context, cfg SourceConfig) error
    List(ctx context.Context, cfg SourceConfig, cursor string) (Page[RemoteDocument], error)
    Fetch(ctx context.Context, cfg SourceConfig, ref RemoteRef) (RawDocument, error)
}
```

Initial connectors:

- Local directory.
- Git Markdown repository.
- Confluence.
- HTML documentation site.
- OpenAPI file or URL.

## 6. Configuration

Configuration should be file-based and overridable by environment variables.

Example:

```yaml
server:
  host: 127.0.0.1
  port: 8787
  data_dir: ./.docgraph

storage:
  dsn: sqlite://./.docgraph/docgraph.db

search:
  dsn: sqlite://./.docgraph/docgraph.db

vector:
  dsn: none://

auth:
  mode: token
  token: ${DOCGRAPH_TOKEN}
```

Authentication configuration:

```yaml
auth:
  mode: none
```

```yaml
auth:
  mode: token
  token: "replace-with-a-random-shared-secret"
```

`auth.mode` defaults to `none` for local development. When `auth.mode` is `token`,
`auth.token` is required and must be treated as a secret. Startup logs and error
messages must not print the token value.

Storage DSN examples:

```text
sqlite://./.docgraph/docgraph.db
sqlite:///var/lib/docgraph/docgraph.db
postgres://user:pass@host:5432/docgraph?sslmode=require
```

The MVP only needs to implement `sqlite://`. `postgres://` can be reserved by the interface and configuration model.

## 7. Storage Strategy

### 7.1 Default: SQLite

SQLite is sufficient for MVP because it provides:

- Single-file local storage.
- Transactions.
- FTS5 full-text search.
- JSON fields.
- Recursive CTEs for basic graph traversal.
- Low operational overhead.

The default data directory:

```text
.docgraph/
  docgraph.db
  blobs/
  config.yaml
  logs/
  cache/
```

### 7.2 Optional: PostgreSQL

PostgreSQL should be optional, not required.

It becomes useful when deployments need:

- Higher concurrency.
- Multi-instance server/worker deployment.
- Managed backup and restore.
- Central observability.
- Stronger enterprise operations.
- pgvector-based vector search.

### 7.3 Storage Abstraction

Use domain-level interfaces, not a generic ORM abstraction.

```go
type Store interface {
    Documents() DocumentStore
    Sections() SectionStore
    Nodes() NodeStore
    Edges() EdgeStore
    Jobs() JobStore
    Feedback() FeedbackStore
    Tx(ctx context.Context, fn func(Store) error) error
}
```

Search and vector interfaces should be separate:

```go
type SearchIndex interface {
    IndexSections(ctx context.Context, sections []Section) error
    Search(ctx context.Context, query string, opts SearchOptions) ([]SearchHit, error)
}

type VectorIndex interface {
    UpsertEmbeddings(ctx context.Context, vectors []Embedding) error
    Similar(ctx context.Context, vector []float32, opts VectorOptions) ([]VectorHit, error)
}
```

MVP implementations:

```text
SQLiteStore
SQLiteFTSIndex
NoopVectorIndex
```

Future implementations:

```text
PostgresStore
OpenSearchIndex
QdrantVectorIndex
Neo4jGraphStore
```

## 8. Data Model

### 8.1 Core Tables

```sql
sources (
  id,
  kind,
  name,
  dsn,
  config_json,
  product_hint,
  module_hint,
  sync_schedule,
  created_at,
  updated_at
)

documents (
  id,
  source_id,
  external_id,
  title,
  url,
  version,
  content_hash,
  metadata_json,
  acl_json,
  updated_at,
  indexed_at
)

sections (
  id,
  document_id,
  heading_path,
  title,
  content,
  content_hash,
  ordinal,
  metadata_json,
  created_at,
  updated_at
)

nodes (
  id,
  kind,
  name,
  canonical_name,
  metadata_json,
  confidence,
  created_at,
  updated_at
)

edges (
  id,
  src_id,
  dst_id,
  kind,
  confidence,
  provenance,
  evidence_section_id,
  source_revision,
  metadata_json,
  created_at,
  updated_at
)

aliases (
  id,
  node_id,
  alias,
  source
)

section_nodes (
  section_id,
  node_id,
  role,
  confidence
)

jobs (
  id,
  kind,
  status,
  payload_json,
  attempts,
  run_after,
  locked_until,
  last_error,
  created_at,
  updated_at
)

feedback_events (
  id,
  target_kind,
  target_id,
  feedback_kind,
  payload_json,
  actor,
  created_at
)
```

### 8.2 Full-Text Search

SQLite FTS5 virtual table:

```sql
fts_sections (
  title,
  heading_path,
  content,
  content='sections'
)
```

Search should combine:

- Exact title and alias match.
- FTS rank.
- Product/module filters.
- Recency.
- Authority signals.
- Manual curation signals.

## 9. Knowledge Model

MVP node kinds:

```text
Product
Module
Feature
Page
API
Document
DocSection
Team
Person
Release
```

MVP edge kinds:

```text
contains
describes
links_to
mentions
owned_by
exposes_api
related_to
changed_in
```

Every extracted fact should track:

```text
provenance: source_link | rule | llm | manual
confidence: 0.0 - 1.0
evidence_section_id
source_revision or content_hash
```

Manual edges should override automatic extraction where needed, without deleting source evidence.

## 10. Indexing Pipeline

### 10.1 Fetch

Connectors retrieve raw documents and source metadata:

- URL or source reference.
- External ID.
- Version or last updated time.
- Content hash.
- ACL metadata where available.

### 10.2 Normalize

Raw content is converted into a canonical document model:

```text
Raw Markdown / HTML / Confluence / OpenAPI
  -> Document
  -> Sections
  -> Blocks
```

The normalizer preserves:

- Heading hierarchy.
- Tables.
- Code blocks.
- Links.
- Metadata.
- Source URL.

### 10.3 Extract

Extraction runs in layers:

1. Deterministic extraction.
2. Rule and dictionary extraction.
3. Optional LLM-assisted candidate extraction.
4. Alias normalization and deduplication.
5. Edge creation with evidence.

Deterministic extraction should be preferred over LLM extraction.

### 10.4 Write Index

Index writes should be transactional per document version:

- Upsert document.
- Replace changed sections.
- Upsert nodes and aliases.
- Upsert edges with evidence.
- Update FTS rows.
- Record job status.

## 11. Query Pipeline

```text
User or agent query
  -> parse intent and filters
  -> permission scope
  -> FTS search
  -> exact entity and alias match
  -> graph expansion
  -> rerank
  -> context packing
  -> response with evidence
```

### 11.1 Search

Search returns:

- Sections.
- Documents.
- Nodes.
- Matched aliases.
- Source URLs.
- Confidence and rank signals.

### 11.2 Related Nodes

Related queries use edge traversal with filters:

- Relation type.
- Direction.
- Depth.
- Product or module scope.
- Permission scope.

SQLite recursive CTEs are sufficient for MVP.

### 11.3 Impact

Impact analysis is graph expansion with relation constraints. MVP impact should be conservative and evidence-based.

Example output:

```text
Target: Membership Benefits Page
Potentially affected:
  API: GET /member/benefits
  Feature: Benefit Calculation Rules
  Module: Membership Level
  Document: Membership Benefits PRD
Evidence: section links and edge provenance
```

### 11.4 Context Pack

Context pack is the main AI-facing output.

It should include:

- Task summary.
- Relevant nodes.
- Relevant sections.
- Related APIs and features.
- Owners.
- Source links.
- Evidence snippets.
- Staleness warnings.
- Confidence and provenance.

## 12. Permissions

Permission filtering must happen before retrieval output.

The permission model should support:

- Public/local mode.
- Token mode.
- Source-level ACL.
- Document-level ACL.
- Future SSO and group-based ACL.

All APIs and MCP tools should receive an actor context:

```text
actor_id
groups
token_scopes
source_permissions
```

Search, vector retrieval, graph expansion, and context packing must apply the same permission scope.

## 13. Web UI Structure

MVP pages:

```text
Dashboard
  Source count, document count, node count, edge count, last sync status

Sources
  Add, edit, test, sync, disable sources

Jobs
  Sync and indexing job status, retry, error details

Search
  Unified search and result filters

Node Detail
  Entity metadata, related docs, related nodes, evidence

Curation
  Mark stale, mark canonical, merge duplicates, remove bad edge

MCP/API
  Endpoint, token, tool list, examples
```

Graph visualization should be secondary. The primary UX is search, context, evidence, and correction.

## 14. Deployment

### 14.1 Local

```bash
docgraph serve
```

### 14.2 Server

```bash
docgraph serve --host 0.0.0.0 --port 8787 --data /var/lib/docgraph
```

### 14.3 systemd

Optional command:

```bash
docgraph install-service
```

### 14.4 Docker

Docker should be an optional wrapper:

```bash
docker run -p 8787:8787 -v ./data:/data docgraph:latest
```

## 15. Observability

MVP should expose:

- `/api/health`
- `/api/status`
- Structured logs.
- Sync job history.
- Connector errors.
- Index statistics.
- Query logs with duration and result count.
- MCP tool call logs.

Future:

- Prometheus metrics.
- Audit log export.
- OpenTelemetry tracing.

## 16. Security

Key requirements:

- Secrets should not be stored in plain text when avoidable.
- Source credentials should be scoped.
- MCP tools should be read-only in MVP.
- Manual curation should require write permission.
- All returned results must include only authorized content.
- Logs should avoid recording sensitive document content.

## 17. Extension Points

Planned extension points:

- Connector plugins.
- Storage backend by DSN scheme.
- Search backend by DSN scheme.
- Vector backend by DSN scheme.
- Extractor rules.
- LLM extraction provider.
- Auth provider.

Keep extension interfaces narrow and product-specific. Avoid building a general framework before the product workflows are proven.

## 18. MVP Implementation Plan

### Milestone 1: Core Shell

- Go project scaffold.
- CLI.
- Config loading.
- SQLite initialization.
- Embedded Web shell.
- Health/status endpoints.

### Milestone 2: Sources and Sync

- Source CRUD.
- Local directory connector.
- Git Markdown connector.
- Job table and worker.
- Manual sync.

### Milestone 3: Index and Search

- Markdown normalization.
- Sections.
- Nodes and edges.
- FTS5 search.
- Search API and UI.

### Milestone 4: Doc Graph

- Entity extraction rules.
- Aliases.
- Related nodes.
- Node detail.
- Evidence display.

### Milestone 5: MCP

- MCP stdio mode.
- MCP HTTP endpoint if needed.
- `doc_search`.
- `doc_context`.
- `doc_related`.

### Milestone 6: Confluence and OpenAPI

- Confluence connector.
- OpenAPI ingestion.
- Source mapping UI.

### Milestone 7: Curation

- Feedback events.
- Mark canonical.
- Mark stale.
- Remove incorrect edge.
- Merge duplicate node.

## 19. Key Design Decisions

| Decision | Choice | Reason |
|---|---|---|
| Primary artifact | Static binary | Lowest deployment friction |
| Runtime language | Go | Better fit for boring static delivery and internal platform work |
| Default storage | SQLite | Single-file, low-ops, good enough for MVP |
| Storage config | DSN | Easy future extension |
| PostgreSQL | Optional future backend | Useful for enterprise scale, not needed for MVP |
| Search | SQLite FTS5 | Avoids external dependency |
| Vector search | Optional later | Product value can be proven with FTS + graph first |
| MCP | Built in | AI agents are a first-class consumer |
| Docker | Optional | Runtime wrapper, not product requirement |
