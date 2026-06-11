package sync

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/docgraph/docgraph/internal/domain"
	"github.com/docgraph/docgraph/internal/ingest/confluence"
	"github.com/docgraph/docgraph/internal/ingest/webdocs"
	"github.com/docgraph/docgraph/internal/query"
	"github.com/docgraph/docgraph/internal/storage/sqlite"
)

func TestLocalPathFromDSN(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "docs")

	tests := []struct {
		name    string
		dsn     string
		want    string
		wantErr string
	}{
		{
			name: "plain path",
			dsn:  filepath.Join("docs", "..", "product-docs"),
			want: filepath.Clean("product-docs"),
		},
		{
			name: "file dsn",
			dsn:  "file://" + abs,
			want: filepath.Clean(abs),
		},
		{
			name: "windows file uri maps to wsl mount",
			dsn:  "file:///C:/Users/me/docs",
			want: filepath.Clean("/mnt/c/Users/me/docs"),
		},
		{
			name: "windows drive path maps to wsl mount",
			dsn:  `C:\Users\me\docs`,
			want: filepath.Clean("/mnt/c/Users/me/docs"),
		},
		{
			name: "wsl localhost unc maps to linux path",
			dsn:  `\\wsl.localhost\Ubuntu\home\me\docs`,
			want: filepath.Clean("/home/me/docs"),
		},
		{
			name:    "empty dsn",
			dsn:     "  ",
			wantErr: "required",
		},
		{
			name:    "file dsn without path",
			dsn:     "file://",
			wantErr: "requires a path",
		},
		{
			name:    "unsupported scheme",
			dsn:     "https://docs.example.com",
			wantErr: "unsupported",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := LocalPathFromDSN(tt.dsn)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatal("LocalPathFromDSN returned nil error")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("LocalPathFromDSN error = %q, want substring %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LocalPathFromDSN returned error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("LocalPathFromDSN = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSyncSourceIndexesLocalMarkdownIntoSQLite(t *testing.T) {
	ctx := context.Background()
	docsDir := t.TempDir()
	writeFile(t, filepath.Join(docsDir, "member.md"), `# Member Benefits

Introductory membership entitlement overview.

## API

GET /member/benefits returns available member benefits.
`)
	writeFile(t, filepath.Join(docsDir, "notes.txt"), "not indexed")

	store, err := sqlite.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "docgraph.db"))
	if err != nil {
		t.Fatalf("sqlite.Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close returned error: %v", err)
		}
	})
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}

	_, err = store.CreateSource(ctx, domain.Source{
		ID:   "src_local",
		Kind: "local",
		Name: "Local Docs",
		DSN:  "file://" + docsDir,
	})
	if err != nil {
		t.Fatalf("CreateSource returned error: %v", err)
	}

	result, err := NewService(store).SyncSource(ctx, "src_local")
	if err != nil {
		t.Fatalf("SyncSource returned error: %v", err)
	}
	if result.SourceID != "src_local" {
		t.Fatalf("SourceID = %q, want src_local", result.SourceID)
	}
	if result.Documents != 1 {
		t.Fatalf("Documents = %d, want 1", result.Documents)
	}

	status, err := store.Status(ctx)
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if status.Documents != 1 {
		t.Fatalf("Documents status = %d, want 1", status.Documents)
	}
	if status.Sections != 2 {
		t.Fatalf("Sections status = %d, want 2", status.Sections)
	}

	hits, err := store.SearchSections(ctx, "member benefits", 10)
	if err != nil {
		t.Fatalf("SearchSections returned error: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("SearchSections returned no hits")
	}
	if hits[0].DocumentTitle != "Member Benefits" {
		t.Fatalf("hit DocumentTitle = %q, want Member Benefits", hits[0].DocumentTitle)
	}
	if !searchHitsContain(hits, "GET /member/benefits") {
		t.Fatalf("SearchSections hits = %#v, want API content", hits)
	}
}

func TestSyncSourceGeneratesProfileAndPreservesAdministratorDesc(t *testing.T) {
	ctx := context.Background()
	docsDir := t.TempDir()
	docPath := filepath.Join(docsDir, "auth.md")
	writeFile(t, docPath, `# 权限接口

## 错误响应

权限接口的几种错误响应包括 401 响应和 403 响应。
`)

	store, err := sqlite.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "docgraph.db"))
	if err != nil {
		t.Fatalf("sqlite.Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close returned error: %v", err)
		}
	})
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}
	if _, err := store.CreateSource(ctx, domain.Source{
		ID:   "src_profiles",
		Kind: "local",
		Name: "Profile Docs",
		DSN:  "file://" + docsDir,
	}); err != nil {
		t.Fatalf("CreateSource returned error: %v", err)
	}

	service := NewService(store)
	if _, err := service.SyncSource(ctx, "src_profiles"); err != nil {
		t.Fatalf("first SyncSource returned error: %v", err)
	}
	artifacts, err := store.ListSourceArtifacts(ctx, "src_profiles", 10, 0)
	if err != nil {
		t.Fatalf("ListSourceArtifacts returned error: %v", err)
	}
	if len(artifacts.Documents) != 1 {
		t.Fatalf("documents = %+v, want one document", artifacts.Documents)
	}
	docID := artifacts.Documents[0].ID
	profile, err := store.GetDocumentProfile(ctx, docID)
	if err != nil {
		t.Fatalf("GetDocumentProfile returned error: %v", err)
	}
	if profile.GeneratedFromHash != artifacts.Documents[0].ContentHash || !strings.Contains(profile.RetrievalProfileJSON, "错误响应") {
		t.Fatalf("generated profile = %+v, want generated hash and Chinese profile terms", profile)
	}

	if _, err := store.UpdateDocumentProfileDesc(ctx, domain.DocumentProfileInput{
		DocumentID: docID,
		Desc:       "管理员维护的文档说明",
	}); err != nil {
		t.Fatalf("UpdateDocumentProfileDesc returned error: %v", err)
	}
	writeFile(t, docPath, `# 权限接口

## 错误响应

权限接口的几种错误响应包括 401 响应、403 响应和 500 响应。
`)
	if _, err := service.SyncSource(ctx, "src_profiles"); err != nil {
		t.Fatalf("second SyncSource returned error: %v", err)
	}
	updatedArtifacts, err := store.ListSourceArtifacts(ctx, "src_profiles", 10, 0)
	if err != nil {
		t.Fatalf("second ListSourceArtifacts returned error: %v", err)
	}
	updatedProfile, err := store.GetDocumentProfile(ctx, docID)
	if err != nil {
		t.Fatalf("GetDocumentProfile after resync returned error: %v", err)
	}
	if updatedProfile.Desc != "管理员维护的文档说明" {
		t.Fatalf("desc after resync = %q, want preserved administrator desc", updatedProfile.Desc)
	}
	if updatedProfile.GeneratedFromHash != updatedArtifacts.Documents[0].ContentHash || updatedProfile.GeneratedFromHash == profile.GeneratedFromHash {
		t.Fatalf("generated hash after resync = %q, old %q, document hash %q", updatedProfile.GeneratedFromHash, profile.GeneratedFromHash, updatedArtifacts.Documents[0].ContentHash)
	}
}

