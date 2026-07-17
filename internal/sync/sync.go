package sync

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/docgraph/docgraph/internal/ids"
	"github.com/docgraph/docgraph/internal/ingest/confluence"
	"github.com/docgraph/docgraph/internal/ingest/gitdocs"
	"github.com/docgraph/docgraph/internal/ingest/htmldocs"
	"github.com/docgraph/docgraph/internal/ingest/localdocs"
	"github.com/docgraph/docgraph/internal/ingest/openapi"
	"github.com/docgraph/docgraph/internal/ingest/sftpdocs"
	"github.com/docgraph/docgraph/internal/ingest/staticdocs"
	"github.com/docgraph/docgraph/internal/ingest/webdocs"
	"github.com/docgraph/docgraph/internal/profile"
	"github.com/docgraph/docgraph/internal/storage"
	"github.com/docgraph/docgraph/internal/technical/extract"
)

type Service struct {
	store storage.Store
	xmind xmindRuntime
}

func NewService(store storage.Store) *Service {
	return NewServiceWithOptions(store, ServiceOptions{})
}

type Result struct {
	SourceID             string                        `json:"source_id"`
	Documents            int                           `json:"documents"`
	JobID                string                        `json:"job_id,omitempty"`
	EntityDiagnostics    storage.EntityDiagnostics     `json:"entity_diagnostics,omitempty"`
	BrokenLinks          []storage.BrokenLink          `json:"broken_links,omitempty"`
	ConnectorDiagnostics *storage.ConnectorDiagnostics `json:"connector_diagnostics,omitempty"`
}

type SectionEntityBackfillOptions struct {
	SourceID   string
	DocumentID string
}

type SectionEntityBackfillResult struct {
	Sources          int `json:"sources"`
	Documents        int `json:"documents"`
	Sections         int `json:"sections"`
	Entities         int `json:"entities"`
	APIEndpoints     int `json:"api_endpoints"`
	PathLiterals     int `json:"path_literals"`
	Operations       int `json:"operations"`
	OpenAPIDocuments int `json:"openapi_documents"`
}

type indexedDocument struct {
	doc      storage.DocumentInput
	sections []storage.SectionInput
	graph    bool
}

type documentSyncPlan struct {
	replace bool
	profile bool
	graph   bool
}

func (s *Service) SyncSource(ctx context.Context, id string) (Result, error) {
	source, err := s.store.GetSource(ctx, id)
	if err != nil {
		return Result{}, fmt.Errorf("source %q not found", id)
	}
	job, err := s.store.CreateSyncJobIfIdle(ctx, source.ID)
	if err != nil {
		return Result{}, err
	}
	result, err := s.syncSource(ctx, source, job.ID)
	if err != nil {
		_ = s.store.FailSyncJob(ctx, job.ID, err.Error())
		if isCredentialFailure(source.Kind, err) {
			_ = s.store.UpdateSourceSyncState(ctx, source.ID, "paused", "credential_required")
		}
		return Result{}, err
	}
	result.JobID = job.ID
	if err := s.attachEntityDiagnostics(ctx, source.ID, &result); err != nil {
		return Result{}, err
	}
	if err := s.store.CompleteSyncJob(ctx, job.ID, storage.ResultPayload{Documents: result.Documents, EntityDiagnostics: result.EntityDiagnostics, BrokenLinks: result.BrokenLinks, ConnectorDiagnostics: result.ConnectorDiagnostics}); err != nil {
		return Result{}, err
	}
	if source.SyncStatus == "paused" {
		_ = s.store.UpdateSourceSyncState(ctx, source.ID, "active", "")
	}
	return result, nil
}

func (s *Service) RunSyncJob(ctx context.Context, job storage.Job) (Result, error) {
	if strings.TrimSpace(job.ID) == "" {
		return Result{}, fmt.Errorf("job id is required")
	}
	sourceID := strings.TrimSpace(job.SourceID)
	if sourceID == "" {
		sourceID = sourceIDFromSyncJobPayload(job.PayloadJSON)
	}
	if sourceID == "" {
		err := fmt.Errorf("sync_source job %q is missing source_id", job.ID)
		_ = s.store.FailSyncJob(ctx, job.ID, err.Error())
		return Result{}, err
	}
	source, err := s.store.GetSource(ctx, sourceID)
	if err != nil {
		err = fmt.Errorf("source %q not found", sourceID)
		_ = s.store.FailSyncJob(ctx, job.ID, err.Error())
		return Result{}, err
	}
	result, err := s.syncSource(ctx, source, job.ID)
	if err != nil {
		_ = s.store.FailSyncJob(ctx, job.ID, err.Error())
		if isCredentialFailure(source.Kind, err) {
			_ = s.store.UpdateSourceSyncState(ctx, source.ID, "paused", "credential_required")
		}
		return Result{}, err
	}
	result.JobID = job.ID
	if err := s.attachEntityDiagnostics(ctx, source.ID, &result); err != nil {
		return Result{}, err
	}
	if err := s.store.CompleteSyncJob(ctx, job.ID, storage.ResultPayload{Documents: result.Documents, EntityDiagnostics: result.EntityDiagnostics, BrokenLinks: result.BrokenLinks, ConnectorDiagnostics: result.ConnectorDiagnostics}); err != nil {
		return Result{}, err
	}
	if source.SyncStatus == "paused" {
		_ = s.store.UpdateSourceSyncState(ctx, source.ID, "active", "")
	}
	return result, nil
}

func (s *Service) attachEntityDiagnostics(ctx context.Context, sourceID string, result *Result) error {
	health, err := s.store.GetSourceHealth(ctx, sourceID)
	if err != nil {
		return err
	}
	result.EntityDiagnostics = health.EntityDiagnostics
	return nil
}

func (s *Service) syncSource(ctx context.Context, source storage.Source, jobID string) (Result, error) {
	switch source.Kind {
	case "local":
		return s.syncLocal(ctx, source)
	case "git":
		return s.syncGit(ctx, source)
	case "html":
		return s.syncHTML(ctx, source)
	case "webdocs":
		return s.syncWebDocs(ctx, source, jobID)
	case "static":
		return s.syncStatic(ctx, source)
	case "sftp":
		return s.syncSFTP(ctx, source)
	case "confluence":
		return s.syncConfluence(ctx, source)
	case "openapi":
		return s.syncOpenAPI(ctx, source)
	case "xmind":
		return s.syncXMind(ctx, source, jobID)
	default:
		return Result{}, fmt.Errorf("sync is not supported for source kind %q", source.Kind)
	}
}

