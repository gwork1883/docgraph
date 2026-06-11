# DocGraph Acceptance Criteria

## 1. Purpose

This document defines the acceptance criteria for the DocGraph MVP.

The goal of acceptance is to verify that DocGraph can be delivered as a low-friction, single-binary local document knowledge graph that can connect existing documentation sources, build a local knowledge graph, provide trusted search and context, and expose the result through Web UI, REST API, and MCP.

## 2. Acceptance Scope

MVP acceptance covers:

- Static binary delivery.
- Basic configuration.
- SQLite default storage.
- Web UI.
- REST API.
- MCP server.
- Source connection.
- Indexing and sync.
- Knowledge graph extraction.
- Search and context retrieval.
- Basic manual curation.
- Basic permissions.
- Observability and operations.

MVP acceptance does not require:

- PostgreSQL backend.
- External vector database.
- OpenSearch.
- Neo4j.
- Kubernetes deployment.
- Full enterprise SSO.
- Advanced graph visualization.
- Perfect automatic relationship extraction.

## 3. Delivery Acceptance

### 3.1 Binary Artifacts

The release must provide runnable artifacts for at least:

- Linux amd64.
- Linux arm64.
- macOS arm64.
- macOS amd64.

Windows amd64 is preferred but can be marked beta in the first release.

Acceptance criteria:

- Each binary starts without external runtime dependencies.
- The binary can show version information.
- The binary can initialize a data directory.
- The binary can start the server.

Example:

```bash
docgraph version
docgraph init
docgraph serve
```

### 3.2 Optional Docker Artifact

Docker can be provided as an optional runtime wrapper.

Acceptance criteria:

- DocGraph does not require Docker to run.
- Docker image uses the same binary and behavior as the standalone artifact.
- Data can be persisted through a mounted volume.

## 4. Startup and Configuration Acceptance

### 4.1 Default Startup

Command:

```bash
docgraph serve
```

Acceptance criteria:

- Starts Web UI on `http://localhost:8787`.
- Starts REST API under `/api`.
- Starts MCP endpoint if HTTP MCP mode is enabled.
- Creates default data directory if missing.
- Creates default SQLite database if missing.
- Logs startup configuration without printing secrets.

### 4.2 Config File

DocGraph must support a YAML config file.

Example:

```yaml
server:
  host: 127.0.0.1
  port: 8787
  data_dir: ./.docgraph

storage:
  dsn: sqlite://./.docgraph/docgraph.db
```

Acceptance criteria:

- Server host and port are configurable.
- Data directory is configurable.
- Storage DSN is configurable.
- Invalid DSN returns a clear startup error.
- Unsupported DSN schemes return a clear error.

### 4.3 SQLite Default

Acceptance criteria:

- `sqlite://` is supported.
- DocGraph can run with only SQLite.
- No PostgreSQL, Redis, OpenSearch, Neo4j, or vector database is required for MVP.

### 4.4 API Token Auth Configuration

Example:

```yaml
auth:
  mode: token
  token: "replace-with-a-random-shared-secret"
```

Acceptance criteria:

- `auth.mode` supports `none` and `token`.
- `auth.mode: none` starts without requiring an API token.
- `auth.mode: token` requires `auth.token`.
- Missing `auth.token` in token mode returns a clear startup error.
- Startup logs and config errors do not print the token value.

## 5. Web UI Acceptance

The Web UI must be embedded in the binary.

Acceptance criteria:

- Opening the server URL loads the Web UI.
- UI can show system status.
- UI can list configured sources.
- UI can add and edit supported sources.
- UI can trigger sync.
- UI can show sync job status and errors.
- UI can search indexed content.
- UI can open a node or document detail page.
- UI can show evidence links for returned results.
- UI can submit basic feedback or curation actions.

Minimum pages:

- Dashboard.
- Sources.
- Jobs or Sync Status.
- Search.
- Node Detail or Document Detail.
- MCP/API setup.

## 6. Source Connector Acceptance

### 6.1 Local Directory Connector

Acceptance criteria:

- User can connect a local directory.
- Markdown files are discovered.
- Files ignored by configured ignore rules are skipped.
- Changed files can be re-indexed.
- Deleted files are reflected after sync.

### 6.2 Git Markdown Connector

Acceptance criteria:

