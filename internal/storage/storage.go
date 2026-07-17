package storage

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/docgraph/docgraph/internal/domain"
	"github.com/docgraph/docgraph/internal/storage/sqlite"
	"github.com/docgraph/docgraph/internal/storage/sqlschema"
	"github.com/docgraph/docgraph/internal/vectorstore"
)

var ErrSyncInProgress = domain.ErrSyncInProgress
var ErrJobNotCancelable = domain.ErrJobNotCancelable

type Store interface {
	Migrate(ctx context.Context) error
	CheckSchema(ctx context.Context) error
	Status(ctx context.Context) (Status, error)
	CreateSource(ctx context.Context, source Source) (Source, error)
	UpdateSource(ctx context.Context, source Source) (Source, error)
	UpdateSourceSyncState(ctx context.Context, id string, status string, reason string) error
	DeleteSource(ctx context.Context, id string) error
	ListSources(ctx context.Context) ([]Source, error)
	GetSource(ctx context.Context, id string) (Source, error)
	CreateConfluenceCookieCredential(ctx context.Context, credential ConfluenceCookieCredential) (ConfluenceCookieCredential, error)
	UpdateConfluenceCookieCredential(ctx context.Context, credential ConfluenceCookieCredential) (ConfluenceCookieCredential, error)
	UpdateConfluenceCookieCredentialValidation(ctx context.Context, id string, status string, errText string) error
	DeleteConfluenceCookieCredential(ctx context.Context, id string) error
	ListConfluenceCookieCredentials(ctx context.Context) ([]ConfluenceCookieCredential, error)
	GetConfluenceCookieCredential(ctx context.Context, id string) (ConfluenceCookieCredential, error)
	ListSourceArtifacts(ctx context.Context, sourceID string, limit, offset int) (SourceArtifacts, error)
	GetSourceArtifactCounts(ctx context.Context, sourceID string) (SourceArtifactCounts, error)
	GetSourceHealth(ctx context.Context, sourceID string) (SourceHealth, error)
	ListSourceDocuments(ctx context.Context, sourceID string, limit, offset int) ([]DocumentSummary, error)
	ListSourceSections(ctx context.Context, sourceID string, limit, offset int) ([]SectionSummary, error)
	ListSourceSectionEntities(ctx context.Context, sourceID string, limit, offset int) ([]SectionEntity, error)
	ListSourceNodes(ctx context.Context, sourceID string, limit, offset int) ([]Node, error)
	ListSourceEdges(ctx context.Context, sourceID string, limit, offset int) ([]EdgeSummary, error)
	GetDocument(ctx context.Context, id string) (DocumentSummary, error)
	ListDocumentSections(ctx context.Context, documentID string, limit, offset int) ([]SectionSummary, error)
	GetDocumentBySourceExternalID(ctx context.Context, sourceID string, externalID string) (DocumentSummary, error)
	ReplaceDocument(ctx context.Context, doc DocumentInput, sections []SectionInput) error
	DeleteDocumentsNotInSource(ctx context.Context, sourceID string, keepDocumentIDs []string) error
	GetDocumentProfile(ctx context.Context, documentID string) (DocumentProfile, error)
	UpdateDocumentProfileDesc(ctx context.Context, input DocumentProfileInput) (DocumentProfile, error)
	UpsertDocumentRetrievalProfile(ctx context.Context, input RetrievalProfileInput) (DocumentProfile, error)
	ReplaceSectionEntities(ctx context.Context, documentID string, entities []SectionEntityInput) error
	ListDocumentEntities(ctx context.Context, documentID string) ([]SectionEntity, error)
	SearchEntities(ctx context.Context, query string, limit int) ([]SectionEntity, error)
	CreateKnowledgeRelationProposal(ctx context.Context, input KnowledgeRelationProposalInput) (KnowledgeRelationProposal, error)
	ListKnowledgeRelationProposals(ctx context.Context, opts KnowledgeRelationProposalListOptions) ([]KnowledgeRelationProposal, error)
	GetKnowledgeRelationProposal(ctx context.Context, id string) (KnowledgeRelationProposal, error)
	ApproveKnowledgeRelationProposal(ctx context.Context, id string, reviewedBy string, reviewNote string) (KnowledgeRelation, error)
	RejectKnowledgeRelationProposal(ctx context.Context, id string, reviewedBy string, reviewNote string) (KnowledgeRelationProposal, error)
	CancelKnowledgeRelationProposal(ctx context.Context, id string, reviewedBy string, reviewNote string) (KnowledgeRelationProposal, error)
	ListKnowledgeRelations(ctx context.Context, opts KnowledgeRelationListOptions) ([]KnowledgeRelation, error)
	DisableKnowledgeRelation(ctx context.Context, id string, reviewedBy string, note string) (KnowledgeRelation, error)
	ReenableKnowledgeRelation(ctx context.Context, id string, reviewedBy string, note string) (KnowledgeRelation, error)
	CreateJob(ctx context.Context, input JobInput) (Job, error)
	ClaimDueJob(ctx context.Context, workerID string, kinds []string, lease time.Duration) (Job, error)
	UpdateJobProgress(ctx context.Context, id string, progressJSON string) error
	CompleteJob(ctx context.Context, id string, resultJSON string) error
	FailJob(ctx context.Context, id string, errText string) error
	CancelJob(ctx context.Context, id string, reason string) (Job, error)
	MarkJobCanceled(ctx context.Context, id string, reason string) error
	ListJobs(ctx context.Context, opts JobListOptions) ([]Job, error)
	CountJobs(ctx context.Context, opts JobListOptions) (int, error)
	GetJob(ctx context.Context, id string) (Job, error)
	CreateEmbeddingEnsureJobIfIdle(ctx context.Context, sourceID string) (Job, error)
	CreateSyncJob(ctx context.Context, sourceID string) (SyncJob, error)
	CreateSyncJobIfIdle(ctx context.Context, sourceID string) (SyncJob, error)
	CompleteSyncJob(ctx context.Context, id string, result ResultPayload) error
	FailSyncJob(ctx context.Context, id string, errText string) error
	ListSyncJobs(ctx context.Context, sourceID string, limit int) ([]SyncJob, error)
	ListLatestSyncJobs(ctx context.Context) ([]SyncJob, error)
	DeleteSyncJob(ctx context.Context, sourceID string, jobID string) error
	UpsertNode(ctx context.Context, node NodeInput) error
	UpsertEdge(ctx context.Context, edge EdgeInput) error
	SearchNodes(ctx context.Context, query string, limit int) ([]Node, error)
	GetNode(ctx context.Context, id string) (Node, error)
	RelatedNodes(ctx context.Context, id string, opts RelatedOptions) ([]RelatedNode, error)
	Impact(ctx context.Context, id string, opts ImpactOptions) (ImpactResult, error)
	CreateFeedbackEvent(ctx context.Context, input FeedbackEventInput) (FeedbackEvent, error)
	DeleteFeedbackEvent(ctx context.Context, id string) error
	ListFeedbackEvents(ctx context.Context, opts FeedbackListOptions) ([]FeedbackEvent, error)
	SearchSections(ctx context.Context, query string, limit int) ([]SearchHit, error)
	SearchSectionsWithOptions(ctx context.Context, opts SearchOptions) (SearchResult, error)
	GetSection(ctx context.Context, id string) (SectionContent, error)
	UpsertSectionEmbedding(ctx context.Context, input SectionEmbeddingInput) error
	GetSectionEmbedding(ctx context.Context, sectionID string, model string) (VectorSearchHit, []float32, error)
	DeleteSectionEmbeddings(ctx context.Context, sectionID string) error
	SearchSectionsByVector(ctx context.Context, embedding []float32, model string, limit int, minSimilarity float64, plan vectorstore.EmbeddingPlanFilter) ([]VectorSearchHit, error)
	ListSectionEmbeddingHashes(ctx context.Context, model string, limit, offset int) ([]SectionEmbeddingHash, error)
	UpsertEmbeddingChunk(ctx context.Context, input EmbeddingChunkInput) error
	GetEmbeddingChunk(ctx context.Context, chunkID string, model string) (VectorSearchHit, []float32, error)
	DeleteEmbeddingChunksBySection(ctx context.Context, sectionID string, model string, generatorVersion string, tokenizer string, chunkStrategy string) error
	SearchChunksByVector(ctx context.Context, embedding []float32, model string, limit int, minSimilarity float64, plan vectorstore.EmbeddingPlanFilter) ([]VectorSearchHit, error)
	ListEmbeddingChunkHashes(ctx context.Context, model string, limit, offset int) ([]EmbeddingChunkHash, error)
	GetSourceEmbeddingStatus(ctx context.Context, sourceID string, model string, generatorVersion string, tokenizer string, chunkStrategy string, chunkTargetTokens int) (EmbeddingStatus, error)
	GetSectionForEmbedding(ctx context.Context, sectionID string) (EmbeddingSection, error)
	CountSectionsForEmbedding(ctx context.Context, sourceID string) (int, error)
	ListSectionsForEmbedding(ctx context.Context, sourceID string, limit, offset int) ([]EmbeddingSection, error)
	RecordQueryObservation(ctx context.Context, input QueryObservationInput) error
	Close() error
}