func (s *Service) BackfillSectionEntities(ctx context.Context, opts SectionEntityBackfillOptions) (SectionEntityBackfillResult, error) {
	sourceID := strings.TrimSpace(opts.SourceID)
	documentID := strings.TrimSpace(opts.DocumentID)
	if sourceID != "" && documentID != "" {
		return SectionEntityBackfillResult{}, fmt.Errorf("source_id and document_id cannot both be set")
	}
	result := SectionEntityBackfillResult{}
	if documentID != "" {
		doc, err := s.store.GetDocument(ctx, documentID)
		if err != nil {
			return result, err
		}
		source, err := s.store.GetSource(ctx, doc.SourceID)
		if err != nil {
			return result, err
		}
		result.Sources = 1
		if err := s.backfillDocumentSectionEntities(ctx, source, doc, &result); err != nil {
			return result, err
		}
		return result, nil
	}

	sources := make([]storage.Source, 0)
	if sourceID != "" {
		source, err := s.store.GetSource(ctx, sourceID)
		if err != nil {
			return result, err
		}
		sources = append(sources, source)
	} else {
		listed, err := s.store.ListSources(ctx)
		if err != nil {
			return result, err
		}
		sources = listed
	}
	result.Sources = len(sources)
	for _, source := range sources {
		for offset := 0; ; offset += 200 {
			docs, err := s.store.ListSourceDocuments(ctx, source.ID, 200, offset)
			if err != nil {
				return result, err
			}
			if len(docs) == 0 {
				break
			}
			for _, doc := range docs {
				if err := s.backfillDocumentSectionEntities(ctx, source, doc, &result); err != nil {
					return result, err
				}
			}
			if len(docs) < 200 {
				break
			}
		}
	}
	return result, nil
}

func (s *Service) backfillDocumentSectionEntities(ctx context.Context, source storage.Source, doc storage.DocumentSummary, result *SectionEntityBackfillResult) error {
	sections := make([]storage.SectionInput, 0)
	for offset := 0; ; offset += 200 {
		sectionSummaries, err := s.store.ListDocumentSections(ctx, doc.ID, 200, offset)
		if err != nil {
			return err
		}
		if len(sectionSummaries) == 0 {
			break
		}
		for _, summary := range sectionSummaries {
			content, err := s.store.GetSection(ctx, summary.ID)
			if err != nil {
				return err
			}
			sections = append(sections, storage.SectionInput{
				ID:          summary.ID,
				DocumentID:  doc.ID,
				HeadingPath: summary.HeadingPath,
				Title:       summary.Title,
				Content:     content.Content,
				Ordinal:     summary.Ordinal,
			})
		}
		if len(sectionSummaries) < 200 {
			break
		}
	}
	docInput := storage.DocumentInput{
		ID:          doc.ID,
		SourceID:    doc.SourceID,
		ExternalID:  doc.ExternalID,
		Title:       doc.Title,
		URL:         doc.URL,
		ContentHash: doc.ContentHash,
	}
	openAPISource := source.Kind == "openapi"
	entities := sectionEntityInputsForDocument(docInput, sections, openAPISource)
	if err := s.store.ReplaceSectionEntities(ctx, doc.ID, entities); err != nil {
		return err
	}
	result.Documents++
	result.Sections += len(sections)
	result.Entities += len(entities)
	if openAPISource {
		result.OpenAPIDocuments++
	}
	for _, entity := range entities {
		switch entity.Kind {
		case string(extract.EntityAPIEndpoint):
			result.APIEndpoints++
		case string(extract.EntityPathLiteral):
			result.PathLiterals++
		case string(extract.EntityOperationCandidate):
			result.Operations++
		}
	}
	return nil
}

func sourceIDFromSyncJobPayload(payload string) string {
	var value struct {
		SourceID string `json:"source_id"`
	}
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return ""
	}
	return strings.TrimSpace(value.SourceID)
}

