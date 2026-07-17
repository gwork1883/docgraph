package xmind

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func candidateJSONPackage(t *testing.T, content any, resources ...zipEntry) []byte {
	t.Helper()
	payload, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("marshal contract-only content.json: %v", err)
	}
	entries := []zipEntry{{name: "content.json", body: payload, method: zip.Store}}
	entries = append(entries, resources...)
	return detectorZIP(t, entries...)
}

func parseCandidateBytes(data []byte, limits Limits) (Workbook, error) {
	return ParseCandidateForValidation(bytes.NewReader(data), int64(len(data)), limits)
}

func TestParseJSONCandidateRichLargeContract(t *testing.T) {
	deep := map[string]any{"id": "deep-0", "title": "Deep 0"}
	current := deep
	for depth := 1; depth <= 11; depth++ {
		child := map[string]any{"id": fmt.Sprintf("deep-%d", depth), "title": fmt.Sprintf("Deep %d", depth)}
		current["children"] = map[string]any{"attached": []any{child}}
		current = child
	}
	attached := []any{
		map[string]any{
			"id": "numbered", "title": "Generated number", "numbering": map[string]any{"numberFormat": "org.xmind.numbering.arabic", "prefix": "[", "suffix": "]", "prependingNumbers": "none"},
			"notes": map[string]any{"plain": map[string]any{"content": "source note"}}, "labels": "architecture", "markers": []any{map[string]any{"markerId": "priority-1"}},
			"href": "xmind:#sheet-2/target", "image": map[string]any{"src": "xap:resources/picture.png", "width": 320, "height": 200},
			"boundaries": []any{map[string]any{"id": "boundary-1", "title": "Scope", "range": "(0,1)"}},
			"summaries":  []any{map[string]any{"id": "summary-def-1", "topicId": "summary-topic", "range": "(0,1)"}},
			"task":       map[string]any{"status": "done", "owner": "owner", "progress": 1}, "futureThing": map[string]any{"preserve": true},
		},
		map[string]any{"id": "manual", "title": "1.manually typed title"},
		deep,
		map[string]any{"id": "external-media", "title": "External media", "image": map[string]any{"src": "https://example.invalid/evidence.png"}},
		map[string]any{"id": "missing-media", "title": "Missing media", "image": map[string]any{"src": "xap:resources/missing.png"}},
	}
	for index := 0; index < 500; index++ {
		attached = append(attached, map[string]any{"id": fmt.Sprintf("bulk-%03d", index), "title": fmt.Sprintf("Bulk %03d", index)})
	}
	root := map[string]any{
		"id": "root-1", "title": "Root",
		"children": map[string]any{
			"attached": attached,
			"detached": []any{map[string]any{"id": "floating-1", "title": "Floating", "position": map[string]any{"x": 10, "y": 20}}},
			"summary":  []any{map[string]any{"id": "summary-topic", "title": "Summary topic"}},
			"callout":  []any{map[string]any{"id": "callout-1", "title": "Callout"}},
		},
	}
	sheets := []any{
		map[string]any{
			"id": "sheet-1", "title": "Sheet One", "rootTopic": root,
			"legend": map[string]any{"markers": map[string]any{"priority-1": map[string]any{"name": "Priority One"}}},
			"relationships": []any{
				map[string]any{"id": "rel-1", "title": "first", "end1Id": "numbered", "end2Id": "manual", "controlPoints": map[string]any{"0": map[string]any{"angle": 0.5, "amount": 10}, "1": map[string]any{"angle": 1.5, "amount": 20}}, "style": map[string]any{"properties": map[string]any{"line-start-arrow-shape": "none", "line-end-arrow-shape": "triangle"}}},
				map[string]any{"id": "rel-2", "title": "second", "end1Id": "numbered", "end2Id": "manual", "controlPoints": map[string]any{"0": map[string]any{"angle": 2, "amount": 30}, "1": map[string]any{"angle": 3, "amount": 40}}},
			},
		},
		map[string]any{"id": "sheet-2", "title": "Sheet Two", "rootTopic": map[string]any{"id": "target", "title": "Cross-sheet target"}},
	}
	data := candidateJSONPackage(t, sheets, zipEntry{name: "resources/picture.png", body: []byte("contract-only-image-bytes"), method: zip.Store})

	workbook, err := parseCandidateBytes(data, Limits{})
	if err != nil {
		t.Fatalf("ParseCandidateForValidation: %v", err)
	}
	if workbook.FormatFamily != FormatClassicZenJSON {
		t.Fatalf("family = %q", workbook.FormatFamily)
	}
	count, maximumDepth := topicStats(workbook)
	if count < 500 || maximumDepth < 10 {
		t.Fatalf("topics/depth = %d/%d, want 500+/10+", count, maximumDepth)
	}
	numbered := findCandidateTopic(workbook, "numbered")
	manual := findCandidateTopic(workbook, "manual")
	if numbered == nil || numbered.Numbering.DisplayNumber != "[1]" {
		t.Fatalf("numbered Topic = %#v", numbered)
	}
	if manual == nil || manual.RawTitle != "1.manually typed title" || manual.Numbering.DisplayNumber != "" {
		t.Fatalf("manual title was rewritten or numbered: %#v", manual)
	}
	if len(numbered.Links) != 1 || numbered.Links[0].TargetSheetID != "sheet-2" || numbered.Links[0].TargetTopicID != "target" {
		t.Fatalf("cross-Sheet link = %#v", numbered.Links)
	}
	if len(numbered.Markers) != 1 || numbered.Markers[0].ResolvedName != "Priority One" {
		t.Fatalf("marker resolution = %#v", numbered.Markers)
	}
	if len(workbook.Sheets[0].Relationships) != 2 || workbook.Sheets[0].Relationships[0].ID == workbook.Sheets[0].Relationships[1].ID {
		t.Fatalf("same-endpoint Relationships collapsed: %#v", workbook.Sheets[0].Relationships)
	}
	if len(workbook.Sheets[0].Roots) != 2 || workbook.Sheets[0].Roots[1].Kind != "floating" {
		t.Fatalf("floating roots = %#v", workbook.Sheets[0].Roots)
	}
	if len(workbook.Assets) != 3 {
		t.Fatalf("assets = %#v", workbook.Assets)
	}
	if workbook.SnapshotHash == "" || workbook.SemanticHash == "" || workbook.MediaManifestHash == "" {
		t.Fatalf("missing deterministic hashes: %#v", workbook)
	}
	second, err := parseCandidateBytes(data, Limits{})
	if err != nil {
		t.Fatalf("second parse: %v", err)
	}
	if workbook.SnapshotHash != second.SnapshotHash || workbook.SemanticHash != second.SemanticHash || workbook.MediaManifestHash != second.MediaManifestHash {
		t.Fatalf("hashes are not deterministic: first=%s/%s/%s second=%s/%s/%s", workbook.SnapshotHash, workbook.SemanticHash, workbook.MediaManifestHash, second.SnapshotHash, second.SemanticHash, second.MediaManifestHash)
	}
	foundUnknown := false
	foundExternal := false
	foundMissing := false
	for _, feature := range workbook.FeatureInventory.Features {
		foundUnknown = foundUnknown || feature.Feature == "unknown_json_field" && feature.SourceName == "futureThing" && feature.Status == InventoryUnsupported
		foundExternal = foundExternal || feature.Feature == "external_media" && feature.ReasonCode == "external_reference_not_fetched"
		foundMissing = foundMissing || feature.Feature == "media" && feature.Status == InventoryRejected && feature.ReasonCode == "missing_package_resource"
	}
	if !foundUnknown || !foundExternal || !foundMissing {
		t.Fatalf("inventory missing unknown/external/missing evidence: %#v", workbook.FeatureInventory.Features)
	}

	productionWorkbook, productionErr := ParseWorkbook(bytes.NewReader(data), int64(len(data)), Limits{})
	if productionErr != nil || productionWorkbook.FormatFamily != FormatClassicZenJSON || productionWorkbook.SemanticHash != workbook.SemanticHash {
		t.Fatalf("production parser did not admit unique legacy Sheet-array JSON: workbook=%#v err=%v", productionWorkbook, productionErr)
	}
}

