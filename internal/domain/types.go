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
	Documents            int                   `json:"documents"`
	EntityDiagnostics    EntityDiagnostics     `json:"entity_diagnostics,omitempty"`
	BrokenLinks          []BrokenLink          `json:"broken_links,omitempty"`
	ConnectorDiagnostics *ConnectorDiagnostics `json:"connector_diagnostics,omitempty"`
}

type ConnectorDiagnostics struct {
	ParseStatus      string                  `json:"parse_status,omitempty"`
	FormatFamily     string                  `json:"format_family,omitempty"`
	FormatVersion    string                  `json:"format_version,omitempty"`
	SnapshotID       string                  `json:"snapshot_id,omitempty"`
	SnapshotHash     string                  `json:"snapshot_hash,omitempty"`
	Sheets           int                     `json:"sheets,omitempty"`
	Topics           int                     `json:"topics,omitempty"`
	AuthoredEdges    int                     `json:"authored_edges,omitempty"`
	MediaAssets      int                     `json:"media_assets,omitempty"`
	MediaBytes       int64                   `json:"media_bytes,omitempty"`
	FeatureInventory []FeatureInventoryEntry `json:"feature_inventory,omitempty"`
	Warnings         []string                `json:"warnings,omitempty"`
}

type BrokenLink struct {
	SourceDocument string `json:"source_document"`
	SourceSection  string `json:"source_section,omitempty"`
	Href           string `json:"href"`
	Text           string `json:"text,omitempty"`
	ResolvedTarget string `json:"resolved_target,omitempty"`
}

type DocumentInput struct {
	ID           string
	SourceID     string
	ExternalID   string
	Title        string
	URL          string
	Version      string
	ContentHash  string
	MetadataJSON string
}

type SectionInput struct {
	ID           string
	DocumentID   string
	HeadingPath  string
	Title        string
	Content      string
	SearchText   string
	ContentHash  string
	Ordinal      int
	MetadataJSON string
	Structure    *SectionStructureInput
}

type SectionStructureInput struct {
	ParentSectionID  string
	SourceElementID  string
	ElementKind      string
	Depth            int
	SiblingOrdinal   int
	OrderPath        []int
	DisplayNumber    string
	PresentationJSON string
}

type SectionStructure struct {
	SectionID        string `json:"section_id"`
	DocumentID       string `json:"document_id"`
	ParentSectionID  string `json:"parent_section_id,omitempty"`
	SourceElementID  string `json:"source_element_id"`
	ElementKind      string `json:"element_kind"`
	Depth            int    `json:"depth"`
	SiblingOrdinal   int    `json:"sibling_ordinal"`
	OrderPath        []int  `json:"order_path"`
	DisplayNumber    string `json:"display_number,omitempty"`
	PresentationJSON string `json:"presentation_json"`
}

type WorkbookDocumentInput struct {
	Document DocumentInput
	Sections []SectionInput
}

type MediaBlobInput struct {
	SHA256           string
	SizeBytes        int64
	SniffedMediaType string
	StorageKey       string
}

type MediaBlob struct {
	SHA256           string `json:"sha256"`
	SizeBytes        int64  `json:"size_bytes"`
	SniffedMediaType string `json:"sniffed_media_type"`
	StorageKey       string `json:"storage_key"`
	CreatedAt        string `json:"created_at"`
}

type SourceSnapshotInput struct {
	ID                string
	SourceID          string
	SourceHash        string
	BlobSHA256        string
	FormatFamily      string
	FormatVersion     string
	SemanticHash      string
	MediaManifestHash string
	MetadataJSON      string
}

type SourceSnapshot struct {
	ID                string `json:"id"`
	SourceID          string `json:"source_id"`
	SourceHash        string `json:"source_hash"`
	BlobSHA256        string `json:"blob_sha256"`
	FormatFamily      string `json:"format_family"`
	FormatVersion     string `json:"format_version"`
	SemanticHash      string `json:"semantic_hash"`
	MediaManifestHash string `json:"media_manifest_hash"`
	MetadataJSON      string `json:"metadata_json"`
	Active            bool   `json:"active"`
	CreatedAt         string `json:"created_at"`
}

