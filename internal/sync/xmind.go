package sync

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docgraph/docgraph/internal/blobstore"
	"github.com/docgraph/docgraph/internal/ids"
	"github.com/docgraph/docgraph/internal/ingest/xmind"
	"github.com/docgraph/docgraph/internal/storage"
)

const (
	xmindWorkbookKey       = "primary"
	xmindBlobGCGracePeriod = 24 * time.Hour
)

// XMindParser is injectable so the workbook-to-DocGraph mapping and atomic
// replacement contract can be tested independently from format fixtures. The
// production parser dispatches only fixture-proven format signatures.
type XMindParser interface {
	Parse(io.ReaderAt, int64, xmind.Limits) (xmind.Workbook, error)
}

type XMindParserFunc func(io.ReaderAt, int64, xmind.Limits) (xmind.Workbook, error)

func (f XMindParserFunc) Parse(reader io.ReaderAt, size int64, limits xmind.Limits) (xmind.Workbook, error) {
	return f(reader, size, limits)
}

// XMindBlobStore is the narrow write side of the central DocGraph BlobStore.
// Blob bytes never enter SQLite and are never written beside the source file.
type XMindBlobStore interface {
	Put(context.Context, string, io.Reader, int64) (blobstore.Blob, error)
}

type ServiceOptions struct {
	DataDir     string
	XMindParser XMindParser
	BlobStore   XMindBlobStore
}

type xmindRuntime struct {
	dataDir string
	parser  XMindParser
	blobs   XMindBlobStore
	initErr error
}

type productionXMindParser struct{}

func (productionXMindParser) Parse(reader io.ReaderAt, size int64, limits xmind.Limits) (xmind.Workbook, error) {
	return xmind.ParseWorkbook(reader, size, limits)
}

func NewServiceWithOptions(store storage.Store, opts ServiceOptions) *Service {
	runtime := xmindRuntime{
		dataDir: strings.TrimSpace(opts.DataDir),
		parser:  opts.XMindParser,
		blobs:   opts.BlobStore,
	}
	if runtime.parser == nil {
		runtime.parser = productionXMindParser{}
	}
	if runtime.blobs == nil && runtime.dataDir != "" {
		runtime.blobs, runtime.initErr = blobstore.New(runtime.dataDir)
	}
	return &Service{store: store, xmind: runtime}
}

func (s *Service) syncXMind(ctx context.Context, source storage.Source, jobID string) (Result, error) {
	if s.xmind.initErr != nil {
		return Result{}, fmt.Errorf("initialize XMind BlobStore: %w", s.xmind.initErr)
	}
	if s.xmind.blobs == nil {
		return Result{}, fmt.Errorf("XMind sync requires the configured DocGraph data_dir")
	}
	workbookStore, ok := s.store.(storage.WorkbookStore)
	if !ok {
		return Result{}, fmt.Errorf("XMind workbook replacement is not supported by this storage backend")
	}

	path, err := resolveXMindSourcePath(source.DSN)
	if err != nil {
		return Result{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return Result{}, fmt.Errorf("open XMind source: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Result{}, fmt.Errorf("stat XMind source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Result{}, fmt.Errorf("XMind source must be a regular file")
	}

	limits := xmind.DefaultLimits()
	// Detection validates every ZIP entry, including CRC, path safety, entry
	// count, sizes, total expansion, compression ratio, and encryption flags.
	// It completes before the parser and before any Blob or relational write.
	if _, err := xmind.DetectPackage(file, info.Size(), limits); err != nil {
		return Result{}, fmt.Errorf("detect XMind package: %w", err)
	}
	workbook, err := s.xmind.parser.Parse(file, info.Size(), limits)
	if err != nil {
		return Result{}, fmt.Errorf("parse XMind workbook: %w", err)
	}
	plan, err := buildXMindBundle(source, workbook)
	if err != nil {
		return Result{}, fmt.Errorf("normalize XMind workbook: %w", err)
	}

	// The entire workbook is parsed and validated above. Only immutable bytes
	// are staged before the single relational aggregate transaction below.
	releaseReferences := func() {}
	if guard, ok := s.xmind.blobs.(xmindBlobReferenceGuard); ok {
		releaseReferences = guard.BeginReferenceUpdate()
	}
	referencesReleased := false
	defer func() {
		if !referencesReleased {
			releaseReferences()
		}
	}()
	scope := firstNonEmptyString(jobID, source.ID)
	snapshotBlob, err := s.xmind.blobs.Put(ctx, scope, io.NewSectionReader(file, 0, info.Size()), int64(limits.MaxArchiveBytes))
	if err != nil {
		return Result{}, fmt.Errorf("preserve XMind snapshot: %w", err)
	}
	if snapshotBlob.SHA256 != workbook.SnapshotHash {
		return Result{}, fmt.Errorf("XMind snapshot hash changed during sync: parsed=%s staged=%s", workbook.SnapshotHash, snapshotBlob.SHA256)
	}
	plan.bundle.Snapshot.BlobSHA256 = snapshotBlob.SHA256
	plan.bundle.Blobs = append(plan.bundle.Blobs, mediaBlobInput(snapshotBlob))

	resourceBlobs, err := stageXMindResources(ctx, s.xmind.blobs, scope, file, info.Size(), limits, plan.resources)
	if err != nil {
		return Result{}, err
	}
	for _, blob := range resourceBlobs {
		plan.bundle.Blobs = append(plan.bundle.Blobs, mediaBlobInput(blob))
	}
	resourceBlobByDigest := make(map[string]blobstore.Blob, len(resourceBlobs))
	for _, blob := range resourceBlobs {
		resourceBlobByDigest[blob.SHA256] = blob
	}
	for index := range plan.bundle.MediaAssets {
		asset := &plan.bundle.MediaAssets[index]
		if blob, ok := resourceBlobByDigest[asset.BlobSHA256]; ok {
			asset.SizeBytes = blob.Size
			asset.MediaType = blob.MediaType
		}
	}
	plan.diagnostics.MediaBytes = 0
	for _, asset := range plan.bundle.MediaAssets {
		if asset.BlobSHA256 != "" {
			plan.diagnostics.MediaBytes += asset.SizeBytes
		}
	}

	replaceResult, err := workbookStore.ReplaceWorkbookBundle(ctx, plan.bundle)
	if err != nil {
		// Immutable staged objects are intentionally left unreferenced. Delayed
		// GC can remove them after its safety window; old relational state and
		// the previous active snapshot remain untouched by a rolled-back tx.
		return Result{}, fmt.Errorf("replace XMind workbook aggregate: %w", err)
	}
	plan.diagnostics.SnapshotID = replaceResult.ActiveSnapshotID
	for _, staleSectionID := range replaceResult.StaleSectionIDs {
		if err := s.store.DeleteSectionEmbeddings(ctx, staleSectionID); err != nil {
			// The relational replacement has committed, so do not misreport the
			// new source revision as rolled back. Record the external vector cleanup
			// failure and let normal embedding maintenance retry it.
			plan.diagnostics.Warnings = append(plan.diagnostics.Warnings,
				fmt.Sprintf("stale vector cleanup deferred for Section %s: %v", staleSectionID, err))
		}
	}
	releaseReferences()
	referencesReleased = true

	result := Result{
		SourceID:             source.ID,
		Documents:            len(plan.bundle.Documents),
		ConnectorDiagnostics: &plan.diagnostics,
	}
	if warning := s.collectXMindGarbage(ctx); warning != "" {
		result.ConnectorDiagnostics.Warnings = append(result.ConnectorDiagnostics.Warnings, warning)
	}
	return result, nil
}

func resolveXMindSourcePath(dsn string) (string, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return "", fmt.Errorf("XMind source DSN is required")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse XMind source DSN: %w", err)
	}
	var sourcePath string
	switch strings.ToLower(parsed.Scheme) {
	case "":
		sourcePath = dsn
	case "file":
		if parsed.Host != "" && !strings.EqualFold(parsed.Host, "localhost") {
			return "", fmt.Errorf("remote file host %q is not supported for XMind sources", parsed.Host)
		}
		if parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", fmt.Errorf("XMind file DSN must not contain a query or fragment")
		}
		decoded, err := url.PathUnescape(parsed.Path)
		if err != nil {
			return "", fmt.Errorf("decode XMind file DSN: %w", err)
		}
		sourcePath = decoded
	default:
		return "", fmt.Errorf("XMind source DSN scheme %q is not supported", parsed.Scheme)
	}
	if strings.TrimSpace(sourcePath) == "" {
		return "", fmt.Errorf("XMind source path is required")
	}
	abs, err := filepath.Abs(filepath.Clean(sourcePath))
	if err != nil {
		return "", fmt.Errorf("resolve XMind source path: %w", err)
	}
	return abs, nil
}