func isCredentialFailure(kind string, err error) bool {
	switch kind {
	case "confluence", "webdocs", "sftp":
	default:
		return false
	}
	text := strings.ToLower(err.Error())
	for _, marker := range []string{
		"auth",
		"unauthorized",
		"forbidden",
		"http 401",
		"http 403",
		"credential",
		"sso",
		"oidc",
		"zero trust",
		"login page",
		"non-json response",
		"redirected to",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func (s *Service) syncLocal(ctx context.Context, source storage.Source) (Result, error) {
	root, err := LocalPathFromDSN(source.DSN)
	if err != nil {
		return Result{}, err
	}
	return s.syncMarkdownDocs(ctx, source, root, root)
}

func (s *Service) syncSFTP(ctx context.Context, source storage.Source) (Result, error) {
	checkout, err := sftpdocs.Sync(ctx, source.DSN, source.ID, source.ConfigJSON)
	if err != nil {
		return Result{}, err
	}
	docs, err := staticdocs.Scan(checkout.Root, source.ConfigJSON)
	if err != nil {
		return Result{}, err
	}
	return s.syncHTMLLikeDocs(ctx, source, checkout.URL, docs)
}

func (s *Service) syncGit(ctx context.Context, source storage.Source) (Result, error) {
	checkout, err := gitdocs.Sync(ctx, source.DSN, source.ConfigJSON)
	if err != nil {
		return Result{}, err
	}
	return s.syncMarkdownDocs(ctx, source, checkout.Root, checkout.URL)
}

type indexedHTMLDocument struct {
	scanned  htmldocs.Document
	doc      storage.DocumentInput
	sections []storage.SectionInput
	graph    bool
}

type htmlSectionRef struct {
	section storage.SectionInput
	docPath string
}

func (s *Service) syncStatic(ctx context.Context, source storage.Source) (Result, error) {
	root, err := LocalPathFromDSN(source.DSN)
	if err != nil {
		return Result{}, err
	}
	docs, err := staticdocs.Scan(root, source.ConfigJSON)
	if err != nil {
		return Result{}, err
	}
	return s.syncHTMLLikeDocs(ctx, source, docURLPrefix(source.ConfigJSON, root), docs)
}

func (s *Service) syncHTML(ctx context.Context, source storage.Source) (Result, error) {
	root, err := LocalPathFromDSN(source.DSN)
	if err != nil {
		return Result{}, err
	}
	docs, err := htmldocs.Scan(root)
	if err != nil {
		return Result{}, err
	}
	return s.syncHTMLLikeDocs(ctx, source, docURLPrefix(source.ConfigJSON, root), docs)
}

func (s *Service) syncWebDocs(ctx context.Context, source storage.Source, jobID string) (Result, error) {
	docs, err := webdocs.LoadWithProgress(ctx, source.DSN, source.ConfigJSON, func(progress webdocs.Progress) {
		_ = s.updateJobProgress(ctx, jobID, map[string]any{
			"phase":   progress.Phase,
			"url":     progress.URL,
			"visited": progress.Visited,
			"queued":  progress.Queued,
			"depth":   progress.Depth,
		})
	})
	if err != nil {
		return Result{}, err
	}
	return s.syncHTMLLikeDocs(ctx, source, "", docs)
}

func (s *Service) updateJobProgress(ctx context.Context, jobID string, progress map[string]any) error {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return nil
	}
	data, err := json.Marshal(progress)
	if err != nil {
		return err
	}
	return s.store.UpdateJobProgress(ctx, jobID, string(data))
}

func (s *Service) syncHTMLLikeDocs(ctx context.Context, source storage.Source, root string, docs []htmldocs.Document) (Result, error) {
	indexed := make([]indexedHTMLDocument, 0, len(docs))
	keepDocumentIDs := make([]string, 0, len(docs))
	for _, scanned := range docs {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		docID := ids.Stable("doc", source.ID, scanned.Path)
		keepDocumentIDs = append(keepDocumentIDs, docID)
		doc := storage.DocumentInput{
			ID:          docID,
			SourceID:    source.ID,
			ExternalID:  scanned.Path,
			Title:       scanned.Title,
			URL:         scanned.URL,
			ContentHash: scanned.Hash,
		}
		if doc.URL == "" {
			doc.URL = docURL(root, scanned.Path)
		}
		sections := make([]storage.SectionInput, 0, len(scanned.Sections))
		for _, scannedSection := range scanned.Sections {
			sections = append(sections, storage.SectionInput{
				ID:          ids.Stable("sec", docID, fmt.Sprint(scannedSection.Ordinal), scannedSection.Hash),
				DocumentID:  docID,
				HeadingPath: strings.Join(scannedSection.HeadingPath, " > "),
				Title:       scannedSection.Title,
				Content:     scannedSection.Content,
				ContentHash: scannedSection.Hash,
				Ordinal:     scannedSection.Ordinal,
			})
		}
		doc.ContentHash = indexedDocumentHash(doc.ContentHash, sections)
		plan, err := s.planDocumentSync(ctx, source, doc)
		if err != nil {
			return Result{}, err
		}
		if plan.replace {
			if err := s.store.ReplaceDocument(ctx, doc, sections); err != nil {
				return Result{}, err
			}
			if err := s.syncSectionEntities(ctx, doc, sections, false); err != nil {
				return Result{}, err
			}
		}
		if plan.profile {
			if err := s.syncDocumentProfile(ctx, doc, sections); err != nil {
				return Result{}, err
			}
		}
		indexed = append(indexed, indexedHTMLDocument{scanned: scanned, doc: doc, sections: sections, graph: plan.graph})
	}
	if err := s.store.DeleteDocumentsNotInSource(ctx, source.ID, keepDocumentIDs); err != nil {
		return Result{}, err
	}

	for _, item := range indexed {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if !item.graph {
			continue
		}
		if err := s.syncDocumentGraph(ctx, source, item.doc, item.sections, false); err != nil {
			return Result{}, err
		}
	}
	brokenLinks, err := s.syncHTMLLinks(ctx, indexed)
	if err != nil {
		return Result{}, err
	}

	return Result{SourceID: source.ID, Documents: len(docs), BrokenLinks: brokenLinks}, nil
}

func (s *Service) syncConfluence(ctx context.Context, source storage.Source) (Result, error) {
	configJSON, err := s.resolveConfluenceConfig(ctx, source)
	if err != nil {
		return Result{}, err
	}
	docs, err := confluence.Load(ctx, source.DSN, configJSON)
	if err != nil {
		return Result{}, err
	}

	indexed := make([]indexedDocument, 0, len(docs))
	keepDocumentIDs := make([]string, 0, len(docs))
	for _, scanned := range docs {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		externalID, webURL := splitConfluenceDocumentPath(scanned.Path)
		docID := ids.Stable("doc", source.ID, externalID)
		keepDocumentIDs = append(keepDocumentIDs, docID)
		doc := storage.DocumentInput{
			ID:          docID,
			SourceID:    source.ID,
			ExternalID:  externalID,
			Title:       scanned.Title,
			URL:         webURL,
			ContentHash: scanned.Hash,
		}
		sections := make([]storage.SectionInput, 0, len(scanned.Sections))
		for _, scannedSection := range scanned.Sections {
			sections = append(sections, storage.SectionInput{
				ID:          ids.Stable("sec", docID, fmt.Sprint(scannedSection.Ordinal), scannedSection.Hash),
				DocumentID:  docID,
				HeadingPath: strings.Join(scannedSection.HeadingPath, " > "),
				Title:       scannedSection.Title,
				Content:     scannedSection.Content,
				ContentHash: scannedSection.Hash,
				Ordinal:     scannedSection.Ordinal,
			})
		}
		doc.ContentHash = indexedDocumentHash(doc.ContentHash, sections)
		plan, err := s.planDocumentSync(ctx, source, doc)
		if err != nil {
			return Result{}, err
		}
		if plan.replace {
			if err := s.store.ReplaceDocument(ctx, doc, sections); err != nil {
				return Result{}, err
			}
			if err := s.syncSectionEntities(ctx, doc, sections, false); err != nil {
				return Result{}, err
			}
		}
		if plan.profile {
			if err := s.syncDocumentProfile(ctx, doc, sections); err != nil {
				return Result{}, err
			}
		}
		indexed = append(indexed, indexedDocument{doc: doc, sections: sections, graph: plan.graph})
	}
	if err := s.store.DeleteDocumentsNotInSource(ctx, source.ID, keepDocumentIDs); err != nil {
		return Result{}, err
	}
	for _, item := range indexed {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if !item.graph {
			continue
		}
		if err := s.syncDocumentGraph(ctx, source, item.doc, item.sections, false); err != nil {
			return Result{}, err
		}
	}

	return Result{SourceID: source.ID, Documents: len(docs)}, nil
}

func (s *Service) resolveConfluenceConfig(ctx context.Context, source storage.Source) (string, error) {
	config := map[string]any{}
	raw := strings.TrimSpace(source.ConfigJSON)
	if raw == "" {
		raw = "{}"
	}
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		return "", fmt.Errorf("parse confluence source config_json: %w", err)
	}
	credentialID, _ := config["cookie_credential_id"].(string)
	credentialID = strings.TrimSpace(credentialID)
	if credentialID == "" {
		return raw, nil
	}
	credential, err := s.store.GetConfluenceCookieCredential(ctx, credentialID)
	if err != nil {
		return "", fmt.Errorf("confluence cookie credential %q not found", credentialID)
	}
	if strings.TrimSpace(credential.Cookie) == "" {
		return "", fmt.Errorf("confluence cookie credential %q has no cookie", credentialID)
	}
	config["cookie"] = strings.TrimSpace(credential.Cookie)
	data, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (s *Service) syncMarkdownDocs(ctx context.Context, source storage.Source, root string, urlPrefix string) (Result, error) {
	docs, err := localdocs.Scan(root)
	if err != nil {
		return Result{}, err
	}

	indexed := make([]indexedDocument, 0, len(docs))
	keepDocumentIDs := make([]string, 0, len(docs))
	for _, scanned := range docs {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		docID := ids.Stable("doc", source.ID, scanned.Path)
		keepDocumentIDs = append(keepDocumentIDs, docID)
		doc := storage.DocumentInput{
			ID:          docID,
			SourceID:    source.ID,
			ExternalID:  scanned.Path,
			Title:       scanned.Title,
			URL:         docURL(urlPrefix, scanned.Path),
			ContentHash: scanned.Hash,
		}
		sections := make([]storage.SectionInput, 0, len(scanned.Sections))
		for _, scannedSection := range scanned.Sections {
			sections = append(sections, storage.SectionInput{
				ID:          ids.Stable("sec", docID, fmt.Sprint(scannedSection.Ordinal), scannedSection.Hash),
				DocumentID:  docID,
				HeadingPath: strings.Join(scannedSection.HeadingPath, " > "),
				Title:       scannedSection.Title,
				Content:     scannedSection.Content,
				ContentHash: scannedSection.Hash,
				Ordinal:     scannedSection.Ordinal,
			})
		}
		doc.ContentHash = indexedDocumentHash(doc.ContentHash, sections)
		plan, err := s.planDocumentSync(ctx, source, doc)
		if err != nil {
			return Result{}, err
		}
		if plan.replace {
			if err := s.store.ReplaceDocument(ctx, doc, sections); err != nil {
				return Result{}, err
			}
			if err := s.syncSectionEntities(ctx, doc, sections, false); err != nil {
				return Result{}, err
			}
		}
		if plan.profile {
			if err := s.syncDocumentProfile(ctx, doc, sections); err != nil {
				return Result{}, err
			}
		}
		indexed = append(indexed, indexedDocument{doc: doc, sections: sections, graph: plan.graph})
	}
	if err := s.store.DeleteDocumentsNotInSource(ctx, source.ID, keepDocumentIDs); err != nil {
		return Result{}, err
	}

	for _, item := range indexed {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if !item.graph {
			continue
		}
		if err := s.syncDocumentGraph(ctx, source, item.doc, item.sections, false); err != nil {
			return Result{}, err
		}
	}

	return Result{SourceID: source.ID, Documents: len(docs)}, nil
}

func (s *Service) syncOpenAPI(ctx context.Context, source storage.Source) (Result, error) {
	path, err := LocalPathFromDSN(source.DSN)
	if err != nil {
		return Result{}, err
	}
	scanned, err := openapi.Load(path)
	if err != nil {
		return Result{}, err
	}

	docID := ids.Stable("doc", source.ID, scanned.Path)
	doc := storage.DocumentInput{
		ID:          docID,
		SourceID:    source.ID,
		ExternalID:  scanned.Path,
		Title:       scanned.Title,
		URL:         filepath.ToSlash(path),
		ContentHash: scanned.Hash,
	}
	sections := make([]storage.SectionInput, 0, len(scanned.Sections))
	for _, scannedSection := range scanned.Sections {
		sections = append(sections, storage.SectionInput{
			ID:          ids.Stable("sec", docID, fmt.Sprint(scannedSection.Ordinal), scannedSection.Hash),
			DocumentID:  docID,
			HeadingPath: strings.Join(scannedSection.HeadingPath, " > "),
			Title:       scannedSection.Title,
			Content:     scannedSection.Content,
			ContentHash: scannedSection.Hash,
			Ordinal:     scannedSection.Ordinal,
		})
	}
	doc.ContentHash = indexedDocumentHash(doc.ContentHash, sections)
	plan, err := s.planDocumentSync(ctx, source, doc)
	if err != nil {
		return Result{}, err
	}
	if plan.replace {
		if err := s.store.ReplaceDocument(ctx, doc, sections); err != nil {
			return Result{}, err
		}
		if err := s.syncSectionEntities(ctx, doc, sections, true); err != nil {
			return Result{}, err
		}
	}
	if plan.profile {
		if err := s.syncDocumentProfile(ctx, doc, sections); err != nil {
			return Result{}, err
		}
	}
	if err := s.store.DeleteDocumentsNotInSource(ctx, source.ID, []string{doc.ID}); err != nil {
		return Result{}, err
	}
	if plan.graph {
		if err := s.syncDocumentGraph(ctx, source, doc, sections, true); err != nil {
			return Result{}, err
		}
	}
	return Result{SourceID: source.ID, Documents: 1}, nil
}

func (s *Service) planDocumentSync(ctx context.Context, source storage.Source, doc storage.DocumentInput) (documentSyncPlan, error) {
	full := documentSyncPlan{replace: true, profile: true, graph: true}
	existing, err := s.store.GetDocumentBySourceExternalID(ctx, doc.SourceID, doc.ExternalID)
	if errors.Is(err, sql.ErrNoRows) {
		return full, nil
	}
	if err != nil {
		return documentSyncPlan{}, err
	}
	if existing.ContentHash != doc.ContentHash {
		return full, nil
	}

	profileCurrent := false
	existingProfile, err := s.store.GetDocumentProfile(ctx, doc.ID)
	if err == nil && existingProfile.GeneratedFromHash == doc.ContentHash {
		profileCurrent = true
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return documentSyncPlan{}, err
	}

	return documentSyncPlan{
		replace: false,
		profile: !profileCurrent,
		graph:   true,
	}, nil
}

func indexedDocumentHash(sourceHash string, sections []storage.SectionInput) string {
	sum := sha256.New()
	sum.Write([]byte(strings.TrimSpace(sourceHash)))
	sum.Write([]byte{0})
	for _, section := range sections {
		sum.Write([]byte(fmt.Sprint(section.Ordinal)))
		sum.Write([]byte{0})
		sum.Write([]byte(section.Title))
		sum.Write([]byte{0})
		sum.Write([]byte(section.HeadingPath))
		sum.Write([]byte{0})
		sum.Write([]byte(section.ContentHash))
		sum.Write([]byte{0})
	}
	return hex.EncodeToString(sum.Sum(nil))
}

func (s *Service) syncDocumentProfile(ctx context.Context, doc storage.DocumentInput, sections []storage.SectionInput) error {
	existing, err := s.store.GetDocumentProfile(ctx, doc.ID)
	if err == nil && existing.GeneratedFromHash == doc.ContentHash {
		return nil
	}
	entities, err := s.store.ListDocumentEntities(ctx, doc.ID)
	if err != nil {
		return err
	}
	profileJSON, err := profile.BuildJSONWithAPIRefs(doc, sections, apiRefsFromSectionEntities(entities))
	if err != nil {
		return err
	}
	_, err = s.store.UpsertDocumentRetrievalProfile(ctx, storage.RetrievalProfileInput{
		DocumentID:           doc.ID,
		RetrievalProfileJSON: profileJSON,
		GeneratedFromHash:    doc.ContentHash,
	})
	return err
}

func apiRefsFromSectionEntities(entities []storage.SectionEntity) []string {
	refs := make([]string, 0, len(entities))
	for _, entity := range entities {
		switch entity.Kind {
		case string(extract.EntityAPIEndpoint):
			if entity.Method != "" && entity.Path != "" {
				refs = append(refs, entity.Method+" "+entity.Path)
			} else if entity.CanonicalText != "" {
				refs = append(refs, entity.CanonicalText)
			}
		case string(extract.EntityPathLiteral):
			if entity.Path != "" {
				refs = append(refs, entity.Path)
			}
		}
	}
	return uniqueStrings(refs)
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func (s *Service) syncSectionEntities(ctx context.Context, doc storage.DocumentInput, sections []storage.SectionInput, openAPISource bool) error {
	entities := sectionEntityInputsForDocument(doc, sections, openAPISource)
	return s.store.ReplaceSectionEntities(ctx, doc.ID, entities)
}

func sectionEntityInputsForDocument(doc storage.DocumentInput, sections []storage.SectionInput, openAPISource bool) []storage.SectionEntityInput {
	entities := make([]storage.SectionEntityInput, 0)
	for _, section := range sections {
		candidates := extract.ExtractSection(extract.SectionInput{
			DocumentID:  doc.ID,
			SectionID:   section.ID,
			Title:       section.Title,
			HeadingPath: section.HeadingPath,
			Content:     section.Content,
		})
		if openAPISource {
			if ref, ok := apiRefFromOpenAPISection(section); ok {
				candidates = append(candidates, extract.FromOpenAPIEndpoint(extract.OpenAPIEndpointInput{
					DocumentID: doc.ID,
					SectionID:  section.ID,
					Method:     ref.Method,
					Path:       ref.Path,
					Operation:  openAPIOperationFromSection(section),
				}))
			}
		}
		for _, candidate := range candidates {
			entities = append(entities, sectionEntityInputFromCandidate(candidate))
		}
	}
	return entities
}

func sectionEntityInputFromCandidate(candidate extract.EntityCandidate) storage.SectionEntityInput {
	return storage.SectionEntityInput{
		SectionID:     candidate.SectionID,
		Kind:          string(candidate.Kind),
		RawText:       candidate.Raw,
		CanonicalText: candidate.Canonical,
		Method:        candidate.Method,
		Path:          candidate.Path,
		Operation:     candidate.Operation,
		Source:        string(candidate.Source),
		Confidence:    candidate.Confidence,
		Evidence:      candidate.Evidence,
		SpanStart:     candidate.SpanStart,
		SpanEnd:       candidate.SpanEnd,
		Notes:         candidate.Notes,
	}
}

func openAPIOperationFromSection(section storage.SectionInput) string {
	for _, line := range strings.Split(section.Content, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "operationId") {
			continue
		}
		return strings.TrimSpace(value)
	}
	title := strings.TrimSpace(section.Title)
	if title == "" {
		return ""
	}
	fields := strings.Fields(title)
	if len(fields) == 0 {
		return ""
	}
	last := fields[len(fields)-1]
	if strings.HasPrefix(last, "/") || isHTTPMethodToken(last) {
		return ""
	}
	return last
}

func isHTTPMethodToken(value string) bool {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "HEAD", "TRACE":
		return true
	default:
		return false
	}
}