type MediaAssetInput struct {
	ID           string
	SourceID     string
	SnapshotID   string
	DocumentID   string
	ExternalID   string
	BlobSHA256   string
	Kind         string
	OriginalName string
	MediaType    string
	SizeBytes    int64
	Status       string
	MetadataJSON string
}

type MediaAsset struct {
	ID           string `json:"id"`
	SourceID     string `json:"source_id"`
	SnapshotID   string `json:"snapshot_id"`
	DocumentID   string `json:"document_id"`
	ExternalID   string `json:"external_id"`
	BlobSHA256   string `json:"blob_sha256,omitempty"`
	Kind         string `json:"kind"`
	OriginalName string `json:"original_name"`
	MediaType    string `json:"media_type"`
	SizeBytes    int64  `json:"size_bytes"`
	Status       string `json:"status"`
	MetadataJSON string `json:"metadata_json"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

type MediaAssetSummary struct {
	ID                    string `json:"id"`
	Kind                  string `json:"kind"`
	OriginalName          string `json:"original_name"`
	MediaType             string `json:"media_type"`
	SizeBytes             int64  `json:"size_bytes"`
	Status                string `json:"status"`
	Role                  string `json:"role,omitempty"`
	Ordinal               int    `json:"ordinal,omitempty"`
	MetadataJSON          string `json:"metadata_json,omitempty"`
	ReferenceMetadataJSON string `json:"reference_metadata_json,omitempty"`
}

type SectionMediaRefInput struct {
	SectionID    string
	AssetID      string
	Role         string
	Ordinal      int
	MetadataJSON string
}

type SectionNodeInput struct {
	SectionID  string
	NodeID     string
	Role       string
	Confidence float64
}

type FeatureInventoryInput struct {
	ID             string
	SnapshotID     string
	FeatureKey     string
	CoverageStatus string
	ElementPath    string
	Count          int
	MetadataJSON   string
}

type FeatureInventoryEntry struct {
	ID             string `json:"id"`
	SnapshotID     string `json:"snapshot_id"`
	FeatureKey     string `json:"feature_key"`
	CoverageStatus string `json:"coverage_status"`
	ElementPath    string `json:"element_path,omitempty"`
	Count          int    `json:"count"`
	MetadataJSON   string `json:"metadata_json"`
}

type WorkbookBundle struct {
	SourceID         string
	WorkbookKey      string
	Snapshot         SourceSnapshotInput
	Documents        []WorkbookDocumentInput
	Nodes            []NodeInput
	Edges            []EdgeInput
	SectionNodes     []SectionNodeInput
	Blobs            []MediaBlobInput
	MediaAssets      []MediaAssetInput
	MediaRefs        []SectionMediaRefInput
	FeatureInventory []FeatureInventoryInput
}

type WorkbookReplaceResult struct {
	ActiveSnapshotID string   `json:"active_snapshot_id"`
	StaleSectionIDs  []string `json:"stale_section_ids,omitempty"`
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
	SectionID              string              `json:"section_id"`
	NodeID                 string              `json:"node_id,omitempty"`
	DocumentID             string              `json:"document_id"`
	SourceID               string              `json:"source_id,omitempty"`
	DocumentTitle          string              `json:"document_title"`
	DocumentURL            string              `json:"document_url"`
	ContentHash            string              `json:"content_hash,omitempty"`
	Desc                   string              `json:"desc,omitempty"`
	Canonical              bool                `json:"canonical"`
	Title                  string              `json:"title"`
	ElementKind            string              `json:"element_kind,omitempty"`
	DisplayNumber          string              `json:"display_number,omitempty"`
	HeadingPath            string              `json:"heading_path"`
	Ancestry               []SectionBreadcrumb `json:"ancestry,omitempty"`
	MediaAssets            []MediaAssetSummary `json:"media_assets,omitempty"`
	MediaAssetsTotal       int                 `json:"media_assets_total"`
	MediaAssetsTruncated   bool                `json:"media_assets_truncated"`
	AuthoredRelationCount  int                 `json:"authored_relation_count,omitempty"`
	EvidenceKind           string              `json:"evidence_kind,omitempty"`
	Content                string              `json:"content,omitempty"`
	Snippet                string              `json:"snippet"`
	Rank                   float64             `json:"rank"`
	HasExplicitReferences  bool                `json:"has_explicit_references"`
	ExplicitReferenceCount int                 `json:"explicit_reference_count"`
	Profile                *SearchHitProfile   `json:"profile,omitempty"`
	RetrievalProfile       any                 `json:"retrieval_profile,omitempty"`
	QueryMatch             *QueryMatch         `json:"query_match,omitempty"`
	ScoreBreakdown         *ScoreBreakdown     `json:"score_breakdown,omitempty"`
	MatchedEntities        []MatchedEntity     `json:"matched_entities,omitempty"`
	RelationMatches        []RelationMatch     `json:"relation_matches,omitempty"`
	EvidenceLevel          string              `json:"evidence_level,omitempty"`
	Trace                  *SearchHitTrace     `json:"trace,omitempty"`
	RRFContribution        *RRFContribution    `json:"rrf_contribution,omitempty"`
}

type SearchHitTrace struct {
	SourceID             string `json:"source_id,omitempty"`
	DocumentID           string `json:"document_id,omitempty"`
	SectionID            string `json:"section_id,omitempty"`
	ChunkID              string `json:"chunk_id,omitempty"`
	ChunkOrdinal         int    `json:"chunk_ordinal,omitempty"`
	ContentHash          string `json:"content_hash,omitempty"`
	EmbeddingModel       string `json:"embedding_model,omitempty"`
	EmbeddingTextHash    string `json:"embedding_text_hash,omitempty"`
	ChunkTextHash        string `json:"chunk_text_hash,omitempty"`
	EmbeddingGeneratedAt string `json:"embedding_generated_at,omitempty"`
	GeneratorVersion     string `json:"generator_version,omitempty"`
	Tokenizer            string `json:"tokenizer,omitempty"`
	ChunkStrategy        string `json:"chunk_strategy,omitempty"`
	VectorTraceValid     bool   `json:"vector_trace_valid,omitempty"`
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
	UnicodeBM25Boost      float64  `json:"unicode_bm25_boost,omitempty"`
	TrigramBM25Boost      float64  `json:"trigram_bm25_boost,omitempty"`
	TitleBoost            float64  `json:"title_boost,omitempty"`
	SectionBoost          float64  `json:"section_boost,omitempty"`
	SymbolBoost           float64  `json:"symbol_boost,omitempty"`
	ExactMatchBoost       float64  `json:"exact_match_boost,omitempty"`
	CanonicalBoost        float64  `json:"canonical_boost,omitempty"`
	CoverageBoost         float64  `json:"coverage_boost,omitempty"`
	FallbackBoost         float64  `json:"fallback_boost,omitempty"`
	VectorBoost           float64  `json:"vector_boost,omitempty"`
	VectorOnlyPenalty     float64  `json:"vector_only_penalty,omitempty"`
	StaleEmbeddingPenalty float64  `json:"stale_embedding_penalty,omitempty"`
	Total                 float64  `json:"total"`
	MatchedFields         []string `json:"matched_fields,omitempty"`
	MatchedTerms          []string `json:"matched_terms,omitempty"`
	MatchedSymbols        []string `json:"matched_symbols,omitempty"`
}

type SearchOptions struct {
	Query                     string
	Limit                     int
	MaxSearches               int
	MaxSectionsPerDocument    int
	ProfileDetail             string
	MaxCharsPerResult         int
	MaxMediaAssetsPerResult   int
	MaxMediaAssetsTotal       int
	MediaDetail               string // "full" (legacy), "compact", or "none"
	Detail                    string // "summary" (default) or "content"
	UseRelationExpansion      bool
	RelationDepth             int
	RelationTypes             []string
	OriginalQuery             string
	ExactTerms                []string
	SemanticIntents           []string
	EmbeddingGeneratorVersion string
}

type SectionEmbeddingInput struct {
	SectionID         string
	DocumentID        string
	SourceID          string
	Model             string
	Dimensions        int
	Embedding         []float32
	ContentHash       string
	EmbeddingTextHash string
	GeneratorVersion  string
}

type EmbeddingChunkInput struct {
	ChunkID            string
	SectionID          string
	DocumentID         string
	SourceID           string
	ChunkOrdinal       int
	ChunkStartToken    int
	ChunkTokenCount    int
	ChunkText          string
	Model              string
	Dimensions         int
	Embedding          []float32
	SectionContentHash string
	ChunkTextHash      string
	Tokenizer          string
	ChunkStrategy      string
	GeneratorVersion   string
}

type VectorSearchHit struct {
	ChunkID            string  `json:"chunk_id,omitempty"`
	SectionID          string  `json:"section_id"`
	DocumentID         string  `json:"document_id"`
	SourceID           string  `json:"source_id"`
	ChunkOrdinal       int     `json:"chunk_ordinal,omitempty"`
	ChunkText          string  `json:"chunk_text,omitempty"`
	Similarity         float64 `json:"similarity"`
	Model              string  `json:"model"`
	ContentHash        string  `json:"content_hash,omitempty"`
	SectionContentHash string  `json:"section_content_hash,omitempty"`
	EmbeddingTextHash  string  `json:"embedding_text_hash,omitempty"`
	ChunkTextHash      string  `json:"chunk_text_hash,omitempty"`
	Tokenizer          string  `json:"tokenizer,omitempty"`
	ChunkStrategy      string  `json:"chunk_strategy,omitempty"`
	GeneratorVersion   string  `json:"generator_version"`
	GeneratedAt        string  `json:"generated_at"`
}

type SectionEmbeddingHash struct {
	SectionID         string `json:"section_id"`
	DocumentID        string `json:"document_id,omitempty"`
	SourceID          string `json:"source_id,omitempty"`
	Model             string `json:"model,omitempty"`
	ContentHash       string `json:"content_hash"`
	EmbeddingTextHash string `json:"embedding_text_hash"`
	GeneratorVersion  string `json:"generator_version"`
	GeneratedAt       string `json:"generated_at,omitempty"`
}

type EmbeddingCoverage struct {
	EmbeddedSections int `json:"embedded_sections"`
	EmbeddedChunks   int `json:"embedded_chunks"`
}

type EmbeddingChunkHash struct {
	ChunkID            string `json:"chunk_id"`
	SectionID          string `json:"section_id"`
	DocumentID         string `json:"document_id,omitempty"`
	SourceID           string `json:"source_id,omitempty"`
	ChunkOrdinal       int    `json:"chunk_ordinal"`
	Model              string `json:"model,omitempty"`
	SectionContentHash string `json:"section_content_hash"`
	ChunkTextHash      string `json:"chunk_text_hash"`
	Tokenizer          string `json:"tokenizer,omitempty"`
	ChunkStrategy      string `json:"chunk_strategy,omitempty"`
	GeneratorVersion   string `json:"generator_version"`
	GeneratedAt        string `json:"generated_at,omitempty"`
}

type EmbeddingStatus struct {
	SourceID         string `json:"source_id,omitempty"`
	Enabled          bool   `json:"enabled"`
	Status           string `json:"status,omitempty"`
	Reason           string `json:"reason,omitempty"`
	Backend          string `json:"backend,omitempty"`
	TotalSections    int    `json:"total_sections"`
	EmbeddedSections int    `json:"embedded_sections"`
	PendingSections  int    `json:"pending_sections"`
	StaleSections    int    `json:"stale_sections"`
	TotalChunks      int    `json:"total_chunks"`
	EmbeddedChunks   int    `json:"embedded_chunks"`
	PendingChunks    int    `json:"pending_chunks"`
	StaleChunks      int    `json:"stale_chunks"`
	Model            string `json:"model"`
	Tokenizer        string `json:"tokenizer,omitempty"`
	ChunkStrategy    string `json:"chunk_strategy,omitempty"`
	GeneratorVersion string `json:"generator_version"`
}

type EmbeddingSection struct {
	SectionID     string
	DocumentID    string
	SourceID      string
	SourceName    string
	ProductHint   string
	ModuleHint    string
	DocumentTitle string
	HeadingPath   string
	Title         string
	Content       string
	SearchText    string
	ContentHash   string
}

type SearchAttempt struct {
	Kind  string   `json:"kind"`
	Query string   `json:"query,omitempty"`
	Terms []string `json:"terms,omitempty"`
	Hits  int      `json:"hits"`
	Error string   `json:"error,omitempty"`
}

type SearchResult struct {
	Query            string              `json:"query"`
	SearchesUsed     int                 `json:"searches_used"`
	Attempts         []SearchAttempt     `json:"attempts"`
	Hits             []SearchHit         `json:"hits"`
	MediaSummary     *SearchMediaSummary `json:"media_summary,omitempty"`
	SuggestedReads   SuggestedReads      `json:"suggested_reads"`
	HybridSearchMeta *HybridSearchMeta   `json:"hybrid_search_meta,omitempty"`
}

type SearchMediaSummary struct {
	Total          int    `json:"total"`
	Returned       int    `json:"returned"`
	Truncated      bool   `json:"truncated"`
	PerResultLimit int    `json:"per_result_limit,omitempty"`
	TotalLimit     int    `json:"total_limit,omitempty"`
	Detail         string `json:"detail"`
}

type HybridSearchMeta struct {
	IntentRoute         string  `json:"intent_route"`
	WText               float64 `json:"w_text"`
	WVector             float64 `json:"w_vector"`
	RRFK                float64 `json:"rrf_k"`
	TextCandidates      int     `json:"text_candidates"`
	VectorCandidates    int     `json:"vector_candidates"`
	VectorMinSimilarity float64 `json:"vector_min_similarity"`
}

type RRFContribution struct {
	TextRank       int      `json:"text_rank,omitempty"`
	VectorRank     int      `json:"vector_rank,omitempty"`
	TextRRF        float64  `json:"text_rrf,omitempty"`
	VectorRRF      float64  `json:"vector_rrf,omitempty"`
	RRFScore       float64  `json:"rrf_score"`
	FinalScore     float64  `json:"final_score"`
	Multiplier     float64  `json:"multiplier"`
	RawSimilarity  float64  `json:"raw_similarity,omitempty"`
	RawBM25Score   float64  `json:"raw_bm25_score,omitempty"`
	SourceEvidence []string `json:"source_evidence,omitempty"`
	DocumentID     string   `json:"document_id,omitempty"` // needed for per-doc section cap
}

type SuggestedReads struct {
	ExplicitReferences  []ExplicitReference `json:"explicit_references"`
	ImplicitSymbolLinks []SuggestedRead     `json:"implicit_symbol_links"`
	CuratedRelations    []SuggestedRead     `json:"curated_relations"`
	StructuralNeighbors []SuggestedRead     `json:"structural_neighbors"`
	AuthoredRelations   []SuggestedRead     `json:"authored_relations,omitempty"`
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
	ID               string `json:"id"`
	DocumentID       string `json:"document_id"`
	DocumentTitle    string `json:"document_title"`
	Title            string `json:"title"`
	HeadingPath      string `json:"heading_path"`
	ContentSnippet   string `json:"content_snippet"`
	ContentHash      string `json:"content_hash,omitempty"`
	Ordinal          int    `json:"ordinal"`
	NodeID           string `json:"node_id"`
	ParentSectionID  string `json:"parent_section_id,omitempty"`
	SourceElementID  string `json:"source_element_id,omitempty"`
	ElementKind      string `json:"element_kind,omitempty"`
	Depth            int    `json:"depth,omitempty"`
	SiblingOrdinal   int    `json:"sibling_ordinal,omitempty"`
	OrderPath        []int  `json:"order_path,omitempty"`
	DisplayNumber    string `json:"display_number,omitempty"`
	PresentationJSON string `json:"presentation_json,omitempty"`
	MetadataJSON     string `json:"metadata_json,omitempty"`
	MediaCount       int    `json:"media_count,omitempty"`
}

type SectionBreadcrumb struct {
	SectionID     string `json:"section_id"`
	Title         string `json:"title"`
	DisplayNumber string `json:"display_number,omitempty"`
	Depth         int    `json:"depth"`
}

type OutlineOptions struct {
	ParentSectionID string
	Limit           int
	Offset          int
}

type DocumentOutline struct {
	DocumentID      string           `json:"document_id"`
	ParentSectionID string           `json:"parent_section_id,omitempty"`
	Sections        []SectionSummary `json:"sections"`
	Limit           int              `json:"limit"`
	Offset          int              `json:"offset"`
	HasMore         bool             `json:"has_more"`
}

type SectionContext struct {
	Section           SectionContent      `json:"section"`
	Structure         *SectionStructure   `json:"structure,omitempty"`
	Ancestors         []SectionSummary    `json:"ancestors"`
	Children          []SectionSummary    `json:"children"`
	ChildrenPage      DocumentOutline     `json:"children_page"`
	AuthoredRelations []RelatedNode       `json:"authored_relations"`
	MediaAssets       []MediaAssetSummary `json:"media_assets"`
	Snapshot          *SourceSnapshot     `json:"snapshot,omitempty"`
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
	Documents              int64 `json:"documents"`
	Sections               int64 `json:"sections"`
	Nodes                  int64 `json:"nodes"`
	Edges                  int64 `json:"edges"`
	SectionEntities        int64 `json:"section_entities"`
	MediaAssets            int64 `json:"media_assets"`
	Snapshots              int64 `json:"snapshots"`
	MissingMediaAssets     int64 `json:"missing_media_assets"`
	RejectedMediaAssets    int64 `json:"rejected_media_assets"`
	UnavailableMediaAssets int64 `json:"unavailable_media_assets"`
	SnapshotBytes          int64 `json:"snapshot_bytes"`
	ReferencedMediaBytes   int64 `json:"referenced_media_bytes"`
}

type SourceArtifacts struct {
	SourceID          string                  `json:"source_id"`
	Counts            SourceArtifactCounts    `json:"counts"`
	EmbeddingStatus   *EmbeddingStatus        `json:"embedding_status,omitempty"`
	EntityDiagnostics EntityDiagnostics       `json:"entity_diagnostics"`
	Documents         []DocumentSummary       `json:"documents"`
	Sections          []SectionSummary        `json:"sections"`
	SectionEntities   []SectionEntity         `json:"section_entities,omitempty"`
	Nodes             []Node                  `json:"nodes"`
	Edges             []EdgeSummary           `json:"edges"`
	ActiveSnapshot    *SourceSnapshot         `json:"active_snapshot,omitempty"`
	FeatureInventory  []FeatureInventoryEntry `json:"feature_inventory,omitempty"`
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
	SourceID             string                  `json:"source_id"`
	Counts               SourceArtifactCounts    `json:"counts"`
	EntityDiagnostics    EntityDiagnostics       `json:"entity_diagnostics"`
	LatestJob            Job                     `json:"latest_job"`
	BrokenLinks          []BrokenLink            `json:"broken_links"`
	ZeroSectionDocuments []DocumentSummary       `json:"zero_section_documents"`
	LowContentSections   []SectionSummary        `json:"low_content_sections"`
	StaleFeedback        []FeedbackEvent         `json:"stale_feedback"`
	Warnings             []SourceHealthWarning   `json:"warnings"`
	ActiveSnapshot       *SourceSnapshot         `json:"active_snapshot,omitempty"`
	FeatureInventory     []FeatureInventoryEntry `json:"feature_inventory,omitempty"`
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
	OwnerSourceID string  `json:"owner_source_id,omitempty"`
	OwnerScope    string  `json:"owner_scope,omitempty"`
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
	OwnerSourceID     string  `json:"owner_source_id,omitempty"`
	OwnerScope        string  `json:"owner_scope,omitempty"`
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
	NodeID             string              `json:"node_id,omitempty"`
	DocumentID         string              `json:"document_id"`
	DocumentTitle      string              `json:"document_title"`
	DocumentURL        string              `json:"document_url"`
	Title              string              `json:"title"`
	HeadingPath        string              `json:"heading_path"`
	Content            string              `json:"content"`
	SearchText         string              `json:"search_text,omitempty"`
	MetadataJSON       string              `json:"metadata_json,omitempty"`
	Structure          *SectionStructure   `json:"structure,omitempty"`
	MediaAssets        []MediaAssetSummary `json:"media_assets,omitempty"`
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
