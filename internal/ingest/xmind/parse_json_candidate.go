package xmind

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type jsonTopicWork struct {
	raw         json.RawMessage
	parent      *Topic
	role        string
	depth       int
	ordinal     int
	orderPath   []int
	location    string
	rootOrdinal int
}

func parseJSONCandidate(payload []byte, state *candidateState) (Workbook, error) {
	var rawSheets []json.RawMessage
	if err := json.Unmarshal(payload, &rawSheets); err != nil {
		return Workbook{}, invalidPayload("decode official-schema JSON candidate", err)
	}
	if len(rawSheets) == 0 {
		return Workbook{}, invalidPayload("decode official-schema JSON candidate", fmt.Errorf("content.json has no Sheets"))
	}
	workbook := Workbook{FormatFamily: FormatJSONCandidate, FormatVersion: "unverified-official-schema-candidate", Sheets: make([]Sheet, 0, len(rawSheets))}
	for sheetIndex, rawSheet := range rawSheets {
		sheet, err := parseJSONSheet(rawSheet, sheetIndex, state)
		if err != nil {
			return Workbook{}, err
		}
		workbook.Sheets = append(workbook.Sheets, sheet)
	}
	return workbook, nil
}

func parseJSONSheet(raw json.RawMessage, sheetIndex int, state *candidateState) (Sheet, error) {
	location := fmt.Sprintf("/%d", sheetIndex)
	object, err := jsonObject(raw, location)
	if err != nil {
		return Sheet{}, err
	}
	id, err := requiredJSONString(object, "id", location)
	if err != nil {
		return Sheet{}, err
	}
	title, err := optionalJSONString(object, "title", location)
	if err != nil {
		state.feature("sheet_title", "title", id, location+"/title", InventoryUnsupported, "invalid_optional_text", object["title"])
		title = ""
	}
	rootRaw, ok := object["rootTopic"]
	if !ok || string(rootRaw) == "null" {
		return Sheet{}, invalidPayload("decode JSON Sheet", fmt.Errorf("%s/rootTopic is required", location))
	}
	if err := state.addElement(location, 0); err != nil {
		return Sheet{}, err
	}
	sheet := Sheet{ID: id, Title: title}
	pendingElementStart := len(state.pendingElements)
	state.feature("sheet", "sheet", id, location, InventoryIndexed, "", nil)
	markerNames := make(map[string]string)
	if rawLegend, ok := object["legend"]; ok {
		legendLocation := location + "/legend"
		sheet.RawFields = append(sheet.RawFields, RawField{Name: "legend", Location: legendLocation, Encoding: "json", JSON: cloneRaw(rawLegend)})
		var legendErr error
		markerNames, legendErr = parseJSONLegend(rawLegend)
		if legendErr != nil {
			state.inventory.Features = append(state.inventory.Features, FeatureEncounter{
				Feature: "legend", SourceName: "legend", SourceElementID: id, Location: legendLocation,
				Status: InventoryUnsupported, Count: 1, ReasonCode: "invalid_legend",
				Detail: legendErr.Error(), Raw: cloneRaw(rawLegend),
			})
		} else {
			state.feature("legend", "legend", id, legendLocation, InventoryIndexed, "", rawLegend)
		}
	}
	for _, key := range []string{"class", "revisionId", "style", "topicPositioning", "topicOverlapping", "theme", "settings", "arrangeableLayerOrder", "labelSortOrder", "extensions", "zones"} {
		raw, ok := object[key]
		if !ok {
			continue
		}
		itemLocation := location + "/" + pointerEscape(key)
		sheet.RawFields = append(sheet.RawFields, RawField{Name: key, Location: itemLocation, Encoding: "json", JSON: cloneRaw(raw)})
		switch key {
		case "extensions":
			state.feature("extension", key, id, itemLocation, InventoryPreserved, "official_extension_schema_is_open", raw)
		case "theme":
			state.feature("theme", key, id, itemLocation, InventoryPreserved, "presentation_source_not_semantically_indexed", raw)
		case "zones":
			var zones []json.RawMessage
			if err := json.Unmarshal(raw, &zones); err != nil {
				state.feature("zone", "zones", id, itemLocation, InventoryUnsupported, "invalid_zone_collection", raw)
				continue
			}
			for index, zone := range zones {
				state.feature("zone", "zone", "", fmt.Sprintf("%s/%d", itemLocation, index), InventoryUnsupported, "zone_encoding_requires_real_feature_fixture", zone)
			}
		default:
			state.feature("sheet_source_field", key, id, itemLocation, InventoryPreserved, "source_provenance_not_semantically_indexed", raw)
		}
	}
	recordUnknownJSON(state, object, jsonKeys("id", "title", "rootTopic", "class", "revisionId", "style", "topicPositioning", "topicOverlapping", "theme", "relationships", "legend", "settings", "arrangeableLayerOrder", "labelSortOrder", "extensions", "zones"), location, &sheet.RawFields)

	queue := []jsonTopicWork{{raw: rootRaw, role: "central", depth: 0, ordinal: 0, orderPath: []int{0}, location: location + "/rootTopic", rootOrdinal: 0}}
	nextRootOrdinal := 1
	for cursor := 0; cursor < len(queue); cursor++ {
		work := queue[cursor]
		topic, childGroups, err := parseJSONTopic(work, markerNames, state)
		if err != nil {
			return Sheet{}, err
		}
		if work.parent == nil {
			sheet.Roots = append(sheet.Roots, topic)
		} else {
			work.parent.Children = append(work.parent.Children, topic)
		}
		if work.role == "callout" {
			sheet.Elements = append(sheet.Elements, Element{ID: topic.ID, Kind: work.role, Title: topic.RawTitle, OwnerTopicID: topic.ParentID, Status: InventoryIndexed})
		}

		childOrdinal := 0
		for _, role := range []string{"attached", "summary", "callout"} {
			children := childGroups[role]
			for index, childRaw := range children {
				childPath := appendCopy(topic.OrderPath, childOrdinal)
				queue = append(queue, jsonTopicWork{raw: childRaw, parent: topic, role: role, depth: topic.Depth + 1, ordinal: childOrdinal, orderPath: childPath, location: fmt.Sprintf("%s/children/%s/%d", work.location, role, index)})
				childOrdinal++
			}
		}
		for index, childRaw := range childGroups["detached"] {
			rootOrdinal := nextRootOrdinal
			nextRootOrdinal++
			queue = append(queue, jsonTopicWork{raw: childRaw, role: "detached", depth: 0, ordinal: rootOrdinal, rootOrdinal: rootOrdinal, orderPath: []int{rootOrdinal}, location: fmt.Sprintf("%s/children/detached/%d", work.location, index)})
		}
	}
	if err := resolveJSONSummaryElements(sheet.Roots, state.pendingElements[pendingElementStart:], state); err != nil {
		return Sheet{}, err
	}

	if relationshipsRaw, ok := object["relationships"]; ok {
		var rawRelationships []json.RawMessage
		if err := json.Unmarshal(relationshipsRaw, &rawRelationships); err != nil {
			itemLocation := location + "/relationships"
			sheet.RawFields = append(sheet.RawFields, RawField{Name: "relationships", Location: itemLocation, Encoding: "json", JSON: cloneRaw(relationshipsRaw)})
			state.inventory.Features = append(state.inventory.Features, FeatureEncounter{
				Feature: "relationships", SourceName: "relationships", SourceElementID: sheet.ID,
				Location: itemLocation, Status: InventoryRejected, Count: 1,
				ReasonCode: "invalid_relationship_collection", Detail: err.Error(), Raw: cloneRaw(relationshipsRaw),
			})
		} else {
			for index, rawRelationship := range rawRelationships {
				itemLocation := fmt.Sprintf("%s/relationships/%d", location, index)
				relationship, err := parseJSONRelationship(rawRelationship, itemLocation, state)
				if err != nil {
					sheet.RawFields = append(sheet.RawFields, RawField{Name: "relationship", Location: itemLocation, Encoding: "json", JSON: cloneRaw(rawRelationship)})
					state.inventory.Features = append(state.inventory.Features, FeatureEncounter{
						Feature: "relationship", SourceName: "relationship", Location: itemLocation,
						Status: InventoryRejected, Count: 1, ReasonCode: "invalid_relationship",
						Detail: err.Error(), Raw: cloneRaw(rawRelationship),
					})
					continue
				}
				sheet.Relationships = append(sheet.Relationships, relationship)
			}
		}
	}
	sheet.Elements = append(sheet.Elements, state.pendingElements[pendingElementStart:]...)
	return sheet, nil
}