type xmindBundlePlan struct {
	bundle      storage.WorkbookBundle
	resources   map[string]string // package path -> expected SHA-256
	diagnostics storage.ConnectorDiagnostics
}

type xmindTopicLocation struct {
	documentID string
	sectionID  string
	nodeID     string
	sheetID    string
}

func buildXMindBundle(source storage.Source, workbook xmind.Workbook) (xmindBundlePlan, error) {
	if strings.TrimSpace(source.ID) == "" {
		return xmindBundlePlan{}, fmt.Errorf("source id is required")
	}
	if len(workbook.Sheets) == 0 {
		return xmindBundlePlan{}, fmt.Errorf("workbook has no Sheets")
	}
	if strings.TrimSpace(workbook.SnapshotHash) == "" || strings.TrimSpace(workbook.SemanticHash) == "" || strings.TrimSpace(workbook.MediaManifestHash) == "" {
		return xmindBundlePlan{}, fmt.Errorf("workbook hashes are incomplete")
	}

	snapshotID := ids.Stable("snap", source.ID, workbook.SnapshotHash)
	parseStatus, unsupportedFeatures, rejectedFeatures := xmindParseStatus(workbook.FeatureInventory)
	plan := xmindBundlePlan{
		bundle: storage.WorkbookBundle{
			SourceID:    source.ID,
			WorkbookKey: xmindWorkbookKey,
			Snapshot: storage.SourceSnapshotInput{
				ID:                snapshotID,
				SourceID:          source.ID,
				SourceHash:        workbook.SnapshotHash,
				FormatFamily:      string(workbook.FormatFamily),
				FormatVersion:     workbook.FormatVersion,
				SemanticHash:      workbook.SemanticHash,
				MediaManifestHash: workbook.MediaManifestHash,
				MetadataJSON:      xmindJSON(map[string]any{"workbook_key": xmindWorkbookKey, "parse_status": parseStatus}),
			},
		},
		resources: make(map[string]string),
		diagnostics: storage.ConnectorDiagnostics{
			ParseStatus:   parseStatus,
			FormatFamily:  string(workbook.FormatFamily),
			FormatVersion: workbook.FormatVersion,
			SnapshotID:    snapshotID,
			SnapshotHash:  workbook.SnapshotHash,
			Sheets:        len(workbook.Sheets),
		},
	}
	if parseStatus == "parsed_with_warnings" {
		plan.diagnostics.Warnings = append(plan.diagnostics.Warnings,
			fmt.Sprintf("XMind parsed with warnings: %d unsupported and %d rejected source features; see feature inventory", unsupportedFeatures, rejectedFeatures))
	}

	topicLocations := make(map[string]xmindTopicLocation)
	topicByID := make(map[string]*xmind.Topic)
	assetByID := make(map[string]xmind.Asset, len(workbook.Assets))
	for _, asset := range workbook.Assets {
		if asset.ID == "" {
			return xmindBundlePlan{}, fmt.Errorf("media asset has no source ID")
		}
		if _, exists := assetByID[asset.ID]; exists {
			return xmindBundlePlan{}, fmt.Errorf("duplicate media asset ID %q", asset.ID)
		}
		assetByID[asset.ID] = asset
	}

	// Establish every stable Topic identity before materializing any cross-Sheet
	// link or Relationship.
	for _, sheet := range workbook.Sheets {
		if strings.TrimSpace(sheet.ID) == "" {
			return xmindBundlePlan{}, fmt.Errorf("Sheet has no source ID")
		}
		documentID := ids.Stable("doc", source.ID, xmindWorkbookKey, sheet.ID)
		for _, topic := range flattenXMindTopics(sheet.Roots) {
			if topic == nil || strings.TrimSpace(topic.ID) == "" {
				return xmindBundlePlan{}, fmt.Errorf("Sheet %q has a Topic without a source ID", sheet.ID)
			}
			if _, exists := topicLocations[topic.ID]; exists {
				return xmindBundlePlan{}, fmt.Errorf("duplicate workbook-scoped Topic ID %q", topic.ID)
			}
			sectionID := ids.Stable("sec", source.ID, xmindWorkbookKey, topic.ID)
			topicLocations[topic.ID] = xmindTopicLocation{
				documentID: documentID,
				sectionID:  sectionID,
				nodeID:     ids.Stable("node", "section", sectionID),
				sheetID:    sheet.ID,
			}
			topicByID[topic.ID] = topic
		}
	}

	relationshipLabels := xmindRelationshipLabels(workbook)
	assetDocuments := make(map[string]map[string]bool)
	assetRefs := make([]storage.SectionMediaRefInput, 0)
	assetRefMetadata := make(map[string]string)
	for _, sheet := range workbook.Sheets {
		documentID := ids.Stable("doc", source.ID, xmindWorkbookKey, sheet.ID)
		docNodeID := ids.Stable("node", "document", documentID)
		documentTitle := firstNonEmptyString(sheet.Title, sheet.ID)
		docNode := storage.NodeInput{
			ID:            docNodeID,
			Kind:          "Document",
			Name:          documentTitle,
			CanonicalName: canonical(source.ID, sheet.ID),
			MetadataJSON: xmindJSON(map[string]any{
				"document_id": documentID, "source_id": source.ID, "sheet_id": sheet.ID,
				"origin": "xmind_sheet", "workbook_key": xmindWorkbookKey,
			}),
			Confidence: 1,
		}
		plan.bundle.Nodes = append(plan.bundle.Nodes, docNode)

		elementsByOwner := make(map[string][]xmind.Element)
		for _, element := range sheet.Elements {
			elementsByOwner[element.OwnerTopicID] = append(elementsByOwner[element.OwnerTopicID], element)
		}

		sections := make([]storage.SectionInput, 0)
		ordinal := 0
		var visit func(*xmind.Topic, []string) error
		visit = func(topic *xmind.Topic, ancestors []string) error {
			location := topicLocations[topic.ID]
			parentSectionID := ""
			if topic.ParentID != "" {
				parent, ok := topicLocations[topic.ParentID]
				if !ok {
					return fmt.Errorf("Topic %q has missing parent %q", topic.ID, topic.ParentID)
				}
				parentSectionID = parent.sectionID
			}
			elementText := make([]string, 0)
			for _, element := range elementsByOwner[topic.ID] {
				elementText = appendSourceText(elementText, element.Title)
			}
			content := xmindTopicContent(topic, elementText)
			assetText := make([]string, 0)
			for _, ref := range topic.Assets {
				asset, ok := assetByID[ref.AssetID]
				if !ok {
					return fmt.Errorf("Topic %q references missing media asset %q", topic.ID, ref.AssetID)
				}
				if assetDocuments[asset.ID] == nil {
					assetDocuments[asset.ID] = make(map[string]bool)
				}
				assetDocuments[asset.ID][documentID] = true
				logicalAssetID := xmindLogicalAssetID(source.ID, documentID, asset.ID)
				refMetadata := xmindJSON(map[string]any{
					"xmind_asset_id": asset.ID, "resource_path": asset.ResourcePath, "original_uri": asset.URI,
					"width": ref.Width, "height": ref.Height, "crop": rawJSONValue(ref.Crop), "align": ref.Align,
				})
				assetRefs = append(assetRefs, storage.SectionMediaRefInput{
					SectionID: location.sectionID, AssetID: logicalAssetID, Role: firstNonEmptyString(ref.Role, "media"),
					Ordinal: ref.Ordinal, MetadataJSON: refMetadata,
				})
				assetRefMetadata[logicalAssetID] = refMetadata
				assetText = appendSourceText(assetText, asset.OriginalName, asset.Caption, asset.AltText, asset.MediaType)
			}
			headingPath := strings.Join(append(append([]string(nil), ancestors...), topic.RawTitle), " / ")
			searchText := xmindTopicSearchText(topic, headingPath, content, relationshipLabels[topic.ID], assetText)
			presentation := xmindJSON(map[string]any{
				"numbering": topic.Numbering, "position": topic.Position, "child_role": topic.ChildRole,
			})
			metadata := xmindJSON(map[string]any{
				"origin": "xmind_topic", "xmind_topic_id": topic.ID, "sheet_id": sheet.ID,
				"topic_kind": firstNonEmptyString(topic.Kind, "topic"), "links": topic.Links,
				"notes": topic.Notes, "labels": topic.Labels, "markers": topic.Markers, "task": topic.Task,
			})
			section := storage.SectionInput{
				ID: location.sectionID, DocumentID: documentID, HeadingPath: headingPath,
				Title: topic.RawTitle, Content: content, SearchText: searchText,
				ContentHash: xmindSectionHash(topic.RawTitle, headingPath, content, searchText, metadata),
				Ordinal:     ordinal, MetadataJSON: metadata,
				Structure: &storage.SectionStructureInput{
					ParentSectionID: parentSectionID, SourceElementID: topic.ID,
					ElementKind: firstNonEmptyString(topic.Kind, "topic"), Depth: topic.Depth,
					SiblingOrdinal: topic.SiblingOrdinal, OrderPath: append([]int(nil), topic.OrderPath...),
					DisplayNumber: topic.Numbering.DisplayNumber, PresentationJSON: presentation,
				},
			}
			ordinal++
			sections = append(sections, section)
			plan.diagnostics.Topics++
			plan.bundle.Nodes = append(plan.bundle.Nodes, storage.NodeInput{
				ID: location.nodeID, Kind: "DocSection", Name: firstNonEmptyString(topic.RawTitle, topic.ID),
				CanonicalName: canonical(source.ID, topic.ID), MetadataJSON: xmindJSON(map[string]any{
					"origin": "xmind_topic", "xmind_topic_id": topic.ID, "section_id": location.sectionID,
					"document_id": documentID, "source_id": source.ID, "sheet_id": sheet.ID,
					"display_number": topic.Numbering.DisplayNumber, "element_kind": firstNonEmptyString(topic.Kind, "topic"),
				}), Confidence: 1,
			})
			plan.bundle.SectionNodes = append(plan.bundle.SectionNodes, storage.SectionNodeInput{
				SectionID: location.sectionID, NodeID: location.nodeID, Role: "primary", Confidence: 1,
			})

			if parentSectionID == "" {
				plan.bundle.Edges = append(plan.bundle.Edges, xmindOwnedEdge(
					ids.Stable("edge", source.ID, xmindWorkbookKey, "sheet_root", sheet.ID, topic.ID),
					docNodeID, location.nodeID, "contains", location.sectionID, workbook.SemanticHash,
					map[string]any{"origin": "xmind_sheet_root", "sheet_id": sheet.ID, "xmind_topic_id": topic.ID},
				))
			} else {
				parent := topicLocations[topic.ParentID]
				plan.bundle.Edges = append(plan.bundle.Edges, xmindOwnedEdge(
					ids.Stable("edge", source.ID, xmindWorkbookKey, "topic_parent", topic.ParentID, topic.ID),
					parent.nodeID, location.nodeID, "contains", location.sectionID, workbook.SemanticHash,
					map[string]any{"origin": "xmind_topic_hierarchy", "parent_topic_id": topic.ParentID, "child_topic_id": topic.ID},
				))
			}

			nextAncestors := append(append([]string(nil), ancestors...), topic.RawTitle)
			for _, child := range topic.Children {
				if err := visit(child, nextAncestors); err != nil {
					return err
				}
			}
			return nil
		}
		for _, root := range sheet.Roots {
			if err := visit(root, nil); err != nil {
				return xmindBundlePlan{}, err
			}
		}

		// Special authored elements are retained as Sections/DocSection Nodes in
		// addition to their explicit authored graph semantics.
		specialOrdinalByOwner := make(map[string]int)
		sheetSpecialOrdinal := 0
		for _, element := range sheet.Elements {
			if strings.TrimSpace(element.ID) == "" {
				return xmindBundlePlan{}, fmt.Errorf("Sheet %q has %s without a source ID", sheet.ID, element.Kind)
			}
			if element.OwnerTopicID != "" {
				if _, ok := topicLocations[element.OwnerTopicID]; !ok {
					return xmindBundlePlan{}, fmt.Errorf("XMind element %q has missing owner Topic %q", element.ID, element.OwnerTopicID)
				}
			}
			for _, targetID := range element.TargetIDs {
				if targetID != "" {
					if _, ok := topicLocations[targetID]; !ok {
						return xmindBundlePlan{}, fmt.Errorf("XMind element %q targets missing Topic %q", element.ID, targetID)
					}
				}
			}
			// Legacy Summary/Callout encodings can expose the same component both
			// as a Topic-like child and as a semantic Element. Reuse that Topic's
			// Section/Node instead of inventing a duplicate source element.
			if existing, ok := topicLocations[element.ID]; ok {
				edges := xmindElementEdges(source.ID, workbook.SemanticHash, element, existing.nodeID, existing.sectionID, topicLocations)
				plan.bundle.Edges = append(plan.bundle.Edges, edges...)
				plan.diagnostics.AuthoredEdges += len(edges)
				continue
			}
			sectionID := ids.Stable("sec", source.ID, xmindWorkbookKey, "element", element.ID)
			nodeID := ids.Stable("node", "section", sectionID)
			parentSectionID := ""
			depth := 0
			siblingOrdinal := len(sheet.Roots) + sheetSpecialOrdinal
			orderPath := []int{siblingOrdinal}
			if owner, ok := topicLocations[element.OwnerTopicID]; ok {
				parentSectionID = owner.sectionID
				ownerTopic := topicByID[element.OwnerTopicID]
				if ownerTopic == nil {
					return xmindBundlePlan{}, fmt.Errorf("XMind element %q owner Topic %q has no normalized structure", element.ID, element.OwnerTopicID)
				}
				depth = ownerTopic.Depth + 1
				siblingOrdinal = len(ownerTopic.Children) + specialOrdinalByOwner[element.OwnerTopicID]
				specialOrdinalByOwner[element.OwnerTopicID]++
				orderPath = append(append([]int(nil), ownerTopic.OrderPath...), siblingOrdinal)
			} else {
				sheetSpecialOrdinal++
			}
			metadata := xmindJSON(map[string]any{
				"origin": "xmind_" + element.Kind, "xmind_element_id": element.ID,
				"xmind_definition_id": element.DefinitionID,
				"owner_topic_id":      element.OwnerTopicID, "target_topic_ids": element.TargetIDs,
				"range": element.Range, "coverage_status": element.Status,
			})
			sections = append(sections, storage.SectionInput{
				ID: sectionID, DocumentID: documentID, Title: element.Title, HeadingPath: element.Title,
				Content: element.Title, SearchText: strings.TrimSpace(element.Title + " " + element.Kind),
				ContentHash: xmindSectionHash(element.Title, element.Kind, metadata), Ordinal: ordinal,
				MetadataJSON: metadata, Structure: &storage.SectionStructureInput{
					ParentSectionID: parentSectionID, SourceElementID: element.ID,
					ElementKind: firstNonEmptyString(element.Kind, "unknown"), Depth: depth,
					SiblingOrdinal: siblingOrdinal, OrderPath: orderPath, PresentationJSON: "{}",
				},
			})
			ordinal++
			plan.bundle.Nodes = append(plan.bundle.Nodes, storage.NodeInput{
				ID: nodeID, Kind: "DocSection", Name: firstNonEmptyString(element.Title, element.ID),
				CanonicalName: canonical(source.ID, "element", element.ID), MetadataJSON: metadata, Confidence: 1,
			})
			plan.bundle.SectionNodes = append(plan.bundle.SectionNodes, storage.SectionNodeInput{
				SectionID: sectionID, NodeID: nodeID, Role: "primary", Confidence: 1,
			})
			edges := xmindElementEdges(source.ID, workbook.SemanticHash, element, nodeID, sectionID, topicLocations)
			plan.bundle.Edges = append(plan.bundle.Edges, edges...)
			plan.diagnostics.AuthoredEdges += len(edges)
		}

		documentHash := xmindDocumentHash(sheet.ID, sections)
		plan.bundle.Documents = append(plan.bundle.Documents, storage.WorkbookDocumentInput{
			Document: storage.DocumentInput{
				ID: documentID, SourceID: source.ID, ExternalID: sheet.ID, Title: documentTitle,
				URL:     "",
				Version: workbook.SnapshotHash, ContentHash: documentHash,
				MetadataJSON: xmindJSON(map[string]any{
					"origin": "xmind_sheet", "sheet_id": sheet.ID, "workbook_key": xmindWorkbookKey,
					"snapshot_hash": workbook.SnapshotHash, "semantic_hash": workbook.SemanticHash,
				}),
			},
			Sections: sections,
		})
	}

	for _, sheet := range workbook.Sheets {
		for _, relationship := range sheet.Relationships {
			src, srcOK := topicLocations[relationship.SourceTopicID]
			dst, dstOK := topicLocations[relationship.TargetTopicID]
			if !srcOK || !dstOK {
				plan.diagnostics.Warnings = append(plan.diagnostics.Warnings,
					fmt.Sprintf("XMind Relationship %q was skipped because endpoint %q -> %q is unresolved", relationship.ID, relationship.SourceTopicID, relationship.TargetTopicID))
				continue
			}
			plan.bundle.Edges = append(plan.bundle.Edges, xmindOwnedEdge(
				ids.Stable("edge", source.ID, xmindWorkbookKey, relationship.ID),
				src.nodeID, dst.nodeID, "related_to", src.sectionID, workbook.SemanticHash,
				map[string]any{
					"origin": "xmind_relationship", "xmind_relationship_id": relationship.ID,
					"label": relationship.Label, "source_topic_id": relationship.SourceTopicID,
					"target_topic_id": relationship.TargetTopicID, "semantic_direction": relationship.SemanticDirection,
					"start_arrow": relationship.StartArrow, "end_arrow": relationship.EndArrow,
					"control_points": relationship.ControlPoints, "style": rawJSONValue(relationship.Style),
				},
			))
			plan.diagnostics.AuthoredEdges++
		}
		for _, topic := range flattenXMindTopics(sheet.Roots) {
			src := topicLocations[topic.ID]
			for index, link := range topic.Links {
				linkID := strings.TrimSpace(link.ID)
				if linkID == "" {
					linkID = ids.Stable("xmind-link", topic.ID, strconv.Itoa(index), link.Target)
				}
				dstNodeID := ""
				if link.TargetTopicID != "" {
					dst, ok := topicLocations[link.TargetTopicID]
					if ok {
						dstNodeID = dst.nodeID
					} else {
						dstNodeID = ids.Stable("node", "unresolved-xmind-link", source.ID, link.Target)
						plan.bundle.Nodes = appendUniqueXMindNode(plan.bundle.Nodes, storage.NodeInput{
							ID: dstNodeID, Kind: "ExternalResource", Name: firstNonEmptyString(link.Label, link.Target),
							CanonicalName: strings.TrimSpace(link.Target), MetadataJSON: xmindJSON(map[string]any{
								"origin": "xmind_topic_link", "target": link.Target, "unresolved_internal": true,
							}), Confidence: 1,
						})
						plan.diagnostics.Warnings = append(plan.diagnostics.Warnings,
							fmt.Sprintf("XMind Topic %q link target %q is unresolved and was preserved as a reference", topic.ID, link.TargetTopicID))
					}
				} else {
					dstNodeID = ids.Stable("node", "external", source.ID, link.Target)
					plan.bundle.Nodes = appendUniqueXMindNode(plan.bundle.Nodes, storage.NodeInput{
						ID: dstNodeID, Kind: "ExternalResource", Name: firstNonEmptyString(link.Label, link.Target),
						CanonicalName: strings.TrimSpace(link.Target), MetadataJSON: xmindJSON(map[string]any{
							"origin": "xmind_topic_link", "target": link.Target, "external": true,
						}), Confidence: 1,
					})
				}
				plan.bundle.Edges = append(plan.bundle.Edges, xmindOwnedEdge(
					ids.Stable("edge", source.ID, xmindWorkbookKey, linkID), src.nodeID, dstNodeID,
					"links_to", src.sectionID, workbook.SemanticHash, map[string]any{
						"origin": "xmind_topic_link", "xmind_link_id": linkID, "label": link.Label,
						"target": link.Target, "target_topic_id": link.TargetTopicID,
						"target_sheet_id": link.TargetSheetID, "external": link.External,
					},
				))
				plan.diagnostics.AuthoredEdges++
			}
		}
	}

	for assetID, documents := range assetDocuments {
		asset := assetByID[assetID]
		for documentID := range documents {
			logicalID := xmindLogicalAssetID(source.ID, documentID, asset.ID)
			status := string(asset.Status)
			blobSHA := ""
			if asset.ResourcePath != "" && asset.SHA256 != "" && asset.Status != xmind.InventoryRejected && asset.Status != xmind.InventoryUnsupported {
				status = "available"
				blobSHA = asset.SHA256
				plan.resources[asset.ResourcePath] = asset.SHA256
			} else if isExternalXMindAsset(asset) {
				status = "external"
			}
			plan.bundle.MediaAssets = append(plan.bundle.MediaAssets, storage.MediaAssetInput{
				ID: logicalID, SourceID: source.ID, SnapshotID: snapshotID, DocumentID: documentID,
				ExternalID: asset.ID, BlobSHA256: blobSHA, Kind: string(asset.Kind),
				OriginalName: asset.OriginalName, MediaType: asset.MediaType, SizeBytes: int64(asset.SizeBytes),
				Status: status, MetadataJSON: xmindJSON(map[string]any{
					"xmind_asset_id": asset.ID, "resource_path": asset.ResourcePath, "original_uri": asset.URI,
					"caption": asset.Caption, "alt_text": asset.AltText,
					"source_status": asset.Status, "placement": rawJSONValue(json.RawMessage(assetRefMetadata[logicalID])),
				}),
			})
			plan.diagnostics.MediaAssets++
			if blobSHA != "" {
				plan.diagnostics.MediaBytes += int64(asset.SizeBytes)
			}
		}
	}
	plan.bundle.MediaRefs = assetRefs
	plan.bundle.FeatureInventory = xmindFeatureInventory(snapshotID, workbook.FeatureInventory)
	plan.diagnostics.FeatureInventory = featureInventoryEntries(plan.bundle.FeatureInventory)
	if err := validateXMindRelationalPlan(plan.bundle); err != nil {
		return xmindBundlePlan{}, err
	}
	return plan, nil
}

