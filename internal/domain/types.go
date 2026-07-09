package domain

import "errors"

var ErrSyncInProgress = errors.New("sync already in progress")
var ErrJobNotCancelable = errors.New("job is not cancelable")

type Source struct {
	ID               string `json:"id"`
	Kind             string `json:"kind"`
	Name             string `json:"name"`
	DSN              string `json:"dsn"`
	ConfigJSON       string `json:"config_json"`
	ProductHint      string `json:"product_hint"`
	ModuleHint       string `json:"module_hint"`
	SyncSchedule     string `json:"sync_schedule"`
	SyncStatus       string `json:"sync_status"`
	SyncStatusReason string `json:"sync_status_reason"`
	SyncPausedAt     string `json:"sync_paused_at"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
}

type ConfluenceCookieCredential struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	BaseURL         string `json:"base_url"`
	Cookie          string `json:"cookie,omitempty"`
	Notes           string `json:"notes"`
	Status          string `json:"status"`
	LastValidatedAt string `json:"last_validated_at"`
	LastError       string `json:"last_error"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

type SyncJob struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	Status       string `json:"status"`
	SourceID     string `json:"source_id"`
	TargetKind   string `json:"target_kind"`
	TargetID     string `json:"target_id"`
	PayloadJSON  string `json:"payload_json"`
	ProgressJSON string `json:"progress_json"`
	ResultJSON   string `json:"result_json"`
	WorkerID     string `json:"worker_id"`
	Attempts     int    `json:"attempts"`
	RunAfter     string `json:"run_after"`
	LockedUntil  string `json:"locked_until"`
	LastError    string `json:"last_error"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

type Job = SyncJob

type JobInput struct {
	Kind         string
	SourceID     string
	TargetKind   string
	TargetID     string
	PayloadJSON  string
	ProgressJSON string
	ResultJSON   string
	RunAfter     string
}

type JobListOptions struct {
	SourceID   string
	Kind       string
	Status     string
	TargetKind string
	TargetID   string
	Limit      int
	Offset     int
}

type ResultPayload struct {
	Documents         int               `json:"documents"`
	EntityDiagnostics EntityDiagnostics `json:"entity_diagnostics,omitempty"`
	BrokenLinks       []BrokenLink      `json:"broken_links,omitempty"`
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

type SectionEntityInput struct {
	SectionID     string
	Kind          string
	RawText       string
	CanonicalText string
	Method        string
	Path          string
	Operation     string
	Source        string
	Confidence    float64
	Evidence      string
	SpanStart     int
	SpanEnd       int
	Notes         []string
}

type SectionEntity struct {
	ID            string  `json:"id"`
	SectionID     string  `json:"section_id"`
	DocumentID    string  `json:"document_id"`
	Kind          string  `json:"kind"`
	RawText       string  `json:"raw_text"`
	CanonicalText string  `json:"canonical_text"`
	Method        string  `json:"method"`
	Path          string  `json:"path"`
	Operation     string  `json:"operation"`
	Source        string  `json:"source"`
	Confidence    float64 `json:"confidence"`
	EvidenceJSON  string  `json:"evidence_json"`
	CreatedAt     string  `json:"created_at"`
}

type MatchedEntity struct {
	ID            string  `json:"id,omitempty"`
	SectionID     string  `json:"section_id,omitempty"`
	DocumentID    string  `json:"document_id,omitempty"`
	Kind          string  `json:"kind"`
	CanonicalText string  `json:"canonical"`
	Method        string  `json:"method,omitempty"`
	Path          string  `json:"path,omitempty"`
	Operation     string  `json:"operation,omitempty"`
	Source        string  `json:"source,omitempty"`
	Confidence    float64 `json:"confidence,omitempty"`
	MatchMode     string  `json:"match_mode,omitempty"`
}

type EntityDiagnostics struct {
	Total             int               `json:"total"`
	APIEndpoints      int               `json:"api_endpoints"`
	PathLiterals      int               `json:"path_literals"`
	Operations        int               `json:"operations"`
	DocumentsWithAny  int               `json:"documents_with_any"`
	DocumentsWithAPI  int               `json:"documents_with_api"`
	DocumentsPathOnly int               `json:"documents_path_only"`
	SectionsWithAny   int               `json:"sections_with_any"`
	TopPathOnlyDocs   []DocumentSummary `json:"top_path_only_documents,omitempty"`
}

type SearchHit struct {
	SectionID              string            `json:"section_id"`
	DocumentID             string            `json:"document_id"`
	DocumentTitle          string            `json:"document_title"`
	DocumentURL            string            `json:"document_url"`
	Desc                   string            `json:"desc,omitempty"`
	Canonical              bool              `json:"canonical"`
	Title                  string            `json:"title"`
	HeadingPath            string            `json:"heading_path"`
	Content                string            `json:"content,omitempty"`
	Snippet                string            `json:"snippet"`
	Rank                   float64           `json:"rank"`
	HasExplicitReferences  bool              `json:"has_explicit_references"`
	ExplicitReferenceCount int               `json:"explicit_reference_count"`
	Profile                *SearchHitProfile `json:"profile,omitempty"`
	RetrievalProfile       any               `json:"retrieval_profile,omitempty"`
	QueryMatch             *QueryMatch       `json:"query_match,omitempty"`
	ScoreBreakdown         *ScoreBreakdown   `json:"score_breakdown,omitempty"`
	MatchedEntities        []MatchedEntity   `json:"matched_entities,omitempty"`
	RelationMatches        []RelationMatch   `json:"relation_matches,omitempty"`
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

type ScoreBreakdown struct {
	UnicodeBM25Boost float64  `json:"unicode_bm25_boost,omitempty"`
	TrigramBM25Boost float64  `json:"trigram_bm25_boost,omitempty"`
	TitleBoost       float64  `json:"title_boost,omitempty"`
	SectionBoost     float64  `json:"section_boost,omitempty"`
	SymbolBoost      float64  `json:"symbol_boost,omitempty"`
	ExactMatchBoost  float64  `json:"exact_match_boost,omitempty"`
	CanonicalBoost   float64  `json:"canonical_boost,omitempty"`
	CoverageBoost    float64  `json:"coverage_boost,omitempty"`
	FallbackBoost    float64  `json:"fallback_boost,omitempty"`
	Total            float64  `json:"total"`
	MatchedFields    []string `json:"matched_fields,omitempty"`
	MatchedTerms     []string `json:"matched_terms,omitempty"`
	MatchedSymbols   []string `json:"matched_symbols,omitempty"`
}

type SearchOptions struct {
	Query                  string
	Limit                  int
	MaxSearches            int
	MaxSectionsPerDocument int
	ProfileDetail          string
	MaxCharsPerResult      int
	Detail                 string // "summary" (default) or "content"
	UseRelationExpansion   bool
	RelationDepth          int
	RelationTypes          []string
}

type SearchAttempt struct {
	Kind  string   `json:"kind"`
	Query string   `json:"query,omitempty"`
	Terms []string `json:"terms,omitempty"`
	Hits  int      `json:"hits"`
}

type SearchResult struct {
	Query          string          `json:"query"`
	SearchesUsed   int             `json:"searches_used"`
	Attempts       []SearchAttempt `json:"attempts"`
	Hits           []SearchHit     `json:"hits"`
	SuggestedReads SuggestedReads  `json:"suggested_reads"`
}

type SuggestedReads struct {
	ExplicitReferences  []ExplicitReference `json:"explicit_references"`
	ImplicitSymbolLinks []SuggestedRead     `json:"implicit_symbol_links"`
	CuratedRelations    []SuggestedRead     `json:"curated_relations"`
	StructuralNeighbors []SuggestedRead     `json:"structural_neighbors"`
}

type SuggestedRead struct {
	SourceSectionID  string  `json:"source_section_id,omitempty"`
	TargetSectionID  string  `json:"target_section_id,omitempty"`
	TargetDocumentID string  `json:"target_document_id,omitempty"`
	Reason           string  `json:"reason,omitempty"`
	Confidence       float64 `json:"confidence,omitempty"`
}

type KnowledgeRelationProposalInput struct {
	ID             string
	RelationType   string
	FromDocumentID string
	FromAnchor     string
	ToDocumentID   string
	ToAnchor       string
	Direction      string
	Reason         string
	EvidenceJSON   string
	ProposedEffect string
	Confidence     float64
	CreatedByType  string
	CreatedByRef   string
}

type KnowledgeRelationProposal struct {
	ID             string  `json:"id"`
	RelationType   string  `json:"relation_type"`
	FromDocumentID string  `json:"from_document_id"`
	FromAnchor     string  `json:"from_anchor"`
	ToDocumentID   string  `json:"to_document_id"`
	ToAnchor       string  `json:"to_anchor"`
	Direction      string  `json:"direction"`
	Reason         string  `json:"reason"`
	EvidenceJSON   string  `json:"evidence_json"`
	ProposedEffect string  `json:"proposed_effect"`
	Confidence     float64 `json:"confidence"`
	CreatedByType  string  `json:"created_by_type"`
	CreatedByRef   string  `json:"created_by_ref"`
	Status         string  `json:"status"`
	ReviewedBy     string  `json:"reviewed_by"`
	ReviewNote     string  `json:"review_note"`
	ReviewedAt     string  `json:"reviewed_at"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
}