func parseJSONTopic(work jsonTopicWork, markerNames map[string]string, state *candidateState) (*Topic, map[string][]json.RawMessage, error) {
	if err := state.addElement(work.location, work.depth); err != nil {
		return nil, nil, err
	}
	object, err := jsonObject(work.raw, work.location)
	if err != nil {
		return nil, nil, err
	}
	id, err := requiredJSONString(object, "id", work.location)
	if err != nil {
		return nil, nil, err
	}
	title, err := optionalJSONString(object, "title", work.location)
	if err != nil {
		state.feature("topic_title", "title", id, work.location+"/title", InventoryUnsupported, "invalid_optional_text", object["title"])
		title = ""
	}
	kind := "topic"
	if work.role == "central" {
		kind = "central"
	} else if work.role == "detached" {
		kind = "floating"
	} else if work.role == "summary" || work.role == "callout" {
		kind = work.role
	}
	parentID := ""
	if work.parent != nil {
		parentID = work.parent.ID
	}
	topic := &Topic{ID: id, ParentID: parentID, Kind: kind, ChildRole: work.role, RawTitle: title, Depth: work.depth, SiblingOrdinal: work.ordinal, OrderPath: append([]int(nil), work.orderPath...)}
	state.feature("topic", work.role, id, work.location, InventoryIndexed, "", nil)

	if rawNotes, ok := object["notes"]; ok {
		topic.Notes, err = parseJSONNotes(rawNotes, work.location+"/notes")
		if err != nil {
			preserveInvalidTopicFeature(state, topic, "notes", id, work.location+"/notes", rawNotes, err)
		} else {
			state.feature("notes", "notes", id, work.location+"/notes", InventoryIndexed, "", nil)
		}
	}
	if rawLabels, ok := object["labels"]; ok {
		topic.Labels, err = parseJSONLabels(rawLabels, work.location+"/labels")
		if err != nil {
			preserveInvalidTopicFeature(state, topic, "labels", id, work.location+"/labels", rawLabels, err)
		} else {
			for range topic.Labels {
				state.feature("label", "label", id, work.location+"/labels", InventoryIndexed, "", nil)
			}
		}
	}
	if rawMarkers, ok := object["markers"]; ok {
		var markerObjects []map[string]json.RawMessage
		if err := json.Unmarshal(rawMarkers, &markerObjects); err != nil {
			preserveInvalidTopicFeature(state, topic, "markers", id, work.location+"/markers", rawMarkers, err)
		} else {
			for index, markerObject := range markerObjects {
				markerLocation := fmt.Sprintf("%s/markers/%d", work.location, index)
				markerID, markerErr := requiredJSONString(markerObject, "markerId", markerLocation)
				if markerErr != nil {
					rawMarker, _ := json.Marshal(markerObject)
					preserveInvalidTopicFeature(state, topic, "marker", id, markerLocation, rawMarker, markerErr)
					continue
				}
				topic.Markers = append(topic.Markers, Marker{ID: markerID, ResolvedName: markerNames[markerID]})
				state.feature("marker", markerID, id, markerLocation, InventoryIndexed, "", nil)
			}
		}
	}
	if rawNumbering, ok := object["numbering"]; ok {
		var supported bool
		topic.Numbering, supported, err = parseJSONNumbering(rawNumbering, work.location+"/numbering", work.parent, work.ordinal)
		if err != nil {
			topic.Numbering = Numbering{Raw: cloneRaw(rawNumbering)}
			preserveInvalidTopicFeature(state, topic, "numbering", id, work.location+"/numbering", rawNumbering, err)
		} else if supported {
			state.feature("numbering", topic.Numbering.Format, id, work.location+"/numbering", InventoryIndexed, "", nil)
		} else {
			state.feature("numbering", topic.Numbering.Format, id, work.location+"/numbering", InventoryUnsupported, "numbering_semantics_not_implemented", rawNumbering)
		}
	}
	if rawPosition, ok := object["position"]; ok {
		positionObject, err := jsonObject(rawPosition, work.location+"/position")
		if err != nil {
			topic.Position = Position{Floating: work.role == "detached", Raw: cloneRaw(rawPosition)}
			preserveInvalidTopicFeature(state, topic, "position", id, work.location+"/position", rawPosition, err)
		} else {
			x, xErr := optionalJSONFloat(positionObject, "x", work.location+"/position")
			y, yErr := optionalJSONFloat(positionObject, "y", work.location+"/position")
			if xErr != nil || yErr != nil {
				topic.Position = Position{Floating: work.role == "detached", Raw: cloneRaw(rawPosition)}
				preserveInvalidTopicFeature(state, topic, "position", id, work.location+"/position", rawPosition, firstError(xErr, yErr))
			} else {
				topic.Position.X = x
				topic.Position.Y = y
				topic.Position.Floating = work.role == "detached"
				topic.Position.Raw = cloneRaw(rawPosition)
			}
		}
	}
	if href, err := optionalJSONString(object, "href", work.location); err != nil {
		preserveInvalidTopicFeature(state, topic, "topic_link", id, work.location+"/href", object["href"], err)
	} else if href != "" {
		if strings.HasPrefix(href, "xap:") {
			state.addAsset(topic, AssetAttachment, href, "attachment", work.location+"/href", 0, 0, nil)
		} else {
			link := parseTopicLink(href)
			topic.Links = append(topic.Links, link)
			state.feature("topic_link", href, id, work.location+"/href", InventoryIndexed, "", nil)
		}
	}
	if rawImage, ok := object["image"]; ok {
		imageObject, err := jsonObject(rawImage, work.location+"/image")
		if err != nil {
			preserveInvalidTopicFeature(state, topic, "image", id, work.location+"/image", rawImage, err)
		} else {
			src, srcErr := requiredJSONString(imageObject, "src", work.location+"/image")
			width, widthErr := optionalJSONFloat(imageObject, "width", work.location+"/image")
			height, heightErr := optionalJSONFloat(imageObject, "height", work.location+"/image")
			align, alignErr := optionalJSONString(imageObject, "align", work.location+"/image")
			if featureErr := firstError(srcErr, widthErr, heightErr, alignErr); featureErr != nil {
				preserveInvalidTopicFeature(state, topic, "image", id, work.location+"/image", rawImage, featureErr)
			} else {
				refRawFields := []RawField{{Name: "image", Location: work.location + "/image", Encoding: "json", JSON: cloneRaw(rawImage)}}
				state.addAsset(topic, AssetImage, src, "image", work.location+"/image", width, height, refRawFields)
				ref := &topic.Assets[len(topic.Assets)-1]
				ref.Align = align
				if crop, ok := imageObject["crop"]; ok {
					ref.Crop = cloneRaw(crop)
				}
				recordUnknownJSON(state, imageObject, jsonKeys("src", "width", "height", "align", "crop"), work.location+"/image", &topic.RawFields)
			}
		}
	}
	if rawTask, ok := firstRaw(object, "task", "taskInfo"); ok {
		topic.Task = parseJSONTask(rawTask)
		topic.Task.Raw = cloneRaw(rawTask)
		state.feature("task", "unverified_task_extension", id, work.location+"/task", InventoryPreserved, "official_extension_schema_is_open", rawTask)
	}

	for _, key := range []string{"style", "class", "structureClass", "branch", "customWidth", "titleUnedited", "extensions"} {
		if raw, ok := object[key]; ok {
			topic.RawFields = append(topic.RawFields, RawField{Name: key, Location: work.location + "/" + pointerEscape(key), Encoding: "json", JSON: cloneRaw(raw)})
			if key == "extensions" {
				state.feature("extension", "extensions", id, work.location+"/extensions", InventoryPreserved, "official_extension_schema_is_open", raw)
			}
		}
	}
	if rawBoundaries, ok := object["boundaries"]; ok {
		if err := parseJSONBoundaries(rawBoundaries, topic, work.location+"/boundaries", state); err != nil {
			preserveInvalidTopicFeature(state, topic, "boundaries", id, work.location+"/boundaries", rawBoundaries, err)
		}
	}
	if rawSummaries, ok := object["summaries"]; ok {
		if err := parseJSONSummaries(rawSummaries, topic, work.location+"/summaries", state); err != nil {
			preserveInvalidTopicFeature(state, topic, "summaries", id, work.location+"/summaries", rawSummaries, err)
		}
	}

	childGroups := make(map[string][]json.RawMessage)
	if rawChildren, ok := object["children"]; ok {
		childrenObject, err := jsonObject(rawChildren, work.location+"/children")
		if err != nil {
			return nil, nil, err
		}
		for _, role := range []string{"attached", "detached", "summary", "callout"} {
			if rawGroup, ok := childrenObject[role]; ok {
				var group []json.RawMessage
				if err := json.Unmarshal(rawGroup, &group); err != nil {
					return nil, nil, invalidPayload("decode JSON Topic children", fmt.Errorf("%s/children/%s: %w", work.location, role, err))
				}
				childGroups[role] = group
			}
		}
		recordUnknownJSON(state, childrenObject, jsonKeys("attached", "detached", "summary", "callout"), work.location+"/children", &topic.RawFields)
	}
	recordUnknownJSON(state, object, jsonKeys("id", "title", "titleUnedited", "style", "class", "position", "structureClass", "branch", "customWidth", "labels", "numbering", "href", "notes", "image", "children", "markers", "boundaries", "summaries", "extensions", "task", "taskInfo"), work.location, &topic.RawFields)
	return topic, childGroups, nil
}

