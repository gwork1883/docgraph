package sync

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/docgraph/docgraph/internal/domain"
	"github.com/docgraph/docgraph/internal/ids"
	"github.com/docgraph/docgraph/internal/ingest/xmind"
	"github.com/docgraph/docgraph/internal/storage"
	"github.com/docgraph/docgraph/internal/storage/sqlite"
)

func TestBuildXMindBundlePreservesDeepHierarchyIdentityNumberingAndAuthoredEvidence(t *testing.T) {
	root := testXMindTopic("topic-root", "Architecture", 0, 0, []int{0})
	parent := root
	for depth := 1; depth <= 11; depth++ {
		child := testXMindTopic("topic-deep-"+testItoa(depth), "Depth "+testItoa(depth), depth, 0, append(append([]int(nil), parent.OrderPath...), 0))
		child.ParentID = parent.ID
		parent.Children = append(parent.Children, child)
		parent = child
	}
	manual := testXMindTopic("topic-manual", "1.Manual title", 1, 1, []int{0, 1})
	manual.ParentID = root.ID
	manual.Numbering = xmind.Numbering{
		Enabled: true, Format: "decimal", Prefix: "", Suffix: ".", Tiered: true,
		DisplayNumber: "2.3.", PrependingNumbers: "2.",
	}
	manual.Assets = []xmind.AssetRef{{AssetID: "asset-image", Role: "image", Ordinal: 0, Width: 320, Height: 180}}
	root.Children = append(root.Children, manual)
	for index := 0; index < 500; index++ {
		child := testXMindTopic("topic-wide-"+testItoa(index), "Wide "+testItoa(index), 1, index+2, []int{0, index + 2})
		child.ParentID = root.ID
		root.Children = append(root.Children, child)
	}
	otherRoot := testXMindTopic("topic-other-root", "Other Sheet", 0, 0, []int{0})
	manual.Links = []xmind.Link{{ID: "link-cross-sheet", Label: "Go to other", Target: "xmind:#topic-other-root", TargetTopicID: otherRoot.ID, TargetSheetID: "sheet-2"}}

	workbook := xmind.Workbook{
		FormatFamily: xmind.FormatClassicZenJSON, SnapshotHash: strings.Repeat("a", 64),
		SemanticHash: strings.Repeat("b", 64), MediaManifestHash: strings.Repeat("c", 64),
		Sheets: []xmind.Sheet{
			{
				ID: "sheet-1", Title: "Architecture", Roots: []*xmind.Topic{root},
				Relationships: []xmind.Relationship{
					{ID: "rel-a", Label: "depends on", SourceTopicID: manual.ID, TargetTopicID: otherRoot.ID, StartArrow: "none", EndArrow: "triangle", ControlPoints: []xmind.Point{{Key: "0", X: 1, Y: 2}}},
					{ID: "rel-b", Label: "reviews", SourceTopicID: manual.ID, TargetTopicID: otherRoot.ID, StartArrow: "diamond", EndArrow: "triangle"},
				},
				Elements: []xmind.Element{
					{ID: "summary-1", Kind: "summary", Title: "Summary text", OwnerTopicID: root.ID, TargetIDs: []string{manual.ID}, Status: xmind.InventoryIndexed},
					{ID: "boundary-1", Kind: "boundary", Title: "Boundary text", OwnerTopicID: root.ID, TargetIDs: []string{manual.ID}, Status: xmind.InventoryIndexed},
					{ID: "callout-1", Kind: "callout", Title: "Callout text", OwnerTopicID: manual.ID, Status: xmind.InventoryIndexed},
				},
			},
			{ID: "sheet-2", Title: "Other", Roots: []*xmind.Topic{otherRoot}},
		},
		Assets: []xmind.Asset{{
			ID: "asset-image", Kind: xmind.AssetImage, ResourcePath: "resources/diagram.png",
			OriginalName: "diagram.png", MediaType: "image/png", SHA256: strings.Repeat("d", 64),
			SizeBytes: 42, Caption: "Deployment diagram", AltText: "service topology", Status: xmind.InventoryPreserved,
		}},
	}
	plan, err := buildXMindBundle(domain.Source{ID: "src-xmind", Kind: "xmind", Name: "Maps"}, workbook)
	if err != nil {
		t.Fatalf("buildXMindBundle returned error: %v", err)
	}
	if plan.diagnostics.Topics < 513 {
		t.Fatalf("Topics = %d, want at least 513", plan.diagnostics.Topics)
	}
	if len(plan.bundle.Documents) != 2 {
		t.Fatalf("Documents = %d, want 2", len(plan.bundle.Documents))
	}

	manualSection := findXMindSection(t, plan.bundle, "topic-manual")
	if manualSection.Title != "1.Manual title" {
		t.Fatalf("raw Topic title = %q, want unchanged manual numbering", manualSection.Title)
	}
	if manualSection.Structure == nil || manualSection.Structure.DisplayNumber != "2.3." {
		t.Fatalf("manual Topic structure = %+v, want independent dynamic display number", manualSection.Structure)
	}
	if strings.Contains(manualSection.SearchText, "2.3.") || !strings.Contains(manualSection.SearchText, "1.Manual title") || !strings.Contains(manualSection.SearchText, "diagram.png") || !strings.Contains(manualSection.SearchText, "depends on") {
		t.Fatalf("manual SearchText = %q, want raw title/media/relation text without presentation numbering", manualSection.SearchText)
	}
	deep := findXMindSection(t, plan.bundle, "topic-deep-11")
	if deep.Structure == nil || deep.Structure.Depth != 11 || len(deep.Structure.OrderPath) != 12 {
		t.Fatalf("deep structure = %+v, want depth 11 and complete order path", deep.Structure)
	}

	documentNodes := map[string]bool{}
	for _, document := range plan.bundle.Documents {
		documentNodes[documentNodeID(document.Document.ID)] = true
	}
	documentContains := 0
	sameEndpointRelationships := map[string]bool{}
	for _, edge := range plan.bundle.Edges {
		if edge.Kind == "contains" && documentNodes[edge.SrcID] {
			documentContains++
		}
		if edge.Kind == "related_to" {
			sameEndpointRelationships[edge.ID] = true
			if edge.Provenance != "source_authored" || !strings.Contains(edge.MetadataJSON, "xmind_relationship_id") || !strings.Contains(edge.MetadataJSON, "control_points") {
				t.Fatalf("Relationship edge = %+v, want authored provenance and full metadata", edge)
			}
		}
	}
	if documentContains != 2 {
		t.Fatalf("Document contains edge count = %d, want exactly two Sheet roots", documentContains)
	}
	if len(sameEndpointRelationships) != 2 {
		t.Fatalf("same-endpoint Relationship edges = %d, want 2 distinct original IDs", len(sameEndpointRelationships))
	}
	if len(plan.bundle.MediaAssets) != 1 || len(plan.bundle.MediaRefs) != 1 || len(plan.resources) != 1 {
		t.Fatalf("media bundle = assets:%d refs:%d resources:%d, want 1/1/1", len(plan.bundle.MediaAssets), len(plan.bundle.MediaRefs), len(plan.resources))
	}

	// Moving the Topic across Sheets must retain its Section/Node identity while
	// changing Document ownership and hierarchy presentation.
	moved := testXMindTopic(manual.ID, "Renamed Topic", 1, 0, []int{0, 0})
	moved.ParentID = otherRoot.ID
	movedWorkbook := xmind.Workbook{
		FormatFamily: workbook.FormatFamily, SnapshotHash: strings.Repeat("e", 64),
		SemanticHash: strings.Repeat("f", 64), MediaManifestHash: strings.Repeat("0", 64),
		Sheets: []xmind.Sheet{
			{ID: "sheet-1", Title: "Architecture", Roots: []*xmind.Topic{rootWithoutTopic(root, manual.ID)}},
			{ID: "sheet-2", Title: "Other", Roots: []*xmind.Topic{cloneXMindTopicWithChild(otherRoot, moved)}},
		},
	}
	movedPlan, err := buildXMindBundle(domain.Source{ID: "src-xmind", Kind: "xmind"}, movedWorkbook)
	if err != nil {
		t.Fatalf("build moved bundle: %v", err)
	}
	movedSection := findXMindSection(t, movedPlan.bundle, manual.ID)
	if movedSection.ID != manualSection.ID {
		t.Fatalf("moved Section ID = %q, want stable %q", movedSection.ID, manualSection.ID)
	}
	if movedSection.DocumentID == manualSection.DocumentID || movedSection.Structure.ParentSectionID == manualSection.Structure.ParentSectionID {
		t.Fatalf("moved Topic did not update Document/parent: before=%+v after=%+v", manualSection.Structure, movedSection.Structure)
	}
}