type KnowledgeRelationProposalListOptions struct {
	Status     string
	DocumentID string
	Limit      int
	Offset     int
}

type KnowledgeRelationInput struct {
	ID                     string
	RelationType           string
	FromDocumentID         string
	FromAnchor             string
	ToDocumentID           string
	ToAnchor               string
	Direction              string
	Effect                 string
	Weight                 float64
	Reason                 string
	EvidenceJSON           string
	ApprovedFromProposalID string
	CreatedBy              string
}

type KnowledgeRelation struct {
	ID                     string  `json:"id"`
	RelationType           string  `json:"relation_type"`
	FromDocumentID         string  `json:"from_document_id"`
	FromAnchor             string  `json:"from_anchor"`
	ToDocumentID           string  `json:"to_document_id"`
	ToAnchor               string  `json:"to_anchor"`
	Direction              string  `json:"direction"`
	Effect                 string  `json:"effect"`
	Weight                 float64 `json:"weight"`
	Reason                 string  `json:"reason"`
	EvidenceJSON           string  `json:"evidence_json"`
	ApprovedFromProposalID string  `json:"approved_from_proposal_id"`
	CreatedBy              string  `json:"created_by"`
	DisabledAt             string  `json:"disabled_at"`
	CreatedAt              string  `json:"created_at"`
	UpdatedAt              string  `json:"updated_at"`
}