// validateXMindRelationalPlan checks the connector-owned aggregate before any
// snapshot or media bytes are staged. SQLite repeats broader validation inside
// ReplaceWorkbookBundle, but parser/normalizer bugs such as an incomplete
// order_path must fail before BlobStore writes begin.
func validateXMindRelationalPlan(bundle storage.WorkbookBundle) error {
	documentIDs := make(map[string]struct{}, len(bundle.Documents))
	sectionDocuments := make(map[string]string)
	sectionDepths := make(map[string]int)
	sectionOrderPaths := make(map[string][]int)
	sourceElementIDs := make(map[string]struct{})
	for _, item := range bundle.Documents {
		documentID := strings.TrimSpace(item.Document.ID)
		if documentID == "" {
			return fmt.Errorf("XMind relational plan has a Document without ID")
		}
		if _, duplicate := documentIDs[documentID]; duplicate {
			return fmt.Errorf("XMind relational plan has duplicate Document ID %q", documentID)
		}
		documentIDs[documentID] = struct{}{}
		for _, section := range item.Sections {
			if strings.TrimSpace(section.ID) == "" || section.Structure == nil {
				return fmt.Errorf("XMind relational plan has an invalid Section in Document %q", documentID)
			}
			if _, duplicate := sectionDocuments[section.ID]; duplicate {
				return fmt.Errorf("XMind relational plan has duplicate Section ID %q", section.ID)
			}
			structure := section.Structure
			if structure.SourceElementID == "" {
				return fmt.Errorf("XMind Section %q has no source element ID", section.ID)
			}
			if _, duplicate := sourceElementIDs[structure.SourceElementID]; duplicate {
				return fmt.Errorf("XMind relational plan has duplicate source element ID %q", structure.SourceElementID)
			}
			if structure.Depth < 0 || structure.SiblingOrdinal < 0 || len(structure.OrderPath) != structure.Depth+1 || structure.OrderPath[len(structure.OrderPath)-1] != structure.SiblingOrdinal {
				return fmt.Errorf("XMind Section %q has invalid depth/sibling/order_path", section.ID)
			}
			sourceElementIDs[structure.SourceElementID] = struct{}{}
			sectionDocuments[section.ID] = documentID
			sectionDepths[section.ID] = structure.Depth
			sectionOrderPaths[section.ID] = append([]int(nil), structure.OrderPath...)
		}
	}
	if len(documentIDs) == 0 || len(sectionDocuments) == 0 {
		return fmt.Errorf("XMind relational plan must contain Documents and Sections")
	}
	for _, item := range bundle.Documents {
		for _, section := range item.Sections {
			structure := section.Structure
			if structure.ParentSectionID == "" {
				if structure.Depth != 0 {
					return fmt.Errorf("XMind root Section %q has depth %d", section.ID, structure.Depth)
				}
				continue
			}
			parentDocumentID, exists := sectionDocuments[structure.ParentSectionID]
			if !exists || parentDocumentID != item.Document.ID || sectionDepths[structure.ParentSectionID]+1 != structure.Depth {
				return fmt.Errorf("XMind Section %q has invalid parent/depth", section.ID)
			}
			parentOrderPath := sectionOrderPaths[structure.ParentSectionID]
			if !orderPathExtends(structure.OrderPath, parentOrderPath) {
				return fmt.Errorf("XMind Section %q has order_path that does not extend parent %q", section.ID, structure.ParentSectionID)
			}
		}
	}

	nodeIDs := make(map[string]struct{}, len(bundle.Nodes))
	for _, node := range bundle.Nodes {
		if node.ID == "" {
			return fmt.Errorf("XMind relational plan has a Node without ID")
		}
		if _, duplicate := nodeIDs[node.ID]; duplicate {
			return fmt.Errorf("XMind relational plan has duplicate Node ID %q", node.ID)
		}
		nodeIDs[node.ID] = struct{}{}
	}
	for _, mapping := range bundle.SectionNodes {
		if _, sectionExists := sectionDocuments[mapping.SectionID]; !sectionExists {
			return fmt.Errorf("XMind Section-Node mapping references missing Section %q", mapping.SectionID)
		}
		if _, nodeExists := nodeIDs[mapping.NodeID]; !nodeExists {
			return fmt.Errorf("XMind Section-Node mapping references missing Node %q", mapping.NodeID)
		}
	}
	edgeIDs := make(map[string]struct{}, len(bundle.Edges))
	for _, edge := range bundle.Edges {
		if edge.ID == "" {
			return fmt.Errorf("XMind relational plan has an Edge without ID")
		}
		if _, duplicate := edgeIDs[edge.ID]; duplicate {
			return fmt.Errorf("XMind relational plan has duplicate Edge ID %q", edge.ID)
		}
		if _, sourceExists := nodeIDs[edge.SrcID]; !sourceExists {
			return fmt.Errorf("XMind Edge %q references missing source Node %q", edge.ID, edge.SrcID)
		}
		if _, targetExists := nodeIDs[edge.DstID]; !targetExists {
			return fmt.Errorf("XMind Edge %q references missing target Node %q", edge.ID, edge.DstID)
		}
		if edge.EvidenceSectionID != "" {
			if _, sectionExists := sectionDocuments[edge.EvidenceSectionID]; !sectionExists {
				return fmt.Errorf("XMind Edge %q references missing evidence Section %q", edge.ID, edge.EvidenceSectionID)
			}
		}
		edgeIDs[edge.ID] = struct{}{}
	}

	assetIDs := make(map[string]struct{}, len(bundle.MediaAssets))
	for _, asset := range bundle.MediaAssets {
		if asset.ID == "" {
			return fmt.Errorf("XMind relational plan has a media asset without ID")
		}
		if _, duplicate := assetIDs[asset.ID]; duplicate {
			return fmt.Errorf("XMind relational plan has duplicate media asset ID %q", asset.ID)
		}
		if _, documentExists := documentIDs[asset.DocumentID]; !documentExists {
			return fmt.Errorf("XMind media asset %q references missing Document %q", asset.ID, asset.DocumentID)
		}
		assetIDs[asset.ID] = struct{}{}
	}
	for _, ref := range bundle.MediaRefs {
		if _, sectionExists := sectionDocuments[ref.SectionID]; !sectionExists {
			return fmt.Errorf("XMind media reference targets missing Section %q", ref.SectionID)
		}
		if _, assetExists := assetIDs[ref.AssetID]; !assetExists {
			return fmt.Errorf("XMind media reference targets missing asset %q", ref.AssetID)
		}
	}
	return nil
}

