package embeddingeval

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/docgraph/docgraph/internal/domain"
	"github.com/docgraph/docgraph/internal/embeddingchunk"
)

type Options struct {
	TargetTokens   int
	MinChunkTokens int
}

type Report struct {
	SectionID          string                 `json:"section_id"`
	ChunkCount         int                    `json:"chunk_count"`
	TotalChunkTokens   int                    `json:"total_chunk_tokens"`
	UnmappedChunkCount int                    `json:"unmapped_chunk_count,omitempty"`
	SizeCompliance     SizeComplianceSummary  `json:"size_compliance"`
	BlockIntegrity     BlockIntegritySummary  `json:"block_integrity"`
	Violations         []BlockIntegrityIssue  `json:"violations,omitempty"`
	ChunkSpans         []ChunkSpan            `json:"chunk_spans,omitempty"`
	ProtectedBlocks    []ProtectedBlockReport `json:"protected_blocks,omitempty"`
}

type PlanReport struct {
	embeddingchunk.ChunkPlanSummary
	SizeCompliance     SizeComplianceSummary `json:"size_compliance"`
	BlockIntegrity     BlockIntegritySummary `json:"block_integrity"`
	Violations         []BlockIntegrityIssue `json:"violations,omitempty"`
	UnmappedChunkCount int                   `json:"unmapped_chunk_count,omitempty"`
}

type SizeComplianceSummary struct {
	MinAcceptableTokens int     `json:"min_acceptable_tokens,omitempty"`
	MaxAcceptableTokens int     `json:"max_acceptable_tokens,omitempty"`
	TooSmallChunks      int     `json:"too_small_chunks,omitempty"`
	TooLargeChunks      int     `json:"too_large_chunks,omitempty"`
	MaxTokens           int     `json:"max_tokens,omitempty"`
	AvgTokens           float64 `json:"avg_tokens,omitempty"`
}

type BlockIntegritySummary struct {
	TotalBlocks     int                         `json:"total_blocks"`
	PreservedBlocks int                         `json:"preserved_blocks"`
	SplitBlocks     int                         `json:"split_blocks"`
	PreserveRate    float64                     `json:"preserve_rate"`
	ByKind          map[string]BlockKindSummary `json:"by_kind,omitempty"`
}

type BlockKindSummary struct {
	Total        int     `json:"total"`
	Preserved    int     `json:"preserved"`
	Split        int     `json:"split"`
	PreserveRate float64 `json:"preserve_rate"`
}

type BlockIntegrityIssue struct {
	Kind      string `json:"kind"`
	Reason    string `json:"reason"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Message   string `json:"message"`
}

type ChunkSpan struct {
	ChunkOrdinal int  `json:"chunk_ordinal"`
	Start        int  `json:"start"`
	End          int  `json:"end"`
	Mapped       bool `json:"mapped"`
}

type ProtectedBlockReport struct {
	Kind      string `json:"kind"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Preserved bool   `json:"preserved"`
}

type protectedBlock struct {
	kind      string
	text      string
	start     int
	end       int
	startLine int
	endLine   int
}

func EvaluateSectionChunks(section domain.EmbeddingSection, chunks []embeddingchunk.EmbeddingChunk, opts Options) Report {
	content := normalizeNewlines(section.Content)
	spans, unmapped := mapChunkSpans(content, chunks)
	blocks := protectedBlocks(content)
	report := Report{
		SectionID:          section.SectionID,
		ChunkCount:         len(chunks),
		UnmappedChunkCount: unmapped,
		ChunkSpans:         spans,
		SizeCompliance:     evaluateSizeCompliance(chunks, opts),
	}
	for _, chunk := range chunks {
		report.TotalChunkTokens += chunk.ChunkTokenCount
	}
	chunkBodies := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		chunkBodies = append(chunkBodies, normalizeForContainment(chunkBodyText(chunk.Text)))
	}
	report.BlockIntegrity = evaluateBlockIntegrity(blocks, spans, chunkBodies, &report)
	return report
}

