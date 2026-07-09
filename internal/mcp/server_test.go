package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docgraph/docgraph/internal/domain"
	"github.com/docgraph/docgraph/internal/query"
	"github.com/docgraph/docgraph/internal/storage"
)

func TestInitialize(t *testing.T) {
	responses := runTestServer(t, newTestQueryService(t), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	resp := requireResponse(t, responses, 0, "1")
	requireNoRPCError(t, resp)

	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools   map[string]any `json:"tools"`
			Prompts map[string]any `json:"prompts"`
		} `json:"capabilities"`
		Instructions string `json:"instructions"`
		ServerInfo   struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	unmarshalResult(t, resp, &result)
	if result.ProtocolVersion != "2024-11-05" {
		t.Fatalf("protocolVersion = %q, want 2024-11-05", result.ProtocolVersion)
	}
	if result.Capabilities.Tools == nil {
		t.Fatalf("capabilities.tools is nil, want object")
	}
	if result.Capabilities.Prompts == nil {
		t.Fatalf("capabilities.prompts is nil, want object")
	}
	if len(result.Capabilities.Prompts) != 0 {
		t.Fatalf("capabilities.prompts = %+v, want empty object", result.Capabilities.Prompts)
	}
	for _, want := range []string{"local documentation knowledge server", "doc_search detail='summary'", "user's language", "exact terms", "English-only queries", "evidence chain", "keyword-only matches", "doc_answer"} {
		if !strings.Contains(result.Instructions, want) {
			t.Fatalf("initialize instructions missing %q: %s", want, result.Instructions)
		}
	}
	if result.ServerInfo.Name != "docgraph" || result.ServerInfo.Version == "" {
		t.Fatalf("serverInfo = %+v, want docgraph with version", result.ServerInfo)
	}
}

