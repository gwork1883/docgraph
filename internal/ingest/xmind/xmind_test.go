package xmind

import (
	"archive/zip"
	"bytes"
	"errors"
	"strings"
	"testing"
)

type zipEntry struct {
	name   string
	body   []byte
	method uint16
	flags  uint16
}

// detectorZIP creates contract-only archives. It does not produce real XMind
// fixtures and must never be used as format compatibility evidence.
func detectorZIP(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: entry.method, Flags: entry.flags}
		part, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatalf("create ZIP entry %q: %v", entry.name, err)
		}
		if _, err := part.Write(entry.body); err != nil {
			t.Fatalf("write ZIP entry %q: %v", entry.name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close ZIP: %v", err)
	}
	return buffer.Bytes()
}

func detectBytes(data []byte, limits Limits) (PackageDetection, error) {
	return DetectPackage(bytes.NewReader(data), int64(len(data)), limits)
}

func requireCode(t *testing.T, err error, code ErrorCode) *PackageError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error code %q", code)
	}
	var packageErr *PackageError
	if !errors.As(err, &packageErr) {
		t.Fatalf("expected *PackageError, got %T: %v", err, err)
	}
	if packageErr.Code != code {
		t.Fatalf("error code = %q, want %q (%v)", packageErr.Code, code, err)
	}
	return packageErr
}

func TestDetectPackageXMLCandidateContract(t *testing.T) {
	data := detectorZIP(t,
		zipEntry{name: "content.xml", body: []byte("<xmap-content/>")},
		zipEntry{name: "resources/picture.png", body: []byte("not-a-real-png")},
	)
	detection, err := detectBytes(data, Limits{})
	if err != nil {
		t.Fatalf("DetectPackage: %v", err)
	}
	if detection.Family != FormatXMind8XML {
		t.Fatalf("family = %q, want %q", detection.Family, FormatXMind8XML)
	}
	if !detection.FixtureRequired {
		t.Fatal("synthetic detector ZIP must remain fixture-required")
	}
	if len(detection.Entries) != 2 || len(detection.Inventory.Resources) != 2 {
		t.Fatalf("entries/resources = %d/%d, want 2/2", len(detection.Entries), len(detection.Inventory.Resources))
	}
	for _, entry := range detection.Entries {
		if !entry.Directory && entry.SHA256 == "" {
			t.Fatalf("entry %q has no digest", entry.Path)
		}
	}
	if err := detection.Inventory.Validate(); err != nil {
		t.Fatalf("inventory invalid: %v", err)
	}
}

func TestDetectPackageAdmitsUniqueLegacyJSONWithoutManifest(t *testing.T) {
	data := detectorZIP(t, zipEntry{name: "content.json", body: []byte(`[{"id":"sheet","rootTopic":{"id":"root"}}]`)})
	detection, err := detectBytes(data, Limits{})
	if err != nil {
		t.Fatalf("DetectPackage: %v", err)
	}
	if detection.Family != FormatClassicZenJSON || detection.FixtureRequired {
		t.Fatalf("family/fixture required = %q/%v, want structure-admitted %q/false", detection.Family, detection.FixtureRequired, FormatClassicZenJSON)
	}
	if len(detection.CandidateFamilies) != 1 || detection.CandidateFamilies[0] != FormatClassicZenJSON {
		t.Fatalf("candidate families = %#v", detection.CandidateFamilies)
	}
	workbook, err := ParseWorkbook(bytes.NewReader(data), int64(len(data)), Limits{})
	if err != nil || len(workbook.Sheets) != 1 {
		t.Fatalf("ParseWorkbook(unique legacy JSON) = %#v, %v", workbook, err)
	}
}

func TestDetectPackageRejectsAmbiguousPayload(t *testing.T) {
	data := detectorZIP(t,
		zipEntry{name: "content.xml", body: []byte("<xmap-content/>")},
		zipEntry{name: "content.json", body: []byte(`[]`)},
	)
	_, err := detectBytes(data, Limits{})
	requireCode(t, err, ErrorAmbiguousFormat)
	if !errors.Is(err, ErrAmbiguousFormat) {
		t.Fatalf("errors.Is(%v, ErrAmbiguousFormat) = false", err)
	}
}

