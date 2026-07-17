package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docgraph/docgraph/internal/domain"
	"github.com/docgraph/docgraph/internal/embedding"
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
			for _, name := range []string{"exact_terms", "semantic_intents"} {
				if _, ok := properties[name].(map[string]any); !ok {
					t.Fatalf("doc_search properties missing %q: %#v", name, properties)
				}
			}
			for name, maximum := range map[string]float64{
				"max_media_assets_per_result": maxMCPMediaPerResult,
				"max_media_assets_total":      maxMCPMediaTotal,
			} {
				schema, ok := properties[name].(map[string]any)
				if !ok || schema["maximum"] != maximum {
					t.Fatalf("doc_search %s schema = %#v, want maximum %v", name, properties[name], maximum)
				}
			}
			mediaDetail, ok := properties["media_detail"].(map[string]any)
			if !ok || !jsonArrayContains(mediaDetail["enum"].([]any), "none") || !jsonArrayContains(mediaDetail["enum"].([]any), "compact") {
				t.Fatalf("doc_search media_detail schema = %#v", properties["media_detail"])
			}
			for _, name := range []string{"use_vector_search", "vector_weight", "chunk_size", "batch_size"} {
				if _, ok := properties[name]; ok {
					t.Fatalf("doc_search exposes internal vector parameter %q: %#v", name, properties)
				}
			}
		}
		if tool.Name == "doc_get_asset_uri" {
			if tool.Annotations["readOnlyHint"] != true || tool.Annotations["openWorldHint"] != false {
				t.Fatalf("doc_get_asset_uri annotations = %#v", tool.Annotations)
			}
			properties, ok := tool.InputSchema["properties"].(map[string]any)
			if !ok {
				t.Fatalf("doc_get_asset_uri properties = %#v", tool.InputSchema["properties"])
			}
			if len(properties) != 1 || properties["id"] == nil {
				t.Fatalf("doc_get_asset_uri properties = %#v, want id only", properties)
			}
			required, ok := tool.InputSchema["required"].([]any)
			if !ok || !jsonArrayContains(required, "id") {
				t.Fatalf("doc_get_asset_uri required = %#v, want id", tool.InputSchema["required"])
			}
		}
	}
	for _, name := range []string{"doc_search", "doc_get_section", "doc_get_asset_uri", "doc_get_node", "doc_related", "doc_impact"} {
		if !gotNames[name] {
			t.Fatalf("tools/list missing %q in %+v", name, result.Tools)
		}
	}
	if gotNames["doc_context"] {
		t.Fatalf("tools/list includes disabled doc_context in %+v", result.Tools)
	}
	if gotNames["doc_get_asset"] {
		t.Fatalf("tools/list includes legacy doc_get_asset in %+v", result.Tools)
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

func TestDocSearchFallsBackWhenEmbeddingFails(t *testing.T) {
	store := newTestStore(t)
	queryService := query.NewService(store)
	responses := runTestServerWithEmbedding(t, queryService, store, mcpFailingEmbedder{}, `{"jsonrpc":"2.0","id":"fallback","method":"tools/call","params":{"name":"doc_search","arguments":{"query":"member benefits","limit":5}}}`)
	resp := requireResponse(t, responses, 0, `"fallback"`)
	requireNoRPCError(t, resp)

	var payload struct {
		Attempts []storage.SearchAttempt `json:"attempts"`
		Hits     []storage.SearchHit     `json:"hits"`
	}
	unmarshalToolText(t, resp, &payload)
	if len(payload.Hits) == 0 || payload.Hits[0].DocumentID != "doc-member" {
		t.Fatalf("doc_search hits = %+v, want lexical fallback hit", payload.Hits)
	}
	if !hasMCPAttemptError(payload.Attempts, "vector", "embedding_failed") {
		t.Fatalf("doc_search attempts = %+v, want vector embedding_failed attempt", payload.Attempts)
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

func TestDocSearchAndGetSectionExposeTopicEvidence(t *testing.T) {
	store := newTestStore(t)
	seedMCPTopicWorkbook(t, store)
	queryService := query.NewService(store)
	responses := runTestServerWithStore(t, queryService, store,
		`{"jsonrpc":"2.0","id":"topic-search","method":"tools/call","params":{"name":"doc_search","arguments":{"query":"mcp-topic-evidence","max_results":5,"max_sections_per_document":5}}}`,
		`{"jsonrpc":"2.0","id":"topic-section","method":"tools/call","params":{"name":"doc_get_section","arguments":{"id":"mcp-topic-child"}}}`,
		`{"jsonrpc":"2.0","id":"topic-section-limited-assets","method":"tools/call","params":{"name":"doc_get_section","arguments":{"id":"mcp-topic-child","max_assets":2}}}`,
		`{"jsonrpc":"2.0","id":"topic-search-hard-limit","method":"tools/call","params":{"name":"doc_search","arguments":{"query":"mcp-topic-evidence","max_results":5,"max_sections_per_document":5,"max_media_assets_per_result":999,"max_media_assets_total":999,"media_detail":"full"}}}`,
	)

	searchResponse := requireResponse(t, responses, 0, `"topic-search"`)
	requireNoRPCError(t, searchResponse)
	var searchPayload struct {
		Hits           []storage.SearchHit         `json:"hits"`
		MediaSummary   *storage.SearchMediaSummary `json:"media_summary"`
		SuggestedReads struct {
			CuratedRelations  []domain.SuggestedRead `json:"curated_relations"`
			AuthoredRelations []domain.SuggestedRead `json:"authored_relations"`
		} `json:"suggested_reads"`
	}
	unmarshalToolText(t, searchResponse, &searchPayload)
	hit := findMCPSearchHit(searchPayload.Hits, "mcp-topic-child")
	if hit == nil || hit.NodeID != "node-mcp-topic-child" || hit.ElementKind != "topic" || hit.DisplayNumber != "1.1" {
		t.Fatalf("topic search hit = %+v", hit)
	}
	if len(hit.MediaAssets) != defaultMCPMediaPerResult || hit.MediaAssets[0].ID != "mcp-asset-diagram" || hit.MediaAssetsTotal != 12 || !hit.MediaAssetsTruncated {
		t.Fatalf("topic search media = %+v", hit.MediaAssets)
	}
	for _, asset := range hit.MediaAssets {
		if asset.MetadataJSON != "" || asset.ReferenceMetadataJSON != "" {
			t.Fatalf("topic search leaked raw media metadata: %+v", asset)
		}
	}
	if hit.EvidenceKind != "media_metadata" {
		t.Fatalf("topic search evidence kind = %q, want media_metadata from omitted media metadata", hit.EvidenceKind)
	}
	if searchPayload.MediaSummary == nil || searchPayload.MediaSummary.Total != 12 || searchPayload.MediaSummary.Returned != defaultMCPMediaPerResult || !searchPayload.MediaSummary.Truncated || searchPayload.MediaSummary.PerResultLimit != defaultMCPMediaPerResult || searchPayload.MediaSummary.TotalLimit != defaultMCPMediaTotal || searchPayload.MediaSummary.Detail != "compact" {
		t.Fatalf("default MCP media summary = %+v", searchPayload.MediaSummary)
	}
	if len(searchPayload.SuggestedReads.AuthoredRelations) != 1 || len(searchPayload.SuggestedReads.AuthoredRelations) > 5 {
		t.Fatalf("authored suggestions = %+v", searchPayload.SuggestedReads.AuthoredRelations)
	}
	if len(searchPayload.SuggestedReads.CuratedRelations) != 0 {
		t.Fatalf("curated suggestions = %+v, want source-authored relation separate", searchPayload.SuggestedReads.CuratedRelations)
	}

	sectionResponse := requireResponse(t, responses, 1, `"topic-section"`)
	requireNoRPCError(t, sectionResponse)
	var section sectionToolResult
	unmarshalToolText(t, sectionResponse, &section)
	if section.SectionID != "mcp-topic-child" || section.NodeID != "node-mcp-topic-child" || section.ElementKind != "topic" || section.DisplayNumber != "1.1" {
		t.Fatalf("doc_get_section topic identity = %+v", section)
	}
	if len(section.Ancestry) != 1 || section.Ancestry[0].ID != "mcp-topic-root" || len(section.MediaAssets) != 12 {
		t.Fatalf("doc_get_section context = ancestry:%+v media:%+v", section.Ancestry, section.MediaAssets)
	}
	if section.MediaAssetsTotal != 12 || section.MediaAssetsTruncated {
		t.Fatalf("doc_get_section media budget = total:%d truncated:%v", section.MediaAssetsTotal, section.MediaAssetsTruncated)
	}
	for _, asset := range section.MediaAssets {
		if asset.MetadataJSON != "" || asset.ReferenceMetadataJSON != "" {
			t.Fatalf("doc_get_section leaked connector media metadata: %+v", asset)
		}
	}
	if len(section.AuthoredRelations) == 0 || len(section.AuthoredRelations) > maxMCPSectionAuthoredRelations {
		t.Fatalf("doc_get_section authored relations = %+v", section.AuthoredRelations)
	}
	if section.Snapshot == nil || section.Snapshot.ID != "mcp-snapshot-topic" || section.Source == nil || section.Source.ID != "mcp-source-topic" {
		t.Fatalf("doc_get_section provenance envelope = snapshot:%+v source:%+v", section.Snapshot, section.Source)
	}
	if section.Provenance.SourceElementID != "mcp-xmind-child" || section.Provenance.FormatFamily != "classic_json" {
		t.Fatalf("doc_get_section provenance = %+v", section.Provenance)
	}

	limitedSectionResponse := requireResponse(t, responses, 2, `"topic-section-limited-assets"`)
	requireNoRPCError(t, limitedSectionResponse)
	var limitedSection sectionToolResult
	unmarshalToolText(t, limitedSectionResponse, &limitedSection)
	if len(limitedSection.MediaAssets) != 2 || limitedSection.MediaAssetsTotal != 12 || !limitedSection.MediaAssetsTruncated {
		t.Fatalf("limited doc_get_section media = returned:%d total:%d truncated:%v", len(limitedSection.MediaAssets), limitedSection.MediaAssetsTotal, limitedSection.MediaAssetsTruncated)
	}

	hardLimitResponse := requireResponse(t, responses, 3, `"topic-search-hard-limit"`)
	requireNoRPCError(t, hardLimitResponse)
	var hardLimitPayload struct {
		Hits         []storage.SearchHit         `json:"hits"`
		MediaSummary *storage.SearchMediaSummary `json:"media_summary"`
	}
	unmarshalToolText(t, hardLimitResponse, &hardLimitPayload)
	hardLimitHit := findMCPSearchHit(hardLimitPayload.Hits, "mcp-topic-child")
	if hardLimitHit == nil || len(hardLimitHit.MediaAssets) != maxMCPMediaPerResult || hardLimitHit.MediaAssetsTotal != 12 || !hardLimitHit.MediaAssetsTruncated {
		t.Fatalf("hard-limited MCP search hit = %+v", hardLimitHit)
	}
	for _, asset := range hardLimitHit.MediaAssets {
		if asset.MetadataJSON != "" || asset.ReferenceMetadataJSON != "" {
			t.Fatalf("hard-limited MCP search leaked raw media metadata: %+v", asset)
		}
	}
	if hardLimitPayload.MediaSummary == nil || hardLimitPayload.MediaSummary.PerResultLimit != maxMCPMediaPerResult || hardLimitPayload.MediaSummary.TotalLimit != maxMCPMediaTotal || hardLimitPayload.MediaSummary.Returned != maxMCPMediaPerResult || hardLimitPayload.MediaSummary.Detail != "compact" {
		t.Fatalf("hard-limited MCP media summary = %+v", hardLimitPayload.MediaSummary)
	}
}

func TestDocGetAssetURIReturnsMetadataWithoutBinaryContent(t *testing.T) {
	store := newTestStore(t)
	seedMCPTopicWorkbook(t, store)
	responses := runTestServerWithStore(t, query.NewService(store), store,
		`{"jsonrpc":"2.0","id":"asset-uri","method":"tools/call","params":{"name":"doc_get_asset_uri","arguments":{"id":"mcp-asset-diagram"}}}`,
		`{"jsonrpc":"2.0","id":"asset-uri-unavailable","method":"tools/call","params":{"name":"doc_get_asset_uri","arguments":{"id":"mcp-asset-extra-01"}}}`,
	)
	for index, id := range []string{`"asset-uri"`} {
		response := requireResponse(t, responses, index, id)
		requireNoRPCError(t, response)
		var payload struct {
			AssetID     string `json:"asset_id"`
			DownloadURI string `json:"download_uri"`
			MIMEType    string `json:"mime_type"`
			Kind        string `json:"kind"`
			Auth        string `json:"auth"`
		}
		unmarshalToolText(t, response, &payload)
		if payload.AssetID != "mcp-asset-diagram" || payload.DownloadURI != "/api/media-assets/mcp-asset-diagram/content" || payload.MIMEType != "image/png" || payload.Kind != "image" || payload.Auth != "same_as_docgraph" {
			t.Fatalf("doc_get_asset_uri payload = %+v", payload)
		}
		encoded, _ := json.Marshal(response.Result)
		if strings.Contains(string(encoded), "blob_sha256") || strings.Contains(string(encoded), `"opaque"`) || strings.Contains(string(encoded), `"data"`) || strings.Contains(string(encoded), `"blob"`) {
			t.Fatalf("doc_get_asset_uri leaked binary or internal metadata: %s", encoded)
		}
	}
	unavailable := requireResponse(t, responses, 1, `"asset-uri-unavailable"`)
	if unavailable.Error == nil || !strings.Contains(unavailable.Error.Message, "has no preserved content") {
		t.Fatalf("unavailable asset URI response = %+v", unavailable)
	}

	handler := NewHandler(query.NewService(store), store)
	handler.SetAssetURIBasePath("/docgraph/")
	if got := handler.assetContentURI("asset/with space"); got != "/docgraph/api/media-assets/asset%2Fwith%20space/content" {
		t.Fatalf("prefixed asset URI = %q", got)
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

func runTestServerWithEmbedding(t *testing.T, queryService *query.Service, store storage.Store, embedder embedding.Embedder, messages ...string) []rpcResponse {
	t.Helper()

	input := bytes.NewBufferString(strings.Join(messages, "\n") + "\n")
	var output bytes.Buffer
	server := NewServerWithStoreAndEmbedding(queryService, store, embedder, "test-generator", 0.4, input, &output)
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

type mcpFailingEmbedder struct{}

func (mcpFailingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return nil, fmt.Errorf("embedding unavailable")
}

func (mcpFailingEmbedder) Model() string {
	return "test-embedding"
}

func (mcpFailingEmbedder) Dimensions() int {
	return 2
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

func seedMCPTopicWorkbook(t *testing.T, store storage.Store) {
	seedMCPTopicWorkbookWithImage(t, store, strings.Repeat("d", 64), 42, "image/png")
}

func seedMCPTopicWorkbookWithImage(t *testing.T, store storage.Store, imageDigest string, imageSize int64, imageMediaType string) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.CreateSource(ctx, domain.Source{ID: "mcp-source-topic", Kind: "xmind", Name: "MCP Architecture", DSN: "/maps/mcp.xmind"}); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("c", 64)
	bundle := domain.WorkbookBundle{
		SourceID:    "mcp-source-topic",
		WorkbookKey: "mcp-workbook",
		Snapshot: domain.SourceSnapshotInput{
			ID: "mcp-snapshot-topic", SourceID: "mcp-source-topic", SourceHash: "mcp-snapshot-hash", BlobSHA256: digest,
			FormatFamily: "classic_json", FormatVersion: "1",
		},
		Blobs: []domain.MediaBlobInput{
			{SHA256: digest, SizeBytes: 100, SniffedMediaType: "application/zip", StorageKey: "sha256/" + digest},
			{SHA256: imageDigest, SizeBytes: imageSize, SniffedMediaType: imageMediaType, StorageKey: "sha256/" + imageDigest},
		},
		Documents: []domain.WorkbookDocumentInput{{
			Document: domain.DocumentInput{ID: "mcp-sheet-topic", SourceID: "mcp-source-topic", ExternalID: "mcp-sheet-topic", Title: "MCP Architecture", ContentHash: "mcp-sheet-hash"},
			Sections: []domain.SectionInput{
				{ID: "mcp-topic-root", Title: "Root", Content: "root context", SearchText: "root context", ContentHash: "mcp-root-hash", Structure: &domain.SectionStructureInput{SourceElementID: "mcp-xmind-root", ElementKind: "topic", Depth: 0, SiblingOrdinal: 0, OrderPath: []int{0}, DisplayNumber: "1"}},
				{ID: "mcp-topic-child", Title: "Child", Content: "mcp-topic-evidence details", SearchText: "mcp-topic-evidence details diagram.png", ContentHash: "mcp-child-hash", Structure: &domain.SectionStructureInput{ParentSectionID: "mcp-topic-root", SourceElementID: "mcp-xmind-child", ElementKind: "topic", Depth: 1, SiblingOrdinal: 0, OrderPath: []int{0, 0}, DisplayNumber: "1.1"}},
			},
		}},
		Nodes: []domain.NodeInput{
			{ID: "node-mcp-topic-root", Kind: "DocSection", Name: "Root", CanonicalName: "mcp-root", MetadataJSON: `{"section_id":"mcp-topic-root","document_id":"mcp-sheet-topic"}`},
			{ID: "node-mcp-topic-child", Kind: "DocSection", Name: "Child", CanonicalName: "mcp-child", MetadataJSON: `{"section_id":"mcp-topic-child","document_id":"mcp-sheet-topic"}`},
		},
		SectionNodes: []domain.SectionNodeInput{
			{SectionID: "mcp-topic-root", NodeID: "node-mcp-topic-root", Role: "represents"},
			{SectionID: "mcp-topic-child", NodeID: "node-mcp-topic-child", Role: "represents"},
		},
		Edges: []domain.EdgeInput{
			{ID: "mcp-edge-hierarchy", SrcID: "node-mcp-topic-root", DstID: "node-mcp-topic-child", Kind: "contains", Provenance: "source_authored"},
			{ID: "mcp-edge-related", SrcID: "node-mcp-topic-child", DstID: "node-mcp-topic-root", Kind: "related_to", Provenance: "source_authored", Confidence: 0.9, MetadataJSON: `{"label":"review"}`},
		},
		MediaAssets: []domain.MediaAssetInput{{ID: "mcp-asset-diagram", SourceID: "mcp-source-topic", SnapshotID: "mcp-snapshot-topic", DocumentID: "mcp-sheet-topic", ExternalID: "diagram", BlobSHA256: imageDigest, Kind: "image", OriginalName: "diagram.png", MediaType: imageMediaType, SizeBytes: imageSize, Status: "available", MetadataJSON: `{"opaque":"first"}`}},
		MediaRefs:   []domain.SectionMediaRefInput{{SectionID: "mcp-topic-child", AssetID: "mcp-asset-diagram", Role: "image", Ordinal: 0}},
	}
	for i := 1; i < 12; i++ {
		metadata := fmt.Sprintf(`{"opaque":"asset-%d"}`, i)
		if i == 11 {
			metadata = `{"opaque":"mcp-topic-evidence"}`
		}
		assetID := fmt.Sprintf("mcp-asset-extra-%02d", i)
		bundle.MediaAssets = append(bundle.MediaAssets, domain.MediaAssetInput{
			ID: assetID, SourceID: "mcp-source-topic", SnapshotID: "mcp-snapshot-topic", DocumentID: "mcp-sheet-topic",
			ExternalID: fmt.Sprintf("extra-%02d", i), Kind: "image", OriginalName: fmt.Sprintf("extra-%02d.png", i),
			MediaType: "image/png", Status: "external", MetadataJSON: metadata,
		})
		bundle.MediaRefs = append(bundle.MediaRefs, domain.SectionMediaRefInput{SectionID: "mcp-topic-child", AssetID: assetID, Role: "image", Ordinal: i})
	}
	workbookStore, ok := store.(storage.WorkbookStore)
	if !ok {
		t.Fatal("store does not implement WorkbookStore")
	}
	if _, err := workbookStore.ReplaceWorkbookBundle(ctx, bundle); err != nil {
		t.Fatal(err)
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

func hasMCPAttemptError(attempts []storage.SearchAttempt, kind string, contains string) bool {
	for _, attempt := range attempts {
		if attempt.Kind == kind && strings.Contains(attempt.Error, contains) {
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
