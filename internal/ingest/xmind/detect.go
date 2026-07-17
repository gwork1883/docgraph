package xmind

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"path"
	"strconv"
	"strings"
)

type AdapterStatus string

const (
	AdapterFixtureRequired AdapterStatus = "fixture_required"
	AdapterStructureGated  AdapterStatus = "structure_gated"
	AdapterSupported       AdapterStatus = "supported"
)

type AdapterContract struct {
	Family            FormatFamily  `json:"family"`
	Status            AdapterStatus `json:"status"`
	RequiredEvidence  string        `json:"required_evidence,omitempty"`
	ValidatedEvidence string        `json:"validated_evidence,omitempty"`
	CoverageLimit     string        `json:"coverage_limit,omitempty"`
}

// AdapterContracts separates format admission evidence from feature coverage.
// A supported or structure-gated family still preserves unrecognized fields
// and reports unsupported features instead of implying that every producer
// feature has a real fixture.
func AdapterContracts() []AdapterContract {
	return []AdapterContract{
		{
			Family:            FormatXMind8XML,
			Status:            AdapterStructureGated,
			RequiredEvidence:  "real native XMind 8 content.xml fixture for feature-specific compatibility claims",
			ValidatedEvidence: "legacy xmap-content with core Sheet/root Topic/ID structure; namespace and version are diagnostic provenance",
			CoverageLimit:     "production admission is structural; unverified namespace/version and unknown local elements are preserved and reported, while feature encodings without a real fixture remain preserved or unsupported",
		},
		{
			Family:            FormatClassicZenJSON,
			Status:            AdapterSupported,
			ValidatedEvidence: "legacy Sheet-array JSON selected by manifest or as the package's unique root workbook payload, validated against the official schema shape and real XMind 12.0.2/dataStructureVersion 3 and XMind 26.01/dataStructureVersion 2 fixtures",
			CoverageLimit:     "admission is driven by the legacy Sheet/rootTopic structure, not producer or version metadata; fixtures prove pre-26.02 JSON v2/v3 feature coverage only, so metadata from other producers or versions is provenance rather than a complete compatibility claim",
		},
		{Family: FormatV2602Plus, Status: AdapterFixtureRequired, RequiredEvidence: "real V26.02+ fixture and fixture-proven discriminator"},
	}
}

type packageManifest struct {
	FileEntries map[string]json.RawMessage `json:"file-entries"`
}