func CompareChunkPlans(section domain.EmbeddingSection, model string, generatorVersion string, plans []embeddingchunk.ChunkPlan) ([]PlanReport, error) {
	if len(plans) == 0 {
		plans = []embeddingchunk.ChunkPlan{{Tokenizer: "auto", Strategy: "auto"}}
	}
	out := make([]PlanReport, 0, len(plans))
	for _, plan := range plans {
		measurer := embeddingchunk.NewTextMeasurer(plan.Tokenizer, model)
		chunker := embeddingchunk.Chunker{
			Measurer:           measurer,
			Strategy:           plan.Strategy,
			ContextTokens:      plan.ContextTokens,
			ChunkTargetTokens:  plan.ChunkTargetTokens,
			ChunkOverlapTokens: plan.ChunkOverlapTokens,
		}.Normalized()
		chunks, err := chunker.BuildChunks(section, model, generatorVersion)
		if err != nil {
			return nil, err
		}
		summary := embeddingchunk.ChunkPlanSummary{Plan: plan, ChunkCount: len(chunks)}
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
		eval := EvaluateSectionChunks(section, chunks, Options{TargetTokens: chunker.ChunkTargetTokens})
		out = append(out, PlanReport{
			ChunkPlanSummary:   summary,
			SizeCompliance:     eval.SizeCompliance,
			BlockIntegrity:     eval.BlockIntegrity,
			Violations:         eval.Violations,
			UnmappedChunkCount: eval.UnmappedChunkCount,
		})
	}
	return out, nil
}

func evaluateSizeCompliance(chunks []embeddingchunk.EmbeddingChunk, opts Options) SizeComplianceSummary {
	if len(chunks) == 0 {
		return SizeComplianceSummary{}
	}
	minTokens := opts.MinChunkTokens
	if minTokens <= 0 && opts.TargetTokens > 0 {
		minTokens = maxInt(32, opts.TargetTokens/5)
	}
	summary := SizeComplianceSummary{
		MinAcceptableTokens: minTokens,
		MaxAcceptableTokens: opts.TargetTokens,
	}
	for i, chunk := range chunks {
		summary.AvgTokens += float64(chunk.ChunkTokenCount)
		if chunk.ChunkTokenCount > summary.MaxTokens {
			summary.MaxTokens = chunk.ChunkTokenCount
		}
		if opts.TargetTokens > 0 && chunk.ChunkTokenCount > opts.TargetTokens {
			summary.TooLargeChunks++
		}
		if minTokens > 0 && i < len(chunks)-1 && chunk.ChunkTokenCount < minTokens {
			summary.TooSmallChunks++
		}
	}
	summary.AvgTokens = summary.AvgTokens / float64(len(chunks))
	return summary
}

func evaluateBlockIntegrity(blocks []protectedBlock, spans []ChunkSpan, chunkBodies []string, report *Report) BlockIntegritySummary {
	summary := BlockIntegritySummary{ByKind: map[string]BlockKindSummary{}}
	for _, block := range blocks {
		preserved := false
		for _, span := range spans {
			if span.Mapped && span.Start <= block.start && span.End >= block.end {
				preserved = true
				break
			}
		}
		if !preserved {
			blockText := normalizeForContainment(block.text)
			for _, body := range chunkBodies {
				if blockText != "" && strings.Contains(body, blockText) {
					preserved = true
					break
				}
			}
		}
		if !preserved && blockAwareSplitPreserved(block, chunkBodies) {
			preserved = true
		}
		summary.TotalBlocks++
		kind := summary.ByKind[block.kind]
		kind.Total++
		if preserved {
			summary.PreservedBlocks++
			kind.Preserved++
		} else {
			summary.SplitBlocks++
			kind.Split++
			report.Violations = append(report.Violations, BlockIntegrityIssue{
				Kind:      block.kind,
				Reason:    "block_not_fully_contained_in_any_chunk",
				StartLine: block.startLine,
				EndLine:   block.endLine,
				Message:   fmt.Sprintf("%s block is not fully contained in any single chunk", block.kind),
			})
		}
		if kind.Total > 0 {
			kind.PreserveRate = float64(kind.Preserved) / float64(kind.Total)
		}
		summary.ByKind[block.kind] = kind
		report.ProtectedBlocks = append(report.ProtectedBlocks, ProtectedBlockReport{
			Kind:      block.kind,
			StartLine: block.startLine,
			EndLine:   block.endLine,
			Preserved: preserved,
		})
	}
	if summary.TotalBlocks > 0 {
		summary.PreserveRate = float64(summary.PreservedBlocks) / float64(summary.TotalBlocks)
	} else {
		summary.ByKind = nil
	}
	return summary
}

