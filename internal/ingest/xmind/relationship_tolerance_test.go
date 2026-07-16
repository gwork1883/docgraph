package xmind

import "testing"

func TestParseJSONRelationshipLocalFailuresDoNotAbortWorkbook(t *testing.T) {
	content := []any{map[string]any{
		"id": "sheet", "rootTopic": map[string]any{
			"id": "root", "children": map[string]any{"attached": []any{map[string]any{"id": "child"}}},
		},
		"relationships": []any{
			map[string]any{"id": "no-geometry", "end1Id": "root", "end2Id": "child"},
			map[string]any{
				"id": "bad-geometry", "end1Id": "root", "end2Id": "child",
				"controlPoints": map[string]any{"0": map[string]any{"angle": "not-a-number"}},
			},
			map[string]any{"id": "bad-style", "end1Id": "root", "end2Id": "child", "style": []any{"unexpected"}},
			map[string]any{"end1Id": "root", "end2Id": "child"},
		},
	}}
	data := candidateJSONPackage(t, content)
	workbook, err := parseCandidateBytes(data, Limits{})
	if err != nil {
		t.Fatalf("ParseCandidateForValidation: %v", err)
	}
	if len(workbook.Sheets) != 1 || len(workbook.Sheets[0].Relationships) != 3 {
		t.Fatalf("valid-endpoint relationships were lost: %#v", workbook.Sheets)
	}
	if len(workbook.Sheets[0].Relationships[1].ControlPoints) != 0 {
		t.Fatalf("invalid geometry was silently converted to numeric zero: %#v", workbook.Sheets[0].Relationships[1].ControlPoints)
	}

	foundBadNumber := false
	foundBadStyle := false
	foundRejectedRelationship := false
	for _, feature := range workbook.FeatureInventory.Features {
		foundBadNumber = foundBadNumber || feature.Feature == "relationship_geometry" && feature.SourceElementID == "bad-geometry" && feature.Status == InventoryUnsupported && feature.ReasonCode == "invalid_control_point_number"
		foundBadStyle = foundBadStyle || feature.Feature == "relationship_style" && feature.SourceElementID == "bad-style" && feature.Status == InventoryUnsupported && feature.ReasonCode == "invalid_relationship_style"
		foundRejectedRelationship = foundRejectedRelationship || feature.Feature == "relationship" && feature.Status == InventoryRejected && feature.ReasonCode == "invalid_relationship"
	}
	if !foundBadNumber || !foundBadStyle || !foundRejectedRelationship {
		t.Fatalf("local Relationship diagnostics are incomplete: %#v", workbook.FeatureInventory.Features)
	}
}

func TestParseJSONInvalidRelationshipCollectionDoesNotAbortTopics(t *testing.T) {
	content := []any{map[string]any{
		"id": "sheet", "rootTopic": map[string]any{"id": "root", "title": "still indexed"},
		"relationships": map[string]any{"unexpected": true},
	}}
	data := candidateJSONPackage(t, content)
	workbook, err := parseCandidateBytes(data, Limits{})
	if err != nil {
		t.Fatalf("ParseCandidateForValidation: %v", err)
	}
	if findCandidateTopic(workbook, "root") == nil || len(workbook.Sheets[0].Relationships) != 0 {
		t.Fatalf("Topic tree was not preserved: %#v", workbook)
	}
	found := false
	for _, feature := range workbook.FeatureInventory.Features {
		found = found || feature.Feature == "relationships" && feature.Status == InventoryRejected && feature.ReasonCode == "invalid_relationship_collection"
	}
	if !found {
		t.Fatalf("invalid relationship collection lacks inventory diagnostic: %#v", workbook.FeatureInventory.Features)
	}
}
