package query

import (
	"context"
	"fmt"
	"math"
	"strings"

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
	SectionID     string `json:"section_id"`
	DocumentID    string `json:"document_id"`
	DocumentTitle string `json:"document_title"`
	DocumentURL   string `json:"document_url"`
	Desc          string `json:"desc,omitempty"`
	Title         string `json:"title"`
	HeadingPath   string `json:"heading_path"`
	Content       string `json:"content"`
	Evidence      string `json:"evidence"`
}

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
	return s.store.SearchSectionsWithOptions(ctx, opts)
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
		sections = append(sections, ContextSection{
			SectionID:     hit.SectionID,
			DocumentID:    hit.DocumentID,
			DocumentTitle: hit.DocumentTitle,
			DocumentURL:   hit.DocumentURL,
			Desc:          hit.Desc,
			Title:         hit.Title,
			HeadingPath:   hit.HeadingPath,
			Content:       content,
			Evidence:      evidenceLabel(hit),
		})
	}

	return ContextPack{Task: task, Query: query, Sections: sections}, nil
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
