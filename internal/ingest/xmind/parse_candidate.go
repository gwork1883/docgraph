package xmind

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
)

// ParseCandidateForValidation exposes official-schema-backed adapters for
// synthetic contract tests. Production ingestion uses the same normalization
// only after DetectPackage has admitted a format through an unambiguous payload
// and its required core structure. Producer/version metadata remains provenance.
func ParseCandidateForValidation(reader io.ReaderAt, size int64, limits Limits) (Workbook, error) {
	detection, err := DetectPackage(reader, size, limits)
	if err != nil {
		return Workbook{}, err
	}
	return parseDetectedWorkbook(reader, size, limits, detection)
}

func parseDetectedWorkbook(reader io.ReaderAt, size int64, limits Limits, detection PackageDetection) (Workbook, error) {
	limits = limits.withDefaults()
	state := newCandidateState(detection, limits)
	var workbook Workbook
	switch detection.Family {
	case FormatJSONCandidate, FormatClassicZenJSON:
		payload, err := readPackageEntry(reader, size, "content.json")
		if err != nil {
			return Workbook{}, invalidPayload("read content.json", err)
		}
		workbook, err = parseJSONCandidate(payload, state)
		if err != nil {
			return Workbook{}, err
		}
		workbook.FormatFamily = detection.Family
		if detection.Family == FormatClassicZenJSON || detection.FormatVersion != "" {
			workbook.FormatVersion = detection.FormatVersion
		}
	case FormatXMind8XML:
		payload, err := readPackageEntry(reader, size, "content.xml")
		if err != nil {
			return Workbook{}, invalidPayload("read content.xml", err)
		}
		workbook, err = parseXMLCandidate(payload, state)
		if err != nil {
			return Workbook{}, err
		}
	default:
		return Workbook{}, &PackageError{Code: ErrorUnsupportedFormat, Operation: "parse candidate workbook", Detail: "no candidate adapter for detected family"}
	}
	reconcileCandidateReferences(&workbook, state)
	workbook.FeatureInventory = state.inventory
	workbook.Assets = state.assets
	if err := validateCandidateLimits(&workbook, limits); err != nil {
		return Workbook{}, err
	}
	if err := validateCandidateWorkbook(&workbook); err != nil {
		return Workbook{}, err
	}
	if err := workbook.FeatureInventory.Validate(); err != nil {
		return Workbook{}, invalidPayload("validate candidate inventory", err)
	}
	snapshotHash, err := hashReaderAt(reader, size)
	if err != nil {
		return Workbook{}, invalidPayload("hash package snapshot", err)
	}
	workbook.SnapshotHash = snapshotHash
	workbook.SemanticHash = semanticHash(workbook)
	workbook.MediaManifestHash = mediaManifestHash(workbook)
	return workbook, nil
}

