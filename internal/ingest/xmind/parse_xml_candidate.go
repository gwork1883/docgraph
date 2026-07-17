package xmind

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

type xmlCandidateNode struct {
	Name     xml.Name
	Attr     []xml.Attr
	Children []*xmlCandidateNode
	Text     string
}

type xmlTopicWork struct {
	node      *xmlCandidateNode
	parent    *Topic
	role      string
	depth     int
	ordinal   int
	orderPath []int
	location  string
}

const (
	xmind8ContentNamespace = "urn:xmind:xmap:xmlns:content:2.0"
	xmind8ContentVersion   = "2.0"
	xhtmlNamespace         = "http://www.w3.org/1999/xhtml"
)

// parseXMLCandidate admits the legacy xmap-content/Sheet/root-Topic structure.
// Namespace and version are provenance signals rather than a whitelist: values
// outside the observed XMind content 2.0 signature are reported, while core
// structure is still parsed. Child elements in a namespace different from
// their parent are preserved and inventoried rather than being allowed to
// masquerade as Sheet or Topic elements.
func parseXMLCandidate(payload []byte, state *candidateState) (Workbook, error) {
	root, err := decodeXMLCandidateTree(payload, state.limits.MaxNormalizedElements)
	if err != nil {
		var packageErr *PackageError
		if errors.As(err, &packageErr) {
			return Workbook{}, packageErr
		}
		return Workbook{}, invalidPayload("decode legacy XML candidate", err)
	}
	if root.Name.Local != "xmap-content" {
		return Workbook{}, unsupportedXMLSignature(fmt.Sprintf("root element is %q, want xmap-content", root.Name.Local))
	}
	if root.Name.Space != xmind8ContentNamespace {
		state.feature("xml_content_namespace", root.Name.Space, "", "/xmap-content/@xmlns", InventoryUnsupported, "unverified_content_namespace", json.RawMessage(strconv.Quote(root.Name.Space)))
	}
	version := strings.TrimSpace(xmlAttr(root, "version"))
	if version != xmind8ContentVersion {
		state.feature("xml_content_version", version, "", "/xmap-content/@version", InventoryUnsupported, "unverified_content_version", json.RawMessage(strconv.Quote(version)))
	}
	workbook := Workbook{FormatFamily: FormatXMind8XML, FormatVersion: version}
	state.updateFeatureDisposition("workbook_payload", "", InventoryIndexed, "structure_validated_xmap_content")
	for _, child := range root.Children {
		if !xmlCoreName(child, root, "sheet") {
			state.feature("unknown_xml_element", child.Name.Local, "", "/xmap-content/"+child.Name.Local, InventoryUnsupported, "unrecognized_xml_element", json.RawMessage(strconv.Quote(xmlNodeSummary(child))))
			continue
		}
		sheet, err := parseXMLSheet(child, len(workbook.Sheets), state)
		if err != nil {
			return Workbook{}, err
		}
		workbook.Sheets = append(workbook.Sheets, sheet)
	}
	return workbook, nil
}

func decodeXMLCandidateTree(payload []byte, maxElements uint64) (*xmlCandidateNode, error) {
	decoder := xml.NewDecoder(bytes.NewReader(payload))
	var roots []*xmlCandidateNode
	var stack []*xmlCandidateNode
	var elementCount uint64
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch typed := token.(type) {
		case xml.StartElement:
			elementCount++
			if elementCount > maxElements {
				return nil, &PackageError{Code: ErrorArchiveLimit, Operation: "decode legacy XML candidate", Limit: "normalized_elements", Actual: elementCount, Maximum: maxElements, Detail: typed.Name.Local}
			}
			node := &xmlCandidateNode{Name: typed.Name, Attr: append([]xml.Attr(nil), typed.Attr...)}
			if len(stack) == 0 {
				roots = append(roots, node)
			} else {
				parent := stack[len(stack)-1]
				parent.Children = append(parent.Children, node)
			}
			stack = append(stack, node)
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].Text += string(typed)
			}
		case xml.EndElement:
			if len(stack) == 0 || stack[len(stack)-1].Name.Local != typed.Name.Local {
				return nil, fmt.Errorf("unbalanced closing element %q", typed.Name.Local)
			}
			stack = stack[:len(stack)-1]
		case xml.Directive:
			return nil, fmt.Errorf("XML directives are not accepted in candidate workbook payloads")
		}
	}
	if len(stack) != 0 || len(roots) != 1 {
		return nil, fmt.Errorf("expected exactly one complete root element")
	}
	return roots[0], nil
}

