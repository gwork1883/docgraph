package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/docgraph/docgraph/internal/embedding"
	"github.com/docgraph/docgraph/internal/query"
	"github.com/docgraph/docgraph/internal/storage"
	"github.com/docgraph/docgraph/internal/vectorstore"
)

// Handler processes individual MCP JSON-RPC messages, independent of transport.
type Handler struct {
	query                     *query.Service
	store                     storage.Store
	embedder                  embedding.Embedder
	embeddingGeneratorVersion string
	vectorSearchWeight        float64
}

func NewHandler(queryService *query.Service, store storage.Store) *Handler {
	return NewHandlerWithEmbedding(queryService, store, embedding.NewNoOpEmbedder(), embedding.DefaultGeneratorVersion)
}

func NewHandlerWithEmbedding(queryService *query.Service, store storage.Store, embedder embedding.Embedder, generatorVersion string) *Handler {
	return NewHandlerWithEmbeddingAndSearchWeight(queryService, store, embedder, generatorVersion, 0.4)
}

func NewHandlerWithEmbeddingAndSearchWeight(queryService *query.Service, store storage.Store, embedder embedding.Embedder, generatorVersion string, vectorSearchWeight float64) *Handler {
	if embedder == nil {
		embedder = embedding.NewNoOpEmbedder()
	}
	if strings.TrimSpace(generatorVersion) == "" {
		generatorVersion = embedding.DefaultGeneratorVersion
	}
	return &Handler{query: queryService, store: store, embedder: embedder, embeddingGeneratorVersion: generatorVersion, vectorSearchWeight: normalizeVectorSearchWeight(vectorSearchWeight)}
}

