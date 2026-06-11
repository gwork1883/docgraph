package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docgraph/docgraph/internal/domain"
)

func TestPathFromDSN(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "docgraph.db")

	tests := []struct {
		name    string
		dsn     string
		want    string
		wantErr string
	}{
		{
			name: "relative path",
			dsn:  "sqlite://.docgraph/../docgraph.db",
			want: filepath.Clean("docgraph.db"),
		},
		{
			name: "absolute path",
			dsn:  "sqlite://" + abs,
			want: abs,
		},
		{
			name:    "unsupported scheme",
			dsn:     "postgres://localhost/docgraph",
			wantErr: "invalid sqlite DSN",
		},
		{
			name:    "empty path",
			dsn:     "sqlite://",
			wantErr: "requires a path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := pathFromDSN(tt.dsn)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatal("pathFromDSN returned nil error")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("pathFromDSN error = %q, want substring %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("pathFromDSN returned error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("pathFromDSN = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOpenCreatesParentDirectory(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "nested", "data", "docgraph.db")

	store, err := Open(ctx, "sqlite://"+dbPath)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if _, err := os.Stat(filepath.Dir(dbPath)); err != nil {
		t.Fatalf("parent directory was not created: %v", err)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("database file was not created: %v", err)
	}
}

func TestStatusRequiresMigration(t *testing.T) {
	ctx := context.Background()
	store := openTempStore(t, ctx)

	_, err := store.Status(ctx)
	if err == nil {
		t.Fatal("Status returned nil error before migration")
	}
}

func TestMigrateAndStatus(t *testing.T) {
	ctx := context.Background()
	store := openTempStore(t, ctx)
	dsn := store.dsn

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate returned error: %v", err)
	}

	status, err := store.Status(ctx)
	if err != nil {
		t.Fatalf("Status returned error after migration: %v", err)
	}
	if status.StorageDSN != dsn {
		t.Fatalf("StorageDSN = %q, want %q", status.StorageDSN, dsn)
	}
	if status.Sources != 0 || status.Documents != 0 || status.Sections != 0 ||
		status.Nodes != 0 || status.Edges != 0 || status.Jobs != 0 {
		t.Fatalf("empty migrated status = %#v, want zero counts", status)
	}

	insertFixtureRows(t, ctx, store)

	status, err = store.Status(ctx)
	if err != nil {
		t.Fatalf("Status returned error with fixture rows: %v", err)
	}
	if status.Sources != 1 {
		t.Fatalf("Sources = %d, want 1", status.Sources)
	}
	if status.Documents != 1 {
		t.Fatalf("Documents = %d, want 1", status.Documents)
	}
	if status.Sections != 1 {
		t.Fatalf("Sections = %d, want 1", status.Sections)
	}
	if status.Nodes != 2 {
		t.Fatalf("Nodes = %d, want 2", status.Nodes)
	}
	if status.Edges != 1 {
		t.Fatalf("Edges = %d, want 1", status.Edges)
	}
	if status.Jobs != 1 {
		t.Fatalf("Jobs = %d, want 1", status.Jobs)
	}
}

func TestCreateListAndGetSource(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)

	source := domain.Source{
		ID:           "source-1",
		Kind:         "local",
		Name:         "Product Docs",
		DSN:          "file:///workspace/docs",
		ProductHint:  "Payments",
		ModuleHint:   "Checkout",
		SyncSchedule: "manual",
	}
	created, err := store.CreateSource(ctx, source)
	if err != nil {
		t.Fatalf("CreateSource returned error: %v", err)
	}
	if created.ID != source.ID {
		t.Fatalf("created ID = %q, want %q", created.ID, source.ID)
	}
	if created.ConfigJSON != "{}" {
		t.Fatalf("created ConfigJSON = %q, want {}", created.ConfigJSON)
	}
	if created.ProductHint != source.ProductHint {
		t.Fatalf("created ProductHint = %q, want %q", created.ProductHint, source.ProductHint)
	}
	if created.CreatedAt == "" || created.UpdatedAt == "" {
		t.Fatalf("created timestamps should be populated: %#v", created)
	}

	got, err := store.GetSource(ctx, source.ID)
	if err != nil {
		t.Fatalf("GetSource returned error: %v", err)
	}
	if got.Name != source.Name {
		t.Fatalf("GetSource Name = %q, want %q", got.Name, source.Name)
	}
	if got.DSN != source.DSN {
		t.Fatalf("GetSource DSN = %q, want %q", got.DSN, source.DSN)
	}

	updated := source
	updated.Name = "Updated Product Docs"
	updated.ConfigJSON = `{"branch":"main"}`
	updated.ModuleHint = "Billing"
	created, err = store.CreateSource(ctx, updated)
	if err != nil {
		t.Fatalf("CreateSource update returned error: %v", err)
	}
	if created.Name != updated.Name {
		t.Fatalf("updated Name = %q, want %q", created.Name, updated.Name)
	}
	if created.ConfigJSON != updated.ConfigJSON {
		t.Fatalf("updated ConfigJSON = %q, want %q", created.ConfigJSON, updated.ConfigJSON)
	}
	if created.ModuleHint != updated.ModuleHint {
		t.Fatalf("updated ModuleHint = %q, want %q", created.ModuleHint, updated.ModuleHint)
	}

	sources, err := store.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources returned error: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("ListSources returned %d sources, want 1: %#v", len(sources), sources)
	}
	if sources[0].ID != source.ID || sources[0].Name != updated.Name {
		t.Fatalf("ListSources[0] = %#v, want updated source %q", sources[0], source.ID)
	}
}