func TestDetectPackageDoesNotFallbackFromManifestSelectedJSON(t *testing.T) {
	data := detectorZIP(t,
		zipEntry{name: "manifest.json", body: []byte(`{"file-entries":{"content.json":{},"metadata.json":{}}}`)},
		zipEntry{name: "metadata.json", body: []byte(`{"dataStructureVersion":"3","activeSheetId":"sheet","creator":{"name":"Vana","version":"12.0.2"}}`)},
		zipEntry{name: "content.json", body: []byte(`[{`)},
		zipEntry{name: "content.xml", body: []byte(`<xmap-content><sheet id="warning"/></xmap-content>`)},
	)
	_, err := detectBytes(data, Limits{})
	requireCode(t, err, ErrorInvalidPayload)
}

func TestDetectPackageRejectsManifestThatOmitsAvailableJSONPayload(t *testing.T) {
	data := detectorZIP(t,
		zipEntry{name: "manifest.json", body: []byte(`{"file-entries":{"metadata.json":{}}}`)},
		zipEntry{name: "metadata.json", body: []byte(`{}`)},
		zipEntry{name: "content.json", body: []byte(`[{"id":"sheet","rootTopic":{"id":"root"}}]`)},
	)
	_, err := detectBytes(data, Limits{})
	packageErr := requireCode(t, err, ErrorInvalidPayload)
	if !strings.Contains(packageErr.Operation, "validate XMind manifest") || !strings.Contains(packageErr.Error(), "does not declare") {
		t.Fatalf("manifest omission did not fail explicitly: %v", packageErr)
	}
}

func TestDetectPackageWarnsOnLegacyMetadataActiveSheetMismatch(t *testing.T) {
	data := detectorZIP(t,
		zipEntry{name: "manifest.json", body: []byte(`{"file-entries":{"content.json":{},"metadata.json":{}}}`)},
		zipEntry{name: "metadata.json", body: []byte(`{"dataStructureVersion":"3","activeSheetId":"missing","creator":{"name":"Vana","version":"12.0.2"}}`)},
		zipEntry{name: "content.json", body: []byte(`[{"id":"sheet","rootTopic":{"id":"root"}}]`)},
	)
	detection, err := detectBytes(data, Limits{})
	if err != nil {
		t.Fatalf("DetectPackage: %v", err)
	}
	if detection.Family != FormatClassicZenJSON || detection.FixtureRequired {
		t.Fatalf("activeSheet mismatch blocked structural admission: %#v", detection)
	}
	foundWarning := false
	for _, feature := range detection.Inventory.Features {
		foundWarning = foundWarning || feature.Feature == "active_sheet_reference" && feature.SourceName == "missing" && feature.Status == InventoryUnsupported && feature.ReasonCode == "missing_active_sheet" && strings.Contains(feature.Detail, "workbook content remains importable")
	}
	if !foundWarning {
		t.Fatalf("activeSheet mismatch lacks inventory warning: %#v", detection.Inventory.Features)
	}
	workbook, err := ParseWorkbook(bytes.NewReader(data), int64(len(data)), Limits{})
	if err != nil || len(workbook.Sheets) != 1 {
		t.Fatalf("ParseWorkbook(activeSheet mismatch) = %#v, %v", workbook, err)
	}
}

