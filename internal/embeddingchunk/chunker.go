package embeddingchunk

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	tiktoken "github.com/pkoukk/tiktoken-go"
	tiktokenloader "github.com/pkoukk/tiktoken-go-loader"

	"github.com/docgraph/docgraph/internal/domain"
	"github.com/docgraph/docgraph/internal/embeddingtext"
)

const DefaultChunkGeneratorVersion = "embedding-chunk-v1"

type TextMeasurer interface {
	CountTokens(text string) int
	Name() string
}

type TokenizerInfo struct {
	Name        string `json:"name"`
	Available   bool   `json:"available"`
	Description string `json:"description,omitempty"`
}

type tokenizerRegistration struct {
	info    TokenizerInfo
	factory func(model string) TextMeasurer
}

type ConservativeTextMeasurer struct{}

func (ConservativeTextMeasurer) Name() string {
	return "conservative"
}

func (ConservativeTextMeasurer) CountTokens(text string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}
	tokens := 0
	inASCIIWord := false
	asciiWordRunes := 0
	flushASCIIWord := func() {
		if !inASCIIWord {
			return
		}
		tokens += (asciiWordRunes + 3) / 4
		if tokens == 0 {
			tokens = 1
		}
		inASCIIWord = false
		asciiWordRunes = 0
	}
	for _, r := range text {
		if r <= unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '/') {
			inASCIIWord = true
			asciiWordRunes++
			continue
		}
		flushASCIIWord()
		if unicode.IsSpace(r) {
			continue
		}
		tokens++
	}
	flushASCIIWord()
	if tokens <= 0 {
		return 1
	}
	return tokens
}

type CL100KTextMeasurer struct {
	encoder *tiktoken.Tiktoken
}

func (CL100KTextMeasurer) Name() string {
	return "cl100k_base"
}

func (m CL100KTextMeasurer) CountTokens(text string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}
	if m.encoder == nil {
		return ConservativeTextMeasurer{}.CountTokens(text)
	}
	return len(m.encoder.EncodeOrdinary(text))
}

var cl100kTokenizer = struct {
	once sync.Once
	enc  *tiktoken.Tiktoken
	err  error
}{}

func newCL100KTextMeasurer() TextMeasurer {
	cl100kTokenizer.once.Do(func() {
		tiktoken.SetBpeLoader(tiktokenloader.NewOfflineLoader())
		cl100kTokenizer.enc, cl100kTokenizer.err = tiktoken.GetEncoding("cl100k_base")
	})
	if cl100kTokenizer.err != nil || cl100kTokenizer.enc == nil {
		return ConservativeTextMeasurer{}
	}
	return CL100KTextMeasurer{encoder: cl100kTokenizer.enc}
}

func NewTextMeasurer(name string, model string) TextMeasurer {
	measurer, _ := ResolveTextMeasurer(name, model)
	return measurer
}

func ResolveTextMeasurer(name string, model string) (TextMeasurer, TokenizerInfo) {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" || key == "auto" {
		key = tokenizerForModel(model)
	}
	reg, ok := tokenizerRegistry[key]
	if ok && reg.info.Available && reg.factory != nil {
		return reg.factory(model), reg.info
	}
	fallback := tokenizerRegistry["conservative"]
	return fallback.factory(model), fallback.info
}

func AvailableTokenizers() []TokenizerInfo {
	keys := []string{"conservative", "cl100k_base"}
	out := make([]TokenizerInfo, 0, len(keys))
	for _, key := range keys {
		if reg, ok := tokenizerRegistry[key]; ok {
			out = append(out, reg.info)
		}
	}
	return out
}

func tokenizerForModel(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.Contains(model, "text-embedding-3"), strings.Contains(model, "ada-002"):
		return "cl100k_base"
	default:
		return "conservative"
	}
}

var tokenizerRegistry = map[string]tokenizerRegistration{
	"conservative": {
		info: TokenizerInfo{
			Name:        "conservative",
			Available:   true,
			Description: "Pure Go conservative estimator used when model tokenizer is unavailable.",
		},
		factory: func(model string) TextMeasurer { return ConservativeTextMeasurer{} },
	},
	"cl100k_base": {
		info: TokenizerInfo{
			Name:        "cl100k_base",
			Available:   true,
			Description: "Pure Go cl100k_base tokenizer using an embedded offline BPE vocabulary; falls back to conservative if initialization fails.",
		},
		factory: func(model string) TextMeasurer { return newCL100KTextMeasurer() },
	},
}

type ChunkStrategyInfo struct {
	Name        string `json:"name"`
	Available   bool   `json:"available"`
	Description string `json:"description,omitempty"`
}

type ChunkStrategy interface {
	Name() string
	BuildBodies(content string, bodyBudget int, overlapTokens int, measurer TextMeasurer) []chunkBody
}

type chunkStrategyRegistration struct {
	info     ChunkStrategyInfo
	strategy ChunkStrategy
	aliasFor string
}

type ChunkPlan struct {
	Tokenizer          string `json:"tokenizer,omitempty"`
	Strategy           string `json:"strategy,omitempty"`
	ContextTokens      int    `json:"context_tokens,omitempty"`
	ChunkTargetTokens  int    `json:"chunk_target_tokens,omitempty"`
	ChunkOverlapTokens int    `json:"chunk_overlap_tokens,omitempty"`
}

type ChunkPlanSummary struct {
	Plan        ChunkPlan `json:"plan"`
	ChunkCount  int       `json:"chunk_count"`
	TotalTokens int       `json:"total_tokens"`
	MinTokens   int       `json:"min_tokens"`
	MaxTokens   int       `json:"max_tokens"`
	AvgTokens   float64   `json:"avg_tokens"`
}