func preserveInvalidTopicFeature(state *candidateState, topic *Topic, feature, topicID, location string, raw json.RawMessage, err error) {
	topic.RawFields = append(topic.RawFields, RawField{Name: feature, Location: location, Encoding: "json", JSON: cloneRaw(raw)})
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	state.inventory.Features = append(state.inventory.Features, FeatureEncounter{
		Feature: feature, SourceName: feature, SourceElementID: topicID, Location: location,
		Status: InventoryUnsupported, Count: 1, ReasonCode: "invalid_optional_feature",
		Detail: detail, Raw: cloneRaw(raw),
	})
}

func firstError(errors ...error) error {
	for _, err := range errors {
		if err != nil {
			return err
		}
	}
	return nil
}

func parseJSONRelationship(raw json.RawMessage, location string, state *candidateState) (Relationship, error) {
	object, err := jsonObject(raw, location)
	if err != nil {
		return Relationship{}, err
	}
	id, err := requiredJSONString(object, "id", location)
	if err != nil {
		return Relationship{}, err
	}
	end1, err := requiredJSONString(object, "end1Id", location)
	if err != nil {
		return Relationship{}, err
	}
	end2, err := optionalJSONString(object, "end2Id", location)
	if err != nil {
		return Relationship{}, err
	}
	if end2 == "" {
		end2, err = requiredJSONString(object, "end2id", location)
		if err != nil {
			return Relationship{}, err
		}
	}
	label, err := optionalJSONString(object, "title", location)
	if err != nil {
		state.feature("relationship_label", "title", id, location+"/title", InventoryUnsupported, "invalid_relationship_label", object["title"])
		label = ""
	}
	relationship := Relationship{ID: id, Label: label, SourceTopicID: end1, TargetTopicID: end2, SemanticDirection: "undirected"}
	rawControlPoints, ok := object["controlPoints"]
	if ok {
		var points map[string]json.RawMessage
		if err := json.Unmarshal(rawControlPoints, &points); err != nil {
			relationship.RawFields = append(relationship.RawFields, RawField{Name: "controlPoints", Location: location + "/controlPoints", Encoding: "json", JSON: cloneRaw(rawControlPoints)})
			state.feature("relationship_geometry", "controlPoints", id, location+"/controlPoints", InventoryUnsupported, "invalid_control_points", rawControlPoints)
		} else {
			keys := make([]string, 0, len(points))
			for key := range points {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				pointLocation := location + "/controlPoints/" + key
				pointObject, pointErr := jsonObject(points[key], pointLocation)
				if pointErr != nil {
					relationship.RawFields = append(relationship.RawFields, RawField{Name: "controlPoint", Location: pointLocation, Encoding: "json", JSON: cloneRaw(points[key])})
					state.feature("relationship_geometry", "controlPoint", id, pointLocation, InventoryUnsupported, "invalid_control_point", points[key])
					continue
				}
				angle, angleErr := optionalJSONFloat(pointObject, "angle", pointLocation)
				amount, amountErr := optionalJSONFloat(pointObject, "amount", pointLocation)
				x, xErr := optionalJSONFloat(pointObject, "x", pointLocation)
				y, yErr := optionalJSONFloat(pointObject, "y", pointLocation)
				if angleErr != nil || amountErr != nil || xErr != nil || yErr != nil {
					relationship.RawFields = append(relationship.RawFields, RawField{Name: "controlPoint", Location: pointLocation, Encoding: "json", JSON: cloneRaw(points[key])})
					state.feature("relationship_geometry", "controlPoint", id, pointLocation, InventoryUnsupported, "invalid_control_point_number", points[key])
					continue
				}
				relationship.ControlPoints = append(relationship.ControlPoints, Point{Key: key, Angle: angle, Amount: amount, X: x, Y: y, Raw: cloneRaw(points[key])})
			}
		}
	}
	if rawStyle, ok := object["style"]; ok {
		relationship.Style = cloneRaw(rawStyle)
		var validStyle bool
		relationship.StartArrow, relationship.EndArrow, validStyle = jsonRelationshipArrows(rawStyle)
		if !validStyle {
			relationship.RawFields = append(relationship.RawFields, RawField{Name: "style", Location: location + "/style", Encoding: "json", JSON: cloneRaw(rawStyle)})
			state.feature("relationship_style", "style", id, location+"/style", InventoryUnsupported, "invalid_relationship_style", rawStyle)
		}
	}
	recordUnknownJSON(state, object, jsonKeys("id", "title", "titleUnedited", "style", "class", "end1Id", "end2Id", "end2id", "controlPoints"), location, &relationship.RawFields)
	state.feature("relationship", "relationship", id, location, InventoryIndexed, "", nil)
	return relationship, nil
}