func TestSyncSourceIndexesStaticDocsMixedFormatsAndRemovesDeletedDocs(t *testing.T) {
	ctx := context.Background()
	docsDir := t.TempDir()
	writeFile(t, filepath.Join(docsDir, "guide.md"), `# Static Guide

Static markdown staticsyncmarkdownalpha content.
`)
	writeFile(t, filepath.Join(docsDir, "index.html"), `<!doctype html>
<html>
  <head><title>Static HTML Docs</title></head>
  <body>
    <h1 id="home">Home</h1>
    <p>Static html staticsynchtmlbeta content.</p>
    <p>Read <a href="detail.html#details">details</a>.</p>
  </body>
</html>`)
	writeFile(t, filepath.Join(docsDir, "detail.html"), `<h1 id="details">Details</h1><p>Static detail staticsynclinkgamma content.</p>`)
	writeFile(t, filepath.Join(docsDir, "notes.txt"), "Static text staticsynctextdelta content.")
	writeFile(t, filepath.Join(docsDir, "draft.tmp"), "staticsyncignored")

	dbPath := filepath.Join(t.TempDir(), "docgraph.db")
	store, err := sqlite.Open(ctx, "sqlite://"+dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close returned error: %v", err)
		}
	})
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}
	_, err = store.CreateSource(ctx, domain.Source{
		ID:         "src_static_docs",
		Kind:       "static",
		Name:       "Static Docs",
		DSN:        "file://" + docsDir,
		ConfigJSON: `{"include":"**/*.md,**/*.html,**/*.txt"}`,
	})
	if err != nil {
		t.Fatalf("CreateSource returned error: %v", err)
	}

	service := NewService(store)
	result, err := service.SyncSource(ctx, "src_static_docs")
	if err != nil {
		t.Fatalf("Static SyncSource returned error: %v", err)
	}
	if result.SourceID != "src_static_docs" || result.Documents != 4 || result.JobID == "" {
		t.Fatalf("Static sync result = %+v, want four docs and job id", result)
	}
	assertSearchContains(t, ctx, store, "staticsyncmarkdownalpha", "Static Guide")
	assertSearchContains(t, ctx, store, "staticsynchtmlbeta", "Static HTML Docs")
	assertSearchContains(t, ctx, store, "staticsynctextdelta", "notes.txt")
	assertSearchEmpty(t, ctx, store, "staticsyncignored")

	contextPack, err := query.NewService(store).Context(ctx, query.ContextRequest{
		Task:        "staticsynctextdelta",
		MaxSections: 2,
		MaxChars:    1000,
	})
	if err != nil {
		t.Fatalf("Context returned error: %v", err)
	}
	if len(contextPack.Sections) == 0 || !contextSectionsContain(contextPack.Sections, "staticsynctextdelta") {
		t.Fatalf("Context sections = %+v, want txt content", contextPack.Sections)
	}

	graphDB := openGraphDB(t, dbPath)
	assertGraphCount(t, ctx, graphDB, 1, `
select count(*)
from edges
join nodes src on src.id = edges.src_id
join nodes dst on dst.id = edges.dst_id
where edges.kind = 'links_to'
  and src.kind = 'DocSection'
  and src.name = 'Home'
  and dst.kind = 'DocSection'
  and dst.name = 'Details'
`)

	if err := os.Remove(filepath.Join(docsDir, "notes.txt")); err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}
	result, err = service.SyncSource(ctx, "src_static_docs")
	if err != nil {
		t.Fatalf("Static SyncSource after delete returned error: %v", err)
	}
	if result.Documents != 3 || result.JobID == "" {
		t.Fatalf("Static sync after delete result = %+v, want three docs and job id", result)
	}
	assertSearchEmpty(t, ctx, store, "staticsynctextdelta")
}