func parseXMLSheet(node *xmlCandidateNode, sheetIndex int, state *candidateState) (Sheet, error) {
	location := fmt.Sprintf("/xmap-content/sheet[%d]", sheetIndex)
	id := xmlAttr(node, "id")
	if id == "" {
		return Sheet{}, invalidPayload("decode legacy XML Sheet", fmt.Errorf("%s has no id", location))
	}
	if err := state.addElement(location, 0); err != nil {
		return Sheet{}, err
	}
	sheet := Sheet{ID: id, Title: directXMLText(node, "title")}
	pendingStart := len(state.pendingElements)
	state.feature("sheet", "sheet", id, location, InventoryIndexed, "", nil)
	recordUnknownXMLAttributes(state, node, xmlNames("id", "theme", "timestamp", "modified-by"), location, &sheet.RawFields)
	if extensions := directXMLChild(node, "extensions"); extensions != nil {
		raw := xmlNodeSummary(extensions)
		sheet.RawFields = append(sheet.RawFields, RawField{Name: "extensions", Location: location + "/extensions", Encoding: "xml", XML: raw})
		state.feature("extension", "extensions", id, location+"/extensions", InventoryPreserved, "legacy_extension_semantics_fixture_required", json.RawMessage(strconv.Quote(raw)))
	}
	rootNodes := directXMLChildren(node, "topic")
	if len(rootNodes) == 0 {
		return Sheet{}, invalidPayload("decode legacy XML Sheet", fmt.Errorf("%s has no root Topic", location))
	}
	if len(rootNodes) != 1 {
		return Sheet{}, invalidPayload("decode legacy XML Sheet", fmt.Errorf("%s has %d root Topics, want exactly one", location, len(rootNodes)))
	}
	rootNode := rootNodes[0]
	queue := []xmlTopicWork{{node: rootNode, role: "central", orderPath: []int{0}, location: location + "/topic[0]"}}
	nextRootOrdinal := 1
	for cursor := 0; cursor < len(queue); cursor++ {
		work := queue[cursor]
		topic, groups, err := parseXMLTopic(work, state)
		if err != nil {
			return Sheet{}, err
		}
		if work.parent == nil {
			sheet.Roots = append(sheet.Roots, topic)
		} else {
			work.parent.Children = append(work.parent.Children, topic)
		}
		if work.role == "summary" || work.role == "callout" {
			sheet.Elements = append(sheet.Elements, Element{ID: topic.ID, Kind: work.role, Title: topic.RawTitle, OwnerTopicID: topic.ParentID, Status: InventoryIndexed})
		}
		childOrdinal := 0
		for _, role := range []string{"attached", "summary", "callout"} {
			for index, child := range groups[role] {
				queue = append(queue, xmlTopicWork{node: child, parent: topic, role: role, depth: topic.Depth + 1, ordinal: childOrdinal, orderPath: appendCopy(topic.OrderPath, childOrdinal), location: fmt.Sprintf("%s/children/topics[@type=%q]/topic[%d]", work.location, role, index)})
				childOrdinal++
			}
		}
		for index, child := range groups["detached"] {
			rootOrdinal := nextRootOrdinal
			nextRootOrdinal++
			queue = append(queue, xmlTopicWork{node: child, role: "detached", ordinal: rootOrdinal, orderPath: []int{rootOrdinal}, location: fmt.Sprintf("%s/children/topics[@type=detached]/topic[%d]", work.location, index)})
		}
	}
	if container := directXMLChild(node, "relationships"); container != nil {
		seenRelationshipIDs := make(map[string]struct{})
		for index, relationshipNode := range directXMLChildren(container, "relationship") {
			relationshipLocation := fmt.Sprintf("%s/relationships/relationship[%d]", location, index)
			relationship, err := parseXMLRelationship(relationshipNode, relationshipLocation, state)
			if err != nil {
				recordUnsupportedXMLNode(state, relationshipNode, "relationship", firstXMLAttr(relationshipNode, "id"), relationshipLocation, "invalid_relationship_fields", &sheet.RawFields)
				continue
			}
			if _, duplicate := seenRelationshipIDs[relationship.ID]; duplicate {
				recordUnsupportedXMLNode(state, relationshipNode, "relationship", relationship.ID, relationshipLocation, "duplicate_relationship_id", &sheet.RawFields)
				continue
			}
			seenRelationshipIDs[relationship.ID] = struct{}{}
			sheet.Relationships = append(sheet.Relationships, relationship)
			state.feature("relationship", "relationship", relationship.ID, relationshipLocation, InventoryIndexed, "", nil)
		}
		recordUnknownXMLChildren(state, container, xmlNames("relationship"), location+"/relationships", &sheet.RawFields)
	}
	if legend := directXMLChild(node, "legend"); legend != nil {
		raw := xmlNodeSummary(legend)
		sheet.RawFields = append(sheet.RawFields, RawField{Name: "legend", Location: location + "/legend", Encoding: "xml", XML: raw})
		state.feature("legend", "legend", id, location+"/legend", InventoryPreserved, "legend_projection_not_implemented", json.RawMessage(strconv.Quote(raw)))
	}
	sheet.Elements = append(sheet.Elements, state.pendingElements[pendingStart:]...)
	for _, child := range node.Children {
		if !xmlCoreNameIn(child, node, "topic", "title", "relationships", "legend", "extensions") {
			recordUnknownXMLNode(state, child, location+"/"+child.Name.Local, &sheet.RawFields)
		}
	}
	return sheet, nil
}

