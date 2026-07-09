package embeddingeval

import (
	"strconv"
	"strings"
	"testing"

	"github.com/docgraph/docgraph/internal/domain"
	"github.com/docgraph/docgraph/internal/embeddingchunk"
)

func TestEvaluateSectionChunksReportsFencedCodeSplit(t *testing.T) {
	lines := []string{"before", "", "```go"}
	for i := 0; i < 80; i++ {
		lines = append(lines, `fmt.Println("`+strconv.Itoa(i)+`")`)
	}
	lines = append(lines, "```", "", "after")
	content := strings.Join(lines, "\n")
	report := evaluateWithPlan(t, content, "structural", 64)

	if report.BlockIntegrity.SplitBlocks == 0 || !hasViolation(report, "fenced_code") {
		t.Fatalf("report = %+v, want fenced_code BI violation", report)
	}
}

func TestEvaluateSectionChunksReportsMarkdownAndHTMLTableSplits(t *testing.T) {
	markdownRows := []string{"| Name | Description |", "| --- | --- |"}
	for i := 0; i < 40; i++ {
		markdownRows = append(markdownRows, "| OPTION_"+strconv.Itoa(i)+"_"+strings.Repeat("X", 12)+" | value description |")
	}
	htmlRows := []string{"<table>", "<thead><tr><th>Name</th><th>Description</th></tr></thead>", "<tbody>"}
	for i := 0; i < 40; i++ {
		htmlRows = append(htmlRows, "<tr><td>FIELD_"+strconv.Itoa(i)+"_"+strings.Repeat("Y", 12)+"</td><td>meaning</td></tr>")
	}
	htmlRows = append(htmlRows, "</tbody>", "</table>")
	content := strings.Join([]string{
		"Markdown table:",
		strings.Join(markdownRows, "\n"),
		"",
		"HTML table:",
		strings.Join(htmlRows, "\n"),
	}, "\n")

	report := evaluateWithPlan(t, content, "structural", 64)

	if !hasViolation(report, "markdown_table") {
		t.Fatalf("violations = %+v, want markdown_table violation", report.Violations)
	}
	if !hasViolation(report, "html_table") {
		t.Fatalf("violations = %+v, want html_table violation", report.Violations)
	}
}

func TestBlockAwarePackPreservesMarkdownAndHTMLTablesThatStructuralSplits(t *testing.T) {
	markdownRows := []string{"| Name | Description |", "| --- | --- |"}
	for i := 0; i < 4; i++ {
		markdownRows = append(markdownRows, "| OPTION_"+strings.Repeat("X", 4)+" | value |")
	}
	markdownPrefix := numberedLines("alpha beta gamma delta", 90)
	htmlRows := []string{
		"<table>",
		"<tr><th>A</th><th>B</th></tr>",
		"<tr><td>X</td><td>Y</td></tr>",
		"</table>",
	}
	htmlPrefix := numberedLines("omega sigma lambda kappa", 90)
	content := strings.Join([]string{
		markdownPrefix,
		strings.Join(markdownRows, "\n"),
		"",
		htmlPrefix,
		strings.Join(htmlRows, "\n"),
	}, "\n")

	blockAware := evaluateWithPlan(t, content, "block_aware_pack", 512)

	if hasViolation(blockAware, "markdown_table") || hasViolation(blockAware, "html_table") {
		t.Fatalf("block-aware violations = %+v, want tables preserved", blockAware.Violations)
	}
	if blockAware.BlockIntegrity.TotalBlocks != 2 || blockAware.BlockIntegrity.PreserveRate != 1 {
		t.Fatalf("block-aware BI = %+v, want both tables preserved", blockAware.BlockIntegrity)
	}
}

func TestBlockAwarePackPreservesFencedCodeThatFitsBudget(t *testing.T) {
	content := "before\n\n```go\n" + strings.Repeat("fmt.Println(\"x\")\n", 8) + "```\n\nafter"
	blockAware := evaluateWithPlan(t, content, "block_aware_pack", 192)

	if hasViolation(blockAware, "fenced_code") {
		t.Fatalf("block-aware violations = %+v, want fenced_code preserved", blockAware.Violations)
	}
}