// reconcileCandidateReferences removes only source-authored relationships that
// cannot be represented because one of their Topic endpoints is absent. The
// workbook snapshot remains the lossless source of truth and the inventory
// records the skipped local feature. Missing Topic-link targets are retained so
// synchronization can expose them as unresolved resources instead of silently
// discarding the link.
func reconcileCandidateReferences(workbook *Workbook, state *candidateState) {
	sheetIDs := make(map[string]struct{}, len(workbook.Sheets))
	topicIDs := make(map[string]struct{})
	for sheetIndex := range workbook.Sheets {
		sheet := &workbook.Sheets[sheetIndex]
		sheetIDs[sheet.ID] = struct{}{}
		queue := append([]*Topic(nil), sheet.Roots...)
		for len(queue) > 0 {
			topic := queue[0]
			queue = queue[1:]
			if topic == nil {
				continue
			}
			topicIDs[topic.ID] = struct{}{}
			queue = append(queue, topic.Children...)
		}
	}

	seenRelationshipIDs := make(map[string]struct{})
	seenElementIDs := make(map[string]struct{})
	for sheetIndex := range workbook.Sheets {
		sheet := &workbook.Sheets[sheetIndex]
		validRelationships := make([]Relationship, 0, len(sheet.Relationships))
		for _, relationship := range sheet.Relationships {
			if relationship.ID == "" {
				state.feature("relationship", "relationship", "", fmt.Sprintf("/sheets/%d/relationships", sheetIndex), InventoryUnsupported, "missing_relationship_id", nil)
				continue
			}
			_, sourceExists := topicIDs[relationship.SourceTopicID]
			_, targetExists := topicIDs[relationship.TargetTopicID]
			if !sourceExists || !targetExists {
				detail := fmt.Sprintf("relationship %q skipped because endpoint %q -> %q is unresolved", relationship.ID, relationship.SourceTopicID, relationship.TargetTopicID)
				state.updateFeatureDispositionDetail("relationship", relationship.ID, InventoryUnsupported, "missing_relationship_endpoint", detail)
				continue
			}
			if _, duplicate := seenRelationshipIDs[relationship.ID]; duplicate {
				state.updateFeatureDispositionDetail("relationship", relationship.ID, InventoryUnsupported, "duplicate_relationship_id", fmt.Sprintf("duplicate workbook-scoped Relationship ID %q was skipped", relationship.ID))
				continue
			}
			seenRelationshipIDs[relationship.ID] = struct{}{}
			validRelationships = append(validRelationships, relationship)
		}
		sheet.Relationships = validRelationships

		validElements := make([]Element, 0, len(sheet.Elements))
		for elementIndex := range sheet.Elements {
			element := sheet.Elements[elementIndex]
			featureID := element.ID
			if element.DefinitionID != "" {
				featureID = element.DefinitionID
			}
			location := fmt.Sprintf("/sheets/%d/elements/%d", sheetIndex, elementIndex)
			if element.ID == "" {
				state.feature("special_element", element.Kind, featureID, location, InventoryUnsupported, "missing_element_id", element.Raw)
				continue
			}
			if _, duplicate := seenElementIDs[element.ID]; duplicate {
				state.feature("special_element", element.Kind, featureID, location, InventoryUnsupported, "duplicate_element_id", element.Raw)
				continue
			}
			seenElementIDs[element.ID] = struct{}{}
			if element.OwnerTopicID != "" {
				if _, ownerExists := topicIDs[element.OwnerTopicID]; !ownerExists {
					state.updateFeatureDispositionDetail(element.Kind, featureID, InventoryUnsupported, "missing_element_owner", fmt.Sprintf("owner Topic %q is unresolved", element.OwnerTopicID))
					element.OwnerTopicID = ""
					element.Status = InventoryPreserved
				}
			}
			validTargets := make([]string, 0, len(element.TargetIDs))
			for _, targetID := range element.TargetIDs {
				if targetID == "" {
					continue
				}
				if _, targetExists := topicIDs[targetID]; !targetExists {
					state.updateFeatureDispositionDetail(element.Kind, featureID, InventoryUnsupported, "missing_element_target", fmt.Sprintf("target Topic %q is unresolved", targetID))
					element.Status = InventoryPreserved
					continue
				}
				validTargets = append(validTargets, targetID)
			}
			element.TargetIDs = validTargets
			validElements = append(validElements, element)
		}
		sheet.Elements = validElements

		queue := append([]*Topic(nil), sheet.Roots...)
		for len(queue) > 0 {
			topic := queue[0]
			queue = queue[1:]
			if topic == nil {
				continue
			}
			for linkIndex, link := range topic.Links {
				_, topicExists := topicIDs[link.TargetTopicID]
				_, sheetExists := sheetIDs[link.TargetSheetID]
				missingTopic := link.TargetTopicID != "" && !topicExists
				missingSheet := link.TargetSheetID != "" && !sheetExists
				if !missingTopic && !missingSheet {
					continue
				}
				state.feature(
					"topic_link_target", link.Target, topic.ID,
					fmt.Sprintf("/sheets/%d/topics/%s/links/%d", sheetIndex, topic.ID, linkIndex),
					InventoryUnsupported, "missing_internal_link_target", json.RawMessage(strconv.Quote(link.Target)),
				)
			}
			queue = append(queue, topic.Children...)
		}
	}
}