func blockAwareSplitPreserved(block protectedBlock, chunkBodies []string) bool {
	switch block.kind {
	case "fenced_code":
		return fencedCodeSplitPreserved(block.text, chunkBodies)
	case "markdown_table":
		return markdownTableSplitPreserved(block.text, chunkBodies)
	case "html_table":
		return htmlTableSplitPreserved(block.text, chunkBodies)
	case "image_caption":
		return contextBlockSplitPreserved(block.text, chunkBodies)
	case "list":
		return listSplitPreserved(block.text, chunkBodies)
	case "blockquote":
		return contextBlockSplitPreserved(block.text, chunkBodies)
	case "admonition":
		return admonitionSplitPreserved(block.text, chunkBodies)
	default:
		return false
	}
}

func fencedCodeSplitPreserved(text string, chunkBodies []string) bool {
	lines := nonEmptyLines(text)
	if len(lines) < 2 || !isFenceStart(lines[0]) {
		return false
	}
	opening := strings.TrimSpace(lines[0])
	closing := "```"
	bodyEnd := len(lines)
	if isFenceStart(lines[len(lines)-1]) {
		closing = strings.TrimSpace(lines[len(lines)-1])
		bodyEnd = len(lines) - 1
	}
	bodyLines := lines[1:bodyEnd]
	if len(bodyLines) == 0 {
		return false
	}
	relevant := chunkBodiesWithAll(chunkBodies, []string{opening, closing})
	if len(relevant) == 0 {
		return false
	}
	for _, line := range bodyLines {
		if !anyBodyContains(relevant, strings.TrimSpace(line)) {
			return false
		}
	}
	return true
}

func markdownTableSplitPreserved(text string, chunkBodies []string) bool {
	lines := nonEmptyLines(text)
	if len(lines) < 3 || !isMarkdownTableSeparator(lines[1]) {
		return false
	}
	header := []string{strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1])}
	relevant := chunkBodiesWithAll(chunkBodies, header)
	if len(relevant) == 0 {
		return false
	}
	for _, row := range lines[2:] {
		if !anyBodyContains(relevant, strings.TrimSpace(row)) && !longLineFragmentsPreserved(row, relevant) {
			return false
		}
	}
	return true
}

func htmlTableSplitPreserved(text string, chunkBodies []string) bool {
	lines := nonEmptyLines(text)
	if len(lines) == 0 {
		return false
	}
	headerNeedles := make([]string, 0)
	for _, line := range lines {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "<table") || strings.Contains(lower, "<th") || strings.Contains(lower, "</table>") {
			headerNeedles = append(headerNeedles, strings.TrimSpace(line))
		}
	}
	if len(headerNeedles) == 0 {
		return false
	}
	relevant := chunkBodiesWithAll(chunkBodies, headerNeedles)
	if len(relevant) == 0 {
		return false
	}
	dataRows := htmlDataRows(lines)
	for _, row := range dataRows {
		if !anyBodyContains(relevant, row) && !longLineFragmentsPreserved(row, relevant) {
			return false
		}
	}
	return len(dataRows) > 0
}

func htmlDataRows(lines []string) []string {
	rows := make([]string, 0)
	for i := 0; i < len(lines); i++ {
		lower := strings.ToLower(lines[i])
		if !strings.Contains(lower, "<tr") {
			continue
		}
		rowLines := []string{lines[i]}
		for !strings.Contains(strings.ToLower(lines[i]), "</tr>") && i+1 < len(lines) {
			i++
			rowLines = append(rowLines, lines[i])
		}
		row := strings.Join(rowLines, "\n")
		if strings.Contains(strings.ToLower(row), "<td") {
			rows = append(rows, row)
		}
	}
	return rows
}

func contextBlockSplitPreserved(text string, chunkBodies []string) bool {
	lines := nonEmptyLines(text)
	if len(lines) == 0 {
		return false
	}
	for _, line := range lines {
		if !anyBodyContains(chunkBodies, strings.TrimSpace(line)) && !longLineFragmentsPreserved(line, chunkBodies) {
			return false
		}
	}
	return true
}