func TestSyncSourceRemovesDeletedLocalMarkdownAndRecordsJobs(t *testing.T) {
	ctx := context.Background()
	docsDir := t.TempDir()
	removedPath := filepath.Join(docsDir, "removed.md")
	writeFile(t, removedPath, `# Removed Guide

Obsolete zebra-token lifecycle content.
`)
	writeFile(t, filepath.Join(docsDir, "kept.md"), `# Kept Guide

Current membership lifecycle content.
`)

	store, err := sqlite.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "docgraph.db"))
	if err != nil {
		t.Fatalf("sqlite.Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close returned error: %v", err)
		}
	})
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}
	_, err = store.CreateSource(ctx, domain.Source{
		ID:   "src_local_delete",
		Kind: "local",
		Name: "Local Docs",
		DSN:  "file://" + docsDir,
	})
	if err != nil {
		t.Fatalf("CreateSource returned error: %v", err)
	}

	service := NewService(store)
	first, err := service.SyncSource(ctx, "src_local_delete")
	if err != nil {
		t.Fatalf("first SyncSource returned error: %v", err)
	}
	if first.Documents != 2 || first.JobID == "" {
		t.Fatalf("first sync result = %+v, want 2 docs and job id", first)
	}
	hits, err := store.SearchSections(ctx, "zebra-token", 10)
	if err != nil {
		t.Fatalf("SearchSections before delete returned error: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("SearchSections before delete returned no removed-doc hit")
	}

	if err := os.Remove(removedPath); err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}
	second, err := service.SyncSource(ctx, "src_local_delete")
	if err != nil {
		t.Fatalf("second SyncSource returned error: %v", err)
	}
	if second.Documents != 1 || second.JobID == "" {
		t.Fatalf("second sync result = %+v, want 1 doc and job id", second)
	}
	hits, err = store.SearchSections(ctx, "zebra-token", 10)
	if err != nil {
		t.Fatalf("SearchSections after delete returned error: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("SearchSections after delete returned %+v, want no removed-doc hits", hits)
	}
	hits, err = store.SearchSections(ctx, "membership lifecycle", 10)
	if err != nil {
		t.Fatalf("SearchSections kept doc returned error: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("SearchSections kept doc returned no hits")
	}

	jobs, err := store.ListSyncJobs(ctx, "src_local_delete", 10)
	if err != nil {
		t.Fatalf("ListSyncJobs returned error: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("ListSyncJobs returned %d jobs, want 2: %+v", len(jobs), jobs)
	}
	for _, job := range jobs {
		if job.Status != "completed" || job.LastError != "" {
			t.Fatalf("sync job = %+v, want completed successful job", job)
		}
	}
}

func TestSyncSourceRecordsFailedJob(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "docgraph.db"))
	if err != nil {
		t.Fatalf("sqlite.Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close returned error: %v", err)
		}
	})
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}
	_, err = store.CreateSource(ctx, domain.Source{
		ID:   "src_local_fail",
		Kind: "local",
		Name: "Broken Local Docs",
		DSN:  "https://docs.example.com",
	})
	if err != nil {
		t.Fatalf("CreateSource returned error: %v", err)
	}

	if _, err := NewService(store).SyncSource(ctx, "src_local_fail"); err == nil {
		t.Fatalf("SyncSource returned nil error for unsupported DSN")
	}
	jobs, err := store.ListSyncJobs(ctx, "src_local_fail", 10)
	if err != nil {
		t.Fatalf("ListSyncJobs returned error: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("ListSyncJobs returned %d jobs, want 1: %+v", len(jobs), jobs)
	}
	if jobs[0].Status != "failed" || !strings.Contains(jobs[0].LastError, "unsupported") {
		t.Fatalf("failed sync job = %+v, want failed job with error", jobs[0])
	}
}

func TestSyncSourceCreatesLocalMarkdownGraphNodesAndContainsEdges(t *testing.T) {
	ctx := context.Background()
	docsDir := t.TempDir()
	writeFile(t, filepath.Join(docsDir, "member.md"), `# Member Benefits

Introductory membership entitlement overview.

## API

GET /member/benefits returns available member benefits.
`)

	dbPath := filepath.Join(t.TempDir(), "docgraph.db")
	store, err := sqlite.Open(ctx, "sqlite://"+dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close returned error: %v", err)
		}
	})
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}

	_, err = store.CreateSource(ctx, domain.Source{
		ID:          "src_local_graph",
		Kind:        "local",
		Name:        "Local Docs",
		DSN:         "file://" + docsDir,
		ProductHint: "Membership",
		ModuleHint:  "Benefits",
	})
	if err != nil {
		t.Fatalf("CreateSource returned error: %v", err)
	}

	service := NewService(store)
	for i := 0; i < 2; i++ {
		if _, err := service.SyncSource(ctx, "src_local_graph"); err != nil {
			t.Fatalf("SyncSource run %d returned error: %v", i+1, err)
		}
	}

	graphDB := openGraphDB(t, dbPath)

	assertGraphCount(t, ctx, graphDB, 1, `
select count(*)
from nodes
where kind = 'Document' and name = 'Member Benefits'
`)
	assertGraphCount(t, ctx, graphDB, 2, `
select count(*)
from nodes
where kind = 'DocSection' and name in ('Member Benefits', 'API')
`)
	assertGraphCount(t, ctx, graphDB, 2, `
select count(*)
from edges
join nodes src on src.id = edges.src_id
join nodes dst on dst.id = edges.dst_id
where edges.kind = 'contains'
  and src.kind = 'Document'
  and src.name = 'Member Benefits'
  and dst.kind = 'DocSection'
  and edges.evidence_section_id is not null
`)
}