func TestParseJSONCandidateRejectsInvalidBeforeWorkbook(t *testing.T) {
	t.Run("malformed JSON", func(t *testing.T) {
		data := detectorZIP(t, zipEntry{name: "content.json", body: []byte(`[{`), method: zip.Store})
		workbook, err := parseCandidateBytes(data, Limits{})
		requireCode(t, err, ErrorInvalidPayload)
		if len(workbook.Sheets) != 0 {
			t.Fatalf("returned partial Workbook: %#v", workbook)
		}
	})
	t.Run("dangling Relationship is skipped with inventory diagnostic", func(t *testing.T) {
		content := []any{
			map[string]any{
				"id":        "sheet",
				"rootTopic": map[string]any{"id": "root"},
				"relationships": []any{
					map[string]any{
						"id": "rel", "end1Id": "root", "end2Id": "missing",
						"controlPoints": map[string]any{"0": map[string]any{}, "1": map[string]any{}},
					},
				},
			},
		}
		data := candidateJSONPackage(t, content)
		workbook, err := parseCandidateBytes(data, Limits{})
		if err != nil {
			t.Fatalf("ParseCandidateForValidation: %v", err)
		}
		if len(workbook.Sheets) != 1 || len(workbook.Sheets[0].Relationships) != 0 {
			t.Fatalf("dangling Relationship was retained: %#v", workbook.Sheets)
		}
		foundDiagnostic := false
		for _, feature := range workbook.FeatureInventory.Features {
			foundDiagnostic = foundDiagnostic || feature.Feature == "relationship" && feature.SourceElementID == "rel" && feature.Status == InventoryUnsupported && feature.ReasonCode == "missing_relationship_endpoint" && strings.Contains(feature.Detail, `endpoint "root" -> "missing" is unresolved`)
		}
		if !foundDiagnostic {
			t.Fatalf("dangling Relationship lacks actionable inventory diagnostic: %#v", workbook.FeatureInventory.Features)
		}
	})
	t.Run("element limit", func(t *testing.T) {
		content := []any{
			map[string]any{
				"id": "sheet",
				"rootTopic": map[string]any{
					"id":       "root",
					"children": map[string]any{"attached": []any{map[string]any{"id": "child"}}},
				},
			},
		}
		data := candidateJSONPackage(t, content)
		_, err := parseCandidateBytes(data, Limits{MaxNormalizedElements: 2})
		requireCode(t, err, ErrorArchiveLimit)
	})
	t.Run("per-element rich text limit", func(t *testing.T) {
		content := []any{map[string]any{
			"id": "sheet", "rootTopic": map[string]any{"id": "root", "title": "oversized title"},
		}}
		data := candidateJSONPackage(t, content)
		workbook, err := parseCandidateBytes(data, Limits{MaxRichTextBytes: 8})
		requireCode(t, err, ErrorArchiveLimit)
		if len(workbook.Sheets) != 0 {
			t.Fatalf("returned partial Workbook: %#v", workbook)
		}
	})
	t.Run("V26-shaped object is not guessed", func(t *testing.T) {
		data := candidateJSONPackage(t, map[string]any{"version": "26.02", "zones": []any{}})
		_, err := parseCandidateBytes(data, Limits{})
		requireCode(t, err, ErrorInvalidPayload)
		for _, contract := range AdapterContracts() {
			if contract.Family == FormatV2602Plus && contract.Status != AdapterFixtureRequired {
				t.Fatalf("V26 contract overclaimed: %#v", contract)
			}
		}
	})
}