func parseXMLTopic(work xmlTopicWork, state *candidateState) (*Topic, map[string][]*xmlCandidateNode, error) {
	if err := state.addElement(work.location, work.depth); err != nil {
		return nil, nil, err
	}
	id := xmlAttr(work.node, "id")
	if id == "" {
		return nil, nil, invalidPayload("decode legacy XML Topic", fmt.Errorf("%s has no id", work.location))
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
	topic := &Topic{ID: id, ParentID: parentID, Kind: kind, ChildRole: work.role, RawTitle: directXMLText(work.node, "title"), Depth: work.depth, SiblingOrdinal: work.ordinal, OrderPath: append([]int(nil), work.orderPath...)}
	state.feature("topic", work.role, id, work.location, InventoryIndexed, "", nil)
	recordUnknownXMLAttributes(state, work.node, xmlNames("id", "href", "structure-class", "class", "timestamp", "modified-by", "branch", "style-id"), work.location, &topic.RawFields)

	if notes := directXMLChild(work.node, "notes"); notes != nil {
		plain := directXMLChild(notes, "plain")
		html := directXMLChild(notes, "html")
		if plain != nil {
			topic.Notes.Plain = recursiveXMLText(plain)
			topic.Notes.Format = "plain"
		}
		if html != nil {
			topic.Notes.RawXML = xmlNodeSummary(html)
			if topic.Notes.Plain == "" {
				topic.Notes.Plain = recursiveXMLText(html)
				topic.Notes.Format = "html"
			} else {
				topic.Notes.Format = "plain+html"
			}
		}
		state.feature("notes", "notes", id, work.location+"/notes", InventoryIndexed, "", nil)
		recordUnknownXMLChildren(state, notes, xmlNames("plain", "html"), work.location+"/notes", &topic.RawFields)
	}
	if labels := directXMLChild(work.node, "labels"); labels != nil {
		for index, label := range directXMLChildren(labels, "label") {
			value := strings.TrimSpace(recursiveXMLText(label))
			if value != "" {
				topic.Labels = append(topic.Labels, value)
				state.feature("label", "label", id, fmt.Sprintf("%s/labels/label[%d]", work.location, index), InventoryIndexed, "", nil)
			}
		}
		recordUnknownXMLChildren(state, labels, xmlNames("label"), work.location+"/labels", &topic.RawFields)
	}
	if markers := directXMLChild(work.node, "marker-refs"); markers != nil {
		for index, marker := range directXMLChildren(markers, "marker-ref") {
			markerID := xmlAttr(marker, "marker-id")
			if markerID != "" {
				topic.Markers = append(topic.Markers, Marker{ID: markerID})
				state.feature("marker", markerID, id, fmt.Sprintf("%s/marker-refs/marker-ref[%d]", work.location, index), InventoryIndexed, "", nil)
			}
		}
		recordUnknownXMLChildren(state, markers, xmlNames("marker-ref"), work.location+"/marker-refs", &topic.RawFields)
	}
	if numbering := directXMLChild(work.node, "numbering"); numbering != nil {
		var supported bool
		topic.Numbering, supported = parseXMLNumbering(numbering, work.parent, work.ordinal)
		if supported {
			state.feature("numbering", topic.Numbering.Format, id, work.location+"/numbering", InventoryIndexed, "", nil)
		} else {
			state.feature("numbering", topic.Numbering.Format, id, work.location+"/numbering", InventoryUnsupported, "numbering_semantics_not_implemented", nil)
		}
		recordUnknownXMLAttributes(state, numbering, xmlNames("number-format", "format", "prepending-numbers", "prepends-parent-numbers", "prefix", "suffix"), work.location+"/numbering", &topic.RawFields)
		recordUnknownXMLChildren(state, numbering, nil, work.location+"/numbering", &topic.RawFields)
	}
	if href := xmlAttr(work.node, "href"); href != "" {
		if strings.HasPrefix(href, "xap:") {
			state.addAsset(topic, AssetAttachment, href, "attachment", work.location+"/@href", 0, 0, nil)
		} else {
			topic.Links = append(topic.Links, parseTopicLink(href))
			state.feature("topic_link", href, id, work.location+"/@href", InventoryIndexed, "", nil)
		}
	}
	if image := firstXMLImageDescendantBeforeNestedTopic(work.node); image != nil {
		imageLocation := work.location + "/image"
		src := firstXMLAttr(image, "src", "href")
		widthRaw := strings.TrimSpace(firstXMLAttr(image, "width"))
		heightRaw := strings.TrimSpace(firstXMLAttr(image, "height"))
		width, widthOK := parseFiniteXMLFloat(widthRaw)
		height, heightOK := parseFiniteXMLFloat(heightRaw)
		switch {
		case strings.TrimSpace(src) == "":
			recordUnsupportedXMLNode(state, image, "media", topic.ID, imageLocation, "missing_image_source", &topic.RawFields)
		case widthRaw == "" || heightRaw == "":
			recordUnsupportedXMLNode(state, image, "media_geometry", topic.ID, imageLocation, "missing_image_dimensions", &topic.RawFields)
			state.addAsset(topic, AssetImage, src, "image", imageLocation, width, height, nil)
		case !widthOK || !heightOK || width <= 0 || height <= 0:
			recordUnsupportedXMLNode(state, image, "media_geometry", topic.ID, imageLocation, "invalid_image_dimensions", &topic.RawFields)
			if !widthOK || width <= 0 {
				width = 0
			}
			if !heightOK || height <= 0 {
				height = 0
			}
			state.addAsset(topic, AssetImage, src, "image", imageLocation, width, height, nil)
		default:
			state.addAsset(topic, AssetImage, src, "image", imageLocation, width, height, nil)
		}
	}
	if taskInfo := firstXMLDescendantBeforeNestedTopic(work.node, "task-info"); taskInfo != nil {
		topic.Task = Task{Status: firstXMLAttr(taskInfo, "status"), Owner: firstXMLAttr(taskInfo, "owner"), Start: firstXMLAttr(taskInfo, "start", "start-date"), Due: firstXMLAttr(taskInfo, "due", "end-date")}
		topic.Task.Progress, _ = strconv.ParseFloat(firstXMLAttr(taskInfo, "progress"), 64)
		state.feature("task", "legacy_task_extension", id, work.location+"/extensions/task-info", InventoryPreserved, "fixture_required_for_extension_semantics", json.RawMessage(strconv.Quote(xmlNodeSummary(taskInfo))))
	}
	if extensions := directXMLChild(work.node, "extensions"); extensions != nil {
		raw := xmlNodeSummary(extensions)
		topic.RawFields = append(topic.RawFields, RawField{Name: "extensions", Location: work.location + "/extensions", Encoding: "xml", XML: raw})
		state.feature("extension", "extensions", id, work.location+"/extensions", InventoryPreserved, "legacy_extension_semantics_fixture_required", json.RawMessage(strconv.Quote(raw)))
	}
	parseXMLBoundariesAndSummaries(work.node, topic, work.location, state)

	groups := make(map[string][]*xmlCandidateNode)
	if children := directXMLChild(work.node, "children"); children != nil {
		for containerIndex, topics := range directXMLChildren(children, "topics") {
			role := xmlAttr(topics, "type")
			if !xmlNameIn(role, "attached", "detached", "summary", "callout") {
				recordUnknownXMLNode(state, topics, work.location+"/children/topics", &topic.RawFields)
				continue
			}
			groups[role] = append(groups[role], directXMLChildren(topics, "topic")...)
			containerLocation := fmt.Sprintf("%s/children/topics[%d]", work.location, containerIndex)
			recordUnknownXMLAttributes(state, topics, xmlNames("type"), containerLocation, &topic.RawFields)
			recordUnknownXMLChildren(state, topics, xmlNames("topic"), containerLocation, &topic.RawFields)
		}
		recordUnknownXMLChildren(state, children, xmlNames("topics"), work.location+"/children", &topic.RawFields)
	}
	for _, child := range work.node.Children {
		coreKnown := xmlCoreNameIn(child, work.node, "title", "children", "notes", "labels", "marker-refs", "numbering", "boundaries", "summaries", "extensions")
		mediaKnown := supportedXMLImageNode(child, work.node)
		if !coreKnown && !mediaKnown {
			recordUnknownXMLNode(state, child, work.location+"/"+child.Name.Local, &topic.RawFields)
		}
	}
	return topic, groups, nil
}

func parseXMLRelationship(node *xmlCandidateNode, location string, state *candidateState) (Relationship, error) {
	id := firstXMLAttr(node, "id")
	end1 := firstXMLAttr(node, "end1", "end1-id", "end1Id")
	end2 := firstXMLAttr(node, "end2", "end2-id", "end2Id")
	if id == "" || end1 == "" || end2 == "" {
		return Relationship{}, invalidPayload("decode legacy XML Relationship", fmt.Errorf("%s requires id/end1/end2", location))
	}
	relationship := Relationship{ID: id, Label: directXMLText(node, "title"), SourceTopicID: end1, TargetTopicID: end2, SemanticDirection: "undirected", StartArrow: firstXMLAttr(node, "start-arrow", "start-arrow-shape"), EndArrow: firstXMLAttr(node, "end-arrow", "end-arrow-shape")}
	if styleID := firstXMLAttr(node, "style-id"); styleID != "" {
		relationship.Style, _ = json.Marshal(map[string]string{"style_id": styleID})
	}
	recordUnknownXMLAttributes(state, node, xmlNames("id", "end1", "end2", "end1-id", "end2-id", "end1Id", "end2Id", "start-arrow", "end-arrow", "start-arrow-shape", "end-arrow-shape", "style-id"), location, &relationship.RawFields)
	for _, child := range node.Children {
		if !xmlCoreNameIn(child, node, "title", "control-points") {
			recordUnknownXMLNode(state, child, location+"/"+child.Name.Local, &relationship.RawFields)
		}
	}
	if points := directXMLChild(node, "control-points"); points != nil {
		for index, point := range directXMLChildren(points, "control-point") {
			pointLocation := fmt.Sprintf("%s/control-points/control-point[%d]", location, index)
			angleRaw := strings.TrimSpace(firstXMLAttr(point, "angle"))
			amountRaw := strings.TrimSpace(firstXMLAttr(point, "amount"))
			angle, angleOK := parseFiniteXMLFloat(angleRaw)
			amount, amountOK := parseFiniteXMLFloat(amountRaw)
			switch {
			case angleRaw == "" || amountRaw == "":
				recordUnsupportedXMLNode(state, point, "relationship_geometry", id, pointLocation, "missing_control_point_number", &relationship.RawFields)
				continue
			case !angleOK || !amountOK:
				recordUnsupportedXMLNode(state, point, "relationship_geometry", id, pointLocation, "invalid_control_point_number", &relationship.RawFields)
				continue
			}
			relationship.ControlPoints = append(relationship.ControlPoints, Point{Key: strconv.Itoa(index), Angle: angle, Amount: amount})
			recordUnknownXMLAttributes(state, point, xmlNames("angle", "amount"), pointLocation, &relationship.RawFields)
		}
		recordUnknownXMLChildren(state, points, xmlNames("control-point"), location+"/control-points", &relationship.RawFields)
	}
	return relationship, nil
}

func parseFiniteXMLFloat(raw string) (float64, bool) {
	if strings.TrimSpace(raw) == "" {
		return 0, false
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
		return 0, false
	}
	return value, true
}

func parseXMLNumbering(node *xmlCandidateNode, parent *Topic, ordinal int) (Numbering, bool) {
	format := firstXMLAttr(node, "number-format", "format")
	prepending := firstXMLAttr(node, "prepending-numbers", "prepends-parent-numbers")
	numbering := Numbering{Enabled: format != "" && !strings.HasSuffix(format, ".none"), Format: format, Prefix: firstXMLAttr(node, "prefix"), Suffix: firstXMLAttr(node, "suffix"), PrependingNumbers: prepending}
	prependingAttributes := 0
	for _, attribute := range node.Attr {
		if attribute.Name.Local == "prepending-numbers" || attribute.Name.Local == "prepends-parent-numbers" {
			prependingAttributes++
		}
	}
	// The current XML contract fixture establishes only omitted, "none", and
	// "false" as non-tiered values. Do not infer tiering/restart semantics from
	// any other token until a real XMind 8 numbering fixture proves it.
	if prependingAttributes > 1 || prependingAttributes == 1 && prepending != "none" && prepending != "false" {
		return numbering, false
	}
	if numbering.Enabled {
		token, supported := formatOrdinal(format, ordinal+1)
		if !supported {
			return numbering, false
		}
		if numbering.Tiered && parent != nil && parent.Numbering.DisplayNumber != "" {
			token = parent.Numbering.DisplayNumber + "." + token
		}
		numbering.DisplayNumber = numbering.Prefix + token + numbering.Suffix
	}
	return numbering, true
}

func parseXMLBoundariesAndSummaries(node *xmlCandidateNode, topic *Topic, location string, state *candidateState) {
	if container := directXMLChild(node, "boundaries"); container != nil {
		for index, boundary := range directXMLChildren(container, "boundary") {
			id := xmlAttr(boundary, "id")
			rangeValue := xmlAttr(boundary, "range")
			state.pendingElements = append(state.pendingElements, Element{ID: id, Kind: "boundary", Title: directXMLText(boundary, "title"), OwnerTopicID: topic.ID, Range: rangeValue, Status: InventoryPreserved})
			state.feature("boundary", "boundary", id, fmt.Sprintf("%s/boundaries/boundary[%d]", location, index), InventoryPreserved, "range_resolution_fixture_required", nil)
		}
	}
	if container := directXMLChild(node, "summaries"); container != nil {
		for index, summary := range directXMLChildren(container, "summary") {
			id := xmlAttr(summary, "id")
			target := firstXMLAttr(summary, "topic-id", "topicId")
			state.pendingElements = append(state.pendingElements, Element{ID: id, Kind: "summary_range", OwnerTopicID: topic.ID, TargetIDs: []string{target}, Range: xmlAttr(summary, "range"), Status: InventoryPreserved})
			state.feature("summary", "summary", id, fmt.Sprintf("%s/summaries/summary[%d]", location, index), InventoryPreserved, "range_resolution_fixture_required", nil)
		}
	}
}

func recordUnknownXMLNode(state *candidateState, node *xmlCandidateNode, location string, destination *[]RawField) {
	raw := xmlNodeSummary(node)
	*destination = append(*destination, RawField{Name: node.Name.Local, Location: location, Encoding: "xml", XML: raw})
	state.feature("unknown_xml_element", node.Name.Local, "", location, InventoryUnsupported, "unrecognized_xml_element", json.RawMessage(strconv.Quote(raw)))
}

func recordUnsupportedXMLNode(state *candidateState, node *xmlCandidateNode, feature, sourceElementID, location, reason string, destination *[]RawField) {
	raw := xmlNodeSummary(node)
	*destination = append(*destination, RawField{Name: node.Name.Local, Location: location, Encoding: "xml", XML: raw})
	state.feature(feature, node.Name.Local, sourceElementID, location, InventoryUnsupported, reason, json.RawMessage(strconv.Quote(raw)))
}

func recordUnknownXMLAttributes(state *candidateState, node *xmlCandidateNode, known map[string]struct{}, location string, destination *[]RawField) {
	for _, attribute := range node.Attr {
		if _, ok := known[attribute.Name.Local]; ok || strings.HasPrefix(attribute.Name.Local, "xmlns") {
			continue
		}
		attributeLocation := location + "/@" + attribute.Name.Local
		*destination = append(*destination, RawField{Name: "@" + attribute.Name.Local, Location: attributeLocation, Encoding: "xml", XML: attribute.Value})
		state.feature("unknown_xml_attribute", attribute.Name.Local, "", attributeLocation, InventoryUnsupported, "unrecognized_xml_attribute", json.RawMessage(strconv.Quote(attribute.Value)))
	}
}

func recordUnknownXMLChildren(state *candidateState, node *xmlCandidateNode, known map[string]struct{}, location string, destination *[]RawField) {
	for _, child := range node.Children {
		_, isKnown := known[child.Name.Local]
		if isKnown && child.Name.Space == node.Name.Space {
			continue
		}
		recordUnknownXMLNode(state, child, location+"/"+child.Name.Local, destination)
	}
}

func directXMLChild(node *xmlCandidateNode, local string) *xmlCandidateNode {
	for _, child := range node.Children {
		if xmlCoreName(child, node, local) {
			return child
		}
	}
	return nil
}

func directXMLChildren(node *xmlCandidateNode, local string) []*xmlCandidateNode {
	var result []*xmlCandidateNode
	for _, child := range node.Children {
		if xmlCoreName(child, node, local) {
			result = append(result, child)
		}
	}
	return result
}

func xmlCoreName(node, parent *xmlCandidateNode, local string) bool {
	return node != nil && parent != nil && node.Name.Local == local && node.Name.Space == parent.Name.Space
}

func xmlCoreNameIn(node, parent *xmlCandidateNode, candidates ...string) bool {
	return node != nil && parent != nil && node.Name.Space == parent.Name.Space && xmlNameIn(node.Name.Local, candidates...)
}

func unsupportedXMLSignature(detail string) *PackageError {
	return &PackageError{Code: ErrorUnsupportedFormat, Operation: "validate XMind 8 XML signature", Detail: detail}
}

func xmlDescendants(node *xmlCandidateNode, local string) []*xmlCandidateNode {
	var result []*xmlCandidateNode
	queue := append([]*xmlCandidateNode(nil), node.Children...)
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current.Name.Local == local {
			result = append(result, current)
		}
		queue = append(queue, current.Children...)
	}
	return result
}