func TestSyncSourceIndexesOpenAPIIntoSQLite(t *testing.T) {
	ctx := context.Background()
	specPath := filepath.Join(t.TempDir(), "membership.openapi.json")
	writeFile(t, specPath, `{
  "openapi": "3.0.3",
  "info": {
    "title": "Membership API",
    "version": "1.0.0"
  },
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

	store, err := sqlite.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "docgraph.db"))
	if err != nil {
		t.Fatalf("sqlite.Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close returned error: %v", err)
		}
	})
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}

	_, err = store.CreateSource(ctx, domain.Source{
		ID:   "src_openapi",
		Kind: "openapi",
		Name: "Membership API",
		DSN:  "file://" + specPath,
	})
	if err != nil {
		t.Fatalf("CreateSource returned error: %v", err)
	}

	result, err := NewService(store).SyncSource(ctx, "src_openapi")
	if err != nil {
		t.Fatalf("SyncSource returned error: %v", err)
	}
	if result.SourceID != "src_openapi" {
		t.Fatalf("SourceID = %q, want src_openapi", result.SourceID)
	}
	if result.Documents != 1 {
		t.Fatalf("Documents = %d, want 1", result.Documents)
	}

	status, err := store.Status(ctx)
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if status.Documents != 1 {
		t.Fatalf("Documents status = %d, want 1", status.Documents)
	}
	if status.Sections != 1 {
		t.Fatalf("Sections status = %d, want 1", status.Sections)
	}

	hits, err := store.SearchSections(ctx, "getMemberBenefits", 10)
	if err != nil {
		t.Fatalf("SearchSections returned error: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("SearchSections returned no hits")
	}
	if hits[0].DocumentTitle != "Membership API" {
		t.Fatalf("hit DocumentTitle = %q, want Membership API", hits[0].DocumentTitle)
	}
	if !searchHitsContain(hits, "GET /member/benefits") {
		t.Fatalf("SearchSections hits = %#v, want endpoint content", hits)
	}
}

func TestSyncSourceCreatesOpenAPIGraphAPIAndEvidenceEdges(t *testing.T) {
	ctx := context.Background()
	specPath := filepath.Join(t.TempDir(), "membership.openapi.json")
	writeFile(t, specPath, `{
  "openapi": "3.0.3",
  "info": {
    "title": "Membership API",
    "version": "1.0.0"
  },
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

	dbPath := filepath.Join(t.TempDir(), "docgraph.db")
	store, err := sqlite.Open(ctx, "sqlite://"+dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close returned error: %v", err)
		}
	})
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}

	_, err = store.CreateSource(ctx, domain.Source{
		ID:          "src_openapi_graph",
		Kind:        "openapi",
		Name:        "Membership API",
		DSN:         "file://" + specPath,
		ProductHint: "Membership",
	})
	if err != nil {
		t.Fatalf("CreateSource returned error: %v", err)
	}

	service := NewService(store)
	for i := 0; i < 2; i++ {
		if _, err := service.SyncSource(ctx, "src_openapi_graph"); err != nil {
			t.Fatalf("SyncSource run %d returned error: %v", i+1, err)
		}
	}

	graphDB := openGraphDB(t, dbPath)

	assertGraphCount(t, ctx, graphDB, 1, `
select count(*)
from nodes
where kind = 'API'
  and (name = 'GET /member/benefits' or canonical_name in ('GET /member/benefits', 'get /member/benefits'))
`)
	assertGraphCount(t, ctx, graphDB, 1, `
select count(*)
from edges
join nodes src on src.id = edges.src_id
join nodes dst on dst.id = edges.dst_id
where edges.kind = 'describes'
  and src.kind = 'DocSection'
  and dst.kind = 'API'
  and dst.name = 'GET /member/benefits'
  and edges.evidence_section_id is not null
`)
	assertGraphCount(t, ctx, graphDB, 1, `
select count(*)
from edges
join nodes src on src.id = edges.src_id
join nodes dst on dst.id = edges.dst_id
where edges.kind = 'exposes_api'
  and src.kind = 'Document'
  and src.name = 'Membership API'
  and dst.kind = 'API'
  and dst.name = 'GET /member/benefits'
  and edges.evidence_section_id is not null
`)
}

func TestSyncSourceIndexesOpenAPIYAMLSearchAndGraph(t *testing.T) {
	ctx := context.Background()
	specPath := filepath.Join(t.TempDir(), "membership.openapi.yaml")
	writeFile(t, specPath, openAPIYAMLFixture)

	dbPath := filepath.Join(t.TempDir(), "docgraph.db")
	store, err := sqlite.Open(ctx, "sqlite://"+dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close returned error: %v", err)
		}
	})
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}

	_, err = store.CreateSource(ctx, domain.Source{
		ID:   "src_openapi_yaml",
		Kind: "openapi",
		Name: "Membership YAML API",
		DSN:  "file://" + specPath,
	})
	if err != nil {
		t.Fatalf("CreateSource returned error: %v", err)
	}

	service := NewService(store)
	for i := 0; i < 2; i++ {
		result, err := service.SyncSource(ctx, "src_openapi_yaml")
		if err != nil {
			t.Fatalf("SyncSource run %d returned error: %v", i+1, err)
		}
		if result.Documents != 1 {
			t.Fatalf("SyncSource run %d Documents = %d, want 1", i+1, result.Documents)
		}
	}

	hits, err := store.SearchSections(ctx, "Member identifier from shared components", 10)
	if err != nil {
		t.Fatalf("SearchSections returned error: %v", err)
	}
	if !searchHitsContain(hits, "GET /member/benefits") || !searchHitsContain(hits, "memberId") {
		t.Fatalf("SearchSections hits = %#v, want YAML endpoint and resolved parameter content", hits)
	}

	graphDB := openGraphDB(t, dbPath)
	assertGraphCount(t, ctx, graphDB, 1, `
select count(*)
from nodes
where kind = 'API'
  and name = 'GET /member/benefits'
`)
	assertGraphCount(t, ctx, graphDB, 1, `
select count(*)
from edges
join nodes src on src.id = edges.src_id
join nodes dst on dst.id = edges.dst_id
where edges.kind = 'describes'
  and src.kind = 'DocSection'
  and dst.kind = 'API'
  and dst.name = 'GET /member/benefits'
  and edges.evidence_section_id is not null
`)
	assertGraphCount(t, ctx, graphDB, 1, `
select count(*)
from edges
join nodes src on src.id = edges.src_id
join nodes dst on dst.id = edges.dst_id
where edges.kind = 'exposes_api'
  and src.kind = 'Document'
  and src.name = 'Membership API'
  and dst.kind = 'API'
  and dst.name = 'GET /member/benefits'
  and edges.evidence_section_id is not null
`)
}