// WorkbookStore is implemented by stores that can atomically replace a
// connector-owned workbook aggregate. It intentionally remains separate from
// Store so lightweight test and connector fakes do not need to implement it.
type WorkbookStore interface {
	ReplaceWorkbookBundle(ctx context.Context, bundle WorkbookBundle) (WorkbookReplaceResult, error)
}

// MediaStore exposes source snapshots and logical media metadata. Blob bytes
// remain the responsibility of the central blob store.
type MediaStore interface {
	GetActiveSourceSnapshot(ctx context.Context, sourceID string) (SourceSnapshot, error)
	GetMediaAsset(ctx context.Context, assetID string) (MediaAsset, error)
	ListDocumentMediaAssets(ctx context.Context, documentID string, limit, offset int) ([]MediaAsset, error)
	ListSectionMediaAssets(ctx context.Context, sectionIDs []string) (map[string][]MediaAssetSummary, error)
	ListSourceFeatureInventory(ctx context.Context, sourceID string) ([]FeatureInventoryEntry, error)
	IsMediaBlobReferenced(ctx context.Context, sha256 string) (bool, error)
	ListUnreferencedMediaBlobs(ctx context.Context, olderThan string, limit int) ([]MediaBlob, error)
	DeleteMediaBlobIfUnreferenced(ctx context.Context, sha256 string, olderThan string) (bool, error)
}

