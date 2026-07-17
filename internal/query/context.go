package query

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/docgraph/docgraph/internal/domain"
	"github.com/docgraph/docgraph/internal/storage"
)

type Service struct {
	store storage.Store
}

func NewService(store storage.Store) *Service {
	return &Service{store: store}
}

type ContextRequest struct {
	Task        string `json:"task"`
	Query       string `json:"query"`
	MaxSections int    `json:"max_sections"`
	MaxChars    int    `json:"max_chars"`
}

type ContextPack struct {
	Task     string           `json:"task"`
	Query    string           `json:"query"`
	Sections []ContextSection `json:"sections"`
}

type ContextSection struct {
	SectionID         string                      `json:"section_id"`
	NodeID            string                      `json:"node_id,omitempty"`
	DocumentID        string                      `json:"document_id"`
	DocumentTitle     string                      `json:"document_title"`
	DocumentURL       string                      `json:"document_url"`
	Desc              string                      `json:"desc,omitempty"`
	Title             string                      `json:"title"`
	ElementKind       string                      `json:"element_kind,omitempty"`
	DisplayNumber     string                      `json:"display_number,omitempty"`
	HeadingPath       string                      `json:"heading_path"`
	Ancestry          []storage.SectionBreadcrumb `json:"ancestry,omitempty"`
	Content           string                      `json:"content"`
	MediaAssets       []storage.MediaAssetSummary `json:"media_assets,omitempty"`
	AuthoredRelations []storage.RelatedNode       `json:"authored_relations,omitempty"`
	Provenance        *ContextProvenance          `json:"provenance,omitempty"`
	Evidence          string                      `json:"evidence"`
}

type ContextProvenance struct {
	SourceID        string `json:"source_id,omitempty"`
	DocumentID      string `json:"document_id"`
	SectionID       string `json:"section_id"`
	NodeID          string `json:"node_id,omitempty"`
	SourceElementID string `json:"source_element_id,omitempty"`
	SnapshotID      string `json:"snapshot_id,omitempty"`
	SnapshotHash    string `json:"snapshot_hash,omitempty"`
	FormatFamily    string `json:"format_family,omitempty"`
	FormatVersion   string `json:"format_version,omitempty"`
}

const (
	maxAuthoredRelationsPerSection = 5
	maxAuthoredSuggestions         = 20
)

func (s *Service) Search(ctx context.Context, query string, limit int) ([]storage.SearchHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	return s.store.SearchSections(ctx, query, limit)
}

func (s *Service) SearchDetailed(ctx context.Context, opts storage.SearchOptions) (storage.SearchResult, error) {
	opts.Query = strings.TrimSpace(opts.Query)
	if opts.Query == "" {
		return storage.SearchResult{}, fmt.Errorf("query is required")
	}
	result, err := s.store.SearchSectionsWithOptions(ctx, opts)
	if err != nil {
		return storage.SearchResult{}, err
	}
	if opts.UseRelationExpansion {
		if err := s.enrichAuthoredSuggestedReads(ctx, &result); err != nil {
			return storage.SearchResult{}, err
		}
	}
	return result, nil
}

func (s *Service) Context(ctx context.Context, req ContextRequest) (ContextPack, error) {
	task := strings.TrimSpace(req.Task)
	query := strings.TrimSpace(req.Query)
	if query == "" {
		query = task
	}
	if query == "" {
		return ContextPack{}, fmt.Errorf("task or query is required")
	}
	if req.MaxSections <= 0 || req.MaxSections > 20 {
		req.MaxSections = 8
	}
	if req.MaxChars <= 0 || req.MaxChars > 20000 {
		req.MaxChars = 12000
	}

	hits, err := s.store.SearchSections(ctx, query, req.MaxSections)
	if err != nil {
		return ContextPack{}, err
	}

	// Allocate character budget proportionally by relevance rank.
	// Higher-ranked sections get more chars, lower-ranked sections get less.
	// This ensures the most relevant content is preserved in full,
	// while less relevant content is truncated more aggressively.
	sections := make([]ContextSection, 0, len(hits))
	budgets := allocateBudget(hits, req.MaxChars)
	remaining := req.MaxChars
	for i, hit := range hits {
		if remaining <= 0 {
			break
		}
		// Use the proportional budget for this section, capped by remaining budget
		sectionBudget := min(budgets[i], remaining)
		content := truncateRunes(strings.TrimSpace(hit.Content), sectionBudget)
		remaining -= len(content)
		section := ContextSection{
			SectionID:     hit.SectionID,
			NodeID:        hit.NodeID,
			DocumentID:    hit.DocumentID,
			DocumentTitle: hit.DocumentTitle,
			DocumentURL:   hit.DocumentURL,
			Desc:          hit.Desc,
			Title:         hit.Title,
			ElementKind:   hit.ElementKind,
			DisplayNumber: hit.DisplayNumber,
			Ancestry:      hit.Ancestry,
			HeadingPath:   hit.HeadingPath,
			Content:       content,
			Evidence:      evidenceLabel(hit),
			Provenance: &ContextProvenance{
				SourceID:   hit.SourceID,
				DocumentID: hit.DocumentID,
				SectionID:  hit.SectionID,
				NodeID:     hit.NodeID,
			},
		}
		if err := s.enrichContextSection(ctx, &section); err != nil {
			return ContextPack{}, err
		}
		sections = append(sections, section)
	}

	return ContextPack{Task: task, Query: query, Sections: sections}, nil
}

