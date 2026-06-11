package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docgraph/docgraph/internal/domain"
	"github.com/docgraph/docgraph/internal/ingest/confluence"
	"github.com/docgraph/docgraph/internal/query"
	"github.com/docgraph/docgraph/internal/storage"
	syncsvc "github.com/docgraph/docgraph/internal/sync"
)

func TestCLISourceSyncSearchAndContext(t *testing.T) {
	dataDir := t.TempDir()
	docsDir := t.TempDir()
	writeCLITestFile(t, filepath.Join(docsDir, "member.md"), `# Member Benefits

Membership entitlement overview for product teams.

## API

GET /member/benefits returns available member benefits.
`)

	addOut := runDocGraph(t, "source", "add", "--data", dataDir, "--name", "Docs", "--dsn", docsDir, "--product", "Membership")
	var source domain.Source
	decodeJSON(t, addOut, &source)
	if source.ID == "" {
		t.Fatalf("source add output missing id: %s", addOut)
	}
	if source.Kind != "local" {
		t.Fatalf("source Kind = %q, want local", source.Kind)
	}
	if source.Name != "Docs" {
		t.Fatalf("source Name = %q, want Docs", source.Name)
	}
	if source.DSN != docsDir {
		t.Fatalf("source DSN = %q, want %q", source.DSN, docsDir)
	}
	if source.ProductHint != "Membership" {
		t.Fatalf("source ProductHint = %q, want Membership", source.ProductHint)
	}

	listOut := runDocGraph(t, "source", "list", "--data", dataDir)
	var listed struct {
		Sources []domain.Source `json:"sources"`
	}
	decodeJSON(t, listOut, &listed)
	if len(listed.Sources) != 1 {
		t.Fatalf("source list returned %d sources, want 1: %s", len(listed.Sources), listOut)
	}
	if listed.Sources[0].ID != source.ID {
		t.Fatalf("listed source ID = %q, want %q", listed.Sources[0].ID, source.ID)
	}

	syncOut := runDocGraph(t, "source", "sync", "--data", dataDir, "--id", source.ID)
	var result syncsvc.Result
	decodeJSON(t, syncOut, &result)
	if result.SourceID != source.ID {
		t.Fatalf("sync SourceID = %q, want %q", result.SourceID, source.ID)
	}
	if result.Documents != 1 {
		t.Fatalf("sync Documents = %d, want 1", result.Documents)
	}

	searchOut := runDocGraph(t, "search", "--data", dataDir, "member", "benefits")
	var searchResult struct {
		Hits []domain.SearchHit `json:"hits"`
	}
	decodeJSON(t, searchOut, &searchResult)
	if len(searchResult.Hits) == 0 {
		t.Fatalf("search returned no hits: %s", searchOut)
	}
	if searchResult.Hits[0].DocumentTitle != "Member Benefits" {
		t.Fatalf("search DocumentTitle = %q, want Member Benefits", searchResult.Hits[0].DocumentTitle)
	}
	if !searchHitsContain(searchResult.Hits, "GET /member/benefits") {
		t.Fatalf("search hits = %#v, want API content", searchResult.Hits)
	}

	contextOut := runDocGraph(t, "context", "--data", dataDir, "--max-sections", "2", "Summarize", "member", "benefits")
	var pack query.ContextPack
	decodeJSON(t, contextOut, &pack)
	if pack.Query != "Summarize member benefits" {
		t.Fatalf("context Query = %q, want Summarize member benefits", pack.Query)
	}
	if len(pack.Sections) == 0 {
		t.Fatalf("context returned no sections: %s", contextOut)
	}
	if pack.Sections[0].Evidence == "" {
		t.Fatalf("context Evidence is empty: %#v", pack.Sections[0])
	}
	if !contextSectionsContain(pack.Sections, "GET /member/benefits") {
		t.Fatalf("context Sections = %#v, want API content", pack.Sections)
	}
}