func listSplitPreserved(text string, chunkBodies []string) bool {
	items := splitListItems(nonEmptyLines(text))
	if len(items) == 0 {
		return false
	}
	for _, item := range items {
		if !anyBodyContains(chunkBodies, item) && !contextBlockSplitPreserved(item, chunkBodies) {
			return false
		}
	}
	return true
}

func admonitionSplitPreserved(text string, chunkBodies []string) bool {
	lines := nonEmptyLines(text)
	if len(lines) < 2 {
		return false
	}
	opening := strings.TrimSpace(lines[0])
	closing := ""
	bodyEnd := len(lines)
	if strings.HasPrefix(opening, ":::") && strings.TrimSpace(lines[len(lines)-1]) == ":::" {
		closing = strings.TrimSpace(lines[len(lines)-1])
		bodyEnd = len(lines) - 1
	}
	needles := []string{opening}
	if closing != "" {
		needles = append(needles, closing)
	}
	relevant := chunkBodiesWithAll(chunkBodies, needles)
	if len(relevant) == 0 {
		return false
	}
	for _, line := range lines[1:bodyEnd] {
		if !anyBodyContains(relevant, strings.TrimSpace(line)) && !longLineFragmentsPreserved(line, relevant) {
			return false
		}
	}
	return true
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
		if len(current) > 0 {
			current = append(current, line)
		}
	}
	flush()
	return items
}

func longLineFragmentsPreserved(line string, chunkBodies []string) bool {
	normalized := normalizeForContainment(line)
	runes := []rune(normalized)
	if len(runes) < 80 {
		return false
	}
	prefix := strings.TrimSpace(string(runes[:40]))
	suffix := strings.TrimSpace(string(runes[len(runes)-40:]))
	return anyBodyContains(chunkBodies, prefix) && anyBodyContains(chunkBodies, suffix)
}

func chunkBodiesWithAll(chunkBodies []string, needles []string) []string {
	out := make([]string, 0)
	for _, body := range chunkBodies {
		ok := true
		for _, needle := range needles {
			needle = normalizeForContainment(needle)
			if needle != "" && !strings.Contains(body, needle) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, body)
		}
	}
	return out
}

func anyBodyContains(bodies []string, needle string) bool {
	needle = normalizeForContainment(needle)
	if needle == "" {
		return true
	}
	for _, body := range bodies {
		if strings.Contains(body, needle) {
			return true
		}
	}
	return false
}

func mapChunkSpans(content string, chunks []embeddingchunk.EmbeddingChunk) ([]ChunkSpan, int) {
	spans := make([]ChunkSpan, 0, len(chunks))
	cursor := 0
	unmapped := 0
	for _, chunk := range chunks {
		body := strings.TrimSpace(normalizeNewlines(chunkBodyText(chunk.Text)))
		span := ChunkSpan{ChunkOrdinal: chunk.ChunkOrdinal}
		if body == "" {
			unmapped++
			spans = append(spans, span)
			continue
		}
		idx := -1
		if cursor < len(content) {
			if rel := strings.Index(content[cursor:], body); rel >= 0 {
				idx = cursor + rel
			}
		}
		if idx < 0 {
			idx = strings.Index(content, body)
		}
		if idx < 0 {
			unmapped++
			spans = append(spans, span)
			continue
		}
		span.Start = idx
		span.End = idx + len(body)
		span.Mapped = true
		cursor = idx + maxInt(1, len(body))
		spans = append(spans, span)
	}
	return spans, unmapped
}