func normalizeVectorSearchWeight(weight float64) float64 {
	if weight <= 0 {
		return 0.4
	}
	if weight > 1 {
		return 1
	}
	return weight
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

func NewServerWithStoreAndEmbedding(queryService *query.Service, store storage.Store, embedder embedding.Embedder, generatorVersion string, vectorSearchWeight float64, in io.Reader, out io.Writer) *Server {
	return NewServerWithStoreAndEmbeddingAndPlan(queryService, store, embedder, generatorVersion, vectorSearchWeight, "auto", "auto", in, out)
}

func NewServerWithStoreAndEmbeddingAndPlan(queryService *query.Service, store storage.Store, embedder embedding.Embedder, generatorVersion string, vectorSearchWeight float64, tokenizer string, chunkStrategy string, in io.Reader, out io.Writer) *Server {
	if runtimeSetter, ok := store.(vectorstore.SearchRuntimeSetter); ok && embedder != nil && strings.TrimSpace(embedder.Model()) != "" {
		tokenizer = strings.TrimSpace(tokenizer)
		if tokenizer == "" {
			tokenizer = "auto"
		}
		chunkStrategy = strings.TrimSpace(chunkStrategy)
		if chunkStrategy == "" {
			chunkStrategy = "auto"
		}
		runtimeSetter.SetVectorSearchRuntime(vectorstore.SearchRuntime{
			Embedder:         embedder,
			SearchWeight:     normalizeVectorSearchWeight(vectorSearchWeight),
			VectorCandidates: 60,
			MinSimilarity:    0,
			GeneratorVersion: generatorVersion,
			Tokenizer:        tokenizer,
			ChunkStrategy:    chunkStrategy,
		})
	}
	return &Server{handler: NewHandlerWithEmbeddingAndSearchWeight(queryService, store, embedder, generatorVersion, vectorSearchWeight), in: in, out: out}
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

const (
	protocolVersion2024 = "2024-11-05"
	protocolVersion2025 = "2025-11-25"
)

const (
	docGraphInstructions = "DocGraph is a local documentation knowledge server. Start with doc_search detail='summary' to discover relevant sections, then call doc_get_section for selected section IDs. Preserve the user's language and exact terms in search intents and queries; do not translate non-English issues into English-only queries. For troubleshooting, narrow the issue intent and evidence chain before searching, and treat keyword-only matches as peripheral unless they directly support the chain. Prompt template: doc_answer."

	troubleshootingSearchPromptName = "doc_answer"
	troubleshootingSearchPromptDesc = "Use DocGraph's local documentation knowledge base to answer a question or build a troubleshooting path from indexed docs."
)

var supportedProtocolVersions = map[string]bool{
	protocolVersion2025: true,
	"2025-06-18":        true,
	"2025-03-26":        true,
	protocolVersion2024: true,
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
			"protocolVersion": negotiateProtocolVersion(req.Params),
			"capabilities": map[string]any{
				"tools":   map[string]any{},
				"prompts": map[string]any{},
			},
			"instructions": docGraphInstructions,
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
	case "prompts/list":
		resp.Result = map[string]any{"prompts": prompts()}
	case "prompts/get":
		result, err := getPrompt(req.Params)
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

func negotiateProtocolVersion(params json.RawMessage) string {
	if len(params) == 0 {
		return protocolVersion2024
	}
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(params, &init); err != nil || strings.TrimSpace(init.ProtocolVersion) == "" {
		return protocolVersion2024
	}
	if supportedProtocolVersions[init.ProtocolVersion] {
		return init.ProtocolVersion
	}
	return protocolVersion2025
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type promptGetParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
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
			Query                  string   `json:"query"`
			Limit                  int      `json:"limit"`
			MaxSearches            int      `json:"max_searches"`
			MaxResults             int      `json:"max_results"`
			MaxSectionsPerDocument int      `json:"max_sections_per_document"`
			ProfileDetail          string   `json:"profile_detail"`
			MaxCharsPerResult      int      `json:"max_chars_per_result"`
			Detail                 string   `json:"detail"`
			UseRelationExpansion   *bool    `json:"use_relation_expansion"`
			RelationTypes          []string `json:"relation_types"`
			ExactTerms             []string `json:"exact_terms"`
			SemanticIntents        []string `json:"semantic_intents"`
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
		useRelationExpansion := true
		if args.UseRelationExpansion != nil {
			useRelationExpansion = *args.UseRelationExpansion
		}
		opts := storage.SearchOptions{
			Query:                     args.Query,
			Limit:                     clampBudget(maxResults, 8, 30),
			MaxSearches:               clampBudget(args.MaxSearches, 5, 5),
			MaxSectionsPerDocument:    clampBudget(args.MaxSectionsPerDocument, 2, 5),
			ProfileDetail:             strings.TrimSpace(args.ProfileDetail),
			MaxCharsPerResult:         clampBudget(args.MaxCharsPerResult, 1000, 4000),
			Detail:                    detail,
			UseRelationExpansion:      useRelationExpansion,
			RelationDepth:             1,
			RelationTypes:             args.RelationTypes,
			OriginalQuery:             args.Query,
			ExactTerms:                args.ExactTerms,
			SemanticIntents:           args.SemanticIntents,
			EmbeddingGeneratorVersion: h.embeddingGeneratorVersion,
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
	case "doc_propose_relation":
		if h.store == nil {
			return nil, fmt.Errorf("graph storage is not configured")
		}
		var args struct {
			RelationType   string          `json:"relation_type"`
			FromDocumentID string          `json:"from_document_id"`
			FromAnchor     string          `json:"from_anchor"`
			ToDocumentID   string          `json:"to_document_id"`
			ToAnchor       string          `json:"to_anchor"`
			Direction      string          `json:"direction"`
			Reason         string          `json:"reason"`
			Evidence       json.RawMessage `json:"evidence"`
			EvidenceJSON   string          `json:"evidence_json"`
			ProposedEffect string          `json:"proposed_effect"`
			Confidence     float64         `json:"confidence"`
			CreatedByRef   string          `json:"created_by_ref"`
		}
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return nil, fmt.Errorf("invalid doc_propose_relation arguments: %w", err)
		}
		evidenceJSON := strings.TrimSpace(args.EvidenceJSON)
		if evidenceJSON == "" && len(args.Evidence) > 0 {
			evidenceJSON = string(args.Evidence)
		}
		proposal, err := h.store.CreateKnowledgeRelationProposal(ctx, storage.KnowledgeRelationProposalInput{
			RelationType:   strings.TrimSpace(args.RelationType),
			FromDocumentID: strings.TrimSpace(args.FromDocumentID),
			FromAnchor:     strings.TrimSpace(args.FromAnchor),
			ToDocumentID:   strings.TrimSpace(args.ToDocumentID),
			ToAnchor:       strings.TrimSpace(args.ToAnchor),
			Direction:      strings.TrimSpace(args.Direction),
			Reason:         strings.TrimSpace(args.Reason),
			EvidenceJSON:   evidenceJSON,
			ProposedEffect: strings.TrimSpace(args.ProposedEffect),
			Confidence:     args.Confidence,
			CreatedByType:  "mcp_agent",
			CreatedByRef:   strings.TrimSpace(args.CreatedByRef),
		})
		if err != nil {
			return nil, err
		}
		return toolResult(map[string]any{"proposal": proposal, "status": proposal.Status}), nil
	case "doc_list_relation_proposals":
		if h.store == nil {
			return nil, fmt.Errorf("graph storage is not configured")
		}
		var args struct {
			Status     string `json:"status"`
			DocumentID string `json:"document_id"`
			Limit      int    `json:"limit"`
			Offset     int    `json:"offset"`
		}
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return nil, fmt.Errorf("invalid doc_list_relation_proposals arguments: %w", err)
		}
		proposals, err := h.store.ListKnowledgeRelationProposals(ctx, storage.KnowledgeRelationProposalListOptions{
			Status:     strings.TrimSpace(args.Status),
			DocumentID: strings.TrimSpace(args.DocumentID),
			Limit:      clampBudget(args.Limit, 20, 100),
			Offset:     args.Offset,
		})
		if err != nil {
			return nil, err
		}
		return toolResult(map[string]any{"proposals": proposals}), nil
	case "doc_get_relation_context":
		if h.store == nil {
			return nil, fmt.Errorf("graph storage is not configured")
		}
		var args struct {
			DocumentID      string   `json:"document_id"`
			RelationTypes   []string `json:"relation_types"`
			IncludeDisabled bool     `json:"include_disabled"`
			Limit           int      `json:"limit"`
		}
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return nil, fmt.Errorf("invalid doc_get_relation_context arguments: %w", err)
		}
		documentID := strings.TrimSpace(args.DocumentID)
		if documentID == "" {
			return nil, fmt.Errorf("document_id is required")
		}
		relations, err := h.store.ListKnowledgeRelations(ctx, storage.KnowledgeRelationListOptions{
			DocumentID:      documentID,
			RelationTypes:   args.RelationTypes,
			IncludeDisabled: args.IncludeDisabled,
			Limit:           clampBudget(args.Limit, 20, 100),
		})
		if err != nil {
			return nil, err
		}
		return toolResult(map[string]any{"document_id": documentID, "relations": relations}), nil
	default:
		return nil, fmt.Errorf("unknown tool %q", call.Name)
	}
}