func TestCLISourceLifecycleUpdateJobsAndDelete(t *testing.T) {
	dataDir := t.TempDir()
	initialDocsDir := t.TempDir()
	updatedDocsDir := t.TempDir()
	writeCLITestFile(t, filepath.Join(initialDocsDir, "initial.md"), `# Initial Docs

Initial content that should not be synced after update.
`)
	writeCLITestFile(t, filepath.Join(updatedDocsDir, "updated.md"), `# Updated Docs

cli-lifecycle-token content for updated source.
`)

	addOut := runDocGraph(t, "source", "add", "--data", dataDir, "--name", "Lifecycle Docs", "--dsn", initialDocsDir, "--product", "Membership", "--module", "Benefits")
	var source domain.Source
	decodeJSON(t, addOut, &source)
	if source.ID == "" {
		t.Fatalf("source add output missing id: %s", addOut)
	}

	updateOut := runDocGraph(t, "source", "update", "--data", dataDir, "--id", source.ID, "--name", "Updated Lifecycle Docs", "--dsn", updatedDocsDir, "--product", "Payments", "--module", "Checkout")
	var updated domain.Source
	decodeJSON(t, updateOut, &updated)
	if updated.Name != "Updated Lifecycle Docs" || updated.DSN != updatedDocsDir || updated.ProductHint != "Payments" || updated.ModuleHint != "Checkout" {
		t.Fatalf("source update output = %+v, want changed name/dsn/product/module", updated)
	}

	syncOut := runDocGraph(t, "source", "sync", "--data", dataDir, "--id", source.ID)
	var result syncsvc.Result
	decodeJSON(t, syncOut, &result)
	if result.SourceID != source.ID || result.Documents != 1 || result.JobID == "" {
		t.Fatalf("source sync output = %+v, want source, one doc, job id", result)
	}

	searchOut := runDocGraph(t, "search", "--data", dataDir, "cli-lifecycle-token")
	var searchResult struct {
		Hits []domain.SearchHit `json:"hits"`
	}
	decodeJSON(t, searchOut, &searchResult)
	if len(searchResult.Hits) == 0 || searchResult.Hits[0].DocumentTitle != "Updated Docs" {
		t.Fatalf("search after update/sync = %+v, want updated docs hit", searchResult.Hits)
	}

	jobsOut := runDocGraph(t, "source", "jobs", "--data", dataDir, "--id", source.ID)
	var jobsBody struct {
		Jobs []domain.SyncJob `json:"jobs"`
	}
	decodeJSON(t, jobsOut, &jobsBody)
	if len(jobsBody.Jobs) != 1 || jobsBody.Jobs[0].Status != "completed" || !strings.Contains(jobsBody.Jobs[0].PayloadJSON, `"documents":1`) {
		t.Fatalf("source jobs output = %+v, want completed sync job", jobsBody.Jobs)
	}

	deleteOut := runDocGraph(t, "source", "delete", "--data", dataDir, "--id", source.ID)
	var deleted struct {
		Deleted string `json:"deleted"`
	}
	decodeJSON(t, deleteOut, &deleted)
	if deleted.Deleted != source.ID {
		t.Fatalf("source delete output = %+v, want deleted source id", deleted)
	}

	listOut := runDocGraph(t, "source", "list", "--data", dataDir)
	var listed struct {
		Sources []domain.Source `json:"sources"`
	}
	decodeJSON(t, listOut, &listed)
	if len(listed.Sources) != 0 {
		t.Fatalf("source list after delete = %+v, want empty", listed.Sources)
	}

	searchOut = runDocGraph(t, "search", "--data", dataDir, "cli-lifecycle-token")
	decodeJSON(t, searchOut, &searchResult)
	if len(searchResult.Hits) != 0 {
		t.Fatalf("search after delete = %+v, want empty hits", searchResult.Hits)
	}
}