func TestBlockAwarePackOversizedMarkdownTableSplitCountsAsPreserved(t *testing.T) {
	rows := []string{"| Name | Value |", "| --- | --- |"}
	for i := 0; i < 40; i++ {
		rows = append(rows, "| KEY_"+strconv.Itoa(i)+" | "+strings.Repeat("value ", 8)+" |")
	}
	report := evaluateWithPlan(t, strings.Join(rows, "\n"), "block_aware_pack", 160)

	if hasViolation(report, "markdown_table") {
		t.Fatalf("violations = %+v, want header-repeat row groups to count as preserved", report.Violations)
	}
	if report.BlockIntegrity.PreserveRate != 1 {
		t.Fatalf("BI = %+v, want markdown table preserved by row groups", report.BlockIntegrity)
	}
}

func TestBlockAwarePackOversizedHTMLTableSplitCountsAsPreserved(t *testing.T) {
	lines := []string{
		"<table>",
		"<thead><tr><th>Name</th><th>Value</th></tr></thead>",
		"<tbody>",
	}
	for i := 0; i < 30; i++ {
		lines = append(lines, "<tr><td>KEY_"+strconv.Itoa(i)+"</td><td>"+strings.Repeat("value ", 8)+"</td></tr>")
	}
	lines = append(lines, "</tbody>", "</table>")
	report := evaluateWithPlan(t, strings.Join(lines, "\n"), "block_aware_pack", 180)

	if hasViolation(report, "html_table") {
		t.Fatalf("violations = %+v, want header-repeat row groups to count as preserved", report.Violations)
	}
	if report.BlockIntegrity.PreserveRate != 1 {
		t.Fatalf("BI = %+v, want html table preserved by row groups", report.BlockIntegrity)
	}
}

func TestBlockAwarePackOversizedHTMLTableWithCaptionSplitCountsAsPreserved(t *testing.T) {
	lines := []string{
		"<table>",
		"<caption>",
		"Configuration keys",
		"</caption>",
		"<thead>",
		"<tr>",
		"<th>Name</th>",
		"<th>Value</th>",
		"</tr>",
		"</thead>",
		"<tbody>",
	}
	for i := 0; i < 24; i++ {
		lines = append(lines,
			"<tr>",
			"<td>KEY_"+strconv.Itoa(i)+"</td>",
			"<td>"+strings.Repeat("value ", 9)+"</td>",
			"</tr>",
		)
	}
	lines = append(lines, "</tbody>", "</table>")
	report := evaluateWithPlan(t, strings.Join(lines, "\n"), "block_aware_pack", 180)

	if hasViolation(report, "html_table") {
		t.Fatalf("violations = %+v, want caption/header-repeat row groups to count as preserved", report.Violations)
	}
	if report.BlockIntegrity.PreserveRate != 1 {
		t.Fatalf("BI = %+v, want html table preserved by row groups", report.BlockIntegrity)
	}
}

func TestBlockAwarePackOversizedFencedCodeSplitCountsAsPreserved(t *testing.T) {
	lines := []string{"```go"}
	for i := 0; i < 60; i++ {
		lines = append(lines, `fmt.Println("`+strconv.Itoa(i)+strings.Repeat("x", 24)+`")`)
	}
	lines = append(lines, "```")
	report := evaluateWithPlan(t, strings.Join(lines, "\n"), "block_aware_pack", 160)

	if hasViolation(report, "fenced_code") {
		t.Fatalf("violations = %+v, want fence-preserving splits to count as preserved", report.Violations)
	}
	if report.BlockIntegrity.PreserveRate != 1 {
		t.Fatalf("BI = %+v, want fenced code preserved by wrapped code groups", report.BlockIntegrity)
	}
}

func TestBlockAwarePackOversizedListAndAdmonitionSplitCountsAsPreserved(t *testing.T) {
	listLines := make([]string, 0)
	for i := 0; i < 30; i++ {
		listLines = append(listLines, "- item "+strconv.Itoa(i)+" "+strings.Repeat("detail ", 12))
	}
	listReport := evaluateWithPlan(t, strings.Join(listLines, "\n"), "block_aware_pack", 120)
	if hasViolation(listReport, "list") {
		t.Fatalf("violations = %+v, want item-group list split to count as preserved", listReport.Violations)
	}

	admonitionLines := []string{"::: warning"}
	for i := 0; i < 24; i++ {
		admonitionLines = append(admonitionLines, "Keep setting "+strconv.Itoa(i)+" "+strings.Repeat("enabled ", 10))
	}
	admonitionLines = append(admonitionLines, ":::")
	admonitionReport := evaluateWithPlan(t, strings.Join(admonitionLines, "\n"), "block_aware_pack", 120)
	if hasViolation(admonitionReport, "admonition") {
		t.Fatalf("violations = %+v, want marker-repeat admonition split to count as preserved", admonitionReport.Violations)
	}
}