func (s *Service) enrichContextSection(ctx context.Context, section *ContextSection) error {
	contextStore, ok := s.store.(storage.SectionContextStore)
	if !ok || section == nil || strings.TrimSpace(section.SectionID) == "" {
		return nil
	}
	detail, err := contextStore.GetSectionContext(ctx, section.SectionID, 1)
	if err != nil {
		return err
	}
	if detail.Structure != nil {
		section.ElementKind = firstNonEmpty(section.ElementKind, detail.Structure.ElementKind)
		section.DisplayNumber = firstNonEmpty(section.DisplayNumber, detail.Structure.DisplayNumber)
		section.Provenance.SourceElementID = detail.Structure.SourceElementID
	}
	if len(detail.Ancestors) > 0 {
		section.Ancestry = breadcrumbsFromSummaries(detail.Ancestors)
	}
	if len(detail.MediaAssets) > 0 {
		section.MediaAssets = detail.MediaAssets
	}
	section.AuthoredRelations = boundedRelatedNodes(detail.AuthoredRelations, maxAuthoredRelationsPerSection)
	if detail.Snapshot != nil {
		section.Provenance.SourceID = firstNonEmpty(section.Provenance.SourceID, detail.Snapshot.SourceID)
		section.Provenance.SnapshotID = detail.Snapshot.ID
		section.Provenance.SnapshotHash = detail.Snapshot.SourceHash
		section.Provenance.FormatFamily = detail.Snapshot.FormatFamily
		section.Provenance.FormatVersion = detail.Snapshot.FormatVersion
	}
	return nil
}