// SectionContextStore provides ordered hierarchy access independently from the
// legacy bounded, flat document section listing.
type SectionContextStore interface {
	GetSectionContext(ctx context.Context, sectionID string, childLimit int) (SectionContext, error)
	ListDocumentOutline(ctx context.Context, documentID string, opts OutlineOptions) (DocumentOutline, error)
}

// AuthoredRelationStore batch-loads source-authored semantic relations for
// search hits without hydrating full Section context for every hit.
type AuthoredRelationStore interface {
	ListAuthoredRelationsForSections(ctx context.Context, sectionIDs []string, perSectionLimit int) (map[string][]RelatedNode, error)
}

type Status = sqlschema.Status

type Source = domain.Source
type ConfluenceCookieCredential = domain.ConfluenceCookieCredential
type SyncJob = domain.SyncJob
type ResultPayload = domain.ResultPayload
type ConnectorDiagnostics = domain.ConnectorDiagnostics
type BrokenLink = domain.BrokenLink
type DocumentInput = domain.DocumentInput
type SectionInput = domain.SectionInput
type SectionStructureInput = domain.SectionStructureInput
type SectionStructure = domain.SectionStructure
type WorkbookDocumentInput = domain.WorkbookDocumentInput
type WorkbookBundle = domain.WorkbookBundle
type WorkbookReplaceResult = domain.WorkbookReplaceResult
type MediaBlobInput = domain.MediaBlobInput
type MediaBlob = domain.MediaBlob
type SourceSnapshotInput = domain.SourceSnapshotInput
type SourceSnapshot = domain.SourceSnapshot
type MediaAssetInput = domain.MediaAssetInput
type MediaAsset = domain.MediaAsset
type MediaAssetSummary = domain.MediaAssetSummary
type SectionMediaRefInput = domain.SectionMediaRefInput
type SectionNodeInput = domain.SectionNodeInput
type FeatureInventoryInput = domain.FeatureInventoryInput
type FeatureInventoryEntry = domain.FeatureInventoryEntry
type DocumentProfile = domain.DocumentProfile
type DocumentProfileInput = domain.DocumentProfileInput
type RetrievalProfileInput = domain.RetrievalProfileInput
type SectionEntityInput = domain.SectionEntityInput
type SectionEntity = domain.SectionEntity
type MatchedEntity = domain.MatchedEntity
type EntityDiagnostics = domain.EntityDiagnostics
type KnowledgeRelationProposalInput = domain.KnowledgeRelationProposalInput
type KnowledgeRelationProposal = domain.KnowledgeRelationProposal
type KnowledgeRelationProposalListOptions = domain.KnowledgeRelationProposalListOptions
type KnowledgeRelationInput = domain.KnowledgeRelationInput
type KnowledgeRelation = domain.KnowledgeRelation
type KnowledgeRelationListOptions = domain.KnowledgeRelationListOptions
type RelationMatch = domain.RelationMatch
type SearchHit = domain.SearchHit
type SearchOptions = domain.SearchOptions
type SearchResult = domain.SearchResult
type SearchMediaSummary = domain.SearchMediaSummary
type SearchAttempt = domain.SearchAttempt
type SectionEmbeddingInput = domain.SectionEmbeddingInput
type EmbeddingChunkInput = domain.EmbeddingChunkInput
type VectorSearchHit = domain.VectorSearchHit
type SectionEmbeddingHash = domain.SectionEmbeddingHash
type EmbeddingChunkHash = domain.EmbeddingChunkHash
type EmbeddingStatus = domain.EmbeddingStatus
type EmbeddingSection = domain.EmbeddingSection
type Job = domain.Job
type JobInput = domain.JobInput
type JobListOptions = domain.JobListOptions
type SourceArtifacts = domain.SourceArtifacts
type SourceArtifactCounts = domain.SourceArtifactCounts
type SourceHealth = domain.SourceHealth
type SourceHealthWarning = domain.SourceHealthWarning
type DocumentSummary = domain.DocumentSummary
type DocumentDetail = domain.DocumentDetail
type SectionSummary = domain.SectionSummary
type SectionBreadcrumb = domain.SectionBreadcrumb
type OutlineOptions = domain.OutlineOptions
type DocumentOutline = domain.DocumentOutline
type SectionContext = domain.SectionContext
type EdgeSummary = domain.EdgeSummary
type QueryObservationInput = domain.QueryObservationInput
type SearchResultObservationInput = domain.SearchResultObservationInput
type NodeInput = domain.NodeInput
type Node = domain.Node
type EdgeInput = domain.EdgeInput
type Edge = domain.Edge
type RelatedOptions = domain.RelatedOptions
type RelatedNode = domain.RelatedNode
type ImpactOptions = domain.ImpactOptions
type ImpactPath = domain.ImpactPath
type ImpactResult = domain.ImpactResult
type FeedbackEventInput = domain.FeedbackEventInput
type FeedbackEvent = domain.FeedbackEvent
type FeedbackListOptions = domain.FeedbackListOptions
type SectionContent = domain.SectionContent

func Open(ctx context.Context, dsn string) (Store, error) {
	if strings.HasPrefix(dsn, "sqlite://") {
		return sqlite.Open(ctx, dsn)
	}
	scheme := dsn
	if i := strings.Index(scheme, "://"); i >= 0 {
		scheme = scheme[:i]
	}
	return nil, fmt.Errorf("unsupported storage scheme %q", scheme)
}

func OpenExisting(ctx context.Context, dsn string) (Store, error) {
	if strings.HasPrefix(dsn, "sqlite://") {
		return sqlite.OpenExisting(ctx, dsn)
	}
	scheme := dsn
	if i := strings.Index(scheme, "://"); i >= 0 {
		scheme = scheme[:i]
	}
	return nil, fmt.Errorf("unsupported storage scheme %q", scheme)
}