func TestEvaluateSectionChunksPreservesSectionWholeBlocks(t *testing.T) {
	content := strings.Join([]string{
		"Intro paragraph.",
		"",
		"| Name | Description |",
		"| --- | --- |",
		"| DOCGRAPH_TOKEN | Auth token |",
		"| DOCGRAPH_DATA | Data directory |",
		"",
		"<table>",
		"<tr><th>Code</th><th>Meaning</th></tr>",
		"<tr><td>409</td><td>sync already running</td></tr>",
		"</table>",
	}, "\n")
	report := evaluateWithPlan(t, content, "section_whole", 512)

	if report.BlockIntegrity.TotalBlocks != 2 {
		t.Fatalf("total blocks = %d, want markdown and html table blocks: %+v", report.BlockIntegrity.TotalBlocks, report.ProtectedBlocks)
	}
	if report.BlockIntegrity.SplitBlocks != 0 || len(report.Violations) != 0 {
		t.Fatalf("report = %+v, want section_whole to preserve table blocks", report)
	}
}

func TestEvaluateSectionChunksReportsImageListBlockquoteAdmonition(t *testing.T) {
	content := strings.Join([]string{
		"![Sync flow](sync.png)",
		"Caption: sync status transition diagram",
		"",
		"- source created",
		"- sync queued",
		"- sync completed",
		"",
		"> stale vector hits must be filtered",
		"> before ranking",
		"",
		"!!! note",
		"    Keep browser-mediated cookie acquisition as the main path.",
	}, "\n")
	report := evaluateWithPlan(t, content, "section_whole", 512)

	for _, kind := range []string{"image_caption", "list", "blockquote", "admonition"} {
		if !hasProtectedBlock(report, kind) {
			t.Fatalf("protected blocks = %+v, want %s", report.ProtectedBlocks, kind)
		}
	}
	if report.BlockIntegrity.SplitBlocks != 0 {
		t.Fatalf("report = %+v, want small section_whole blocks preserved", report)
	}
}

func TestEvaluateSectionChunksReportsSizeCompliance(t *testing.T) {
	report := evaluateWithPlan(t, strings.Repeat("alpha beta gamma delta\n\n", 120), "structural", 64)

	if report.SizeCompliance.MaxAcceptableTokens != 64 {
		t.Fatalf("size compliance = %+v, want target max", report.SizeCompliance)
	}
	if report.SizeCompliance.MaxTokens <= 0 || report.SizeCompliance.AvgTokens <= 0 {
		t.Fatalf("size compliance = %+v, want populated token stats", report.SizeCompliance)
	}
}

func evaluateWithPlan(t *testing.T, content string, strategy string, targetTokens int) Report {
	t.Helper()
	section := domain.EmbeddingSection{
		SectionID:     "section-eval",
		DocumentID:    "doc-eval",
		SourceID:      "source-eval",
		SourceName:    "Docs",
		DocumentTitle: "Chunk Eval",
		Title:         "Evaluation",
		HeadingPath:   "Doc > Evaluation",
		Content:       content,
		ContentHash:   "hash-eval",
	}
	chunks, err := embeddingchunk.Chunker{
		Measurer:           embeddingchunk.ConservativeTextMeasurer{},
		Strategy:           strategy,
		ChunkTargetTokens:  targetTokens,
		ChunkOverlapTokens: 0,
	}.BuildChunks(section, "test-model", "generator-v1")
	if err != nil {
		t.Fatalf("BuildChunks returned error: %v", err)
	}
	return EvaluateSectionChunks(section, chunks, Options{TargetTokens: targetTokens})
}

func hasViolation(report Report, kind string) bool {
	for _, violation := range report.Violations {
		if violation.Kind == kind {
			return true
		}
	}
	return false
}

func hasProtectedBlock(report Report, kind string) bool {
	for _, block := range report.ProtectedBlocks {
		if block.Kind == kind {
			return true
		}
	}
	return false
}

func numberedLines(prefix string, count int) string {
	lines := make([]string, 0, count)
	for i := 0; i < count; i++ {
		lines = append(lines, prefix+" "+strconv.Itoa(i))
	}
	return strings.Join(lines, "\n")
}
