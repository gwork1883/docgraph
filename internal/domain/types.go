package domain

import "errors"

var ErrSyncInProgress = errors.New("sync already in progress")

type Source struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	DSN          string `json:"dsn"`
	ConfigJSON   string `json:"config_json"`
	ProductHint  string `json:"product_hint"`
	ModuleHint   string `json:"module_hint"`
	SyncSchedule string `json:"sync_schedule"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

type SyncJob struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Status      string `json:"status"`
	PayloadJSON string `json:"payload_json"`
	Attempts    int    `json:"attempts"`
	RunAfter    string `json:"run_after"`
	LockedUntil string `json:"locked_until"`
	LastError   string `json:"last_error"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

type ResultPayload struct {
	Documents   int          `json:"documents"`
	BrokenLinks []BrokenLink `json:"broken_links,omitempty"`
}

type BrokenLink struct {
	SourceDocument string `json:"source_document"`
	SourceSection  string `json:"source_section,omitempty"`
	Href           string `json:"href"`
	Text           string `json:"text,omitempty"`
	ResolvedTarget string `json:"resolved_target,omitempty"`
}

type DocumentInput struct {
	ID          string
	SourceID    string
	ExternalID  string
	Title       string
	URL         string
	Version     string
	ContentHash string
}

type SectionInput struct {
	ID          string
	DocumentID  string
	HeadingPath string
	Title       string
	Content     string
	ContentHash string
	Ordinal     int
}

type DocumentProfile struct {
	DocumentID           string `json:"document_id"`
	Desc                 string `json:"desc"`
	RetrievalProfileJSON string `json:"retrieval_profile_json"`
	GeneratedFromHash    string `json:"generated_from_hash"`
	GeneratedAt          string `json:"generated_at"`
	CreatedAt            string `json:"created_at"`
	UpdatedAt            string `json:"updated_at"`
}

type DocumentProfileInput struct {
	DocumentID string
	Desc       string
}

type RetrievalProfileInput struct {
	DocumentID           string
	RetrievalProfileJSON string
	GeneratedFromHash    string
}

type SearchHit struct {
	SectionID        string            `json:"section_id"`
	DocumentID       string            `json:"document_id"`
	DocumentTitle    string            `json:"document_title"`
	DocumentURL      string            `json:"document_url"`
	Desc             string            `json:"desc,omitempty"`
	Canonical        bool              `json:"canonical"`
	Title            string            `json:"title"`
	HeadingPath      string            `json:"heading_path"`
	Content          string            `json:"content,omitempty"`
	Snippet          string            `json:"snippet"`
	Rank             float64           `json:"rank"`
	Profile          *SearchHitProfile `json:"profile,omitempty"`
	RetrievalProfile any               `json:"retrieval_profile,omitempty"`
	QueryMatch       *QueryMatch       `json:"query_match,omitempty"`
}

type SearchHitProfile struct {
	TopTags           []string      `json:"top_tags,omitempty"`
	TopTerms          []ProfileTerm `json:"top_terms,omitempty"`
	MatchedTerms      []string      `json:"matched_terms,omitempty"`
	MatchedKeyphrases []string      `json:"matched_keyphrases,omitempty"`
}

type ProfileTerm struct {
	Term        string  `json:"term"`
	TF          int     `json:"tf"`
	Sections    int     `json:"sections"`
	HeadingHits int     `json:"heading_hits"`
	TitleHits   int     `json:"title_hits"`
	Score       float64 `json:"score"`
}

type QueryMatch struct {
	MatchedTerms     []string `json:"matched_terms,omitempty"`
	MatchedFields    []string `json:"matched_fields,omitempty"`
	SearchAttempts   []string `json:"search_attempts,omitempty"`
	ScoreExplanation string   `json:"score_explanation,omitempty"`
}

type SearchOptions struct {
	Query                  string
	Limit                  int
	MaxSearches            int
	MaxSectionsPerDocument int
	ProfileDetail          string
	MaxCharsPerResult      int
	Detail                 string // "summary" (default) or "content"
}

type SearchAttempt struct {
	Kind  string   `json:"kind"`
	Query string   `json:"query,omitempty"`
	Terms []string `json:"terms,omitempty"`
	Hits  int      `json:"hits"`
}

type SearchResult struct {
	Query        string          `json:"query"`
	SearchesUsed int             `json:"searches_used"`
	Attempts     []SearchAttempt `json:"attempts"`
	Hits         []SearchHit     `json:"hits"`
}