func validateCandidateLimits(workbook *Workbook, limits Limits) error {
	var elements uint64
	addElement := func(location string) error {
		elements++
		if elements > limits.MaxNormalizedElements {
			return &PackageError{Code: ErrorArchiveLimit, Operation: "validate candidate workbook", Limit: "normalized_elements", Actual: elements, Maximum: limits.MaxNormalizedElements, Detail: location}
		}
		return nil
	}
	checkText := func(location, value string) error {
		if uint64(len(value)) > limits.MaxRichTextBytes {
			return &PackageError{Code: ErrorArchiveLimit, Operation: "validate candidate workbook", Limit: "rich_text_bytes", Actual: uint64(len(value)), Maximum: limits.MaxRichTextBytes, Detail: location}
		}
		return nil
	}
	checkRawFields := func(location string, fields []RawField) error {
		for index, field := range fields {
			if err := checkText(fmt.Sprintf("%s/raw_fields/%d", location, index), string(field.JSON)+field.XML); err != nil {
				return err
			}
		}
		return nil
	}
	for sheetIndex := range workbook.Sheets {
		sheet := &workbook.Sheets[sheetIndex]
		sheetLocation := fmt.Sprintf("/sheets/%d", sheetIndex)
		if err := addElement(sheetLocation); err != nil {
			return err
		}
		if err := checkText(sheetLocation+"/title", sheet.Title); err != nil {
			return err
		}
		if err := checkRawFields(sheetLocation, sheet.RawFields); err != nil {
			return err
		}
		queue := append([]*Topic(nil), sheet.Roots...)
		for len(queue) > 0 {
			topic := queue[0]
			queue = queue[1:]
			if topic == nil {
				continue
			}
			location := sheetLocation + "/topics/" + topic.ID
			if err := addElement(location); err != nil {
				return err
			}
			for _, item := range []struct{ name, value string }{
				{name: "title", value: topic.RawTitle},
				{name: "notes_plain", value: topic.Notes.Plain},
				{name: "notes_xml", value: topic.Notes.RawXML},
				{name: "task_owner", value: topic.Task.Owner},
			} {
				if err := checkText(location+"/"+item.name, item.value); err != nil {
					return err
				}
			}
			if err := checkText(location+"/notes_raw", string(topic.Notes.Raw)); err != nil {
				return err
			}
			for index, label := range topic.Labels {
				if err := checkText(fmt.Sprintf("%s/labels/%d", location, index), label); err != nil {
					return err
				}
			}
			for index, marker := range topic.Markers {
				if err := checkText(fmt.Sprintf("%s/markers/%d", location, index), marker.ResolvedName); err != nil {
					return err
				}
			}
			for index, link := range topic.Links {
				if err := checkText(fmt.Sprintf("%s/links/%d", location, index), link.Label+link.Target); err != nil {
					return err
				}
			}
			if err := checkRawFields(location, topic.RawFields); err != nil {
				return err
			}
			queue = append(queue, topic.Children...)
		}
		for index, relationship := range sheet.Relationships {
			location := fmt.Sprintf("%s/relationships/%d", sheetLocation, index)
			if err := addElement(location); err != nil {
				return err
			}
			if err := checkText(location+"/label", relationship.Label); err != nil {
				return err
			}
			if err := checkText(location+"/style", string(relationship.Style)); err != nil {
				return err
			}
			if err := checkRawFields(location, relationship.RawFields); err != nil {
				return err
			}
		}
		for index, element := range sheet.Elements {
			location := fmt.Sprintf("%s/elements/%d", sheetLocation, index)
			if err := addElement(location); err != nil {
				return err
			}
			if err := checkText(location+"/title", element.Title); err != nil {
				return err
			}
			if err := checkText(location+"/raw", string(element.Raw)); err != nil {
				return err
			}
			if err := checkRawFields(location, element.RawFields); err != nil {
				return err
			}
		}
	}
	for index, asset := range workbook.Assets {
		location := fmt.Sprintf("/assets/%d", index)
		if err := addElement(location); err != nil {
			return err
		}
		if err := checkText(location, asset.OriginalName+asset.Caption+asset.AltText+asset.URI); err != nil {
			return err
		}
		if err := checkRawFields(location, asset.RawFields); err != nil {
			return err
		}
	}
	for index, feature := range workbook.FeatureInventory.Features {
		if err := checkText(fmt.Sprintf("/feature_inventory/%d/raw", index), string(feature.Raw)); err != nil {
			return err
		}
	}
	return nil
}