func (h *Handler) queryEmbedding(ctx context.Context, queryText string, intents []string, enabled bool) ([]float32, string, error) {
	if !enabled || h.embedder == nil || strings.TrimSpace(h.embedder.Model()) == "" {
		return nil, "", nil
	}
	texts := []string{strings.TrimSpace(queryText)}
	texts = append(texts, intents...)
	text := strings.Join(nonEmptyStrings(texts), "\n")
	if text == "" {
		return nil, "", nil
	}
	vectors, err := h.embedder.Embed(ctx, []string{text})
	if err != nil {
		return nil, "", err
	}
	if len(vectors) == 0 {
		return nil, "", nil
	}
	return vectors[0], h.embedder.Model(), nil
}

func (h *Handler) canUseVectorCandidates() bool {
	return h.embedder != nil && strings.TrimSpace(h.embedder.Model()) != ""
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

func nonEmptyStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
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

func prompts() []map[string]any {
	return []map[string]any{
		{
			"name":        troubleshootingSearchPromptName,
			"description": troubleshootingSearchPromptDesc,
			"arguments": []map[string]any{
				{
					"name":        "issue",
					"description": "The troubleshooting issue or symptom to investigate in local documentation.",
					"required":    true,
				},
			},
		},
	}
}

func getPrompt(params json.RawMessage) (any, error) {
	var req promptGetParams
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, fmt.Errorf("invalid prompt params: %w", err)
	}
	switch strings.TrimSpace(req.Name) {
	case troubleshootingSearchPromptName:
		issue := strings.TrimSpace(fmt.Sprint(req.Arguments["issue"]))
		if issue == "" || issue == "<nil>" {
			return nil, fmt.Errorf("argument issue is required")
		}
		return map[string]any{
			"description": troubleshootingSearchPromptDesc,
			"messages": []map[string]any{
				{
					"role": "user",
					"content": map[string]any{
						"type": "text",
						"text": troubleshootingSearchPromptText(issue),
					},
				},
			},
		}, nil
	default:
		return nil, fmt.Errorf("unknown prompt %q", req.Name)
	}
}