func TestSyncSourceIndexesGitMarkdownUpdatesAndRemovesDeletedDocs(t *testing.T) {
	ctx := context.Background()
	repo := initGitDocsRepo(t, map[string]string{
		"docs/member.md": `# Member Git Docs

Initial syncgitonealpha membership content.
`,
		"notes/internal.md": `# Internal Notes

This file has syncgitignoredtoken content outside the configured path and must not be indexed.
`,
	})

	store, err := sqlite.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "docgraph.db"))
	if err != nil {
		t.Fatalf("sqlite.Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close returned error: %v", err)
		}
	})
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}
	_, err = store.CreateSource(ctx, domain.Source{
		ID:         "src_git_docs",
		Kind:       "git",
		Name:       "Git Docs",
		DSN:        "file://" + filepath.ToSlash(repo),
		ConfigJSON: `{"branch":"main","path":"docs"}`,
	})
	if err != nil {
		t.Fatalf("CreateSource returned error: %v", err)
	}

	service := NewService(store)
	result, err := service.SyncSource(ctx, "src_git_docs")
	if err != nil {
		t.Fatalf("initial Git SyncSource returned error: %v", err)
	}
	if result.SourceID != "src_git_docs" || result.Documents != 1 || result.JobID == "" {
		t.Fatalf("initial Git sync result = %+v, want one document and job id", result)
	}
	assertSearchContains(t, ctx, store, "syncgitonealpha", "Member Git Docs")
	assertSearchEmpty(t, ctx, store, "syncgitignoredtoken")

	writeFile(t, filepath.Join(repo, "docs/member.md"), `# Member Git Docs

Updated syncgittwobeta membership content.
`)
	commitGitRepo(t, repo, "update docs")
	result, err = service.SyncSource(ctx, "src_git_docs")
	if err != nil {
		t.Fatalf("updated Git SyncSource returned error: %v", err)
	}
	if result.Documents != 1 || result.JobID == "" {
		t.Fatalf("updated Git sync result = %+v, want one document and job id", result)
	}
	assertSearchEmpty(t, ctx, store, "syncgitonealpha")
	assertSearchContains(t, ctx, store, "syncgittwobeta", "Member Git Docs")

	if err := os.Remove(filepath.Join(repo, "docs/member.md")); err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}
	commitGitRepo(t, repo, "remove docs")
	result, err = service.SyncSource(ctx, "src_git_docs")
	if err != nil {
		t.Fatalf("deleted Git SyncSource returned error: %v", err)
	}
	if result.Documents != 0 || result.JobID == "" {
		t.Fatalf("deleted Git sync result = %+v, want zero documents and job id", result)
	}
	assertSearchEmpty(t, ctx, store, "syncgittwobeta")
}

func TestSyncSourceGitReportsFailedJobsForInvalidBranchAndPath(t *testing.T) {
	ctx := context.Background()
	repo := initGitDocsRepo(t, map[string]string{
		"docs/member.md": `# Member Git Docs

Git branch and path failure fixture.
`,
	})

	tests := []struct {
		name       string
		sourceID   string
		configJSON string
		wantErr    string
	}{
		{
			name:       "invalid branch",
			sourceID:   "src_git_bad_branch",
			configJSON: `{"branch":"missing-branch","path":"docs"}`,
			wantErr:    "missing-branch",
		},
		{
			name:       "invalid path",
			sourceID:   "src_git_bad_path",
			configJSON: `{"branch":"main","path":"missing-docs"}`,
			wantErr:    "missing-docs",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, err := sqlite.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "docgraph.db"))
			if err != nil {
				t.Fatalf("sqlite.Open returned error: %v", err)
			}
			t.Cleanup(func() {
				if err := store.Close(); err != nil {
					t.Fatalf("store.Close returned error: %v", err)
				}
			})
			if err := store.Migrate(ctx); err != nil {
				t.Fatalf("Migrate returned error: %v", err)
			}
			_, err = store.CreateSource(ctx, domain.Source{
				ID:         tt.sourceID,
				Kind:       "git",
				Name:       "Broken Git Docs",
				DSN:        "file://" + filepath.ToSlash(repo),
				ConfigJSON: tt.configJSON,
			})
			if err != nil {
				t.Fatalf("CreateSource returned error: %v", err)
			}

			_, err = NewService(store).SyncSource(ctx, tt.sourceID)
			if err == nil {
				t.Fatalf("SyncSource returned nil error for %s", tt.name)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("SyncSource error = %q, want substring %q", err.Error(), tt.wantErr)
			}
			jobs, err := store.ListSyncJobs(ctx, tt.sourceID, 10)
			if err != nil {
				t.Fatalf("ListSyncJobs returned error: %v", err)
			}
			if len(jobs) != 1 || jobs[0].Status != "failed" || !strings.Contains(jobs[0].LastError, tt.wantErr) {
				t.Fatalf("failed jobs = %+v, want one failed job mentioning %q", jobs, tt.wantErr)
			}
		})
	}
}

