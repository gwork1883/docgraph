package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docgraph/docgraph/internal/config"
	"github.com/docgraph/docgraph/internal/ids"
	"github.com/docgraph/docgraph/internal/ingest/confluence"
	"github.com/docgraph/docgraph/internal/storage"
)

func TestHealthAndStatusHandlers(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("health status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var health struct {
		Status string `json:"status"`
	}
	decodeJSON(t, rr, &health)
	if health.Status != "ok" {
		t.Fatalf("health status body = %q, want ok", health.Status)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var status storage.Status
	decodeJSON(t, rr, &status)
	if status.Sources != 0 || status.Documents != 0 || status.Sections != 0 {
		t.Fatalf("initial status counts = sources:%d documents:%d sections:%d, want all zero", status.Sources, status.Documents, status.Sections)
	}
	if !strings.HasPrefix(status.StorageDSN, "sqlite://") {
		t.Fatalf("status storage dsn = %q, want sqlite dsn", status.StorageDSN)
	}
}

func TestAuthModeNoneKeepsAPIBehaviorWithoutToken(t *testing.T) {
	handler, cleanup := newTestHandlerWithAuth(t, config.AuthConfig{Mode: "none"})
	defer cleanup()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status without token = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
}

func TestTokenAuthProtectsAPIExceptHealth(t *testing.T) {
	const token = "server-test-token"
	handler, cleanup := newTestHandlerWithAuth(t, config.AuthConfig{Mode: "token", Token: token})
	defer cleanup()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("health without token = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status without token = %d, want %d; body: %s", rr.Code, http.StatusUnauthorized, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status with wrong bearer token = %d, want %d; body: %s", rr.Code, http.StatusUnauthorized, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status with bearer token = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("X-DocGraph-Token", token)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status with X-DocGraph-Token = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("X-ProductGraph-Token", token)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status with legacy X-ProductGraph-Token = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
}

func TestLocalSourceSyncAndSearchHandlers(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	docsDir := t.TempDir()
	writeFile(t, filepath.Join(docsDir, "member.md"), `# Member Benefits

The member benefits overview explains how users see entitlement details.

## API

Use GET /member/benefits to load current member benefits for the account.
`)

	createBody := map[string]any{
		"kind":         "local",
		"name":         "Local Product Docs",
		"dsn":          "file://" + filepath.ToSlash(docsDir),
		"product_hint": "membership",
		"module_hint":  "benefits",
	}
	rr := postJSON(t, handler, "/api/sources", createBody)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create source status = %d, want %d; body: %s", rr.Code, http.StatusCreated, rr.Body.String())
	}
	var created storage.Source
	decodeJSON(t, rr, &created)
	if created.ID == "" {
		t.Fatalf("created source id is empty")
	}
	if created.Kind != "local" || created.Name != "Local Product Docs" || created.ProductHint != "membership" || created.ModuleHint != "benefits" {
		t.Fatalf("created source = %+v, want local source with provided fields", created)
	}

	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/sources", nil)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list sources status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var listed struct {
		Sources []storage.Source `json:"sources"`
	}
	decodeJSON(t, rr, &listed)
	if len(listed.Sources) != 1 {
		t.Fatalf("listed sources len = %d, want 1", len(listed.Sources))
	}
	if listed.Sources[0].ID != created.ID {
		t.Fatalf("listed source id = %q, want %q", listed.Sources[0].ID, created.ID)
	}

	rr = postJSON(t, handler, "/api/sources/"+created.ID+"/sync", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("sync status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var syncResult struct {
		SourceID  string `json:"source_id"`
		Documents int    `json:"documents"`
	}
	decodeJSON(t, rr, &syncResult)
	if syncResult.SourceID != created.ID || syncResult.Documents != 1 {
		t.Fatalf("sync result = %+v, want source %q with 1 document", syncResult, created.ID)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/sources/"+created.ID+"/artifacts?limit=10", nil)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("artifacts status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var artifacts storage.SourceArtifacts
	decodeJSON(t, rr, &artifacts)
	if len(artifacts.Documents) != 1 {
		t.Fatalf("artifacts documents = %+v, want one document", artifacts.Documents)
	}
	docID := artifacts.Documents[0].ID

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/documents/"+docID+"/profile", nil)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get profile status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var profile storage.DocumentProfile
	decodeJSON(t, rr, &profile)
	if profile.DocumentID != docID || profile.RetrievalProfileJSON == "" || profile.GeneratedFromHash == "" {
		t.Fatalf("initial profile = %+v, want generated retrieval profile", profile)
	}

	rr = putJSON(t, handler, "/api/documents/"+docID+"/profile", map[string]any{
		"desc": "Administrator maintained document description",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("put profile status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	decodeJSON(t, rr, &profile)
	if profile.Desc != "Administrator maintained document description" {
		t.Fatalf("updated profile desc = %q, want administrator desc", profile.Desc)
	}

	writeFile(t, filepath.Join(docsDir, "member.md"), `# Member Benefits

The member benefits overview explains how users see entitlement details after a profile edit.

## API

Use GET /member/benefits to load current member benefits for the account.
`)
	rr = postJSON(t, handler, "/api/sources/"+created.ID+"/sync", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("resync status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/documents/"+docID+"/profile", nil)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get profile after resync status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	decodeJSON(t, rr, &profile)
	if profile.Desc != "Administrator maintained document description" {
		t.Fatalf("profile desc after resync = %q, want preserved administrator desc", profile.Desc)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("post-sync status code = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var status storage.Status
	decodeJSON(t, rr, &status)
	if status.Sources != 1 || status.Documents != 1 || status.Sections == 0 {
		t.Fatalf("post-sync status counts = sources:%d documents:%d sections:%d, want 1/1/>0", status.Sources, status.Documents, status.Sections)
	}

	rr = postJSON(t, handler, "/api/search", map[string]any{
		"query": "member benefits",
		"limit": 10,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("search status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var searchResult struct {
		Hits []storage.SearchHit `json:"hits"`
	}
	decodeJSON(t, rr, &searchResult)
	if len(searchResult.Hits) == 0 {
		t.Fatalf("search hits empty, want at least one hit")
	}
	foundAPISection := false
	for _, hit := range searchResult.Hits {
		if strings.Contains(hit.Snippet, "member benefits") || strings.Contains(hit.HeadingPath, "API") || hit.Title == "API" {
			foundAPISection = true
			break
		}
	}
	if !foundAPISection {
		t.Fatalf("search hits = %+v, want hit for member benefits/API content", searchResult.Hits)
	}
}

func TestSourceLifecycleHandlersUpdateJobsAndDelete(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	docsDir := t.TempDir()
	writeFile(t, filepath.Join(docsDir, "member.md"), `# Member Benefits

Unique lifecycle-token content for API source lifecycle tests.
`)

	rr := postJSON(t, handler, "/api/sources", map[string]any{
		"kind":         "local",
		"name":         "Lifecycle Docs",
		"dsn":          "file://" + filepath.ToSlash(docsDir),
		"product_hint": "Membership",
		"module_hint":  "Benefits",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create source status = %d, want %d; body: %s", rr.Code, http.StatusCreated, rr.Body.String())
	}
	var created storage.Source
	decodeJSON(t, rr, &created)

	rr = putJSON(t, handler, "/api/sources/"+created.ID, map[string]any{
		"name":         "Updated Lifecycle Docs",
		"dsn":          "file://" + filepath.ToSlash(docsDir),
		"product_hint": "Payments",
		"module_hint":  "Checkout",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("update source status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var updated storage.Source
	decodeJSON(t, rr, &updated)
	if updated.Name != "Updated Lifecycle Docs" || updated.DSN != "file://"+filepath.ToSlash(docsDir) || updated.ProductHint != "Payments" || updated.ModuleHint != "Checkout" {
		t.Fatalf("updated source = %+v, want changed name/dsn/product/module", updated)
	}

	rr = postJSON(t, handler, "/api/sources/"+created.ID+"/sync", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("sync status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var syncResult struct {
		SourceID  string `json:"source_id"`
		Documents int    `json:"documents"`
		JobID     string `json:"job_id"`
	}
	decodeJSON(t, rr, &syncResult)
	if syncResult.SourceID != created.ID || syncResult.Documents != 1 || syncResult.JobID == "" {
		t.Fatalf("sync result = %+v, want source, document count, and job id", syncResult)
	}

	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/sources/"+created.ID+"/jobs?limit=5", nil)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("jobs status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var jobsBody struct {
		Jobs []storage.SyncJob `json:"jobs"`
	}
	decodeJSON(t, rr, &jobsBody)
	if len(jobsBody.Jobs) != 1 || jobsBody.Jobs[0].Status != "completed" || !strings.Contains(jobsBody.Jobs[0].PayloadJSON, `"documents":1`) {
		t.Fatalf("jobs response = %+v, want completed sync job", jobsBody.Jobs)
	}

	rr = postJSON(t, handler, "/api/search", map[string]any{
		"query": "lifecycle-token",
		"limit": 10,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("search before delete status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var searchResult struct {
		Hits []storage.SearchHit `json:"hits"`
	}
	decodeJSON(t, rr, &searchResult)
	if len(searchResult.Hits) == 0 {
		t.Fatalf("search before delete returned no hits")
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/api/sources/"+created.ID, nil)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/sources", nil)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list after delete status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var listed struct {
		Sources []storage.Source `json:"sources"`
	}
	decodeJSON(t, rr, &listed)
	if len(listed.Sources) != 0 {
		t.Fatalf("sources after delete = %+v, want empty", listed.Sources)
	}

	rr = postJSON(t, handler, "/api/search", map[string]any{
		"query": "lifecycle-token",
		"limit": 10,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("search after delete status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	decodeJSON(t, rr, &searchResult)
	if len(searchResult.Hits) != 0 {
		t.Fatalf("search after delete hits = %+v, want empty", searchResult.Hits)
	}
}

func TestUpdateSourceClearsEmptyConfigFields(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	rr := postJSON(t, handler, "/api/sources", map[string]any{
		"kind":        "webdocs",
		"name":        "Web Docs",
		"dsn":         "https://docs.example/docs",
		"config_json": `{"max_pages":"1","max_depth":"3"}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create source status = %d, want %d; body: %s", rr.Code, http.StatusCreated, rr.Body.String())
	}
	var created storage.Source
	decodeJSON(t, rr, &created)

	rr = putJSON(t, handler, "/api/sources/"+created.ID, map[string]any{
		"max_pages": "",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("update source status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var updated storage.Source
	decodeJSON(t, rr, &updated)

	var config map[string]string
	if err := json.Unmarshal([]byte(updated.ConfigJSON), &config); err != nil {
		t.Fatalf("unmarshal updated config %q: %v", updated.ConfigJSON, err)
	}
	if _, ok := config["max_pages"]; ok {
		t.Fatalf("updated config = %s, want max_pages cleared", updated.ConfigJSON)
	}
	if config["max_depth"] != "3" {
		t.Fatalf("updated config = %s, want max_depth preserved", updated.ConfigJSON)
	}
}

func TestCreateSourceSyncsAndSearchesOpenAPIKind(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	specPath := filepath.Join(t.TempDir(), "membership.openapi.json")
	writeFile(t, specPath, `{
  "openapi": "3.0.3",
  "info": {"title": "Membership API"},
  "paths": {
    "/member/benefits": {
      "get": {
        "operationId": "getMemberBenefits",
        "summary": "List member benefits",
        "description": "Returns available benefits for a member account."
      }
    }
  }
}`)

	rr := postJSON(t, handler, "/api/sources", map[string]any{
		"kind": "openapi",
		"name": "Membership API",
		"dsn":  "file://" + filepath.ToSlash(specPath),
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create source status = %d, want %d; body: %s", rr.Code, http.StatusCreated, rr.Body.String())
	}

	var created storage.Source
	decodeJSON(t, rr, &created)
	if created.Kind != "openapi" {
		t.Fatalf("created Kind = %q, want openapi", created.Kind)
	}
	if created.Name != "Membership API" {
		t.Fatalf("created Name = %q, want Membership API", created.Name)
	}
	if created.DSN != "file://"+filepath.ToSlash(specPath) {
		t.Fatalf("created DSN = %q, want file DSN", created.DSN)
	}

	rr = postJSON(t, handler, "/api/sources/"+created.ID+"/sync", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("sync status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	rr = postJSON(t, handler, "/api/search", map[string]any{
		"query": "getMemberBenefits",
		"limit": 10,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("search status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var searchResult struct {
		Hits []storage.SearchHit `json:"hits"`
	}
	decodeJSON(t, rr, &searchResult)
	if len(searchResult.Hits) == 0 {
		t.Fatalf("search hits empty, want OpenAPI endpoint hit")
	}
	if searchResult.Hits[0].DocumentTitle != "Membership API" {
		t.Fatalf("search DocumentTitle = %q, want Membership API", searchResult.Hits[0].DocumentTitle)
	}
	if !strings.Contains(searchResult.Hits[0].Content, "GET /member/benefits") {
		t.Fatalf("search hit Content = %q, want endpoint content", searchResult.Hits[0].Content)
	}
}

func TestCreateSourceSyncsOpenAPIYAMLAndReturnsGraphAPI(t *testing.T) {
	handler, _, cleanup := newTestHandlerWithStore(t)
	defer cleanup()

	specPath := filepath.Join(t.TempDir(), "membership.openapi.yaml")
	writeFile(t, specPath, openAPIServerYAMLFixture)

	rr := postJSON(t, handler, "/api/sources", map[string]any{
		"kind": "openapi",
		"name": "Membership API",
		"dsn":  "file://" + filepath.ToSlash(specPath),
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create source status = %d, want %d; body: %s", rr.Code, http.StatusCreated, rr.Body.String())
	}
	var created storage.Source
	decodeJSON(t, rr, &created)

	rr = postJSON(t, handler, "/api/sources/"+created.ID+"/sync", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("sync status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	rr = postJSON(t, handler, "/api/search", map[string]any{
		"query": "Member identifier from shared components",
		"limit": 10,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("search status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var searchResult struct {
		Hits []storage.SearchHit `json:"hits"`
	}
	decodeJSON(t, rr, &searchResult)
	if len(searchResult.Hits) == 0 || !strings.Contains(searchResult.Hits[0].Content, "GET /member/benefits") || !strings.Contains(searchResult.Hits[0].Content, "memberId") {
		t.Fatalf("search hits = %+v, want YAML endpoint with resolved parameter text", searchResult.Hits)
	}

	apiID := ids.Stable("api", created.ID, "GET", "/member/benefits")
	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/nodes/"+apiID, nil)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get API node status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var node storage.Node
	decodeJSON(t, rr, &node)
	if node.ID != apiID || node.Kind != "API" || node.Name != "GET /member/benefits" {
		t.Fatalf("API node = %+v, want synced OpenAPI API node", node)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/nodes/"+apiID+"/related?direction=in&kind=describes&limit=5", nil)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("related describes status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var relatedBody struct {
		Related []storage.RelatedNode `json:"related"`
	}
	decodeJSON(t, rr, &relatedBody)
	if len(relatedBody.Related) != 1 {
		t.Fatalf("describes related len = %d, want 1: %+v", len(relatedBody.Related), relatedBody.Related)
	}
	if relatedBody.Related[0].Node.Kind != "DocSection" || relatedBody.Related[0].Edge.Kind != "describes" || relatedBody.Related[0].Edge.EvidenceSectionID == "" {
		t.Fatalf("describes related = %+v, want DocSection evidence edge", relatedBody.Related[0])
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/nodes/"+apiID+"/related?direction=in&kind=exposes_api&limit=5", nil)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("related exposes_api status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	decodeJSON(t, rr, &relatedBody)
	if len(relatedBody.Related) != 1 {
		t.Fatalf("exposes_api related len = %d, want 1: %+v", len(relatedBody.Related), relatedBody.Related)
	}
	if relatedBody.Related[0].Node.Kind != "Document" || relatedBody.Related[0].Edge.Kind != "exposes_api" || relatedBody.Related[0].Edge.EvidenceSectionID == "" {
		t.Fatalf("exposes_api related = %+v, want Document evidence edge", relatedBody.Related[0])
	}
}

func TestGitSourceHandlersSyncSearchUpdateAndCleanStaleDocs(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	repo := initServerGitDocsRepo(t, map[string]string{
		"docs/member.md": `# Member Git Docs

Server servergitonealpha membership content.
`,
		"other/ignored.md": `# Ignored

This servergitignoredtoken content is outside the configured Git path.
`,
	})

	rr := postJSON(t, handler, "/api/sources", map[string]any{
		"kind":         "git",
		"name":         "Git Product Docs",
		"dsn":          "file://" + filepath.ToSlash(repo),
		"branch":       "main",
		"path":         "docs",
		"product_hint": "Membership",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create Git source status = %d, want %d; body: %s", rr.Code, http.StatusCreated, rr.Body.String())
	}
	var created storage.Source
	decodeJSON(t, rr, &created)
	if created.Kind != "git" || created.Name != "Git Product Docs" {
		t.Fatalf("created Git source = %+v, want Git source", created)
	}
	if !strings.Contains(created.ConfigJSON, `"branch":"main"`) || !strings.Contains(created.ConfigJSON, `"path":"docs"`) {
		t.Fatalf("created ConfigJSON = %q, want branch/path config", created.ConfigJSON)
	}

	rr = postJSON(t, handler, "/api/sources/"+created.ID+"/sync", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("sync Git source status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	assertServerSearchContains(t, handler, "servergitonealpha", "Member Git Docs")
	assertServerSearchEmpty(t, handler, "servergitignoredtoken")

	writeFile(t, filepath.Join(repo, "docs/member.md"), `# Member Git Docs

Server servergittwobeta membership content.
`)
	commitServerGitRepo(t, repo, "update docs")
	rr = postJSON(t, handler, "/api/sources/"+created.ID+"/sync", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("sync updated Git source status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	assertServerSearchEmpty(t, handler, "servergitonealpha")
	assertServerSearchContains(t, handler, "servergittwobeta", "Member Git Docs")

	if err := os.Remove(filepath.Join(repo, "docs/member.md")); err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}
	commitServerGitRepo(t, repo, "remove docs")
	rr = postJSON(t, handler, "/api/sources/"+created.ID+"/sync", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("sync deleted Git source status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	assertServerSearchEmpty(t, handler, "servergittwobeta")
}

func TestGitSourceHandlersReportInvalidPathAndFailedJob(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	repo := initServerGitDocsRepo(t, map[string]string{
		"docs/member.md": `# Member Git Docs

Server invalid path fixture.
`,
	})

	rr := postJSON(t, handler, "/api/sources", map[string]any{
		"kind":   "git",
		"name":   "Broken Git Docs",
		"dsn":    "file://" + filepath.ToSlash(repo),
		"branch": "main",
		"path":   "missing-docs",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create broken Git source status = %d, want %d; body: %s", rr.Code, http.StatusCreated, rr.Body.String())
	}
	var created storage.Source
	decodeJSON(t, rr, &created)

	rr = postJSON(t, handler, "/api/sources/"+created.ID+"/sync", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("sync invalid Git path status = %d, want %d; body: %s", rr.Code, http.StatusBadRequest, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "missing-docs") {
		t.Fatalf("sync invalid Git path body = %q, want missing path", rr.Body.String())
	}

	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/sources/"+created.ID+"/jobs?limit=5", nil)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("jobs status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var jobsBody struct {
		Jobs []storage.SyncJob `json:"jobs"`
	}
	decodeJSON(t, rr, &jobsBody)
	if len(jobsBody.Jobs) != 1 || jobsBody.Jobs[0].Status != "failed" || !strings.Contains(jobsBody.Jobs[0].LastError, "missing-docs") {
		t.Fatalf("jobs response = %+v, want failed Git sync job mentioning path", jobsBody.Jobs)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/api/sources/"+created.ID+"/jobs/"+jobsBody.Jobs[0].ID, nil)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("delete sync job status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/sources/"+created.ID+"/jobs?limit=5", nil)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("jobs after delete status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	decodeJSON(t, rr, &jobsBody)
	if len(jobsBody.Jobs) != 0 {
		t.Fatalf("jobs after delete = %+v, want empty", jobsBody.Jobs)
	}
}

func TestHTMLSourceHandlersSyncSearchAndCleanStaleDocs(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	docsDir := t.TempDir()
	writeFile(t, filepath.Join(docsDir, "index.html"), `<!doctype html>
<html>
  <head><title>Server HTML Docs</title></head>
  <body>
    <h1 id="overview">Server HTML Overview</h1>
    <p>Server serverhtmlonealpha content.</p>
    <h2 id="api">API Usage</h2>
    <p>See <a href="details.html#quota">quota docs</a> for linked details.</p>
  </body>
</html>`)
	writeFile(t, filepath.Join(docsDir, "details.html"), `<!doctype html>
<html>
  <head><title>Server HTML Detail Docs</title></head>
  <body><h1 id="quota">Quota Details</h1><p>Server serverhtmlquotabeta content.</p></body>
</html>`)

	rr := postJSON(t, handler, "/api/sources", map[string]any{
		"kind": "html",
		"name": "HTML Product Docs",
		"dsn":  docsDir,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create HTML source status = %d, want %d; body: %s", rr.Code, http.StatusCreated, rr.Body.String())
	}
	var created storage.Source
	decodeJSON(t, rr, &created)
	if created.Kind != "html" || created.Name != "HTML Product Docs" {
		t.Fatalf("created HTML source = %+v, want HTML source", created)
	}

	rr = postJSON(t, handler, "/api/sources/"+created.ID+"/sync", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("sync HTML source status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	assertServerSearchContains(t, handler, "serverhtmlonealpha", "Server HTML Docs")
	assertServerSearchContains(t, handler, "serverhtmlquotabeta", "Server HTML Detail Docs")

	if err := os.Remove(filepath.Join(docsDir, "details.html")); err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}
	rr = postJSON(t, handler, "/api/sources/"+created.ID+"/sync", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("sync deleted HTML source status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	assertServerSearchEmpty(t, handler, "serverhtmlquotabeta")
	assertServerSearchContains(t, handler, "serverhtmlonealpha", "Server HTML Docs")
}

func TestConfluenceSourceHandlersSyncSearchAndFailedJobs(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	confluenceServer := newServerConfluenceMock(t, "Bearer server-secret")

	rr := postJSON(t, handler, "/api/sources", map[string]any{
		"kind": "confluence",
		"name": "Confluence Product Docs",
		"dsn":  confluenceServer.URL + "/wiki",
		"config_json": serverConfluenceConfig(t, map[string]any{
			"base_url":         confluenceServer.URL + "/wiki",
			"page_id":          "100",
			"token":            "server-secret",
			"include_children": true,
		}),
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create Confluence source status = %d, want %d; body: %s", rr.Code, http.StatusCreated, rr.Body.String())
	}
	var created storage.Source
	decodeJSON(t, rr, &created)
	if created.Kind != "confluence" || created.Name != "Confluence Product Docs" || !strings.Contains(created.ConfigJSON, `"page_id":"100"`) {
		t.Fatalf("created Confluence source = %+v, want Confluence source with config", created)
	}

	rr = postJSON(t, handler, "/api/sources/"+created.ID+"/sync", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("sync Confluence source status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	assertServerSearchContains(t, handler, "serverconfluencerootalpha", "Server Confluence Root")
	assertServerSearchContains(t, handler, "serverconfluencechildbeta", "Server Confluence Child")

	rr = postJSON(t, handler, "/api/sources", map[string]any{
		"kind": "confluence",
		"name": "Broken Confluence Product Docs",
		"dsn":  confluenceServer.URL + "/wiki",
		"config_json": serverConfluenceConfig(t, map[string]any{
			"base_url": confluenceServer.URL + "/wiki",
			"page_id":  "100",
			"token":    "bad-secret",
		}),
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create broken Confluence source status = %d, want %d; body: %s", rr.Code, http.StatusCreated, rr.Body.String())
	}
	var broken storage.Source
	decodeJSON(t, rr, &broken)

	rr = postJSON(t, handler, "/api/sources/"+broken.ID+"/sync", nil)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "auth") {
		t.Fatalf("sync broken Confluence status/body = %d/%s, want auth bad request", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/sources/"+broken.ID+"/jobs?limit=5", nil)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("jobs status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var jobsBody struct {
		Jobs []storage.SyncJob `json:"jobs"`
	}
	decodeJSON(t, rr, &jobsBody)
	if len(jobsBody.Jobs) != 1 || jobsBody.Jobs[0].Status != "failed" || !strings.Contains(jobsBody.Jobs[0].LastError, "auth") {
		t.Fatalf("jobs response = %+v, want failed Confluence auth job", jobsBody.Jobs)
	}
}

func TestContextHandlerReturnsEvidenceSections(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	docsDir := t.TempDir()
	writeFile(t, filepath.Join(docsDir, "member.md"), `# Member Benefits

Members see entitlement details on the benefits page.

## API

GET /member/benefits returns active member benefits.
`)

	rr := postJSON(t, handler, "/api/sources", map[string]any{
		"kind": "local",
		"name": "Context Docs",
		"dsn":  "file://" + filepath.ToSlash(docsDir),
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create source status = %d, want %d; body: %s", rr.Code, http.StatusCreated, rr.Body.String())
	}
	var created storage.Source
	decodeJSON(t, rr, &created)

	rr = postJSON(t, handler, "/api/sources/"+created.ID+"/sync", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("sync status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	rr = postJSON(t, handler, "/api/context", map[string]any{
		"task":         "Summarize member benefits",
		"max_sections": 4,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("context status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var body struct {
		Task     string `json:"task"`
		Query    string `json:"query"`
		Sections []struct {
			SectionID     string `json:"section_id"`
			DocumentTitle string `json:"document_title"`
			DocumentURL   string `json:"document_url"`
			HeadingPath   string `json:"heading_path"`
			Content       string `json:"content"`
			Evidence      string `json:"evidence"`
		} `json:"sections"`
	}
	decodeJSON(t, rr, &body)
	if body.Task != "Summarize member benefits" || body.Query != "Summarize member benefits" {
		t.Fatalf("context task/query = %q/%q, want task-derived query", body.Task, body.Query)
	}
	if len(body.Sections) == 0 {
		t.Fatalf("context sections empty, want evidence sections")
	}
	if body.Sections[0].SectionID == "" || body.Sections[0].DocumentTitle != "Member Benefits" {
		t.Fatalf("context first section = %+v, want member benefits evidence", body.Sections[0])
	}
	if !strings.Contains(body.Sections[0].Content, "member benefits") && !strings.Contains(body.Sections[0].Content, "entitlement") {
		t.Fatalf("context content = %q, want member benefits content", body.Sections[0].Content)
	}
	if body.Sections[0].DocumentURL == "" || body.Sections[0].Evidence == "" {
		t.Fatalf("context section lacks source evidence: %+v", body.Sections[0])
	}
}

func TestNodeHandlersReturnNodeAndRelated(t *testing.T) {
	handler, store, cleanup := newTestHandlerWithStore(t)
	defer cleanup()

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
		Confidence:    0.92,
	}); err != nil {
		t.Fatalf("upsert api node: %v", err)
	}
	if err := store.UpsertEdge(ctx, storage.EdgeInput{
		ID:         "edge-product-api",
		SrcID:      "node-product",
		DstID:      "node-api",
		Kind:       "exposes_api",
		Confidence: 0.9,
		Provenance: "rule",
	}); err != nil {
		t.Fatalf("upsert graph edge: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/nodes/node-product", nil)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get node status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var node storage.Node
	decodeJSON(t, rr, &node)
	if node.ID != "node-product" || node.Kind != "Product" || node.Name != "Membership" {
		t.Fatalf("node response = %+v, want product node", node)
	}
	if node.CanonicalName != "membership" || node.MetadataJSON != `{"owner":"growth"}` || node.Confidence != 1 {
		t.Fatalf("node details = %+v, want canonical metadata and confidence", node)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/nodes/node-product/related?direction=out&kind=exposes_api&limit=5", nil)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("related status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var relatedBody struct {
		Related []storage.RelatedNode `json:"related"`
	}
	decodeJSON(t, rr, &relatedBody)
	if len(relatedBody.Related) != 1 {
		t.Fatalf("related len = %d, want 1: %+v", len(relatedBody.Related), relatedBody.Related)
	}
	related := relatedBody.Related[0]
	if related.Direction != "out" || related.Node.ID != "node-api" || related.Edge.Kind != "exposes_api" {
		t.Fatalf("related response = %+v, want outgoing API relation", related)
	}
	if related.Edge.ID != "edge-product-api" || related.Edge.SrcID != "node-product" || related.Edge.DstID != "node-api" {
		t.Fatalf("related edge ids = %+v, want product to API edge", related.Edge)
	}
	if related.Edge.Confidence != 0.9 || related.Edge.Provenance != "rule" {
		t.Fatalf("related edge details = %+v, want rule edge confidence", related.Edge)
	}
}

func TestImpactHandlerReturnsTraversalPaths(t *testing.T) {
	handler, store, cleanup := newTestHandlerWithStore(t)
	defer cleanup()

	seedServerImpactGraph(t, store)

	rr := postJSON(t, handler, "/api/impact", map[string]any{
		"id":        "node-product",
		"direction": "out",
		"kind":      "exposes_api",
		"max_depth": 3,
		"limit":     10,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("impact status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var result storage.ImpactResult
	decodeJSON(t, rr, &result)
	if result.StartNode.ID != "node-product" {
		t.Fatalf("impact start node = %+v, want node-product", result.StartNode)
	}
	if len(result.Paths) != 1 {
		t.Fatalf("impact paths len = %d, want 1: %+v", len(result.Paths), result.Paths)
	}
	path := result.Paths[0]
	if len(path.Nodes) != 2 || len(path.Edges) != 1 {
		t.Fatalf("impact path = %+v, want one exposes_api path", path)
	}
	if path.Nodes[1].ID != "node-api" || path.Edges[0].Kind != "exposes_api" {
		t.Fatalf("impact path = %+v, want product to API exposes_api path", path)
	}
	if path.Edges[0].EvidenceSectionID != "section-impact" || path.Edges[0].Provenance != "rule" {
		t.Fatalf("impact edge = %+v, want evidence and provenance", path.Edges[0])
	}

	rr = postJSON(t, handler, "/api/impact", map[string]any{
		"id":        "node-product",
		"direction": "sideways",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid impact status = %d, want %d; body: %s", rr.Code, http.StatusBadRequest, rr.Body.String())
	}
}

func TestFeedbackHandlersCreateListAndValidate(t *testing.T) {
	handler, store, cleanup := newTestHandlerWithStore(t)
	defer cleanup()
	seedServerImpactGraph(t, store)

	rr := postJSON(t, handler, "/api/feedback", map[string]any{
		"target_kind":   "edge",
		"target_id":     "edge-product-api",
		"feedback_kind": "relationship_wrong",
		"actor":         "alice",
		"payload":       map[string]any{"reason": "duplicate"},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create feedback status = %d, want %d; body: %s", rr.Code, http.StatusCreated, rr.Body.String())
	}
	var created storage.FeedbackEvent
	decodeJSON(t, rr, &created)
	if created.ID == "" || created.TargetID != "edge-product-api" || created.FeedbackKind != "relationship_wrong" || created.Actor != "alice" {
		t.Fatalf("created feedback = %+v, want populated event", created)
	}

	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/nodes/node-product/related?direction=out&kind=exposes_api", nil)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("related after feedback status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var relatedBody struct {
		Related []storage.RelatedNode `json:"related"`
	}
	decodeJSON(t, rr, &relatedBody)
	if len(relatedBody.Related) != 0 {
		t.Fatalf("related after feedback = %+v, want relationship_wrong edge filtered", relatedBody.Related)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/feedback?target_kind=edge&target_id=edge-product-api&limit=5", nil)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list feedback status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var listBody struct {
		Feedback []storage.FeedbackEvent `json:"feedback"`
	}
	decodeJSON(t, rr, &listBody)
	if len(listBody.Feedback) != 1 || listBody.Feedback[0].ID != created.ID {
		t.Fatalf("list feedback = %+v, want created event", listBody.Feedback)
	}

	rr = postJSON(t, handler, "/api/feedback", map[string]any{
		"target_kind": "edge",
		"target_id":   "edge-product-api",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid feedback status = %d, want %d; body: %s", rr.Code, http.StatusBadRequest, rr.Body.String())
	}

	rr = postJSON(t, handler, "/api/feedback", map[string]any{
		"target_kind":   "edge",
		"target_id":     "edge-product-api",
		"feedback_kind": "document_stale",
	})
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "target_kind") {
		t.Fatalf("wrong target feedback status/body = %d/%s, want target_kind bad request", rr.Code, rr.Body.String())
	}
}

func TestFeedbackDeleteAndEdgeRestored(t *testing.T) {
	handler, store, cleanup := newTestHandlerWithStore(t)
	defer cleanup()
	seedServerImpactGraph(t, store)

	// Create a relationship_wrong feedback event.
	rr := postJSON(t, handler, "/api/feedback", map[string]any{
		"target_kind":   "edge",
		"target_id":     "edge-product-api",
		"feedback_kind": "relationship_wrong",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create feedback status = %d, want %d; body: %s", rr.Code, http.StatusCreated, rr.Body.String())
	}
	var created storage.FeedbackEvent
	decodeJSON(t, rr, &created)

	// Verify edge is hidden.
	rr2 := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/nodes/node-product/related?direction=out&kind=exposes_api", nil)
	handler.ServeHTTP(rr2, req)
	if rr2.Code != http.StatusOK {
		t.Fatalf("related status = %d, want %d", rr2.Code, http.StatusOK)
	}
	var relatedBody struct {
		Related []storage.RelatedNode `json:"related"`
	}
	decodeJSON(t, rr2, &relatedBody)
	if len(relatedBody.Related) != 0 {
		t.Fatalf("related after feedback = %+v, want edge filtered", relatedBody.Related)
	}

	// Delete the feedback event.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/api/feedback/"+created.ID, nil)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("delete feedback status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	// Verify edge is restored.
	rr2 = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/nodes/node-product/related?direction=out&kind=exposes_api", nil)
	handler.ServeHTTP(rr2, req)
	if rr2.Code != http.StatusOK {
		t.Fatalf("related after delete status = %d, want %d", rr2.Code, http.StatusOK)
	}
	decodeJSON(t, rr2, &relatedBody)
	if len(relatedBody.Related) != 1 {
		t.Fatalf("related after delete = %d edges, want 1 restored edge", len(relatedBody.Related))
	}

	// Delete non-existent feedback should return 404.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/api/feedback/fb_nonexistent", nil)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("delete missing feedback status = %d, want %d", rr.Code, http.StatusNotFound)
	}
}

func TestNodeHandlersValidateRequests(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/nodes/missing-node", nil)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("missing node status = %d, want %d; body: %s", rr.Code, http.StatusNotFound, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/nodes/missing-node/related?limit=abc", nil)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid related limit status = %d, want %d; body: %s", rr.Code, http.StatusBadRequest, rr.Body.String())
	}
}

func newTestHandler(t *testing.T) (http.Handler, func()) {
	handler, _, cleanup := newTestHandlerWithStore(t)
	return handler, cleanup
}

func newTestHandlerWithAuth(t *testing.T, auth config.AuthConfig) (http.Handler, func()) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "docgraph.db")
	store, err := storage.Open(context.Background(), "sqlite://"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.Migrate(context.Background()); err != nil {
		_ = store.Close()
		t.Fatalf("migrate store: %v", err)
	}

	srv := NewWithAuth("127.0.0.1:0", store, slog.New(slog.NewTextHandler(io.Discard, nil)), auth)
	handler, err := srv.routes()
	if err != nil {
		_ = store.Close()
		t.Fatalf("build routes: %v", err)
	}

	return handler, func() {
		if err := store.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
	}
}

func newTestHandlerWithStore(t *testing.T) (http.Handler, storage.Store, func()) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "docgraph.db")
	store, err := storage.Open(context.Background(), "sqlite://"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.Migrate(context.Background()); err != nil {
		_ = store.Close()
		t.Fatalf("migrate store: %v", err)
	}

	srv := New("127.0.0.1:0", store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler, err := srv.routes()
	if err != nil {
		_ = store.Close()
		t.Fatalf("build routes: %v", err)
	}

	return handler, store, func() {
		if err := store.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
	}
}

func seedServerImpactGraph(t *testing.T, store storage.Store) {
	t.Helper()

	ctx := context.Background()
	if _, err := store.CreateSource(ctx, storage.Source{
		ID:   "source-impact",
		Kind: "local",
		Name: "Impact Docs",
		DSN:  "file:///impact",
	}); err != nil {
		t.Fatalf("create impact source: %v", err)
	}
	if err := store.ReplaceDocument(ctx, storage.DocumentInput{
		ID:          "doc-impact",
		SourceID:    "source-impact",
		ExternalID:  "impact.md",
		Title:       "Impact",
		ContentHash: "hash-impact-doc",
	}, []storage.SectionInput{
		{
			ID:          "section-impact",
			Title:       "Impact Evidence",
			Content:     "Membership exposes API and downstream module behavior.",
			ContentHash: "hash-impact-section",
		},
	}); err != nil {
		t.Fatalf("replace impact document: %v", err)
	}
	nodes := []storage.NodeInput{
		{ID: "node-product", Kind: "Product", Name: "Membership", CanonicalName: "membership"},
		{ID: "node-api", Kind: "API", Name: "GET /member/benefits", CanonicalName: "get /member/benefits"},
		{ID: "node-module", Kind: "Module", Name: "Entitlements", CanonicalName: "entitlements"},
	}
	for _, node := range nodes {
		if err := store.UpsertNode(ctx, node); err != nil {
			t.Fatalf("upsert impact node %s: %v", node.ID, err)
		}
	}
	edges := []storage.EdgeInput{
		{ID: "edge-product-api", SrcID: "node-product", DstID: "node-api", Kind: "exposes_api", EvidenceSectionID: "section-impact"},
		{ID: "edge-api-module", SrcID: "node-api", DstID: "node-module", Kind: "describes", EvidenceSectionID: "section-impact"},
		{ID: "edge-module-product", SrcID: "node-module", DstID: "node-product", Kind: "depends_on", EvidenceSectionID: "section-impact"},
	}
	for _, edge := range edges {
		if err := store.UpsertEdge(ctx, edge); err != nil {
			t.Fatalf("upsert impact edge %s: %v", edge.ID, err)
		}
	}
}

func postJSON(t *testing.T, handler http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()

	return requestJSON(t, handler, http.MethodPost, path, body)
}

func putJSON(t *testing.T, handler http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()

	return requestJSON(t, handler, http.MethodPut, path, body)
}

func requestJSON(t *testing.T, handler http.Handler, method string, path string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var r io.Reader
	if body != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode request body: %v", err)
		}
		r = &buf
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func decodeJSON(t *testing.T, rr *httptest.ResponseRecorder, dst any) {
	t.Helper()

	if err := json.NewDecoder(rr.Body).Decode(dst); err != nil {
		t.Fatalf("decode JSON response %q: %v", rr.Body.String(), err)
	}
}

func assertServerSearchContains(t *testing.T, handler http.Handler, query string, wantTitle string) {
	t.Helper()

	rr := postJSON(t, handler, "/api/search", map[string]any{
		"query": query,
		"limit": 10,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("search %q status = %d, want %d; body: %s", query, rr.Code, http.StatusOK, rr.Body.String())
	}
	var result struct {
		Hits []storage.SearchHit `json:"hits"`
	}
	decodeJSON(t, rr, &result)
	if len(result.Hits) == 0 {
		t.Fatalf("search %q returned no hits", query)
	}
	if wantTitle != "" && result.Hits[0].DocumentTitle != wantTitle {
		t.Fatalf("search %q first title = %q, want %q", query, result.Hits[0].DocumentTitle, wantTitle)
	}
}

func assertServerSearchEmpty(t *testing.T, handler http.Handler, query string) {
	t.Helper()

	rr := postJSON(t, handler, "/api/search", map[string]any{
		"query": query,
		"limit": 10,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("search %q status = %d, want %d; body: %s", query, rr.Code, http.StatusOK, rr.Body.String())
	}
	var result struct {
		Hits []storage.SearchHit `json:"hits"`
	}
	decodeJSON(t, rr, &result)
	for _, hit := range result.Hits {
		if strings.Contains(strings.ToLower(hit.Content), strings.ToLower(query)) {
			t.Fatalf("search %q returned stale hit %+v", query, hit)
		}
	}
}

type serverConfluenceMock struct {
	URL string
}

func newServerConfluenceMock(t *testing.T, wantAuth string) *serverConfluenceMock {
	t.Helper()

	restore := confluence.SetHTTPClientFactoryForTest(func() *http.Client {
		return &http.Client{Transport: serverConfluenceRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			return serverConfluenceResponseViaRecorder(handleServerConfluenceRequest(t, r, wantAuth)), nil
		})}
	})
	t.Cleanup(restore)
	return &serverConfluenceMock{URL: "https://confluence.example"}
}

type serverConfluenceRoundTripFunc func(*http.Request) (*http.Response, error)

func (f serverConfluenceRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func handleServerConfluenceRequest(t *testing.T, r *http.Request, wantAuth string) *http.Response {
	t.Helper()

	if r.Header.Get("Authorization") != wantAuth {
		return serverConfluenceTextResponse(http.StatusUnauthorized, "unauthorized")
	}
	switch r.URL.Path {
	case "/wiki/rest/api/content/100":
		return serverConfluencePageResponse(t, "100", "Server Confluence Root", 4, `<h1>Overview</h1><p>Server serverconfluencerootalpha content.</p>`)
	case "/wiki/rest/api/content/100/child/page":
		return serverConfluenceJSONResponse(t, map[string]any{
			"results": []map[string]any{{"id": "200", "title": "Server Confluence Child"}},
		})
	case "/wiki/rest/api/content/200":
		return serverConfluencePageResponse(t, "200", "Server Confluence Child", 1, `<h1>Child</h1><p>Server serverconfluencechildbeta content.</p>`)
	case "/wiki/rest/api/content/200/child/page":
		return serverConfluenceJSONResponse(t, map[string]any{"results": []map[string]any{}})
	default:
		return serverConfluenceTextResponse(http.StatusNotFound, "not found")
	}
}

func serverConfluencePageResponse(t *testing.T, id string, title string, version int, body string) *http.Response {
	t.Helper()

	return serverConfluenceJSONResponse(t, map[string]any{
		"id":    id,
		"type":  "page",
		"title": title,
		"version": map[string]any{
			"number": version,
		},
		"_links": map[string]any{
			"webui": "/spaces/PROD/pages/" + id,
		},
		"body": map[string]any{
			"storage": map[string]any{
				"value":          body,
				"representation": "storage",
			},
		},
	})
}

func serverConfluenceJSONResponse(t *testing.T, body any) *http.Response {
	t.Helper()

	var data bytes.Buffer
	if err := json.NewEncoder(&data).Encode(body); err != nil {
		t.Fatalf("encode confluence response: %v", err)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(&data),
	}
}

func serverConfluenceTextResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func serverConfluenceResponseViaRecorder(resp *http.Response) *http.Response {
	rr := httptest.NewRecorder()
	for key, values := range resp.Header {
		for _, value := range values {
			rr.Header().Add(key, value)
		}
	}
	rr.WriteHeader(resp.StatusCode)
	if resp.Body != nil {
		_, _ = io.Copy(rr, resp.Body)
	}
	return rr.Result()
}

func serverConfluenceConfig(t *testing.T, value map[string]any) string {
	t.Helper()

	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal confluence config: %v", err)
	}
	return string(data)
}

func initServerGitDocsRepo(t *testing.T, files map[string]string) string {
	t.Helper()

	requireServerGit(t)
	repo := t.TempDir()
	runServerGit(t, repo, "init")
	runServerGit(t, repo, "checkout", "-b", "main")
	runServerGit(t, repo, "config", "user.email", "docgraph-tests@example.com")
	runServerGit(t, repo, "config", "user.name", "DocGraph Tests")
	for path, content := range files {
		writeFile(t, filepath.Join(repo, filepath.FromSlash(path)), content)
	}
	commitServerGitRepo(t, repo, "initial docs")
	return repo
}

func commitServerGitRepo(t *testing.T, repo string, message string) {
	t.Helper()

	runServerGit(t, repo, "add", "-A")
	runServerGit(t, repo, "commit", "-m", message)
}

func requireServerGit(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable not available")
	}
}

func runServerGit(t *testing.T, repo string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
}

const openAPIServerYAMLFixture = `openapi: 3.0.3
info:
  title: Membership API
paths:
  /member/benefits:
    get:
      operationId: getMemberBenefits
      tags:
        - membership
        - benefits
      summary: List member benefits
      description: Returns available benefits for a member account.
      parameters:
        - $ref: '#/components/parameters/MemberID'
      responses:
        '200':
          $ref: '#/components/responses/BenefitListResponse'
components:
  parameters:
    MemberID:
      name: memberId
      in: query
      description: Member identifier from shared components.
  responses:
    BenefitListResponse:
      description: Successful benefit list response.
`

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
}
