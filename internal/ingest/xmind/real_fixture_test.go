package xmind

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	realClassicFixtureHash    = "d4ab251a7ecd528210720693119381464c9a786b0b60326626036624c38874f0"
	realClassicFixtureVersion = "12.0.2.202204260729"
	realRichFixtureHash       = "4df1a27ee86803aa9972953c176970472ef5ce44fa622f56e9664c3e99a56701"
	realRichFixtureVersion    = "26.01.03145"
)

func openRealClassicFixture(t *testing.T) (*os.File, int64) {
	return openRealFixture(t, "烘焙知识体系.xmind", realClassicFixtureHash)
}

func openRealFixture(t *testing.T, name, wantHash string) (*os.File, int64) {
	t.Helper()
	fixturePath := filepath.Join("..", "..", "..", "docs", "tests", name)
	file, err := os.Open(fixturePath)
	if err != nil {
		t.Fatalf("open real XMind fixture %q: %v", fixturePath, err)
	}
	t.Cleanup(func() { _ = file.Close() })
	info, err := file.Stat()
	if err != nil {
		t.Fatalf("stat real XMind fixture: %v", err)
	}
	digest := sha256.New()
	if _, err := file.Seek(0, 0); err != nil {
		t.Fatalf("seek real XMind fixture: %v", err)
	}
	if _, err := io.Copy(digest, file); err != nil {
		t.Fatalf("hash real XMind fixture: %v", err)
	}
	if got := hex.EncodeToString(digest.Sum(nil)); got != wantHash {
		t.Fatalf("real XMind fixture SHA-256 = %s, want immutable contract %s", got, wantHash)
	}
	if _, err := file.Seek(0, 0); err != nil {
		t.Fatalf("rewind real XMind fixture: %v", err)
	}
	return file, info.Size()
}