func TestCLISourceSyncsAndSearchesOpenAPIKind(t *testing.T) {
	dataDir := t.TempDir()
	specPath := filepath.Join(t.TempDir(), "membership.openapi.json")
	writeCLITestFile(t, specPath, `{
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

	addOut := runDocGraph(t, "source", "add", "--data", dataDir, "--name", "Membership API", "--kind", "openapi", "--dsn", specPath)
	var source domain.Source
	decodeJSON(t, addOut, &source)
	if source.ID == "" {
		t.Fatalf("source add output missing id: %s", addOut)
	}
	if source.Kind != "openapi" {
		t.Fatalf("source Kind = %q, want openapi", source.Kind)
	}
	if source.Name != "Membership API" {
		t.Fatalf("source Name = %q, want Membership API", source.Name)
	}
	if source.DSN != specPath {
		t.Fatalf("source DSN = %q, want %q", source.DSN, specPath)
	}

	syncOut := runDocGraph(t, "source", "sync", "--data", dataDir, "--id", source.ID)
	var result syncsvc.Result
	decodeJSON(t, syncOut, &result)
	if result.SourceID != source.ID || result.Documents != 1 {
		t.Fatalf("sync result = %+v, want source %q with 1 document", result, source.ID)
	}

	searchOut := runDocGraph(t, "search", "--data", dataDir, "getMemberBenefits")
	var searchResult struct {
		Hits []domain.SearchHit `json:"hits"`
	}
	decodeJSON(t, searchOut, &searchResult)
	if len(searchResult.Hits) == 0 {
		t.Fatalf("search returned no hits: %s", searchOut)
	}
	if searchResult.Hits[0].DocumentTitle != "Membership API" {
		t.Fatalf("search DocumentTitle = %q, want Membership API", searchResult.Hits[0].DocumentTitle)
	}
	if !searchHitsContain(searchResult.Hits, "GET /member/benefits") {
		t.Fatalf("search hits = %#v, want OpenAPI endpoint content", searchResult.Hits)
	}
}

func TestCLIGitSourceSyncSearchesUpdatesAndCleansStaleDocs(t *testing.T) {
	dataDir := t.TempDir()
	repo := initCLIGitDocsRepo(t, map[string]string{
		"docs/member.md": `# Member Git Docs

CLI cligitonealpha membership content.
`,
		"other/ignored.md": `# Ignored

This cligitignoredtoken content is outside the configured Git path.
`,
	})

	addOut := runDocGraph(t, "source", "add", "--data", dataDir, "--name", "Git Docs", "--kind", "git", "--dsn", "file://"+filepath.ToSlash(repo), "--branch", "main", "--path", "docs", "--product", "Membership")
	var source domain.Source
	decodeJSON(t, addOut, &source)
	if source.ID == "" {
		t.Fatalf("source add output missing id: %s", addOut)
	}
	if source.Kind != "git" || source.Name != "Git Docs" {
		t.Fatalf("source add output = %+v, want Git source", source)
	}
	if !strings.Contains(source.ConfigJSON, `"branch":"main"`) || !strings.Contains(source.ConfigJSON, `"path":"docs"`) {
		t.Fatalf("source ConfigJSON = %q, want branch/path config", source.ConfigJSON)
	}

	syncOut := runDocGraph(t, "source", "sync", "--data", dataDir, "--id", source.ID)
	var result syncsvc.Result
	decodeJSON(t, syncOut, &result)
	if result.SourceID != source.ID || result.Documents != 1 || result.JobID == "" {
		t.Fatalf("sync output = %+v, want one Git document and job id", result)
	}
	assertCLISearchContains(t, dataDir, "cligitonealpha", "Member Git Docs")
	assertCLISearchEmpty(t, dataDir, "cligitignoredtoken")

	writeCLITestFile(t, filepath.Join(repo, "docs/member.md"), `# Member Git Docs

CLI cligittwobeta membership content.
`)
	commitCLIGitRepo(t, repo, "update docs")
	runDocGraph(t, "source", "sync", "--data", dataDir, "--id", source.ID)
	assertCLISearchEmpty(t, dataDir, "cligitonealpha")
	assertCLISearchContains(t, dataDir, "cligittwobeta", "Member Git Docs")

	if err := os.Remove(filepath.Join(repo, "docs/member.md")); err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}
	commitCLIGitRepo(t, repo, "remove docs")
	runDocGraph(t, "source", "sync", "--data", dataDir, "--id", source.ID)
	assertCLISearchEmpty(t, dataDir, "cligittwobeta")
}

func TestCLIGitSourceInvalidBranchRecordsFailedJob(t *testing.T) {
	dataDir := t.TempDir()
	repo := initCLIGitDocsRepo(t, map[string]string{
		"docs/member.md": `# Member Git Docs

CLI invalid branch fixture.
`,
	})

	addOut := runDocGraph(t, "source", "add", "--data", dataDir, "--name", "Broken Git Docs", "--kind", "git", "--dsn", "file://"+filepath.ToSlash(repo), "--branch", "missing-branch", "--path", "docs")
	var source domain.Source
	decodeJSON(t, addOut, &source)

	err := runDocGraphError("source", "sync", "--data", dataDir, "--id", source.ID)
	if err == nil {
		t.Fatalf("source sync returned nil error for missing branch")
	}
	if !strings.Contains(err.Error(), "missing-branch") {
		t.Fatalf("source sync error = %q, want missing branch", err.Error())
	}

	jobsOut := runDocGraph(t, "source", "jobs", "--data", dataDir, "--id", source.ID)
	var jobsBody struct {
		Jobs []domain.SyncJob `json:"jobs"`
	}
	decodeJSON(t, jobsOut, &jobsBody)
	if len(jobsBody.Jobs) != 1 || jobsBody.Jobs[0].Status != "failed" || !strings.Contains(jobsBody.Jobs[0].LastError, "missing-branch") {
		t.Fatalf("source jobs output = %+v, want failed Git sync job mentioning branch", jobsBody.Jobs)
	}
}

func TestCLIHTMLSourceSyncSearchesAndCleansStaleDocs(t *testing.T) {
	dataDir := t.TempDir()
	docsDir := t.TempDir()
	writeCLITestFile(t, filepath.Join(docsDir, "index.html"), `<!doctype html>
<html>
  <head><title>CLI HTML Docs</title></head>
  <body>
    <h1 id="overview">CLI HTML Overview</h1>
    <p>CLI clihtmlonealpha content.</p>
    <h2 id="api">API Usage</h2>
    <p>Use <code>GET /cli/html</code> for clihtmlapitoken.</p>
    <p>Read <a href="details.html#quota">quota docs</a>.</p>
  </body>
</html>`)
	writeCLITestFile(t, filepath.Join(docsDir, "details.html"), `<!doctype html>
<html>
  <head><title>CLI HTML Detail Docs</title></head>
  <body><h1 id="quota">Quota Details</h1><p>CLI clihtmlquotabeta content.</p></body>
</html>`)

	addOut := runDocGraph(t, "source", "add", "--data", dataDir, "--name", "HTML Docs", "--kind", "html", "--dsn", docsDir)
	var source domain.Source
	decodeJSON(t, addOut, &source)
	if source.ID == "" {
		t.Fatalf("source add output missing id: %s", addOut)
	}
	if source.Kind != "html" || source.Name != "HTML Docs" {
		t.Fatalf("source add output = %+v, want HTML source", source)
	}

	syncOut := runDocGraph(t, "source", "sync", "--data", dataDir, "--id", source.ID)
	var result syncsvc.Result
	decodeJSON(t, syncOut, &result)
	if result.SourceID != source.ID || result.Documents != 2 || result.JobID == "" {
		t.Fatalf("sync output = %+v, want two HTML documents and job id", result)
	}
	assertCLISearchContains(t, dataDir, "clihtmlonealpha", "CLI HTML Docs")
	assertCLISearchContains(t, dataDir, "clihtmlapitoken", "CLI HTML Docs")
	assertCLISearchContains(t, dataDir, "clihtmlquotabeta", "CLI HTML Detail Docs")

	if err := os.Remove(filepath.Join(docsDir, "details.html")); err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}
	runDocGraph(t, "source", "sync", "--data", dataDir, "--id", source.ID)
	assertCLISearchEmpty(t, dataDir, "clihtmlquotabeta")
	assertCLISearchContains(t, dataDir, "clihtmlonealpha", "CLI HTML Docs")
}

func TestCLIConfluenceSourceSyncSearchAndFailedJobs(t *testing.T) {
	dataDir := t.TempDir()
	confluenceServer := newCLIConfluenceMock(t, "Bearer cli-secret")

	addOut := runDocGraph(t,
		"source", "add",
		"--data", dataDir,
		"--name", "Confluence Docs",
		"--kind", "confluence",
		"--dsn", confluenceServer.URL+"/wiki",
		"--base-url", confluenceServer.URL+"/wiki",
		"--page-id", "100",
		"--token", "cli-secret",
		"--include-children",
	)
	var source domain.Source
	decodeJSON(t, addOut, &source)
	if source.ID == "" {
		t.Fatalf("source add output missing id: %s", addOut)
	}
	if source.Kind != "confluence" || source.Name != "Confluence Docs" || !strings.Contains(source.ConfigJSON, `"page_id":"100"`) {
		t.Fatalf("source add output = %+v, want Confluence source with config", source)
	}

	syncOut := runDocGraph(t, "source", "sync", "--data", dataDir, "--id", source.ID)
	var result syncsvc.Result
	decodeJSON(t, syncOut, &result)
	if result.SourceID != source.ID || result.Documents != 2 || result.JobID == "" {
		t.Fatalf("sync output = %+v, want two Confluence documents and job id", result)
	}
	assertCLISearchContains(t, dataDir, "cliconfluencerootalpha", "CLI Confluence Root")
	assertCLISearchContains(t, dataDir, "cliconfluencechildbeta", "CLI Confluence Child")

	brokenOut := runDocGraph(t,
		"source", "add",
		"--data", dataDir,
		"--name", "Broken Confluence Docs",
		"--kind", "confluence",
		"--dsn", confluenceServer.URL+"/wiki",
		"--base-url", confluenceServer.URL+"/wiki",
		"--page-id", "100",
		"--token", "bad-secret",
	)
	var broken domain.Source
	decodeJSON(t, brokenOut, &broken)
	err := runDocGraphError("source", "sync", "--data", dataDir, "--id", broken.ID)
	if err == nil || !strings.Contains(err.Error(), "auth") {
		t.Fatalf("source sync error = %v, want auth error", err)
	}

	jobsOut := runDocGraph(t, "source", "jobs", "--data", dataDir, "--id", broken.ID)
	var jobsBody struct {
		Jobs []domain.SyncJob `json:"jobs"`
	}
	decodeJSON(t, jobsOut, &jobsBody)
	if len(jobsBody.Jobs) != 1 || jobsBody.Jobs[0].Status != "failed" || !strings.Contains(jobsBody.Jobs[0].LastError, "auth") {
		t.Fatalf("source jobs output = %+v, want failed Confluence auth job", jobsBody.Jobs)
	}
}

func TestCLINodeGetAndRelated(t *testing.T) {
	dataDir := t.TempDir()
	seedCLIGraph(t, dataDir)

	nodeOut := runDocGraph(t, "node", "get", "--data", dataDir, "--id", "node-product")
	var node domain.Node
	decodeJSON(t, nodeOut, &node)
	if node.ID != "node-product" || node.Kind != "Product" || node.Name != "Membership" {
		t.Fatalf("node get output = %+v, want product node", node)
	}
	if node.CanonicalName != "membership" || node.Confidence != 1 {
		t.Fatalf("node get details = %+v, want canonical product node", node)
	}

	relatedOut := runDocGraph(t, "node", "related", "--data", dataDir, "--id", "node-product", "--direction", "out", "--kind", "exposes_api", "--limit", "5")
	var relatedBody struct {
		Related []domain.RelatedNode `json:"related"`
	}
	decodeJSON(t, relatedOut, &relatedBody)
	if len(relatedBody.Related) != 1 {
		t.Fatalf("node related len = %d, want 1: %s", len(relatedBody.Related), relatedOut)
	}
	related := relatedBody.Related[0]
	if related.Direction != "out" || related.Node.ID != "node-api" || related.Edge.Kind != "exposes_api" {
		t.Fatalf("node related output = %+v, want outgoing API relation", related)
	}
	if related.Edge.ID != "edge-product-api" || related.Edge.SrcID != "node-product" || related.Edge.DstID != "node-api" {
		t.Fatalf("node related edge ids = %+v, want product to API edge", related.Edge)
	}
	if related.Edge.Confidence != 0.91 || related.Edge.Provenance != "rule" {
		t.Fatalf("node related edge details = %+v, want seeded confidence and provenance", related.Edge)
	}
}

func TestCLIFeedbackAddAndList(t *testing.T) {
	dataDir := t.TempDir()
	seedCLIGraph(t, dataDir)

	addOut := runDocGraph(t,
		"feedback", "add",
		"--data", dataDir,
		"--target-kind", "edge",
		"--target-id", "edge-product-api",
		"--kind", "relationship_wrong",
		"--actor", "alice",
		"--payload", `{"reason":"duplicate"}`,
	)
	var event domain.FeedbackEvent
	decodeJSON(t, addOut, &event)
	if event.ID == "" || event.TargetID != "edge-product-api" || event.FeedbackKind != "relationship_wrong" || event.Actor != "alice" {
		t.Fatalf("feedback add output = %+v, want populated event", event)
	}

	relatedOut := runDocGraph(t, "node", "related", "--data", dataDir, "--id", "node-product", "--direction", "out", "--kind", "exposes_api")
	var relatedBody struct {
		Related []domain.RelatedNode `json:"related"`
	}
	decodeJSON(t, relatedOut, &relatedBody)
	if len(relatedBody.Related) != 0 {
		t.Fatalf("node related after feedback = %+v, want edge filtered", relatedBody.Related)
	}

	listOut := runDocGraph(t,
		"feedback", "list",
		"--data", dataDir,
		"--target-kind", "edge",
		"--target-id", "edge-product-api",
	)
	var listBody struct {
		Feedback []domain.FeedbackEvent `json:"feedback"`
	}
	decodeJSON(t, listOut, &listBody)
	if len(listBody.Feedback) != 1 || listBody.Feedback[0].ID != event.ID {
		t.Fatalf("feedback list output = %+v, want created event", listBody.Feedback)
	}

	runDocGraph(t,
		"feedback", "add",
		"--data", dataDir,
		"--target-kind", "node",
		"--target-id", "node-product",
		"--kind", "relationship_add",
		"--payload", `{"src_id":"node-product","dst_id":"node-module","kind":"owns","edge_id":"edge-manual-cli"}`,
	)
	manualOut := runDocGraph(t, "node", "related", "--data", dataDir, "--id", "node-product", "--direction", "out", "--kind", "owns")
	decodeJSON(t, manualOut, &relatedBody)
	if len(relatedBody.Related) != 1 || relatedBody.Related[0].Edge.ID != "edge-manual-cli" || relatedBody.Related[0].Edge.Provenance != "manual" {
		t.Fatalf("node related after relationship_add = %+v, want manual edge", relatedBody.Related)
	}
}

func TestCLIImpactReturnsPaths(t *testing.T) {
	dataDir := t.TempDir()
	seedCLIGraph(t, dataDir)

	out := runDocGraph(t, "impact", "--data", dataDir, "--id", "node-product", "--direction", "out", "--max-depth", "2", "--limit", "10")
	var result domain.ImpactResult
	decodeJSON(t, out, &result)
	if result.StartNode.ID != "node-product" {
		t.Fatalf("impact start node = %+v, want node-product", result.StartNode)
	}
	if len(result.Paths) != 2 {
		t.Fatalf("impact paths len = %d, want 2 one/two-hop paths: %s", len(result.Paths), out)
	}
	foundTwoHop := false
	for _, path := range result.Paths {
		if len(path.Nodes) != len(path.Edges)+1 {
			t.Fatalf("impact path = %+v, want path nodes and edges", path)
		}
		if len(path.Edges) == 2 && path.Nodes[2].ID == "node-module" {
			foundTwoHop = true
		}
	}
	if !foundTwoHop {
		t.Fatalf("impact paths = %+v, want two-hop API to module path", result.Paths)
	}

	filteredOut := runDocGraph(t, "impact", "--data", dataDir, "--id", "node-product", "--direction", "out", "--kind", "exposes_api", "--max-depth", "4")
	var filtered domain.ImpactResult
	decodeJSON(t, filteredOut, &filtered)
	if len(filtered.Paths) != 1 || filtered.Paths[0].Edges[0].Kind != "exposes_api" {
		t.Fatalf("filtered impact = %+v, want only exposes_api path", filtered.Paths)
	}
}

func seedCLIGraph(t *testing.T, dataDir string) {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(dataDir, "docgraph.db")
	store, err := storage.Open(ctx, "sqlite://"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
	}()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate store: %v", err)
	}
	if err := store.UpsertNode(ctx, storage.NodeInput{
		ID:            "node-product",
		Kind:          "Product",
		Name:          "Membership",
		CanonicalName: "membership",
		MetadataJSON:  "{}",
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
		Confidence:    0.93,
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
		ID:         "edge-product-api",
		SrcID:      "node-product",
		DstID:      "node-api",
		Kind:       "exposes_api",
		Confidence: 0.91,
		Provenance: "rule",
	}); err != nil {
		t.Fatalf("upsert graph edge: %v", err)
	}
	if err := store.UpsertEdge(ctx, storage.EdgeInput{
		ID:         "edge-api-module",
		SrcID:      "node-api",
		DstID:      "node-module",
		Kind:       "describes",
		Confidence: 0.87,
		Provenance: "rule",
	}); err != nil {
		t.Fatalf("upsert api module edge: %v", err)
	}
	if err := store.UpsertEdge(ctx, storage.EdgeInput{
		ID:         "edge-module-product",
		SrcID:      "node-module",
		DstID:      "node-product",
		Kind:       "depends_on",
		Confidence: 0.86,
		Provenance: "rule",
	}); err != nil {
		t.Fatalf("upsert module product edge: %v", err)
	}
}

func runDocGraph(t *testing.T, args ...string) string {
	t.Helper()

	return captureStdout(t, func() {
		allArgs := append([]string{"docgraph"}, args...)
		if err := run(allArgs); err != nil {
			t.Fatalf("run(%q) returned error: %v", strings.Join(allArgs, " "), err)
		}
	})
}

func runDocGraphError(args ...string) error {
	allArgs := append([]string{"docgraph"}, args...)
	return run(allArgs)
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe returned error: %v", err)
	}
	os.Stdout = w
	defer func() {
		os.Stdout = oldStdout
		_ = w.Close()
		_ = r.Close()
	}()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("stdout pipe Close returned error: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll stdout returned error: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("stdout pipe read Close returned error: %v", err)
	}
	return string(out)
}

func decodeJSON(t *testing.T, raw string, value any) {
	t.Helper()

	if err := json.Unmarshal([]byte(raw), value); err != nil {
		t.Fatalf("json.Unmarshal(%q) returned error: %v", raw, err)
	}
}

func assertCLISearchContains(t *testing.T, dataDir string, query string, wantTitle string) {
	t.Helper()

	out := runDocGraph(t, "search", "--data", dataDir, query)
	var result struct {
		Hits []domain.SearchHit `json:"hits"`
	}
	decodeJSON(t, out, &result)
	if len(result.Hits) == 0 {
		t.Fatalf("search %q returned no hits: %s", query, out)
	}
	if wantTitle != "" && result.Hits[0].DocumentTitle != wantTitle {
		t.Fatalf("search %q first title = %q, want %q", query, result.Hits[0].DocumentTitle, wantTitle)
	}
}

func assertCLISearchEmpty(t *testing.T, dataDir string, query string) {
	t.Helper()

	out := runDocGraph(t, "search", "--data", dataDir, query)
	var result struct {
		Hits []domain.SearchHit `json:"hits"`
	}
	decodeJSON(t, out, &result)
	for _, hit := range result.Hits {
		if strings.Contains(strings.ToLower(hit.Content), strings.ToLower(query)) {
			t.Fatalf("search %q returned stale hit %+v", query, hit)
		}
	}
}

func searchHitsContain(hits []domain.SearchHit, text string) bool {
	for _, hit := range hits {
		if strings.Contains(hit.Content, text) {
			return true
		}
	}
	return false
}

func contextSectionsContain(sections []query.ContextSection, text string) bool {
	for _, section := range sections {
		if strings.Contains(section.Content, text) {
			return true
		}
	}
	return false
}

type cliConfluenceMock struct {
	URL string
}

func newCLIConfluenceMock(t *testing.T, wantAuth string) *cliConfluenceMock {
	t.Helper()

	restore := confluence.SetHTTPClientFactoryForTest(func() *http.Client {
		return &http.Client{Transport: cliConfluenceRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			return cliConfluenceResponseViaRecorder(handleCLIConfluenceRequest(t, r, wantAuth)), nil
		})}
	})
	t.Cleanup(restore)
	return &cliConfluenceMock{URL: "https://confluence.example"}
}

type cliConfluenceRoundTripFunc func(*http.Request) (*http.Response, error)

func (f cliConfluenceRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func handleCLIConfluenceRequest(t *testing.T, r *http.Request, wantAuth string) *http.Response {
	t.Helper()

	if r.Header.Get("Authorization") != wantAuth {
		return cliConfluenceTextResponse(http.StatusUnauthorized, "unauthorized")
	}
	switch r.URL.Path {
	case "/wiki/rest/api/content/100":
		return cliConfluencePageResponse(t, "100", "CLI Confluence Root", 5, `<h1>Overview</h1><p>CLI cliconfluencerootalpha content.</p>`)
	case "/wiki/rest/api/content/100/child/page":
		return cliConfluenceJSONResponse(t, map[string]any{
			"results": []map[string]any{{"id": "200", "title": "CLI Confluence Child"}},
		})
	case "/wiki/rest/api/content/200":
		return cliConfluencePageResponse(t, "200", "CLI Confluence Child", 1, `<h1>Child</h1><p>CLI cliconfluencechildbeta content.</p>`)
	case "/wiki/rest/api/content/200/child/page":
		return cliConfluenceJSONResponse(t, map[string]any{"results": []map[string]any{}})
	default:
		return cliConfluenceTextResponse(http.StatusNotFound, "not found")
	}
}

func cliConfluencePageResponse(t *testing.T, id string, title string, version int, body string) *http.Response {
	t.Helper()

	return cliConfluenceJSONResponse(t, map[string]any{
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

func cliConfluenceJSONResponse(t *testing.T, body any) *http.Response {
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

func cliConfluenceTextResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func cliConfluenceResponseViaRecorder(resp *http.Response) *http.Response {
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

func initCLIGitDocsRepo(t *testing.T, files map[string]string) string {
	t.Helper()

	requireCLIGit(t)
	repo := t.TempDir()
	runCLIGit(t, repo, "init")
	runCLIGit(t, repo, "checkout", "-b", "main")
	runCLIGit(t, repo, "config", "user.email", "docgraph-tests@example.com")
	runCLIGit(t, repo, "config", "user.name", "DocGraph Tests")
	for path, content := range files {
		writeCLITestFile(t, filepath.Join(repo, filepath.FromSlash(path)), content)
	}
	commitCLIGitRepo(t, repo, "initial docs")
	return repo
}

func commitCLIGitRepo(t *testing.T, repo string, message string) {
	t.Helper()

	runCLIGit(t, repo, "add", "-A")
	runCLIGit(t, repo, "commit", "-m", message)
}

func requireCLIGit(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable not available")
	}
}

func runCLIGit(t *testing.T, repo string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func writeCLITestFile(t *testing.T, path string, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
}