func TestSyncSourceIndexesHTMLSearchContextLinksAndRemovesDeletedDocs(t *testing.T) {
	ctx := context.Background()
	docsDir := t.TempDir()
	writeFile(t, filepath.Join(docsDir, "index.html"), `<!doctype html>
<html>
  <head><title>Member HTML Docs</title></head>
  <body>
    <h1 id="overview">Member Portal</h1>
    <p>HTML sync overview htmlsyncoverviewtoken for product teams.</p>
    <main id="main-content"></main>
    <h2 id="api">API Usage</h2>
    <p>Call <code>GET /html/member</code> for htmlsyncapitoken.</p>
    <code>curl /html/member htmlsynccodetoken</code>
    <p>Read <a href="details.html#quota">quota details</a> and <a href="index#main-content">main content</a>.</p>
  </body>
</html>`)
	writeFile(t, filepath.Join(docsDir, "details.html"), `<!doctype html>
<html>
  <head><title>Member Detail Docs</title></head>
  <body>
    <h1 id="quota">Quota Details</h1>
    <p>HTML quota htmlsyncquotatoken policy text.</p>
  </body>
</html>`)

	dbPath := filepath.Join(t.TempDir(), "docgraph.db")
	store, err := sqlite.Open(ctx, "sqlite://"+dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close returned error: %v", err)
		}
	})
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}
	_, err = store.CreateSource(ctx, domain.Source{
		ID:   "src_html_docs",
		Kind: "html",
		Name: "HTML Docs",
		DSN:  docsDir,
	})
	if err != nil {
		t.Fatalf("CreateSource returned error: %v", err)
	}

	service := NewService(store)
	for i := 0; i < 2; i++ {
		result, err := service.SyncSource(ctx, "src_html_docs")
		if err != nil {
			t.Fatalf("HTML SyncSource pass %d returned error: %v", i+1, err)
		}
		if result.SourceID != "src_html_docs" || result.Documents != 2 || result.JobID == "" {
			t.Fatalf("HTML sync result = %+v, want two documents and job id", result)
		}
		if len(result.BrokenLinks) != 0 {
			t.Fatalf("HTML sync broken links = %+v, want none", result.BrokenLinks)
		}
	}
	assertSearchContains(t, ctx, store, "htmlsyncapitoken", "Member HTML Docs")
	assertSearchContains(t, ctx, store, "htmlsynccodetoken", "Member HTML Docs")
	assertSearchContains(t, ctx, store, "htmlsyncquotatoken", "Member Detail Docs")

	contextPack, err := query.NewService(store).Context(ctx, query.ContextRequest{
		Task:        "htmlsynccodetoken",
		MaxSections: 3,
		MaxChars:    4000,
	})
	if err != nil {
		t.Fatalf("Context returned error: %v", err)
	}
	if len(contextPack.Sections) == 0 || !contextSectionsContain(contextPack.Sections, "curl /html/member htmlsynccodetoken") {
		t.Fatalf("Context sections = %+v, want code content", contextPack.Sections)
	}

	graphDB := openGraphDB(t, dbPath)
	assertGraphCount(t, ctx, graphDB, 1, `
select count(*)
from edges
join nodes src on src.id = edges.src_id
join nodes dst on dst.id = edges.dst_id
where edges.kind = 'links_to'
  and src.kind = 'DocSection'
  and src.name = 'API Usage'
  and dst.kind = 'DocSection'
  and dst.name = 'Quota Details'
  and edges.evidence_section_id is not null
`)

	if err := os.Remove(filepath.Join(docsDir, "details.html")); err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}
	result, err := service.SyncSource(ctx, "src_html_docs")
	if err != nil {
		t.Fatalf("HTML SyncSource after delete returned error: %v", err)
	}
	if result.Documents != 1 || result.JobID == "" {
		t.Fatalf("HTML sync after delete result = %+v, want one document and job id", result)
	}
	if len(result.BrokenLinks) != 1 || result.BrokenLinks[0].Href != "details.html#quota" {
		t.Fatalf("HTML sync broken links = %+v, want missing details link", result.BrokenLinks)
	}
	jobs, err := store.ListSyncJobs(ctx, "src_html_docs", 1)
	if err != nil {
		t.Fatalf("ListSyncJobs after broken link sync returned error: %v", err)
	}
	if len(jobs) != 1 || !strings.Contains(jobs[0].PayloadJSON, `"broken_links"`) || !strings.Contains(jobs[0].PayloadJSON, `details.html#quota`) {
		t.Fatalf("HTML sync job payload = %+v, want broken link details", jobs)
	}
	assertSearchEmpty(t, ctx, store, "htmlsyncquotatoken")
	assertSearchContains(t, ctx, store, "htmlsyncapitoken", "Member HTML Docs")
}

func TestSyncSourceResolvesDocumentCenterRelativeAnchors(t *testing.T) {
	ctx := context.Background()
	docsDir := t.TempDir()
	writeFile(t, filepath.Join(docsDir, "cooperate", "bug", "index.html"), `<!doctype html>
<html>
  <head><title>如何给平台提BUGS | 宙斯平台文档中心</title></head>
  <body>
    <h1>如何给平台提BUGS</h1>
    <main id="main-content">
      <p>Report bugs bugdocmaincontenttoken.</p>
      <p><a href="cooperate/bug/index.html#main-content">main content</a></p>
    </main>
  </body>
</html>`)

	store, err := sqlite.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "docgraph.db"))
	if err != nil {
		t.Fatalf("sqlite.Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close returned error: %v", err)
		}
	})
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}
	_, err = store.CreateSource(ctx, domain.Source{
		ID:   "src_relative_anchor_docs",
		Kind: "html",
		Name: "Relative Anchor Docs",
		DSN:  docsDir,
	})
	if err != nil {
		t.Fatalf("CreateSource returned error: %v", err)
	}

	result, err := NewService(store).SyncSource(ctx, "src_relative_anchor_docs")
	if err != nil {
		t.Fatalf("SyncSource returned error: %v", err)
	}
	if result.Documents != 1 || len(result.BrokenLinks) != 0 {
		t.Fatalf("SyncSource result = %+v, want one document and no broken links", result)
	}
}

func TestSyncSourceRejectsDuplicateRunningJob(t *testing.T) {
	ctx := context.Background()
	docsDir := t.TempDir()
	writeFile(t, filepath.Join(docsDir, "index.md"), "# Docs\n\nsync duplicate token\n")

	store, err := sqlite.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "docgraph.db"))
	if err != nil {
		t.Fatalf("sqlite.Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close returned error: %v", err)
		}
	})
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}
	_, err = store.CreateSource(ctx, domain.Source{
		ID:   "src_duplicate_sync",
		Kind: "local",
		Name: "Duplicate Sync Docs",
		DSN:  docsDir,
	})
	if err != nil {
		t.Fatalf("CreateSource returned error: %v", err)
	}
	if _, err := store.CreateSyncJob(ctx, "src_duplicate_sync"); err != nil {
		t.Fatalf("CreateSyncJob returned error: %v", err)
	}

	_, err = NewService(store).SyncSource(ctx, "src_duplicate_sync")
	if !errors.Is(err, domain.ErrSyncInProgress) {
		t.Fatalf("SyncSource error = %v, want ErrSyncInProgress", err)
	}
	jobs, err := store.ListSyncJobs(ctx, "src_duplicate_sync", 10)
	if err != nil {
		t.Fatalf("ListSyncJobs returned error: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Status != "running" {
		t.Fatalf("jobs = %+v, want only original running job", jobs)
	}
}