func TestParseRealRichLegacyJSONFixture(t *testing.T) {
	file, size := openRealFixture(t, "classic-rich-v1.xmind", realRichFixtureHash)
	detection, err := DetectPackage(file, size, Limits{})
	if err != nil {
		t.Fatalf("DetectPackage(real rich fixture): %v", err)
	}
	if detection.Family != FormatClassicZenJSON || detection.FixtureRequired || detection.FormatVersion != realRichFixtureVersion {
		t.Fatalf("rich detection = %#v", detection)
	}
	foundV2Evidence := false
	for _, evidence := range detection.Evidence {
		foundV2Evidence = foundV2Evidence || evidence.Entry == "metadata.json" && strings.Contains(evidence.Reason, "dataStructureVersion 2")
	}
	if !foundV2Evidence {
		t.Fatalf("rich detection lacks v2 evidence: %#v", detection.Evidence)
	}
	workbook, err := ParseWorkbook(file, size, Limits{})
	if err != nil {
		t.Fatalf("ParseWorkbook(real rich fixture): %v", err)
	}
	if workbook.SnapshotHash != realRichFixtureHash || workbook.FormatVersion != realRichFixtureVersion || len(workbook.Sheets) != 3 {
		t.Fatalf("rich workbook provenance = %#v", workbook)
	}
	count, maximumDepth := topicStats(workbook)
	if count != 55 || maximumDepth != 12 {
		t.Fatalf("rich topics/max depth = %d/%d, want 55/12", count, maximumDepth)
	}
	rootCount := 0
	for _, sheet := range workbook.Sheets {
		rootCount += len(sheet.Roots)
	}
	if rootCount != 4 {
		t.Fatalf("normalized Sheet roots = %d, want 3 central + 1 floating", rootCount)
	}
	floating := findCandidateTopic(workbook, "23fbcea3-fbe3-41e6-8a7d-19170f108320")
	if floating == nil || floating.Kind != "floating" || floating.ParentID != "" || floating.Depth != 0 || !floating.Position.Floating || floating.Position.X != 491.5 || floating.Position.Y != -177 {
		t.Fatalf("Floating Topic = %#v", floating)
	}
	manual := findCandidateTopic(workbook, "abfef871-3a66-4d6f-8c29-00ff12174d75")
	if manual == nil || manual.RawTitle != "1.1 bake" || manual.Numbering.Enabled || manual.Numbering.DisplayNumber != "" {
		t.Fatalf("manual numbered title = %#v", manual)
	}
	for _, topic := range flattenFixtureTopics(workbook) {
		if topic.Numbering.Enabled || topic.Numbering.DisplayNumber != "" {
			t.Fatalf("fixture without numbering fields produced dynamic numbering: %#v", topic)
		}
	}
	structureSheet := findFixtureSheet(t, workbook, "DG-STRUCTURE")
	if len(structureSheet.Elements) != 2 {
		t.Fatalf("DG-STRUCTURE elements = %#v, want Callout and resolved Summary", structureSheet.Elements)
	}
	summary := findFixtureElement(structureSheet.Elements, "summary")
	if summary == nil || summary.ID != "a92ac19b-e4ec-4a87-8c3b-c6f01e282b78" || summary.DefinitionID != "8e7786a0-914d-4b76-a321-d520415cac43" || summary.Range != "(1,2)" || summary.Status != InventoryIndexed || !reflect.DeepEqual(summary.TargetIDs, []string{"97d773b1-9489-4cea-bae3-5d331a84a4a1", "e86d9fe2-70fb-4f07-8497-7cf1ab23b147"}) {
		t.Fatalf("resolved Summary = %#v", summary)
	}
	callout := findFixtureElement(structureSheet.Elements, "callout")
	if callout == nil || callout.Title != "this is a note for test" || callout.OwnerTopicID != "22c6c570-41fb-47cb-ba38-f6603dc29451" {
		t.Fatalf("Callout = %#v", callout)
	}
	linkTopic := findCandidateTopic(workbook, "8ee7d054-ed41-462d-8b60-e1ef83bf404c")
	if linkTopic == nil || len(linkTopic.Links) != 1 || linkTopic.Links[0].Target != "https://www.google.com" || !linkTopic.Links[0].External {
		t.Fatalf("external Topic Link = %#v", linkTopic)
	}
	relationshipSheet := findFixtureSheet(t, workbook, "DG-LINK-TARGET")
	if len(relationshipSheet.Relationships) != 1 {
		t.Fatalf("Relationships = %#v", relationshipSheet.Relationships)
	}
	relationship := relationshipSheet.Relationships[0]
	if relationship.ID != "fda61fa0-01f8-4435-881b-a47404ac0b06" || relationship.Label != "relation to b" || relationship.SourceTopicID != "4a598108-94b6-40db-b893-c3bf996a2b72" || relationship.TargetTopicID != "07240964-83cb-40b7-9d0f-d448af402d32" || len(relationship.ControlPoints) != 2 || relationship.ControlPoints[0].X != 99.5 || relationship.ControlPoints[1].Y != -0.5 || relationship.StartArrow != "" || relationship.EndArrow != "" {
		t.Fatalf("authored Relationship = %#v", relationship)
	}
	if len(workbook.Assets) != 4 {
		t.Fatalf("deduplicated media assets = %d, want 4", len(workbook.Assets))
	}
	foundWebPAsset := false
	foundMisnamedJPEGAsset := false
	for _, asset := range workbook.Assets {
		foundWebPAsset = foundWebPAsset || strings.HasSuffix(asset.ResourcePath, ".webp") && asset.MediaType == "image/webp"
		foundMisnamedJPEGAsset = foundMisnamedJPEGAsset || strings.HasSuffix(asset.ResourcePath, ".png") && asset.MediaType == "image/jpeg"
	}
	if !foundWebPAsset || !foundMisnamedJPEGAsset {
		t.Fatalf("normalized assets lack content-sniffed MIME: %#v", workbook.Assets)
	}
	assetRefs := 0
	for _, topic := range flattenFixtureTopics(workbook) {
		assetRefs += len(topic.Assets)
	}
	if assetRefs != 5 {
		t.Fatalf("Topic media refs = %d, want 5", assetRefs)
	}
	shared := findFixtureResource(t, workbook.FeatureInventory, "resources/a25c03cbeda09cb034ff9a39a00772306abdc5cfa92573f10ea098d357e93ea4.webp")
	if shared.ReferenceCount != 2 || len(shared.OwningTopicIDs) != 2 || shared.DetectedMIME != "image/webp" {
		t.Fatalf("shared WebP inventory = %#v", shared)
	}
	misnamed := findFixtureResource(t, workbook.FeatureInventory, "resources/11847c2fea90848795b92891d0c4ebdca2afa6aee6b8fdeee7dc08be18a67fc9.png")
	if misnamed.ReferenceCount != 1 || misnamed.DetectedMIME != "image/jpeg" {
		t.Fatalf("content-sniffed misnamed PNG = %#v", misnamed)
	}
	if !hasRawField(structureSheet.RawFields, "labelSortOrder") {
		t.Fatal("labelSortOrder was not preserved as a recognized Sheet source field")
	}
	for _, feature := range workbook.FeatureInventory.Features {
		if feature.Feature == "unknown_json_field" && feature.SourceName == "labelSortOrder" {
			t.Fatalf("recognized labelSortOrder remained unsupported: %#v", feature)
		}
	}
}

