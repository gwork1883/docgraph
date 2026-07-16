package xmind

import "testing"

func TestParseJSONInvalidOptionalTopicFeaturesPreserveCoreTree(t *testing.T) {
	content := []any{map[string]any{
		"id": "sheet", "legend": 42, "rootTopic": map[string]any{
			"id": "root", "title": "Root",
			"notes": 42, "labels": map[string]any{"unexpected": true}, "markers": "bad",
			"numbering": []any{"bad"}, "position": "bad", "href": 99,
			"image": map[string]any{"src": 99}, "boundaries": map[string]any{"bad": true},
			"summaries": []any{map[string]any{"id": "summary-definition", "topicId": "missing-summary-topic", "range": "(0,0)"}},
			"children":  map[string]any{"attached": []any{map[string]any{"id": "child", "title": "Child"}}},
		},
	}}
	data := candidateJSONPackage(t, content)
	workbook, err := parseCandidateBytes(data, Limits{})
	if err != nil {
		t.Fatalf("ParseCandidateForValidation: %v", err)
	}
	root := findCandidateTopic(workbook, "root")
	if root == nil || len(root.Children) != 1 || root.Children[0].ID != "child" {
		t.Fatalf("core Topic tree was lost: %#v", workbook.Sheets)
	}
	if len(root.Assets) != 0 || len(root.Links) != 0 || root.Numbering.DisplayNumber != "" {
		t.Fatalf("invalid optional data was invented: %#v", root)
	}
	if len(workbook.Sheets[0].Elements) != 1 || workbook.Sheets[0].Elements[0].ID != "summary-definition" || workbook.Sheets[0].Elements[0].Status != InventoryPreserved {
		t.Fatalf("unresolved Summary definition was not preserved: %#v", workbook.Sheets[0].Elements)
	}

	reasons := map[string]bool{}
	for _, feature := range workbook.FeatureInventory.Features {
		if feature.Status == InventoryUnsupported {
			reasons[feature.ReasonCode] = true
		}
	}
	if !reasons["invalid_optional_feature"] || !reasons["missing_summary_topic"] || !reasons["invalid_legend"] {
		t.Fatalf("optional feature warnings are incomplete: %#v", workbook.FeatureInventory.Features)
	}
}