func AvailableChunkStrategies() []ChunkStrategyInfo {
	keys := []string{"auto", "section_whole", "block_aware_pack", "structural", "recursive", "adaptive"}
	out := make([]ChunkStrategyInfo, 0, len(keys))
	for _, key := range keys {
		if reg, ok := chunkStrategyRegistry[key]; ok {
			out = append(out, reg.info)
		}
	}
	return out
}

func ResolveChunkStrategy(name string) (ChunkStrategy, ChunkStrategyInfo, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		key = "auto"
	}
	reg, ok := chunkStrategyRegistry[key]
	if !ok {
		return nil, ChunkStrategyInfo{}, fmt.Errorf("unsupported embedding chunk strategy %q", name)
	}
	if reg.aliasFor != "" {
		reg = chunkStrategyRegistry[reg.aliasFor]
	}
	if !reg.info.Available || reg.strategy == nil {
		return nil, reg.info, fmt.Errorf("embedding chunk strategy %q is not available", key)
	}
	return reg.strategy, reg.info, nil
}

type structuralStrategy struct{}

func (structuralStrategy) Name() string { return "structural" }

func (structuralStrategy) BuildBodies(content string, bodyBudget int, overlapTokens int, measurer TextMeasurer) []chunkBody {
	return packUnits(splitContentUnits(content), bodyBudget, overlapTokens, measurer)
}

type autoStrategy struct{}

func (autoStrategy) Name() string { return "auto" }

func (autoStrategy) BuildBodies(content string, bodyBudget int, overlapTokens int, measurer TextMeasurer) []chunkBody {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	if measurer.CountTokens(content) <= bodyBudget {
		return []chunkBody{{Text: content, StartToken: 0, TokenCount: measurer.CountTokens(content)}}
	}
	return blockAwarePackStrategy{}.BuildBodies(content, bodyBudget, 0, measurer)
}

type sectionWholeStrategy struct{}

func (sectionWholeStrategy) Name() string { return "section_whole" }

func (sectionWholeStrategy) BuildBodies(content string, bodyBudget int, overlapTokens int, measurer TextMeasurer) []chunkBody {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	if measurer.CountTokens(content) <= bodyBudget {
		return []chunkBody{{Text: content, StartToken: 0, TokenCount: measurer.CountTokens(content)}}
	}
	return structuralStrategy{}.BuildBodies(content, bodyBudget, overlapTokens, measurer)
}

type blockAwarePackStrategy struct{}

func (blockAwarePackStrategy) Name() string { return "block_aware_pack" }

func (blockAwarePackStrategy) BuildBodies(content string, bodyBudget int, overlapTokens int, measurer TextMeasurer) []chunkBody {
	return packBlockAwareUnits(splitContentBlockAwareUnits(content), bodyBudget, overlapTokens, measurer)
}

type adaptiveStrategy struct{}

func (adaptiveStrategy) Name() string { return "adaptive" }

func (adaptiveStrategy) BuildBodies(content string, bodyBudget int, overlapTokens int, measurer TextMeasurer) []chunkBody {
	units := splitContentUnits(content)
	if len(units) == 0 {
		return nil
	}
	candidates := adaptiveCandidates(bodyBudget, overlapTokens)
	var best []chunkBody
	bestScore := math.Inf(1)
	for _, candidate := range candidates {
		bodies := packUnits(units, candidate.budget, candidate.overlap, measurer)
		if len(bodies) == 0 {
			continue
		}
		score := scoreChunkPlan(bodies, content, candidate.budget, candidate.overlap, measurer)
		if score < bestScore {
			bestScore = score
			best = bodies
		}
	}
	if len(best) == 0 {
		return packUnits(units, bodyBudget, overlapTokens, measurer)
	}
	return best
}

type adaptiveCandidate struct {
	budget  int
	overlap int
}

func adaptiveCandidates(bodyBudget int, overlapTokens int) []adaptiveCandidate {
	if bodyBudget <= 0 {
		bodyBudget = 64
	}
	baseOverlap := overlapTokens
	if baseOverlap < 0 {
		baseOverlap = 0
	}
	budgets := []int{bodyBudget * 3 / 4, bodyBudget, bodyBudget * 5 / 4}
	overlaps := []int{baseOverlap / 2, baseOverlap, baseOverlap * 2}
	if baseOverlap == 0 {
		overlaps = []int{0, maxInt(1, bodyBudget/20)}
	}
	seen := map[string]bool{}
	out := make([]adaptiveCandidate, 0, len(budgets)*len(overlaps))
	for _, budget := range budgets {
		if budget < 32 {
			budget = 32
		}
		for _, overlap := range overlaps {
			maxOverlap := budget / 5
			if overlap > maxOverlap {
				overlap = maxOverlap
			}
			if overlap < 0 {
				overlap = 0
			}
			key := fmt.Sprintf("%d/%d", budget, overlap)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, adaptiveCandidate{budget: budget, overlap: overlap})
		}
	}
	return out
}

func scoreChunkPlan(bodies []chunkBody, content string, budget int, overlap int, measurer TextMeasurer) float64 {
	if len(bodies) == 0 {
		return math.Inf(1)
	}
	contentTokens := maxInt(1, measurer.CountTokens(content))
	totalTokens := 0
	squaredBalance := 0.0
	boundaryPenalty := 0.0
	for _, body := range bodies {
		totalTokens += body.TokenCount
		fill := float64(body.TokenCount) / float64(maxInt(1, budget))
		squaredBalance += math.Pow(1-fill, 2)
		text := strings.TrimSpace(body.Text)
		if text != "" && !strings.Contains(text, "\n\n") && body.TokenCount > budget*4/5 {
			boundaryPenalty += 0.12
		}
	}
	balance := squaredBalance / float64(len(bodies))
	overlapWaste := float64(maxInt(0, totalTokens-contentTokens)) / float64(maxInt(1, totalTokens))
	chunkPenalty := float64(len(bodies)) * 0.01
	overlapPenalty := float64(overlap) / float64(maxInt(1, budget)) * 0.08
	return balance + overlapWaste + boundaryPenalty + chunkPenalty + overlapPenalty
}

