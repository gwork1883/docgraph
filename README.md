# DocGraph

English | [中文](README.zh-CN.md)

DocGraph is a local-first document knowledge graph for teams, internal knowledge bases, and local AI agents. It indexes local documents, Git repository docs, HTML sites, Confluence pages, OpenAPI specs, SFTP directories, and web documentation into local SQLite, then exposes the result through an embedded Web UI, REST API, and MCP tools.

The goal is to keep document search, context assembly, and impact analysis close to your machine or intranet, without syncing internal knowledge into an external SaaS service.

## Screenshots

Dashboard shows source, document, section, node, edge, and sync-job counts for the local knowledge base.

![DocGraph dashboard](docs/assets/screenshots/docgraph-dashboard.png)

Connectors let you add, edit, sync, and inspect documentation sources across 8 connector types.

![DocGraph connectors](docs/assets/screenshots/docgraph-connectors.png)

Search provides hybrid local retrieval across document sections, retrieval profiles, and vector embeddings.

![DocGraph search](docs/assets/screenshots/docgraph-search.png)

Sync Tasks shows job history with status tracking, plus per-source cron schedules for automatic syncs.

![DocGraph sync tasks](docs/assets/screenshots/docgraph-sync-tasks.png)

Credentials manage Confluence cookie credentials for session-based access.

![DocGraph credentials](docs/assets/screenshots/docgraph-credentials.png)

Governance reviews stale documents, knowledge relation proposals, and feedback markers.

![DocGraph governance](docs/assets/screenshots/docgraph-governance.png)

Nodes browse and search the knowledge graph — product, module, document, section, and API nodes with their relationships.

![DocGraph nodes](docs/assets/screenshots/docgraph-nodes.png)

## Features

- Single Go binary with an embedded Web UI.
- Local SQLite + FTS5 storage, no external database required. Optional pgvector for hybrid vector search.
- Connectors for `local`, `git`, `static`, `html`, `sftp`, `confluence`, `openapi`, and `webdocs` sources.
- MCP tools: `doc_search`, `doc_get_node`, `doc_get_section`, `doc_related`, and `doc_impact`; legacy `doc_context` calls remain compatible but are no longer advertised.
- Document-backed knowledge graph with nodes, edges, provenance, sync history, and feedback markers.
- Hybrid vector search with Reciprocal Rank Fusion (RRF): combines FTS5 text retrieval with pgvector cosine similarity, intent-aware routing for entity/conceptual/general queries.
- Embedding pipeline with configurable OpenAI-compatible provider, chunking strategies (auto/sentence/fixed), and generator version tracking.
- GSE-based search tokenizer for accurate CJK segmentation alongside English token analysis.
- Technical term extraction from headings, tables, and code-like content for better recall on API names and config keys.
- Explicit cross-reference resolution: author-written references surfaced as `suggested_reads.explicit_references`.
- Sync scheduling with cron expressions per source; job management API for listing, inspecting, and canceling sync jobs.
- Confluence cookie credential management for session-based Confluence access.
- Chinese-aware retrieval profile generation for mixed Chinese/English internal docs.
- Optional token authentication for Web/API/MCP endpoints.
- MCP over stdio and Streamable HTTP; legacy SSE endpoint retained for older clients.

## Search Quality

DocGraph is built for documentation search rather than generic text lookup:

- Multi-stage local retrieval combines FTS5 token search, trigram search, profile lookup, and substring fallback so short Chinese phrases, English terms, API names, and symbols can all contribute to recall.
- Optional hybrid vector search adds a pgvector cosine-similarity lane fused with the text lane via Reciprocal Rank Fusion (RRF). Intent routing classifies queries as entity, conceptual, or general and adjusts text/vector weights accordingly.
- Search is section-first: results point to the exact document section, heading path, snippet, source URL, and matched evidence instead of only returning whole files.
- Generated retrieval profiles add deterministic tags, keyphrases, aliases, API references, technical terms, and section distribution signals while keeping human-maintained document descriptions separate from sync-generated metadata.
- Author-written explicit cross-references (e.g., "See also: [Config Guide](../config.md)") are resolved and surfaced as suggested reads alongside knowledge-graph relations.
- Ranking uses local signals such as canonical document status, title and heading matches, term coverage, profile matches, exact hits, vector similarity, and approved knowledge relations.
- MCP tools return bounded search summaries first and let agents fetch full sections only when needed, keeping local agent context focused and auditable.

## How It Works

DocGraph turns existing documentation into a local, queryable knowledge layer:

1. Connect sources: add local directories, Git repositories, static HTML, Confluence pages, OpenAPI files, SFTP folders, or web documentation centers.
2. Normalize documents: each connector converts source content into a common document and section model.
3. Index locally: documents, sections, retrieval profiles, nodes, edges, aliases, and sync jobs are stored in SQLite with FTS5 search. Optionally, section chunks are embedded into pgvector for hybrid vector retrieval.
4. Build relationships: DocGraph derives product/module/document/section/API nodes and connects them with evidence-backed edges. Explicit cross-references and technical terms are extracted for richer retrieval signals.
5. Query with evidence: users and agents search sections, assemble task context, inspect related nodes, and run impact analysis through the Web UI, REST API, or MCP (stdio and Streamable HTTP).

All data stays in the configured local data directory unless you explicitly expose the server or move the database.

## Quick Start

```bash
go build -buildvcs=false -o bin/docgraph ./cmd/docgraph
./bin/docgraph serve
```

The first `serve` run creates `docgraph.yaml`, generates a local admin token, migrates the SQLite database, and starts the server. Open `http://127.0.0.1:8787`, enter the token from `docgraph.yaml`, then add and sync documentation sources from the Web UI. `docgraph init` remains available when you want to pre-create or check the config and database without starting the server.

To enable hybrid vector search, add the `vector_search` section to `docgraph.yaml`:

```yaml
vector_search:
  enabled: true
  search_weight: 0.4
  embedding:
    provider: openai
    model: text-embedding-3-small
    api_url: https://api.openai.com/v1/embeddings
    api_key: sk-...
    dimensions: 1536
  vector_db:
    dsn: pgvector://user:pass@localhost:5432/docgraph?sslmode=disable
```

See the configuration reference in `docgraph.yaml` for all options including intent route weights, chunk strategies, and embedding pipeline tuning.

When serving DocGraph behind a reverse proxy path prefix, set `server.web_prefix` in `docgraph.yaml`. For example, `web_prefix: docgraph` serves the Web UI, REST API, and HTTP MCP endpoints under `/docgraph/`; an empty value keeps the default root routes. See `scripts/nginx-docgraph.conf` and `scripts/nginx-docgraph-location.conf` for nginx examples that preserve the prefix when proxying to DocGraph.

You can also add a local documentation source from the CLI:

```bash
./bin/docgraph source add --data ./.docgraph --name "Docs" --dsn /path/to/docs
./bin/docgraph source sync --data ./.docgraph --id src_xxx
./bin/docgraph search --data ./.docgraph "authentication failure"
./bin/docgraph context --data ./.docgraph --max-sections 5 "Debug authentication failure"
```

## Development

```bash
make test
make build
make build-release
make run
```

`make test` runs `go test -buildvcs=false ./...`. The Makefile keeps Go build caches inside `.gocache/` and `.gomodcache/`, both ignored by Git.

## Release Builds

The repository includes a GitHub Actions workflow at `.github/workflows/release.yml`.

- Manual `release` workflow runs build and uploads artifacts.
- Pushing a `v*` tag runs tests, builds binaries, and creates a GitHub Release.
- Build artifacts cover `linux/windows/darwin` on `amd64/arm64`.
- Release artifacts include platform archives and SHA-256 checksum files.
- To rebuild an existing release manually, run the `release` workflow and set `release_tag` to the tag name, such as `v0.1.0`.

Example:

```bash
git tag v0.1.0
git push origin v0.1.0
```

## MCP

DocGraph supports two MCP transports:

| Transport | Endpoint | Use case |
|-----------|----------|----------|
| **stdio** | `docgraph mcp` | Local agents, IDE extensions (Claude Code, VS Code, JetBrains) |
| **Streamable HTTP** | `/mcp` on running server | Remote agents, HTTP-based integrations |
| **Legacy SSE** | `/mcp/sse` | Older clients; deprecated but retained for compatibility |

stdio mode:

```bash
./bin/docgraph mcp --data ./.docgraph
```

Streamable HTTP is available when `docgraph serve` is running. See [docs/mcp-setup.md](docs/mcp-setup.md) for full setup instructions.

## Runtime Dependencies

DocGraph itself is a single Go binary. Some connectors require local tools at runtime:

- Git sources require a local `git` executable.
- SPA web crawling requires Chrome, Chromium, Edge, or `ROD_BROWSER_BIN` pointing to a browser executable.

## Documentation

- [Product requirements](docs/docgraph-prd.md)
- [Architecture](docs/docgraph-architecture.md)
- [MCP setup](docs/mcp-setup.md)
- [Acceptance checklist](docs/docgraph-acceptance.md)

## Security Notes

- Do not commit real tokens, cookies, private keys, internal domains, local databases, or indexed document data.
- For team or shared deployments, use `auth.mode: token`.
- `.docgraph/`, `.docgraph-test/`, `.gocache/`, `.gomodcache/`, and `bin/` are ignored.

## License

DocGraph is licensed under the [Server Side Public License v1.0](LICENSE).

Internal company use, internal deployment, and internal modification are allowed. If you make DocGraph or a modified version available to third parties as a hosted service, you must comply with SSPL section 13 by making the corresponding Service Source Code available, or obtain a separate commercial license.