func (s *Service) syncDocumentGraph(ctx context.Context, source storage.Source, doc storage.DocumentInput, sections []storage.SectionInput, openAPISource bool) error {
	entities, err := s.store.ListDocumentEntities(ctx, doc.ID)
	if err != nil {
		return err
	}
	entityRefsBySection := apiRefsFromSectionEntitiesBySection(entities)
	productNode, hasProduct := sourceHintNode("Product", "prd", source.ID, source.ProductHint)
	moduleNode, hasModule := sourceHintNode("Module", "mod", source.ID, source.ModuleHint)
	if hasProduct {
		if err := s.store.UpsertNode(ctx, productNode); err != nil {
			return err
		}
	}
	if hasModule {
		if err := s.store.UpsertNode(ctx, moduleNode); err != nil {
			return err
		}
	}
	if hasProduct && hasModule {
		if err := s.store.UpsertEdge(ctx, graphEdge(productNode.ID, moduleNode.ID, "contains", "", doc.ContentHash, "source_hint")); err != nil {
			return err
		}
	}

	docNode := storage.NodeInput{
		ID:            documentNodeID(doc.ID),
		Kind:          "Document",
		Name:          doc.Title,
		CanonicalName: canonical(doc.SourceID, doc.ExternalID),
		MetadataJSON: metadataJSON(map[string]string{
			"document_id": doc.ID,
			"source_id":   doc.SourceID,
			"external_id": doc.ExternalID,
			"url":         doc.URL,
			"version":     doc.Version,
		}),
		Confidence: 1,
	}
	if err := s.store.UpsertNode(ctx, docNode); err != nil {
		return err
	}
	if hasProduct {
		if err := s.store.UpsertEdge(ctx, graphEdge(productNode.ID, docNode.ID, "describes", "", doc.ContentHash, "source_hint")); err != nil {
			return err
		}
	}
	if hasModule {
		if err := s.store.UpsertEdge(ctx, graphEdge(moduleNode.ID, docNode.ID, "describes", "", doc.ContentHash, "source_hint")); err != nil {
			return err
		}
	}

	for _, section := range sections {
		if err := ctx.Err(); err != nil {
			return err
		}
		sectionNode := storage.NodeInput{
			ID:            sectionNodeID(section.ID),
			Kind:          "DocSection",
			Name:          sectionNodeName(section),
			CanonicalName: canonical(doc.SourceID, doc.ExternalID, fmt.Sprint(section.Ordinal)),
			MetadataJSON: metadataJSON(map[string]string{
				"section_id":    section.ID,
				"document_id":   doc.ID,
				"source_id":     doc.SourceID,
				"heading_path":  section.HeadingPath,
				"content_hash":  section.ContentHash,
				"document_url":  doc.URL,
				"document_name": doc.Title,
			}),
			Confidence: 1,
		}
		if err := s.store.UpsertNode(ctx, sectionNode); err != nil {
			return err
		}
		if err := s.store.UpsertEdge(ctx, graphEdge(docNode.ID, sectionNode.ID, "contains", section.ID, section.ContentHash, "sync")); err != nil {
			return err
		}

		refs := entityRefsBySection[section.ID]
		if len(refs) == 0 {
			refs = apiRefsFromSection(section)
		}
		if openAPISource {
			if ref, ok := apiRefFromOpenAPISection(section); ok {
				refs = append(refs, ref)
			}
		}
		refs = uniqueAPIRefs(refs)
		for _, ref := range refs {
			apiNode := apiNodeInput(source.ID, ref)
			if err := s.store.UpsertNode(ctx, apiNode); err != nil {
				return err
			}
			edgeKind := "mentions"
			if openAPISource {
				edgeKind = "describes"
			}
			if err := s.store.UpsertEdge(ctx, graphEdge(sectionNode.ID, apiNode.ID, edgeKind, section.ID, section.ContentHash, "rule")); err != nil {
				return err
			}
			if openAPISource {
				if err := s.store.UpsertEdge(ctx, graphEdge(docNode.ID, apiNode.ID, "exposes_api", section.ID, doc.ContentHash, "rule")); err != nil {
					return err
				}
			}
			if hasProduct {
				if err := s.store.UpsertEdge(ctx, graphEdge(productNode.ID, apiNode.ID, "exposes_api", section.ID, section.ContentHash, "source_hint")); err != nil {
					return err
				}
			}
			if hasModule {
				if err := s.store.UpsertEdge(ctx, graphEdge(moduleNode.ID, apiNode.ID, "exposes_api", section.ID, section.ContentHash, "source_hint")); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func apiRefsFromSectionEntitiesBySection(entities []storage.SectionEntity) map[string][]apiRef {
	refsBySection := make(map[string][]apiRef)
	for _, entity := range entities {
		if entity.Kind != string(extract.EntityAPIEndpoint) {
			continue
		}
		ref := apiRefFromSectionEntity(entity)
		if ref.Method == "" || ref.Path == "" || entity.SectionID == "" {
			continue
		}
		refsBySection[entity.SectionID] = append(refsBySection[entity.SectionID], ref)
	}
	for sectionID, refs := range refsBySection {
		refsBySection[sectionID] = uniqueAPIRefs(refs)
	}
	return refsBySection
}

func apiRefFromSectionEntity(entity storage.SectionEntity) apiRef {
	return apiRef{
		Method: strings.ToUpper(strings.TrimSpace(entity.Method)),
		Path:   strings.TrimSpace(entity.Path),
	}
}

func (s *Service) syncHTMLLinks(ctx context.Context, indexed []indexedHTMLDocument) ([]storage.BrokenLink, error) {
	targets := map[string]htmlSectionRef{}
	for _, item := range indexed {
		docPath := filepath.ToSlash(item.scanned.Path)
		headingAnchors := map[string]int{}
		for i, scannedSection := range item.scanned.Sections {
			if i >= len(item.sections) {
				continue
			}
			section := item.sections[i]
			ref := htmlSectionRef{section: section, docPath: docPath}
			if scannedSection.Anchor != "" {
				targets[htmlTargetKey(docPath, scannedSection.Anchor)] = ref
			}
			for _, anchor := range htmlHeadingAnchorCandidates(scannedSection.Title, headingAnchors) {
				key := htmlTargetKey(docPath, anchor)
				if _, exists := targets[key]; !exists {
					targets[key] = ref
				}
			}
			if i == 0 {
				targets[htmlTargetKey(docPath, "")] = ref
				for _, anchor := range item.scanned.Anchors {
					key := htmlTargetKey(docPath, anchor)
					if _, exists := targets[key]; !exists {
						targets[key] = ref
					}
				}
			}
		}
	}

	broken := make([]storage.BrokenLink, 0)
	for _, item := range indexed {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sourceDocPath := filepath.ToSlash(item.scanned.Path)
		for i, scannedSection := range item.scanned.Sections {
			if i >= len(item.sections) {
				continue
			}
			fromSection := item.sections[i]
			for _, link := range scannedSection.Links {
				resolvedTargets := resolveHTMLHrefCandidates(sourceDocPath, link.Href)
				resolvedTarget := ""
				if len(resolvedTargets) > 0 {
					resolvedTarget = resolvedTargets[0]
				}
				target, ok := firstHTMLTarget(targets, resolvedTargets)
				if !ok || target.section.ID == fromSection.ID {
					if !ok {
						broken = append(broken, storage.BrokenLink{
							SourceDocument: sourceDocPath,
							SourceSection:  scannedSection.Title,
							Href:           link.Href,
							Text:           link.Text,
							ResolvedTarget: resolvedTarget,
						})
					}
					continue
				}
				edge := graphEdge(sectionNodeID(fromSection.ID), sectionNodeID(target.section.ID), "links_to", fromSection.ID, fromSection.ContentHash, "html_link")
				if err := s.store.UpsertEdge(ctx, edge); err != nil {
					return nil, err
				}
			}
		}
	}
	return uniqueBrokenLinks(broken), nil
}

func LocalPathFromDSN(dsn string) (string, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return "", fmt.Errorf("local source dsn is required")
	}
	if strings.HasPrefix(dsn, "file://") {
		path := strings.TrimPrefix(dsn, "file://")
		if path == "" {
			return "", fmt.Errorf("file DSN requires a path")
		}
		return normalizeLocalPath(path), nil
	}
	if strings.Contains(dsn, "://") {
		return "", fmt.Errorf("unsupported local source dsn %q", dsn)
	}
	return normalizeLocalPath(dsn), nil
}

func normalizeLocalPath(path string) string {
	path = strings.TrimSpace(path)
	if len(path) >= 4 && path[0] == '/' && isWindowsDrivePath(path[1:]) {
		return normalizeLocalPath(path[1:])
	}
	if len(path) >= 3 && isWindowsDrivePath(path) {
		drive := strings.ToLower(path[:1])
		rest := strings.TrimLeft(path[2:], `\/`)
		return filepath.Clean("/mnt/" + drive + "/" + strings.ReplaceAll(rest, `\`, "/"))
	}
	if strings.HasPrefix(path, `\\wsl$\`) || strings.HasPrefix(path, `\\wsl.localhost\`) {
		parts := strings.Split(strings.TrimLeft(path, `\`), `\`)
		if len(parts) >= 3 {
			return filepath.Clean("/" + strings.Join(parts[2:], "/"))
		}
	}
	return filepath.Clean(path)
}

func isWindowsDrivePath(path string) bool {
	c := path[0]
	return ((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')) && path[1] == ':' && (path[2] == '\\' || path[2] == '/')
}

func docURL(prefix string, relPath string) string {
	prefix = strings.TrimSpace(prefix)
	relPath = filepath.ToSlash(relPath)
	if prefix == "" {
		return relPath
	}
	if strings.Contains(prefix, "://") {
		return strings.TrimRight(prefix, "/") + "/" + relPath
	}
	return filepath.ToSlash(filepath.Join(prefix, filepath.FromSlash(relPath)))
}

func docURLPrefix(configJSON string, fallback string) string {
	var value struct {
		URLPrefix string `json:"url_prefix"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(configJSON)), &value); err == nil && strings.TrimSpace(value.URLPrefix) != "" {
		return strings.TrimSpace(value.URLPrefix)
	}
	return fallback
}

func resolveHTMLHref(sourceDocPath string, href string) string {
	candidates := resolveHTMLHrefCandidates(sourceDocPath, href)
	if len(candidates) == 0 {
		return htmlTargetKey(sourceDocPath, "")
	}
	return candidates[0]
}

func resolveHTMLHrefCandidates(sourceDocPath string, href string) []string {
	href = strings.TrimSpace(href)
	parts := strings.SplitN(href, "#", 2)
	targetPath := strings.TrimSpace(strings.SplitN(parts[0], "?", 2)[0])
	anchor := ""
	if len(parts) == 2 {
		anchor = strings.TrimSpace(parts[1])
	}
	paths := make([]string, 0, 6)
	if targetPath == "" {
		paths = append(paths, filepath.ToSlash(sourceDocPath))
	} else {
		cleanTarget := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(filepath.FromSlash(targetPath))), "./")
		if strings.HasPrefix(targetPath, "/") {
			paths = append(paths, strings.TrimPrefix(cleanTarget, "/"))
		} else {
			paths = append(paths,
				filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(filepath.FromSlash(sourceDocPath)), filepath.FromSlash(targetPath)))),
				cleanTarget,
			)
		}
	}
	out := make([]string, 0, len(paths)*3)
	seen := map[string]bool{}
	for _, path := range paths {
		for _, candidate := range htmlPathCandidates(path) {
			key := htmlTargetKey(candidate, anchor)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, key)
		}
	}
	return out
}