func parseJSONBoundaries(raw json.RawMessage, topic *Topic, location string, state *candidateState) error {
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return invalidPayload("decode JSON Boundaries", err)
	}
	for index, value := range values {
		itemLocation := fmt.Sprintf("%s/%d", location, index)
		object, err := jsonObject(value, itemLocation)
		if err != nil {
			return err
		}
		id, err := requiredJSONString(object, "id", itemLocation)
		if err != nil {
			return err
		}
		rangeValue, err := requiredJSONString(object, "range", itemLocation)
		if err != nil {
			return err
		}
		title, _ := optionalJSONString(object, "title", itemLocation)
		state.feature("boundary", "boundary", id, itemLocation, InventoryPreserved, "range_resolution_fixture_required", value)
		state.pendingElements = append(state.pendingElements, Element{ID: id, Kind: "boundary", Title: title, OwnerTopicID: topic.ID, Range: rangeValue, Status: InventoryPreserved, Raw: cloneRaw(value)})
	}
	return nil
}

func parseJSONSummaries(raw json.RawMessage, topic *Topic, location string, state *candidateState) error {
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return invalidPayload("decode JSON Summaries", err)
	}
	for index, value := range values {
		itemLocation := fmt.Sprintf("%s/%d", location, index)
		object, err := jsonObject(value, itemLocation)
		if err != nil {
			return err
		}
		id, err := requiredJSONString(object, "id", itemLocation)
		if err != nil {
			return err
		}
		rangeValue, err := requiredJSONString(object, "range", itemLocation)
		if err != nil {
			return err
		}
		topicID, err := requiredJSONString(object, "topicId", itemLocation)
		if err != nil {
			return err
		}
		state.feature("summary", "summary", id, itemLocation, InventoryPreserved, "summary_range_pending_resolution", value)
		state.pendingElements = append(state.pendingElements, Element{ID: topicID, DefinitionID: id, Kind: "summary", OwnerTopicID: topic.ID, Range: rangeValue, Status: InventoryPreserved, Raw: cloneRaw(value)})
	}
	return nil
}

