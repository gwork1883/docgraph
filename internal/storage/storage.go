package storage

import (
	"context"
	"fmt"
	"strings"

	"github.com/docgraph/docgraph/internal/domain"
	"github.com/docgraph/docgraph/internal/storage/sqlite"
	"github.com/docgraph/docgraph/internal/storage/sqlschema"
)

var ErrSyncInProgress = domain.ErrSyncInProgress

type Store interface {
	Migrate(ctx context.Context) error
	Status(ctx context.Context) (Status, error)
	CreateSource(ctx context.Context, source Source) (Source, error)
	UpdateSource(ctx context.Context, source Source) (Source, error)
	DeleteSource(ctx context.Context, id string) error
	ListSources(ctx context.Context) ([]Source, error)
	GetSource(ctx context.Context, id string) (Source, error)
	ListSourceArtifacts(ctx context.Context, sourceID string, limit, offset int) (SourceArtifacts, error)
	ReplaceDocument(ctx context.Context, doc DocumentInput, sections []SectionInput) error
	DeleteDocumentsNotInSource(ctx context.Context, sourceID string, keepDocumentIDs []string) error
	GetDocumentProfile(ctx context.Context, documentID string) (DocumentProfile, error)
	UpdateDocumentProfileDesc(ctx context.Context, input DocumentProfileInput) (DocumentProfile, error)
	UpsertDocumentRetrievalProfile(ctx context.Context, input RetrievalProfileInput) (DocumentProfile, error)
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
	Close() error
}

type Status = sqlschema.Status

type Source = domain.Source
type SyncJob = domain.SyncJob
type ResultPayload = domain.ResultPayload
type BrokenLink = domain.BrokenLink
type DocumentInput = domain.DocumentInput
type SectionInput = domain.SectionInput
type DocumentProfile = domain.DocumentProfile
type DocumentProfileInput = domain.DocumentProfileInput
type RetrievalProfileInput = domain.RetrievalProfileInput
type SearchHit = domain.SearchHit
type SearchOptions = domain.SearchOptions
type SearchResult = domain.SearchResult
type SearchAttempt = domain.SearchAttempt
type SourceArtifacts = domain.SourceArtifacts
type DocumentSummary = domain.DocumentSummary
type SectionSummary = domain.SectionSummary
type EdgeSummary = domain.EdgeSummary
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