func TestUpdateDeleteSourceAndSyncJobsLifecycle(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)

	_, err := store.CreateSource(ctx, domain.Source{
		ID:          "source-life",
		Kind:        "local",
		Name:        "Docs",
		DSN:         "file:///docs",
		ProductHint: "Membership",
		ModuleHint:  "Benefits",
	})
	if err != nil {
		t.Fatalf("CreateSource returned error: %v", err)
	}
	updated, err := store.UpdateSource(ctx, domain.Source{
		ID:          "source-life",
		Kind:        "local",
		Name:        "Updated Docs",
		DSN:         "file:///updated-docs",
		ConfigJSON:  `{"branch":"main"}`,
		ProductHint: "Payments",
		ModuleHint:  "Checkout",
	})
	if err != nil {
		t.Fatalf("UpdateSource returned error: %v", err)
	}
	if updated.Name != "Updated Docs" || updated.DSN != "file:///updated-docs" || updated.ProductHint != "Payments" || updated.ModuleHint != "Checkout" {
		t.Fatalf("updated source = %+v, want updated name/dsn/product/module", updated)
	}

	if err := store.ReplaceDocument(ctx, domain.DocumentInput{
		ID:          "doc-life",
		SourceID:    "source-life",
		ExternalID:  "life.md",
		Title:       "Lifecycle",
		ContentHash: "hash-doc",
	}, []domain.SectionInput{
		{
			ID:          "section-life",
			Title:       "Lifecycle",
			Content:     "membership lifecycle search content",
			ContentHash: "hash-section",
		},
	}); err != nil {
		t.Fatalf("ReplaceDocument returned error: %v", err)
	}
	hits, err := store.SearchSections(ctx, "membership lifecycle", 10)
	if err != nil {
		t.Fatalf("SearchSections before delete returned error: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("SearchSections before delete returned no hits")
	}

	job, err := store.CreateSyncJob(ctx, "source-life")
	if err != nil {
		t.Fatalf("CreateSyncJob returned error: %v", err)
	}
	if job.Status != "running" || !strings.Contains(job.PayloadJSON, `"source_id":"source-life"`) {
		t.Fatalf("created sync job = %+v, want running source job", job)
	}
	if err := store.CompleteSyncJob(ctx, job.ID, domain.ResultPayload{Documents: 1}); err != nil {
		t.Fatalf("CompleteSyncJob returned error: %v", err)
	}
	failed, err := store.CreateSyncJob(ctx, "source-life")
	if err != nil {
		t.Fatalf("second CreateSyncJob returned error: %v", err)
	}
	if err := store.FailSyncJob(ctx, failed.ID, "boom"); err != nil {
		t.Fatalf("FailSyncJob returned error: %v", err)
	}
	jobs, err := store.ListSyncJobs(ctx, "source-life", 10)
	if err != nil {
		t.Fatalf("ListSyncJobs returned error: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("ListSyncJobs returned %d jobs, want 2: %+v", len(jobs), jobs)
	}
	if !syncJobsContain(jobs, "completed", `"documents":1`) || !syncJobsContain(jobs, "failed", "boom") {
		t.Fatalf("sync jobs = %+v, want completed and failed job history", jobs)
	}
	if err := store.DeleteSyncJob(ctx, "source-life", failed.ID); err != nil {
		t.Fatalf("DeleteSyncJob returned error: %v", err)
	}
	jobs, err = store.ListSyncJobs(ctx, "source-life", 10)
	if err != nil {
		t.Fatalf("ListSyncJobs after DeleteSyncJob returned error: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID == failed.ID {
		t.Fatalf("jobs after DeleteSyncJob = %+v, want failed job removed", jobs)
	}

	if err := store.DeleteSource(ctx, "source-life"); err != nil {
		t.Fatalf("DeleteSource returned error: %v", err)
	}
	sources, err := store.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources after delete returned error: %v", err)
	}
	if len(sources) != 0 {
		t.Fatalf("ListSources after delete returned %+v, want empty", sources)
	}
	assertCount(t, ctx, store, "documents", "source_id = 'source-life'", 0)
	assertCount(t, ctx, store, "sections", "document_id = 'doc-life'", 0)
	assertCount(t, ctx, store, "fts_sections", "document_id = 'doc-life'", 0)
	hits, err = store.SearchSections(ctx, "membership lifecycle", 10)
	if err != nil {
		t.Fatalf("SearchSections after delete returned error: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("SearchSections after delete returned %+v, want no hits", hits)
	}
}

func TestReplaceDocumentReplacesSectionsAndFTSRows(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-1")

	doc := domain.DocumentInput{
		ID:          "doc-1",
		SourceID:    "source-1",
		ExternalID:  "guide.md",
		Title:       "Guide",
		URL:         "file:///guide.md",
		Version:     "v1",
		ContentHash: "hash-doc-v1",
	}
	firstSections := []domain.SectionInput{
		{
			ID:          "section-old-1",
			HeadingPath: "Guide",
			Title:       "Old Overview",
			Content:     "legacy onboarding content",
			ContentHash: "hash-old-1",
			Ordinal:     0,
		},
		{
			ID:          "section-old-2",
			HeadingPath: "Guide > Details",
			Title:       "Old Details",
			Content:     "obsolete billing details",
			ContentHash: "hash-old-2",
			Ordinal:     1,
		},
	}
	if err := store.ReplaceDocument(ctx, doc, firstSections); err != nil {
		t.Fatalf("first ReplaceDocument returned error: %v", err)
	}
	assertCount(t, ctx, store, "sections", "document_id = 'doc-1'", 2)
	assertCount(t, ctx, store, "fts_sections", "document_id = 'doc-1'", 2)

	doc.Title = "Updated Guide"
	doc.ContentHash = "hash-doc-v2"
	replacementSections := []domain.SectionInput{
		{
			ID:          "section-new-1",
			HeadingPath: "Guide > Current",
			Title:       "Current Overview",
			Content:     "fresh entitlement content",
			ContentHash: "hash-new-1",
			Ordinal:     0,
		},
	}
	if err := store.ReplaceDocument(ctx, doc, replacementSections); err != nil {
		t.Fatalf("second ReplaceDocument returned error: %v", err)
	}

	assertCount(t, ctx, store, "documents", "id = 'doc-1' and title = 'Updated Guide' and content_hash = 'hash-doc-v2'", 1)
	assertCount(t, ctx, store, "sections", "document_id = 'doc-1'", 1)
	assertCount(t, ctx, store, "sections", "id = 'section-new-1' and title = 'Current Overview'", 1)
	assertCount(t, ctx, store, "sections", "id in ('section-old-1', 'section-old-2')", 0)
	assertCount(t, ctx, store, "fts_sections", "document_id = 'doc-1'", 1)
	assertCount(t, ctx, store, "fts_sections", "section_id = 'section-new-1' and title = 'Current Overview'", 1)
	assertCount(t, ctx, store, "fts_sections", "section_id in ('section-old-1', 'section-old-2')", 0)
}

func TestSearchSections(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-1")

	doc := domain.DocumentInput{
		ID:          "doc-1",
		SourceID:    "source-1",
		ExternalID:  "search.md",
		Title:       "Search",
		ContentHash: "hash-doc",
	}
	sections := []domain.SectionInput{
		{
			ID:          "section-match",
			HeadingPath: "Search > Benefits",
			Title:       "Benefit Rules",
			Content:     "membership entitlement configuration",
			ContentHash: "hash-match",
			Ordinal:     0,
		},
		{
			ID:          "section-other",
			HeadingPath: "Search > Checkout",
			Title:       "Checkout Rules",
			Content:     "payment routing configuration",
			ContentHash: "hash-other",
			Ordinal:     1,
		},
	}
	if err := store.ReplaceDocument(ctx, doc, sections); err != nil {
		t.Fatalf("ReplaceDocument returned error: %v", err)
	}

	hits, err := store.SearchSections(ctx, "membership", 10)
	if err != nil {
		t.Fatalf("SearchSections returned error: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("SearchSections returned %d hits, want 1: %#v", len(hits), hits)
	}
	if hits[0].SectionID != "section-match" {
		t.Fatalf("hit SectionID = %q, want section-match", hits[0].SectionID)
	}
	if hits[0].DocumentID != "doc-1" {
		t.Fatalf("hit DocumentID = %q, want doc-1", hits[0].DocumentID)
	}
	if hits[0].Title != "Benefit Rules" {
		t.Fatalf("hit Title = %q, want Benefit Rules", hits[0].Title)
	}
	if !strings.Contains(hits[0].Snippet, "<mark>membership</mark>") {
		t.Fatalf("hit Snippet = %q, want highlighted membership", hits[0].Snippet)
	}

	hits, err = store.SearchSections(ctx, "   ", 10)
	if err != nil {
		t.Fatalf("empty SearchSections returned error: %v", err)
	}
	if hits != nil {
		t.Fatalf("empty SearchSections returned %#v, want nil", hits)
	}
}

func TestDocumentProfilePreservesDescAndCascades(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-1")

	doc := domain.DocumentInput{
		ID:          "doc-profile",
		SourceID:    "source-1",
		ExternalID:  "profile.md",
		Title:       "Profile",
		ContentHash: "hash-v1",
	}
	if err := store.ReplaceDocument(ctx, doc, []domain.SectionInput{
		{ID: "section-profile", DocumentID: doc.ID, Title: "Overview", Content: "profile content", ContentHash: "section-hash"},
	}); err != nil {
		t.Fatalf("ReplaceDocument returned error: %v", err)
	}

	profile, err := store.GetDocumentProfile(ctx, doc.ID)
	if err != nil {
		t.Fatalf("GetDocumentProfile returned error: %v", err)
	}
	if profile.Desc != "" || profile.RetrievalProfileJSON != "{}" {
		t.Fatalf("default profile = %+v, want empty desc and empty retrieval profile", profile)
	}

	profile, err = store.UpdateDocumentProfileDesc(ctx, domain.DocumentProfileInput{
		DocumentID: doc.ID,
		Desc:       "管理员维护的文档说明",
	})
	if err != nil {
		t.Fatalf("UpdateDocumentProfileDesc returned error: %v", err)
	}
	if profile.Desc != "管理员维护的文档说明" {
		t.Fatalf("profile desc = %q, want administrator desc", profile.Desc)
	}

	profile, err = store.UpsertDocumentRetrievalProfile(ctx, domain.RetrievalProfileInput{
		DocumentID:           doc.ID,
		RetrievalProfileJSON: `{"top_tags":["权限"],"top_terms":[{"term":"权限","tf":1,"sections":1,"score":1}]}`,
		GeneratedFromHash:    "hash-v1",
	})
	if err != nil {
		t.Fatalf("UpsertDocumentRetrievalProfile returned error: %v", err)
	}
	if profile.Desc != "管理员维护的文档说明" || profile.GeneratedFromHash != "hash-v1" {
		t.Fatalf("generated profile update = %+v, want desc preserved and generated hash", profile)
	}

	doc.ContentHash = "hash-v2"
	if err := store.ReplaceDocument(ctx, doc, []domain.SectionInput{
		{ID: "section-profile-v2", DocumentID: doc.ID, Title: "Overview", Content: "updated profile content", ContentHash: "section-hash-v2"},
	}); err != nil {
		t.Fatalf("second ReplaceDocument returned error: %v", err)
	}
	profile, err = store.GetDocumentProfile(ctx, doc.ID)
	if err != nil {
		t.Fatalf("GetDocumentProfile after ReplaceDocument returned error: %v", err)
	}
	if profile.Desc != "管理员维护的文档说明" {
		t.Fatalf("desc after ReplaceDocument = %q, want preserved administrator desc", profile.Desc)
	}

	if err := store.DeleteDocumentsNotInSource(ctx, "source-1", nil); err != nil {
		t.Fatalf("DeleteDocumentsNotInSource returned error: %v", err)
	}
	if _, err := store.GetDocumentProfile(ctx, doc.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetDocumentProfile after document delete error = %v, want sql.ErrNoRows", err)
	}
}

func TestSearchSectionsUsesChineseSubstringAndProfileEvidence(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-1")

	doc := domain.DocumentInput{
		ID:          "doc-auth-errors",
		SourceID:    "source-1",
		ExternalID:  "auth-errors.md",
		Title:       "权限接口",
		ContentHash: "hash-auth-errors",
	}
	if err := store.ReplaceDocument(ctx, doc, []domain.SectionInput{
		{
			ID:          "section-auth-errors",
			DocumentID:  doc.ID,
			HeadingPath: "权限接口 > 错误响应",
			Title:       "错误响应",
			Content:     "权限接口的几种错误响应包括 401 和 403。",
			ContentHash: "hash-section-auth-errors",
		},
	}); err != nil {
		t.Fatalf("ReplaceDocument returned error: %v", err)
	}
	if _, err := store.UpsertDocumentRetrievalProfile(ctx, domain.RetrievalProfileInput{
		DocumentID:           doc.ID,
		RetrievalProfileJSON: `{"top_tags":["权限","错误响应"],"top_terms":[{"term":"错误响应","tf":2,"sections":1,"heading_hits":1,"score":7.2}],"keyphrases":["权限接口","错误响应"],"api_refs":["401","403"]}`,
		GeneratedFromHash:    doc.ContentHash,
	}); err != nil {
		t.Fatalf("UpsertDocumentRetrievalProfile returned error: %v", err)
	}

	result, err := store.SearchSectionsWithOptions(ctx, domain.SearchOptions{
		Query:                  "错误响应",
		Limit:                  5,
		MaxSearches:            5,
		MaxSectionsPerDocument: 2,
		ProfileDetail:          "compact",
	})
	if err != nil {
		t.Fatalf("SearchSectionsWithOptions returned error: %v", err)
	}
	if len(result.Hits) == 0 || result.Hits[0].DocumentID != doc.ID {
		t.Fatalf("SearchSectionsWithOptions hits = %+v, want auth errors document", result.Hits)
	}
	if result.SearchesUsed == 0 || len(result.Attempts) == 0 {
		t.Fatalf("search attempts = %+v, want populated attempts", result.Attempts)
	}
	hit := result.Hits[0]
	if hit.QueryMatch == nil || len(hit.QueryMatch.MatchedFields) == 0 {
		t.Fatalf("hit query_match = %+v, want match evidence", hit.QueryMatch)
	}
	if hit.Profile == nil || !containsString(hit.Profile.TopTags, "错误响应") {
		t.Fatalf("hit profile = %+v, want compact generated tags", hit.Profile)
	}
}

func TestSearchSectionsProfileAliasEvidenceAndBoundedFullProfile(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-1")

	doc := domain.DocumentInput{
		ID:          "doc-alias",
		SourceID:    "source-1",
		ExternalID:  "alias.md",
		Title:       "Alias Document",
		ContentHash: "hash-alias",
	}
	longContent := strings.Repeat("large section content ", 80)
	if err := store.ReplaceDocument(ctx, doc, []domain.SectionInput{
		{
			ID:          "section-alias",
			DocumentID:  doc.ID,
			Title:       "Overview",
			Content:     longContent,
			ContentHash: "hash-section-alias",
		},
	}); err != nil {
		t.Fatalf("ReplaceDocument returned error: %v", err)
	}
	if _, err := store.UpsertDocumentRetrievalProfile(ctx, domain.RetrievalProfileInput{
		DocumentID: doc.ID,
		RetrievalProfileJSON: `{
			"top_tags":["alias"],
			"top_terms":[{"term":"alias","tf":1,"sections":1,"score":2}],
			"keyphrases":["alias"],
			"aliases":["alias-only-token"],
			"api_refs":["GET /alias"],
			"section_distribution":[
				{"section_id":"section-alias","title":"Overview","terms":["alias","alias-only-token","GET /alias","extra1","extra2","extra3","extra4","extra5","extra6"],"term_count":9},
				{"section_id":"section-extra-1","title":"Extra 1","terms":["extra"],"term_count":1},
				{"section_id":"section-extra-2","title":"Extra 2","terms":["extra"],"term_count":1},
				{"section_id":"section-extra-3","title":"Extra 3","terms":["extra"],"term_count":1},
				{"section_id":"section-extra-4","title":"Extra 4","terms":["extra"],"term_count":1},
				{"section_id":"section-extra-5","title":"Extra 5","terms":["extra"],"term_count":1}
			],
			"stats":{"token_count":99,"section_count":6,"unique_term_count":12}
		}`,
		GeneratedFromHash: doc.ContentHash,
	}); err != nil {
		t.Fatalf("UpsertDocumentRetrievalProfile returned error: %v", err)
	}

	result, err := store.SearchSectionsWithOptions(ctx, domain.SearchOptions{
		Query:                  "alias-only-token",
		Limit:                  5,
		MaxSearches:            5,
		MaxSectionsPerDocument: 2,
		ProfileDetail:          "full",
		MaxCharsPerResult:      40,
	})
	if err != nil {
		t.Fatalf("SearchSectionsWithOptions returned error: %v", err)
	}
	if len(result.Hits) != 1 {
		t.Fatalf("hits = %+v, want one alias profile hit", result.Hits)
	}
	hit := result.Hits[0]
	if hit.QueryMatch == nil || !containsString(hit.QueryMatch.MatchedFields, "profile") {
		t.Fatalf("query match = %+v, want profile evidence for alias-only hit", hit.QueryMatch)
	}
	if len(hit.Content) > 40 {
		t.Fatalf("content len = %d, want <= 40", len(hit.Content))
	}
	full, ok := hit.RetrievalProfile.(map[string]any)
	if !ok {
		t.Fatalf("retrieval_profile = %#v, want bounded full profile map", hit.RetrievalProfile)
	}
	sections, ok := full["section_distribution"].([]storedSectionDistribution)
	if !ok || len(sections) > 5 {
		t.Fatalf("bounded section_distribution = %#v, want at most 5 sections", full["section_distribution"])
	}
	if len(sections) == 0 || len(sections[0].Terms) > 8 {
		t.Fatalf("bounded first section = %+v, want limited terms", sections)
	}
}

func TestFeedbackValidationCanonicalRankingAndNodeMerge(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-1")

	for _, doc := range []struct {
		id    string
		title string
		secID string
	}{
		{id: "doc-a", title: "Regular Document", secID: "section-a"},
		{id: "doc-b", title: "Canonical Document", secID: "section-b"},
	} {
		if err := store.ReplaceDocument(ctx, domain.DocumentInput{
			ID:          doc.id,
			SourceID:    "source-1",
			ExternalID:  doc.id + ".md",
			Title:       doc.title,
			ContentHash: "hash-" + doc.id,
		}, []domain.SectionInput{
			{
				ID:          doc.secID,
				Title:       doc.title,
				Content:     "canonical ranking sharedtoken",
				ContentHash: "hash-" + doc.secID,
			},
		}); err != nil {
			t.Fatalf("ReplaceDocument(%s) returned error: %v", doc.id, err)
		}
	}
	if _, err := store.CreateFeedbackEvent(ctx, domain.FeedbackEventInput{
		TargetKind:   "document",
		TargetID:     "doc-b",
		FeedbackKind: "document_canonical",
	}); err != nil {
		t.Fatalf("CreateFeedbackEvent document_canonical returned error: %v", err)
	}
	hits, err := store.SearchSections(ctx, "canonical ranking sharedtoken", 10)
	if err != nil {
		t.Fatalf("SearchSections returned error: %v", err)
	}
	if len(hits) != 2 || hits[0].DocumentID != "doc-b" || !hits[0].Canonical {
		t.Fatalf("SearchSections hits = %+v, want canonical document first", hits)
	}

	if _, err := store.CreateFeedbackEvent(ctx, domain.FeedbackEventInput{
		TargetKind:   "edge",
		TargetID:     "doc-b",
		FeedbackKind: "document_stale",
	}); err == nil || !strings.Contains(err.Error(), `target_kind "document"`) {
		t.Fatalf("document_stale wrong target error = %v, want target_kind document error", err)
	}
	if _, err := store.CreateFeedbackEvent(ctx, domain.FeedbackEventInput{
		TargetKind:   "node",
		TargetID:     "node-a",
		FeedbackKind: "node_merge",
		PayloadJSON:  `{}`,
	}); err == nil || !strings.Contains(err.Error(), "merged_into") {
		t.Fatalf("node_merge missing payload error = %v, want merged_into error", err)
	}

	for _, node := range []domain.NodeInput{
		{ID: "node-a", Kind: "Product", Name: "A", CanonicalName: "a"},
		{ID: "node-b", Kind: "Product", Name: "B", CanonicalName: "b"},
	} {
		if err := store.UpsertNode(ctx, node); err != nil {
			t.Fatalf("UpsertNode(%s) returned error: %v", node.ID, err)
		}
	}
	if _, err := store.CreateFeedbackEvent(ctx, domain.FeedbackEventInput{
		TargetKind:   "node",
		TargetID:     "node-a",
		FeedbackKind: "node_merge",
		PayloadJSON:  `{"merged_into":"node-b"}`,
	}); err != nil {
		t.Fatalf("CreateFeedbackEvent node_merge returned error: %v", err)
	}
	related, err := store.RelatedNodes(ctx, "node-a", domain.RelatedOptions{Direction: "out", Kind: "merged_into"})
	if err != nil {
		t.Fatalf("RelatedNodes merged_into returned error: %v", err)
	}
	if len(related) != 1 || related[0].Node.ID != "node-b" || related[0].Edge.Provenance != "manual" {
		t.Fatalf("RelatedNodes merged_into = %+v, want manual merge edge to node-b", related)
	}
}

func TestFeedbackEventsApplyCurationRules(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-1")

	if err := store.ReplaceDocument(ctx, domain.DocumentInput{
		ID:          "doc-feedback",
		SourceID:    "source-1",
		ExternalID:  "feedback.md",
		Title:       "Feedback",
		ContentHash: "hash-doc",
	}, []domain.SectionInput{
		{
			ID:          "section-feedback",
			Title:       "Feedback",
			Content:     "feedback stale unique token",
			ContentHash: "hash-section",
		},
	}); err != nil {
		t.Fatalf("ReplaceDocument returned error: %v", err)
	}
	for _, node := range []domain.NodeInput{
		{ID: "node-a", Kind: "Product", Name: "A", CanonicalName: "a"},
		{ID: "node-b", Kind: "API", Name: "B", CanonicalName: "b"},
		{ID: "node-c", Kind: "Module", Name: "C", CanonicalName: "c"},
	} {
		if err := store.UpsertNode(ctx, node); err != nil {
			t.Fatalf("UpsertNode(%s) returned error: %v", node.ID, err)
		}
	}
	if err := store.UpsertEdge(ctx, domain.EdgeInput{
		ID:         "edge-auto",
		SrcID:      "node-a",
		DstID:      "node-b",
		Kind:       "exposes_api",
		Provenance: "rule",
	}); err != nil {
		t.Fatalf("UpsertEdge returned error: %v", err)
	}

	event, err := store.CreateFeedbackEvent(ctx, domain.FeedbackEventInput{
		TargetKind:   "edge",
		TargetID:     "edge-auto",
		FeedbackKind: "relationship_wrong",
		Actor:        "alice",
	})
	if err != nil {
		t.Fatalf("CreateFeedbackEvent relationship_wrong returned error: %v", err)
	}
	if event.ID == "" || event.Actor != "alice" || event.PayloadJSON != "{}" {
		t.Fatalf("relationship_wrong event = %+v, want populated event", event)
	}
	related, err := store.RelatedNodes(ctx, "node-a", domain.RelatedOptions{Direction: "out"})
	if err != nil {
		t.Fatalf("RelatedNodes after relationship_wrong returned error: %v", err)
	}
	if len(related) != 0 {
		t.Fatalf("RelatedNodes after relationship_wrong = %+v, want edge filtered", related)
	}

	event, err = store.CreateFeedbackEvent(ctx, domain.FeedbackEventInput{
		TargetKind:   "node",
		TargetID:     "node-a",
		FeedbackKind: "relationship_add",
		PayloadJSON:  `{"src_id":"node-a","dst_id":"node-c","kind":"depends_on","edge_id":"edge-manual"}`,
		Actor:        "bob",
	})
	if err != nil {
		t.Fatalf("CreateFeedbackEvent relationship_add returned error: %v", err)
	}
	if event.FeedbackKind != "relationship_add" {
		t.Fatalf("relationship_add event = %+v", event)
	}
	related, err = store.RelatedNodes(ctx, "node-a", domain.RelatedOptions{Direction: "out", Kind: "depends_on"})
	if err != nil {
		t.Fatalf("RelatedNodes after relationship_add returned error: %v", err)
	}
	if len(related) != 1 || related[0].Edge.ID != "edge-manual" || related[0].Edge.Provenance != "manual" {
		t.Fatalf("RelatedNodes after relationship_add = %+v, want manual edge", related)
	}

	hits, err := store.SearchSections(ctx, "feedback stale unique", 10)
	if err != nil {
		t.Fatalf("SearchSections before document_stale returned error: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("SearchSections before document_stale = %+v, want hit", hits)
	}
	if _, err := store.CreateFeedbackEvent(ctx, domain.FeedbackEventInput{
		TargetKind:   "document",
		TargetID:     "doc-feedback",
		FeedbackKind: "document_stale",
	}); err != nil {
		t.Fatalf("CreateFeedbackEvent document_stale returned error: %v", err)
	}
	hits, err = store.SearchSections(ctx, "feedback stale unique", 10)
	if err != nil {
		t.Fatalf("SearchSections after document_stale returned error: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("SearchSections after document_stale = %+v, want no stale hits", hits)
	}

	events, err := store.ListFeedbackEvents(ctx, domain.FeedbackListOptions{TargetKind: "edge", TargetID: "edge-auto", Limit: 10})
	if err != nil {
		t.Fatalf("ListFeedbackEvents returned error: %v", err)
	}
	if len(events) != 1 || events[0].FeedbackKind != "relationship_wrong" {
		t.Fatalf("ListFeedbackEvents = %+v, want relationship_wrong event", events)
	}
}

func TestGraphNodeAndEdgeUpsertIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	createTestSource(t, ctx, store, "source-1")

	if err := store.ReplaceDocument(ctx, domain.DocumentInput{
		ID:          "doc-1",
		SourceID:    "source-1",
		ExternalID:  "guide.md",
		Title:       "Guide",
		ContentHash: "hash-doc",
	}, []domain.SectionInput{
		{
			ID:          "section-1",
			Title:       "API",
			Content:     "GET /member/benefits returns benefits.",
			ContentHash: "hash-section",
		},
	}); err != nil {
		t.Fatalf("ReplaceDocument returned error: %v", err)
	}

	if err := store.UpsertNode(ctx, domain.NodeInput{
		ID:            "node-document",
		Kind:          "Document",
		Name:          "Guide",
		CanonicalName: "doc:guide.md",
		MetadataJSON:  `{"external_id":"guide.md"}`,
		Confidence:    1.0,
	}); err != nil {
		t.Fatalf("first UpsertNode document returned error: %v", err)
	}
	if err := store.UpsertNode(ctx, domain.NodeInput{
		ID:            "node-section",
		Kind:          "DocSection",
		Name:          "API",
		CanonicalName: "section:guide.md#api",
		MetadataJSON:  `{"ordinal":0}`,
		Confidence:    0.95,
	}); err != nil {
		t.Fatalf("first UpsertNode section returned error: %v", err)
	}
	if err := store.UpsertEdge(ctx, domain.EdgeInput{
		ID:                "edge-document-section",
		SrcID:             "node-document",
		DstID:             "node-section",
		Kind:              "contains",
		Confidence:        0.9,
		Provenance:        "rule",
		EvidenceSectionID: "section-1",
		SourceRevision:    "rev-1",
		MetadataJSON:      `{"source":"first-sync"}`,
	}); err != nil {
		t.Fatalf("first UpsertEdge returned error: %v", err)
	}

	if err := store.UpsertNode(ctx, domain.NodeInput{
		ID:            "node-document",
		Kind:          "Document",
		Name:          "Updated Guide",
		CanonicalName: "doc:guide.md",
		MetadataJSON:  `{"external_id":"guide.md","title":"Updated Guide"}`,
		Confidence:    0.88,
	}); err != nil {
		t.Fatalf("second UpsertNode document returned error: %v", err)
	}
	if err := store.UpsertEdge(ctx, domain.EdgeInput{
		ID:                "edge-document-section",
		SrcID:             "node-document",
		DstID:             "node-section",
		Kind:              "contains",
		Confidence:        0.77,
		Provenance:        "rule",
		EvidenceSectionID: "section-1",
		SourceRevision:    "rev-2",
		MetadataJSON:      `{"source":"second-sync"}`,
	}); err != nil {
		t.Fatalf("second UpsertEdge returned error: %v", err)
	}

	assertCount(t, ctx, store, "nodes", "id in ('node-document', 'node-section')", 2)
	assertCount(t, ctx, store, "edges", "id = 'edge-document-section'", 1)
	assertCount(t, ctx, store, "nodes", "id = 'node-document' and name = 'Updated Guide' and confidence = 0.88 and metadata_json like '%Updated Guide%'", 1)
	assertCount(t, ctx, store, "edges", "id = 'edge-document-section' and confidence = 0.77 and source_revision = 'rev-2' and metadata_json like '%second-sync%'", 1)

	node, err := store.GetNode(ctx, "node-document")
	if err != nil {
		t.Fatalf("GetNode returned error: %v", err)
	}
	if node.Name != "Updated Guide" || node.MetadataJSON != `{"external_id":"guide.md","title":"Updated Guide"}` {
		t.Fatalf("GetNode returned %#v, want updated document node", node)
	}

	related, err := store.RelatedNodes(ctx, "node-document", domain.RelatedOptions{
		Direction: "out",
		Kind:      "contains",
	})
	if err != nil {
		t.Fatalf("RelatedNodes returned error: %v", err)
	}
	if len(related) != 1 {
		t.Fatalf("RelatedNodes returned %d rows, want 1: %#v", len(related), related)
	}
	if related[0].Node.ID != "node-section" || related[0].Direction != "out" {
		t.Fatalf("RelatedNodes[0] = %#v, want outgoing section relation", related[0])
	}
	if related[0].Edge.ID != "edge-document-section" || related[0].Edge.SourceRevision != "rev-2" {
		t.Fatalf("RelatedNodes edge = %#v, want updated edge", related[0].Edge)
	}
}

func TestImpactReturnsPathsWithDepthDirectionKindAndCycleHandling(t *testing.T) {
	ctx := context.Background()
	store := openMigratedTempStore(t, ctx)
	seedImpactGraph(t, ctx, store)

	depthOne, err := store.Impact(ctx, "node-product", domain.ImpactOptions{
		Direction: "out",
		MaxDepth:  1,
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("Impact depth one returned error: %v", err)
	}
	if depthOne.StartNode.ID != "node-product" {
		t.Fatalf("Impact start node = %+v, want node-product", depthOne.StartNode)
	}
	if len(depthOne.Paths) != 2 {
		t.Fatalf("Impact depth one returned %d paths, want 2: %#v", len(depthOne.Paths), depthOne.Paths)
	}
	for _, path := range depthOne.Paths {
		if len(path.Nodes) != 2 || len(path.Edges) != 1 {
			t.Fatalf("Impact depth one path = %#v, want one-hop path", path)
		}
	}

	filtered, err := store.Impact(ctx, "node-product", domain.ImpactOptions{
		Direction: "out",
		Kind:      "exposes_api",
		MaxDepth:  3,
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("Impact filtered returned error: %v", err)
	}
	if len(filtered.Paths) != 1 {
		t.Fatalf("Impact filtered returned %d paths, want only exposes_api edge: %#v", len(filtered.Paths), filtered.Paths)
	}
	if filtered.Paths[0].Edges[0].Kind != "exposes_api" || filtered.Paths[0].Nodes[1].ID != "node-api" {
		t.Fatalf("Impact filtered path = %#v, want product to API exposes_api path", filtered.Paths[0])
	}
	if filtered.Paths[0].Edges[0].EvidenceSectionID != "section-impact" {
		t.Fatalf("Impact evidence section = %q, want section-impact", filtered.Paths[0].Edges[0].EvidenceSectionID)
	}

	incoming, err := store.Impact(ctx, "node-product", domain.ImpactOptions{
		Direction: "in",
		MaxDepth:  1,
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("Impact incoming returned error: %v", err)
	}
	if len(incoming.Paths) != 1 || incoming.Paths[0].Edges[0].ID != "edge-module-product" || incoming.Paths[0].Nodes[1].ID != "node-module" {
		t.Fatalf("Impact incoming paths = %#v, want module to product incoming path", incoming.Paths)
	}

	withCycle, err := store.Impact(ctx, "node-product", domain.ImpactOptions{
		Direction: "out",
		MaxDepth:  4,
		Limit:     20,
	})
	if err != nil {
		t.Fatalf("Impact with cycle returned error: %v", err)
	}
	for _, path := range withCycle.Paths {
		seen := map[string]bool{}
		for _, node := range path.Nodes {
			if seen[node.ID] {
				t.Fatalf("Impact path contains cycle node %q: %#v", node.ID, path)
			}
			seen[node.ID] = true
		}
		if len(path.Nodes) != len(path.Edges)+1 {
			t.Fatalf("Impact path = %#v, want nodes to describe path, not flat nodes", path)
		}
	}
}

func openTempStore(t *testing.T, ctx context.Context) *Store {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "docgraph.db")
	store, err := Open(ctx, "sqlite://"+dbPath)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Fatalf("Close returned error: %v", err)
		}
	})
	return store
}

func openMigratedTempStore(t *testing.T, ctx context.Context) *Store {
	t.Helper()

	store := openTempStore(t, ctx)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}
	return store
}

func createTestSource(t *testing.T, ctx context.Context, store *Store, id string) {
	t.Helper()

	_, err := store.CreateSource(ctx, domain.Source{
		ID:   id,
		Kind: "local",
		Name: "Docs",
		DSN:  "file:///docs",
	})
	if err != nil {
		t.Fatalf("CreateSource returned error: %v", err)
	}
}

func assertCount(t *testing.T, ctx context.Context, store *Store, table string, where string, want int64) {
	t.Helper()

	var got int64
	query := "select count(*) from " + table
	if where != "" {
		query += " where " + where
	}
	if err := store.db.QueryRowContext(ctx, query).Scan(&got); err != nil {
		t.Fatalf("count %s where %q: %v", table, where, err)
	}
	if got != want {
		t.Fatalf("count %s where %q = %d, want %d", table, where, got, want)
	}
}

func syncJobsContain(jobs []domain.SyncJob, status string, text string) bool {
	for _, job := range jobs {
		if job.Status != status {
			continue
		}
		if strings.Contains(job.PayloadJSON, text) || strings.Contains(job.LastError, text) {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func seedImpactGraph(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()

	createTestSource(t, ctx, store, "source-impact")
	if err := store.ReplaceDocument(ctx, domain.DocumentInput{
		ID:          "doc-impact",
		SourceID:    "source-impact",
		ExternalID:  "impact.md",
		Title:       "Impact",
		ContentHash: "hash-impact-doc",
	}, []domain.SectionInput{
		{
			ID:          "section-impact",
			Title:       "Impact Evidence",
			Content:     "Membership exposes API and downstream module behavior.",
			ContentHash: "hash-impact-section",
		},
	}); err != nil {
		t.Fatalf("ReplaceDocument for impact graph returned error: %v", err)
	}

	nodes := []domain.NodeInput{
		{ID: "node-product", Kind: "Product", Name: "Membership", CanonicalName: "membership"},
		{ID: "node-api", Kind: "API", Name: "GET /member/benefits", CanonicalName: "get /member/benefits"},
		{ID: "node-module", Kind: "Module", Name: "Entitlements", CanonicalName: "entitlements"},
		{ID: "node-section", Kind: "DocSection", Name: "Impact Evidence", CanonicalName: "impact#evidence"},
	}
	for _, node := range nodes {
		if err := store.UpsertNode(ctx, node); err != nil {
			t.Fatalf("UpsertNode(%s) returned error: %v", node.ID, err)
		}
	}

	edges := []domain.EdgeInput{
		{ID: "edge-product-api", SrcID: "node-product", DstID: "node-api", Kind: "exposes_api", EvidenceSectionID: "section-impact"},
		{ID: "edge-product-section", SrcID: "node-product", DstID: "node-section", Kind: "contains", EvidenceSectionID: "section-impact"},
		{ID: "edge-api-module", SrcID: "node-api", DstID: "node-module", Kind: "describes", EvidenceSectionID: "section-impact"},
		{ID: "edge-module-product", SrcID: "node-module", DstID: "node-product", Kind: "depends_on", EvidenceSectionID: "section-impact"},
	}
	for _, edge := range edges {
		if err := store.UpsertEdge(ctx, edge); err != nil {
			t.Fatalf("UpsertEdge(%s) returned error: %v", edge.ID, err)
		}
	}
}

func insertFixtureRows(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()

	statements := []string{
		`insert into sources (id, kind, name, dsn) values ('source-1', 'git', 'Docs', 'git://docs')`,
		`insert into documents (id, source_id, external_id, title, content_hash) values ('doc-1', 'source-1', 'README.md', 'Readme', 'hash-doc')`,
		`insert into sections (id, document_id, title, content_hash) values ('section-1', 'doc-1', 'Overview', 'hash-section')`,
		`insert into nodes (id, kind, name, canonical_name) values ('node-1', 'Product', 'Payments', 'payments')`,
		`insert into nodes (id, kind, name, canonical_name) values ('node-2', 'API', 'GET /payments', 'get /payments')`,
		`insert into edges (id, src_id, dst_id, kind, evidence_section_id) values ('edge-1', 'node-1', 'node-2', 'exposes_api', 'section-1')`,
		`insert into jobs (id, kind, status) values ('job-1', 'sync', 'queued')`,
	}

	for _, stmt := range statements {
		if _, err := store.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
}