- User can connect a Git repository URL or local Git checkout.
- User can configure branch and path filters.
- Markdown files are indexed.
- Source URL or file path is preserved.
- Incremental sync detects changed content.

### 6.3 Confluence Connector

Acceptance criteria:

- User can configure Confluence base URL and authentication.
- User can restrict sync by space, page tree, or query.
- Pages are fetched with title, body, URL, version, and updated time.
- Page hierarchy is preserved when available.
- Sync errors are visible in the Web UI.

### 6.4 OpenAPI Ingestion

Acceptance criteria:

- User can ingest an OpenAPI file or URL.
- API paths and methods become API nodes.
- API nodes are linked to source document sections.
- Basic search can find an API by path or operation name.

## 7. Indexing Acceptance

### 7.1 Document Normalization

Acceptance criteria:

- Markdown documents are split by heading hierarchy.
- Confluence pages are converted into sections.
- HTML documents preserve title, headings, links, and text.
- OpenAPI specs extract APIs and descriptions.
- Source URL, title, updated time, and content hash are stored.

### 7.2 Incremental Sync

Acceptance criteria:

- Unchanged documents are not fully reprocessed unnecessarily.
- Changed documents update their sections and derived graph data.
- Deleted documents are marked deleted or removed from active search.
- Sync job history records success, failure, duration, and error message.

### 7.3 Evidence Preservation

Acceptance criteria:

- Extracted nodes and edges can be traced to source document sections when applicable.
- Search results include source document links.
- Context packs include evidence references.
- Manual relationships are marked as manual.

## 8. Knowledge Graph Acceptance

### 8.1 Node Types

MVP must support these node kinds:

- Product.
- Module.
- Feature.
- Page.
- API.
- Document.
- DocSection.
- Team.
- Person.
- Release.

Acceptance criteria:

- Documents and sections are represented as graph nodes.
- APIs from OpenAPI are represented as API nodes.
- Product and module hints can be represented as nodes.
- Nodes have stable IDs.
- Nodes support aliases.

### 8.2 Edge Types

MVP must support these edge kinds:

- contains.
- describes.
- links_to.
- mentions.
- owned_by.
- exposes_api.
- related_to.
- changed_in.

Acceptance criteria:

- Document hierarchy creates `contains` edges.
- Hyperlinks create `links_to` edges when target is known.
- API references create `mentions` or `exposes_api` edges.
- Manual edits can add or remove supported edge types.
- Each edge stores confidence and provenance.

### 8.3 Basic Graph Query

Acceptance criteria:

- User can view related nodes for a selected node.
- User can limit related query by depth.
- User can filter by relation type.
- Result includes source evidence where available.

## 9. Search Acceptance

### 9.1 Full-Text Search

Acceptance criteria:

- Search works over document titles, section headings, and section content.
- Search can find exact API paths.
- Search can find aliases.
- Search result shows title, snippet, source, updated time, and entity type.
- Search result respects permission filtering.

### 9.2 Context Retrieval

Acceptance criteria:

- API and MCP can request a context pack for a task.
- Context pack includes relevant sections and related graph nodes.
- Context pack includes source links and evidence.
- Context pack has a configurable size limit.
- Context pack avoids returning duplicate sections where possible.

### 9.3 Impact Query

Acceptance criteria:

- User or API can ask for impact of a node.
- Result returns related features, APIs, modules, and documents.
- Result explains relationship type and evidence.
- MVP impact is conservative and does not claim certainty without evidence.

## 10. REST API Acceptance

Minimum endpoints:

```text
GET  /api/health
GET  /api/status
GET  /api/sources
POST /api/sources
POST /api/sources/{id}/sync
POST /api/search
POST /api/context
GET  /api/nodes/{id}
GET  /api/nodes/{id}/related
POST /api/impact
POST /api/feedback
```

Acceptance criteria:

- API returns JSON.
- API errors use consistent error format.
- API validates input.
- API enforces permission filtering.
- When `auth.mode` is `token`, every API route except `GET /api/health` rejects requests without a valid token.
- `GET /api/health` remains accessible without a token.
- Valid tokens are accepted through `X-DocGraph-Token: <token>`.
- Valid tokens are accepted through `Authorization: Bearer <token>`.
- Missing or invalid tokens return `401 Unauthorized`.
- API includes request IDs in logs.

## 11. MCP Acceptance

MVP tools:

- `doc_search`
- `doc_context`
- `doc_get_node`
- `doc_related`
- `doc_impact`
- `product_owner`

Acceptance criteria:

- MCP stdio mode starts with `docgraph mcp`.
- MCP tools are discoverable by a compatible client.
- Tool input schemas are explicit.
- Tool responses include structured results.
- Tool responses include evidence and source links.
- MCP tools are read-only in MVP.
- MCP tools enforce the same permission model as REST API.

## 12. Manual Curation Acceptance

Acceptance criteria:

- User can mark a document as canonical.
- User can mark a document as stale.
- User can mark a relationship as wrong.
- User can add a manual relationship.
- User can merge duplicate nodes.
- Curation events are stored in an audit-friendly format.
- Manual curation is reflected in search and context results.

## 13. Permission Acceptance

MVP permission model may be simple but must be consistent.

Acceptance criteria:

- Source-level visibility can be configured.
- Document-level ACL metadata can be stored.
- Search filters unauthorized content before returning results.
- Graph expansion does not traverse into unauthorized documents or sections.
- MCP does not expose content unavailable through API for the same actor.
- Logs do not print restricted document body by default.

## 14. Operational Acceptance

### 14.1 Health and Status

Acceptance criteria:

- `/api/health` returns healthy when server and storage are available.
- `/api/status` returns database stats, source counts, document counts, node counts, edge counts, and recent job status.
- `docgraph status` returns similar information from CLI.

### 14.2 Logs

Acceptance criteria:

- Logs include startup, source sync, indexing, API errors, and MCP tool calls.
- Logs include request IDs or job IDs.
- Logs avoid secrets and full restricted document bodies.

### 14.3 Backup and Restore

Acceptance criteria:

- SQLite database location is documented.
- DocGraph can be stopped and restarted without data loss.
- Copying the data directory is sufficient for basic backup in MVP.

## 15. Performance Acceptance

Initial target dataset:

- 10,000 documents.
- 100,000 sections.
- 100,000 nodes.
- 500,000 edges.

Acceptance criteria on a normal developer workstation or small VM:

- Server starts in under 5 seconds with existing index.
- Search p95 under 1 second for the target dataset.
- Node related query p95 under 1 second for depth 1-2.
- Context pack generation p95 under 3 seconds.
- Incremental sync of 100 changed Markdown files completes in under 2 minutes.

These numbers are MVP targets, not hard enterprise-scale guarantees.

## 16. Reliability Acceptance

Acceptance criteria:

- Failed sync jobs can be retried.
- One bad document does not fail the entire source sync.
- Indexing errors are visible in UI and API.
- Server restart does not lose queued jobs.
- Database migration runs automatically and safely on startup.
- Version downgrade behavior is documented if unsupported.

## 17. Security Acceptance

Acceptance criteria:

- Source credentials are not shown after save.
- Secrets are redacted in logs.
- API token values are not printed in startup logs, request logs, or validation errors.
- API tokens can be rotated by updating configuration and restarting the process.
- MCP tools are read-only by default.
- Write operations require explicit permissions.
- CORS behavior is configurable.
- Server can bind to localhost by default and `0.0.0.0` when configured.

## 18. Documentation Acceptance

Release documentation must include:

- Installation guide.
- Quickstart.
- Configuration reference.
- Source connector setup.
- MCP setup guide.
- REST API reference.
- Backup and restore notes.
- Troubleshooting guide.
- Known limitations.

## 19. Demo Acceptance Scenario

A successful MVP demo should complete this flow:

1. Download `docgraph` binary.
2. Run `docgraph serve`.
3. Open Web UI.
4. Add a local Markdown or Git Markdown source.
5. Trigger sync.
6. Confirm documents, sections, nodes, and edges are created.
7. Search for a feature or API.
8. Open node detail and view related documents.
9. Run impact query on a node.
10. Start MCP mode or connect an MCP client.
11. Call `doc_context` for a product task.
12. Confirm returned context includes source evidence.
13. Mark one document as canonical or stale.
14. Confirm search/context reflects the curation.

## 20. Acceptance Exit Criteria

The MVP is accepted when:

- All critical acceptance criteria pass.
- Demo scenario passes end to end.
- Known limitations are documented.
- No known permission leak exists.
- No required runtime dependency exists besides the binary and writable data directory for default mode.
- DocGraph can answer useful product knowledge questions from indexed sources with source evidence.