type candidateState struct {
	limits          Limits
	inventory       FeatureInventory
	resourceIndex   map[string]int
	assetIndex      map[string]int
	assets          []Asset
	elements        uint64
	pendingElements []Element
}

func newCandidateState(detection PackageDetection, limits Limits) *candidateState {
	state := &candidateState{
		limits:        limits,
		inventory:     detection.Inventory,
		resourceIndex: make(map[string]int, len(detection.Inventory.Resources)),
		assetIndex:    make(map[string]int),
	}
	for index, resource := range state.inventory.Resources {
		state.resourceIndex[resource.Path] = index
	}
	return state
}

func (s *candidateState) addElement(location string, depth int) error {
	if depth < 0 || uint64(depth) > s.limits.MaxDepth {
		return &PackageError{Code: ErrorArchiveLimit, Operation: "normalize candidate workbook", Limit: "normalized_depth", Actual: uint64(max(depth, 0)), Maximum: s.limits.MaxDepth, Detail: location}
	}
	s.elements++
	if s.elements > s.limits.MaxNormalizedElements {
		return &PackageError{Code: ErrorArchiveLimit, Operation: "normalize candidate workbook", Limit: "normalized_elements", Actual: s.elements, Maximum: s.limits.MaxNormalizedElements, Detail: location}
	}
	return nil
}

func (s *candidateState) feature(feature, name, id, location string, status InventoryStatus, reason string, raw json.RawMessage) {
	s.inventory.Features = append(s.inventory.Features, FeatureEncounter{
		Feature: feature, SourceName: name, SourceElementID: id, Location: location,
		Status: status, Count: 1, ReasonCode: reason, Raw: cloneRaw(raw),
	})
}

func (s *candidateState) updateFeatureDisposition(feature, sourceElementID string, status InventoryStatus, reason string) {
	s.updateFeatureDispositionDetail(feature, sourceElementID, status, reason, "")
}

func (s *candidateState) updateFeatureDispositionDetail(feature, sourceElementID string, status InventoryStatus, reason, detail string) {
	for index := len(s.inventory.Features) - 1; index >= 0; index-- {
		item := &s.inventory.Features[index]
		if item.Feature == feature && item.SourceElementID == sourceElementID {
			item.Status = status
			item.ReasonCode = reason
			item.Detail = detail
			return
		}
	}
}

