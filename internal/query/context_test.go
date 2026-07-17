package query

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docgraph/docgraph/internal/domain"
	"github.com/docgraph/docgraph/internal/storage"
)

type legacyOnlyStore struct {
	storage.Store
}

type countingAuthoredRelationStore struct {
	storage.Store
	contextCalls int
	batchCalls   int
}

func (s *countingAuthoredRelationStore) GetSectionContext(ctx context.Context, sectionID string, childLimit int) (storage.SectionContext, error) {
	s.contextCalls++
	return s.Store.(storage.SectionContextStore).GetSectionContext(ctx, sectionID, childLimit)
}

func (s *countingAuthoredRelationStore) ListDocumentOutline(ctx context.Context, documentID string, opts storage.OutlineOptions) (storage.DocumentOutline, error) {
	return s.Store.(storage.SectionContextStore).ListDocumentOutline(ctx, documentID, opts)
}

func (s *countingAuthoredRelationStore) ListAuthoredRelationsForSections(ctx context.Context, sectionIDs []string, perSectionLimit int) (map[string][]storage.RelatedNode, error) {
	s.batchCalls++
	return s.Store.(storage.AuthoredRelationStore).ListAuthoredRelationsForSections(ctx, sectionIDs, perSectionLimit)
}

func TestContextIncludesTopicEvidenceAndAuthoredSuggestions(t *testing.T) {
	store := newTopicContextStore(t)
	service := NewService(store)

	pack, err := service.Context(context.Background(), ContextRequest{Query: "architecture-evidence", MaxSections: 2, MaxChars: 1000})
	if err != nil {
		t.Fatalf("Context returned error: %v", err)
	}
	if len(pack.Sections) == 0 {
		t.Fatal("Context returned no sections")
	}
	section := pack.Sections[0]
	if section.SectionID != "topic-child" || section.NodeID != "node-topic-child" {
		t.Fatalf("context topic identity = %+v", section)
	}
	if section.ElementKind != "topic" || section.DisplayNumber != "1.1" {
		t.Fatalf("context topic presentation = %+v", section)
	}
	if len(section.Ancestry) != 1 || section.Ancestry[0].SectionID != "topic-root" {
		t.Fatalf("context ancestry = %+v", section.Ancestry)
	}
	if len(section.MediaAssets) != 1 || section.MediaAssets[0].ID != "asset-diagram" {
		t.Fatalf("context media = %+v", section.MediaAssets)
	}
	if len(section.AuthoredRelations) == 0 || len(section.AuthoredRelations) > maxAuthoredRelationsPerSection {
		t.Fatalf("context authored relations = %+v", section.AuthoredRelations)
	}
	if section.Provenance == nil || section.Provenance.SnapshotID != "snapshot-topic" || section.Provenance.SourceElementID != "xmind-child" {
		t.Fatalf("context provenance = %+v", section.Provenance)
	}

	result, err := service.SearchDetailed(context.Background(), storage.SearchOptions{Query: "architecture-evidence", Limit: 5, MaxSearches: 5, MaxSectionsPerDocument: 5, UseRelationExpansion: true})
	if err != nil {
		t.Fatalf("SearchDetailed returned error: %v", err)
	}
	if len(result.SuggestedReads.AuthoredRelations) != 1 {
		t.Fatalf("authored suggestions = %+v, want one related_to suggestion", result.SuggestedReads.AuthoredRelations)
	}
	suggestion := result.SuggestedReads.AuthoredRelations[0]
	if suggestion.SourceSectionID != "topic-child" || suggestion.TargetSectionID != "topic-root" || !strings.Contains(suggestion.Reason, "review") {
		t.Fatalf("authored suggestion = %+v", suggestion)
	}
	if len(result.SuggestedReads.CuratedRelations) != 0 {
		t.Fatalf("curated suggestions = %+v, want source-authored relation kept separate", result.SuggestedReads.CuratedRelations)
	}
}

func TestContextKeepsLegacyStoreCompatibility(t *testing.T) {
	store := legacyOnlyStore{Store: newTopicContextStore(t)}
	service := NewService(store)
	pack, err := service.Context(context.Background(), ContextRequest{Query: "architecture-evidence", MaxSections: 1, MaxChars: 500})
	if err != nil {
		t.Fatalf("legacy Context returned error: %v", err)
	}
	if len(pack.Sections) != 1 || pack.Sections[0].SectionID != "topic-child" {
		t.Fatalf("legacy context = %+v", pack)
	}
	result, err := service.SearchDetailed(context.Background(), storage.SearchOptions{Query: "architecture-evidence", Limit: 1})
	if err != nil {
		t.Fatalf("legacy SearchDetailed returned error: %v", err)
	}
	if len(result.SuggestedReads.AuthoredRelations) != 0 {
		t.Fatalf("legacy authored suggestions = %+v, want none without optional context store", result.SuggestedReads.AuthoredRelations)
	}
}

