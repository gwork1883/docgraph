# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

DocGraph is a local-first document knowledge graph — a single Go binary that ingests documentation from 8 source types (local Markdown, Git repos, HTML, static files, SFTP, Confluence, OpenAPI specs, web crawls), indexes them into SQLite with FTS5, builds a knowledge graph of nodes/edges, and exposes results via an embedded Web UI, REST API, and MCP server for AI agents. The Go module is `github.com/docgraph/docgraph`, Go 1.25.0.

## Build & Test Commands

```bash
make build          # Dev build → bin/docgraph-dev
make build-release  # Release build → bin/docgraph
make test           # Run all tests (go test -buildvcs=false ./...)
make fmt            # Format code (gofmt -w cmd internal)
make run            # Build + serve on 127.0.0.1:8787
make dev            # Same as make run
make tidy           # go mod tidy
make clean          # Remove bin/, caches, .docgraph/

# Run a single test:
go test -buildvcs=false -run TestName ./internal/storage/sqlite/
# Run tests for one package:
go test -buildvcs=false ./internal/sync/
```

No linter config exists. No CI/CD pipeline. The Makefile uses local `.gocache/` and `.gomodcache/` directories instead of system Go caches.

## Architecture

### Package Dependency Flow

```
cmd/docgraph → app → {config, mcp, server, storage}
server → {config, ids, query, storage, sync, web}
sync → {ids, ingest/*, profile, storage}
query → storage
mcp → {query, storage}
storage → {domain, sqlite, sqlschema}
domain — leaf package, no internal imports (pure types)
ids — leaf package, no internal imports
config — leaf package, no internal imports
```

### Key Abstractions

- **`internal/domain/types.go`** — All domain types (Source, DocumentInput, SectionInput, Node, Edge, SyncJob, SearchHit, ImpactResult, etc.). Pure structs with no methods or internal imports. Every other package references these types via `storage` type aliases.

- **`internal/storage/storage.go`** — `Store` interface (~45 methods) with domain-specific operations like `ReplaceDocument`, `UpsertNode`, `UpsertEdge`, `SearchSectionsWithOptions`, `Impact`. NOT generic CRUD. Callers import domain types through `storage` package aliases.

- **`internal/storage/sqlite/sqlite.go`** — Largest file (2084 lines), sole backend. Uses `modernc.org/sqlite` (pure Go, no CGO). Single connection pool, WAL mode, FTS5, raw SQL strings, `INSERT ON CONFLICT DO UPDATE` for upserts. Search cascade: phrase FTS → token FTS → Chinese ngram LIKE → profile lookup, deduplicated via `searchCollector`. Impact analysis uses BFS in Go (not recursive CTE).

- **`internal/ids/ids.go`** — Two ID schemes: `Random(prefix, n)` for one-shot entities (sources, jobs, feedback), `Stable(prefix, parts...)` for content-addressed entities (docs, sections, nodes, edges). Stable IDs are SHA-256 based, deterministic from inputs — enabling idempotent syncs.

- **`internal/sync/sync.go`** — Central sync orchestrator (866 lines). Per-kind dispatch to ingest connectors, then common document processing: stable ID generation → `ReplaceDocument` → profile generation → stale doc cleanup → graph construction → HTML link extraction. Graph construction creates Product/Module/Document/Section/API nodes and `contains`/`describes`/`mentions`/`exposes_api` edges with provenance tracking.

- **`internal/ingest/*`** — 8 source connectors. All converge to `htmldocs.Document` type. `staticdocs` delegates to `localdocs` (md) or `htmldocs` (html). `sftpdocs` mirrors remote then uses `staticdocs`. `webdocs` crawls then uses `htmldocs.Parse`. `confluence` fetches API pages then uses `htmldocs.Parse`. `openapi` parses spec into section-per-endpoint.

- **`internal/profile/profile.go`** — TF-IDF-like retrieval profile generation with heading/title boosts. Supports Chinese text via Han character n-gram extraction (1-4 char sliding windows).

- **`internal/server/server.go`** — Go 1.22+ `http.ServeMux` with method-based routing. Token auth via `X-DocGraph-Token`/`X-ProductGraph-Token`/`Authorization: Bearer` with `subtle.ConstantTimeCompare`. Embedded Web UI via `go:embed`.

- **`internal/mcp/server.go`** — JSON-RPC 2.0 over stdio. 6 tools: `doc_search`, `doc_context`, `doc_get_node`, `doc_get_section`, `doc_related`, `doc_impact`. Backward-compatible aliases (`product_search` → `doc_search`). Search tools accept budgeted parameters (max_searches, max_results, max_sections_per_document, profile_detail, max_chars_per_result). `doc_get_section` retrieves full section content by ID without truncation.

### Configuration

YAML-based with hand-written line parser (not yaml library). Sections: `server`, `storage`, `search`, `vector`, `auth`. Keys use snake_case. Defaults: host `127.0.0.1`, port `8787`, data dir `.docgraph`, storage `sqlite://.docgraph/docgraph.db`, vector `none://`, auth `none`. CLI `--data` flag builds DSN as `sqlite://<dataDir>/docgraph.db`.

### Database Schema

10 tables + 1 FTS5 virtual table in `internal/storage/sqlschema/schema.go`. Key tables: sources, documents (unique per source+external_id), sections (cascade delete), document_profiles, nodes, edges (with provenance field), aliases, section_nodes, jobs (with locking), feedback_events. Foreign keys enabled, WAL journal, all timestamps as SQLite `current_timestamp` text.

### Feedback/Curation System

Edges with provenance `relationship_wrong` are filtered from graph traversal. Documents marked `document_stale` are excluded from search. `document_canonical` documents get search boost. Feedback events target nodes, edges, or documents.

## Testing Patterns

- No mocking frameworks — tests use real SQLite (in-memory), `httptest.NewServer`, `io.Pipe` for MCP, or function variable substitution (`SetHTTPClientFactoryForTest`)
- Ingest tests use sample input files and assert parsed output
- Storage tests create temp databases with in-memory SQLite
- HTTP tests verify endpoint behavior end-to-end
- MCP tests exchange raw JSON-RPC messages

## Key Design Decisions

- Single static binary, no external runtime deps, `go:embed` for web assets
- Pure Go SQLite (no CGO), FTS5 for search, no vector DB in MVP
- Content-addressed stable IDs for idempotent sync
- Edge provenance tracking (source_hint, sync, rule, html_link, manual)
- Search strategy cascade with deduplication
- No external HTTP router (stdlib ServeMux), no CLI framework (stdlib flag), no logging framework (log/slog), no ORM