func firstHTMLTarget(targets map[string]htmlSectionRef, keys []string) (htmlSectionRef, bool) {
	for _, key := range keys {
		if target, ok := targets[key]; ok {
			return target, true
		}
	}
	return htmlSectionRef{}, false
}

func htmlPathCandidates(path string) []string {
	path = filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(path))))
	if path == "." || path == "" {
		path = "index.html"
	}
	candidates := []string{path}
	if base := strings.ToLower(filepath.Base(path)); base == ".html" || base == ".htm" {
		dir := filepath.ToSlash(filepath.Dir(filepath.FromSlash(path)))
		if dir == "." || dir == "" {
			candidates = append(candidates, "index.html")
		} else {
			candidates = append(candidates, filepath.ToSlash(filepath.Join(dir, "index.html")))
		}
	}
	if strings.HasSuffix(path, "/") || strings.HasSuffix(path, "/.") {
		candidates = append(candidates, strings.TrimRight(path, "/.")+"/index.html")
	}
	if strings.ToLower(filepath.Base(path)) != "index.html" && filepath.Ext(path) == "" {
		candidates = append(candidates, strings.TrimRight(path, "/")+"/index.html", path+".html")
	}
	if strings.HasSuffix(strings.ToLower(path), "/index.html") {
		candidates = append(candidates, strings.TrimSuffix(path, "index.html"))
		candidates = append(candidates, strings.TrimSuffix(path, "/index.html"))
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		candidate = filepath.ToSlash(filepath.Clean(filepath.FromSlash(candidate)))
		if candidate == "." || candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		out = append(out, candidate)
	}
	return out
}