func resolveJSONSummaryElements(roots []*Topic, elements []Element, state *candidateState) error {
	topics := make(map[string]*Topic)
	queue := append([]*Topic(nil), roots...)
	for len(queue) > 0 {
		topic := queue[0]
		queue = queue[1:]
		topics[topic.ID] = topic
		queue = append(queue, topic.Children...)
	}
	for index := range elements {
		element := &elements[index]
		if element.Kind != "summary" || element.DefinitionID == "" {
			continue
		}
		summaryTopic, ok := topics[element.ID]
		if !ok {
			missingTopicID := element.ID
			element.ID = element.DefinitionID
			element.Status = InventoryPreserved
			state.updateFeatureDispositionDetail("summary", element.DefinitionID, InventoryUnsupported, "missing_summary_topic", fmt.Sprintf("summary Topic %q is missing", missingTopicID))
			continue
		}
		owner, ok := topics[element.OwnerTopicID]
		if !ok {
			element.ID = element.DefinitionID
			element.Status = InventoryPreserved
			state.updateFeatureDispositionDetail("summary", element.DefinitionID, InventoryUnsupported, "missing_summary_owner", fmt.Sprintf("summary owner Topic %q is missing", element.OwnerTopicID))
			continue
		}
		element.Title = summaryTopic.RawTitle
		start, end, err := parseJSONSiblingRange(element.Range)
		if err != nil {
			state.updateFeatureDisposition("summary", element.DefinitionID, InventoryPreserved, "unsupported_summary_range")
			continue
		}
		attached := make([]*Topic, 0, len(owner.Children))
		for _, child := range owner.Children {
			if child.ChildRole == "attached" {
				attached = append(attached, child)
			}
		}
		if start >= len(attached) || end >= len(attached) {
			state.updateFeatureDisposition("summary", element.DefinitionID, InventoryPreserved, "summary_range_out_of_bounds")
			continue
		}
		for _, target := range attached[start : end+1] {
			element.TargetIDs = append(element.TargetIDs, target.ID)
		}
		element.Status = InventoryIndexed
		state.updateFeatureDisposition("summary", element.DefinitionID, InventoryIndexed, "")
	}
	return nil
}