var chunkStrategyRegistry = map[string]chunkStrategyRegistration{
	"auto": {
		info:     ChunkStrategyInfo{Name: "auto", Available: true, Description: "Default DocGraph policy: keeps fitting H2 sections whole, otherwise uses block-aware section-internal packing."},
		strategy: autoStrategy{},
	},
	"section_whole": {
		info:     ChunkStrategyInfo{Name: "section_whole", Available: true, Description: "Keeps an H2 section as one chunk when it fits the token budget; falls back to structural splitting for oversized sections."},
		strategy: sectionWholeStrategy{},
	},
	"structural": {
		info:     ChunkStrategyInfo{Name: "structural", Available: true, Description: "Metadata-aware structural splitter using paragraphs, lines, token budgets, and overlap."},
		strategy: structuralStrategy{},
	},
	"block_aware_pack": {
		info:     ChunkStrategyInfo{Name: "block_aware_pack", Available: true, Description: "Section-scoped packer that preserves Markdown/HTML tables, fenced code, lists, blockquotes, admonitions, and image captions when they fit the token budget."},
		strategy: blockAwarePackStrategy{},
	},
	"recursive": {
		info:     ChunkStrategyInfo{Name: "recursive", Available: true, Description: "Alias for structural splitter for compatibility with common RAG terminology."},
		aliasFor: "structural",
	},
	"adaptive": {
		info:     ChunkStrategyInfo{Name: "adaptive", Available: true, Description: "Deterministic adaptive splitter that compares multiple token budgets and overlap plans using intrinsic chunk metrics."},
		strategy: adaptiveStrategy{},
	},
}

type Chunker struct {
	Measurer           TextMeasurer
	Strategy           string
	ContextTokens      int
	ChunkTargetTokens  int
	ChunkOverlapTokens int
	MetadataReserve    int
}

type EmbeddingChunk struct {
	ChunkID            string
	SectionID          string
	DocumentID         string
	SourceID           string
	ChunkOrdinal       int
	ChunkStartToken    int
	ChunkTokenCount    int
	Text               string
	TextHash           string
	SectionContentHash string
	Tokenizer          string
	ChunkStrategy      string
}

func (c Chunker) Normalized() Chunker {
	if c.Measurer == nil {
		c.Measurer = ConservativeTextMeasurer{}
	}
	if c.MetadataReserve < 0 {
		c.MetadataReserve = 0
	}
	if c.ChunkTargetTokens <= 0 {
		if c.ContextTokens > 0 {
			c.ChunkTargetTokens = c.ContextTokens * 3 / 5
		} else {
			c.ChunkTargetTokens = 512
		}
	}
	if c.ContextTokens > 0 && c.ChunkTargetTokens > c.ContextTokens-c.MetadataReserve-16 {
		c.ChunkTargetTokens = c.ContextTokens - c.MetadataReserve - 16
	}
	if c.ChunkTargetTokens < 64 {
		c.ChunkTargetTokens = 64
	}
	if c.ChunkOverlapTokens < 0 {
		c.ChunkOverlapTokens = 0
	}
	maxOverlap := c.ChunkTargetTokens / 5
	if c.ChunkOverlapTokens == 0 {
		c.ChunkOverlapTokens = maxOverlap / 2
	}
	if c.ChunkOverlapTokens > maxOverlap {
		c.ChunkOverlapTokens = maxOverlap
	}
	return c
}

func (c Chunker) BuildChunks(section domain.EmbeddingSection, model string, generatorVersion string) ([]EmbeddingChunk, error) {
	c = c.Normalized()
	strategy, strategyInfo, err := ResolveChunkStrategy(c.Strategy)
	if err != nil {
		return nil, err
	}
	c.Strategy = strategyInfo.Name
	prefix := metadataPrefix(section)
	prefixTokens := c.Measurer.CountTokens(prefix + "\ncontent:\n")
	bodyBudget := c.ChunkTargetTokens - prefixTokens - 16
	if bodyBudget < 32 {
		bodyBudget = 32
	}
	bodyChunks := strategy.BuildBodies(section.Content, bodyBudget, c.ChunkOverlapTokens, c.Measurer)
	if len(bodyChunks) == 0 {
		bodyChunks = []chunkBody{{Text: strings.TrimSpace(section.Content), StartToken: 0, TokenCount: c.Measurer.CountTokens(section.Content)}}
	}
	out := make([]EmbeddingChunk, 0, len(bodyChunks))
	for i, body := range bodyChunks {
		text := strings.TrimSpace(prefix + "\ncontent:\n" + strings.TrimSpace(body.Text))
		textHash := embeddingtext.HashText(text)
		chunk := EmbeddingChunk{
			SectionID:          section.SectionID,
			DocumentID:         section.DocumentID,
			SourceID:           section.SourceID,
			ChunkOrdinal:       i,
			ChunkStartToken:    body.StartToken,
			ChunkTokenCount:    c.Measurer.CountTokens(text),
			Text:               text,
			TextHash:           textHash,
			SectionContentHash: section.ContentHash,
			Tokenizer:          c.Measurer.Name(),
			ChunkStrategy:      c.Strategy,
		}
		chunk.ChunkID = BuildChunkIDForPlan(section.SectionID, i, model, generatorVersion, chunk.Tokenizer, chunk.ChunkStrategy, textHash)
		out = append(out, chunk)
	}
	return out, nil
}