func (s *candidateState) addAsset(topic *Topic, kind AssetKind, source, role, location string, width, height float64, rawFields []RawField) {
	if source == "" {
		return
	}
	assetKey := string(kind) + "\x00" + source
	digest := sha256.Sum256([]byte(assetKey))
	assetID := "xmind-asset-" + hex.EncodeToString(digest[:16])
	assetIndex, exists := s.assetIndex[assetID]
	if !exists {
		asset := Asset{ID: assetID, Kind: kind, URI: source, OriginalName: path.Base(source), Status: InventoryPreserved, RawFields: rawFields}
		parsedURL, parseErr := url.Parse(source)
		external := parseErr == nil && parsedURL.Scheme != "" && parsedURL.Scheme != "xap"
		if strings.HasPrefix(source, "//") || strings.HasPrefix(source, `\\`) {
			external = true
		}
		if external {
			asset.OriginalName = path.Base(parsedURL.Path)
			s.feature("external_media", source, assetID, location, InventoryPreserved, "external_reference_not_fetched", nil)
		} else {
			resourcePath := strings.TrimPrefix(source, "xap:")
			resourcePath = strings.TrimPrefix(resourcePath, "/")
			asset.ResourcePath = resourcePath
			asset.OriginalName = path.Base(resourcePath)
			normalizedPath, pathErr := safeArchivePath(resourcePath)
			if pathErr != nil {
				asset.Status = InventoryRejected
				s.feature("media", source, assetID, location, InventoryRejected, "unsafe_package_resource_path", nil)
			} else if resourceIndex, ok := s.resourceIndex[normalizedPath]; ok {
				asset.ResourcePath = normalizedPath
				resource := &s.inventory.Resources[resourceIndex]
				asset.SHA256 = resource.SHA256
				asset.SizeBytes = resource.UncompressedBytes
				asset.MediaType = resource.DetectedMIME
				s.feature("media", source, assetID, location, InventoryPreserved, "package_resource", nil)
			} else {
				asset.Status = InventoryRejected
				s.feature("media", source, assetID, location, InventoryRejected, "missing_package_resource", nil)
			}
		}
		s.assets = append(s.assets, asset)
		assetIndex = len(s.assets) - 1
		s.assetIndex[assetID] = assetIndex
	}
	asset := &s.assets[assetIndex]
	if asset.ResourcePath != "" && asset.SHA256 != "" && asset.Status != InventoryRejected {
		if resourceIndex, ok := s.resourceIndex[asset.ResourcePath]; ok {
			resource := &s.inventory.Resources[resourceIndex]
			resource.Referenced = true
			resource.ReferenceCount++
			resource.OwningTopicIDs = appendUnique(resource.OwningTopicIDs, topic.ID)
			resource.Roles = appendUnique(resource.Roles, role)
		}
	}
	topic.Assets = append(topic.Assets, AssetRef{AssetID: asset.ID, Role: role, Ordinal: len(topic.Assets), Width: width, Height: height, RawFields: rawFields})
}

func appendUnique(values []string, value string) []string {
	for _, current := range values {
		if current == value {
			return values
		}
	}
	return append(values, value)
}

func readPackageEntry(reader io.ReaderAt, size int64, name string) ([]byte, error) {
	zr, err := zip.NewReader(reader, size)
	if err != nil {
		return nil, err
	}
	for _, file := range zr.File {
		if file.Name != name || file.FileInfo().IsDir() {
			continue
		}
		stream, err := file.Open()
		if err != nil {
			return nil, err
		}
		data, readErr := io.ReadAll(stream)
		closeErr := stream.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		return data, nil
	}
	return nil, fmt.Errorf("entry %q not found", name)
}

func invalidPayload(operation string, err error) *PackageError {
	return &PackageError{Code: ErrorInvalidPayload, Operation: operation, Err: err}
}

func validationError(detail string) *PackageError {
	return &PackageError{Code: ErrorValidation, Operation: "validate candidate workbook", Detail: detail}
}