func parseJSONSiblingRange(value string) (int, int, error) {
	value = strings.TrimSpace(value)
	if len(value) < 5 || value[0] != '(' || value[len(value)-1] != ')' {
		return 0, 0, fmt.Errorf("unsupported sibling range %q", value)
	}
	parts := strings.Split(strings.TrimSpace(value[1:len(value)-1]), ",")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("unsupported sibling range %q", value)
	}
	start, startErr := strconv.Atoi(strings.TrimSpace(parts[0]))
	end, endErr := strconv.Atoi(strings.TrimSpace(parts[1]))
	if startErr != nil || endErr != nil || start < 0 || end < start {
		return 0, 0, fmt.Errorf("invalid sibling range %q", value)
	}
	return start, end, nil
}

func parseJSONNotes(raw json.RawMessage, location string) (RichText, error) {
	object, err := jsonObject(raw, location)
	if err != nil {
		return RichText{}, err
	}
	notes := RichText{Raw: cloneRaw(raw)}
	if rawPlain, ok := object["plain"]; ok {
		plainObject, err := jsonObject(rawPlain, location+"/plain")
		if err != nil {
			return RichText{}, err
		}
		notes.Plain, err = requiredJSONString(plainObject, "content", location+"/plain")
		if err != nil {
			return RichText{}, err
		}
		notes.Format = "plain"
	}
	if rawHTML, ok := object["html"]; ok {
		if notes.Plain == "" {
			notes.Plain = collectJSONNoteText(rawHTML)
		}
		if notes.Format == "plain" {
			notes.Format = "plain+html"
		} else {
			notes.Format = "html"
		}
	}
	return notes, nil
}