func CompareChunkPlans(section domain.EmbeddingSection, model string, generatorVersion string, plans []ChunkPlan) ([]ChunkPlanSummary, error) {
	if len(plans) == 0 {
		plans = []ChunkPlan{{Tokenizer: "auto", Strategy: "auto"}}
	}
	out := make([]ChunkPlanSummary, 0, len(plans))
	for _, plan := range plans {
		measurer := NewTextMeasurer(plan.Tokenizer, model)
		chunks, err := Chunker{
			Measurer:           measurer,
			Strategy:           plan.Strategy,
			ContextTokens:      plan.ContextTokens,
			ChunkTargetTokens:  plan.ChunkTargetTokens,
			ChunkOverlapTokens: plan.ChunkOverlapTokens,
		}.BuildChunks(section, model, generatorVersion)
		if err != nil {
			return nil, err
		}
		summary := ChunkPlanSummary{Plan: plan, ChunkCount: len(chunks)}
		for i, chunk := range chunks {
			tokens := chunk.ChunkTokenCount
			summary.TotalTokens += tokens
			if i == 0 || tokens < summary.MinTokens {
				summary.MinTokens = tokens
			}
			if tokens > summary.MaxTokens {
				summary.MaxTokens = tokens
			}
		}
		if summary.ChunkCount > 0 {
			summary.AvgTokens = float64(summary.TotalTokens) / float64(summary.ChunkCount)
		}
		out = append(out, summary)
	}
	return out, nil
}

func BuildChunkID(sectionID string, ordinal int, model string, generatorVersion string, textHash string) string {
	return BuildChunkIDForPlan(sectionID, ordinal, model, generatorVersion, "", "", textHash)
}

func BuildChunkIDForPlan(sectionID string, ordinal int, model string, generatorVersion string, tokenizer string, chunkStrategy string, textHash string) string {
	value := fmt.Sprintf("%s\n%d\n%s\n%s\n%s\n%s\n%s", strings.TrimSpace(sectionID), ordinal, strings.TrimSpace(model), strings.TrimSpace(generatorVersion), strings.TrimSpace(tokenizer), strings.TrimSpace(chunkStrategy), strings.TrimSpace(textHash))
	sum := sha256.Sum256([]byte(value))
	return "embchk_" + hex.EncodeToString(sum[:16])
}

func metadataPrefix(section domain.EmbeddingSection) string {
	parts := []string{
		"document: " + section.DocumentTitle,
		"source: " + section.SourceName,
	}
	if hint := strings.TrimSpace(strings.Join(nonEmpty([]string{section.ProductHint, section.ModuleHint}), " / ")); hint != "" {
		parts = append(parts, "hint: "+hint)
	}
	if strings.TrimSpace(section.HeadingPath) != "" {
		parts = append(parts, "heading_path: "+section.HeadingPath)
	}
	if strings.TrimSpace(section.Title) != "" {
		parts = append(parts, "section: "+section.Title)
	}
	return strings.Join(parts, "\n")
}

type chunkBody struct {
	Text       string
	StartToken int
	TokenCount int
}

type blockAwareUnit struct {
	Kind string
	Text string
}

func splitContentUnits(content string) []string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	blocks := strings.Split(content, "\n\n")
	units := make([]string, 0, len(blocks))
	for _, block := range blocks {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		lines := strings.Split(block, "\n")
		if len(lines) > 1 {
			allShort := true
			for _, line := range lines {
				if utf8.RuneCountInString(line) > 240 {
					allShort = false
					break
				}
			}
			if allShort {
				for _, line := range lines {
					if line = strings.TrimSpace(line); line != "" {
						units = append(units, line)
					}
				}
				continue
			}
		}
		units = append(units, block)
	}
	return units
}

func splitContentBlockAwareUnits(content string) []blockAwareUnit {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	lines := strings.Split(content, "\n")
	units := make([]blockAwareUnit, 0)
	flushParagraph := func(paragraph []string) []string {
		text := strings.TrimSpace(strings.Join(paragraph, "\n"))
		if text != "" {
			units = append(units, blockAwareUnit{Kind: "paragraph", Text: text})
		}
		return nil
	}
	var paragraph []string
	addBlock := func(kind string, start int, end int) {
		paragraph = flushParagraph(paragraph)
		if start < 0 {
			start = 0
		}
		if end >= len(lines) {
			end = len(lines) - 1
		}
		if start <= end {
			if text := strings.TrimSpace(strings.Join(lines[start:end+1], "\n")); text != "" {
				units = append(units, blockAwareUnit{Kind: kind, Text: text})
			}
		}
	}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			paragraph = flushParagraph(paragraph)
			continue
		}
		switch {
		case isFenceStartLine(trimmed):
			fence := trimmed[:3]
			end := i
			for j := i + 1; j < len(lines); j++ {
				end = j
				if strings.HasPrefix(strings.TrimSpace(lines[j]), fence) {
					break
				}
			}
			addBlock("fenced_code", i, end)
			i = end
		case strings.Contains(strings.ToLower(trimmed), "<table"):
			end := i
			depth := 0
			for j := i; j < len(lines); j++ {
				lower := strings.ToLower(lines[j])
				depth += strings.Count(lower, "<table")
				depth -= strings.Count(lower, "</table>")
				end = j
				if depth <= 0 && strings.Contains(lower, "</table>") {
					break
				}
			}
			addBlock("html_table", i, end)
			i = end
		case isMarkdownTableStart(lines, i):
			end := i + 1
			for end+1 < len(lines) && isMarkdownTableRow(lines[end+1]) {
				end++
			}
			addBlock("markdown_table", i, end)
			i = end
		case isFigureStartLine(trimmed):
			end := i
			for j := i; j < len(lines); j++ {
				end = j
				if strings.Contains(strings.ToLower(lines[j]), "</figure>") {
					break
				}
			}
			addBlock("image_caption", i, end)
			i = end
		case isImageLine(trimmed):
			end := i
			if i+1 < len(lines) && isCaptionLine(lines[i+1]) {
				end = i + 1
			}
			addBlock("image_caption", i, end)
			i = end
		case isListLine(line):
			end := i
			for end+1 < len(lines) && isListContinuationLine(lines[end+1]) {
				end++
			}
			addBlock("list", i, end)
			i = end
		case isAdmonitionStartLine(trimmed):
			end := i
			for end+1 < len(lines) && strings.TrimSpace(lines[end+1]) != "" {
				end++
				if strings.TrimSpace(lines[end]) == ":::" {
					break
				}
			}
			addBlock("admonition", i, end)
			i = end
		case isBlockquoteLine(line):
			end := i
			for end+1 < len(lines) && isBlockquoteLine(lines[end+1]) {
				end++
			}
			if end > i {
				addBlock("blockquote", i, end)
				i = end
			} else {
				paragraph = append(paragraph, line)
			}
		default:
			paragraph = append(paragraph, line)
		}
	}
	flushParagraph(paragraph)
	return units
}