func validateCandidateWorkbook(workbook *Workbook) error {
	if len(workbook.Sheets) == 0 {
		return validationError("workbook has no Sheets")
	}
	sheetIDs := make(map[string]struct{}, len(workbook.Sheets))
	topics := make(map[string]*Topic)
	topicSheetIDs := make(map[string]string)
	assets := make(map[string]struct{}, len(workbook.Assets))
	for _, asset := range workbook.Assets {
		if asset.ID == "" {
			return validationError("asset has no ID")
		}
		assets[asset.ID] = struct{}{}
	}
	for sheetIndex := range workbook.Sheets {
		sheet := &workbook.Sheets[sheetIndex]
		if sheet.ID == "" {
			return validationError(fmt.Sprintf("Sheet %d has no ID", sheetIndex))
		}
		if _, exists := sheetIDs[sheet.ID]; exists {
			return validationError(fmt.Sprintf("duplicate Sheet ID %q", sheet.ID))
		}
		sheetIDs[sheet.ID] = struct{}{}
		if len(sheet.Roots) == 0 {
			return validationError(fmt.Sprintf("Sheet %q has no roots", sheet.ID))
		}
		for _, root := range sheet.Roots {
			if root == nil || root.ParentID != "" || root.Depth != 0 {
				return validationError(fmt.Sprintf("Sheet %q has an invalid root Topic", sheet.ID))
			}
		}
		queue := append([]*Topic(nil), sheet.Roots...)
		for len(queue) > 0 {
			topic := queue[0]
			queue = queue[1:]
			if topic == nil || topic.ID == "" {
				return validationError(fmt.Sprintf("Sheet %q has a Topic without ID", sheet.ID))
			}
			if _, exists := topics[topic.ID]; exists {
				return validationError(fmt.Sprintf("duplicate workbook-scoped Topic ID %q in Sheets %q and %q; normal XMind workbooks use unique Topic IDs, so resave the workbook before importing", topic.ID, topicSheetIDs[topic.ID], sheet.ID))
			}
			topics[topic.ID] = topic
			topicSheetIDs[topic.ID] = sheet.ID
			for _, ref := range topic.Assets {
				if _, ok := assets[ref.AssetID]; !ok {
					return validationError(fmt.Sprintf("Topic %q references missing asset %q", topic.ID, ref.AssetID))
				}
			}
			for _, child := range topic.Children {
				if child.ParentID != topic.ID || child.Depth != topic.Depth+1 {
					return validationError(fmt.Sprintf("Topic %q has inconsistent parent/depth", child.ID))
				}
			}
			queue = append(queue, topic.Children...)
		}
	}
	relationshipIDs := make(map[string]struct{})
	for sheetIndex := range workbook.Sheets {
		sheet := &workbook.Sheets[sheetIndex]
		for _, relationship := range sheet.Relationships {
			if relationship.ID == "" {
				return validationError(fmt.Sprintf("Sheet %q has Relationship without ID", sheet.ID))
			}
			if _, exists := relationshipIDs[relationship.ID]; exists {
				return validationError(fmt.Sprintf("duplicate Relationship ID %q", relationship.ID))
			}
			relationshipIDs[relationship.ID] = struct{}{}
			if _, ok := topics[relationship.SourceTopicID]; !ok {
				return validationError(fmt.Sprintf("Relationship %q has missing source Topic %q", relationship.ID, relationship.SourceTopicID))
			}
			if _, ok := topics[relationship.TargetTopicID]; !ok {
				return validationError(fmt.Sprintf("Relationship %q has missing target Topic %q", relationship.ID, relationship.TargetTopicID))
			}
		}
		elementIDs := make(map[string]struct{})
		for _, element := range sheet.Elements {
			if element.ID == "" {
				return validationError(fmt.Sprintf("Sheet %q has an Element without ID", sheet.ID))
			}
			if _, exists := elementIDs[element.ID]; exists {
				return validationError(fmt.Sprintf("duplicate Element ID %q in Sheet %q", element.ID, sheet.ID))
			}
			elementIDs[element.ID] = struct{}{}
			if element.OwnerTopicID != "" {
				if _, ok := topics[element.OwnerTopicID]; !ok {
					return validationError(fmt.Sprintf("Element %q has missing owner Topic %q", element.ID, element.OwnerTopicID))
				}
			}
			for _, targetID := range element.TargetIDs {
				if targetID != "" {
					if _, ok := topics[targetID]; !ok {
						return validationError(fmt.Sprintf("Element %q targets missing Topic %q", element.ID, targetID))
					}
				}
			}
		}
	}
	return nil
}