func collectJSONNoteText(raw json.RawMessage) string {
	var root any
	if json.Unmarshal(raw, &root) != nil {
		return ""
	}
	queue := []any{root}
	var text []string
	for len(queue) > 0 {
		value := queue[0]
		queue = queue[1:]
		switch typed := value.(type) {
		case map[string]any:
			if part, ok := typed["text"].(string); ok {
				text = append(text, part)
			}
			for _, key := range []string{"content", "paragraphs", "spans"} {
				if child, ok := typed[key]; ok {
					queue = append(queue, child)
				}
			}
		case []any:
			queue = append(queue, typed...)
		}
	}
	return strings.Join(text, "\n")
}

func parseJSONLabels(raw json.RawMessage, location string) ([]string, error) {
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		if single == "" {
			return nil, nil
		}
		return []string{single}, nil
	}
	var multiple []string
	if err := json.Unmarshal(raw, &multiple); err != nil {
		return nil, invalidPayload("decode JSON Labels", fmt.Errorf("%s: %w", location, err))
	}
	return multiple, nil
}

func parseJSONNumbering(raw json.RawMessage, location string, parent *Topic, ordinal int) (Numbering, bool, error) {
	object, err := jsonObject(raw, location)
	if err != nil {
		return Numbering{}, false, err
	}
	format, err := optionalJSONString(object, "numberFormat", location)
	if err != nil {
		return Numbering{}, false, err
	}
	prefix, err := optionalJSONString(object, "prefix", location)
	if err != nil {
		return Numbering{}, false, err
	}
	suffix, err := optionalJSONString(object, "suffix", location)
	if err != nil {
		return Numbering{}, false, err
	}
	prepending, err := optionalJSONString(object, "prependingNumbers", location)
	if err != nil {
		return Numbering{}, false, err
	}
	numbering := Numbering{Enabled: format != "" && !strings.HasSuffix(format, ".none") && format != "none", Format: format, Prefix: prefix, Suffix: suffix, PrependingNumbers: prepending, Raw: cloneRaw(raw)}
	knownKeys := jsonKeys("numberFormat", "prefix", "suffix", "prependingNumbers")
	// The official legacy JSON contract and the existing contract fixture only
	// establish an omitted value or "none" as non-tiered semantics. Values such
	// as booleans, "true", restart modes, or future tokens must remain source
	// provenance until a real numbering fixture proves how XMind renders them.
	rawPrepending, hasPrepending := object["prependingNumbers"]
	prependingAbsent := !hasPrepending || string(rawPrepending) == "null"
	supported := prependingAbsent || prepending == "none"
	for key := range object {
		if _, known := knownKeys[key]; !known {
			supported = false
		}
	}
	if numbering.Enabled {
		token, knownFormat := formatOrdinal(format, ordinal+1)
		if !knownFormat {
			supported = false
		}
		if !supported {
			return numbering, false, nil
		}
		if numbering.Tiered && parent != nil && parent.Numbering.DisplayNumber != "" {
			token = parent.Numbering.DisplayNumber + "." + token
		}
		numbering.DisplayNumber = prefix + token + suffix
	}
	return numbering, supported, nil
}

func formatOrdinal(format string, value int) (string, bool) {
	format = strings.TrimPrefix(format, "org.xmind.numbering.")
	switch format {
	case "arabic", "decimal":
		return strconv.Itoa(value), true
	case "roman":
		return roman(value), true
	case "lowercase":
		return alphabetic(value, false), true
	case "uppercase":
		return alphabetic(value, true), true
	default:
		return "", false
	}
}

func roman(value int) string {
	if value <= 0 || value > 3999 {
		return strconv.Itoa(value)
	}
	values := []struct {
		value int
		text  string
	}{{1000, "M"}, {900, "CM"}, {500, "D"}, {400, "CD"}, {100, "C"}, {90, "XC"}, {50, "L"}, {40, "XL"}, {10, "X"}, {9, "IX"}, {5, "V"}, {4, "IV"}, {1, "I"}}
	var out strings.Builder
	for _, item := range values {
		for value >= item.value {
			out.WriteString(item.text)
			value -= item.value
		}
	}
	return out.String()
}

func alphabetic(value int, upper bool) string {
	if value <= 0 {
		return strconv.Itoa(value)
	}
	var out []byte
	base := byte('a')
	if upper {
		base = 'A'
	}
	for value > 0 {
		value--
		out = append([]byte{base + byte(value%26)}, out...)
		value /= 26
	}
	return string(out)
}

