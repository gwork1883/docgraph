package storage

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/docgraph/docgraph/internal/domain"
	"github.com/docgraph/docgraph/internal/storage/sqlite"
	"github.com/docgraph/docgraph/internal/storage/sqlschema"
)

var ErrSyncInProgress = domain.ErrSyncInProgress

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
	ListSourceDocuments(ctx context.Context, sourceID string, limit, offset int) ([]DocumentSummary, error)
	ListSourceSections(ctx context.Context, sourceID string, limit, offset int) ([]SectionSummary, error)
	ListSourceNodes(ctx context.Context, sourceID string, limit, offset int) ([]Node, error)
	ListSourceEdges(ctx context.Context, sourceID string, limit, offset int) ([]EdgeSummary, error)
	GetDocumentBySourceExternalID(ctx context.Context, sourceID string, externalID string) (DocumentSummary, error)
	ReplaceDocument(ctx context.Context, doc DocumentInput, sections []SectionInput) error
	DeleteDocumentsNotInSource(ctx context.Context, sourceID string, keepDocumentIDs []string) error
	GetDocumentProfile(ctx context.Context, documentID string) (DocumentProfile, error)
	UpdateDocumentProfileDesc(ctx context.Context, input DocumentProfileInput) (DocumentProfile, error)
	UpsertDocumentRetrievalProfile(ctx context.Context, input RetrievalProfileInput) (DocumentProfile, error)
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
	ListJobs(ctx context.Context, opts JobListOptions) ([]Job, error)
	CountJobs(ctx context.Context, opts JobListOptions) (int, error)
	GetJob(ctx context.Context, id string) (Job, error)
	CreateSyncJob(ctx context.Context, sourceID string) (SyncJob, error)
	CreateSyncJobIfIdle(ctx context.Context, sourceID string) (SyncJob, error)
	CompleteSyncJob(ctx context.Context, id string, result ResultPayload) error
	FailSyncJob(ctx context.Context, id string, errText string) error
	ListSyncJobs(ctx context.Context, sourceID string, limit int) ([]SyncJob, error)
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
	RecordQueryObservation(ctx context.Context, input QueryObservationInput) error
	Close() error
}

type Status = sqlschema.Status

type Source = domain.Source
type ConfluenceCookieCredential = domain.ConfluenceCookieCredential
type SyncJob = domain.SyncJob
type ResultPayload = domain.ResultPayload
type BrokenLink = domain.BrokenLink
type DocumentInput = domain.DocumentInput
type SectionInput = domain.SectionInput
type DocumentProfile = domain.DocumentProfile
type DocumentProfileInput = domain.DocumentProfileInput
type RetrievalProfileInput = domain.RetrievalProfileInput
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
type SearchAttempt = domain.SearchAttempt
type Job = domain.Job
type JobInput = domain.JobInput
type JobListOptions = domain.JobListOptions
type SourceArtifacts = domain.SourceArtifacts
type SourceArtifactCounts = domain.SourceArtifactCounts
type DocumentSummary = domain.DocumentSummary
type SectionSummary = domain.SectionSummary
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