func firstXMLDescendant(node *xmlCandidateNode, locals ...string) *xmlCandidateNode {
	queue := append([]*xmlCandidateNode(nil), node.Children...)
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if xmlNameIn(current.Name.Local, locals...) {
			return current
		}
		queue = append(queue, current.Children...)
	}
	return nil
}

func firstXMLDescendantBeforeNestedTopic(node *xmlCandidateNode, locals ...string) *xmlCandidateNode {
	queue := append([]*xmlCandidateNode(nil), node.Children...)
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current.Name.Local == "topic" {
			continue
		}
		if xmlNameIn(current.Name.Local, locals...) {
			return current
		}
		queue = append(queue, current.Children...)
	}
	return nil
}

func firstXMLImageDescendantBeforeNestedTopic(node *xmlCandidateNode) *xmlCandidateNode {
	queue := append([]*xmlCandidateNode(nil), node.Children...)
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current.Name.Local == "topic" && current.Name.Space == node.Name.Space {
			continue
		}
		if supportedXMLImageNode(current, node) {
			return current
		}
		queue = append(queue, current.Children...)
	}
	return nil
}

func supportedXMLImageNode(node, topic *xmlCandidateNode) bool {
	return node != nil && topic != nil && xmlNameIn(node.Name.Local, "img", "image") && (node.Name.Space == topic.Name.Space || node.Name.Space == xhtmlNamespace)
}