func parseTopicLink(href string) Link {
	link := Link{Target: href}
	if strings.HasPrefix(href, "xmind:#") {
		target := strings.TrimPrefix(href, "xmind:#")
		parts := strings.Split(target, "/")
		if len(parts) > 1 {
			link.TargetSheetID = parts[0]
			link.TargetTopicID = parts[len(parts)-1]
		} else {
			link.TargetTopicID = target
		}
	} else {
		link.External = true
	}
	return link
}

func parseJSONTask(raw json.RawMessage) Task {
	var object map[string]json.RawMessage
	_ = json.Unmarshal(raw, &object)
	status, _ := optionalJSONString(object, "status", "/task")
	owner, _ := optionalJSONString(object, "owner", "/task")
	start, _ := optionalJSONString(object, "start", "/task")
	due, _ := optionalJSONString(object, "due", "/task")
	progress, _ := optionalJSONFloat(object, "progress", "/task")
	return Task{Status: status, Owner: owner, Start: start, Due: due, Progress: progress}
}

func parseJSONLegend(raw json.RawMessage) (map[string]string, error) {
	result := make(map[string]string)
	var legend map[string]json.RawMessage
	if len(raw) == 0 {
		return result, nil
	}
	if err := json.Unmarshal(raw, &legend); err != nil || legend == nil {
		if err == nil {
			err = fmt.Errorf("expected object")
		}
		return result, err
	}
	rawMarkers, ok := legend["markers"]
	if !ok {
		return result, nil
	}
	var markers map[string]map[string]json.RawMessage
	if err := json.Unmarshal(rawMarkers, &markers); err != nil {
		return result, err
	}
	var firstErr error
	for markerID, marker := range markers {
		name, err := optionalJSONString(marker, "name", "/legend/markers/"+markerID)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		result[markerID] = name
	}
	return result, firstErr
}

func jsonRelationshipArrows(raw json.RawMessage) (string, string, bool) {
	var style map[string]json.RawMessage
	if json.Unmarshal(raw, &style) != nil || style == nil {
		return "", "", false
	}
	rawProperties, ok := style["properties"]
	if !ok {
		return "", "", true
	}
	var properties map[string]json.RawMessage
	if json.Unmarshal(rawProperties, &properties) != nil || properties == nil {
		return "", "", false
	}
	start, startErr := optionalJSONString(properties, "line-start-arrow-shape", "/relationship/style/properties")
	end, endErr := optionalJSONString(properties, "line-end-arrow-shape", "/relationship/style/properties")
	return start, end, startErr == nil && endErr == nil
}

func jsonObject(raw json.RawMessage, location string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		if err == nil {
			err = fmt.Errorf("expected object")
		}
		return nil, invalidPayload("decode JSON object", fmt.Errorf("%s: %w", location, err))
	}
	return object, nil
}

func requiredJSONString(object map[string]json.RawMessage, key, location string) (string, error) {
	_, ok := object[key]
	if !ok {
		return "", invalidPayload("decode JSON string", fmt.Errorf("%s/%s is required", location, key))
	}
	value, err := optionalJSONString(object, key, location)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", invalidPayload("decode JSON string", fmt.Errorf("%s/%s is empty", location, key))
	}
	return value, nil
}

func optionalJSONString(object map[string]json.RawMessage, key, location string) (string, error) {
	raw, ok := object[key]
	if !ok || string(raw) == "null" {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", invalidPayload("decode JSON string", fmt.Errorf("%s/%s: %w", location, key, err))
	}
	return value, nil
}

func optionalJSONFloat(object map[string]json.RawMessage, key, location string) (float64, error) {
	raw, ok := object[key]
	if !ok || string(raw) == "null" {
		return 0, nil
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, invalidPayload("decode JSON number", fmt.Errorf("%s/%s: %w", location, key, err))
	}
	return value, nil
}

func recordUnknownJSON(state *candidateState, object map[string]json.RawMessage, known map[string]struct{}, location string, destination *[]RawField) {
	keys := make([]string, 0)
	for key := range object {
		if _, ok := known[key]; !ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		itemLocation := location + "/" + pointerEscape(key)
		raw := cloneRaw(object[key])
		*destination = append(*destination, RawField{Name: key, Location: itemLocation, Encoding: "json", JSON: raw})
		state.feature("unknown_json_field", key, "", itemLocation, InventoryUnsupported, "unrecognized_json_key", raw)
	}
}

func pointerEscape(value string) string {
	value = strings.ReplaceAll(value, "~", "~0")
	return strings.ReplaceAll(value, "/", "~1")
}

func jsonKeys(values ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func appendCopy(values []int, value int) []int {
	result := make([]int, len(values)+1)
	copy(result, values)
	result[len(values)] = value
	return result
}

func firstRaw(object map[string]json.RawMessage, keys ...string) (json.RawMessage, bool) {
	for _, key := range keys {
		if raw, ok := object[key]; ok {
			return raw, true
		}
	}
	return nil, false
}