func htmlTargetKey(docPath string, anchor string) string {
	docPath = filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(docPath))))
	if docPath == "." || docPath == "" {
		docPath = "index.html"
	}
	return docPath + "#" + normalizeHTMLAnchor(anchor)
}

func normalizeHTMLAnchor(anchor string) string {
	anchor = strings.TrimPrefix(strings.TrimSpace(anchor), "#")
	if anchor == "" {
		return ""
	}
	if decoded, err := url.PathUnescape(anchor); err == nil {
		return strings.TrimSpace(decoded)
	}
	return anchor
}

func htmlHeadingAnchorCandidates(title string, counts map[string]int) []string {
	slug := htmlHeadingSlug(title)
	if slug == "" {
		return nil
	}
	count := counts[slug]
	counts[slug] = count + 1
	if count == 0 {
		return []string{slug}
	}
	return []string{fmt.Sprintf("%s-%d", slug, count)}
}

func htmlHeadingSlug(title string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.TrimSpace(title) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
			lastDash = false
		case unicode.IsSpace(r) || r == '-' || r == '_':
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func uniqueBrokenLinks(links []storage.BrokenLink) []storage.BrokenLink {
	seen := make(map[string]storage.BrokenLink, len(links))
	for _, link := range links {
		link.SourceDocument = strings.TrimSpace(link.SourceDocument)
		link.SourceSection = strings.TrimSpace(link.SourceSection)
		link.Href = strings.TrimSpace(link.Href)
		link.Text = strings.TrimSpace(link.Text)
		link.ResolvedTarget = strings.TrimSpace(link.ResolvedTarget)
		if link.SourceDocument == "" || link.Href == "" {
			continue
		}
		key := strings.Join([]string{link.SourceDocument, link.SourceSection, link.Href, link.ResolvedTarget}, "\x00")
		seen[key] = link
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]storage.BrokenLink, 0, len(keys))
	for _, key := range keys {
		out = append(out, seen[key])
	}
	return out
}