func packBlockAwareUnits(units []blockAwareUnit, budget int, overlap int, measurer TextMeasurer) []chunkBody {
	out := make([]chunkBody, 0)
	var current []string
	currentTokens := 0
	startToken := 0
	seenTokens := 0
	flush := func() {
		if len(current) == 0 {
			return
		}
		text := strings.Join(current, "\n\n")
		out = append(out, chunkBody{Text: text, StartToken: startToken, TokenCount: measurer.CountTokens(text)})
		if overlap > 0 {
			overlapText := suffixApproxTokens(text, overlap, measurer)
			current = []string{}
			if strings.TrimSpace(overlapText) != "" {
				current = append(current, overlapText)
				currentTokens = measurer.CountTokens(overlapText)
				startToken = seenTokens - currentTokens
				if startToken < 0 {
					startToken = 0
				}
				return
			}
		}
		current = nil
		currentTokens = 0
		startToken = seenTokens
	}
	for _, unit := range units {
		unit.Text = strings.TrimSpace(unit.Text)
		if unit.Text == "" {
			continue
		}
		unitTokens := measurer.CountTokens(unit.Text)
		if unitTokens > budget {
			flush()
			for _, part := range splitOversizedBlockAwareUnit(unit, budget, overlap, measurer, seenTokens) {
				out = append(out, part)
				seenTokens = part.StartToken + part.TokenCount
			}
			current = nil
			currentTokens = 0
			startToken = seenTokens
			continue
		}
		if len(current) > 0 && currentTokens+unitTokens > budget {
			flush()
		}
		if len(current) == 0 {
			startToken = seenTokens
		}
		current = append(current, unit.Text)
		currentTokens += unitTokens
		seenTokens += unitTokens
	}
	flush()
	return out
}

func splitOversizedBlockAwareUnit(unit blockAwareUnit, budget int, overlap int, measurer TextMeasurer, start int) []chunkBody {
	switch unit.Kind {
	case "fenced_code":
		return splitOversizedFencedCode(unit.Text, budget, measurer, start)
	case "markdown_table":
		return splitOversizedMarkdownTable(unit.Text, budget, measurer, start)
	case "html_table":
		return splitOversizedHTMLTable(unit.Text, budget, measurer, start)
	case "image_caption":
		return splitOversizedImageCaption(unit.Text, budget, measurer, start)
	case "list":
		return splitOversizedList(unit.Text, budget, measurer, start)
	case "blockquote":
		return splitOversizedBlockquote(unit.Text, budget, measurer, start)
	case "admonition":
		return splitOversizedAdmonition(unit.Text, budget, measurer, start)
	default:
		return splitLongUnit(unit.Text, budget, overlap, measurer, start)
	}
}

func splitOversizedFencedCode(text string, budget int, measurer TextMeasurer, start int) []chunkBody {
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n")), "\n")
	if len(lines) < 2 || !isFenceStartLine(strings.TrimSpace(lines[0])) {
		return splitLongUnit(text, budget, 0, measurer, start)
	}
	opening := lines[0]
	fence := strings.TrimSpace(opening)[:3]
	closing := fence
	bodyEnd := len(lines)
	if strings.HasPrefix(strings.TrimSpace(lines[len(lines)-1]), fence) {
		closing = lines[len(lines)-1]
		bodyEnd = len(lines) - 1
	}
	bodyLines := lines[1:bodyEnd]
	makePart := func(partLines []string) string {
		items := []string{opening}
		items = append(items, partLines...)
		items = append(items, closing)
		return strings.TrimSpace(strings.Join(items, "\n"))
	}
	groups := groupCodeLinesByBoundary(bodyLines)
	groups = expandOversizedCodeGroups(groups, makePart, budget, measurer)
	return packLineGroupsWithWrapper(groups, makePart, budget, measurer, start)
}

func groupCodeLinesByBoundary(lines []string) []string {
	groups := make([]string, 0, len(lines))
	current := make([]string, 0)
	flush := func() {
		if len(current) == 0 {
			return
		}
		groups = append(groups, strings.TrimSpace(strings.Join(current, "\n")))
		current = nil
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			flush()
			continue
		}
		if len(current) > 0 && (isCodeDeclarationLine(trimmed) || isCodeCommentBoundaryLine(trimmed)) {
			flush()
		}
		current = append(current, line)
	}
	flush()
	if len(groups) == 0 {
		return lines
	}
	return groups
}

func isCodeDeclarationLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "func ") ||
		strings.HasPrefix(lower, "func(") ||
		strings.HasPrefix(lower, "def ") ||
		strings.HasPrefix(lower, "async def ") ||
		strings.HasPrefix(lower, "class ") ||
		strings.HasPrefix(lower, "function ") ||
		strings.HasPrefix(lower, "async function ") ||
		strings.HasPrefix(lower, "export function ") {
		return true
	}
	if strings.Contains(trimmed, "(") && strings.Contains(trimmed, ")") && strings.HasSuffix(trimmed, "{") {
		for _, prefix := range []string{"public ", "private ", "protected ", "static ", "void ", "int ", "string ", "bool "} {
			if strings.HasPrefix(lower, prefix) {
				return true
			}
		}
	}
	return false
}

func isCodeCommentBoundaryLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "//") ||
		strings.HasPrefix(trimmed, "#") ||
		strings.HasPrefix(trimmed, "/*") ||
		strings.HasPrefix(trimmed, "* ") ||
		strings.HasPrefix(trimmed, "<!--")
}

func expandOversizedCodeGroups(groups []string, makePart func([]string) string, budget int, measurer TextMeasurer) []string {
	out := make([]string, 0, len(groups))
	for _, group := range groups {
		if measurer.CountTokens(makePart([]string{group})) <= budget || !strings.Contains(group, "\n") {
			out = append(out, group)
			continue
		}
		for _, line := range strings.Split(group, "\n") {
			if strings.TrimSpace(line) != "" {
				out = append(out, line)
			}
		}
	}
	if len(out) == 0 {
		return groups
	}
	return out
}

func splitOversizedMarkdownTable(text string, budget int, measurer TextMeasurer, start int) []chunkBody {
	lines := nonEmptyLines(text)
	if len(lines) < 3 || !isMarkdownTableSeparator(lines[1]) {
		return splitLongUnit(text, budget, 0, measurer, start)
	}
	header := []string{lines[0], lines[1]}
	rows := lines[2:]
	makePart := func(partRows []string) string {
		items := append([]string{}, header...)
		items = append(items, partRows...)
		return strings.TrimSpace(strings.Join(items, "\n"))
	}
	return packLineGroupsWithWrapper(rows, makePart, budget, measurer, start)
}

func splitOversizedHTMLTable(text string, budget int, measurer TextMeasurer, start int) []chunkBody {
	lines := nonEmptyLines(text)
	if len(lines) == 0 {
		return nil
	}
	dataRows := htmlTableDataRows(lines)
	if len(dataRows) == 0 {
		return splitLongUnit(text, budget, 0, measurer, start)
	}
	prefix := append([]string{}, lines[:dataRows[0].start]...)
	rows := make([]string, 0, len(dataRows))
	for _, row := range dataRows {
		rows = append(rows, row.text)
	}
	suffix := append([]string{}, lines[dataRows[len(dataRows)-1].end+1:]...)
	makePart := func(partRows []string) string {
		items := append([]string{}, prefix...)
		items = append(items, partRows...)
		items = append(items, suffix...)
		return strings.TrimSpace(strings.Join(items, "\n"))
	}
	return packLineGroupsWithWrapper(rows, makePart, budget, measurer, start)
}

type htmlTableRow struct {
	start int
	end   int
	text  string
}

func htmlTableDataRows(lines []string) []htmlTableRow {
	rows := make([]htmlTableRow, 0)
	for i := 0; i < len(lines); i++ {
		lower := strings.ToLower(lines[i])
		if !strings.Contains(lower, "<tr") {
			continue
		}
		start := i
		end := i
		rowLines := []string{lines[i]}
		for !strings.Contains(strings.ToLower(lines[end]), "</tr>") && end+1 < len(lines) {
			end++
			rowLines = append(rowLines, lines[end])
		}
		text := strings.Join(rowLines, "\n")
		if strings.Contains(strings.ToLower(text), "<td") {
			rows = append(rows, htmlTableRow{start: start, end: end, text: text})
		}
		i = end
	}
	return rows
}

func splitOversizedImageCaption(text string, budget int, measurer TextMeasurer, start int) []chunkBody {
	lines := nonEmptyLines(text)
	if len(lines) <= 1 {
		return splitLongUnit(text, budget, 0, measurer, start)
	}
	prefix := []string{lines[0]}
	suffix := []string{}
	bodyEnd := len(lines)
	if strings.Contains(strings.ToLower(lines[0]), "<figure") && strings.Contains(strings.ToLower(lines[len(lines)-1]), "</figure>") {
		suffix = []string{lines[len(lines)-1]}
		bodyEnd = len(lines) - 1
	}
	body := append([]string{}, lines[1:bodyEnd]...)
	makePart := func(partLines []string) string {
		items := append([]string{}, prefix...)
		items = append(items, partLines...)
		items = append(items, suffix...)
		return strings.TrimSpace(strings.Join(items, "\n"))
	}
	return packLineGroupsWithWrapper(body, makePart, budget, measurer, start)
}

func splitOversizedList(text string, budget int, measurer TextMeasurer, start int) []chunkBody {
	items := splitListItems(nonEmptyLines(text))
	if len(items) == 0 {
		return splitLongUnit(text, budget, 0, measurer, start)
	}
	expanded := make([]string, 0, len(items))
	for _, item := range items {
		if measurer.CountTokens(item) <= budget {
			expanded = append(expanded, item)
			continue
		}
		expanded = append(expanded, splitOversizedListItem(item, budget, measurer)...)
	}
	makePart := func(partItems []string) string {
		return strings.TrimSpace(strings.Join(partItems, "\n"))
	}
	return packLineGroupsWithWrapper(expanded, makePart, budget, measurer, start)
}

func splitListItems(lines []string) []string {
	items := make([]string, 0)
	current := make([]string, 0)
	flush := func() {
		if len(current) == 0 {
			return
		}
		items = append(items, strings.TrimSpace(strings.Join(current, "\n")))
		current = nil
	}
	for _, line := range lines {
		if isListLine(line) {
			flush()
			current = append(current, line)
			continue
		}
		if len(current) == 0 {
			current = append(current, line)
			continue
		}
		current = append(current, line)
	}
	flush()
	return items
}

func splitOversizedListItem(item string, budget int, measurer TextMeasurer) []string {
	lines := strings.Split(strings.TrimSpace(item), "\n")
	if len(lines) == 0 {
		return nil
	}
	marker, content := listMarkerAndContent(lines[0])
	if marker == "" {
		return []string{item}
	}
	if len(lines) > 1 {
		content = strings.TrimSpace(content + "\n" + strings.Join(lines[1:], "\n"))
	}
	makeLine := func(parts []string) string {
		return strings.TrimSpace(marker + strings.Join(parts, "\n"))
	}
	out := make([]string, 0)
	for _, part := range splitWrappedLine(content, makeLine, budget, measurer) {
		out = append(out, makeLine([]string{part}))
	}
	return out
}