func TestXMindSyncAtomicReconcileMediaAndFailedInputPreservesLastSuccess(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := sqlite.Open(ctx, "sqlite://"+filepath.Join(dataDir, "docgraph.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	mediaBytes := []byte("\x89PNG\r\n\x1a\nDocGraph-XMind-media")
	mediaDigest := sha256.Sum256(mediaBytes)
	packagePath := filepath.Join(t.TempDir(), "workbook.xmind")
	writeXMindTestPackage(t, packagePath, mediaBytes)
	if _, err := db.CreateSource(ctx, domain.Source{ID: "src-xmind-sync", Kind: "xmind", Name: "Architecture Maps", DSN: packagePath}); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}

	root := testXMindTopic("root", "Root", 0, 0, []int{0})
	child := testXMindTopic("stable-topic", "Original Topic", 1, 0, []int{0, 0})
	child.ParentID = root.ID
	child.Assets = []xmind.AssetRef{{AssetID: "image-1", Role: "image", Ordinal: 0}}
	root.Children = []*xmind.Topic{child}
	otherRoot := testXMindTopic("other-root", "Other", 0, 0, []int{0})
	current := xmind.Workbook{
		FormatFamily: xmind.FormatClassicZenJSON, SemanticHash: strings.Repeat("1", 64), MediaManifestHash: strings.Repeat("2", 64),
		Sheets: []xmind.Sheet{
			{ID: "sheet-a", Title: "A", Roots: []*xmind.Topic{root}, Relationships: []xmind.Relationship{{ID: "relationship-delete", SourceTopicID: child.ID, TargetTopicID: otherRoot.ID, Label: "temporary"}}},
			{ID: "sheet-b", Title: "B", Roots: []*xmind.Topic{otherRoot}},
		},
		Assets: []xmind.Asset{{ID: "image-1", Kind: xmind.AssetImage, ResourcePath: "resources/image.png", OriginalName: "image.png", MediaType: "image/png", SHA256: hex.EncodeToString(mediaDigest[:]), SizeBytes: uint64(len(mediaBytes)), Status: xmind.InventoryPreserved}},
		FeatureInventory: xmind.FeatureInventory{Features: []xmind.FeatureEncounter{{
			Feature: "future_feature", Location: "/future", Status: xmind.InventoryUnsupported,
			Count: 1, ReasonCode: "unrecognized_test_feature",
		}}},
	}
	parser := XMindParserFunc(func(reader io.ReaderAt, size int64, _ xmind.Limits) (xmind.Workbook, error) {
		workbook := current
		workbook.SnapshotHash = hashXMindReaderAt(t, reader, size)
		return workbook, nil
	})
	service := NewServiceWithOptions(db, ServiceOptions{DataDir: dataDir, XMindParser: parser})
	first, err := service.SyncSource(ctx, "src-xmind-sync")
	if err != nil {
		t.Fatalf("first XMind sync: %v", err)
	}
	if first.Documents != 2 || first.ConnectorDiagnostics == nil || first.ConnectorDiagnostics.MediaAssets != 1 || first.ConnectorDiagnostics.ParseStatus != "parsed_with_warnings" {
		t.Fatalf("first result = %+v, want two Sheets and one media asset", first)
	}
	stableSection := findStoredSectionByElementID(t, db, "src-xmind-sync", child.ID)
	mediaStore := storage.MediaStore(db)
	assets, err := mediaStore.ListDocumentMediaAssets(ctx, stableSection.DocumentID, 10, 0)
	if err != nil || len(assets) != 1 || assets[0].BlobSHA256 == "" {
		t.Fatalf("stored media assets = %+v, err=%v", assets, err)
	}
	oldAssetID := assets[0].ID
	activeBefore, err := mediaStore.GetActiveSourceSnapshot(ctx, "src-xmind-sync")
	if err != nil {
		t.Fatalf("GetActiveSourceSnapshot: %v", err)
	}
	firstInventory, err := mediaStore.ListSourceFeatureInventory(ctx, "src-xmind-sync")
	if err != nil || len(firstInventory) == 0 {
		t.Fatalf("first feature inventory = %#v, err=%v", firstInventory, err)
	}
	firstEdges, err := db.ListSourceEdges(ctx, "src-xmind-sync", 100, 0)
	if err != nil {
		t.Fatalf("first ListSourceEdges: %v", err)
	}
	if !hasStoredEdgeID(firstEdges, relationshipEdgeID("src-xmind-sync", "relationship-delete")) {
		t.Fatalf("first Relationship edge missing: %#v", firstEdges)
	}
	firstSearchHits, err := db.SearchSections(ctx, "Original Topic", 10)
	if err != nil || !hasSearchSectionID(firstSearchHits, stableSection.ID) {
		t.Fatalf("first FTS result = %#v, err=%v", firstSearchHits, err)
	}

	// A damaged attempt made while the successful revision still contains a
	// Relationship and media must preserve the complete active aggregate.
	if err := os.WriteFile(packagePath, []byte("not a zip"), 0o600); err != nil {
		t.Fatalf("write first damaged package: %v", err)
	}
	if _, err := service.SyncSource(ctx, "src-xmind-sync"); err == nil || !strings.Contains(err.Error(), "detect XMind package") {
		t.Fatalf("first damaged sync error = %v, want pre-write detection failure", err)
	}
	if preserved, err := db.GetSection(ctx, stableSection.ID); err != nil || preserved.Title != "Original Topic" {
		t.Fatalf("Section after first failure = %#v, err=%v", preserved, err)
	}
	if preservedAssets, err := mediaStore.ListDocumentMediaAssets(ctx, stableSection.DocumentID, 10, 0); err != nil || len(preservedAssets) != 1 || preservedAssets[0].ID != oldAssetID {
		t.Fatalf("media after first failure = %#v, err=%v", preservedAssets, err)
	}
	if preservedEdges, err := db.ListSourceEdges(ctx, "src-xmind-sync", 100, 0); err != nil || !hasStoredEdgeID(preservedEdges, relationshipEdgeID("src-xmind-sync", "relationship-delete")) {
		t.Fatalf("Relationship after first failure = %#v, err=%v", preservedEdges, err)
	}
	if preservedHits, err := db.SearchSections(ctx, "Original Topic", 10); err != nil || !hasSearchSectionID(preservedHits, stableSection.ID) {
		t.Fatalf("FTS after first failure = %#v, err=%v", preservedHits, err)
	}
	if preservedInventory, err := mediaStore.ListSourceFeatureInventory(ctx, "src-xmind-sync"); err != nil || !reflect.DeepEqual(firstInventory, preservedInventory) {
		t.Fatalf("inventory after first failure = %#v, want %#v, err=%v", preservedInventory, firstInventory, err)
	}
	if preservedSnapshot, err := mediaStore.GetActiveSourceSnapshot(ctx, "src-xmind-sync"); err != nil || preservedSnapshot.ID != activeBefore.ID {
		t.Fatalf("snapshot after first failure = %#v, want %#v, err=%v", preservedSnapshot, activeBefore, err)
	}
	writeXMindTestPackage(t, packagePath, mediaBytes)

	// Rename and move the Topic across Sheets, remove its media and authored
	// Relationship, and verify stable identity plus ghost-free reconcile.
	root.Children = nil
	moved := testXMindTopic(child.ID, "Renamed Topic", 1, 0, []int{0, 0})
	moved.ParentID = otherRoot.ID
	otherWithChild := cloneXMindTopicWithChild(otherRoot, moved)
	current = xmind.Workbook{
		FormatFamily: xmind.FormatClassicZenJSON, SemanticHash: strings.Repeat("3", 64), MediaManifestHash: strings.Repeat("4", 64),
		Sheets: []xmind.Sheet{
			{ID: "sheet-a", Title: "A", Roots: []*xmind.Topic{root}},
			{ID: "sheet-b", Title: "B", Roots: []*xmind.Topic{otherWithChild}},
		},
	}
	writeXMindTestPackage(t, packagePath, nil)
	if _, err := service.SyncSource(ctx, "src-xmind-sync"); err != nil {
		t.Fatalf("second XMind sync: %v", err)
	}
	stableAfter := findStoredSectionByElementID(t, db, "src-xmind-sync", child.ID)
	if stableAfter.ID != stableSection.ID {
		t.Fatalf("stable Section ID after move = %q, want %q", stableAfter.ID, stableSection.ID)
	}
	if stableAfter.DocumentID == stableSection.DocumentID || stableAfter.Title != "Renamed Topic" || stableAfter.ParentSectionID == stableSection.ParentSectionID {
		t.Fatalf("moved stored Topic = %+v, before=%+v", stableAfter, stableSection)
	}
	if _, err := mediaStore.GetMediaAsset(ctx, oldAssetID); !errorsIsSQLNoRows(err) {
		t.Fatalf("deleted media GetMediaAsset error = %v, want sql.ErrNoRows", err)
	}
	edges, err := db.ListSourceEdges(ctx, "src-xmind-sync", 100, 0)
	if err != nil {
		t.Fatalf("ListSourceEdges: %v", err)
	}
	for _, edge := range edges {
		if edge.ID == relationshipEdgeID("src-xmind-sync", "relationship-delete") {
			t.Fatalf("deleted Relationship edge remains: %+v", edge)
		}
	}
	activeAfter, err := mediaStore.GetActiveSourceSnapshot(ctx, "src-xmind-sync")
	if err != nil || activeAfter.ID == activeBefore.ID {
		t.Fatalf("active snapshot after second sync = %+v, err=%v", activeAfter, err)
	}

	// A damaged package fails during pre-write detection and cannot erase the
	// last successful Topic, graph, media state, FTS, or active snapshot.
	if err := os.WriteFile(packagePath, []byte("not a zip"), 0o600); err != nil {
		t.Fatalf("write damaged package: %v", err)
	}
	if _, err := service.SyncSource(ctx, "src-xmind-sync"); err == nil || !strings.Contains(err.Error(), "detect XMind package") {
		t.Fatalf("damaged sync error = %v, want pre-write detection failure", err)
	}
	preserved, err := db.GetSection(ctx, stableSection.ID)
	if err != nil || preserved.Title != "Renamed Topic" {
		t.Fatalf("last successful Section after failure = %+v, err=%v", preserved, err)
	}
	activeFailed, err := mediaStore.GetActiveSourceSnapshot(ctx, "src-xmind-sync")
	if err != nil || activeFailed.ID != activeAfter.ID {
		t.Fatalf("active snapshot after failed sync = %+v, want %+v, err=%v", activeFailed, activeAfter, err)
	}
}

func TestXMindProductionParseFailurePreservesLastSuccessfulAggregate(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := sqlite.Open(ctx, "sqlite://"+filepath.Join(dataDir, "docgraph.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	packagePath := filepath.Join(t.TempDir(), "parse-failure.xmind")
	mediaBytes := []byte("\x89PNG\r\n\x1a\nparse-failure-media")
	validContent := `[{"id":"sheet","title":"Stable Sheet","rootTopic":{"id":"root","title":"Stable Root","children":{"attached":[{"id":"child","title":"Parse failure needle","image":{"src":"xap:resources/image.png"}}]}},"relationships":[{"id":"stable-relation","end1Id":"root","end2Id":"child"}]}]`
	writeXMindJSONPackage(t, packagePath, validContent, map[string][]byte{"resources/image.png": mediaBytes})
	const sourceID = "source-production-parse-failure"
	if _, err := db.CreateSource(ctx, domain.Source{ID: sourceID, Kind: "xmind", Name: "Parse Failure", DSN: packagePath}); err != nil {
		t.Fatal(err)
	}
	service := NewServiceWithOptions(db, ServiceOptions{DataDir: dataDir})
	if _, err := service.SyncSource(ctx, sourceID); err != nil {
		t.Fatalf("initial production sync: %v", err)
	}
	child := findStoredSectionByElementID(t, db, sourceID, "child")
	mediaStore := storage.MediaStore(db)
	assetsBefore, err := mediaStore.ListDocumentMediaAssets(ctx, child.DocumentID, 10, 0)
	if err != nil || len(assetsBefore) != 1 {
		t.Fatalf("assets before parse failure = %#v, err=%v", assetsBefore, err)
	}
	edgesBefore, err := db.ListSourceEdges(ctx, sourceID, 100, 0)
	if err != nil || !hasStoredEdgeID(edgesBefore, relationshipEdgeID(sourceID, "stable-relation")) {
		t.Fatalf("edges before parse failure = %#v, err=%v", edgesBefore, err)
	}
	inventoryBefore, err := mediaStore.ListSourceFeatureInventory(ctx, sourceID)
	if err != nil || len(inventoryBefore) == 0 {
		t.Fatalf("inventory before parse failure = %#v, err=%v", inventoryBefore, err)
	}
	snapshotBefore, err := mediaStore.GetActiveSourceSnapshot(ctx, sourceID)
	if err != nil {
		t.Fatal(err)
	}

	// The root signature passes package detection, but full parsing rejects the
	// nested Topic without a source ID before Blob or relational replacement.
	invalidContent := `[{"id":"sheet","rootTopic":{"id":"root","children":{"attached":[{"title":"missing stable ID"}]}}}]`
	writeXMindJSONPackage(t, packagePath, invalidContent, nil)
	if _, err := service.SyncSource(ctx, sourceID); err == nil || !strings.Contains(err.Error(), "parse XMind workbook") {
		t.Fatalf("production parse failure = %v", err)
	}
	if sectionAfter, err := db.GetSection(ctx, child.ID); err != nil || sectionAfter.Title != "Parse failure needle" {
		t.Fatalf("Section after parse failure = %#v, err=%v", sectionAfter, err)
	}
	if hitsAfter, err := db.SearchSections(ctx, "Parse failure needle", 10); err != nil || !hasSearchSectionID(hitsAfter, child.ID) {
		t.Fatalf("FTS after parse failure = %#v, err=%v", hitsAfter, err)
	}
	if assetsAfter, err := mediaStore.ListDocumentMediaAssets(ctx, child.DocumentID, 10, 0); err != nil || !reflect.DeepEqual(assetsBefore, assetsAfter) {
		t.Fatalf("media after parse failure = %#v, want %#v, err=%v", assetsAfter, assetsBefore, err)
	}
	if edgesAfter, err := db.ListSourceEdges(ctx, sourceID, 100, 0); err != nil || !reflect.DeepEqual(edgesBefore, edgesAfter) {
		t.Fatalf("edges after parse failure = %#v, want %#v, err=%v", edgesAfter, edgesBefore, err)
	}
	if inventoryAfter, err := mediaStore.ListSourceFeatureInventory(ctx, sourceID); err != nil || !reflect.DeepEqual(inventoryBefore, inventoryAfter) {
		t.Fatalf("inventory after parse failure = %#v, want %#v, err=%v", inventoryAfter, inventoryBefore, err)
	}
	if snapshotAfter, err := mediaStore.GetActiveSourceSnapshot(ctx, sourceID); err != nil || snapshotAfter.ID != snapshotBefore.ID {
		t.Fatalf("snapshot after parse failure = %#v, want %#v, err=%v", snapshotAfter, snapshotBefore, err)
	}
}

func hasStoredEdgeID(edges []domain.EdgeSummary, id string) bool {
	for _, edge := range edges {
		if edge.ID == id {
			return true
		}
	}
	return false
}

func hasSearchSectionID(hits []domain.SearchHit, id string) bool {
	for _, hit := range hits {
		if hit.SectionID == id {
			return true
		}
	}
	return false
}

func TestBuildXMindBundleReusesTopicLikeSummaryAndCalloutComponents(t *testing.T) {
	root := testXMindTopic("root", "Root", 0, 0, []int{0})
	callout := testXMindTopic("callout-component", "Callout", 1, 0, []int{0, 0})
	callout.Kind = "callout"
	callout.ChildRole = "callout"
	callout.ParentID = root.ID
	root.Children = []*xmind.Topic{callout}
	workbook := xmind.Workbook{
		FormatFamily: xmind.FormatJSONCandidate, SnapshotHash: strings.Repeat("1", 64),
		SemanticHash: strings.Repeat("2", 64), MediaManifestHash: strings.Repeat("3", 64),
		Sheets: []xmind.Sheet{{
			ID: "sheet", Title: "Sheet", Roots: []*xmind.Topic{root},
			Elements: []xmind.Element{{ID: callout.ID, Kind: "callout", Title: callout.RawTitle, OwnerTopicID: root.ID, Status: xmind.InventoryIndexed}},
		}},
	}
	plan, err := buildXMindBundle(domain.Source{ID: "source", Kind: "xmind"}, workbook)
	if err != nil {
		t.Fatalf("buildXMindBundle: %v", err)
	}
	sections := 0
	annotates := 0
	for _, document := range plan.bundle.Documents {
		for _, section := range document.Sections {
			if section.Structure != nil && section.Structure.SourceElementID == callout.ID {
				sections++
			}
		}
	}
	for _, edge := range plan.bundle.Edges {
		if edge.Kind == "annotates" {
			annotates++
		}
	}
	if sections != 1 || annotates != 1 {
		t.Fatalf("topic-like callout sections=%d annotates=%d, want 1/1", sections, annotates)
	}
}

func TestBuildXMindBundleSpecialElementOrderPathExtendsOwnerPath(t *testing.T) {
	root := testXMindTopic("root-order", "Root", 0, 0, []int{0})
	owner := testXMindTopic("owner-order", "Owner", 1, 0, []int{0, 0})
	owner.ParentID = root.ID
	root.Children = []*xmind.Topic{owner}
	workbook := xmind.Workbook{
		FormatFamily: xmind.FormatClassicZenJSON,
		SnapshotHash: strings.Repeat("1", 64), SemanticHash: strings.Repeat("2", 64), MediaManifestHash: strings.Repeat("3", 64),
		Sheets: []xmind.Sheet{{
			ID: "sheet-order", Title: "Order path", Roots: []*xmind.Topic{root},
			Elements: []xmind.Element{{
				ID: "boundary-order", Kind: "boundary", Title: "Boundary", OwnerTopicID: owner.ID,
				TargetIDs: []string{owner.ID}, Range: "(0,0)", Status: xmind.InventoryPreserved,
			}},
		}},
	}
	plan, err := buildXMindBundle(domain.Source{ID: "source-order", Kind: "xmind"}, workbook)
	if err != nil {
		t.Fatalf("buildXMindBundle: %v", err)
	}
	boundary := findXMindSection(t, plan.bundle, "boundary-order")
	if boundary.Structure == nil {
		t.Fatal("Boundary has no structure")
	}
	if boundary.Structure.Depth != 2 || boundary.Structure.ParentSectionID == "" || !reflect.DeepEqual(boundary.Structure.OrderPath, []int{0, 0, 0}) {
		t.Fatalf("Boundary structure = %+v, want owner path [0,0] plus special ordinal", boundary.Structure)
	}
}

func TestBuildXMindBundleRejectsInvalidOrderPathBeforeStaging(t *testing.T) {
	root := testXMindTopic("root-invalid-order", "Root", 0, 0, []int{0})
	child := testXMindTopic("child-invalid-order", "Child", 2, 0, []int{0})
	child.ParentID = root.ID
	root.Children = []*xmind.Topic{child}
	workbook := xmind.Workbook{
		FormatFamily: xmind.FormatClassicZenJSON,
		SnapshotHash: strings.Repeat("1", 64), SemanticHash: strings.Repeat("2", 64), MediaManifestHash: strings.Repeat("3", 64),
		Sheets: []xmind.Sheet{{ID: "sheet-invalid-order", Roots: []*xmind.Topic{root}}},
	}
	_, err := buildXMindBundle(domain.Source{ID: "source-invalid-order", Kind: "xmind"}, workbook)
	if err == nil || !strings.Contains(err.Error(), "invalid depth/sibling/order_path") {
		t.Fatalf("invalid order_path error = %v", err)
	}
}

func TestBuildXMindBundleRejectsOrderPathOutsideParentPrefix(t *testing.T) {
	root := testXMindTopic("root-invalid-prefix", "Root", 0, 0, []int{0})
	child := testXMindTopic("child-invalid-prefix", "Child", 1, 0, []int{9, 0})
	child.ParentID = root.ID
	root.Children = []*xmind.Topic{child}
	workbook := xmind.Workbook{
		FormatFamily: xmind.FormatClassicZenJSON,
		SnapshotHash: strings.Repeat("1", 64), SemanticHash: strings.Repeat("2", 64), MediaManifestHash: strings.Repeat("3", 64),
		Sheets: []xmind.Sheet{{ID: "sheet-invalid-prefix", Roots: []*xmind.Topic{root}}},
	}
	_, err := buildXMindBundle(domain.Source{ID: "source-invalid-prefix", Kind: "xmind"}, workbook)
	if err == nil || !strings.Contains(err.Error(), "does not extend parent") {
		t.Fatalf("invalid parent order_path error = %v", err)
	}
}

func TestBuildXMindBundlePresentationPositionDoesNotChangeContentHashes(t *testing.T) {
	topic := testXMindTopic("floating-hash", "Floating", 0, 0, []int{0})
	topic.Kind = "floating"
	topic.ChildRole = "detached"
	topic.Position = xmind.Position{X: 10, Y: 20, Floating: true}
	workbook := xmind.Workbook{
		FormatFamily: xmind.FormatClassicZenJSON,
		SnapshotHash: strings.Repeat("1", 64), SemanticHash: strings.Repeat("2", 64), MediaManifestHash: strings.Repeat("3", 64),
		Sheets: []xmind.Sheet{{ID: "sheet-hash", Title: "Hash", Roots: []*xmind.Topic{topic}}},
	}
	first, err := buildXMindBundle(domain.Source{ID: "source-hash", Kind: "xmind"}, workbook)
	if err != nil {
		t.Fatal(err)
	}
	movedTopic := *topic
	movedTopic.Position.X = 999
	movedTopic.Position.Y = -500
	workbook.Sheets[0].Roots = []*xmind.Topic{&movedTopic}
	second, err := buildXMindBundle(domain.Source{ID: "source-hash", Kind: "xmind"}, workbook)
	if err != nil {
		t.Fatal(err)
	}
	firstSection := findXMindSection(t, first.bundle, topic.ID)
	secondSection := findXMindSection(t, second.bundle, topic.ID)
	if firstSection.Structure == nil || secondSection.Structure == nil || firstSection.Structure.PresentationJSON == secondSection.Structure.PresentationJSON {
		t.Fatalf("position did not update presentation: first=%+v second=%+v", firstSection.Structure, secondSection.Structure)
	}
	if firstSection.ContentHash != secondSection.ContentHash || first.bundle.Documents[0].Document.ContentHash != second.bundle.Documents[0].Document.ContentHash {
		t.Fatalf("position-only move changed text hashes: section %s/%s document %s/%s", firstSection.ContentHash, secondSection.ContentHash, first.bundle.Documents[0].Document.ContentHash, second.bundle.Documents[0].Document.ContentHash)
	}
	renamedTopic := movedTopic
	renamedTopic.RawTitle = "Renamed Floating"
	workbook.Sheets[0].Roots = []*xmind.Topic{&renamedTopic}
	renamed, err := buildXMindBundle(domain.Source{ID: "source-hash", Kind: "xmind"}, workbook)
	if err != nil {
		t.Fatal(err)
	}
	renamedSection := findXMindSection(t, renamed.bundle, topic.ID)
	if renamedSection.ContentHash == secondSection.ContentHash || renamed.bundle.Documents[0].Document.ContentHash == second.bundle.Documents[0].Document.ContentHash {
		t.Fatal("title change did not update text/document hashes")
	}
}

func TestBuildXMindBundleReportsParserWarningsAndPreservesUnresolvedTopicLink(t *testing.T) {
	root := testXMindTopic("root-warning", "Root", 0, 0, []int{0})
	root.Links = []xmind.Link{{
		ID: "link-missing", Target: "xmind:#missing-sheet/missing-topic",
		TargetSheetID: "missing-sheet", TargetTopicID: "missing-topic",
	}}
	workbook := xmind.Workbook{
		FormatFamily: xmind.FormatClassicZenJSON,
		SnapshotHash: strings.Repeat("1", 64), SemanticHash: strings.Repeat("2", 64), MediaManifestHash: strings.Repeat("3", 64),
		Sheets: []xmind.Sheet{{
			ID: "sheet-warning", Roots: []*xmind.Topic{root},
			Relationships: []xmind.Relationship{{ID: "rel-missing", SourceTopicID: root.ID, TargetTopicID: "missing-topic"}},
		}},
		FeatureInventory: xmind.FeatureInventory{Features: []xmind.FeatureEncounter{{
			Feature: "topic_link_target", SourceElementID: root.ID, Location: "/links/0",
			Status: xmind.InventoryUnsupported, Count: 1, ReasonCode: "missing_internal_link_target",
		}}},
	}
	plan, err := buildXMindBundle(domain.Source{ID: "source-warning", Kind: "xmind"}, workbook)
	if err != nil {
		t.Fatalf("buildXMindBundle: %v", err)
	}
	if plan.diagnostics.ParseStatus != "parsed_with_warnings" || len(plan.diagnostics.Warnings) < 3 {
		t.Fatalf("warning diagnostics = %#v", plan.diagnostics)
	}
	foundUnresolvedNode := false
	foundLinkEdge := false
	foundRelationshipEdge := false
	for _, node := range plan.bundle.Nodes {
		foundUnresolvedNode = foundUnresolvedNode || node.Kind == "ExternalResource" && strings.Contains(node.MetadataJSON, `"unresolved_internal":true`)
	}
	for _, edge := range plan.bundle.Edges {
		foundLinkEdge = foundLinkEdge || edge.Kind == "links_to" && strings.Contains(edge.MetadataJSON, `"target_topic_id":"missing-topic"`)
		foundRelationshipEdge = foundRelationshipEdge || edge.Kind == "related_to"
	}
	if !foundUnresolvedNode || !foundLinkEdge || foundRelationshipEdge {
		t.Fatalf("unresolved local references were not handled safely: node=%v link=%v relationship=%v", foundUnresolvedNode, foundLinkEdge, foundRelationshipEdge)
	}
}

func TestXMindFeatureInventoryAggregatesLargeCoverageWithBoundedSamples(t *testing.T) {
	inventory := xmind.FeatureInventory{}
	for index := 0; index < 10000; index++ {
		inventory.Features = append(inventory.Features, xmind.FeatureEncounter{
			Feature: "topic", SourceName: "attached", SourceElementID: "topic-" + strconv.Itoa(index),
			Location: "/topics/" + strconv.Itoa(index), Status: xmind.InventoryIndexed, Count: 1,
		})
	}
	for index := 0; index < 25; index++ {
		inventory.Features = append(inventory.Features, xmind.FeatureEncounter{
			Feature: "unknown_json_field", SourceName: "future", Location: "/future/" + strconv.Itoa(index),
			Status: xmind.InventoryUnsupported, Count: 1, ReasonCode: "unrecognized_json_key",
		})
	}
	for index := 0; index < 100; index++ {
		inventory.Resources = append(inventory.Resources, xmind.ResourceEncounter{
			PackageEntry: xmind.PackageEntry{Path: "resources/" + strconv.Itoa(index) + ".png"},
			Status:       xmind.InventoryPreserved, ReasonCode: "package_snapshot", Referenced: true, DetectedMIME: "image/png",
		})
	}
	rows := xmindFeatureInventory("snapshot-large-inventory", inventory)
	if len(rows) != 3 {
		t.Fatalf("aggregated inventory rows = %d, want one topic, unknown, and resource summary: %#v", len(rows), rows)
	}
	counts := map[string]int{}
	for _, row := range rows {
		counts[row.FeatureKey+"/"+row.CoverageStatus] = row.Count
		if !strings.Contains(row.MetadataJSON, `"details_truncated":true`) {
			t.Fatalf("large aggregate lacks explicit sample truncation: %+v", row)
		}
	}
	if counts["topic/indexed"] != 10000 || counts["unknown_json_field/unsupported"] != 25 || counts["package_resource/preserved"] != 100 {
		t.Fatalf("aggregated inventory counts = %#v", counts)
	}
}

func TestXMindProductionSyncRealClassicJSONFixture(t *testing.T) {
	ctx := context.Background()
	fixturePath, err := filepath.Abs(filepath.Join("..", "..", "docs", "tests", "烘焙知识体系.xmind"))
	if err != nil {
		t.Fatalf("resolve real XMind fixture: %v", err)
	}
	if _, err := os.Stat(fixturePath); err != nil {
		t.Fatalf("stat real XMind fixture: %v", err)
	}
	dataDir := t.TempDir()
	db, err := sqlite.Open(ctx, "sqlite://"+filepath.Join(dataDir, "docgraph.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	const sourceID = "src-real-classic-xmind"
	if _, err := db.CreateSource(ctx, domain.Source{ID: sourceID, Kind: "xmind", Name: "烘焙知识体系", DSN: fixturePath}); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	service := NewServiceWithOptions(db, ServiceOptions{DataDir: dataDir})
	result, err := service.SyncSource(ctx, sourceID)
	if err != nil {
		t.Fatalf("production real-fixture sync: %v", err)
	}
	if result.Documents != 1 || result.ConnectorDiagnostics == nil || result.ConnectorDiagnostics.FormatFamily != string(xmind.FormatClassicZenJSON) || result.ConnectorDiagnostics.FormatVersion != "12.0.2.202204260729" || result.ConnectorDiagnostics.Topics != 84 {
		t.Fatalf("sync result = %#v", result)
	}
	documents, err := db.ListSourceDocuments(ctx, sourceID, 10, 0)
	if err != nil || len(documents) != 1 || documents[0].Title != "Map" {
		t.Fatalf("stored Documents = %#v, err=%v", documents, err)
	}
	sections, err := db.ListSourceSections(ctx, sourceID, 100, 0)
	if err != nil || len(sections) != 84 {
		t.Fatalf("stored Sections = %d, err=%v", len(sections), err)
	}
	var deep domain.SectionSummary
	for _, section := range sections {
		if section.SourceElementID == "" {
			t.Fatalf("Section lost XMind Topic ID: %#v", section)
		}
		if section.DisplayNumber != "" {
			t.Fatalf("manual title was persisted as dynamic numbering: %#v", section)
		}
		if section.SourceElementID == "e7359f10-a377-4f04-adcc-da9acefd0c9e" {
			deep = section
		}
	}
	if deep.ID == "" || deep.Title != "1.1.1 面筋形成" || deep.Depth != 3 || deep.ParentSectionID == "" {
		t.Fatalf("deep stored Topic = %#v", deep)
	}
	contextResult, err := db.GetSectionContext(ctx, deep.ID, 10)
	if err != nil || len(contextResult.Ancestors) != 3 || len(contextResult.MediaAssets) != 0 {
		t.Fatalf("deep Topic context = %#v, err=%v", contextResult, err)
	}
	hits, err := db.SearchSections(ctx, "1.1.1 面筋形成", 10)
	if err != nil || len(hits) == 0 || hits[0].SectionID != deep.ID || hits[0].DisplayNumber != "" {
		t.Fatalf("manual-number title search = %#v, err=%v", hits, err)
	}
	warningHits, err := db.SearchSections(ctx, "永久性丢失", 10)
	if err != nil || len(warningHits) != 0 {
		t.Fatalf("inactive compatibility XML leaked into search: %#v, err=%v", warningHits, err)
	}
	nodes, err := db.ListSourceNodes(ctx, sourceID, 200, 0)
	if err != nil || len(nodes) != 85 {
		t.Fatalf("stored Nodes = %d, err=%v", len(nodes), err)
	}
	docSectionNodes := 0
	for _, node := range nodes {
		if node.Kind == "DocSection" {
			docSectionNodes++
		}
	}
	if docSectionNodes != 84 {
		t.Fatalf("DocSection Nodes = %d, want 84", docSectionNodes)
	}
	edges, err := db.ListSourceEdges(ctx, sourceID, 200, 0)
	if err != nil {
		t.Fatalf("ListSourceEdges: %v", err)
	}
	documentContains := 0
	topicContains := 0
	for _, edge := range edges {
		if edge.Kind != "contains" {
			t.Fatalf("fixture invented non-hierarchy edge: %#v", edge)
		}
		if edge.SrcKind == "Document" {
			documentContains++
		} else if edge.SrcKind == "DocSection" {
			topicContains++
		}
	}
	if documentContains != 1 || topicContains != 83 {
		t.Fatalf("contains graph = document:%d topic:%d, want 1/83", documentContains, topicContains)
	}
	mediaStore := storage.MediaStore(db)
	snapshot, err := mediaStore.GetActiveSourceSnapshot(ctx, sourceID)
	if err != nil || snapshot.SourceHash != "d4ab251a7ecd528210720693119381464c9a786b0b60326626036624c38874f0" || snapshot.BlobSHA256 != snapshot.SourceHash {
		t.Fatalf("active content-addressed snapshot = %#v, err=%v", snapshot, err)
	}
	assets, err := mediaStore.ListDocumentMediaAssets(ctx, documents[0].ID, 10, 0)
	if err != nil || len(assets) != 0 {
		t.Fatalf("workbook thumbnail became Topic media: %#v, err=%v", assets, err)
	}
	inventory, err := mediaStore.ListSourceFeatureInventory(ctx, sourceID)
	if err != nil || len(inventory) == 0 {
		t.Fatalf("stored feature inventory = %#v, err=%v", inventory, err)
	}
	foundInactiveXML := false
	foundTheme := false
	for _, feature := range inventory {
		foundInactiveXML = foundInactiveXML || feature.FeatureKey == "inactive_workbook_payload" && feature.CoverageStatus == string(xmind.InventoryPreserved)
		foundTheme = foundTheme || feature.FeatureKey == "theme" && feature.CoverageStatus == string(xmind.InventoryPreserved)
	}
	if !foundInactiveXML || !foundTheme {
		t.Fatalf("stored inventory lacks compatibility/theme preservation: %#v", inventory)
	}
}

func TestXMindProductionSyncRealRichLegacyJSONFixture(t *testing.T) {
	ctx := context.Background()
	fixturePath, err := filepath.Abs(filepath.Join("..", "..", "docs", "tests", "classic-rich-v1.xmind"))
	if err != nil {
		t.Fatalf("resolve real rich XMind fixture: %v", err)
	}
	dataDir := t.TempDir()
	db, err := sqlite.Open(ctx, "sqlite://"+filepath.Join(dataDir, "docgraph.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	const sourceID = "src-real-rich-xmind"
	if _, err := db.CreateSource(ctx, domain.Source{ID: sourceID, Kind: "xmind", Name: "Rich XMind", DSN: fixturePath}); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	service := NewServiceWithOptions(db, ServiceOptions{DataDir: dataDir})
	result, err := service.SyncSource(ctx, sourceID)
	if err != nil {
		t.Fatalf("production rich real-fixture sync: %v", err)
	}
	if result.Documents != 3 || result.ConnectorDiagnostics == nil || result.ConnectorDiagnostics.FormatVersion != "26.01.03145" || result.ConnectorDiagnostics.Topics != 55 || result.ConnectorDiagnostics.MediaAssets != 4 || result.ConnectorDiagnostics.AuthoredEdges != 5 {
		t.Fatalf("rich sync result = %#v", result)
	}
	documents, err := db.ListSourceDocuments(ctx, sourceID, 10, 0)
	if err != nil || len(documents) != 3 {
		t.Fatalf("rich Documents = %#v, err=%v", documents, err)
	}
	sections, err := db.ListSourceSections(ctx, sourceID, 100, 0)
	if err != nil || len(sections) != 55 {
		t.Fatalf("rich Sections = %d, err=%v", len(sections), err)
	}
	containsDocument := 0
	containsTopic := 0
	authoredKinds := map[string]int{}
	edges, err := db.ListSourceEdges(ctx, sourceID, 200, 0)
	if err != nil {
		t.Fatalf("ListSourceEdges: %v", err)
	}
	for _, edge := range edges {
		if edge.Kind == "contains" && edge.SrcKind == "Document" {
			containsDocument++
		} else if edge.Kind == "contains" && edge.SrcKind == "DocSection" {
			containsTopic++
		} else {
			authoredKinds[edge.Kind]++
		}
	}
	if containsDocument != 4 || containsTopic != 51 || authoredKinds["summarizes"] != 2 || authoredKinds["annotates"] != 1 || authoredKinds["related_to"] != 1 || authoredKinds["links_to"] != 1 {
		t.Fatalf("rich graph = document:%d topic:%d authored:%#v", containsDocument, containsTopic, authoredKinds)
	}
	summarySection := findStoredSectionByElementID(t, db, sourceID, "a92ac19b-e4ec-4a87-8c3b-c6f01e282b78")
	summaryRelations, err := db.RelatedNodes(ctx, summarySection.NodeID, domain.RelatedOptions{Direction: "out", Kind: "summarizes", Limit: 10})
	if err != nil || len(summaryRelations) != 2 {
		t.Fatalf("Summary relations = %#v, err=%v", summaryRelations, err)
	}
	for _, relation := range summaryRelations {
		if relation.Edge.Provenance != "source_authored" || !strings.Contains(relation.Edge.MetadataJSON, `"xmind_definition_id":"8e7786a0-914d-4b76-a321-d520415cac43"`) {
			t.Fatalf("Summary edge lost definition provenance: %#v", relation)
		}
	}
	calloutSection := findStoredSectionByElementID(t, db, sourceID, "02e80930-e774-4239-bd18-9c97627a023b")
	calloutRelations, err := db.RelatedNodes(ctx, calloutSection.NodeID, domain.RelatedOptions{Direction: "out", Kind: "annotates", Limit: 10})
	if err != nil || len(calloutRelations) != 1 {
		t.Fatalf("Callout relations = %#v, err=%v", calloutRelations, err)
	}
	deep := findStoredSectionByElementID(t, db, sourceID, "0bf8a1bf-0009-4d51-a591-5b73e951a5a0")
	if deep.Depth != 12 || len(deep.OrderPath) != 13 {
		t.Fatalf("real depth-12 Topic = %#v", deep)
	}
	deepContext, err := db.GetSectionContext(ctx, deep.ID, 10)
	if err != nil || len(deepContext.Ancestors) != 12 {
		t.Fatalf("real deep Topic context ancestors=%d err=%v", len(deepContext.Ancestors), err)
	}
	markerHits, err := db.SearchSections(ctx, "people-red", 10)
	if err != nil || len(markerHits) == 0 || markerHits[0].Title != "profile" {
		t.Fatalf("raw Marker ID search = %#v, err=%v", markerHits, err)
	}
	relationshipHits, err := db.SearchSections(ctx, "relation to b", 10)
	if err != nil || len(relationshipHits) == 0 {
		t.Fatalf("Relationship label search = %#v, err=%v", relationshipHits, err)
	}
	mediaTypeHits, err := db.SearchSections(ctx, "image/webp", 10)
	if err != nil || len(mediaTypeHits) == 0 {
		t.Fatalf("media MIME search = %#v, err=%v", mediaTypeHits, err)
	}
	var mediaDocument domain.DocumentSummary
	for _, document := range documents {
		if document.Title == "DG-SPECIAL-MEDIA" {
			mediaDocument = document
		}
	}
	if mediaDocument.ID == "" {
		t.Fatal("DG-SPECIAL-MEDIA Document not found")
	}
	mediaStore := storage.MediaStore(db)
	assets, err := mediaStore.ListDocumentMediaAssets(ctx, mediaDocument.ID, 10, 0)
	if err != nil || len(assets) != 4 {
		t.Fatalf("stored rich media assets = %#v, err=%v", assets, err)
	}
	foundMisnamedJPEG := false
	for _, asset := range assets {
		if strings.HasSuffix(asset.OriginalName, ".png") {
			foundMisnamedJPEG = asset.MediaType == "image/jpeg" && asset.Status == "available"
		}
	}
	if !foundMisnamedJPEG {
		t.Fatalf("misnamed PNG was not content-sniffed as JPEG: %#v", assets)
	}
	mediaParent := findStoredSectionByElementID(t, db, sourceID, "8530d7ca-5715-4ce3-923f-d08e27cbe5eb")
	mediaChild := findStoredSectionByElementID(t, db, sourceID, "9d18e1ac-cc52-40ea-b68c-39c56b57a60d")
	sectionAssets, err := mediaStore.ListSectionMediaAssets(ctx, []string{mediaParent.ID, mediaChild.ID})
	if err != nil || len(sectionAssets[mediaParent.ID]) != 1 || len(sectionAssets[mediaChild.ID]) != 1 || sectionAssets[mediaParent.ID][0].ID != sectionAssets[mediaChild.ID][0].ID {
		t.Fatalf("shared image Section refs = %#v, err=%v", sectionAssets, err)
	}
	snapshot, err := mediaStore.GetActiveSourceSnapshot(ctx, sourceID)
	if err != nil || snapshot.SourceHash != "4df1a27ee86803aa9972953c176970472ef5ce44fa622f56e9664c3e99a56701" || snapshot.BlobSHA256 != snapshot.SourceHash {
		t.Fatalf("rich active snapshot = %#v, err=%v", snapshot, err)
	}
	warningHits, err := db.SearchSections(ctx, "permanently lost", 10)
	if err != nil || len(warningHits) != 0 {
		t.Fatalf("inactive compatibility XML leaked into rich search: %#v, err=%v", warningHits, err)
	}
}

func testXMindTopic(id, title string, depth, sibling int, orderPath []int) *xmind.Topic {
	return &xmind.Topic{ID: id, Kind: "topic", RawTitle: title, Depth: depth, SiblingOrdinal: sibling, OrderPath: append([]int(nil), orderPath...)}
}

func testItoa(value int) string {
	return strconv.Itoa(value)
}

func findXMindSection(t *testing.T, bundle storage.WorkbookBundle, sourceElementID string) storage.SectionInput {
	t.Helper()
	for _, document := range bundle.Documents {
		for _, section := range document.Sections {
			if section.Structure != nil && section.Structure.SourceElementID == sourceElementID {
				return section
			}
		}
	}
	t.Fatalf("source element %q not found", sourceElementID)
	return storage.SectionInput{}
}

func rootWithoutTopic(root *xmind.Topic, removeID string) *xmind.Topic {
	copyRoot := *root
	copyRoot.Children = nil
	for _, child := range root.Children {
		if child.ID != removeID {
			copyRoot.Children = append(copyRoot.Children, child)
		}
	}
	return &copyRoot
}

func cloneXMindTopicWithChild(root, child *xmind.Topic) *xmind.Topic {
	clone := *root
	clone.Children = append([]*xmind.Topic(nil), root.Children...)
	clone.Children = append(clone.Children, child)
	return &clone
}

func writeXMindTestPackage(t *testing.T, path string, media []byte) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create XMind test package: %v", err)
	}
	archive := zip.NewWriter(file)
	content, err := archive.Create("content.json")
	if err != nil {
		t.Fatalf("create content.json: %v", err)
	}
	if _, err := content.Write([]byte(`[{"id":"fixture-contract-only","rootTopic":{"id":"fixture-root"}}]`)); err != nil {
		t.Fatalf("write content.json: %v", err)
	}
	if len(media) > 0 {
		entry, err := archive.Create("resources/image.png")
		if err != nil {
			t.Fatalf("create media entry: %v", err)
		}
		if _, err := entry.Write(media); err != nil {
			t.Fatalf("write media entry: %v", err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatalf("close XMind ZIP: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close XMind package: %v", err)
	}
}

func writeXMindJSONPackage(t *testing.T, targetPath, content string, resources map[string][]byte) {
	t.Helper()
	file, err := os.Create(targetPath)
	if err != nil {
		t.Fatalf("create XMind JSON package: %v", err)
	}
	writer := zip.NewWriter(file)
	contentEntry, err := writer.CreateHeader(&zip.FileHeader{Name: "content.json", Method: zip.Store})
	if err != nil {
		t.Fatalf("create content.json: %v", err)
	}
	if _, err := contentEntry.Write([]byte(content)); err != nil {
		t.Fatalf("write content.json: %v", err)
	}
	paths := make([]string, 0, len(resources))
	for resourcePath := range resources {
		paths = append(paths, resourcePath)
	}
	sort.Strings(paths)
	for _, resourcePath := range paths {
		entry, err := writer.CreateHeader(&zip.FileHeader{Name: resourcePath, Method: zip.Store})
		if err != nil {
			t.Fatalf("create resource %q: %v", resourcePath, err)
		}
		if _, err := entry.Write(resources[resourcePath]); err != nil {
			t.Fatalf("write resource %q: %v", resourcePath, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close XMind JSON ZIP: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close XMind JSON package: %v", err)
	}
}

func hashXMindReaderAt(t *testing.T, reader io.ReaderAt, size int64) string {
	t.Helper()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.NewSectionReader(reader, 0, size)); err != nil {
		t.Fatalf("hash XMind package: %v", err)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func findStoredSectionByElementID(t *testing.T, db *sqlite.Store, sourceID, elementID string) domain.SectionSummary {
	t.Helper()
	sectionID := ids.Stable("sec", sourceID, xmindWorkbookKey, elementID)
	section, err := db.GetSection(context.Background(), sectionID)
	if err != nil {
		t.Fatalf("GetSection(%q): %v", sectionID, err)
	}
	summary := domain.SectionSummary{
		ID: section.SectionID, DocumentID: section.DocumentID, DocumentTitle: section.DocumentTitle,
		Title: section.Title, HeadingPath: section.HeadingPath, MetadataJSON: section.MetadataJSON, NodeID: section.NodeID,
	}
	if section.Structure != nil {
		summary.ParentSectionID = section.Structure.ParentSectionID
		summary.SourceElementID = section.Structure.SourceElementID
		summary.ElementKind = section.Structure.ElementKind
		summary.Depth = section.Structure.Depth
		summary.SiblingOrdinal = section.Structure.SiblingOrdinal
		summary.OrderPath = section.Structure.OrderPath
		summary.DisplayNumber = section.Structure.DisplayNumber
	}
	return summary
}

func relationshipEdgeID(sourceID, relationshipID string) string {
	return ids.Stable("edge", sourceID, xmindWorkbookKey, relationshipID)
}

func errorsIsSQLNoRows(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}