func splitConfluenceDocumentPath(value string) (string, string) {
	parts := strings.SplitN(value, "|", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return value, value
}

type apiRef struct {
	Method string
	Path   string
}

var apiRefPattern = regexp.MustCompile(`\b(GET|POST|PUT|PATCH|DELETE|OPTIONS|HEAD|TRACE)\s+(/[A-Za-z0-9._~:/?#\[\]@!$&'()*+,;=%{}-]+)`)

func apiRefsFromSection(section storage.SectionInput) []apiRef {
	matches := apiRefPattern.FindAllStringSubmatch(section.Content+"\n"+section.Title, -1)
	refs := make([]apiRef, 0, len(matches))
	for _, match := range matches {
		refs = append(refs, apiRef{
			Method: strings.ToUpper(match[1]),
			Path:   strings.TrimRight(match[2], ".,;:"),
		})
	}
	return refs
}

func apiRefFromOpenAPISection(section storage.SectionInput) (apiRef, bool) {
	match := apiRefPattern.FindStringSubmatch(section.Title)
	if len(match) == 3 {
		return apiRef{Method: strings.ToUpper(match[1]), Path: strings.TrimRight(match[2], ".,;:")}, true
	}
	return apiRef{}, false
}

func uniqueAPIRefs(refs []apiRef) []apiRef {
	seen := make(map[string]apiRef, len(refs))
	for _, ref := range refs {
		ref.Method = strings.ToUpper(strings.TrimSpace(ref.Method))
		ref.Path = strings.TrimSpace(ref.Path)
		if ref.Method == "" || ref.Path == "" {
			continue
		}
		seen[ref.Method+" "+ref.Path] = ref
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]apiRef, 0, len(keys))
	for _, key := range keys {
		out = append(out, seen[key])
	}
	return out
}