func TestDetectPackageAdmitsManifestSelectedLegacyJSONByStructure(t *testing.T) {
	tests := []struct {
		name          string
		manifest      string
		metadata      *string
		formatVersion string
	}{
		{
			name:     "empty metadata from official SDK shape",
			manifest: `{"file-entries":{"content.json":{},"metadata.json":{}}}`,
			metadata: stringPointer(`{}`),
		},
		{
			name:     "malformed optional metadata",
			manifest: `{"file-entries":{"content.json":{},"metadata.json":{}}}`,
			metadata: stringPointer(`{"creator":`),
		},
		{
			name:          "unknown producer and data version",
			manifest:      `{"file-entries":{"content.json":{},"metadata.json":{}}}`,
			metadata:      stringPointer(`{"dataStructureVersion":"77","activeSheetId":"sheet","creator":{"name":"Other Producer","version":"99.7"}}`),
			formatVersion: "99.7",
		},
		{
			name:          "26.02 version metadata on legacy shape",
			manifest:      `{"file-entries":{"content.json":{},"metadata.json":{}}}`,
			metadata:      stringPointer(`{"dataStructureVersion":"2","activeSheetId":"sheet","creator":{"name":"Vana","version":"26.02.00001"}}`),
			formatVersion: "26.02.00001",
		},
		{
			name:     "metadata entry absent",
			manifest: `{"file-entries":{"content.json":{}}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entries := []zipEntry{
				{name: "manifest.json", body: []byte(test.manifest)},
				{name: "content.json", body: []byte(`[{"id":"sheet","title":"Sheet","rootTopic":{"id":"root","title":"Root"}}]`)},
			}
			if test.metadata != nil {
				entries = append(entries, zipEntry{name: "metadata.json", body: []byte(*test.metadata)})
			}
			data := detectorZIP(t, entries...)
			detection, err := detectBytes(data, Limits{})
			if err != nil {
				t.Fatalf("DetectPackage: %v", err)
			}
			if detection.Family != FormatClassicZenJSON || detection.FixtureRequired || detection.FormatVersion != test.formatVersion {
				t.Fatalf("structure-driven detection = %#v", detection)
			}
			if len(detection.CandidateFamilies) != 1 || detection.CandidateFamilies[0] != FormatClassicZenJSON {
				t.Fatalf("candidate families = %#v", detection.CandidateFamilies)
			}
			foundIndexedPayload := false
			foundMetadataProvenance := test.metadata == nil
			foundInvalidMetadata := false
			for _, feature := range detection.Inventory.Features {
				foundIndexedPayload = foundIndexedPayload || feature.Feature == "workbook_payload" && feature.SourceName == "content.json" && feature.Status == InventoryIndexed && feature.ReasonCode == "validated_legacy_sheet_array_structure"
				foundMetadataProvenance = foundMetadataProvenance || feature.Feature == "package_metadata" && feature.SourceName == "metadata.json" && feature.Status == InventoryPreserved && feature.ReasonCode == "format_provenance_not_compatibility_claim"
				foundInvalidMetadata = foundInvalidMetadata || feature.Feature == "package_metadata" && feature.Status == InventoryUnsupported && feature.ReasonCode == "invalid_metadata_json"
			}
			if !foundIndexedPayload || (!foundMetadataProvenance && !foundInvalidMetadata) {
				t.Fatalf("missing structural/provenance inventory: %#v", detection.Inventory.Features)
			}
			workbook, err := ParseWorkbook(bytes.NewReader(data), int64(len(data)), Limits{})
			if err != nil {
				t.Fatalf("ParseWorkbook: %v", err)
			}
			if workbook.FormatFamily != FormatClassicZenJSON || workbook.FormatVersion != test.formatVersion || len(workbook.Sheets) != 1 || len(workbook.Sheets[0].Roots) != 1 {
				t.Fatalf("workbook = %#v", workbook)
			}
		})
	}
	for _, contract := range AdapterContracts() {
		if contract.Family == FormatV2602Plus && contract.Status != AdapterFixtureRequired {
			t.Fatalf("structure-driven legacy admission overclaimed current-format support: %#v", contract)
		}
	}
}

func TestDetectPackageRejectsManifestSelectedJSONWithIncompatibleCoreShape(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "object payload", content: `{"version":"26.02","zones":[]}`},
		{name: "empty Sheet array", content: `[]`},
		{name: "missing Sheet id", content: `[{"rootTopic":{"id":"root"}}]`},
		{name: "missing root Topic", content: `[{"id":"sheet"}]`},
		{name: "missing root Topic id", content: `[{"id":"sheet","rootTopic":{"title":"Root"}}]`},
		{name: "non-string root Topic id", content: `[{"id":"sheet","rootTopic":{"id":42}}]`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data := detectorZIP(t,
				zipEntry{name: "manifest.json", body: []byte(`{"file-entries":{"content.json":{}}}`)},
				zipEntry{name: "content.json", body: []byte(test.content)},
			)
			_, err := detectBytes(data, Limits{})
			packageErr := requireCode(t, err, ErrorInvalidPayload)
			if !strings.Contains(packageErr.Operation, "legacy XMind JSON structure") {
				t.Fatalf("error is not actionable for incompatible JSON structure: %v", packageErr)
			}
		})
	}
}

func stringPointer(value string) *string {
	return &value
}

func TestDetectPackageRejectsUnknownAndDamaged(t *testing.T) {
	t.Run("unknown payload", func(t *testing.T) {
		data := detectorZIP(t, zipEntry{name: "metadata.json", body: []byte(`{}`)})
		_, err := detectBytes(data, Limits{})
		requireCode(t, err, ErrorUnknownFormat)
	})
	t.Run("not zip", func(t *testing.T) {
		_, err := detectBytes([]byte("not a zip"), Limits{})
		requireCode(t, err, ErrorInvalidArchive)
	})
	t.Run("truncated zip", func(t *testing.T) {
		data := detectorZIP(t, zipEntry{name: "content.xml", body: []byte("<xmap-content/>")})
		_, err := detectBytes(data[:len(data)-5], Limits{})
		requireCode(t, err, ErrorInvalidArchive)
	})
	t.Run("damaged entry", func(t *testing.T) {
		body := []byte("unique-uncompressed-payload")
		data := detectorZIP(t, zipEntry{name: "content.xml", body: body, method: zip.Store})
		position := bytes.Index(data, body)
		if position < 0 {
			t.Fatal("stored payload not found in ZIP")
		}
		data[position] ^= 0xff
		_, err := detectBytes(data, Limits{})
		requireCode(t, err, ErrorInvalidArchive)
	})
}

func TestDetectPackageRejectsEncryptedEntry(t *testing.T) {
	data := detectorZIP(t, zipEntry{name: "content.xml", body: []byte("encrypted marker"), flags: 1})
	_, err := detectBytes(data, Limits{})
	requireCode(t, err, ErrorEncryptedArchive)
}

func TestDetectPackageRejectsUnsafePaths(t *testing.T) {
	paths := []string{"../content.xml", "/content.xml", "folder/../content.xml", "folder\\content.xml", "C:/content.xml", "./content.xml", "folder//content.xml"}
	for _, name := range paths {
		t.Run(strings.ReplaceAll(name, "/", "_"), func(t *testing.T) {
			data := detectorZIP(t, zipEntry{name: name, body: []byte("payload")})
			_, err := detectBytes(data, Limits{})
			requireCode(t, err, ErrorUnsafeArchivePath)
		})
	}
}

func TestDetectPackageRejectsDuplicateNormalizedEntry(t *testing.T) {
	data := detectorZIP(t,
		zipEntry{name: "content.xml", body: []byte("one")},
		zipEntry{name: "content.xml", body: []byte("two")},
	)
	_, err := detectBytes(data, Limits{})
	requireCode(t, err, ErrorDuplicateEntry)
}

func TestDetectPackageEnforcesLimits(t *testing.T) {
	t.Run("archive bytes", func(t *testing.T) {
		data := detectorZIP(t, zipEntry{name: "content.xml", body: []byte("payload")})
		_, err := detectBytes(data, Limits{MaxArchiveBytes: uint64(len(data) - 1)})
		limitErr := requireCode(t, err, ErrorArchiveLimit)
		if limitErr.Limit != "archive_bytes" {
			t.Fatalf("limit = %q", limitErr.Limit)
		}
	})
	t.Run("entry count", func(t *testing.T) {
		data := detectorZIP(t,
			zipEntry{name: "content.xml", body: []byte("payload")},
			zipEntry{name: "meta.json", body: []byte("payload")},
		)
		_, err := detectBytes(data, Limits{MaxEntries: 1})
		requireCode(t, err, ErrorArchiveLimit)
	})
	t.Run("entry bytes", func(t *testing.T) {
		data := detectorZIP(t, zipEntry{name: "content.xml", body: []byte("12345"), method: zip.Store})
		_, err := detectBytes(data, Limits{MaxEntryBytes: 4})
		requireCode(t, err, ErrorArchiveLimit)
	})
	t.Run("total bytes", func(t *testing.T) {
		data := detectorZIP(t,
			zipEntry{name: "content.xml", body: []byte("1234"), method: zip.Store},
			zipEntry{name: "meta.json", body: []byte("5678"), method: zip.Store},
		)
		_, err := detectBytes(data, Limits{MaxEntryBytes: 10, MaxTotalBytes: 7})
		requireCode(t, err, ErrorArchiveLimit)
	})
	t.Run("compression ratio", func(t *testing.T) {
		data := detectorZIP(t, zipEntry{name: "content.xml", body: bytes.Repeat([]byte("a"), 4096), method: zip.Deflate})
		_, err := detectBytes(data, Limits{MaxCompressionRatio: 2})
		limitErr := requireCode(t, err, ErrorArchiveLimit)
		if limitErr.Limit != "compression_ratio" {
			t.Fatalf("limit = %q", limitErr.Limit)
		}
	})
}

func TestParseWorkbookFailsClosedBeforeReturningWorkbook(t *testing.T) {
	t.Run("detection error wins", func(t *testing.T) {
		workbook, err := ParseWorkbook(bytes.NewReader([]byte("damaged")), int64(len("damaged")), Limits{})
		requireCode(t, err, ErrorInvalidArchive)
		if workbook.FormatFamily != "" || len(workbook.Sheets) != 0 {
			t.Fatalf("workbook returned on failure: %#v", workbook)
		}
	})
	t.Run("invalid XML core structure", func(t *testing.T) {
		data := detectorZIP(t, zipEntry{name: "content.xml", body: []byte("<xmap-content/>")})
		workbook, err := ParseWorkbook(bytes.NewReader(data), int64(len(data)), Limits{})
		requireCode(t, err, ErrorValidation)
		if workbook.FormatFamily != "" || len(workbook.Sheets) != 0 {
			t.Fatalf("workbook returned on invalid XML structure: %#v", workbook)
		}
	})
	t.Run("ambiguous payload fails before adapter dispatch", func(t *testing.T) {
		data := detectorZIP(t,
			zipEntry{name: "content.xml", body: []byte("<xmap-content/>")},
			zipEntry{name: "content.json", body: []byte(`[]`)},
		)
		workbook, err := ParseWorkbook(bytes.NewReader(data), int64(len(data)), Limits{})
		requireCode(t, err, ErrorAmbiguousFormat)
		if workbook.FormatFamily != "" || len(workbook.Sheets) != 0 {
			t.Fatalf("workbook returned for ambiguous payload: %#v", workbook)
		}
	})
}

func TestAdapterContractsExposeEvidenceAndCoverageBoundaries(t *testing.T) {
	contracts := AdapterContracts()
	if len(contracts) != 3 {
		t.Fatalf("contracts = %d, want 3", len(contracts))
	}
	for _, contract := range contracts {
		switch contract.Family {
		case FormatClassicZenJSON:
			if contract.Status != AdapterSupported || contract.ValidatedEvidence == "" || contract.CoverageLimit == "" {
				t.Fatalf("classic/Zen contract lacks fixture evidence or coverage boundary: %#v", contract)
			}
		case FormatXMind8XML:
			if contract.Status != AdapterStructureGated || contract.RequiredEvidence == "" || contract.ValidatedEvidence == "" || contract.CoverageLimit == "" {
				t.Fatalf("XMind 8 contract lacks structural or fixture boundary: %#v", contract)
			}
		default:
			if contract.Status != AdapterFixtureRequired || contract.RequiredEvidence == "" {
				t.Fatalf("adapter contract overclaims support: %#v", contract)
			}
		}
	}
}

func TestFeatureInventoryCoverageAndValidation(t *testing.T) {
	inventory := FeatureInventory{Features: []FeatureEncounter{
		{Feature: "topic", Location: "/0/rootTopic", Status: InventoryIndexed, Count: 2},
		{Feature: "zone", Location: "/0/extensions/zone", Status: InventoryUnsupported, Count: 1, ReasonCode: "adapter_fixture_required", Raw: jsonRaw(`{"future":true}`)},
		{Feature: "topic", Location: "/1/rootTopic", Status: InventoryPreserved, Count: 1},
	}}
	if err := inventory.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	coverage := inventory.Coverage()
	want := []FeatureCoverage{
		{Feature: "topic", Status: InventoryIndexed, Count: 2},
		{Feature: "topic", Status: InventoryPreserved, Count: 1},
		{Feature: "zone", Status: InventoryUnsupported, Count: 1},
	}
	if len(coverage) != len(want) {
		t.Fatalf("coverage = %#v, want %#v", coverage, want)
	}
	for index := range want {
		if coverage[index] != want[index] {
			t.Fatalf("coverage[%d] = %#v, want %#v", index, coverage[index], want[index])
		}
	}

	invalid := FeatureInventory{Features: []FeatureEncounter{{Feature: "unknown", Location: "/x", Count: 1}}}
	if err := invalid.Validate(); err == nil {
		t.Fatal("inventory item without disposition was silently accepted")
	}

	unsupportedWithoutReason := FeatureInventory{Features: []FeatureEncounter{{Feature: "unknown", Location: "/x", Status: InventoryUnsupported, Count: 1}}}
	if err := unsupportedWithoutReason.Validate(); err == nil {
		t.Fatal("unsupported item without an explicit reason was silently accepted")
	}
}

func jsonRaw(value string) []byte { return []byte(value) }