type KnowledgeRelationListOptions struct {
	DocumentID      string
	RelationTypes   []string
	IncludeDisabled bool
	Limit           int
}

type RelationMatch struct {
	RelationID       string  `json:"relation_id"`
	RelationType     string  `json:"relation_type"`
	SourceDocumentID string  `json:"source_document_id"`
	TargetDocumentID string  `json:"target_document_id"`
	Direction        string  `json:"direction"`
	Effect           string  `json:"effect"`
	Weight           float64 `json:"weight"`
	Reason           string  `json:"reason"`
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

type DocumentDetail struct {
	Document    DocumentSummary       `json:"document"`
	Source      Source                `json:"source"`
	Profile     DocumentProfile       `json:"profile"`
	Sections    []SectionSummary      `json:"sections"`
	Entities    []SectionEntity       `json:"entities,omitempty"`
	Feedback    []FeedbackEvent       `json:"feedback"`
	Related     []RelatedNode         `json:"related"`
	Diagnostics []SourceHealthWarning `json:"diagnostics"`
	LatestJob   Job                   `json:"latest_job"`
	BrokenLinks []BrokenLink          `json:"broken_links"`
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
	Documents       int64 `json:"documents"`
	Sections        int64 `json:"sections"`
	Nodes           int64 `json:"nodes"`
	Edges           int64 `json:"edges"`
	SectionEntities int64 `json:"section_entities"`
}

type SourceArtifacts struct {
	SourceID          string               `json:"source_id"`
	Counts            SourceArtifactCounts `json:"counts"`
	EntityDiagnostics EntityDiagnostics    `json:"entity_diagnostics"`
	Documents         []DocumentSummary    `json:"documents"`
	Sections          []SectionSummary     `json:"sections"`
	SectionEntities   []SectionEntity      `json:"section_entities,omitempty"`
	Nodes             []Node               `json:"nodes"`
	Edges             []EdgeSummary        `json:"edges"`
}

type SourceHealthWarning struct {
	Kind       string `json:"kind"`
	Severity   string `json:"severity"`
	Message    string `json:"message"`
	DocumentID string `json:"document_id,omitempty"`
	SectionID  string `json:"section_id,omitempty"`
	Count      int    `json:"count,omitempty"`
}

type SourceHealth struct {
	SourceID             string                `json:"source_id"`
	Counts               SourceArtifactCounts  `json:"counts"`
	EntityDiagnostics    EntityDiagnostics     `json:"entity_diagnostics"`
	LatestJob            Job                   `json:"latest_job"`
	BrokenLinks          []BrokenLink          `json:"broken_links"`
	ZeroSectionDocuments []DocumentSummary     `json:"zero_section_documents"`
	LowContentSections   []SectionSummary      `json:"low_content_sections"`
	StaleFeedback        []FeedbackEvent       `json:"stale_feedback"`
	Warnings             []SourceHealthWarning `json:"warnings"`
}

type QueryObservationInput struct {
	ID              string
	QueryText       string
	NormalizedQuery string
	Source          string
	ResultCount     int
	LatencyMS       int64
	CacheHit        bool
	Results         []SearchResultObservationInput
}

type SearchResultObservationInput struct {
	DocumentID string
	SectionID  string
	Rank       int
	Score      float64
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
	SectionID          string              `json:"section_id"`
	DocumentID         string              `json:"document_id"`
	DocumentTitle      string              `json:"document_title"`
	DocumentURL        string              `json:"document_url"`
	Title              string              `json:"title"`
	HeadingPath        string              `json:"heading_path"`
	Content            string              `json:"content"`
	ExplicitReferences []ExplicitReference `json:"explicit_references"`
}

type ExplicitReference struct {
	SourceSectionID   string                       `json:"source_section_id"`
	TargetSectionID   string                       `json:"target_section_id,omitempty"`
	TargetDocumentID  string                       `json:"target_document_id,omitempty"`
	TargetURL         string                       `json:"target_url,omitempty"`
	TargetHeadingPath string                       `json:"target_heading_path,omitempty"`
	RefText           string                       `json:"ref_text"`
	RefKind           string                       `json:"ref_kind"`
	Extractor         string                       `json:"extractor"`
	Resolved          bool                         `json:"resolved"`
	Confidence        float64                      `json:"confidence"`
	Candidates        []ExplicitReferenceCandidate `json:"candidates,omitempty"`
}

type ExplicitReferenceCandidate struct {
	TargetSectionID   string  `json:"target_section_id,omitempty"`
	TargetDocumentID  string  `json:"target_document_id,omitempty"`
	TargetURL         string  `json:"target_url,omitempty"`
	TargetHeadingPath string  `json:"target_heading_path,omitempty"`
	Confidence        float64 `json:"confidence,omitempty"`
}