func (s *Service) enrichAuthoredSuggestedReads(ctx context.Context, result *storage.SearchResult) error {
	relationStore, ok := s.store.(storage.AuthoredRelationStore)
	if !ok || result == nil || len(result.Hits) == 0 {
		return nil
	}
	sectionIDs := make([]string, 0, len(result.Hits))
	for _, hit := range result.Hits {
		if id := strings.TrimSpace(hit.SectionID); id != "" {
			sectionIDs = append(sectionIDs, id)
		}
	}
	relationsBySection, err := relationStore.ListAuthoredRelationsForSections(ctx, sectionIDs, maxAuthoredRelationsPerSection)
	if err != nil {
		return err
	}
	seen := make(map[string]bool)
	for _, hit := range result.Hits {
		if len(result.SuggestedReads.AuthoredRelations) >= maxAuthoredSuggestions || strings.TrimSpace(hit.SectionID) == "" {
			break
		}
		perHit := 0
		for _, relation := range relationsBySection[hit.SectionID] {
			if perHit >= maxAuthoredRelationsPerSection || len(result.SuggestedReads.AuthoredRelations) >= maxAuthoredSuggestions {
				break
			}
			if relation.Edge.Kind != "related_to" && relation.Edge.Kind != "links_to" {
				continue
			}
			targetSectionID, targetDocumentID := relatedNodeTargets(relation)
			if targetSectionID == "" || targetSectionID == hit.SectionID {
				continue
			}
			key := strings.TrimSpace(relation.Edge.ID)
			if key == "" {
				key = strings.Join([]string{hit.SectionID, targetSectionID, relation.Edge.Kind}, "\x00")
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			result.SuggestedReads.AuthoredRelations = append(result.SuggestedReads.AuthoredRelations, domain.SuggestedRead{
				SourceSectionID:  hit.SectionID,
				TargetSectionID:  targetSectionID,
				TargetDocumentID: targetDocumentID,
				Reason:           authoredRelationReason(relation),
				Confidence:       relation.Edge.Confidence,
			})
			perHit++
		}
	}
	return nil
}

func breadcrumbsFromSummaries(sections []storage.SectionSummary) []storage.SectionBreadcrumb {
	result := make([]storage.SectionBreadcrumb, 0, len(sections))
	for _, section := range sections {
		result = append(result, storage.SectionBreadcrumb{
			SectionID:     section.ID,
			Title:         section.Title,
			DisplayNumber: section.DisplayNumber,
			Depth:         section.Depth,
		})
	}
	return result
}

func boundedRelatedNodes(relations []storage.RelatedNode, limit int) []storage.RelatedNode {
	if limit <= 0 || len(relations) <= limit {
		return relations
	}
	return relations[:limit]
}

func relatedNodeTargets(relation storage.RelatedNode) (string, string) {
	var metadata struct {
		SectionID  string `json:"section_id"`
		DocumentID string `json:"document_id"`
	}
	_ = json.Unmarshal([]byte(relation.Node.MetadataJSON), &metadata)
	return strings.TrimSpace(metadata.SectionID), strings.TrimSpace(metadata.DocumentID)
}

func authoredRelationReason(relation storage.RelatedNode) string {
	var metadata struct {
		Label string `json:"label"`
	}
	_ = json.Unmarshal([]byte(relation.Edge.MetadataJSON), &metadata)
	reason := "source-authored " + firstNonEmpty(relation.Edge.Kind, "relation")
	if strings.TrimSpace(metadata.Label) != "" {
		reason += ": " + strings.TrimSpace(metadata.Label)
	}
	return reason
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

// allocateBudget distributes totalMaxChars across hits proportionally by rank.
// Sections with higher rank receive more characters. Minimum allocation is 200 chars.
func allocateBudget(hits []storage.SearchHit, totalMaxChars int) []int {
	if len(hits) == 0 {
		return nil
	}
	// Compute weights: use rank as weight, with a minimum floor for low-rank hits.
	minWeight := 0.1
	weights := make([]float64, len(hits))
	totalWeight := 0.0
	for i, hit := range hits {
		w := math.Max(hit.Rank, minWeight)
		weights[i] = w
		totalWeight += w
	}
	// Distribute budget proportionally, with a minimum floor of 200 chars per section.
	minBudget := 200
	budgets := make([]int, len(hits))
	allocated := 0
	for i, w := range weights {
		proportional := int(float64(totalMaxChars) * w / totalWeight)
		budgets[i] = max(proportional, minBudget)
		allocated += budgets[i]
	}
	// If total allocated exceeds budget, scale down from the end (lowest rank first).
	if allocated > totalMaxChars {
		for i := len(hits) - 1; i >= 0 && allocated > totalMaxChars; i-- {
			excess := allocated - totalMaxChars
			reduction := min(budgets[i]-minBudget, excess)
			budgets[i] -= reduction
			allocated -= reduction
		}
	}
	return budgets
}

func evidenceLabel(hit storage.SearchHit) string {
	parts := make([]string, 0, 3)
	if hit.DocumentTitle != "" {
		parts = append(parts, hit.DocumentTitle)
	}
	if hit.HeadingPath != "" {
		parts = append(parts, hit.HeadingPath)
	}
	if hit.DocumentURL != "" {
		parts = append(parts, hit.DocumentURL)
	}
	return strings.Join(parts, " | ")
}

func truncateRunes(value string, max int) string {
	if max <= 0 || len(value) <= max {
		return value
	}
	// Paragraph-aware truncation: keep complete paragraphs that fit within budget.
	// Falls back to rune-level truncation only for the first oversized paragraph.
	paragraphs := splitParagraphs(value)
	var b strings.Builder
	for _, p := range paragraphs {
		if b.Len()+len(p) > max {
			if b.Len() == 0 {
				// First paragraph exceeds budget — truncate within it.
				return truncateRunesHard(p, max)
			}
			break
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(p)
	}
	return b.String()
}

func splitParagraphs(value string) []string {
	parts := strings.Split(value, "\n\n")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func truncateRunesHard(value string, max int) string {
	var b strings.Builder
	b.Grow(max)
	for _, r := range value {
		if b.Len()+len(string(r)) > max {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}