func TestDetectRealClassicJSONFixture(t *testing.T) {
	file, size := openRealClassicFixture(t)
	detection, err := DetectPackage(file, size, Limits{})
	if err != nil {
		t.Fatalf("DetectPackage(real fixture): %v", err)
	}
	if detection.Family != FormatClassicZenJSON || detection.FixtureRequired {
		t.Fatalf("detection = family:%q fixture_required:%v", detection.Family, detection.FixtureRequired)
	}
	if detection.FormatVersion != realClassicFixtureVersion {
		t.Fatalf("format version = %q, want %q", detection.FormatVersion, realClassicFixtureVersion)
	}
	if len(detection.CandidateFamilies) != 1 || detection.CandidateFamilies[0] != FormatClassicZenJSON {
		t.Fatalf("candidate families = %#v", detection.CandidateFamilies)
	}
	if !hasFixtureFeature(detection.Inventory, "workbook_payload", "content.json", InventoryIndexed) {
		t.Fatalf("content.json is not the indexed workbook payload: %#v", detection.Inventory.Features)
	}
	if !hasFixtureFeature(detection.Inventory, "inactive_workbook_payload", "content.xml", InventoryPreserved) {
		t.Fatalf("unlisted compatibility XML was not preserved: %#v", detection.Inventory.Features)
	}
}