func orderPathExtends(child, parent []int) bool {
	if len(child) != len(parent)+1 {
		return false
	}
	for index := range parent {
		if child[index] != parent[index] {
			return false
		}
	}
	return true
}

func xmindParseStatus(inventory xmind.FeatureInventory) (status string, unsupported, rejected int) {
	for _, feature := range inventory.Features {
		switch feature.Status {
		case xmind.InventoryUnsupported:
			unsupported += int(feature.Count)
		case xmind.InventoryRejected:
			rejected += int(feature.Count)
		}
	}
	for _, resource := range inventory.Resources {
		switch resource.Status {
		case xmind.InventoryUnsupported:
			unsupported++
		case xmind.InventoryRejected:
			rejected++
		}
	}
	if unsupported > 0 || rejected > 0 {
		return "parsed_with_warnings", unsupported, rejected
	}
	return "parsed", 0, 0
}

func flattenXMindTopics(roots []*xmind.Topic) []*xmind.Topic {
	result := make([]*xmind.Topic, 0)
	var visit func(*xmind.Topic)
	visit = func(topic *xmind.Topic) {
		if topic == nil {
			result = append(result, nil)
			return
		}
		result = append(result, topic)
		for _, child := range topic.Children {
			visit(child)
		}
	}
	for _, root := range roots {
		visit(root)
	}
	return result
}