func TestParseJSONCandidatePreservesUnknownNumberingWithoutInventingDecimalText(t *testing.T) {
	content := []any{map[string]any{
		"id": "sheet", "rootTopic": map[string]any{
			"id": "root", "title": "Bread steps", "children": map[string]any{"attached": []any{
				map[string]any{"id": "unknown-number", "title": "Proof dough", "numbering": map[string]any{
					"numberFormat": "org.xmind.numbering.future-format", "prefix": "[", "suffix": "]",
				}},
			}},
		},
	}}
	data := candidateJSONPackage(t, content)
	workbook, err := parseCandidateBytes(data, Limits{})
	if err != nil {
		t.Fatalf("ParseCandidateForValidation: %v", err)
	}
	topic := findCandidateTopic(workbook, "unknown-number")
	if topic == nil || topic.Numbering.Format != "org.xmind.numbering.future-format" || !topic.Numbering.Enabled || topic.Numbering.DisplayNumber != "" {
		t.Fatalf("unknown numbering was not preserved fail-closed: %#v", topic)
	}
	foundUnsupported := false
	for _, feature := range workbook.FeatureInventory.Features {
		foundUnsupported = foundUnsupported || feature.Feature == "numbering" && feature.SourceElementID == "unknown-number" && feature.Status == InventoryUnsupported && feature.ReasonCode == "numbering_semantics_not_implemented"
	}
	if !foundUnsupported {
		t.Fatalf("unknown numbering lacks unsupported inventory: %#v", workbook.FeatureInventory.Features)
	}
}