func TestParseRealClassicJSONFixturePreservesHierarchyAndManualTitles(t *testing.T) {
	file, size := openRealClassicFixture(t)
	workbook, err := ParseWorkbook(file, size, Limits{})
	if err != nil {
		t.Fatalf("ParseWorkbook(real fixture): %v", err)
	}
	if workbook.FormatFamily != FormatClassicZenJSON || workbook.FormatVersion != realClassicFixtureVersion || workbook.SnapshotHash != realClassicFixtureHash {
		t.Fatalf("workbook provenance = %q/%q/%q", workbook.FormatFamily, workbook.FormatVersion, workbook.SnapshotHash)
	}
	if len(workbook.Sheets) != 1 || workbook.Sheets[0].Title != "Map" {
		t.Fatalf("Sheets = %#v", workbook.Sheets)
	}
	count, maximumDepth := topicStats(workbook)
	if count != 84 || maximumDepth != 3 {
		t.Fatalf("topics/max depth = %d/%d, want 84/3", count, maximumDepth)
	}
	root := findCandidateTopic(workbook, "96c5fb06-a056-40e0-a3f4-5ff34d95738a")
	deep := findCandidateTopic(workbook, "e7359f10-a377-4f04-adcc-da9acefd0c9e")
	circled := findCandidateTopic(workbook, "f3e9dfe7-931e-491f-be36-34bbb3fee3eb")
	if root == nil || root.RawTitle != "烘焙知识体系" || root.ParentID != "" || root.Depth != 0 {
		t.Fatalf("root Topic = %#v", root)
	}
	if deep == nil || deep.RawTitle != "1.1.1 面筋形成" || deep.ParentID != "e0c827f0-0b8a-4c50-86cd-713aecd8f500" || deep.Depth != 3 || deep.SiblingOrdinal != 0 || !reflect.DeepEqual(deep.OrderPath, []int{0, 0, 0, 0}) {
		t.Fatalf("deep Topic hierarchy = %#v", deep)
	}
	if circled == nil || circled.RawTitle != "⑩ 冷却" || circled.SiblingOrdinal != 9 || !reflect.DeepEqual(circled.OrderPath, []int{0, 3, 2, 9}) {
		t.Fatalf("circled manual title Topic = %#v", circled)
	}
	for _, topic := range flattenFixtureTopics(workbook) {
		if topic.Numbering.Enabled || topic.Numbering.DisplayNumber != "" {
			t.Fatalf("manual title was interpreted as dynamic numbering: %#v", topic)
		}
	}
	if len(workbook.Assets) != 0 || len(workbook.Sheets[0].Relationships) != 0 || len(workbook.Sheets[0].Elements) != 0 {
		t.Fatalf("fixture-only features were invented: assets=%d relationships=%d elements=%d", len(workbook.Assets), len(workbook.Sheets[0].Relationships), len(workbook.Sheets[0].Elements))
	}
	for _, feature := range workbook.FeatureInventory.Features {
		if feature.Feature == "zone" || feature.Feature == "unknown_json_field" {
			t.Fatalf("empty Zone container or recognized preserved field was misreported: %#v", feature)
		}
	}
	for _, name := range []string{"class", "revisionId", "theme", "arrangeableLayerOrder", "extensions", "zones"} {
		if !hasRawField(workbook.Sheets[0].RawFields, name) {
			t.Fatalf("Sheet field %q was not preserved", name)
		}
	}
	foundThumbnail := false
	for _, resource := range workbook.FeatureInventory.Resources {
		if resource.Path == "Thumbnails/thumbnail.png" {
			foundThumbnail = true
			if resource.Referenced || resource.ReferenceCount != 0 {
				t.Fatalf("workbook thumbnail was treated as Topic media: %#v", resource)
			}
		}
	}
	if !foundThumbnail {
		t.Fatal("workbook thumbnail missing from package resource inventory")
	}
}

func hasFixtureFeature(inventory FeatureInventory, feature, sourceName string, status InventoryStatus) bool {
	for _, item := range inventory.Features {
		if item.Feature == feature && item.SourceName == sourceName && item.Status == status {
			return true
		}
	}
	return false
}

func flattenFixtureTopics(workbook Workbook) []*Topic {
	var topics []*Topic
	for _, sheet := range workbook.Sheets {
		queue := append([]*Topic(nil), sheet.Roots...)
		for len(queue) > 0 {
			topic := queue[0]
			queue = queue[1:]
			topics = append(topics, topic)
			queue = append(queue, topic.Children...)
		}
	}
	return topics
}

func hasRawField(fields []RawField, name string) bool {
	for _, field := range fields {
		if field.Name == name {
			return true
		}
	}
	return false
}

func findFixtureSheet(t *testing.T, workbook Workbook, title string) Sheet {
	t.Helper()
	for _, sheet := range workbook.Sheets {
		if sheet.Title == title {
			return sheet
		}
	}
	t.Fatalf("Sheet %q not found", title)
	return Sheet{}
}

func findFixtureElement(elements []Element, kind string) *Element {
	for index := range elements {
		if elements[index].Kind == kind {
			return &elements[index]
		}
	}
	return nil
}

func findFixtureResource(t *testing.T, inventory FeatureInventory, path string) ResourceEncounter {
	t.Helper()
	for _, resource := range inventory.Resources {
		if resource.Path == path {
			return resource
		}
	}
	t.Fatalf("resource %q not found", path)
	return ResourceEncounter{}
}
