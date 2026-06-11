# DocGraph MCP Setup Guide

This guide explains how to connect DocGraph to an MCP-compatible client.

## Transports

DocGraph supports two MCP transports: **stdio** (standalone process) and **SSE** (via the HTTP server).

### stdio Transport

```bash
docgraph mcp --data ./.docgraph
```

Use `--config docgraph.yaml` when you want DocGraph to load a specific
configuration file instead of the default config search path.

### SSE Transport

When DocGraph is running as an HTTP server (`docgraph serve`), MCP is also
available over Server-Sent Events (SSE) at the following endpoints:

| Method | Endpoint                           | Purpose                             |
|--------|------------------------------------|-------------------------------------|
| GET    | `/mcp/sse`                         | Open an SSE connection for a client |
| POST   | `/mcp/sse/messages?sessionId=xxx`  | Send JSON-RPC requests              |

**Flow:**

1. Client opens a GET connection to `/mcp/sse`.
2. Server responds with an `endpoint` event containing the session-scoped POST URL.
3. Client sends JSON-RPC requests via POST to that URL.
4. Responses are pushed back to the client as SSE `message` events.

Example with `curl`:

```bash
# Terminal 1: open SSE stream
curl -N http://127.0.0.1:8787/mcp/sse
# event: endpoint
# data: /mcp/sse/messages?sessionId=sse_xxxxxxxxxxxx

# Terminal 2: send a request
curl -X POST "http://127.0.0.1:8787/mcp/sse/messages?sessionId=sse_xxxxxxxxxxxx" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":"1","method":"tools/list"}'

# Terminal 1 receives:
# event: message
# data: {"jsonrpc":"2.0","id":"1","result":{"tools":[...]}}
```

Sessions timeout after 5 minutes of inactivity.

The HTTP API server (`docgraph serve`) and the MCP stdio server use the
same SQLite store when they point at the same `--data` directory or storage DSN.
Index sources first, then connect the MCP client:

```bash
docgraph source add --data ./.docgraph --name "Docs" --dsn /path/to/docs
docgraph source sync --data ./.docgraph --id src_xxx
docgraph mcp --data ./.docgraph
```

## Client Configuration

### stdio Mode

Most MCP clients can launch DocGraph as a local command. A typical client
entry looks like this:

```json
{
  "mcpServers": {
    "docgraph": {
      "command": "docgraph",
      "args": ["mcp", "--data", "/absolute/path/to/.docgraph"]
    }
  }
}
```

If you run from a checkout instead of an installed binary, point `command` to the
built binary:

```json
{
  "mcpServers": {
    "docgraph": {
      "command": "/absolute/path/to/docgraph",
      "args": ["mcp", "--config", "/absolute/path/to/docgraph.yaml"]
    }
  }
}
```

### SSE Mode

For SSE, run `docgraph serve` first, then configure the MCP client to use
`mcp-remote` as a proxy between the client's stdio transport and DocGraph's
SSE endpoint:

```json
{
  "mcpServers": {
    "docgraph": {
      "command": "npx",
      "transport": "sse",
      "args": [
        "-y",
        "mcp-remote",
        "http://127.0.0.1:8787/mcp/sse"
      ]
    }
  }
}
```

If your client supports URL-based SSE natively (without a proxy):

```json
{
  "mcpServers": {
    "docgraph": {
      "url": "http://127.0.0.1:8787/mcp/sse"
    }
  }
}
```

Set the auth token via headers when token auth is enabled in the server config:

```json
{
  "mcpServers": {
    "docgraph": {
      "command": "npx",
      "transport": "sse",
      "args": [
        "-y",
        "mcp-remote",
        "http://127.0.0.1:8787/mcp/sse",
        "--header",
        "Authorization: Bearer your-token-here"
      ]
    }
  }
}
```

## Tools

### `doc_search`

Search indexed documentation. Returns matching sections with their full text
content, title, heading path, and source URL. The returned content is the
original indexed text — you do not need to fetch the source URL separately.

Backward-compatible input:

```json
{
  "query": "member benefits",
  "limit": 8
}
```

Budgeted input:

```json
{
  "query": "authentication endpoint error response",
  "max_searches": 3,
  "max_results": 10,
  "max_sections_per_document": 2,
  "profile_detail": "compact",
  "max_chars_per_result": 1200
}
```

Fields:

- `query`: required search text.
- `max_searches`: internal retrieval attempts. Default `3`, hard limit `5`.
- `max_results`: final result count. Default `8`, hard limit `30`.
- `max_sections_per_document`: per-document result cap. Default `2`, hard limit `5`.
- `profile_detail`: `none`, `compact`, or `full`. Default `compact`.
- `max_chars_per_result`: text budget per result. Default `1000`, hard limit `4000`.

The response includes the final hits plus `searches_used` and `attempts` so the
caller can understand how retrieval was performed.

### `doc_context`

Build an evidence-backed context pack for a documentation task. Returns
matching sections with full text content, document titles, and heading paths.
This is the primary tool for getting comprehensive document content. The
returned content is the original indexed text — you do not need to fetch the
source URL separately.

```json
{
  "task": "Summarize member benefits",
  "max_sections": 3,
  "max_chars": 4000
}
```

### `doc_get_node`

Fetch a knowledge graph node by ID. Returns node metadata (kind, name,
canonical name, confidence). Does not return document content — use
`doc_search`, `doc_context`, or `doc_get_section` for content.

```json
{
  "id": "node_xxx"
}
```

### `doc_related`

Fetch related knowledge graph nodes. Returns connected nodes with their edges,
including edge kind (contains, describes, mentions, exposes_api) and provenance.

```json
{
  "id": "node_xxx",
  "direction": "out",
  "kind": "exposes_api",
  "limit": 10
}
```

`direction` can be `out`, `in`, or `both`.

### `doc_impact`

Traverse the graph to find multi-hop impact and dependency chains. Use this
for understanding what would be affected if a product, module, or API changes.
For single-hop relationships, use `doc_related` instead.

```json
{
  "id": "node_xxx",
  "direction": "out",
  "kind": "exposes_api",
  "max_depth": 2,
  "limit": 50
}
```

### `doc_get_section`

Retrieve the full text content of a specific documentation section by its
`section_id`. Returns the complete section content without truncation, along
with document title, heading path, and source URL.

Use this when a previous `doc_search` or `doc_context` result was truncated
and you need the full content of a specific section.

```json
{
  "id": "sect_xxx"
}
```

The returned content is the original indexed text — you do not need to fetch
the source URL separately.

## Authentication

**stdio transport:** reads local storage directly and does not use HTTP token
authentication. Protect access by controlling who can launch the local command
and who can read the configured data directory.

**SSE transport:** runs through the HTTP server and respects the server's token
auth configuration (`auth.mode: token` / `auth.token: your-token`). When auth is
enabled, include the `Authorization: Bearer <token>` header in the MCP client
configuration.

## Troubleshooting

- If tools return no results, verify the same `--data` directory is used for
  `source sync` and `mcp`.
- If the client cannot start the server, use an absolute `command` path or make
  sure `docgraph` is on the client's `PATH`.
- If graph tools return `graph storage is not configured`, update to a build
  that starts MCP through `docgraph mcp`; tests and current CLI wire graph
  storage for this command.
- If results look stale, run `docgraph source sync --id src_xxx` before
  reconnecting the MCP client.