func directXMLText(node *xmlCandidateNode, local string) string {
	child := directXMLChild(node, local)
	if child == nil {
		return ""
	}
	return strings.TrimSpace(recursiveXMLText(child))
}

func recursiveXMLText(node *xmlCandidateNode) string {
	var parts []string
	queue := []*xmlCandidateNode{node}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if text := strings.TrimSpace(current.Text); text != "" {
			parts = append(parts, text)
		}
		queue = append(queue, current.Children...)
	}
	return strings.Join(parts, " ")
}

func xmlNodeSummary(node *xmlCandidateNode) string {
	var out strings.Builder
	out.WriteString("<")
	out.WriteString(node.Name.Local)
	for _, attribute := range node.Attr {
		out.WriteString(" ")
		out.WriteString(attribute.Name.Local)
		out.WriteString("=")
		out.WriteString(strconv.Quote(attribute.Value))
	}
	out.WriteString(">")
	if text := strings.TrimSpace(recursiveXMLText(node)); text != "" {
		out.WriteString(text)
	}
	out.WriteString("</")
	out.WriteString(node.Name.Local)
	out.WriteString(">")
	return out.String()
}

func xmlAttr(node *xmlCandidateNode, local string) string {
	return firstXMLAttr(node, local)
}

func firstXMLAttr(node *xmlCandidateNode, locals ...string) string {
	for _, attribute := range node.Attr {
		if xmlNameIn(attribute.Name.Local, locals...) {
			return attribute.Value
		}
	}
	return ""
}

func xmlNameIn(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if value == candidate {
			return true
		}
	}
	return false
}

func xmlNames(values ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}