func listMarkerAndContent(line string) (string, string) {
	trimmed := strings.TrimSpace(line)
	for _, marker := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(trimmed, marker) {
			return marker, strings.TrimSpace(strings.TrimPrefix(trimmed, marker))
		}
	}
	for _, sep := range []string{". ", ") "} {
		idx := strings.Index(trimmed, sep)
		if idx <= 0 || idx > 3 {
			continue
		}
		allDigits := true
		for _, r := range trimmed[:idx] {
			if !unicode.IsDigit(r) {
				allDigits = false
				break
			}
		}
		if allDigits {
			marker := trimmed[:idx+len(sep)]
			return marker, strings.TrimSpace(strings.TrimPrefix(trimmed, marker))
		}
	}
	return "", trimmed
}

func splitOversizedBlockquote(text string, budget int, measurer TextMeasurer, start int) []chunkBody {
	lines := nonEmptyLines(text)
	if len(lines) == 0 {
		return nil
	}
	expanded := make([]string, 0, len(lines))
	for _, line := range lines {
		if measurer.CountTokens(line) <= budget {
			expanded = append(expanded, line)
			continue
		}
		marker, content := quoteMarkerAndContent(line)
		makeLine := func(parts []string) string {
			return strings.TrimSpace(marker + strings.Join(parts, "\n"))
		}
		for _, part := range splitWrappedLine(content, makeLine, budget, measurer) {
			expanded = append(expanded, makeLine([]string{part}))
		}
	}
	makePart := func(partLines []string) string {
		return strings.TrimSpace(strings.Join(partLines, "\n"))
	}
	return packLineGroupsWithWrapper(expanded, makePart, budget, measurer, start)
}

func quoteMarkerAndContent(line string) (string, string) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, ">") {
		return "", trimmed
	}
	content := strings.TrimSpace(strings.TrimPrefix(trimmed, ">"))
	return "> ", content
}

func splitOversizedAdmonition(text string, budget int, measurer TextMeasurer, start int) []chunkBody {
	lines := nonEmptyLines(text)
	if len(lines) <= 1 {
		return splitLongUnit(text, budget, 0, measurer, start)
	}
	opening := lines[0]
	closing := ""
	bodyEnd := len(lines)
	if strings.HasPrefix(strings.TrimSpace(opening), ":::") && strings.TrimSpace(lines[len(lines)-1]) == ":::" {
		closing = lines[len(lines)-1]
		bodyEnd = len(lines) - 1
	}
	body := append([]string{}, lines[1:bodyEnd]...)
	makePart := func(partLines []string) string {
		items := []string{opening}
		items = append(items, partLines...)
		if closing != "" {
			items = append(items, closing)
		}
		return strings.TrimSpace(strings.Join(items, "\n"))
	}
	return packLineGroupsWithWrapper(body, makePart, budget, measurer, start)
}

func packLineGroupsWithWrapper(lines []string, makePart func([]string) string, budget int, measurer TextMeasurer, start int) []chunkBody {
	if budget <= 0 {
		budget = 64
	}
	out := make([]chunkBody, 0)
	current := make([]string, 0)
	tokenStart := start
	flush := func() {
		if len(current) == 0 {
			return
		}
		text := makePart(current)
		tokens := measurer.CountTokens(text)
		out = append(out, chunkBody{Text: text, StartToken: tokenStart, TokenCount: tokens})
		tokenStart += tokens
		current = nil
	}
	for _, line := range lines {
		line = strings.TrimRight(line, "\n")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if measurer.CountTokens(makePart([]string{line})) > budget {
			flush()
			for _, part := range splitWrappedLine(line, makePart, budget, measurer) {
				text := makePart([]string{part})
				tokens := measurer.CountTokens(text)
				out = append(out, chunkBody{Text: text, StartToken: tokenStart, TokenCount: tokens})
				tokenStart += tokens
			}
			continue
		}
		candidate := append(append([]string{}, current...), line)
		if len(current) > 0 && measurer.CountTokens(makePart(candidate)) > budget {
			flush()
		}
		current = append(current, line)
		if len(current) == 1 && measurer.CountTokens(makePart(current)) > budget {
			flush()
		}
	}
	flush()
	if len(out) == 0 {
		text := makePart(nil)
		if strings.TrimSpace(text) != "" {
			out = append(out, chunkBody{Text: text, StartToken: start, TokenCount: measurer.CountTokens(text)})
		}
	}
	return out
}

func splitWrappedLine(line string, makePart func([]string) string, budget int, measurer TextMeasurer) []string {
	if budget <= 0 {
		budget = 64
	}
	runes := []rune(line)
	out := make([]string, 0)
	for i := 0; i < len(runes); {
		lastGood := i
		for j := i; j < len(runes); j++ {
			candidate := string(runes[i : j+1])
			if measurer.CountTokens(makePart([]string{candidate})) > budget {
				break
			}
			lastGood = j + 1
		}
		if lastGood <= i {
			lastGood = i + 1
		}
		part := strings.TrimSpace(string(runes[i:lastGood]))
		if part != "" {
			out = append(out, part)
		}
		i = lastGood
	}
	if len(out) == 0 && strings.TrimSpace(line) != "" {
		out = append(out, strings.TrimSpace(line))
	}
	return out
}

