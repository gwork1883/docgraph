package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/docgraph/docgraph/internal/query"
	"github.com/docgraph/docgraph/internal/storage"
)

// Handler processes individual MCP JSON-RPC messages, independent of transport.
type Handler struct {
	query *query.Service
	store storage.Store
}

func NewHandler(queryService *query.Service, store storage.Store) *Handler {
	return &Handler{query: queryService, store: store}
}

// Handle dispatches a single JSON-RPC request and returns the response.
func (h *Handler) Handle(ctx context.Context, req Request) Response {
	return handle(ctx, h, req)
}

// Server is the stdio transport for MCP.
type Server struct {
	handler *Handler
	in      io.Reader
	out     io.Writer
}

func NewServer(queryService *query.Service, in io.Reader, out io.Writer) *Server {
	return &Server{handler: NewHandler(queryService, nil), in: in, out: out}
}

func NewServerWithStore(queryService *query.Service, store storage.Store, in io.Reader, out io.Writer) *Server {
	return &Server{handler: NewHandler(queryService, store), in: in, out: out}
}

type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *ResponseError  `json:"error,omitempty"`
}

type ResponseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *Server) Run(ctx context.Context) error {
	scanner := bufio.NewScanner(s.in)
	writer := bufio.NewWriter(s.out)
	defer writer.Flush()

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			if err := writeJSONResponse(writer, Response{
				JSONRPC: "2.0",
				Error:   &ResponseError{Code: -32700, Message: "parse error"},
			}); err != nil {
				return err
			}
			continue
		}
		if len(req.ID) == 0 {
			continue
		}

		resp := s.handler.Handle(ctx, req)
		if err := writeJSONResponse(writer, resp); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func handle(ctx context.Context, h *Handler, req Request) Response {
	resp := Response{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    "docgraph",
				"version": "dev",
			},
		}
	case "tools/list":
		resp.Result = map[string]any{"tools": tools()}
	case "tools/call":
		result, err := callTool(ctx, h, req.Params)
		if err != nil {
			resp.Error = &ResponseError{Code: -32602, Message: err.Error()}
			return resp
		}
		resp.Result = result
	default:
		resp.Error = &ResponseError{Code: -32601, Message: "method not found"}
	}
	return resp
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func callTool(ctx context.Context, h *Handler, params json.RawMessage) (any, error) {
	var call toolCallParams
	if err := json.Unmarshal(params, &call); err != nil {
		return nil, fmt.Errorf("invalid tool call params: %w", err)
	}
	call.Name = canonicalToolName(call.Name)

	switch call.Name {
	case "doc_search":
		var args struct {
			Query                  string `json:"query"`
			Limit                  int    `json:"limit"`
			MaxSearches            int    `json:"max_searches"`
			MaxResults             int    `json:"max_results"`
			MaxSectionsPerDocument int    `json:"max_sections_per_document"`
			ProfileDetail          string `json:"profile_detail"`
			MaxCharsPerResult      int    `json:"max_chars_per_result"`
			Detail                 string `json:"detail"`
		}
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return nil, fmt.Errorf("invalid doc_search arguments: %w", err)
		}
		maxResults := args.MaxResults
		if maxResults == 0 {
			maxResults = args.Limit
		}
		detail := strings.TrimSpace(args.Detail)
		if detail == "" {
			detail = "summary"
		}
		opts := storage.SearchOptions{
			Query:                  args.Query,
			Limit:                  clampBudget(maxResults, 8, 30),
			MaxSearches:            clampBudget(args.MaxSearches, 3, 5),
			MaxSectionsPerDocument: clampBudget(args.MaxSectionsPerDocument, 2, 5),
			ProfileDetail:          strings.TrimSpace(args.ProfileDetail),
			MaxCharsPerResult:      clampBudget(args.MaxCharsPerResult, 1000, 4000),
			Detail:                 detail,
		}
		// In summary mode, profile_detail and max_chars_per_result are irrelevant
		if detail == "summary" {
			opts.ProfileDetail = "none"
			opts.MaxCharsPerResult = 0
		}
		result, err := h.query.SearchDetailed(ctx, opts)
		if err != nil {
			return nil, err
		}
		return toolResult(result), nil
	case "doc_context":
		var args query.ContextRequest
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return nil, fmt.Errorf("invalid doc_context arguments: %w", err)
		}
		pack, err := h.query.Context(ctx, args)
		if err != nil {
			return nil, err
		}
		return toolResult(pack), nil
	case "doc_get_node":
		if h.store == nil {
			return nil, fmt.Errorf("graph storage is not configured")
		}
		var args struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return nil, fmt.Errorf("invalid doc_get_node arguments: %w", err)
		}
		id := strings.TrimSpace(args.ID)
		if id == "" {
			return nil, fmt.Errorf("id is required")
		}
		node, err := h.store.GetNode(ctx, id)
		if err != nil {
			return nil, err
		}
		return toolResult(node), nil
	case "doc_related":
		if h.store == nil {
			return nil, fmt.Errorf("graph storage is not configured")
		}
		var args struct {
			ID        string `json:"id"`
			Direction string `json:"direction"`
			Kind      string `json:"kind"`
			Limit     int    `json:"limit"`
		}
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return nil, fmt.Errorf("invalid doc_related arguments: %w", err)
		}
		id := strings.TrimSpace(args.ID)
		if id == "" {
			return nil, fmt.Errorf("id is required")
		}
		related, err := h.store.RelatedNodes(ctx, id, storage.RelatedOptions{
			Direction: strings.TrimSpace(args.Direction),
			Kind:      strings.TrimSpace(args.Kind),
			Limit:     args.Limit,
		})
		if err != nil {
			return nil, err
		}
		return toolResult(map[string]any{"related": related}), nil
	case "doc_impact":
		if h.store == nil {
			return nil, fmt.Errorf("graph storage is not configured")
		}
		var args struct {
			ID        string `json:"id"`
			Direction string `json:"direction"`
			Kind      string `json:"kind"`
			MaxDepth  int    `json:"max_depth"`
			Limit     int    `json:"limit"`
		}
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return nil, fmt.Errorf("invalid doc_impact arguments: %w", err)
		}
		id := strings.TrimSpace(args.ID)
		if id == "" {
			return nil, fmt.Errorf("id is required")
		}
		result, err := h.store.Impact(ctx, id, storage.ImpactOptions{
			Direction: strings.TrimSpace(args.Direction),
			Kind:      strings.TrimSpace(args.Kind),
			MaxDepth:  args.MaxDepth,
			Limit:     args.Limit,
		})
		if err != nil {
			return nil, err
		}
		return toolResult(result), nil
	case "doc_get_section":
		if h.store == nil {
			return nil, fmt.Errorf("graph storage is not configured")
		}
		var args struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return nil, fmt.Errorf("invalid doc_get_section arguments: %w", err)
		}
		id := strings.TrimSpace(args.ID)
		if id == "" {
			return nil, fmt.Errorf("id is required")
		}
		section, err := h.store.GetSection(ctx, id)
		if err != nil {
			return nil, err
		}
		return toolResult(section), nil
	default:
		return nil, fmt.Errorf("unknown tool %q", call.Name)
	}
}

