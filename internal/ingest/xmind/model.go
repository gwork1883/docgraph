package xmind

import "encoding/json"

type FormatFamily string

const (
	FormatUnknown        FormatFamily = "unknown"
	FormatXMind8XML      FormatFamily = "xmind8_xml"
	FormatClassicZenJSON FormatFamily = "classic_zen_json"
	FormatV2602Plus      FormatFamily = "v26_02_plus"

	// FormatJSONCandidate deliberately does not claim a known JSON structure.
	// Unambiguously selected legacy Sheet-array payloads are promoted to
	// FormatClassicZenJSON; structurally different payloads remain fail-closed.
	FormatJSONCandidate FormatFamily = "json_candidate"
)

type Workbook struct {
	FormatFamily      FormatFamily     `json:"format_family"`
	FormatVersion     string           `json:"format_version,omitempty"`
	SnapshotHash      string           `json:"snapshot_hash"`
	SemanticHash      string           `json:"semantic_hash"`
	MediaManifestHash string           `json:"media_manifest_hash"`
	Sheets            []Sheet          `json:"sheets"`
	Assets            []Asset          `json:"assets"`
	FeatureInventory  FeatureInventory `json:"feature_inventory"`
}

type Sheet struct {
	ID            string         `json:"id"`
	Title         string         `json:"title"`
	Roots         []*Topic       `json:"roots"`
	Relationships []Relationship `json:"relationships"`
	Elements      []Element      `json:"elements"`
	RawFields     []RawField     `json:"raw_fields,omitempty"`
}

