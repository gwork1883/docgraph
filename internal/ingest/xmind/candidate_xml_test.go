package xmind

import (
	"archive/zip"
	"bytes"
	"testing"
)

func TestParseXMind8XMLStructureGatedProduction(t *testing.T) {
	content := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<xmap-content xmlns="urn:xmind:xmap:xmlns:content:2.0" xmlns:xhtml="http://www.w3.org/1999/xhtml" xmlns:xlink="http://www.w3.org/1999/xlink" xmlns:future="urn:docgraph:test:future" version="2.0">
  <future:sheet id="must-not-be-core"><future:topic id="must-not-be-indexed"/></future:sheet>
  <sheet id="sheet-xml-1">
    <topic id="xml-root">
      <title>XML Root</title>
      <notes><plain>legacy note</plain><html><p>rich note</p></html></notes>
	  <labels><label>legacy-label</label><future:label>preserve nested extension</future:label></labels>
	  <marker-refs><marker-ref marker-id="priority-1"/></marker-refs>
	  <numbering number-format="org.xmind.numbering.arabic" prefix="(" suffix=")" prepends-parent-numbers="false"/>
	  <future:img src="xap:resources/must-not-be-indexed.png"/>
	  <xhtml:img xhtml:src="xap:resources/legacy.png" xhtml:width="120" xhtml:height="80"/>
      <boundaries><boundary id="xml-boundary" range="(0,1)"><title>Legacy Scope</title></boundary><boundary range="(2,3)"/></boundaries>
      <summaries><summary id="xml-summary-def" range="(0,1)" topic-id="xml-summary-topic"/><summary id="xml-summary-dangling" topic-id="missing-topic"/></summaries>
      <children>
        <topics type="attached"><topic id="xml-child" xlink:href="xmind:#sheet-xml-2/xml-target"><title>1.manually typed XML</title><future-widget mode="preserve"/></topic></topics>
        <topics type="detached"><topic id="xml-floating"><title>Floating XML</title></topic></topics>
        <topics type="summary"><topic id="xml-summary-topic"><title>Summary XML</title></topic></topics>
        <topics type="callout"><topic id="xml-callout"><title>Callout XML</title></topic></topics>
      </children>
    </topic>
    <title>Legacy Sheet</title>
	  <relationships>
	    <relationship id="xml-rel-1" end1="xml-root" end2="xml-child" start-arrow="none" end-arrow="triangle"><title>Authored XML</title><control-points><control-point angle="0.5" amount="12"/></control-points></relationship>
	    <relationship id="xml-rel-2" end1="xml-root" end2="xml-child"><title>Second XML</title></relationship>
	    <relationship id="xml-rel-invalid" end1="xml-root"><title>Missing endpoint is local</title></relationship>
	    <relationship id="xml-rel-2" end1="xml-root" end2="xml-child"><title>Duplicate ID is local</title></relationship>
	  </relationships>
  </sheet>
  <sheet id="sheet-xml-2"><topic id="xml-target"><title>XML target</title></topic><title>Second</title></sheet>
</xmap-content>`)
	data := detectorZIP(t,
		zipEntry{name: "content.xml", body: content, method: zip.Store},
		zipEntry{name: "resources/legacy.png", body: []byte("contract-only-legacy-image"), method: zip.Store},
	)
	workbook, err := parseCandidateBytes(data, Limits{})
	if err != nil {
		t.Fatalf("ParseCandidateForValidation XML: %v", err)
	}
	if workbook.FormatFamily != FormatXMind8XML || workbook.FormatVersion != "2.0" || len(workbook.Sheets) != 2 {
		t.Fatalf("XML workbook identity = %#v", workbook)
	}
	root := findCandidateTopic(workbook, "xml-root")
	child := findCandidateTopic(workbook, "xml-child")
	if root == nil || root.Numbering.DisplayNumber != "(1)" || root.Notes.Plain != "legacy note" || len(root.Assets) != 1 {
		t.Fatalf("XML root projection = %#v", root)
	}
	if child == nil || child.RawTitle != "1.manually typed XML" || len(child.Links) != 1 || child.Links[0].TargetTopicID != "xml-target" {
		t.Fatalf("XML child/link = %#v", child)
	}
	if len(workbook.Sheets[0].Roots) != 2 || workbook.Sheets[0].Roots[1].Kind != "floating" {
		t.Fatalf("XML floating roots = %#v", workbook.Sheets[0].Roots)
	}
	if len(workbook.Sheets[0].Relationships) != 2 || workbook.Sheets[0].Relationships[0].ID == workbook.Sheets[0].Relationships[1].ID {
		t.Fatalf("XML Relationships collapsed: %#v", workbook.Sheets[0].Relationships)
	}
	foundUnknown := map[string]bool{}
	foundInvalidRelationship := false
	foundInvalidElement := false
	for _, feature := range workbook.FeatureInventory.Features {
		if feature.Feature == "unknown_xml_element" && feature.Status == InventoryUnsupported {
			foundUnknown[feature.SourceName] = true
		}
		if feature.Feature == "relationship" && feature.Status == InventoryUnsupported && (feature.ReasonCode == "invalid_relationship_fields" || feature.ReasonCode == "duplicate_relationship_id") {
			foundInvalidRelationship = true
		}
		if feature.Status == InventoryUnsupported && (feature.ReasonCode == "missing_element_id" || feature.ReasonCode == "missing_element_target") {
			foundInvalidElement = true
		}
	}
	if !foundUnknown["future-widget"] || !foundUnknown["label"] || !foundUnknown["sheet"] || !foundUnknown["img"] {
		t.Fatalf("unknown XML element was not inventoried: %#v", workbook.FeatureInventory.Features)
	}
	if !foundInvalidRelationship {
		t.Fatalf("invalid XML Relationship was not localized: %#v", workbook.FeatureInventory.Features)
	}
	if !foundInvalidElement {
		t.Fatalf("invalid XML special element was not localized: %#v", workbook.FeatureInventory.Features)
	}
	production, productionErr := ParseWorkbook(bytes.NewReader(data), int64(len(data)), Limits{})
	if productionErr != nil {
		t.Fatalf("ParseWorkbook XML: %v", productionErr)
	}
	if production.FormatFamily != FormatXMind8XML || len(production.Sheets) != 2 || findCandidateTopic(production, "must-not-be-indexed") != nil {
		t.Fatalf("production XML projection = %#v", production)
	}
}

func TestParseXMind8XMLLocalizesInvalidMediaAndRelationshipGeometry(t *testing.T) {
	content := []byte(`
<xmap-content xmlns="urn:xmind:xmap:xmlns:content:2.0" xmlns:xhtml="http://www.w3.org/1999/xhtml" version="2.0">
  <sheet id="geometry-sheet">
    <topic id="geometry-root">
      <title>Geometry root</title>
      <children><topics type="attached">
        <topic id="valid-image"><title>Valid image</title><xhtml:img xhtml:src="xap:resources/valid.png" xhtml:width="120.5" xhtml:height="80"/></topic>
        <topic id="invalid-image"><title>Invalid image</title><xhtml:img xhtml:src="xap:resources/invalid.png" xhtml:width="not-a-number" xhtml:height="80"/></topic>
        <topic id="missing-image-size"><title>Missing image size</title><xhtml:img xhtml:src="xap:resources/missing.png" xhtml:width="90"/></topic>
		<topic id="missing-image-source"><title>Missing image source</title><xhtml:img xhtml:width="90" xhtml:height="60"/></topic>
      </topics></children>
    </topic>
    <relationships>
      <relationship id="geometry-relation" end1="geometry-root" end2="valid-image">
        <control-points>
          <control-point angle="0.5" amount="12"/>
          <control-point angle="NaN" amount="13"/>
          <control-point angle="1.5"/>
        </control-points>
      </relationship>
    </relationships>
  </sheet>
</xmap-content>`)
	data := detectorZIP(t,
		zipEntry{name: "content.xml", body: content, method: zip.Store},
		zipEntry{name: "resources/valid.png", body: []byte("valid-image"), method: zip.Store},
		zipEntry{name: "resources/invalid.png", body: []byte("invalid-image"), method: zip.Store},
		zipEntry{name: "resources/missing.png", body: []byte("missing-image"), method: zip.Store},
	)
	workbook, err := ParseWorkbook(bytes.NewReader(data), int64(len(data)), Limits{})
	if err != nil {
		t.Fatalf("ParseWorkbook: %v", err)
	}
	validImage := findCandidateTopic(workbook, "valid-image")
	invalidImage := findCandidateTopic(workbook, "invalid-image")
	missingImageSize := findCandidateTopic(workbook, "missing-image-size")
	missingImageSource := findCandidateTopic(workbook, "missing-image-source")
	if validImage == nil || len(validImage.Assets) != 1 || len(workbook.Assets) != 3 {
		t.Fatalf("valid image projection = topic:%#v assets:%#v", validImage, workbook.Assets)
	}
	if invalidImage == nil || len(invalidImage.Assets) != 1 || missingImageSize == nil || len(missingImageSize.Assets) != 1 {
		t.Fatalf("images with unknown geometry were discarded: invalid=%#v missing=%#v", invalidImage, missingImageSize)
	}
	if missingImageSource == nil || len(missingImageSource.Assets) != 0 || len(missingImageSource.RawFields) == 0 {
		t.Fatalf("image without a source was not preserved as unsupported: %#v", missingImageSource)
	}
	if invalidImage.Assets[0].Width != 0 || invalidImage.Assets[0].Height != 80 || missingImageSize.Assets[0].Width != 90 || missingImageSize.Assets[0].Height != 0 {
		t.Fatalf("unknown image dimensions were not isolated: invalid=%#v missing=%#v", invalidImage.Assets[0], missingImageSize.Assets[0])
	}
	if len(invalidImage.RawFields) == 0 || len(missingImageSize.RawFields) == 0 {
		t.Fatalf("invalid image XML was not preserved: invalid=%#v missing=%#v", invalidImage.RawFields, missingImageSize.RawFields)
	}
	if len(workbook.Sheets[0].Relationships) != 1 || len(workbook.Sheets[0].Relationships[0].ControlPoints) != 1 {
		t.Fatalf("Relationship geometry = %#v", workbook.Sheets[0].Relationships)
	}
	relationship := workbook.Sheets[0].Relationships[0]
	if len(relationship.RawFields) < 2 {
		t.Fatalf("invalid control-point XML was not preserved: %#v", relationship.RawFields)
	}
	point := relationship.ControlPoints[0]
	if point.Angle != 0.5 || point.Amount != 12 {
		t.Fatalf("valid control point = %#v", point)
	}
	warnings := map[string]bool{}
	for _, feature := range workbook.FeatureInventory.Features {
		if feature.Status == InventoryUnsupported {
			warnings[feature.ReasonCode] = true
		}
	}
	for _, reason := range []string{"missing_image_source", "invalid_image_dimensions", "missing_image_dimensions", "invalid_control_point_number", "missing_control_point_number"} {
		if !warnings[reason] {
			t.Fatalf("missing warning %q: %#v", reason, workbook.FeatureInventory.Features)
		}
	}
}

func TestParseXMLCandidateRejectsMalformedPayload(t *testing.T) {
	data := detectorZIP(t, zipEntry{name: "content.xml", body: []byte(`<xmap-content><sheet></xmap-content>`), method: zip.Store})
	workbook, err := parseCandidateBytes(data, Limits{})
	requireCode(t, err, ErrorInvalidPayload)
	if len(workbook.Sheets) != 0 {
		t.Fatalf("returned partial Workbook: %#v", workbook)
	}
}

func TestParseXMind8XMLRejectsNonXMapRoot(t *testing.T) {
	content := `<workbook xmlns="urn:xmind:xmap:xmlns:content:2.0" version="2.0"/>`
	data := detectorZIP(t, zipEntry{name: "content.xml", body: []byte(content), method: zip.Store})
	workbook, err := ParseWorkbook(bytes.NewReader(data), int64(len(data)), Limits{})
	requireCode(t, err, ErrorUnsupportedFormat)
	if workbook.FormatFamily != "" || len(workbook.Sheets) != 0 {
		t.Fatalf("returned partial Workbook: %#v", workbook)
	}
}

func TestParseXMind8XMLTreatsNamespaceAndVersionAsDiagnostics(t *testing.T) {
	tests := []struct {
		name        string
		attributes  string
		wantFeature string
	}{
		{name: "missing namespace", attributes: `version="2.0"`, wantFeature: "xml_content_namespace"},
		{name: "unverified namespace", attributes: `xmlns="urn:example:not-xmind" version="2.0"`, wantFeature: "xml_content_namespace"},
		{name: "missing version", attributes: `xmlns="urn:xmind:xmap:xmlns:content:2.0"`, wantFeature: "xml_content_version"},
		{name: "unverified version", attributes: `xmlns="urn:xmind:xmap:xmlns:content:2.0" version="3.0"`, wantFeature: "xml_content_version"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := `<xmap-content ` + test.attributes + `><sheet id="s"><topic id="t"/></sheet></xmap-content>`
			data := detectorZIP(t, zipEntry{name: "content.xml", body: []byte(content), method: zip.Store})
			workbook, err := ParseWorkbook(bytes.NewReader(data), int64(len(data)), Limits{})
			if err != nil {
				t.Fatalf("ParseWorkbook: %v", err)
			}
			if len(workbook.Sheets) != 1 || workbook.Sheets[0].ID != "s" {
				t.Fatalf("workbook = %#v", workbook)
			}
			found := false
			for _, feature := range workbook.FeatureInventory.Features {
				if feature.Feature == test.wantFeature && feature.Status == InventoryUnsupported && feature.ReasonCode != "" {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing diagnostic %q: %#v", test.wantFeature, workbook.FeatureInventory.Features)
			}
		})
	}
}

func TestParseXMind8XMLNumberingRejectsUnprovenPrependingSemantics(t *testing.T) {
	tests := []struct {
		name       string
		attributes string
		wantValue  string
	}{
		{name: "true is not yet fixture proven", attributes: `prepends-parent-numbers="true"`, wantValue: "true"},
		{name: "unknown restart token", attributes: `prepending-numbers="restart-each-level"`, wantValue: "restart-each-level"},
		{name: "explicit empty is not omitted", attributes: `prepending-numbers=""`, wantValue: ""},
		{name: "conflicting aliases", attributes: `prepending-numbers="none" prepends-parent-numbers="false"`, wantValue: "none"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := `<xmap-content xmlns="urn:xmind:xmap:xmlns:content:2.0" version="2.0"><sheet id="sheet"><topic id="root"><title>Root</title><numbering number-format="org.xmind.numbering.arabic" prefix="[" suffix="]" ` + test.attributes + `/></topic></sheet></xmap-content>`
			data := detectorZIP(t, zipEntry{name: "content.xml", body: []byte(content), method: zip.Store})
			workbook, err := parseCandidateBytes(data, Limits{})
			if err != nil {
				t.Fatalf("ParseCandidateForValidation: %v", err)
			}
			topic := findCandidateTopic(workbook, "root")
			if topic == nil || !topic.Numbering.Enabled || topic.Numbering.PrependingNumbers != test.wantValue || topic.Numbering.DisplayNumber != "" {
				t.Fatalf("unproven XML prepending semantics generated display text: %#v", topic)
			}
			if !hasNumberingDiagnostic(workbook, "root", "numbering_semantics_not_implemented") {
				t.Fatalf("unproven XML prepending semantics lacks inventory diagnostic: %#v", workbook.FeatureInventory.Features)
			}
		})
	}
}

func TestParseXMind8XMLRejectsInvalidCoreStructure(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "missing Sheet id", content: `<sheet><topic id="root"/></sheet>`},
		{name: "missing root Topic", content: `<sheet id="sheet"/>`},
		{name: "multiple root Topics", content: `<sheet id="sheet"><topic id="one"/><topic id="two"/></sheet>`},
		{name: "missing Topic id", content: `<sheet id="sheet"><topic/></sheet>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := `<xmap-content xmlns="urn:xmind:xmap:xmlns:content:2.0" version="2.0">` + test.content + `</xmap-content>`
			data := detectorZIP(t, zipEntry{name: "content.xml", body: []byte(payload), method: zip.Store})
			workbook, err := ParseWorkbook(bytes.NewReader(data), int64(len(data)), Limits{})
			requireCode(t, err, ErrorInvalidPayload)
			if workbook.FormatFamily != "" || len(workbook.Sheets) != 0 {
				t.Fatalf("returned partial Workbook: %#v", workbook)
			}
		})
	}
}