func toolResult(value any) any {
	data, err := json.Marshal(value)
	if err != nil {
		data = []byte(fmt.Sprintf("%v", value))
	}
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(data)},
		},
	}
}

func clampBudget(value int, fallback int, hardLimit int) int {
	if value <= 0 {
		return fallback
	}
	if value > hardLimit {
		return hardLimit
	}
	return value
}

func canonicalToolName(name string) string {
	switch name {
	case "product_search":
		return "doc_search"
	case "product_context":
		return "doc_context"
	case "product_get_node":
		return "doc_get_node"
	case "product_related":
		return "doc_related"
	case "product_impact":
		return "doc_impact"
	default:
		return name
	}
}

func tools() []map[string]any {
	return []map[string]any{
		{
			"name": "doc_search",
			"description": "Search the DocGraph knowledge base for indexed documentation. " +
				"By default returns lightweight summaries (section IDs, titles, heading paths, snippets) to help you quickly identify relevant sections. " +
				"Use detail='content' to include full text content, or call doc_get_section on specific section_ids for targeted deep reading.\n\n" +
				"Recommended workflow: 1) Call doc_search with default detail='summary' to scan results, " +
				"2) Identify the most relevant section_ids, " +
				"3) Call doc_get_section for those section_ids to get complete content.\n\n" +
				"This two-step approach keeps your context focused and reduces information overload.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Search query. Use natural language or keywords to find relevant documentation. Examples: 'authentication flow', 'how to configure SFTP source', 'REST API error handling'",
					},
					"limit": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"maximum":     200,
						"description": "Deprecated — use max_results instead. Maximum number of results.",
					},
					"max_searches": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"maximum":     5,
						"description": "Maximum number of internal search strategies to attempt (phrase match, token match, etc.). Higher values improve recall. Default 3.",
					},
					"max_results": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"maximum":     30,
						"description": "Maximum number of search result sections to return. Default 8.",
					},
					"max_sections_per_document": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"maximum":     5,
						"description": "Maximum sections to return per document. Prevents one document from dominating results. Default 2.",
					},
					"profile_detail": map[string]any{
						"type":        "string",
						"enum":        []string{"none", "compact", "full"},
						"description": "Level of search analysis metadata to include. Only applies when detail='content'. 'none' = content only, 'compact' = matched terms and tags, 'full' = all analytical detail. Default 'none'.",
					},
					"max_chars_per_result": map[string]any{
						"type":        "integer",
						"minimum":     1000,
						"maximum":     4000,
						"description": "Maximum characters of section content per result. Only applies when detail='content'. Increase this (up to 4000) for more detailed content. Default 1000.",
					},
					"detail": map[string]any{
						"type":        "string",
						"enum":        []string{"summary", "content"},
						"description": "Output detail level. 'summary' (default) = lightweight results with section IDs, titles, heading paths, and FTS snippets — ideal for scanning and identifying relevant sections before deep reading with doc_get_section. 'content' = full text content per section (previous behavior). Default 'summary'.",
					},
				},
				"required": []string{"query"},
			},
		},
		{
			"name": "doc_context",
			"description": "Build a focused, evidence-backed context pack from indexed documentation for a specific task or question. " +
				"Returns up to max_sections matching document sections with full text content, document titles, URLs, and heading paths.\n\n" +
				"Use this tool when:\n" +
				"- You need comprehensive documentation context to answer a complex question about a product or system\n" +
				"- You want to understand a product's capabilities, architecture, or behavior in depth\n" +
				"- You need to gather evidence from multiple documents to support a conclusion\n\n" +
				"This is the primary tool for getting full document content. The returned content is the complete indexed text " +
				"from the documentation — you do NOT need to fetch the document_url separately. " +
				"If you only need quick keyword matches, use doc_search instead.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task": map[string]any{
						"type":        "string",
						"description": "Natural language description of what you need documentation context for. Example: 'Explain the authentication architecture of Product X', 'How does data sync work for Git sources?'",
					},
					"query": map[string]any{
						"type":        "string",
						"description": "Search query to find relevant documentation. Used if task is not provided, or as a supplement to task for more targeted retrieval.",
					},
					"max_sections": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"maximum":     20,
						"description": "Maximum number of documentation sections to include in the context pack. Default 8.",
					},
					"max_chars": map[string]any{
						"type":        "integer",
						"minimum":     1000,
						"maximum":     20000,
						"description": "Total character budget for all section content combined. Increase this (up to 20000) for comprehensive context. Default 12000.",
					},
				},
			},
		},
		{
			"name": "doc_get_node",
			"description": "Fetch a knowledge graph node by its ID. " +
				"Returns the node's kind (product, module, document, section, api, term), name, canonical name, confidence, and metadata. " +
				"Does NOT return document content — use doc_search, doc_context, or doc_get_section for content.\n\n" +
				"Use this tool when:\n" +
				"- You already have a node ID from a previous search or graph traversal and want its metadata\n" +
				"- You need to identify what kind of entity a node represents before exploring its relationships",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{
						"type":        "string",
						"description": "The unique node ID. Obtain from doc_search results (section_id, document_id) or from doc_related/doc_impact results.",
					},
				},
				"required": []string{"id"},
			},
		},
		{
			"name": "doc_related",
			"description": "Find knowledge graph nodes connected to a given node via edges. " +
				"Returns related nodes with their connecting edges, including edge kind (contains, describes, mentions, exposes_api) and provenance.\n\n" +
				"Use this tool when:\n" +
				"- You want to discover what products contain a module, what modules expose an API, or what documents mention a term\n" +
				"- You need to navigate the knowledge graph from a known node to explore relationships\n\n" +
				"Direction: 'out' = nodes this node points to (children/dependents), 'in' = nodes pointing to this node (parents/dependencies), 'both' = all. " +
				"Kind filter: contains, describes, mentions, exposes_api — leave empty for all kinds.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{
						"type":        "string",
						"description": "Node ID to find related nodes for. Obtain from doc_search, doc_get_node, or previous doc_related results.",
					},
					"direction": map[string]any{
						"type":        "string",
						"enum":        []string{"both", "out", "in"},
						"description": "Edge direction filter. 'out' = nodes this node points to, 'in' = nodes pointing to this node, 'both' = all. Default 'both'.",
					},
					"kind": map[string]any{
						"type":        "string",
						"description": "Edge kind filter: 'contains' (product→module, module→document), 'describes' (document→term), 'mentions' (section→term), 'exposes_api' (module→api). Leave empty for all kinds.",
					},
					"limit": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"maximum":     100,
						"description": "Maximum number of related nodes to return. Default 20.",
					},
				},
				"required": []string{"id"},
			},
		},
		{
			"name": "doc_impact",
			"description": "Trace impact and dependency chains through the knowledge graph from a starting node. " +
				"Returns multi-hop paths showing how changes to one entity may affect downstream entities.\n\n" +
				"Use this tool when:\n" +
				"- You need to understand what would be affected if a product, module, or API changes\n" +
				"- You want to trace dependency chains (e.g., 'if this API changes, which documents and products are impacted?')\n" +
				"- You need multi-hop relationship analysis beyond direct connections (use doc_related for single-hop)\n\n" +
				"Direction: 'out' = downstream impact (what this change affects), 'in' = upstream dependencies (what this depends on), 'both' = bidirectional. " +
				"Max depth 1-5 hops.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{
						"type":        "string",
						"description": "Starting node ID for impact chain traversal.",
					},
					"direction": map[string]any{
						"type":        "string",
						"enum":        []string{"out", "in", "both"},
						"description": "'out' = trace downstream impact (what this change affects), 'in' = trace upstream dependencies (what this depends on), 'both' = bidirectional. Default 'out'.",
					},
					"kind": map[string]any{
						"type":        "string",
						"description": "Edge kind filter: contains, describes, mentions, exposes_api. Leave empty for all kinds.",
					},
					"max_depth": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"maximum":     5,
						"description": "Maximum number of hops to traverse. 1 = direct connections only, 2-5 = deeper chains. Default 3.",
					},
					"limit": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"maximum":     200,
						"description": "Maximum number of impact paths to return. Default 50.",
					},
				},
				"required": []string{"id"},
			},
		},
		{
			"name": "doc_get_section",
			"description": "Retrieve the full text content of a specific documentation section by its section_id. " +
				"Returns the complete section content with document title, heading path, and source URL.\n\n" +
				"Use this tool when:\n" +
				"- You have a section_id from a previous doc_search (summary mode) result and need the full content\n" +
				"- The content from a search result was truncated and you want to see the complete section\n\n" +
				"This is the second step of the recommended search workflow: doc_search → identify relevant section_ids → doc_get_section.\n" +
				"This returns the original indexed text — you do NOT need to fetch the document_url.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{
						"type":        "string",
						"description": "Section ID from a previous doc_search or doc_context result",
					},
				},
				"required": []string{"id"},
			},
		},
	}
}

func writeJSONResponse(w *bufio.Writer, resp Response) error {
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		return err
	}
	return w.Flush()
}