func apiNodeInput(sourceID string, ref apiRef) storage.NodeInput {
	name := ref.Method + " " + ref.Path
	return storage.NodeInput{
		ID:            ids.Stable("api", sourceID, ref.Method, ref.Path),
		Kind:          "API",
		Name:          name,
		CanonicalName: canonical(ref.Method, ref.Path),
		MetadataJSON: metadataJSON(map[string]string{
			"source_id": sourceID,
			"method":    ref.Method,
			"path":      ref.Path,
		}),
		Confidence: 1,
	}
}

func documentNodeID(documentID string) string {
	return ids.Stable("node", "document", documentID)
}

func sectionNodeID(sectionID string) string {
	return ids.Stable("node", "section", sectionID)
}

func graphEdge(srcID, dstID, kind, evidenceSectionID, sourceRevision, provenance string) storage.EdgeInput {
	return storage.EdgeInput{
		ID:                ids.Stable("edge", srcID, dstID, kind),
		SrcID:             srcID,
		DstID:             dstID,
		Kind:              kind,
		Confidence:        1,
		Provenance:        provenance,
		EvidenceSectionID: evidenceSectionID,
		SourceRevision:    sourceRevision,
		MetadataJSON:      "{}",
	}
}

func sourceHintNode(kind, prefix, sourceID, name string) (storage.NodeInput, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return storage.NodeInput{}, false
	}
	return storage.NodeInput{
		ID:            ids.Stable(prefix, sourceID, canonical(name)),
		Kind:          kind,
		Name:          name,
		CanonicalName: canonical(name),
		MetadataJSON: metadataJSON(map[string]string{
			"source_id": sourceID,
			"origin":    "source_hint",
		}),
		Confidence: 1,
	}, true
}

func sectionNodeName(section storage.SectionInput) string {
	if strings.TrimSpace(section.Title) != "" {
		return strings.TrimSpace(section.Title)
	}
	if strings.TrimSpace(section.HeadingPath) != "" {
		return strings.TrimSpace(section.HeadingPath)
	}
	return fmt.Sprintf("Section %d", section.Ordinal)
}

func canonical(parts ...string) string {
	joined := strings.Join(parts, " ")
	joined = strings.ToLower(strings.TrimSpace(joined))
	return strings.Join(strings.Fields(joined), " ")
}

func metadataJSON(value map[string]string) string {
	data, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(data)
}