func protectedBlocks(content string) []protectedBlock {
	lines, starts := splitLinesWithOffsets(content)
	blocks := make([]protectedBlock, 0)
	covered := make([]bool, len(lines))
	add := func(kind string, startLine int, endLine int) {
		if startLine < 0 {
			startLine = 0
		}
		if endLine >= len(lines) {
			endLine = len(lines) - 1
		}
		if startLine > endLine || startLine >= len(lines) {
			return
		}
		for i := startLine; i <= endLine; i++ {
			covered[i] = true
		}
		blocks = append(blocks, protectedBlock{
			kind:      kind,
			text:      strings.Join(lines[startLine:endLine+1], "\n"),
			start:     starts[startLine],
			end:       starts[endLine] + len(lines[endLine]),
			startLine: startLine + 1,
			endLine:   endLine + 1,
		})
	}

	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if isFenceStart(trimmed) {
			fence := trimmed[:3]
			end := i
			for j := i + 1; j < len(lines); j++ {
				end = j
				if strings.HasPrefix(strings.TrimSpace(lines[j]), fence) {
					break
				}
			}
			add("fenced_code", i, end)
			i = end
			continue
		}
		if strings.Contains(strings.ToLower(trimmed), "<table") {
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
			add("html_table", i, end)
			i = end
		}
	}

	for i := 0; i < len(lines); i++ {
		if covered[i] {
			continue
		}
		if isMarkdownTableStart(lines, i) {
			end := i + 1
			for end+1 < len(lines) && !covered[end+1] && isMarkdownTableRow(lines[end+1]) {
				end++
			}
			add("markdown_table", i, end)
			i = end
		}
	}

	for i := 0; i < len(lines); i++ {
		if covered[i] {
			continue
		}
		switch {
		case isFigureStart(lines[i]):
			end := i
			for j := i; j < len(lines); j++ {
				end = j
				if strings.Contains(strings.ToLower(lines[j]), "</figure>") {
					break
				}
			}
			add("image_caption", i, end)
			i = end
		case isImageLine(lines[i]):
			end := i
			if i+1 < len(lines) && !covered[i+1] && isCaptionLine(lines[i+1]) {
				end = i + 1
			}
			add("image_caption", i, end)
			i = end
		case isListLine(lines[i]):
			end := i
			for end+1 < len(lines) && !covered[end+1] && isListContinuation(lines[end+1]) {
				end++
			}
			if end > i {
				add("list", i, end)
				i = end
			}
		case isAdmonitionStart(lines[i]):
			end := i
			for end+1 < len(lines) && !covered[end+1] && strings.TrimSpace(lines[end+1]) != "" {
				end++
				if strings.TrimSpace(lines[end]) == ":::" {
					break
				}
			}
			add("admonition", i, end)
			i = end
		case isBlockquoteLine(lines[i]):
			end := i
			for end+1 < len(lines) && !covered[end+1] && isBlockquoteLine(lines[end+1]) {
				end++
			}
			if end > i {
				add("blockquote", i, end)
				i = end
			}
		}
	}

	sort.SliceStable(blocks, func(i, j int) bool {
		if blocks[i].startLine == blocks[j].startLine {
			return blocks[i].endLine < blocks[j].endLine
		}
		return blocks[i].startLine < blocks[j].startLine
	})
	return blocks
}

func splitLinesWithOffsets(content string) ([]string, []int) {
	lines := strings.Split(content, "\n")
	starts := make([]int, len(lines))
	offset := 0
	for i, line := range lines {
		starts[i] = offset
		offset += len(line) + 1
	}
	return lines, starts
}

func chunkBodyText(text string) string {
	const marker = "\ncontent:\n"
	if _, body, ok := strings.Cut(text, marker); ok {
		return body
	}
	return text
}

func normalizeNewlines(text string) string {
	return strings.ReplaceAll(text, "\r\n", "\n")
}

func nonEmptyLines(text string) []string {
	raw := strings.Split(strings.TrimSpace(normalizeNewlines(text)), "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func normalizeForContainment(text string) string {
	text = normalizeNewlines(text)
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

func isFenceStart(line string) bool {
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

func isFigureStart(line string) bool {
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
		strings.Contains(trimmed, "<figcaption") ||
		isItalicCaption(trimmed)
}

var italicCaptionPattern = regexp.MustCompile(`^[_*].+[_*]$`)

func isItalicCaption(line string) bool {
	return italicCaptionPattern.MatchString(strings.TrimSpace(line))
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

func isListContinuation(line string) bool {
	return strings.TrimSpace(line) == "" || isListLine(line) || strings.HasPrefix(line, "  ")
}

func isBlockquoteLine(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), ">")
}

func isAdmonitionStart(line string) bool {
	trimmed := strings.TrimSpace(strings.ToLower(line))
	return strings.HasPrefix(trimmed, "!!! ") ||
		strings.HasPrefix(trimmed, "??? ") ||
		strings.HasPrefix(trimmed, ":::") ||
		strings.HasPrefix(trimmed, "> [!note]") ||
		strings.HasPrefix(trimmed, "> [!warning]") ||
		strings.HasPrefix(trimmed, "> [!tip]")
}

func maxInt(a int, b int) int {
	if a > b {
		return a
	}
	return b
}