func TestSyncSourceIndexesWebDocsURLTreeSearchAndGraph(t *testing.T) {
	ctx := context.Background()
	restore := webdocs.SetHTTPClientFactoryForTest(func() *http.Client {
		return &http.Client{Transport: webdocsRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch r.URL.Path {
			case "/docs":
				return webdocsHTMLResponse(`<!doctype html>
<html>
  <head><title>Web Docs Home</title></head>
  <body>
    <h1 id="home">Home</h1>
    <p>Online docs webdocssyncrootalpha content.</p>
    <p><a href="/docs/guide#start">Guide</a> <a href="/docs/missing/">Missing</a></p>
  </body>
</html>`), nil
			case "/docs/guide":
				return webdocsHTMLResponse(`<!doctype html>
<html>
  <head><title>Web Docs Guide</title></head>
  <body><h1 id="start">Guide</h1><p>Online docs webdocssyncchildbeta content.</p></body>
</html>`), nil
			case "/docs/missing/":
				return confluenceTextResponse(http.StatusNotFound, "not found"), nil
			default:
				return confluenceTextResponse(http.StatusNotFound, "not found"), nil
			}
		})}
	})
	t.Cleanup(restore)

	dbPath := filepath.Join(t.TempDir(), "docgraph.db")
	store, err := sqlite.Open(ctx, "sqlite://"+dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close returned error: %v", err)
		}
	})
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}
	_, err = store.CreateSource(ctx, domain.Source{
		ID:         "src_webdocs",
		Kind:       "webdocs",
		Name:       "Web Docs",
		DSN:        "https://docs.example/docs",
		ConfigJSON: `{"max_pages":"10","max_depth":"3"}`,
	})
	if err != nil {
		t.Fatalf("CreateSource returned error: %v", err)
	}

	result, err := NewService(store).SyncSource(ctx, "src_webdocs")
	if err != nil {
		t.Fatalf("WebDocs SyncSource returned error: %v", err)
	}
	if result.SourceID != "src_webdocs" || result.Documents != 2 || result.JobID == "" {
		t.Fatalf("WebDocs sync result = %+v, want two documents and job id", result)
	}
	assertSearchContains(t, ctx, store, "webdocssyncrootalpha", "Web Docs Home")
	assertSearchContains(t, ctx, store, "webdocssyncchildbeta", "Web Docs Guide")

	graphDB := openGraphDB(t, dbPath)
	assertGraphCount(t, ctx, graphDB, 1, `
select count(*)
from edges
join nodes src on src.id = edges.src_id
join nodes dst on dst.id = edges.dst_id
where edges.kind = 'links_to'
  and src.kind = 'DocSection'
  and src.name = 'Home'
  and dst.kind = 'DocSection'
  and dst.name = 'Guide'
`)
}

func TestSyncSourceIndexesConfluencePagesChildrenSearchContextAndGraph(t *testing.T) {
	ctx := context.Background()
	server, childRequests := newConfluenceMockServer(t, "Bearer sync-secret")

	dbPath := filepath.Join(t.TempDir(), "docgraph.db")
	store, err := sqlite.Open(ctx, "sqlite://"+dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close returned error: %v", err)
		}
	})
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}
	_, err = store.CreateSource(ctx, domain.Source{
		ID:   "src_confluence_docs",
		Kind: "confluence",
		Name: "Confluence Docs",
		DSN:  server.URL + "/wiki",
		ConfigJSON: confluenceTestConfig(t, map[string]any{
			"base_url":         server.URL + "/wiki",
			"page_id":          "100",
			"token":            "sync-secret",
			"include_children": true,
		}),
		ProductHint: "Membership",
	})
	if err != nil {
		t.Fatalf("CreateSource returned error: %v", err)
	}

	service := NewService(store)
	for i := 0; i < 2; i++ {
		result, err := service.SyncSource(ctx, "src_confluence_docs")
		if err != nil {
			t.Fatalf("Confluence SyncSource pass %d returned error: %v", i+1, err)
		}
		if result.SourceID != "src_confluence_docs" || result.Documents != 2 || result.JobID == "" {
			t.Fatalf("Confluence sync result = %+v, want two documents and job id", result)
		}
	}
	if childRequests.Load() == 0 {
		t.Fatalf("Confluence child endpoint was not requested")
	}
	assertSearchContains(t, ctx, store, "confluencesyncrootalpha", "Member Confluence Root")
	assertSearchContains(t, ctx, store, "confluencesyncchildbeta", "Member Confluence Child")
	assertSearchContains(t, ctx, store, "confluencesyncapi", "Member Confluence Root")

	contextPack, err := query.NewService(store).Context(ctx, query.ContextRequest{
		Task:        "confluencesyncchildbeta",
		MaxSections: 3,
		MaxChars:    4000,
	})
	if err != nil {
		t.Fatalf("Context returned error: %v", err)
	}
	if len(contextPack.Sections) == 0 || !contextSectionsContain(contextPack.Sections, "confluencesyncchildbeta") {
		t.Fatalf("Context sections = %+v, want Confluence child content", contextPack.Sections)
	}

	graphDB := openGraphDB(t, dbPath)
	assertGraphCount(t, ctx, graphDB, 2, `
select count(*)
from nodes
where kind = 'Document'
  and name in ('Member Confluence Root', 'Member Confluence Child')
`)
	assertGraphCount(t, ctx, graphDB, 1, `
select count(*)
from edges
join nodes src on src.id = edges.src_id
join nodes dst on dst.id = edges.dst_id
where edges.kind = 'exposes_api'
  and src.kind = 'Product'
  and src.name = 'Membership'
  and dst.kind = 'API'
  and dst.name = 'GET /confluence/member'
`)
}

func TestSyncSourceConfluenceFailuresWriteFailedJobs(t *testing.T) {
	ctx := context.Background()
	server, _ := newConfluenceMockServer(t, "Bearer good-secret")

	tests := []struct {
		name       string
		sourceID   string
		configJSON string
		wantErr    string
	}{
		{
			name:     "auth failure",
			sourceID: "src_confluence_bad_auth",
			configJSON: confluenceTestConfig(t, map[string]any{
				"base_url": server.URL + "/wiki",
				"page_id":  "100",
				"token":    "bad-secret",
			}),
			wantErr: "auth",
		},
		{
			name:     "missing page",
			sourceID: "src_confluence_missing_page",
			configJSON: confluenceTestConfig(t, map[string]any{
				"base_url": server.URL + "/wiki",
				"page_id":  "404",
				"token":    "good-secret",
			}),
			wantErr: "404",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, err := sqlite.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "docgraph.db"))
			if err != nil {
				t.Fatalf("sqlite.Open returned error: %v", err)
			}
			t.Cleanup(func() {
				if err := store.Close(); err != nil {
					t.Fatalf("store.Close returned error: %v", err)
				}
			})
			if err := store.Migrate(ctx); err != nil {
				t.Fatalf("Migrate returned error: %v", err)
			}
			_, err = store.CreateSource(ctx, domain.Source{
				ID:         tt.sourceID,
				Kind:       "confluence",
				Name:       "Broken Confluence Docs",
				DSN:        server.URL + "/wiki",
				ConfigJSON: tt.configJSON,
			})
			if err != nil {
				t.Fatalf("CreateSource returned error: %v", err)
			}

			_, err = NewService(store).SyncSource(ctx, tt.sourceID)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("SyncSource error = %v, want substring %q", err, tt.wantErr)
			}
			jobs, err := store.ListSyncJobs(ctx, tt.sourceID, 10)
			if err != nil {
				t.Fatalf("ListSyncJobs returned error: %v", err)
			}
			if len(jobs) != 1 || jobs[0].Status != "failed" || !strings.Contains(jobs[0].LastError, tt.wantErr) {
				t.Fatalf("failed jobs = %+v, want one failed job mentioning %q", jobs, tt.wantErr)
			}
		})
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