func hashReaderAt(reader io.ReaderAt, size int64) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(reader, 0, size)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func semanticHash(workbook Workbook) string {
	h := sha256.New()
	hashFields(h, string(workbook.FormatFamily), workbook.FormatVersion)
	for sheetIndex := range workbook.Sheets {
		sheet := &workbook.Sheets[sheetIndex]
		hashFields(h, "sheet", sheet.ID, sheet.Title)
		queue := append([]*Topic(nil), sheet.Roots...)
		for len(queue) > 0 {
			topic := queue[0]
			queue = queue[1:]
			hashFields(h, "topic", topic.ID, topic.ParentID, topic.Kind, topic.ChildRole, topic.RawTitle, strconv.Itoa(topic.Depth), strconv.Itoa(topic.SiblingOrdinal), fmt.Sprint(topic.OrderPath), topic.Notes.Plain)
			for _, label := range topic.Labels {
				hashFields(h, "label", label)
			}
			for _, marker := range topic.Markers {
				hashFields(h, "marker", marker.ID, marker.ResolvedName, marker.Group)
			}
			hashFields(h, "numbering", topic.Numbering.Format, topic.Numbering.Prefix, topic.Numbering.Suffix, topic.Numbering.PrependingNumbers, topic.Numbering.DisplayNumber)
			hashFields(h, "task", topic.Task.Status, topic.Task.Owner, topic.Task.Start, topic.Task.Due, strconv.FormatFloat(topic.Task.Progress, 'g', -1, 64))
			for _, link := range topic.Links {
				hashFields(h, "link", link.ID, link.Label, link.Target, link.TargetTopicID, link.TargetSheetID, strconv.FormatBool(link.External))
			}
			queue = append(queue, topic.Children...)
		}
		for _, relationship := range sheet.Relationships {
			hashFields(h, "relationship", relationship.ID, relationship.Label, relationship.SourceTopicID, relationship.TargetTopicID, relationship.StartArrow, relationship.EndArrow)
			for _, point := range relationship.ControlPoints {
				hashFields(h, point.Key, strconv.FormatFloat(point.Angle, 'g', -1, 64), strconv.FormatFloat(point.Amount, 'g', -1, 64), strconv.FormatFloat(point.X, 'g', -1, 64), strconv.FormatFloat(point.Y, 'g', -1, 64))
			}
		}
		for _, element := range sheet.Elements {
			hashFields(h, "element", element.ID, element.DefinitionID, element.Kind, element.Title, element.OwnerTopicID, element.Range, string(element.Status))
			for _, target := range element.TargetIDs {
				hashFields(h, target)
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func mediaManifestHash(workbook Workbook) string {
	h := sha256.New()
	assets := append([]Asset(nil), workbook.Assets...)
	sort.Slice(assets, func(i, j int) bool { return assets[i].ID < assets[j].ID })
	for _, asset := range assets {
		hashFields(h, asset.ID, string(asset.Kind), asset.ResourcePath, asset.URI, asset.OriginalName, asset.MediaType, asset.SHA256, strconv.FormatUint(asset.SizeBytes, 10), string(asset.Status))
	}
	for sheetIndex := range workbook.Sheets {
		queue := append([]*Topic(nil), workbook.Sheets[sheetIndex].Roots...)
		for len(queue) > 0 {
			topic := queue[0]
			queue = queue[1:]
			for _, ref := range topic.Assets {
				hashFields(h, topic.ID, ref.AssetID, ref.Role, strconv.Itoa(ref.Ordinal), strconv.FormatFloat(ref.Width, 'g', -1, 64), strconv.FormatFloat(ref.Height, 'g', -1, 64), string(ref.Crop), ref.Align)
			}
			queue = append(queue, topic.Children...)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func hashFields(h hash.Hash, fields ...string) {
	for _, field := range fields {
		_, _ = io.WriteString(h, strconv.Itoa(len(field)))
		_, _ = io.WriteString(h, ":")
		_, _ = io.WriteString(h, field)
	}
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), raw...)
}

func max(left, right int) int {
	if left > right {
		return left
	}
	return right
}