type DocumentSummary struct {
	ID           string `json:"id"`
	SourceID     string `json:"source_id"`
	ExternalID   string `json:"external_id"`
	Title        string `json:"title"`
	URL          string `json:"url"`
	ContentHash  string `json:"content_hash"`
	IndexedAt    string `json:"indexed_at"`
	SectionCount int    `json:"section_count"`
	NodeID       string `json:"node_id"`
}

type SectionSummary struct {
	ID             string `json:"id"`
	DocumentID     string `json:"document_id"`
	DocumentTitle  string `json:"document_title"`
	Title          string `json:"title"`
	HeadingPath    string `json:"heading_path"`
	ContentSnippet string `json:"content_snippet"`
	Ordinal        int    `json:"ordinal"`
	NodeID         string `json:"node_id"`
}

type EdgeSummary struct {
	ID                string  `json:"id"`
	SrcID             string  `json:"src_id"`
	SrcName           string  `json:"src_name"`
	SrcKind           string  `json:"src_kind"`
	DstID             string  `json:"dst_id"`
	DstName           string  `json:"dst_name"`
	DstKind           string  `json:"dst_kind"`
	Kind              string  `json:"kind"`
	Confidence        float64 `json:"confidence"`
	Provenance        string  `json:"provenance"`
	EvidenceSectionID string  `json:"evidence_section_id"`
}

type SourceArtifactCounts struct {
	Documents int64 `json:"documents"`
	Sections  int64 `json:"sections"`
	Nodes     int64 `json:"nodes"`
	Edges     int64 `json:"edges"`
}

type SourceArtifacts struct {
	SourceID  string               `json:"source_id"`
	Counts    SourceArtifactCounts `json:"counts"`
	Documents []DocumentSummary    `json:"documents"`
	Sections  []SectionSummary     `json:"sections"`
	Nodes     []Node               `json:"nodes"`
	Edges     []EdgeSummary        `json:"edges"`
}

type NodeInput struct {
	ID            string
	Kind          string
	Name          string
	CanonicalName string
	MetadataJSON  string
	Confidence    float64
}

type Node struct {
	ID            string  `json:"id"`
	Kind          string  `json:"kind"`
	Name          string  `json:"name"`
	CanonicalName string  `json:"canonical_name"`
	MetadataJSON  string  `json:"metadata_json"`
	Confidence    float64 `json:"confidence"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
}

type EdgeInput struct {
	ID                string
	SrcID             string
	DstID             string
	Kind              string
	Confidence        float64
	Provenance        string
	EvidenceSectionID string
	SourceRevision    string
	MetadataJSON      string
}

type Edge struct {
	ID                string  `json:"id"`
	SrcID             string  `json:"src_id"`
	DstID             string  `json:"dst_id"`
	Kind              string  `json:"kind"`
	Confidence        float64 `json:"confidence"`
	Provenance        string  `json:"provenance"`
	EvidenceSectionID string  `json:"evidence_section_id"`
	SourceRevision    string  `json:"source_revision"`
	MetadataJSON      string  `json:"metadata_json"`
	CreatedAt         string  `json:"created_at"`
	UpdatedAt         string  `json:"updated_at"`
}

type RelatedOptions struct {
	Direction string
	Kind      string
	Limit     int
}

type RelatedNode struct {
	Node      Node   `json:"node"`
	Edge      Edge   `json:"edge"`
	Direction string `json:"direction"`
}

type ImpactOptions struct {
	Direction string
	Kind      string
	MaxDepth  int
	Limit     int
}

type ImpactPath struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

type ImpactResult struct {
	StartNode Node         `json:"start_node"`
	Paths     []ImpactPath `json:"paths"`
}

type FeedbackEventInput struct {
	ID           string
	TargetKind   string
	TargetID     string
	FeedbackKind string
	PayloadJSON  string
	Actor        string
}

type FeedbackEvent struct {
	ID           string `json:"id"`
	TargetKind   string `json:"target_kind"`
	TargetID     string `json:"target_id"`
	FeedbackKind string `json:"feedback_kind"`
	PayloadJSON  string `json:"payload_json"`
	Actor        string `json:"actor"`
	CreatedAt    string `json:"created_at"`
}

type FeedbackListOptions struct {
	TargetKind   string
	TargetID     string
	FeedbackKind string
	Limit        int
}

// SectionContent holds the full content of a single documentation section,
// including its parent document metadata.
type SectionContent struct {
	SectionID     string `json:"section_id"`
	DocumentID    string `json:"document_id"`
	DocumentTitle string `json:"document_title"`
	DocumentURL   string `json:"document_url"`
	Title         string `json:"title"`
	HeadingPath   string `json:"heading_path"`
	Content       string `json:"content"`
}