func xmindRelationshipLabels(workbook xmind.Workbook) map[string][]string {
	labels := make(map[string][]string)
	for _, sheet := range workbook.Sheets {
		for _, relationship := range sheet.Relationships {
			if strings.TrimSpace(relationship.Label) == "" {
				continue
			}
			labels[relationship.SourceTopicID] = append(labels[relationship.SourceTopicID], relationship.Label)
			labels[relationship.TargetTopicID] = append(labels[relationship.TargetTopicID], relationship.Label)
		}
	}
	return labels
}

func xmindTopicContent(topic *xmind.Topic, associatedElementText []string) string {
	lines := make([]string, 0)
	lines = appendSourceText(lines, topic.Notes.Plain)
	for _, label := range topic.Labels {
		lines = appendSourceText(lines, label)
	}
	for _, marker := range topic.Markers {
		lines = appendSourceText(lines, firstNonEmptyString(marker.ResolvedName, marker.ID))
	}
	lines = appendSourceText(lines, topic.Task.Status, topic.Task.Owner, topic.Task.Start, topic.Task.Due)
	for _, link := range topic.Links {
		lines = appendSourceText(lines, link.Label, link.Target)
	}
	lines = appendSourceText(lines, associatedElementText...)
	return strings.Join(lines, "\n")
}