type packageMetadata struct {
	DataStructureVersion string `json:"dataStructureVersion"`
	ActiveSheetID        string `json:"activeSheetId"`
	LayoutEngineVersion  string `json:"layoutEngineVersion"`
	Creator              struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"creator"`
}

type DetectionEvidence struct {
	Entry  string `json:"entry"`
	Reason string `json:"reason"`
}

type PackageDetection struct {
	Family            FormatFamily        `json:"family"`
	CandidateFamilies []FormatFamily      `json:"candidate_families"`
	FormatVersion     string              `json:"format_version,omitempty"`
	FixtureRequired   bool                `json:"fixture_required"`
	Evidence          []DetectionEvidence `json:"evidence"`
	Entries           []PackageEntry      `json:"entries"`
	Inventory         FeatureInventory    `json:"inventory"`
}

// DetectPackage validates the complete ZIP before returning format evidence.
// It never extracts a member to disk and never treats the filename extension as
// format evidence.
func DetectPackage(reader io.ReaderAt, size int64, limits Limits) (PackageDetection, error) {
	if reader == nil || size < 0 {
		return PackageDetection{}, &PackageError{Code: ErrorInvalidArgument, Operation: "detect xmind package", Detail: "reader is nil or size is negative"}
	}
	limits = limits.withDefaults()
	if uint64(size) > limits.MaxArchiveBytes {
		return PackageDetection{}, limitError("archive_bytes", uint64(size), limits.MaxArchiveBytes, "")
	}

	zr, err := zip.NewReader(reader, size)
	if err != nil {
		return PackageDetection{}, &PackageError{Code: ErrorInvalidArchive, Operation: "open xmind zip", Err: err}
	}
	if uint64(len(zr.File)) > limits.MaxEntries {
		return PackageDetection{}, limitError("entry_count", uint64(len(zr.File)), limits.MaxEntries, "")
	}

	entries := make([]PackageEntry, 0, len(zr.File))
	resources := make([]ResourceEncounter, 0, len(zr.File))
	seen := make(map[string]string, len(zr.File))
	var total uint64
	for _, file := range zr.File {
		if file.Flags&(1|1<<6) != 0 {
			return PackageDetection{}, &PackageError{Code: ErrorEncryptedArchive, Operation: "inspect xmind zip", Entry: file.Name, Detail: "encrypted ZIP entries are not supported"}
		}
		normalized, err := safeArchivePath(file.Name)
		if err != nil {
			return PackageDetection{}, &PackageError{Code: ErrorUnsafeArchivePath, Operation: "inspect xmind zip", Entry: file.Name, Err: err}
		}
		if previous, ok := seen[normalized]; ok {
			return PackageDetection{}, &PackageError{Code: ErrorDuplicateEntry, Operation: "inspect xmind zip", Entry: file.Name, Detail: fmt.Sprintf("normalizes to the same path as %q", previous)}
		}
		seen[normalized] = file.Name

		if file.UncompressedSize64 > limits.MaxEntryBytes {
			return PackageDetection{}, limitError("entry_uncompressed_bytes", file.UncompressedSize64, limits.MaxEntryBytes, file.Name)
		}
		if file.UncompressedSize64 > limits.MaxTotalBytes || total > limits.MaxTotalBytes-file.UncompressedSize64 {
			return PackageDetection{}, limitError("total_uncompressed_bytes", saturatingAdd(total, file.UncompressedSize64), limits.MaxTotalBytes, file.Name)
		}
		total += file.UncompressedSize64
		if ratioExceeded(file.UncompressedSize64, file.CompressedSize64, limits.MaxCompressionRatio) {
			return PackageDetection{}, limitError("compression_ratio", compressionRatioCeiling(file.UncompressedSize64, file.CompressedSize64), limits.MaxCompressionRatio, file.Name)
		}
		if file.UncompressedSize64 > math.MaxInt64 {
			return PackageDetection{}, limitError("entry_stream_bytes", file.UncompressedSize64, math.MaxInt64, file.Name)
		}

		entry := PackageEntry{
			Path:              file.Name,
			Directory:         file.FileInfo().IsDir(),
			Method:            file.Method,
			Flags:             file.Flags,
			CRC32:             file.CRC32,
			CompressedBytes:   file.CompressedSize64,
			UncompressedBytes: file.UncompressedSize64,
		}
		digest, err := validateAndDigest(file)
		if err != nil {
			return PackageDetection{}, &PackageError{Code: ErrorInvalidArchive, Operation: "read xmind zip entry", Entry: file.Name, Err: err}
		}
		detectedMIME := ""
		if !entry.Directory {
			entry.SHA256 = digest
			detectedMIME, err = sniffPackageEntry(file)
			if err != nil {
				return PackageDetection{}, &PackageError{Code: ErrorInvalidArchive, Operation: "sniff xmind zip entry", Entry: file.Name, Err: err}
			}
		}
		entries = append(entries, entry)
		resources = append(resources, ResourceEncounter{PackageEntry: entry, Status: InventoryPreserved, ReasonCode: "package_snapshot", DetectedMIME: detectedMIME})
	}

	hasXML := false
	hasJSON := false
	hasManifest := false
	hasMetadata := false
	for _, entry := range entries {
		if entry.Directory {
			continue
		}
		switch entry.Path {
		case "content.xml":
			hasXML = true
		case "content.json":
			hasJSON = true
		case "manifest.json":
			hasManifest = true
		case "metadata.json":
			hasMetadata = true
		}
	}

	selectedPayload := ""
	manifestSelectsPayload := false
	if hasXML && hasJSON {
		if !hasManifest {
			return PackageDetection{}, &PackageError{Code: ErrorAmbiguousFormat, Operation: "detect xmind payload", Detail: "both root content.xml and content.json are present without a manifest-selected payload"}
		}
		payload, err := readPackageEntry(reader, size, "manifest.json")
		if err != nil {
			return PackageDetection{}, invalidPayload("read XMind manifest", err)
		}
		var manifest packageManifest
		if err := json.Unmarshal(payload, &manifest); err != nil {
			return PackageDetection{}, invalidPayload("decode XMind manifest", err)
		}
		_, declaresXML := manifest.FileEntries["content.xml"]
		_, declaresJSON := manifest.FileEntries["content.json"]
		switch {
		case declaresJSON && !declaresXML:
			selectedPayload = "content.json"
			manifestSelectsPayload = true
		case declaresXML && !declaresJSON:
			selectedPayload = "content.xml"
			manifestSelectsPayload = true
		default:
			return PackageDetection{}, &PackageError{Code: ErrorAmbiguousFormat, Operation: "detect xmind payload", Detail: "manifest does not select exactly one of content.xml and content.json"}
		}
	} else if hasJSON {
		selectedPayload = "content.json"
		if hasManifest {
			selected, err := manifestSelects(reader, size, "content.json")
			if err != nil {
				return PackageDetection{}, err
			}
			if !selected {
				return PackageDetection{}, invalidPayload("validate XMind manifest", fmt.Errorf("manifest does not declare the available content.json payload"))
			}
			manifestSelectsPayload = true
		}
	} else if hasXML {
		selectedPayload = "content.xml"
		if hasManifest {
			selected, err := manifestSelects(reader, size, "content.xml")
			if err != nil {
				return PackageDetection{}, err
			}
			if !selected {
				return PackageDetection{}, invalidPayload("validate XMind manifest", fmt.Errorf("manifest does not declare the available content.xml payload"))
			}
			manifestSelectsPayload = true
		}
	}

	detection := PackageDetection{
		FixtureRequired: true,
		Entries:         entries,
		Inventory:       FeatureInventory{Resources: resources},
	}
	switch selectedPayload {
	case "content.xml":
		detection.Family = FormatXMind8XML
		detection.CandidateFamilies = []FormatFamily{FormatXMind8XML}
		detection.Evidence = []DetectionEvidence{{Entry: "content.xml", Reason: "root XML workbook payload candidate"}}
		if hasJSON {
			detection.Evidence = append(detection.Evidence, DetectionEvidence{Entry: "manifest.json", Reason: "manifest selects content.xml and excludes root content.json"})
			detection.Inventory.Features = append(detection.Inventory.Features, FeatureEncounter{Feature: "inactive_workbook_payload", SourceName: "content.json", Location: "zip:/content.json", Status: InventoryPreserved, Count: 1, ReasonCode: "not_selected_by_manifest"})
		}
		detection.Inventory.Features = append(detection.Inventory.Features, FeatureEncounter{Feature: "workbook_payload", SourceName: "content.xml", Location: "zip:/content.xml", Status: InventoryPreserved, Count: 1, ReasonCode: "adapter_fixture_required"})
	case "content.json":
		// A manifest-selected content.json, or the unique root content.json when
		// no manifest is present, can use the legacy JSON adapter when it has the
		// legacy Sheet-array/rootTopic shape. Metadata is provenance, not a
		// compatibility whitelist; structurally different current formats remain
		// fail-closed.
		detection.Family = FormatJSONCandidate
		detection.CandidateFamilies = []FormatFamily{FormatClassicZenJSON, FormatV2602Plus}
		detection.Evidence = []DetectionEvidence{{Entry: "content.json", Reason: "root JSON workbook payload; unambiguous payload selection and core structure determine adapter admission"}}
		detection.Inventory.Features = append(detection.Inventory.Features, FeatureEncounter{Feature: "workbook_payload", SourceName: "content.json", Location: "zip:/content.json", Status: InventoryPreserved, Count: 1, ReasonCode: "adapter_fixture_required"})
		if hasXML {
			detection.Evidence = append(detection.Evidence, DetectionEvidence{Entry: "manifest.json", Reason: "manifest selects content.json and excludes root content.xml"})
			detection.Inventory.Features = append(detection.Inventory.Features, FeatureEncounter{Feature: "inactive_workbook_payload", SourceName: "content.xml", Location: "zip:/content.xml", Status: InventoryPreserved, Count: 1, ReasonCode: "not_selected_by_manifest"})
		}
		activeSheetID := ""
		if hasMetadata {
			metadataPayload, err := readPackageEntry(reader, size, "metadata.json")
			if err != nil {
				return PackageDetection{}, invalidPayload("read XMind metadata", err)
			}
			var metadata packageMetadata
			if err := json.Unmarshal(metadataPayload, &metadata); err != nil {
				detection.Evidence = append(detection.Evidence, DetectionEvidence{
					Entry:  "metadata.json",
					Reason: "metadata could not be decoded and was ignored for adapter admission: " + err.Error(),
				})
				detection.Inventory.Features = append(detection.Inventory.Features, FeatureEncounter{
					Feature: "package_metadata", SourceName: "metadata.json", Location: "zip:/metadata.json",
					Status: InventoryUnsupported, Count: 1, ReasonCode: "invalid_metadata_json",
					Detail: err.Error(), Raw: json.RawMessage(strconv.Quote(string(metadataPayload))),
				})
			} else {
				activeSheetID = metadata.ActiveSheetID
				detection.FormatVersion = strings.TrimSpace(metadata.Creator.Version)
				detection.Evidence = append(detection.Evidence, DetectionEvidence{
					Entry:  "metadata.json",
					Reason: metadataEvidence(metadata),
				})
				detection.Inventory.Features = append(detection.Inventory.Features, FeatureEncounter{Feature: "package_metadata", SourceName: "metadata.json", Location: "zip:/metadata.json", Status: InventoryPreserved, Count: 1, ReasonCode: "format_provenance_not_compatibility_claim", Raw: cloneRaw(metadataPayload)})
			}
		}
		structureSelected := manifestSelectsPayload || !hasManifest && !hasXML
		if structureSelected {
			contentPayload, err := readPackageEntry(reader, size, "content.json")
			if err != nil {
				return PackageDetection{}, invalidPayload("read XMind JSON payload", err)
			}
			activeSheetFound, err := validateLegacyJSONSignature(contentPayload, activeSheetID)
			if err != nil {
				return PackageDetection{}, invalidPayload("validate legacy XMind JSON structure", err)
			}
			if activeSheetID != "" && !activeSheetFound {
				detection.Inventory.Features = append(detection.Inventory.Features, FeatureEncounter{
					Feature:    "active_sheet_reference",
					SourceName: activeSheetID,
					Location:   "zip:/metadata.json#/activeSheetId",
					Status:     InventoryUnsupported,
					Count:      1,
					ReasonCode: "missing_active_sheet",
					Detail:     fmt.Sprintf("metadata activeSheetId %q does not match any parsed Sheet; workbook content remains importable", activeSheetID),
				})
			}
			detection.Family = FormatClassicZenJSON
			detection.CandidateFamilies = []FormatFamily{FormatClassicZenJSON}
			detection.FixtureRequired = false
			selectionReason := "unique root content.json selected because no manifest or competing XML payload exists"
			if manifestSelectsPayload {
				selectionReason = "manifest-selected content.json"
			}
			detection.Evidence = append(detection.Evidence, DetectionEvidence{
				Entry:  "content.json",
				Reason: selectionReason + "; legacy Sheet array with required Sheet/rootTopic IDs validated; producer/version metadata was not used for admission",
			})
			for index := range detection.Inventory.Features {
				feature := &detection.Inventory.Features[index]
				if feature.Feature == "workbook_payload" && feature.SourceName == "content.json" {
					feature.Status = InventoryIndexed
					feature.ReasonCode = "validated_legacy_sheet_array_structure"
					break
				}
			}
		}
	default:
		return PackageDetection{}, &PackageError{Code: ErrorUnknownFormat, Operation: "detect xmind payload", Detail: "no recognized root workbook payload"}
	}
	if err := detection.Inventory.Validate(); err != nil {
		return PackageDetection{}, &PackageError{Code: ErrorInvalidArchive, Operation: "validate feature inventory", Err: err}
	}
	return detection, nil
}

func sniffPackageEntry(file *zip.File) (string, error) {
	reader, err := file.Open()
	if err != nil {
		return "", err
	}
	head := make([]byte, 512)
	read, readErr := io.ReadFull(reader, head)
	closeErr := reader.Close()
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		return "", readErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return http.DetectContentType(head[:read]), nil
}

func metadataEvidence(metadata packageMetadata) string {
	creator := strings.TrimSpace(strings.TrimSpace(metadata.Creator.Name + " " + metadata.Creator.Version))
	dataVersion := strings.TrimSpace(metadata.DataStructureVersion)
	if creator == "" && dataVersion == "" && strings.TrimSpace(metadata.ActiveSheetID) == "" {
		return "metadata is empty; retained as provenance and not used for adapter admission"
	}
	parts := make([]string, 0, 3)
	if dataVersion != "" {
		parts = append(parts, "dataStructureVersion "+dataVersion)
	}
	if creator != "" {
		parts = append(parts, "creator "+creator)
	}
	if activeSheetID := strings.TrimSpace(metadata.ActiveSheetID); activeSheetID != "" {
		parts = append(parts, "activeSheetId "+activeSheetID)
	}
	return strings.Join(parts, "; ") + "; retained as provenance and not used for adapter admission"
}

func manifestSelects(reader io.ReaderAt, size int64, payloadName string) (bool, error) {
	payload, err := readPackageEntry(reader, size, "manifest.json")
	if err != nil {
		return false, invalidPayload("read XMind manifest", err)
	}
	var manifest packageManifest
	if err := json.Unmarshal(payload, &manifest); err != nil {
		return false, invalidPayload("decode XMind manifest", err)
	}
	_, selected := manifest.FileEntries[payloadName]
	return selected, nil
}

func validateLegacyJSONSignature(payload []byte, activeSheetID string) (bool, error) {
	var sheets []json.RawMessage
	if err := json.Unmarshal(payload, &sheets); err != nil {
		return false, err
	}
	if len(sheets) == 0 {
		return false, fmt.Errorf("content.json has no Sheets")
	}
	foundActive := activeSheetID == ""
	for index, rawSheet := range sheets {
		var sheet struct {
			ID        string          `json:"id"`
			RootTopic json.RawMessage `json:"rootTopic"`
		}
		if err := json.Unmarshal(rawSheet, &sheet); err != nil {
			return false, fmt.Errorf("Sheet %d: %w", index, err)
		}
		if strings.TrimSpace(sheet.ID) == "" {
			return false, fmt.Errorf("Sheet %d has no non-empty string id", index)
		}
		var root map[string]json.RawMessage
		if len(sheet.RootTopic) == 0 || string(sheet.RootTopic) == "null" || json.Unmarshal(sheet.RootTopic, &root) != nil || root == nil {
			return false, fmt.Errorf("Sheet %d rootTopic is not an object", index)
		}
		var rootID string
		if rawRootID, ok := root["id"]; !ok || json.Unmarshal(rawRootID, &rootID) != nil || strings.TrimSpace(rootID) == "" {
			return false, fmt.Errorf("Sheet %d rootTopic has no non-empty string id", index)
		}
		if sheet.ID == activeSheetID {
			foundActive = true
		}
	}
	return foundActive, nil
}

func validateAndDigest(file *zip.File) (string, error) {
	reader, err := file.Open()
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(hash, reader)
	closeErr := reader.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if uint64(written) != file.UncompressedSize64 {
		return "", fmt.Errorf("uncompressed size mismatch: read=%d declared=%d", written, file.UncompressedSize64)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func safeArchivePath(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty entry name")
	}
	if strings.ContainsRune(name, '\x00') {
		return "", fmt.Errorf("entry name contains NUL")
	}
	if strings.Contains(name, "\\") {
		return "", fmt.Errorf("entry name contains a backslash")
	}
	if strings.HasPrefix(name, "/") || path.IsAbs(name) {
		return "", fmt.Errorf("entry name is absolute")
	}
	if len(name) >= 2 && ((name[0] >= 'A' && name[0] <= 'Z') || (name[0] >= 'a' && name[0] <= 'z')) && name[1] == ':' {
		return "", fmt.Errorf("entry name has a drive prefix")
	}
	trimmed := strings.TrimSuffix(name, "/")
	if trimmed == "" {
		return "", fmt.Errorf("entry name resolves to archive root")
	}
	cleaned := path.Clean(trimmed)
	if cleaned != trimmed || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("entry name is not a clean relative path")
	}
	return cleaned, nil
}

func ratioExceeded(uncompressed, compressed, maximum uint64) bool {
	if uncompressed == 0 {
		return false
	}
	if compressed == 0 {
		return true
	}
	quotient := uncompressed / compressed
	remainder := uncompressed % compressed
	return quotient > maximum || (quotient == maximum && remainder != 0)
}

func compressionRatioCeiling(uncompressed, compressed uint64) uint64 {
	if compressed == 0 {
		return math.MaxUint64
	}
	quotient := uncompressed / compressed
	if uncompressed%compressed != 0 {
		quotient++
	}
	return quotient
}

func saturatingAdd(left, right uint64) uint64 {
	if math.MaxUint64-left < right {
		return math.MaxUint64
	}
	return left + right
}

func limitError(name string, actual, maximum uint64, entry string) *PackageError {
	return &PackageError{Code: ErrorArchiveLimit, Operation: "inspect xmind zip", Entry: entry, Limit: name, Actual: actual, Maximum: maximum}
}