func assertSearchContains(t *testing.T, ctx context.Context, store *sqlite.Store, query string, wantTitle string) {
	t.Helper()

	hits, err := store.SearchSections(ctx, query, 10)
	if err != nil {
		t.Fatalf("SearchSections(%q) returned error: %v", query, err)
	}
	if len(hits) == 0 {
		t.Fatalf("SearchSections(%q) returned no hits", query)
	}
	if wantTitle != "" && hits[0].DocumentTitle != wantTitle {
		t.Fatalf("SearchSections(%q) first title = %q, want %q", query, hits[0].DocumentTitle, wantTitle)
	}
}

func assertSearchEmpty(t *testing.T, ctx context.Context, store *sqlite.Store, query string) {
	t.Helper()

	hits, err := store.SearchSections(ctx, query, 10)
	if err != nil {
		t.Fatalf("SearchSections(%q) returned error: %v", query, err)
	}
	if len(hits) != 0 {
		t.Fatalf("SearchSections(%q) returned %+v, want no hits", query, hits)
	}
}

func openGraphDB(t *testing.T, dbPath string) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("graph db Close returned error: %v", err)
		}
	})
	return db
}

func assertGraphCount(t *testing.T, ctx context.Context, db *sql.DB, want int64, query string, args ...any) {
	t.Helper()

	var got int64
	if err := db.QueryRowContext(ctx, query, args...).Scan(&got); err != nil {
		t.Fatalf("graph count query failed: %v", err)
	}
	if got != want {
		t.Fatalf("graph count = %d, want %d for query:\n%s", got, want, strings.TrimSpace(query))
	}
}

type confluenceMockServer struct {
	URL string
}

func newConfluenceMockServer(t *testing.T, wantAuth string) (*confluenceMockServer, *atomic.Int32) {
	t.Helper()

	var childRequests atomic.Int32
	restore := confluence.SetHTTPClientFactoryForTest(func() *http.Client {
		return &http.Client{Transport: confluenceRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			return confluenceResponseViaRecorder(handleConfluenceMockRequest(t, r, wantAuth, &childRequests)), nil
		})}
	})
	t.Cleanup(restore)
	return &confluenceMockServer{URL: "https://confluence.example"}, &childRequests
}

type confluenceRoundTripFunc func(*http.Request) (*http.Response, error)

func (f confluenceRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type webdocsRoundTripFunc func(*http.Request) (*http.Response, error)

func (f webdocsRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func handleConfluenceMockRequest(t *testing.T, r *http.Request, wantAuth string, childRequests *atomic.Int32) *http.Response {
	t.Helper()

	if r.Header.Get("Authorization") != wantAuth {
		return confluenceTextResponse(http.StatusUnauthorized, "unauthorized")
	}
	switch r.URL.Path {
	case "/wiki/rest/api/content/100":
		return confluencePageResponse(t, "100", "Member Confluence Root", 3, `<h1>Overview</h1><p>Root confluencesyncrootalpha content. GET /confluence/member returns confluencesyncapi.</p>`)
	case "/wiki/rest/api/content/100/child/page":
		childRequests.Add(1)
		return confluenceJSONResponse(t, map[string]any{
			"results": []map[string]any{{"id": "200", "title": "Member Confluence Child"}},
		})
	case "/wiki/rest/api/content/200":
		return confluencePageResponse(t, "200", "Member Confluence Child", 1, `<h1>Child</h1><p>Child confluencesyncchildbeta policy details.</p>`)
	case "/wiki/rest/api/content/200/child/page":
		return confluenceJSONResponse(t, map[string]any{"results": []map[string]any{}})
	default:
		return confluenceTextResponse(http.StatusNotFound, "not found")
	}
}

func confluencePageResponse(t *testing.T, id string, title string, version int, body string) *http.Response {
	t.Helper()

	return confluenceJSONResponse(t, map[string]any{
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

func confluenceJSONResponse(t *testing.T, body any) *http.Response {
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

func confluenceTextResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func webdocsHTMLResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/html"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func confluenceResponseViaRecorder(resp *http.Response) *http.Response {
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

func confluenceTestConfig(t *testing.T, value map[string]any) string {
	t.Helper()

	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal confluence config: %v", err)
	}
	return string(data)
}

func initGitDocsRepo(t *testing.T, files map[string]string) string {
	t.Helper()

	requireGit(t)
	repo := t.TempDir()
	runGit(t, repo, "init")
	runGit(t, repo, "checkout", "-b", "main")
	runGit(t, repo, "config", "user.email", "docgraph-tests@example.com")
	runGit(t, repo, "config", "user.name", "DocGraph Tests")
	for path, content := range files {
		writeFile(t, filepath.Join(repo, filepath.FromSlash(path)), content)
	}
	commitGitRepo(t, repo, "initial docs")
	return repo
}

func commitGitRepo(t *testing.T, repo string, message string) {
	t.Helper()

	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", message)
}

func requireGit(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable not available")
	}
}

func runGit(t *testing.T, repo string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
}

const openAPIYAMLFixture = `openapi: 3.0.3
info:
  title: Membership API
  version: 1.0.0
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
      content:
        application/json:
          schema:
            type: object
`

func writeFile(t *testing.T, path string, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
}