func xmindTopicSearchText(topic *xmind.Topic, headingPath, content string, relationLabels, assetText []string) string {
	parts := make([]string, 0)
	parts = appendSourceText(parts, topic.RawTitle)
	parts = appendSourceText(parts, headingPath, content)
	parts = appendSourceText(parts, relationLabels...)
	parts = appendSourceText(parts, assetText...)
	return strings.Join(parts, "\n")
}

func appendSourceText(target []string, values ...string) []string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			target = append(target, value)
		}
	}
	return target
}

func xmindOwnedEdge(id, srcID, dstID, kind, evidenceSectionID, revision string, metadata map[string]any) storage.EdgeInput {
	return storage.EdgeInput{
		ID: id, SrcID: srcID, DstID: dstID, Kind: kind, Confidence: 1,
		Provenance: "source_authored", EvidenceSectionID: evidenceSectionID,
		SourceRevision: revision, MetadataJSON: xmindJSON(metadata),
	}
}

func xmindElementEdges(sourceID, revision string, element xmind.Element, elementNodeID, sectionID string, topics map[string]xmindTopicLocation) []storage.EdgeInput {
	edges := make([]storage.EdgeInput, 0)
	kind := strings.ToLower(strings.TrimSpace(element.Kind))
	sourceElementID := firstNonEmptyString(element.DefinitionID, element.ID)
	if kind == "callout" && element.OwnerTopicID != "" {
		if owner, ok := topics[element.OwnerTopicID]; ok {
			edges = append(edges, xmindOwnedEdge(
				ids.Stable("edge", sourceID, xmindWorkbookKey, sourceElementID, element.OwnerTopicID),
				elementNodeID, owner.nodeID, "annotates", sectionID, revision,
				map[string]any{"origin": "xmind_callout", "xmind_element_id": element.ID, "xmind_definition_id": element.DefinitionID, "owner_topic_id": element.OwnerTopicID},
			))
		}
	}
	edgeKind := ""
	switch kind {
	case "summary":
		edgeKind = "summarizes"
	case "boundary", "zone":
		edgeKind = "groups"
	}
	if edgeKind != "" {
		for _, targetID := range element.TargetIDs {
			if target, ok := topics[targetID]; ok {
				edges = append(edges, xmindOwnedEdge(
					ids.Stable("edge", sourceID, xmindWorkbookKey, sourceElementID, targetID),
					elementNodeID, target.nodeID, edgeKind, sectionID, revision,
					map[string]any{"origin": "xmind_" + kind, "xmind_element_id": element.ID, "xmind_definition_id": element.DefinitionID, "target_topic_id": targetID, "range": element.Range},
				))
			}
		}
	}
	return edges
}