func TestSearchDetailedBatchLoadsOnlySemanticAuthoredRelations(t *testing.T) {
	store := &countingAuthoredRelationStore{Store: newTopicContextStore(t)}
	service := NewService(store)
	result, err := service.SearchDetailed(context.Background(), storage.SearchOptions{Query: "architecture-evidence", Limit: 30, MaxSearches: 5, MaxSectionsPerDocument: 30, UseRelationExpansion: true})
	if err != nil {
		t.Fatalf("SearchDetailed returned error: %v", err)
	}
	if store.batchCalls != 1 || store.contextCalls != 0 {
		t.Fatalf("authored relation hydration batch/context calls = %d/%d, want 1/0", store.batchCalls, store.contextCalls)
	}
	if len(result.SuggestedReads.AuthoredRelations) != 1 {
		t.Fatalf("semantic authored suggestions = %+v", result.SuggestedReads.AuthoredRelations)
	}
	for _, hit := range result.Hits {
		if hit.SectionID == "topic-child" && hit.AuthoredRelationCount != 1 {
			t.Fatalf("contains edge polluted authored relation count: %+v", hit)
		}
	}
	withoutRelations, err := service.SearchDetailed(context.Background(), storage.SearchOptions{Query: "architecture-evidence", Limit: 5, MaxSearches: 5, MaxSectionsPerDocument: 5, UseRelationExpansion: false})
	if err != nil {
		t.Fatal(err)
	}
	if store.batchCalls != 1 || len(withoutRelations.SuggestedReads.AuthoredRelations) != 0 {
		t.Fatalf("disabled relation expansion still queried/returned authored relations: calls=%d result=%+v", store.batchCalls, withoutRelations.SuggestedReads.AuthoredRelations)
	}
}

func newTopicContextStore(t *testing.T) storage.Store {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, "sqlite://"+filepath.ToSlash(filepath.Join(t.TempDir(), "docgraph.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSource(ctx, domain.Source{ID: "source-topic", Kind: "xmind", Name: "Architecture Map", DSN: "/maps/architecture.xmind"}); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	imageDigest := strings.Repeat("b", 64)
	bundle := domain.WorkbookBundle{
		SourceID:    "source-topic",
		WorkbookKey: "architecture",
		Snapshot: domain.SourceSnapshotInput{
			ID: "snapshot-topic", SourceID: "source-topic", SourceHash: "snapshot-hash", BlobSHA256: digest,
			FormatFamily: "classic_json", FormatVersion: "1",
		},
		Blobs: []domain.MediaBlobInput{
			{SHA256: digest, SizeBytes: 100, SniffedMediaType: "application/zip", StorageKey: "sha256/" + digest},
			{SHA256: imageDigest, SizeBytes: 42, SniffedMediaType: "image/png", StorageKey: "sha256/" + imageDigest},
		},
		Documents: []domain.WorkbookDocumentInput{{
			Document: domain.DocumentInput{ID: "sheet-topic", SourceID: "source-topic", ExternalID: "sheet-topic", Title: "Architecture", ContentHash: "sheet-hash"},
			Sections: []domain.SectionInput{
				{ID: "topic-root", Title: "Root", Content: "root context", SearchText: "root context", ContentHash: "root-hash", Structure: &domain.SectionStructureInput{SourceElementID: "xmind-root", ElementKind: "topic", Depth: 0, SiblingOrdinal: 0, OrderPath: []int{0}, DisplayNumber: "1"}},
				{ID: "topic-child", Title: "Child", Content: "architecture-evidence details", SearchText: "architecture-evidence details diagram.png", ContentHash: "child-hash", Structure: &domain.SectionStructureInput{ParentSectionID: "topic-root", SourceElementID: "xmind-child", ElementKind: "topic", Depth: 1, SiblingOrdinal: 0, OrderPath: []int{0, 0}, DisplayNumber: "1.1"}},
			},
		}},
		Nodes: []domain.NodeInput{
			{ID: "node-topic-root", Kind: "DocSection", Name: "Root", CanonicalName: "root", MetadataJSON: `{"section_id":"topic-root","document_id":"sheet-topic"}`},
			{ID: "node-topic-child", Kind: "DocSection", Name: "Child", CanonicalName: "child", MetadataJSON: `{"section_id":"topic-child","document_id":"sheet-topic"}`},
		},
		SectionNodes: []domain.SectionNodeInput{
			{SectionID: "topic-root", NodeID: "node-topic-root", Role: "represents"},
			{SectionID: "topic-child", NodeID: "node-topic-child", Role: "represents"},
		},
		Edges: []domain.EdgeInput{
			{ID: "edge-hierarchy", SrcID: "node-topic-root", DstID: "node-topic-child", Kind: "contains", Provenance: "source_authored"},
			{ID: "edge-review", SrcID: "node-topic-child", DstID: "node-topic-root", Kind: "related_to", Provenance: "source_authored", Confidence: 0.9, MetadataJSON: `{"label":"review"}`},
		},
		MediaAssets: []domain.MediaAssetInput{{ID: "asset-diagram", SourceID: "source-topic", SnapshotID: "snapshot-topic", DocumentID: "sheet-topic", ExternalID: "diagram", BlobSHA256: imageDigest, Kind: "image", OriginalName: "diagram.png", MediaType: "image/png", SizeBytes: 42, Status: "available"}},
		MediaRefs:   []domain.SectionMediaRefInput{{SectionID: "topic-child", AssetID: "asset-diagram", Role: "image", Ordinal: 0}},
	}
	workbookStore, ok := store.(storage.WorkbookStore)
	if !ok {
		t.Fatal("store does not implement WorkbookStore")
	}
	if _, err := workbookStore.ReplaceWorkbookBundle(ctx, bundle); err != nil {
		t.Fatal(err)
	}
	return store
}