func nonEmptyLines(text string) []string {
	raw := strings.Split(strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n")), "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func packUnits(units []string, budget int, overlap int, measurer TextMeasurer) []chunkBody {
	out := make([]chunkBody, 0)
	var current []string
	currentTokens := 0
	startToken := 0
	seenTokens := 0
	flush := func() {
		if len(current) == 0 {
			return
		}
		text := strings.Join(current, "\n\n")
		out = append(out, chunkBody{Text: text, StartToken: startToken, TokenCount: measurer.CountTokens(text)})
		if overlap > 0 {
			overlapText := suffixApproxTokens(text, overlap, measurer)
			current = []string{}
			if strings.TrimSpace(overlapText) != "" {
				current = append(current, overlapText)
				currentTokens = measurer.CountTokens(overlapText)
				startToken = seenTokens - currentTokens
				if startToken < 0 {
					startToken = 0
				}
				return
			}
		}
		current = nil
		currentTokens = 0
		startToken = seenTokens
	}
	for _, unit := range units {
		unit = strings.TrimSpace(unit)
		if unit == "" {
			continue
		}
		unitTokens := measurer.CountTokens(unit)
		if unitTokens > budget {
			flush()
			for _, part := range splitLongUnit(unit, budget, overlap, measurer, seenTokens) {
				out = append(out, part)
				seenTokens = part.StartToken + part.TokenCount
			}
			startToken = seenTokens
			continue
		}
		if len(current) > 0 && currentTokens+unitTokens > budget {
			flush()
		}
		if len(current) == 0 {
			startToken = seenTokens
		}
		current = append(current, unit)
		currentTokens += unitTokens
		seenTokens += unitTokens
	}
	flush()
	return out
}

func splitLongUnit(unit string, budget int, overlap int, measurer TextMeasurer, start int) []chunkBody {
	runes := []rune(unit)
	out := make([]chunkBody, 0)
	if budget <= 0 {
		budget = 64
	}
	i := 0
	tokenStart := start
	for i < len(runes) {
		j := i
		lastGood := i
		for j < len(runes) {
			candidate := string(runes[i : j+1])
			if measurer.CountTokens(candidate) > budget {
				break
			}
			lastGood = j + 1
			j++
		}
		if lastGood <= i {
			lastGood = i + 1
		}
		text := strings.TrimSpace(string(runes[i:lastGood]))
		tokens := measurer.CountTokens(text)
		out = append(out, chunkBody{Text: text, StartToken: tokenStart, TokenCount: tokens})
		if overlap > 0 && lastGood < len(runes) {
			overlapText := suffixApproxTokens(text, overlap, measurer)
			i = lastGood - utf8.RuneCountInString(overlapText)
			if i < 0 || i >= lastGood {
				i = lastGood
			}
			tokenStart += tokens - measurer.CountTokens(overlapText)
			if tokenStart < start {
				tokenStart = start
			}
		} else {
			i = lastGood
			tokenStart += tokens
		}
	}
	return out
}

func suffixApproxTokens(text string, maxTokens int, measurer TextMeasurer) string {
	if maxTokens <= 0 {
		return ""
	}
	runes := []rune(text)
	for i := len(runes); i >= 0; i-- {
		suffix := strings.TrimSpace(string(runes[i:]))
		if suffix == "" {
			continue
		}
		if measurer.CountTokens(suffix) > maxTokens {
			if i+1 < len(runes) {
				return strings.TrimSpace(string(runes[i+1:]))
			}
			return ""
		}
	}
	return strings.TrimSpace(text)
}

func isFenceStartLine(line string) bool {
	return strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~")
}

func isMarkdownTableStart(lines []string, i int) bool {
	return i+1 < len(lines) && isMarkdownTableRow(lines[i]) && isMarkdownTableSeparator(lines[i+1])
}

func isMarkdownTableRow(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.Contains(trimmed, "|") && strings.Count(trimmed, "|") >= 2
}

func isMarkdownTableSeparator(line string) bool {
	trimmed := strings.TrimSpace(line)
	if !isMarkdownTableRow(trimmed) {
		return false
	}
	for _, r := range trimmed {
		switch r {
		case '|', '-', ':', ' ':
		default:
			return false
		}
	}
	return strings.Contains(trimmed, "-")
}

func isFigureStartLine(line string) bool {
	return strings.Contains(strings.ToLower(line), "<figure")
}

func isImageLine(line string) bool {
	trimmed := strings.TrimSpace(strings.ToLower(line))
	return strings.HasPrefix(trimmed, "![") || strings.Contains(trimmed, "<img ")
}

func isCaptionLine(line string) bool {
	trimmed := strings.TrimSpace(strings.ToLower(line))
	return strings.HasPrefix(trimmed, "caption:") ||
		strings.HasPrefix(trimmed, "figure:") ||
		strings.HasPrefix(trimmed, "图片") ||
		strings.HasPrefix(trimmed, "图:") ||
		strings.HasPrefix(trimmed, "图 ") ||
		strings.Contains(trimmed, "<figcaption")
}

func isListLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") || strings.HasPrefix(trimmed, "+ ") {
		return true
	}
	for _, sep := range []string{". ", ") "} {
		idx := strings.Index(trimmed, sep)
		if idx <= 0 || idx > 3 {
			continue
		}
		allDigits := true
		for _, r := range trimmed[:idx] {
			if !unicode.IsDigit(r) {
				allDigits = false
				break
			}
		}
		if allDigits {
			return true
		}
	}
	return false
}

func isListContinuationLine(line string) bool {
	return strings.TrimSpace(line) == "" || isListLine(line) || strings.HasPrefix(line, "  ")
}

func isBlockquoteLine(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), ">")
}

func isAdmonitionStartLine(line string) bool {
	trimmed := strings.TrimSpace(strings.ToLower(line))
	return strings.HasPrefix(trimmed, "!!! ") ||
		strings.HasPrefix(trimmed, "??? ") ||
		strings.HasPrefix(trimmed, ":::") ||
		strings.HasPrefix(trimmed, "> [!note]") ||
		strings.HasPrefix(trimmed, "> [!warning]") ||
		strings.HasPrefix(trimmed, "> [!tip]")
}

func nonEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func maxInt(a int, b int) int {
	if a > b {
		return a
	}
	return b
}