func appendUniqueXMindNode(nodes []storage.NodeInput, candidate storage.NodeInput) []storage.NodeInput {
	for _, node := range nodes {
		if node.ID == candidate.ID {
			return nodes
		}
	}
	return append(nodes, candidate)
}

func xmindLogicalAssetID(sourceID, documentID, sourceAssetID string) string {
	return ids.Stable("asset", sourceID, xmindWorkbookKey, documentID, sourceAssetID)
}

func isExternalXMindAsset(asset xmind.Asset) bool {
	if asset.ResourcePath != "" {
		return false
	}
	parsed, err := url.Parse(strings.TrimSpace(asset.URI))
	return err == nil && (parsed.Scheme != "" || strings.HasPrefix(asset.URI, "//") || strings.HasPrefix(asset.URI, `\\`))
}

func xmindFeatureInventory(snapshotID string, inventory xmind.FeatureInventory) []storage.FeatureInventoryInput {
	const sampleLimit = 10
	type aggregate struct {
		feature     string
		status      string
		reason      string
		count       int
		paths       []string
		sourceNames []string
		elementIDs  []string
		details     []string
		rawSamples  []string
		truncated   bool
	}
	aggregates := make(map[string]*aggregate)
	addSample := func(values *[]string, value string, item *aggregate) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		for _, existing := range *values {
			if existing == value {
				return
			}
		}
		if len(*values) >= sampleLimit {
			item.truncated = true
			return
		}
		*values = append(*values, value)
	}
	for _, feature := range inventory.Features {
		key := strings.Join([]string{feature.Feature, string(feature.Status), feature.ReasonCode}, "\x00")
		item := aggregates[key]
		if item == nil {
			item = &aggregate{feature: feature.Feature, status: string(feature.Status), reason: feature.ReasonCode}
			aggregates[key] = item
		}
		item.count += int(feature.Count)
		addSample(&item.paths, feature.Location, item)
		addSample(&item.sourceNames, feature.SourceName, item)
		addSample(&item.elementIDs, feature.SourceElementID, item)
		addSample(&item.details, feature.Detail, item)
		if len(feature.Raw) > 0 {
			raw := string(feature.Raw)
			if len(raw) > 2048 {
				raw = raw[:2048]
				item.truncated = true
			}
			addSample(&item.rawSamples, raw, item)
		}
	}
	for _, resource := range inventory.Resources {
		referenced := strconv.FormatBool(resource.Referenced)
		key := strings.Join([]string{"package_resource", string(resource.Status), resource.ReasonCode, referenced, resource.DetectedMIME}, "\x00")
		item := aggregates[key]
		if item == nil {
			item = &aggregate{feature: "package_resource", status: string(resource.Status), reason: resource.ReasonCode}
			aggregates[key] = item
		}
		item.count++
		addSample(&item.paths, "zip:/"+resource.Path, item)
		addSample(&item.sourceNames, resource.DetectedMIME, item)
		resourceDetail := xmindJSON(map[string]any{
			"path": resource.Path, "sha256": resource.SHA256, "referenced": resource.Referenced,
			"reference_count": resource.ReferenceCount, "owning_topic_ids": resource.OwningTopicIDs,
			"roles": resource.Roles, "detected_mime": resource.DetectedMIME,
			"compressed_bytes": resource.CompressedBytes, "uncompressed_bytes": resource.UncompressedBytes,
		})
		addSample(&item.details, resourceDetail, item)
	}
	keys := make([]string, 0, len(aggregates))
	for key := range aggregates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]storage.FeatureInventoryInput, 0, len(keys))
	for _, key := range keys {
		item := aggregates[key]
		detailsTruncated := item.truncated || item.count > len(item.paths)
		metadata := xmindJSON(map[string]any{
			"reason_code": item.reason, "sample_paths": item.paths, "sample_source_names": item.sourceNames,
			"sample_source_element_ids": item.elementIDs, "sample_details": item.details,
			"sample_raw": item.rawSamples, "details_truncated": detailsTruncated,
		})
		elementPath := ""
		if len(item.paths) > 0 {
			elementPath = item.paths[0]
		}
		result = append(result, storage.FeatureInventoryInput{
			ID: ids.Stable("feature", snapshotID, "aggregate", key), SnapshotID: snapshotID,
			FeatureKey: item.feature, CoverageStatus: item.status, ElementPath: elementPath,
			Count: item.count, MetadataJSON: metadata,
		})
	}
	return result
}