func TestToolsList(t *testing.T) {
	responses := runTestServer(t, newTestQueryService(t), `{"jsonrpc":"2.0","id":"tools","method":"tools/list"}`)
	resp := requireResponse(t, responses, 0, `"tools"`)
	requireNoRPCError(t, resp)

	var result struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			Annotations map[string]any `json:"annotations"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	unmarshalResult(t, resp, &result)

	gotNames := make(map[string]bool, len(result.Tools))
	for _, tool := range result.Tools {
		gotNames[tool.Name] = true
		if tool.Description == "" {
			t.Fatalf("tool %q has empty description", tool.Name)
		}
		if tool.InputSchema["type"] != "object" {
			t.Fatalf("tool %q inputSchema.type = %v, want object", tool.Name, tool.InputSchema["type"])
		}
		if tool.Name == "doc_search" {
			for _, want := range []string{
				"hybrid retrieval",
				"Chinese terms",
				"technical symbols",
				"trigram matching",
				"profile fallback",
				"substring fallback",
				"detail='summary'",
				"suggested_reads.explicit_references",
				"hit metadata",
				"detail='content'",
				"doc_get_section",
				"Preserve product names",
				"business terms",
				"module names",
				"concise issue intent",
				"issue chain",
				"keyword-only matches",
			} {
				if !strings.Contains(tool.Description, want) {
					t.Fatalf("doc_search description missing %q: %s", want, tool.Description)
				}
			}
			if tool.Annotations["readOnlyHint"] != true {
				t.Fatalf("doc_search readOnlyHint = %v, want true", tool.Annotations["readOnlyHint"])
			}
			if tool.Annotations["openWorldHint"] != false {
				t.Fatalf("doc_search openWorldHint = %v, want false", tool.Annotations["openWorldHint"])
			}
			properties, ok := tool.InputSchema["properties"].(map[string]any)
			if !ok {
				t.Fatalf("doc_search properties = %#v, want object", tool.InputSchema["properties"])
			}
			required, ok := tool.InputSchema["required"].([]any)
			if !ok || !jsonArrayContains(required, "query") {
				t.Fatalf("doc_search required = %#v, want query", tool.InputSchema["required"])
			}
			querySchema, ok := properties["query"].(map[string]any)
			if !ok {
				t.Fatalf("doc_search query schema = %#v, want object", properties["query"])
			}
			queryDescription, _ := querySchema["description"].(string)
			for _, want := range []string{"Preserve exact product names", "business terms", "GSE Chinese terms", "future terminology dictionaries", "maintainer-facing terms", "raw prompt noise"} {
				if !strings.Contains(queryDescription, want) {
					t.Fatalf("doc_search query description missing %q: %s", want, queryDescription)
				}
			}
			limitSchema, ok := properties["limit"].(map[string]any)
			if !ok {
				t.Fatalf("doc_search limit schema = %#v, want object", properties["limit"])
			}
			if limitSchema["maximum"] != float64(30) {
				t.Fatalf("doc_search limit maximum = %v, want 30", limitSchema["maximum"])
			}
			limitDescription, _ := limitSchema["description"].(string)
			for _, want := range []string{"Deprecated alias", "Clamped"} {
				if !strings.Contains(limitDescription, want) {
					t.Fatalf("doc_search limit description missing %q: %s", want, limitDescription)
				}
			}
		}
	}
	for _, name := range []string{"doc_search", "doc_get_section", "doc_get_node", "doc_related", "doc_impact"} {
		if !gotNames[name] {
			t.Fatalf("tools/list missing %q in %+v", name, result.Tools)
		}
	}
	if gotNames["doc_context"] {
		t.Fatalf("tools/list includes disabled doc_context in %+v", result.Tools)
	}
}

func TestPromptsListAndGet(t *testing.T) {
	responses := runTestServer(t, newTestQueryService(t),
		`{"jsonrpc":"2.0","id":"prompts","method":"prompts/list"}`,
		`{"jsonrpc":"2.0","id":"prompt","method":"prompts/get","params":{"name":"doc_answer","arguments":{"issue":"imported license changes are not reflected in access control page"}}}`,
	)
	listResp := requireResponse(t, responses, 0, `"prompts"`)
	requireNoRPCError(t, listResp)
	var listResult struct {
		Prompts []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Arguments   []struct {
				Name     string `json:"name"`
				Required bool   `json:"required"`
			} `json:"arguments"`
		} `json:"prompts"`
	}
	unmarshalResult(t, listResp, &listResult)
	if len(listResult.Prompts) != 1 || listResult.Prompts[0].Name != "doc_answer" {
		t.Fatalf("prompts/list = %+v, want doc_answer", listResult.Prompts)
	}
	if listResult.Prompts[0].Description == "" {
		t.Fatalf("doc_answer description is empty")
	}
	if len(listResult.Prompts[0].Arguments) != 1 || listResult.Prompts[0].Arguments[0].Name != "issue" || !listResult.Prompts[0].Arguments[0].Required {
		t.Fatalf("doc_answer arguments = %+v, want required issue", listResult.Prompts[0].Arguments)
	}

	getResp := requireResponse(t, responses, 1, `"prompt"`)
	requireNoRPCError(t, getResp)
	var getResult struct {
		Description string `json:"description"`
		Messages    []struct {
			Role    string `json:"role"`
			Content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	unmarshalResult(t, getResp, &getResult)
	if getResult.Description == "" || len(getResult.Messages) != 1 {
		t.Fatalf("prompts/get result = %+v, want description and one message", getResult)
	}
	message := getResult.Messages[0]
	if message.Role != "user" || message.Content.Type != "text" {
		t.Fatalf("prompt message = %+v, want user text message", message)
	}
	for _, want := range []string{"DocGraph's local documentation knowledge base", "user's original language", "Chinese questions", "Chinese terms as the primary query language", "English-only queries", "supplemental aliases", "evidence chain", "doc_search detail='summary'", "doc_get_section", "keyword-only matches"} {
		if !strings.Contains(message.Content.Text, want) {
			t.Fatalf("prompt text missing %q: %s", want, message.Content.Text)
		}
	}
}

func TestPromptsGetUnknownPrompt(t *testing.T) {
	responses := runTestServer(t, newTestQueryService(t), `{"jsonrpc":"2.0","id":"unknown-prompt","method":"prompts/get","params":{"name":"unknown","arguments":{"issue":"missing docs"}}}`)
	resp := requireResponse(t, responses, 0, `"unknown-prompt"`)
	if resp.Error == nil {
		t.Fatalf("unknown prompt error is nil")
	}
	if resp.Error.Code != -32602 {
		t.Fatalf("unknown prompt error code = %d, want -32602", resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, `unknown prompt "unknown"`) {
		t.Fatalf("unknown prompt error message = %q", resp.Error.Message)
	}
}

func TestProductSearchToolCall(t *testing.T) {
	responses := runTestServer(t, newTestQueryService(t), `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"doc_search","arguments":{"query":"member benefits","limit":5}}}`)
	resp := requireResponse(t, responses, 0, "2")
	requireNoRPCError(t, resp)

	var payload struct {
		Query        string                  `json:"query"`
		SearchesUsed int                     `json:"searches_used"`
		Attempts     []storage.SearchAttempt `json:"attempts"`
		Hits         []storage.SearchHit     `json:"hits"`
	}
	unmarshalToolText(t, resp, &payload)
	if payload.Query != "member benefits" || payload.SearchesUsed == 0 || len(payload.Attempts) == 0 {
		t.Fatalf("doc_search metadata = %+v, want query and attempts", payload)
	}
	if len(payload.Hits) == 0 {
		t.Fatalf("doc_search returned no hits")
	}
	hit := payload.Hits[0]
	if hit.SectionID == "" || hit.DocumentID != "doc-member" {
		t.Fatalf("first hit ids = section:%q document:%q, want doc-member hit", hit.SectionID, hit.DocumentID)
	}
	if hit.DocumentTitle != "Member Benefits" {
		t.Fatalf("first hit document title = %q, want Member Benefits", hit.DocumentTitle)
	}
	// Default detail=summary: content should contain hint, snippet should contain highlighted match
	if hit.Content == "" {
		t.Fatalf("summary mode hit content is empty, want hint")
	}
	if !strings.Contains(hit.Content, "doc_get_section") {
		t.Fatalf("summary mode hit content = %q, want doc_get_section hint", hit.Content)
	}
	if !strings.Contains(hit.Snippet, "member/benefits") {
		t.Fatalf("summary mode hit snippet = %q, want highlighted match", hit.Snippet)
	}
}

func TestLegacyProductSearchToolAlias(t *testing.T) {
	responses := runTestServer(t, newTestQueryService(t), `{"jsonrpc":"2.0","id":"legacy","method":"tools/call","params":{"name":"product_search","arguments":{"query":"member benefits","limit":5}}}`)
	resp := requireResponse(t, responses, 0, `"legacy"`)
	requireNoRPCError(t, resp)

	var payload struct {
		Hits []storage.SearchHit `json:"hits"`
	}
	unmarshalToolText(t, resp, &payload)
	if len(payload.Hits) == 0 {
		t.Fatalf("legacy product_search returned no hits")
	}
}

func TestDocSearchToolReturnsExplicitReferenceSummary(t *testing.T) {
	store := newTestStore(t)
	seedMCPExplicitReferenceDocument(t, store)
	responses := runTestServerWithStore(t, query.NewService(store), store, `{"jsonrpc":"2.0","id":"refs","method":"tools/call","params":{"name":"doc_search","arguments":{"query":"configuring operations","max_results":5,"max_sections_per_document":5}}}`)
	resp := requireResponse(t, responses, 0, `"refs"`)
	requireNoRPCError(t, resp)

	var payload struct {
		Hits           []storage.SearchHit `json:"hits"`
		SuggestedReads struct {
			ExplicitReferences  []domain.ExplicitReference `json:"explicit_references"`
			ImplicitSymbolLinks []domain.SuggestedRead     `json:"implicit_symbol_links"`
			CuratedRelations    []domain.SuggestedRead     `json:"curated_relations"`
			StructuralNeighbors []domain.SuggestedRead     `json:"structural_neighbors"`
		} `json:"suggested_reads"`
	}
	unmarshalToolText(t, resp, &payload)
	hit := findMCPSearchHit(payload.Hits, "section-explicit-source")
	if hit == nil {
		t.Fatalf("doc_search hits = %+v, want explicit source hit", payload.Hits)
	}
	if !hit.HasExplicitReferences || hit.ExplicitReferenceCount != 1 {
		t.Fatalf("explicit source hit = %+v, want reference summary metadata", *hit)
	}
	if len(payload.SuggestedReads.ExplicitReferences) == 0 {
		t.Fatalf("suggested_reads = %+v, want explicit reference suggestions", payload.SuggestedReads)
	}
	ref := payload.SuggestedReads.ExplicitReferences[0]
	if ref.SourceSectionID != "section-explicit-source" || ref.TargetSectionID != "section-explicit-propagation" || !ref.Resolved {
		t.Fatalf("explicit reference suggestion = %+v, want resolved propagation target", ref)
	}
	if payload.SuggestedReads.ImplicitSymbolLinks == nil || payload.SuggestedReads.CuratedRelations == nil || payload.SuggestedReads.StructuralNeighbors == nil {
		t.Fatalf("suggested_reads = %+v, want separate non-explicit fields", payload.SuggestedReads)
	}
}

func TestProductSearchToolBudgetParameters(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if _, err := store.UpdateDocumentProfileDesc(ctx, storage.DocumentProfileInput{
		DocumentID: "doc-member",
		Desc:       "Administrator maintained member benefits description",
	}); err != nil {
		t.Fatalf("UpdateDocumentProfileDesc returned error: %v", err)
	}
	if _, err := store.UpsertDocumentRetrievalProfile(ctx, storage.RetrievalProfileInput{
		DocumentID:           "doc-member",
		RetrievalProfileJSON: `{"top_tags":["benefits","api"],"top_terms":[{"term":"member benefits","tf":2,"sections":2,"score":5.5}],"keyphrases":["member benefits"]}`,
		GeneratedFromHash:    "hash-doc-member",
	}); err != nil {
		t.Fatalf("UpsertDocumentRetrievalProfile returned error: %v", err)
	}
	responses := runTestServer(t, query.NewService(store), `{"jsonrpc":"2.0","id":"budget","method":"tools/call","params":{"name":"doc_search","arguments":{"query":"member benefits","max_searches":2,"max_results":1,"max_sections_per_document":1,"profile_detail":"compact","max_chars_per_result":30,"detail":"content"}}}`)
	resp := requireResponse(t, responses, 0, `"budget"`)
	requireNoRPCError(t, resp)

	var payload struct {
		Query        string                  `json:"query"`
		SearchesUsed int                     `json:"searches_used"`
		Attempts     []storage.SearchAttempt `json:"attempts"`
		Hits         []storage.SearchHit     `json:"hits"`
	}
	unmarshalToolText(t, resp, &payload)
	if payload.Query != "member benefits" || payload.SearchesUsed != 2 || len(payload.Attempts) != 2 {
		t.Fatalf("budgeted doc_search metadata = %+v, want two attempts", payload)
	}
	if len(payload.Hits) != 1 {
		t.Fatalf("budgeted doc_search hits len = %d, want 1: %+v", len(payload.Hits), payload.Hits)
	}
	hit := payload.Hits[0]
	if !strings.Contains(hit.Content, "truncated at 30 bytes") {
		t.Fatalf("budgeted hit content = %q, want truncation hint", hit.Content)
	}
	if hit.Desc == "" || hit.Profile == nil || hit.QueryMatch == nil {
		t.Fatalf("budgeted hit = %+v, want desc, compact profile, and query evidence", hit)
	}
}

func TestProductSearchToolClampsInvalidBudgetsAndBoundsFullProfile(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if _, err := store.UpsertDocumentRetrievalProfile(ctx, storage.RetrievalProfileInput{
		DocumentID: "doc-member",
		RetrievalProfileJSON: `{
			"top_tags":["benefits"],
			"top_terms":[{"term":"member benefits","tf":2,"sections":2,"score":5.5}],
			"keyphrases":["member benefits"],
			"aliases":["member-benefits-alias"],
			"section_distribution":[
				{"section_id":"section-member-overview","title":"Overview","terms":["member benefits","entitlement","account","active","current","details","extra1","extra2","extra3"],"term_count":9},
				{"section_id":"section-member-api","title":"API","terms":["GET /member/benefits"],"term_count":1},
				{"section_id":"section-extra-1","title":"Extra 1","terms":["extra"],"term_count":1},
				{"section_id":"section-extra-2","title":"Extra 2","terms":["extra"],"term_count":1},
				{"section_id":"section-extra-3","title":"Extra 3","terms":["extra"],"term_count":1},
				{"section_id":"section-extra-4","title":"Extra 4","terms":["extra"],"term_count":1}
			],
			"stats":{"token_count":99,"section_count":6,"unique_term_count":20}
		}`,
		GeneratedFromHash: "hash-doc-member",
	}); err != nil {
		t.Fatalf("UpsertDocumentRetrievalProfile returned error: %v", err)
	}
	responses := runTestServer(t, query.NewService(store), `{"jsonrpc":"2.0","id":"clamp","method":"tools/call","params":{"name":"doc_search","arguments":{"query":"member benefits","max_searches":-4,"max_results":-2,"max_sections_per_document":-1,"profile_detail":"full","max_chars_per_result":-10,"detail":"content"}}}`)
	resp := requireResponse(t, responses, 0, `"clamp"`)
	requireNoRPCError(t, resp)

	var payload struct {
		SearchesUsed int                 `json:"searches_used"`
		Hits         []storage.SearchHit `json:"hits"`
	}
	unmarshalToolText(t, resp, &payload)
	if payload.SearchesUsed < 1 {
		t.Fatalf("searches_used = %d, want at least one search attempt", payload.SearchesUsed)
	}
	if len(payload.Hits) == 0 {
		t.Fatalf("doc_search returned no hits")
	}
	for _, hit := range payload.Hits {
		if len(hit.Content) > 1000 {
			t.Fatalf("hit content len = %d, want default clamp <= 1000", len(hit.Content))
		}
	}
	full, ok := payload.Hits[0].RetrievalProfile.(map[string]any)
	if !ok {
		t.Fatalf("retrieval_profile = %#v, want bounded full profile map", payload.Hits[0].RetrievalProfile)
	}
	rawSections, ok := full["section_distribution"].([]any)
	if !ok || len(rawSections) > 5 {
		t.Fatalf("section_distribution = %#v, want at most 5 entries", full["section_distribution"])
	}
}

func TestProductContextToolCallCompatibility(t *testing.T) {
	responses := runTestServer(t, newTestQueryService(t), `{"jsonrpc":"2.0","id":"ctx","method":"tools/call","params":{"name":"doc_context","arguments":{"task":"Summarize member benefits","max_sections":3,"max_chars":500}}}`)
	resp := requireResponse(t, responses, 0, `"ctx"`)
	requireNoRPCError(t, resp)

	var pack query.ContextPack
	unmarshalToolText(t, resp, &pack)
	if pack.Query != "Summarize member benefits" {
		t.Fatalf("doc_context query = %q, want task fallback", pack.Query)
	}
	if len(pack.Sections) == 0 {
		t.Fatalf("doc_context returned no sections")
	}
}

func TestProductGetNodeToolCall(t *testing.T) {
	queryService, store := newTestGraphServices(t)
	responses := runTestServerWithStore(t, queryService, store, `{"jsonrpc":"2.0","id":"node","method":"tools/call","params":{"name":"doc_get_node","arguments":{"id":"node-product"}}}`)
	resp := requireResponse(t, responses, 0, `"node"`)
	requireNoRPCError(t, resp)

	var node storage.Node
	unmarshalToolText(t, resp, &node)
	if node.ID != "node-product" || node.Kind != "Product" || node.Name != "Membership" {
		t.Fatalf("doc_get_node returned %+v, want product node", node)
	}
	if node.CanonicalName != "membership" || node.MetadataJSON != `{"owner":"growth"}` || node.Confidence != 1 {
		t.Fatalf("doc_get_node details = %+v, want canonical metadata and confidence", node)
	}
}

func TestProductRelatedToolCall(t *testing.T) {
	queryService, store := newTestGraphServices(t)
	responses := runTestServerWithStore(t, queryService, store, `{"jsonrpc":"2.0","id":"related","method":"tools/call","params":{"name":"doc_related","arguments":{"id":"node-product","direction":"out","kind":"exposes_api","limit":5}}}`)
	resp := requireResponse(t, responses, 0, `"related"`)
	requireNoRPCError(t, resp)

	var payload struct {
		Related []storage.RelatedNode `json:"related"`
	}
	unmarshalToolText(t, resp, &payload)
	if len(payload.Related) != 1 {
		t.Fatalf("doc_related returned %d nodes, want 1", len(payload.Related))
	}
	related := payload.Related[0]
	if related.Direction != "out" || related.Node.ID != "node-api" || related.Edge.Kind != "exposes_api" {
		t.Fatalf("doc_related returned %+v, want outgoing API relation", related)
	}
	if related.Edge.ID != "edge-product-api" || related.Edge.EvidenceSectionID != "section-member-api" {
		t.Fatalf("doc_related edge = %+v, want seeded edge with evidence section", related.Edge)
	}
	if related.Edge.Confidence != 0.9 || related.Edge.Provenance != "rule" {
		t.Fatalf("doc_related edge details = %+v, want rule confidence", related.Edge)
	}
}

func TestProductImpactToolCall(t *testing.T) {
	queryService, store := newTestGraphServices(t)
	responses := runTestServerWithStore(t, queryService, store, `{"jsonrpc":"2.0","id":"impact","method":"tools/call","params":{"name":"doc_impact","arguments":{"id":"node-product","direction":"out","max_depth":2,"limit":10}}}`)
	resp := requireResponse(t, responses, 0, `"impact"`)
	requireNoRPCError(t, resp)

	var result storage.ImpactResult
	unmarshalToolText(t, resp, &result)
	if result.StartNode.ID != "node-product" {
		t.Fatalf("doc_impact start node = %+v, want node-product", result.StartNode)
	}
	if len(result.Paths) != 2 {
		t.Fatalf("doc_impact returned %d paths, want one/two-hop paths: %+v", len(result.Paths), result.Paths)
	}
	foundTwoHop := false
	for _, path := range result.Paths {
		if len(path.Nodes) != len(path.Edges)+1 {
			t.Fatalf("doc_impact path = %+v, want path nodes and edges", path)
		}
		for _, edge := range path.Edges {
			if edge.EvidenceSectionID != "section-member-api" || edge.Provenance != "rule" {
				t.Fatalf("doc_impact edge = %+v, want evidence and provenance", edge)
			}
		}
		if len(path.Edges) == 2 && path.Nodes[2].ID == "node-module" {
			foundTwoHop = true
		}
	}
	if !foundTwoHop {
		t.Fatalf("doc_impact paths = %+v, want two-hop module path", result.Paths)
	}
}

func TestUnknownToolError(t *testing.T) {
	responses := runTestServer(t, newTestQueryService(t), `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"missing_tool","arguments":{}}}`)
	resp := requireResponse(t, responses, 0, "3")
	if resp.Error == nil {
		t.Fatalf("unknown tool error is nil")
	}
	if resp.Error.Code != -32602 {
		t.Fatalf("unknown tool error code = %d, want -32602", resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, `unknown tool "missing_tool"`) {
		t.Fatalf("unknown tool error message = %q, want unknown tool", resp.Error.Message)
	}
}

func TestInvalidMethodError(t *testing.T) {
	responses := runTestServer(t, newTestQueryService(t), `{"jsonrpc":"2.0","id":4,"method":"resources/list"}`)
	resp := requireResponse(t, responses, 0, "4")
	if resp.Error == nil {
		t.Fatalf("invalid method error is nil")
	}
	if resp.Error.Code != -32601 {
		t.Fatalf("invalid method error code = %d, want -32601", resp.Error.Code)
	}
	if resp.Error.Message != "method not found" {
		t.Fatalf("invalid method error message = %q, want method not found", resp.Error.Message)
	}
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *ResponseError  `json:"error,omitempty"`
}

func runTestServer(t *testing.T, queryService *query.Service, messages ...string) []rpcResponse {
	t.Helper()

	input := bytes.NewBufferString(strings.Join(messages, "\n") + "\n")
	var output bytes.Buffer
	server := NewServer(queryService, input, &output)
	if err := server.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	var responses []rpcResponse
	scanner := bufio.NewScanner(&output)
	for scanner.Scan() {
		var resp rpcResponse
		if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
			t.Fatalf("decode response line %q: %v", scanner.Text(), err)
		}
		responses = append(responses, resp)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan responses: %v", err)
	}
	return responses
}

func runTestServerWithStore(t *testing.T, queryService *query.Service, store storage.Store, messages ...string) []rpcResponse {
	t.Helper()

	input := bytes.NewBufferString(strings.Join(messages, "\n") + "\n")
	var output bytes.Buffer
	server := NewServerWithStore(queryService, store, input, &output)
	if err := server.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	var responses []rpcResponse
	scanner := bufio.NewScanner(&output)
	for scanner.Scan() {
		var resp rpcResponse
		if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
			t.Fatalf("decode response line %q: %v", scanner.Text(), err)
		}
		responses = append(responses, resp)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan responses: %v", err)
	}
	return responses
}

func newTestQueryService(t *testing.T) *query.Service {
	t.Helper()
	return query.NewService(newTestStore(t))
}

func newTestGraphServices(t *testing.T) (*query.Service, storage.Store) {
	t.Helper()
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.UpsertNode(ctx, storage.NodeInput{
		ID:            "node-product",
		Kind:          "Product",
		Name:          "Membership",
		CanonicalName: "membership",
		MetadataJSON:  `{"owner":"growth"}`,
		Confidence:    1,
	}); err != nil {
		t.Fatalf("upsert product node: %v", err)
	}
	if err := store.UpsertNode(ctx, storage.NodeInput{
		ID:            "node-api",
		Kind:          "API",
		Name:          "GET /member/benefits",
		CanonicalName: "get /member/benefits",
		MetadataJSON:  "{}",
		Confidence:    0.94,
	}); err != nil {
		t.Fatalf("upsert api node: %v", err)
	}
	if err := store.UpsertNode(ctx, storage.NodeInput{
		ID:            "node-module",
		Kind:          "Module",
		Name:          "Entitlements",
		CanonicalName: "entitlements",
		MetadataJSON:  "{}",
		Confidence:    0.88,
	}); err != nil {
		t.Fatalf("upsert module node: %v", err)
	}
	if err := store.UpsertEdge(ctx, storage.EdgeInput{
		ID:                "edge-product-api",
		SrcID:             "node-product",
		DstID:             "node-api",
		Kind:              "exposes_api",
		Confidence:        0.9,
		Provenance:        "rule",
		EvidenceSectionID: "section-member-api",
	}); err != nil {
		t.Fatalf("upsert graph edge: %v", err)
	}
	if err := store.UpsertEdge(ctx, storage.EdgeInput{
		ID:                "edge-api-module",
		SrcID:             "node-api",
		DstID:             "node-module",
		Kind:              "describes",
		Confidence:        0.87,
		Provenance:        "rule",
		EvidenceSectionID: "section-member-api",
	}); err != nil {
		t.Fatalf("upsert api module edge: %v", err)
	}
	if err := store.UpsertEdge(ctx, storage.EdgeInput{
		ID:                "edge-module-product",
		SrcID:             "node-module",
		DstID:             "node-product",
		Kind:              "depends_on",
		Confidence:        0.86,
		Provenance:        "rule",
		EvidenceSectionID: "section-member-api",
	}); err != nil {
		t.Fatalf("upsert module product edge: %v", err)
	}
	return query.NewService(store), store
}

func newTestStore(t *testing.T) storage.Store {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "docgraph.db")
	store, err := storage.Open(ctx, "sqlite://"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
	})
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate store: %v", err)
	}
	if _, err := store.CreateSource(ctx, domain.Source{
		ID:   "source-docs",
		Kind: "local",
		Name: "Product Docs",
		DSN:  "file:///docs",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	if err := store.ReplaceDocument(ctx, domain.DocumentInput{
		ID:          "doc-member",
		SourceID:    "source-docs",
		ExternalID:  "member.md",
		Title:       "Member Benefits",
		URL:         "file:///docs/member.md",
		Version:     "v1",
		ContentHash: "hash-doc-member",
	}, []domain.SectionInput{
		{
			ID:          "section-member-overview",
			DocumentID:  "doc-member",
			HeadingPath: "Member Benefits > Overview",
			Title:       "Overview",
			Content:     "The member benefits page shows entitlement details and current member benefits for each account.",
			ContentHash: "hash-section-overview",
			Ordinal:     0,
		},
		{
			ID:          "section-member-api",
			DocumentID:  "doc-member",
			HeadingPath: "Member Benefits > API",
			Title:       "API",
			Content:     "GET /member/benefits returns active member benefits.",
			ContentHash: "hash-section-api",
			Ordinal:     1,
		},
	}); err != nil {
		t.Fatalf("replace document: %v", err)
	}
	return store
}

func seedMCPExplicitReferenceDocument(t *testing.T, store storage.Store) {
	t.Helper()
	ctx := context.Background()
	if err := store.ReplaceDocument(ctx, domain.DocumentInput{
		ID:          "doc-explicit",
		SourceID:    "source-docs",
		ExternalID:  "schema.md",
		Title:       "Schema",
		URL:         "file:///docs/schema.md",
		Version:     "v1",
		ContentHash: "hash-doc-explicit",
	}, []domain.SectionInput{
		{
			ID:          "section-explicit-source",
			DocumentID:  "doc-explicit",
			HeadingPath: "Schema > Operation",
			Title:       "Operation",
			Content:     "When configuring operations, detailed rules see §5 Propagation propagation rules.",
			ContentHash: "hash-section-explicit-source",
			Ordinal:     0,
		},
		{
			ID:          "section-explicit-propagation",
			DocumentID:  "doc-explicit",
			HeadingPath: "Schema > 5 Propagation propagation rules",
			Title:       "5 Propagation propagation rules",
			Content:     "Propagation propagation rules apply to existing roles.",
			ContentHash: "hash-section-explicit-propagation",
			Ordinal:     1,
		},
	}); err != nil {
		t.Fatalf("replace explicit reference document: %v", err)
	}
}

func findMCPSearchHit(hits []storage.SearchHit, sectionID string) *storage.SearchHit {
	for i := range hits {
		if hits[i].SectionID == sectionID {
			return &hits[i]
		}
	}
	return nil
}

func jsonArrayContains(values []any, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func requireResponse(t *testing.T, responses []rpcResponse, index int, wantID string) rpcResponse {
	t.Helper()

	if len(responses) <= index {
		t.Fatalf("response count = %d, want index %d; responses: %+v", len(responses), index, responses)
	}
	resp := responses[index]
	if resp.JSONRPC != "2.0" {
		t.Fatalf("response JSONRPC = %q, want 2.0", resp.JSONRPC)
	}
	if string(resp.ID) != wantID {
		t.Fatalf("response ID = %s, want %s", string(resp.ID), wantID)
	}
	return resp
}

func requireNoRPCError(t *testing.T, resp rpcResponse) {
	t.Helper()

	if resp.Error != nil {
		t.Fatalf("unexpected RPC error: %+v", resp.Error)
	}
	if len(resp.Result) == 0 {
		t.Fatalf("response result is empty")
	}
}

func unmarshalResult(t *testing.T, resp rpcResponse, dst any) {
	t.Helper()

	if err := json.Unmarshal(resp.Result, dst); err != nil {
		t.Fatalf("unmarshal result %s: %v", string(resp.Result), err)
	}
}

func unmarshalToolText(t *testing.T, resp rpcResponse, dst any) {
	t.Helper()

	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	unmarshalResult(t, resp, &result)
	if len(result.Content) != 1 {
		t.Fatalf("tool content len = %d, want 1", len(result.Content))
	}
	if result.Content[0].Type != "text" {
		t.Fatalf("tool content type = %q, want text", result.Content[0].Type)
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), dst); err != nil {
		t.Fatalf("unmarshal tool text %q: %v", result.Content[0].Text, err)
	}
}