func troubleshootingSearchPromptText(issue string) string {
	return "Use DocGraph's local documentation knowledge base to answer this question: " + issue + "\n\n" +
		"1. Rewrite the question as a concise answer intent in the user's original language.\n" +
		"2. Preserve exact user-provided terms in search queries; for Chinese questions, keep Chinese terms as the primary query language and do not translate them into English-only queries.\n" +
		"3. Identify the evidence chain that must be explained.\n" +
		"4. Search with exact user-provided terms first, then add maintainer-facing terminology only as supplemental aliases.\n" +
		"5. Start with doc_search detail='summary' to scan matching sections and next-read signals.\n" +
		"6. Fetch selected sections with doc_get_section before relying on detailed claims.\n" +
		"7. Build the answer from evidence directly tied to the chain.\n" +
		"8. Downgrade keyword-only matches to peripheral notes unless they directly support the chain."
}

func tools() []map[string]any {
	return []map[string]any{
		{
			"name": "doc_search",
			"description": "Search indexed local documentation with hybrid retrieval over Chinese terms, technical symbols, identifier subterms, Unicode FTS, trigram matching, profile fallback, substring fallback, and maintained terminology dictionaries. When vector search is enabled, results also include semantic vector candidates fused via intent-adaptive RRF. " +
				"Default detail='summary' returns section IDs, titles, heading paths, snippets, suggested_reads.explicit_references, and hit metadata (plus hybrid diagnostics when vector search is enabled); use detail='content' for one-shot full text or call doc_get_section for selected section IDs. " +
				"Preserve product names, business terms, config keys, schema fields, enum values, API names, paths, and code symbols verbatim; add clear aliases or module names only when they clarify intent. " +
				"For troubleshooting, search with a concise issue intent plus exact user-provided terms; prefer evidence directly tied to the issue chain over keyword-only matches.",
			"annotations": map[string]any{
				"readOnlyHint":  true,
				"openWorldHint": false,
			},
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Search query. Use natural language, keywords, or a mix. The backend keeps the query text and also expands it with GSE Chinese terms, technical symbols, identifier subterms, trigram/profile/substring fallbacks, and future terminology dictionaries. Preserve exact product names, business terms, config keys, schema fields, enum values, API names, file paths, and code identifiers verbatim; add aliases or related terms only when they clarify the user's intent. For troubleshooting, prefer a concise issue intent and maintainer-facing terms over raw prompt noise. Examples: 'access control schema operation group configuration', 'authentication flow', 'how to configure SFTP source', 'REST API error handling'",
					},
					"limit": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"maximum":     30,
						"description": "Deprecated alias for max_results. Clamped to the same maximum as max_results.",
					},
					"max_searches": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"maximum":     5,
						"description": "Maximum number of internal search strategies to attempt, in order, from token/Unicode FTS and trigram search through profile and substring fallbacks. Higher values improve recall and may broaden results. Default 5.",
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
					"use_relation_expansion": map[string]any{
						"type":        "boolean",
						"description": "Whether to use approved knowledge relations for one-hop retrieval expansion and result explanations. Default true.",
					},
					"exact_terms": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Must-keep exact terms such as API paths, config keys, error codes, symbols, or identifiers. They continue through exact/lexical search even when semantic intents are supplied.",
					},
					"semantic_intents": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Optional semantic intent expansions for recall planning. They do not replace query or exact_terms. When vector search is enabled, providing intents biases retrieval toward semantic matching over exact-text lookup.",
					},
					"relation_types": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string", "enum": []string{"related_to", "schema_reference", "deprecated_by", "should_ignore"}},
						"description": "Optional approved relation types to use for expansion/explanations. Empty means all supported relation types.",
					},
				},
				"required": []string{"query"},
			},
		},
		{
			"name": "doc_get_node",
			"description": "Fetch a knowledge graph node by its ID. " +
				"Returns the node's kind (product, module, document, section, api, term), name, canonical name, confidence, and metadata. " +
				"Does NOT return document content — use doc_search or doc_get_section for content.\n\n" +
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
				"Returns the complete section content with document title, heading path, source URL, and explicit author-written references. " +
				"It does not automatically read referenced target sections.\n\n" +
				"Use this tool when:\n" +
				"- You have a section_id from a previous doc_search (summary mode) result and need the full content\n" +
				"- The content from a search result was truncated and you want to see the complete section\n\n" +
				"This is the second step of the recommended search workflow: doc_search → suggested reads / explicit references → doc_get_section.\n" +
				"This returns the original indexed text — you do NOT need to fetch the document_url.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{
						"type":        "string",
						"description": "Section ID from a previous doc_search result or suggested read",
					},
				},
				"required": []string{"id"},
			},
		},
		{
			"name":        "doc_propose_relation",
			"description": "Submit a knowledge relation proposal for Web approval. This does not change retrieval behavior until a reviewer approves it. Use when you identify that two documents should be linked, one document supersedes another, or an old document should be ignored/demoted.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"relation_type": map[string]any{
						"type":        "string",
						"enum":        []string{"related_to", "schema_reference", "deprecated_by", "should_ignore"},
						"description": "Type of relation being proposed.",
					},
					"from_document_id": map[string]any{
						"type":        "string",
						"description": "Source document ID for the relation.",
					},
					"from_anchor": map[string]any{
						"type":        "string",
						"description": "Optional heading, fragment, or phrase in the source document.",
					},
					"to_document_id": map[string]any{
						"type":        "string",
						"description": "Target document ID for the relation.",
					},
					"to_anchor": map[string]any{
						"type":        "string",
						"description": "Optional heading, fragment, or phrase in the target document.",
					},
					"direction": map[string]any{
						"type":        "string",
						"enum":        []string{"directed", "undirected"},
						"description": "Relation direction. Default directed.",
					},
					"reason": map[string]any{
						"type":        "string",
						"description": "Required explanation for the reviewer. Include why this relation improves retrieval or avoids stale guidance.",
					},
					"evidence": map[string]any{
						"type":        "object",
						"description": "Optional structured evidence, such as query, snippets, or session notes.",
					},
					"proposed_effect": map[string]any{
						"type":        "string",
						"enum":        []string{"context_link", "boost", "demote", "ignore"},
						"description": "Expected retrieval effect after approval. Defaults from relation_type.",
					},
					"confidence": map[string]any{
						"type":        "number",
						"minimum":     0,
						"maximum":     1,
						"description": "Optional proposer confidence.",
					},
					"created_by_ref": map[string]any{
						"type":        "string",
						"description": "Optional client/session identifier for audit.",
					},
				},
				"required": []string{"relation_type", "from_document_id", "to_document_id", "reason"},
			},
		},
		{
			"name":        "doc_list_relation_proposals",
			"description": "List knowledge relation proposals, usually pending Web approval. This is read-only and does not approve changes.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"status": map[string]any{
						"type":        "string",
						"enum":        []string{"pending", "approved", "rejected", "cancelled", "all"},
						"description": "Proposal status filter. Default all.",
					},
					"document_id": map[string]any{
						"type":        "string",
						"description": "Optional document ID filter for proposals touching a document.",
					},
					"limit": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"maximum":     100,
						"description": "Maximum proposals to return. Default 20.",
					},
					"offset": map[string]any{
						"type":        "integer",
						"minimum":     0,
						"description": "Pagination offset.",
					},
				},
			},
		},
		{
			"name":        "doc_get_relation_context",
			"description": "Fetch approved knowledge relations for a document. Use this after doc_search or doc_get_section to understand linked schema references, related docs, deprecation, or ignore/demotion guidance.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"document_id": map[string]any{
						"type":        "string",
						"description": "Document ID to inspect.",
					},
					"relation_types": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string", "enum": []string{"related_to", "schema_reference", "deprecated_by", "should_ignore"}},
						"description": "Optional relation type filter.",
					},
					"include_disabled": map[string]any{
						"type":        "boolean",
						"description": "Whether to include disabled relations. Default false.",
					},
					"limit": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"maximum":     100,
						"description": "Maximum relations to return. Default 20.",
					},
				},
				"required": []string{"document_id"},
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