func TestParseJSONNumberingRejectsUnprovenAdditionalSemanticsAndInvalidTypes(t *testing.T) {
	t.Run("unknown prepending semantics", func(t *testing.T) {
		content := []any{map[string]any{
			"id": "sheet", "rootTopic": map[string]any{
				"id": "root", "title": "Root", "numbering": map[string]any{
					"numberFormat": "org.xmind.numbering.arabic", "prefix": "[", "suffix": "]", "prependingNumbers": "restart-each-level",
				},
			},
		}}
		workbook, err := parseCandidateBytes(candidateJSONPackage(t, content), Limits{})
		if err != nil {
			t.Fatalf("ParseCandidateForValidation: %v", err)
		}
		topic := findCandidateTopic(workbook, "root")
		if topic == nil || !topic.Numbering.Enabled || topic.Numbering.PrependingNumbers != "restart-each-level" || topic.Numbering.DisplayNumber != "" {
			t.Fatalf("unknown prepending semantics generated display text: %#v", topic)
		}
		if !hasNumberingDiagnostic(workbook, "root", "numbering_semantics_not_implemented") {
			t.Fatalf("unknown prepending semantics lacks inventory diagnostic: %#v", workbook.FeatureInventory.Features)
		}
	})
	t.Run("explicit empty prepending is not treated as omitted", func(t *testing.T) {
		content := []any{map[string]any{
			"id": "sheet", "rootTopic": map[string]any{"id": "root", "numbering": map[string]any{
				"numberFormat": "org.xmind.numbering.arabic", "prependingNumbers": "",
			}},
		}}
		workbook, err := parseCandidateBytes(candidateJSONPackage(t, content), Limits{})
		if err != nil {
			t.Fatalf("ParseCandidateForValidation: %v", err)
		}
		topic := findCandidateTopic(workbook, "root")
		if topic == nil || topic.Numbering.DisplayNumber != "" || !hasNumberingDiagnostic(workbook, "root", "numbering_semantics_not_implemented") {
			t.Fatalf("explicit empty prepending was interpreted: topic=%#v inventory=%#v", topic, workbook.FeatureInventory.Features)
		}
	})

	invalidTypes := []struct {
		name  string
		field string
		value any
	}{
		{name: "numeric prefix", field: "prefix", value: 42},
		{name: "boolean suffix", field: "suffix", value: true},
		{name: "boolean prepending", field: "prependingNumbers", value: false},
		{name: "array prepending", field: "prependingNumbers", value: []any{"none"}},
	}
	for _, test := range invalidTypes {
		t.Run(test.name, func(t *testing.T) {
			numbering := map[string]any{"numberFormat": "org.xmind.numbering.arabic", "prefix": "[", "suffix": "]", "prependingNumbers": "none"}
			numbering[test.field] = test.value
			content := []any{map[string]any{
				"id": "sheet", "rootTopic": map[string]any{"id": "root", "title": "Root", "numbering": numbering},
			}}
			workbook, err := parseCandidateBytes(candidateJSONPackage(t, content), Limits{})
			if err != nil {
				t.Fatalf("ParseCandidateForValidation: %v", err)
			}
			topic := findCandidateTopic(workbook, "root")
			if topic == nil || topic.Numbering.DisplayNumber != "" {
				t.Fatalf("invalid %s type generated display text: %#v", test.field, topic)
			}
			foundRaw := false
			for _, field := range topic.RawFields {
				foundRaw = foundRaw || field.Name == "numbering" && len(field.JSON) > 0
			}
			if !foundRaw || !hasNumberingDiagnostic(workbook, "root", "invalid_optional_feature") {
				t.Fatalf("invalid %s type was not preserved/diagnosed: topic=%#v inventory=%#v", test.field, topic, workbook.FeatureInventory.Features)
			}
		})
	}
}

func hasNumberingDiagnostic(workbook Workbook, topicID, reason string) bool {
	for _, feature := range workbook.FeatureInventory.Features {
		if feature.Feature == "numbering" && feature.SourceElementID == topicID && feature.Status == InventoryUnsupported && feature.ReasonCode == reason {
			return true
		}
	}
	return false
}

func TestParseJSONCandidateRejectsDuplicateTopicIDAcrossSheetsWithActionableDiagnostic(t *testing.T) {
	data := candidateJSONPackage(t, []any{
		map[string]any{"id": "sheet-a", "rootTopic": map[string]any{"id": "duplicate-topic", "title": "A"}},
		map[string]any{"id": "sheet-b", "rootTopic": map[string]any{"id": "duplicate-topic", "title": "B"}},
	})
	_, err := parseCandidateBytes(data, Limits{})
	packageErr := requireCode(t, err, ErrorValidation)
	if !strings.Contains(packageErr.Detail, "Sheets \"sheet-a\" and \"sheet-b\"") || !strings.Contains(packageErr.Detail, "resave") {
		t.Fatalf("duplicate Topic diagnostic = %q", packageErr.Detail)
	}
}

func topicStats(workbook Workbook) (count int, maximumDepth int) {
	for sheetIndex := range workbook.Sheets {
		queue := append([]*Topic(nil), workbook.Sheets[sheetIndex].Roots...)
		for len(queue) > 0 {
			topic := queue[0]
			queue = queue[1:]
			count++
			if topic.Depth > maximumDepth {
				maximumDepth = topic.Depth
			}
			queue = append(queue, topic.Children...)
		}
	}
	return count, maximumDepth
}

func findCandidateTopic(workbook Workbook, id string) *Topic {
	for sheetIndex := range workbook.Sheets {
		queue := append([]*Topic(nil), workbook.Sheets[sheetIndex].Roots...)
		for len(queue) > 0 {
			topic := queue[0]
			queue = queue[1:]
			if topic.ID == id {
				return topic
			}
			queue = append(queue, topic.Children...)
		}
	}
	return nil
}