func featureInventoryEntries(inputs []storage.FeatureInventoryInput) []storage.FeatureInventoryEntry {
	entries := make([]storage.FeatureInventoryEntry, 0, len(inputs))
	for _, input := range inputs {
		entries = append(entries, storage.FeatureInventoryEntry{
			ID: input.ID, SnapshotID: input.SnapshotID, FeatureKey: input.FeatureKey,
			CoverageStatus: input.CoverageStatus, ElementPath: input.ElementPath,
			Count: input.Count, MetadataJSON: input.MetadataJSON,
		})
	}
	return entries
}

func stageXMindResources(ctx context.Context, blobs XMindBlobStore, scope string, reader io.ReaderAt, size int64, limits xmind.Limits, resources map[string]string) ([]blobstore.Blob, error) {
	if len(resources) == 0 {
		return nil, nil
	}
	zr, err := zip.NewReader(reader, size)
	if err != nil {
		return nil, fmt.Errorf("reopen validated XMind ZIP: %w", err)
	}
	entries := make(map[string]*zip.File, len(zr.File))
	for _, file := range zr.File {
		entries[file.Name] = file
	}
	paths := make([]string, 0, len(resources))
	for resourcePath := range resources {
		paths = append(paths, resourcePath)
	}
	sort.Strings(paths)
	result := make([]blobstore.Blob, 0, len(paths))
	seenDigests := make(map[string]bool)
	for _, resourcePath := range paths {
		entry := entries[resourcePath]
		if entry == nil || entry.FileInfo().IsDir() {
			return nil, fmt.Errorf("XMind resource %q is missing after validation", resourcePath)
		}
		stream, err := entry.Open()
		if err != nil {
			return nil, fmt.Errorf("open XMind resource %q: %w", resourcePath, err)
		}
		blob, putErr := blobs.Put(ctx, scope, stream, int64(limits.MaxEntryBytes))
		closeErr := stream.Close()
		if putErr != nil {
			return nil, fmt.Errorf("preserve XMind resource %q: %w", resourcePath, putErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close XMind resource %q: %w", resourcePath, closeErr)
		}
		expected := strings.ToLower(strings.TrimSpace(resources[resourcePath]))
		if expected != "" && blob.SHA256 != expected {
			return nil, fmt.Errorf("XMind resource %q digest mismatch: parsed=%s staged=%s", resourcePath, expected, blob.SHA256)
		}
		if !seenDigests[blob.SHA256] {
			seenDigests[blob.SHA256] = true
			result = append(result, blob)
		}
	}
	return result, nil
}

func mediaBlobInput(blob blobstore.Blob) storage.MediaBlobInput {
	return storage.MediaBlobInput{
		SHA256: blob.SHA256, SizeBytes: blob.Size,
		SniffedMediaType: blob.MediaType, StorageKey: blob.StorageKey,
	}
}

type xmindBlobGarbageCollector interface {
	CollectGarbage(context.Context, time.Time, func(context.Context, string) (bool, error)) (blobstore.GCResult, error)
}

type xmindBlobReferenceGuard interface {
	BeginReferenceUpdate() func()
}

type xmindBlobReferenceStore interface {
	IsMediaBlobReferenced(context.Context, string) (bool, error)
}

func (s *Service) collectXMindGarbage(ctx context.Context) string {
	collector, ok := s.xmind.blobs.(xmindBlobGarbageCollector)
	if !ok {
		return ""
	}
	references, ok := s.store.(xmindBlobReferenceStore)
	if !ok {
		return "Blob GC deferred: storage backend cannot check current references"
	}
	cutoff := time.Now().Add(-xmindBlobGCGracePeriod)
	result, err := collector.CollectGarbage(ctx, cutoff, references.IsMediaBlobReferenced)
	if err != nil {
		return "Blob GC deferred: " + err.Error()
	}
	if mediaStore, ok := s.store.(storage.MediaStore); ok {
		cutoffText := cutoff.UTC().Format("2006-01-02 15:04:05")
		for _, digest := range result.DeletedDigests {
			if _, err := mediaStore.DeleteMediaBlobIfUnreferenced(ctx, digest, cutoffText); err != nil {
				return "Blob metadata GC deferred: " + err.Error()
			}
		}
		candidates, err := mediaStore.ListUnreferencedMediaBlobs(ctx, cutoffText, 1000)
		if err != nil {
			return "Blob metadata GC deferred: " + err.Error()
		}
		for _, candidate := range candidates {
			if _, err := mediaStore.DeleteMediaBlobIfUnreferenced(ctx, candidate.SHA256, cutoffText); err != nil {
				return "Blob metadata GC deferred: " + err.Error()
			}
		}
	}
	return ""
}

func xmindDocumentHash(sheetID string, sections []storage.SectionInput) string {
	hash := sha256.New()
	writeHashField(hash, sheetID)
	for _, section := range sections {
		writeHashField(hash, section.ID)
		writeHashField(hash, section.ContentHash)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func xmindSectionHash(fields ...string) string {
	hash := sha256.New()
	for _, field := range fields {
		writeHashField(hash, field)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func writeHashField(writer io.Writer, value string) {
	_, _ = io.WriteString(writer, strconv.Itoa(len(value)))
	_, _ = io.WriteString(writer, ":")
	_, _ = io.WriteString(writer, value)
}

func xmindJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func rawJSONValue(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	return string(raw)
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

var _ = errors.Is