type Topic struct {
	ID             string          `json:"id"`
	ParentID       string          `json:"parent_id,omitempty"`
	Kind           string          `json:"kind"`
	ChildRole      string          `json:"child_role,omitempty"`
	RawTitle       string          `json:"raw_title"`
	Depth          int             `json:"depth"`
	SiblingOrdinal int             `json:"sibling_ordinal"`
	OrderPath      []int           `json:"order_path"`
	Notes          RichText        `json:"notes"`
	Labels         []string        `json:"labels"`
	Markers        []Marker        `json:"markers"`
	Numbering      Numbering       `json:"numbering"`
	Children       []*Topic        `json:"children"`
	Assets         []AssetRef      `json:"assets"`
	Links          []Link          `json:"links"`
	Position       Position        `json:"position"`
	Task           Task            `json:"task"`
	RawFields      []RawField      `json:"raw_fields,omitempty"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
}

type RichText struct {
	Plain  string          `json:"plain"`
	Format string          `json:"format,omitempty"`
	Raw    json.RawMessage `json:"raw,omitempty"`
	RawXML string          `json:"raw_xml,omitempty"`
}

type Marker struct {
	ID           string `json:"id"`
	ResolvedName string `json:"resolved_name,omitempty"`
	Group        string `json:"group,omitempty"`
}

type Numbering struct {
	Enabled           bool            `json:"enabled"`
	Format            string          `json:"format,omitempty"`
	Prefix            string          `json:"prefix,omitempty"`
	Suffix            string          `json:"suffix,omitempty"`
	Tiered            bool            `json:"tiered"`
	Restart           bool            `json:"restart"`
	PrependingNumbers string          `json:"prepending_numbers,omitempty"`
	DisplayNumber     string          `json:"display_number,omitempty"`
	Raw               json.RawMessage `json:"raw,omitempty"`
}

type Position struct {
	X        float64         `json:"x,omitempty"`
	Y        float64         `json:"y,omitempty"`
	Width    float64         `json:"width,omitempty"`
	Height   float64         `json:"height,omitempty"`
	Floating bool            `json:"floating"`
	Raw      json.RawMessage `json:"raw,omitempty"`
}

type Task struct {
	Status   string          `json:"status,omitempty"`
	Owner    string          `json:"owner,omitempty"`
	Start    string          `json:"start,omitempty"`
	Due      string          `json:"due,omitempty"`
	Progress float64         `json:"progress,omitempty"`
	Raw      json.RawMessage `json:"raw,omitempty"`
}

type AssetKind string

const (
	AssetImage      AssetKind = "image"
	AssetAttachment AssetKind = "attachment"
	AssetSnapshot   AssetKind = "snapshot"
	AssetUnknown    AssetKind = "unknown"
)

type Asset struct {
	ID           string          `json:"id"`
	Kind         AssetKind       `json:"kind"`
	ResourcePath string          `json:"resource_path,omitempty"`
	URI          string          `json:"uri,omitempty"`
	OriginalName string          `json:"original_name,omitempty"`
	MediaType    string          `json:"media_type,omitempty"`
	SHA256       string          `json:"sha256,omitempty"`
	SizeBytes    uint64          `json:"size_bytes,omitempty"`
	Caption      string          `json:"caption,omitempty"`
	AltText      string          `json:"alt_text,omitempty"`
	Status       InventoryStatus `json:"status"`
	RawFields    []RawField      `json:"raw_fields,omitempty"`
}

type AssetRef struct {
	AssetID   string          `json:"asset_id"`
	Role      string          `json:"role"`
	Ordinal   int             `json:"ordinal"`
	Width     float64         `json:"width,omitempty"`
	Height    float64         `json:"height,omitempty"`
	Crop      json.RawMessage `json:"crop,omitempty"`
	Align     string          `json:"align,omitempty"`
	RawFields []RawField      `json:"raw_fields,omitempty"`
}

type Link struct {
	ID            string     `json:"id,omitempty"`
	Label         string     `json:"label,omitempty"`
	Target        string     `json:"target"`
	TargetTopicID string     `json:"target_topic_id,omitempty"`
	TargetSheetID string     `json:"target_sheet_id,omitempty"`
	External      bool       `json:"external"`
	RawFields     []RawField `json:"raw_fields,omitempty"`
}

type Relationship struct {
	ID                string          `json:"id"`
	Label             string          `json:"label,omitempty"`
	SourceTopicID     string          `json:"source_topic_id"`
	TargetTopicID     string          `json:"target_topic_id"`
	SemanticDirection string          `json:"semantic_direction,omitempty"`
	StartArrow        string          `json:"start_arrow,omitempty"`
	EndArrow          string          `json:"end_arrow,omitempty"`
	ControlPoints     []Point         `json:"control_points"`
	Style             json.RawMessage `json:"style,omitempty"`
	RawFields         []RawField      `json:"raw_fields,omitempty"`
}

type Point struct {
	Key    string          `json:"key,omitempty"`
	X      float64         `json:"x,omitempty"`
	Y      float64         `json:"y,omitempty"`
	Angle  float64         `json:"angle,omitempty"`
	Amount float64         `json:"amount,omitempty"`
	Raw    json.RawMessage `json:"raw,omitempty"`
}

// Element preserves Summary, Boundary, Callout, Zone, and other topic-like
// structures without forcing adapter-specific source fields into Topic.
type Element struct {
	ID           string          `json:"id"`
	DefinitionID string          `json:"definition_id,omitempty"`
	Kind         string          `json:"kind"`
	Title        string          `json:"title,omitempty"`
	OwnerTopicID string          `json:"owner_topic_id,omitempty"`
	TargetIDs    []string        `json:"target_ids,omitempty"`
	Range        string          `json:"range,omitempty"`
	Status       InventoryStatus `json:"status"`
	Raw          json.RawMessage `json:"raw,omitempty"`
	RawFields    []RawField      `json:"raw_fields,omitempty"`
}

type RawField struct {
	Name     string          `json:"name"`
	Location string          `json:"location"`
	Encoding string          `json:"encoding"`
	JSON     json.RawMessage `json:"json,omitempty"`
	XML      string          `json:"xml,omitempty"`
}
