# DocGraph PRD

## 1. Product Summary

DocGraph is a local-first document knowledge graph for teams and local agents. It connects existing documentation and knowledge bases, automatically indexes them, extracts documents, entities, and relationships, and exposes the result through a Web UI, REST API, and MCP tools.

The product is not intended to replace existing documentation systems. It adds a searchable, linkable, AI-ready semantic layer over Markdown docs, Confluence pages, generated HTML sites, OpenAPI specs, and other local or enterprise knowledge sources.

## 2. Background

Company product knowledge is often spread across multiple systems:

- Product documentation sites generated from Markdown or MDX.
- Confluence spaces and page trees.
- API specs such as OpenAPI or Swagger.
- Release notes, changelogs, PRDs, FAQs, and troubleshooting documents.
- Product-specific documentation portals with separate access paths.

The current pain points are:

- Entry points are fragmented.
- Different teams use different documentation formats.
- Documents link poorly across product boundaries.
- Product concepts, APIs, modules, owners, and releases are not represented as structured knowledge.
- AI agents cannot reliably retrieve product context because normal RAG only sees text chunks, not product relationships.
- Manual knowledge graph maintenance would be too expensive and unlikely to be adopted.

## 3. Product Goals

DocGraph should:

- Provide a low-friction way to connect existing documentation sources.
- Automatically build a local document knowledge graph from existing content.
- Support light manual correction for high-value relationships.
- Let users search document-backed knowledge from a Web UI.
- Let internal systems query document-backed knowledge through APIs.
- Let local AI agents retrieve trusted context through MCP.
- Preserve source evidence for every extracted relationship and returned answer.
- Ship as a single static binary by default, with Docker as an optional runtime wrapper.

## 4. Non-Goals

The initial product will not:

- Replace Confluence, Git-based docs, or existing product portals.
- Require teams to migrate documentation into a new authoring system.
- Depend on PostgreSQL, Docker, Kubernetes, Neo4j, OpenSearch, or a vector database for MVP.
- Require users to manually build most relationships.
- Attempt to perfectly model all company product knowledge from day one.
- Provide a complex graph visualization as the primary user experience.

## 5. Target Users

### Product Managers

Need to find authoritative product documents, understand feature dependencies, and identify owners or related releases.

### Engineers

Need fast context on product flows, APIs, modules, behavior, and change impact before implementation.

### QA and Support

Need to understand affected features, known issues, test scope, and troubleshooting references.

### Documentation Owners

Need to find outdated, duplicate, conflicting, or unowned documents.

### AI Agents

Need structured, permission-aware context with source evidence for product reasoning and task execution.

## 6. Core Use Cases

### 6.1 Unified Product Search

Users search for a product concept, feature, API, or module and receive:

- Matching documents and sections.
- Related product entities.
- Source links.
- Last updated time.
- Owner information when available.
- Confidence and authority signals.

### 6.2 Product Context for AI

An AI agent calls MCP tool `doc_context` with a task such as:

> Change the membership benefits page behavior.

DocGraph returns a context pack containing:

- Relevant features and pages.
- Related API specs.
- Related business flows.
- Owner and team information.
- Source sections with evidence.
- Related releases or stale documents if detected.

### 6.3 Impact Analysis

Users or agents ask what may be affected by changing a feature, API, page, or module.

DocGraph returns related nodes through graph expansion:

- Upstream and downstream features.
- Related APIs.
- Related documents.
- Related modules.
- Known issues or releases.

### 6.4 Documentation Governance

Documentation owners identify:

- Duplicate documents.
- Potentially stale documents.
- Documents without owners.
- Documents that conflict with other sources.
- Candidate canonical documents.

### 6.5 Manual Correction

Users can correct the graph with minimal effort:

- Mark a document as canonical.
- Mark a document as stale.
- Merge duplicate entities.
- Remove an incorrect relationship.
- Add a high-value relationship.

Manual correction should refine the automatic graph, not become the main data entry workflow.

## 7. MVP Scope

The MVP should include:

- Single static binary named `docgraph`.
- Built-in Web UI.
- Built-in REST API.
- Built-in MCP server.
- Built-in background indexer.
- SQLite as the default storage engine.
- DSN-based storage configuration.
- Git Markdown connector.
- Confluence connector.
- Basic HTML site connector if straightforward.
- OpenAPI file ingestion.
- Source setup through Web UI and CLI.
- Automatic extraction of documents, sections, links, product/module hints, APIs, owners, and mentions.
- Knowledge graph with nodes, edges, aliases, and source evidence.
- Full-text search through SQLite FTS5.
- Basic graph expansion for related entities and impact analysis.
- MCP tools for product search and context retrieval.

## 8. MVP Functional Requirements

### 8.1 Binary and Runtime

The product must run with a single command:

```bash
docgraph serve
```

Default endpoints:

```text
Web UI:   http://localhost:8787
REST API: http://localhost:8787/api
MCP:      http://localhost:8787/mcp
Data dir: ./.docgraph or ~/.docgraph
```

The binary should also support stdio MCP mode:

```bash
docgraph mcp
```

### 8.2 Source Management

Users can create and manage sources from the Web UI:

- Git Markdown repository.
- Local directory.
- Confluence space or page tree.
- HTML documentation site.
- OpenAPI spec file or URL.

Each source supports:

- Name.
- Type.
- Connection parameters.
- Authentication where needed.
- Product and module mapping hints.
- Sync schedule.
- Manual sync trigger.
- Last sync status and error details.

### 8.3 Indexing

The indexer should:

- Fetch source content.
- Normalize documents into a common internal format.
- Split documents by structure, not only fixed token windows.
- Extract sections, headings, links, code blocks, tables, and metadata.
- Extract product entities and relationships.
- Store source evidence for nodes and edges.
- Re-index only changed documents where possible.

### 8.4 Knowledge Graph

MVP node kinds:

- Product
- Module
- Feature
- Page
- API
- Document
- DocSection
- Team
- Person
- Release

MVP edge kinds:

- contains
- describes
- links_to
- mentions
- owned_by
- exposes_api
- related_to
- changed_in

Every edge should include:

- Source node.
- Destination node.
- Kind.
- Confidence.
- Provenance: source_link, rule, llm, or manual.
- Evidence section when available.
- Source revision or content hash.

### 8.5 Query

The Web UI and API should support:

- Full-text search.
- Entity search.
- Node detail.
- Related nodes.
- Related documents.
- Basic impact analysis.
- Context pack generation for AI.

### 8.6 MCP

MVP MCP tools:

- `doc_search`
- `doc_context`
- `doc_get_node`
- `doc_related`
- `doc_impact`
- `product_owner`

Tool results should include:

- Structured data.
- Source links.
- Evidence sections.
- Timestamps.
- Confidence and provenance.

### 8.7 Permissions

MVP should support source-level and document-level permissions. Search and graph expansion must filter data before returning results.

For local or team deployments, a simple token-based model is acceptable. Enterprise identity integration can be added later.

### 8.8 Feedback and Curation

Users can:

- Mark relation as wrong.
- Mark document as stale.
- Mark document as canonical.
- Merge duplicate nodes.
- Add or remove relationship.

All manual edits should be auditable.

## 9. Configuration

The product should use DSN strings for storage and future extensibility.

Default:

```yaml
storage:
  dsn: sqlite://./.docgraph/docgraph.db
```

Future enterprise option:

```yaml
storage:
  dsn: postgres://user:pass@host:5432/docgraph?sslmode=require
```

Search and vector backends can follow the same pattern later:

```yaml
search:
  dsn: sqlite://./.docgraph/docgraph.db

vector:
  dsn: none://
```

## 10. Delivery Artifacts

Primary artifacts:

- `docgraph-linux-amd64`
- `docgraph-linux-arm64`
- `docgraph-darwin-amd64`
- `docgraph-darwin-arm64`
- `docgraph-windows-amd64.exe`

Optional artifacts:

- Docker image.
- Example config files.
- systemd service template.
- MCP setup examples.
- API documentation.
- Connector setup documentation.

## 11. Success Metrics

MVP success should be measured by:

- Number of connected documentation sources.
- Number of indexed documents and sections.
- Search success rate from user feedback.
- Reduction in time to find authoritative product docs.
- AI context retrieval usefulness.
- Number of manual corrections per indexed document, ideally low.
- Ratio of automatically created relationships to manual relationships.
- Number of stale or duplicate documents discovered.

## 12. Risks

### Knowledge Quality

Automatic extraction may create wrong or weak relationships. Mitigation: store provenance, confidence, and evidence; keep manual correction lightweight.

### Permissions

AI-facing tools can accidentally expose restricted information if filtering is not applied before retrieval. Mitigation: enforce ACL at query time before search results, graph expansion, and context packing.

### Adoption

Users will not maintain a graph manually. Mitigation: connect existing sources and make manual edits optional and contextual.

### Scope

Graph visualization and advanced ontology design can distract from the useful workflows. Mitigation: prioritize search, context, evidence, and impact analysis.

### Storage Scale

SQLite may eventually hit concurrency or scale limits. Mitigation: design DSN-based storage and domain-level interfaces so PostgreSQL can be added later.

## 13. Roadmap

### Phase 1: Local-First MVP

- Single binary.
- SQLite.
- Git Markdown and Confluence connectors.
- Web UI for source setup and search.
- MCP search/context tools.
- Basic graph and evidence model.

### Phase 2: Team Deployment

- Token-based user access.
- Better sync scheduling.
- More curation workflows.
- OpenAPI ingestion.
- Stale and duplicate document detection.

### Phase 3: Enterprise Scale

- PostgreSQL backend.
- External search backend such as OpenSearch.
- External vector backend such as Qdrant.
- SSO and group-based ACL.
- Audit logs.
- Advanced impact analysis.

### Phase 4: Advanced Product Intelligence

- Release diff.
- Cross-product dependency analysis.
- Conflict detection.
- Test impact suggestions.
- Product graph quality dashboard.
