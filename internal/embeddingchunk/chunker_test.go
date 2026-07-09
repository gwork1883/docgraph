package embeddingchunk

import (
	"strconv"
	"strings"
	"testing"

	"github.com/docgraph/docgraph/internal/domain"
)

func TestCL100KTokenizerIsAvailableAndCountsDifferently(t *testing.T) {
	measurer, info := ResolveTextMeasurer("cl100k_base", "text-embedding-3-small")
	if measurer == nil {
		t.Fatal("ResolveTextMeasurer returned nil measurer")
	}
	if measurer.Name() != "cl100k_base" {
		t.Fatalf("measurer name = %q, want cl100k_base", measurer.Name())
	}
	if info.Name != "cl100k_base" || !info.Available {
		t.Fatalf("info = %+v, want available cl100k_base", info)
	}

	text := "hello world!你好，世界！GET /api/v1/documents"
	got := measurer.CountTokens(text)
	conservative := (ConservativeTextMeasurer{}).CountTokens(text)
	if got == conservative {
		t.Fatalf("cl100k tokens = conservative tokens = %d for %q, want different counts", got, text)
	}

	infos := AvailableTokenizers()
	foundCL100K := false
	for _, item := range infos {
		if item.Name == "cl100k_base" && item.Available {
			foundCL100K = true
		}
	}
	if !foundCL100K {
		t.Fatalf("AvailableTokenizers = %+v, want available cl100k_base entry", infos)
	}
}

func TestTokenizerRegistryFallsBackForUnavailableCL100K(t *testing.T) {
	reg := tokenizerRegistry["cl100k_base"]
	t.Cleanup(func() {
		tokenizerRegistry["cl100k_base"] = reg
	})
	reg.info.Available = false
	reg.factory = nil
	tokenizerRegistry["cl100k_base"] = reg

	measurer, info := ResolveTextMeasurer("cl100k_base", "text-embedding-3-small")
	if measurer == nil {
		t.Fatal("ResolveTextMeasurer returned nil measurer")
	}
	if measurer.Name() != "conservative" {
		t.Fatalf("measurer name = %q, want conservative fallback", measurer.Name())
	}
	if info.Name != "conservative" || !info.Available {
		t.Fatalf("info = %+v, want conservative available fallback", info)
	}
}

func TestCompareChunkPlansSummarizesMultiplePlans(t *testing.T) {
	section := domain.EmbeddingSection{
		SectionID:     "section-compare",
		DocumentID:    "doc-compare",
		SourceID:      "source-compare",
		SourceName:    "Docs",
		DocumentTitle: "Compare",
		Content:       strings.Repeat("alpha beta gamma delta\n\n", 80),
		ContentHash:   "hash-compare",
	}

	summaries, err := CompareChunkPlans(section, "test-model", "generator-v1", []ChunkPlan{
		{Tokenizer: "conservative", Strategy: "structural", ChunkTargetTokens: 96},
		{Tokenizer: "cl100k_base", Strategy: "recursive", ChunkTargetTokens: 160},
		{Tokenizer: "conservative", Strategy: "adaptive", ChunkTargetTokens: 128},
	})
	if err != nil {
		t.Fatalf("CompareChunkPlans returned error: %v", err)
	}
	if len(summaries) != 3 {
		t.Fatalf("summary count = %d, want 3", len(summaries))
	}
	if summaries[0].ChunkCount <= summaries[1].ChunkCount {
		t.Fatalf("summaries = %+v, want smaller target to produce more chunks", summaries)
	}
	for _, summary := range summaries {
		if summary.TotalTokens <= 0 || summary.MinTokens <= 0 || summary.MaxTokens <= 0 || summary.AvgTokens <= 0 {
			t.Fatalf("summary = %+v, want populated token metrics", summary)
		}
	}
}

func TestUnsupportedChunkStrategyReturnsError(t *testing.T) {
	_, err := Chunker{Strategy: "missing_strategy"}.BuildChunks(domain.EmbeddingSection{
		SectionID:   "section",
		DocumentID:  "doc",
		SourceID:    "source",
		Content:     "content",
		ContentHash: "hash",
	}, "model", "generator")
	if err == nil {
		t.Fatal("BuildChunks returned nil error for unsupported strategy")
	}
}

func TestAutoChunkStrategyKeepsSmallSectionAndUsesStablePlanName(t *testing.T) {
	chunks := buildAutoChunks(t, "Intro.\n\n| Name | Value |\n| --- | --- |\n| DOCGRAPH_TOKEN | required |", 512)
	if len(chunks) != 1 {
		t.Fatalf("auto chunks = %d, want 1 for fitting section", len(chunks))
	}
	if chunks[0].ChunkStrategy != "auto" {
		t.Fatalf("chunk strategy = %q, want auto", chunks[0].ChunkStrategy)
	}
	if !strings.Contains(chunks[0].Text, "DOCGRAPH_TOKEN") {
		t.Fatalf("chunk text = %q, want section content", chunks[0].Text)
	}
}

func TestAutoChunkStrategyFallsBackToBlockAwarePackingForOversizedSection(t *testing.T) {
	rows := []string{"| Name | Value |", "| --- | --- |"}
	for i := 0; i < 40; i++ {
		rows = append(rows, "| KEY_"+strconv.Itoa(i)+" | "+strings.Repeat("value ", 8)+" |")
	}
	chunks := buildAutoChunks(t, strings.Join(rows, "\n"), 160)
	tableChunks := chunksContaining(chunks, "| Name | Value |")
	if len(tableChunks) < 2 {
		t.Fatalf("table chunks = %d, want oversized auto table split with repeated header: %+v", len(tableChunks), chunks)
	}
	for _, chunk := range tableChunks {
		if chunk.ChunkStrategy != "auto" {
			t.Fatalf("chunk strategy = %q, want auto", chunk.ChunkStrategy)
		}
		if !strings.Contains(chunk.Text, "| Name | Value |\n| --- | --- |") {
			t.Fatalf("chunk text = %q, want repeated markdown table header", chunk.Text)
		}
	}
}

func TestSectionWholeChunkStrategyKeepsSmallSectionTogether(t *testing.T) {
	strategy, info, err := ResolveChunkStrategy("section_whole")
	if err != nil {
		t.Fatalf("ResolveChunkStrategy section_whole returned error: %v", err)
	}
	if strategy.Name() != "section_whole" || !info.Available {
		t.Fatalf("strategy=%s info=%+v, want available section_whole", strategy.Name(), info)
	}
	section := domain.EmbeddingSection{
		SectionID:     "section-whole",
		DocumentID:    "doc-whole",
		SourceID:      "source-whole",
		SourceName:    "Docs",
		DocumentTitle: "Whole",
		Title:         "Small section",
		Content:       "Intro.\n\n| Name | Value |\n| --- | --- |\n| DOCGRAPH_TOKEN | required |",
		ContentHash:   "hash-whole",
	}
	chunks, err := Chunker{
		Measurer:          ConservativeTextMeasurer{},
		Strategy:          "section_whole",
		ChunkTargetTokens: 512,
	}.BuildChunks(section, "test-model", "generator-v1")
	if err != nil {
		t.Fatalf("BuildChunks section_whole returned error: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("section_whole chunks = %d, want 1", len(chunks))
	}
	if !strings.Contains(chunks[0].Text, "DOCGRAPH_TOKEN") {
		t.Fatalf("chunk text = %q, want section content", chunks[0].Text)
	}
}

func TestBlockAwarePackStrategyKeepsMarkdownTableTogetherWhenItFits(t *testing.T) {
	section := domain.EmbeddingSection{
		SectionID:     "section-block-aware",
		DocumentID:    "doc-block-aware",
		SourceID:      "source-block-aware",
		SourceName:    "Docs",
		DocumentTitle: "Block aware",
		Title:         "Table section",
		Content:       numberedChunkerLines("alpha beta gamma delta", 35) + "\n| Name | Value |\n| --- | --- |\n| DOCGRAPH_TOKEN | required |\n| DOCGRAPH_DATA | path |\n\n" + strings.Repeat("omega sigma lambda kappa\n\n", 20),
		ContentHash:   "hash-block-aware",
	}
	structural, err := Chunker{
		Measurer:           ConservativeTextMeasurer{},
		Strategy:           "structural",
		ChunkTargetTokens:  192,
		ChunkOverlapTokens: 0,
	}.BuildChunks(section, "test-model", "generator-v1")
	if err != nil {
		t.Fatalf("BuildChunks structural returned error: %v", err)
	}
	blockAware, err := Chunker{
		Measurer:           ConservativeTextMeasurer{},
		Strategy:           "block_aware_pack",
		ChunkTargetTokens:  192,
		ChunkOverlapTokens: 0,
	}.BuildChunks(section, "test-model", "generator-v1")
	if err != nil {
		t.Fatalf("BuildChunks block_aware_pack returned error: %v", err)
	}
	table := "| Name | Value |\n| --- | --- |\n| DOCGRAPH_TOKEN | required |\n| DOCGRAPH_DATA | path |"
	if chunksContainText(structural, table) {
		t.Fatalf("structural unexpectedly preserved table in one chunk: %+v", structural)
	}
	if !chunksContainText(blockAware, table) {
		t.Fatalf("block_aware_pack chunks = %+v, want table preserved in one chunk", blockAware)
	}
}

func TestBlockAwarePackSplitsOversizedMarkdownTableWithRepeatedHeader(t *testing.T) {
	rows := []string{"| Name | Value |", "| --- | --- |"}
	for i := 0; i < 40; i++ {
		rows = append(rows, "| KEY_"+strconv.Itoa(i)+" | "+strings.Repeat("value ", 8)+" |")
	}
	chunks := buildBlockAwareChunks(t, strings.Join(rows, "\n"), 160)
	tableChunks := chunksContaining(chunks, "| Name | Value |")
	if len(tableChunks) < 2 {
		t.Fatalf("table chunks = %d, want oversized table split into multiple chunks: %+v", len(tableChunks), chunks)
	}
	for _, chunk := range tableChunks {
		if !strings.Contains(chunk.Text, "| Name | Value |\n| --- | --- |") {
			t.Fatalf("chunk text = %q, want repeated markdown table header", chunk.Text)
		}
	}
}

func TestBlockAwarePackSplitsOversizedHTMLTableWithRepeatedHeader(t *testing.T) {
	lines := []string{
		"<table>",
		"<thead><tr><th>Name</th><th>Value</th></tr></thead>",
		"<tbody>",
	}
	for i := 0; i < 30; i++ {
		lines = append(lines, "<tr><td>KEY_"+strconv.Itoa(i)+"</td><td>"+strings.Repeat("value ", 8)+"</td></tr>")
	}
	lines = append(lines, "</tbody>", "</table>")
	chunks := buildBlockAwareChunks(t, strings.Join(lines, "\n"), 180)
	tableChunks := chunksContaining(chunks, "<table>")
	if len(tableChunks) < 2 {
		t.Fatalf("table chunks = %d, want oversized html table split into multiple chunks: %+v", len(tableChunks), chunks)
	}
	for _, chunk := range tableChunks {
		if !strings.Contains(chunk.Text, "<thead><tr><th>Name</th><th>Value</th></tr></thead>") ||
			!strings.Contains(chunk.Text, "</table>") {
			t.Fatalf("chunk text = %q, want repeated html table header and closing table", chunk.Text)
		}
	}
}

func TestBlockAwarePackSplitsOversizedFencedCodeWithFencePreserved(t *testing.T) {
	lines := []string{"```go"}
	for i := 0; i < 60; i++ {
		lines = append(lines, `fmt.Println("`+strconv.Itoa(i)+strings.Repeat("x", 24)+`")`)
	}
	lines = append(lines, "```")
	chunks := buildBlockAwareChunks(t, strings.Join(lines, "\n"), 160)
	codeChunks := chunksContaining(chunks, "```go")
	if len(codeChunks) < 2 {
		t.Fatalf("code chunks = %d, want oversized code block split into multiple chunks: %+v", len(codeChunks), chunks)
	}
	for _, chunk := range codeChunks {
		body := chunkBodyForTest(chunk.Text)
		if !strings.HasPrefix(body, "```go\n") || !strings.HasSuffix(body, "\n```") {
			t.Fatalf("chunk body = %q, want fenced code wrapper preserved", body)
		}
	}
}

func TestBlockAwarePackSplitsOversizedFencedCodePrefersFunctionBoundaries(t *testing.T) {
	lines := []string{"```go"}
	for fn := 0; fn < 6; fn++ {
		lines = append(lines, "")
		lines = append(lines, "// function "+strconv.Itoa(fn))
		lines = append(lines, "func Example"+strconv.Itoa(fn)+"() {")
		for i := 0; i < 8; i++ {
			lines = append(lines, `fmt.Println("fn`+strconv.Itoa(fn)+` line `+strconv.Itoa(i)+strings.Repeat("x", 16)+`")`)
		}
		lines = append(lines, "}")
	}
	lines = append(lines, "```")
	chunks := buildBlockAwareChunks(t, strings.Join(lines, "\n"), 170)
	codeChunks := chunksContaining(chunks, "```go")
	if len(codeChunks) < 2 {
		t.Fatalf("code chunks = %d, want oversized code block split into multiple chunks: %+v", len(codeChunks), chunks)
	}
	for _, chunk := range codeChunks {
		body := chunkBodyForTest(chunk.Text)
		if !strings.HasPrefix(body, "```go\n") || !strings.HasSuffix(body, "\n```") {
			t.Fatalf("chunk body = %q, want fenced code wrapper preserved", body)
		}
		if strings.Contains(body, "func Example1()") && strings.Contains(body, "func Example2()") {
			t.Fatalf("chunk body = %q, want function boundaries preferred over mixing adjacent functions", body)
		}
	}
}

func TestBlockAwarePackSplitsOversizedHTMLTableWithCaptionAndMultiLineSections(t *testing.T) {
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
	chunks := buildBlockAwareChunks(t, strings.Join(lines, "\n"), 180)
	tableChunks := chunksContaining(chunks, "<table>")
	if len(tableChunks) < 2 {
		t.Fatalf("table chunks = %d, want oversized html table split into multiple chunks: %+v", len(tableChunks), chunks)
	}
	for _, chunk := range tableChunks {
		body := chunkBodyForTest(chunk.Text)
		for _, want := range []string{"<caption>\nConfiguration keys\n</caption>", "<thead>\n<tr>\n<th>Name</th>\n<th>Value</th>\n</tr>\n</thead>", "<tbody>", "</tbody>\n</table>"} {
			if !strings.Contains(body, want) {
				t.Fatalf("chunk body = %q, want repeated html table context %q", body, want)
			}
		}
	}
}

func TestBlockAwarePackSplitsOversizedSingleTableRowsWithHeaderContext(t *testing.T) {
	markdown := strings.Join([]string{
		"| Name | Value |",
		"| --- | --- |",
		"| OVERSIZED | " + strings.Repeat("value ", 120) + " |",
	}, "\n")
	markdownChunks := chunksContaining(buildBlockAwareChunks(t, markdown, 120), "| Name | Value |")
	if len(markdownChunks) < 2 {
		t.Fatalf("markdown chunks = %d, want single oversized row split with repeated header", len(markdownChunks))
	}
	for _, chunk := range markdownChunks {
		body := chunkBodyForTest(chunk.Text)
		if !strings.Contains(body, "| Name | Value |\n| --- | --- |") {
			t.Fatalf("chunk body = %q, want markdown header context", body)
		}
	}

	html := strings.Join([]string{
		"<table>",
		"<caption>Large row</caption>",
		"<thead><tr><th>Name</th><th>Value</th></tr></thead>",
		"<tbody>",
		"<tr><td>OVERSIZED</td><td>" + strings.Repeat("value ", 120) + "</td></tr>",
		"</tbody>",
		"</table>",
	}, "\n")
	htmlChunks := chunksContaining(buildBlockAwareChunks(t, html, 140), "<table>")
	if len(htmlChunks) < 2 {
		t.Fatalf("html chunks = %d, want single oversized row split with repeated header", len(htmlChunks))
	}
	for _, chunk := range htmlChunks {
		body := chunkBodyForTest(chunk.Text)
		if !strings.Contains(body, "<caption>Large row</caption>") ||
			!strings.Contains(body, "<thead><tr><th>Name</th><th>Value</th></tr></thead>") ||
			!strings.Contains(body, "</table>") {
			t.Fatalf("chunk body = %q, want html table context", body)
		}
	}
}

func TestBlockAwarePackSplitsOversizedListAndAdmonitionWithMarkers(t *testing.T) {
	listLines := make([]string, 0)
	for i := 0; i < 30; i++ {
		listLines = append(listLines, "- item "+strconv.Itoa(i)+" "+strings.Repeat("detail ", 12))
	}
	listChunks := buildBlockAwareChunks(t, strings.Join(listLines, "\n"), 120)
	if len(listChunks) < 2 {
		t.Fatalf("list chunks = %d, want oversized list split into item groups", len(listChunks))
	}
	for _, chunk := range listChunks {
		body := chunkBodyForTest(chunk.Text)
		if !strings.Contains(body, "- item") {
			t.Fatalf("chunk body = %q, want list marker context", body)
		}
	}

	admonitionLines := []string{"::: warning"}
	for i := 0; i < 24; i++ {
		admonitionLines = append(admonitionLines, "Keep setting "+strconv.Itoa(i)+" "+strings.Repeat("enabled ", 10))
	}
	admonitionLines = append(admonitionLines, ":::")
	admonitionChunks := chunksContaining(buildBlockAwareChunks(t, strings.Join(admonitionLines, "\n"), 120), "::: warning")
	if len(admonitionChunks) < 2 {
		t.Fatalf("admonition chunks = %d, want oversized admonition split with marker repeated", len(admonitionChunks))
	}
	for _, chunk := range admonitionChunks {
		body := chunkBodyForTest(chunk.Text)
		if !strings.HasPrefix(body, "::: warning\n") || !strings.HasSuffix(body, "\n:::") {
			t.Fatalf("chunk body = %q, want admonition wrapper context", body)
		}
	}
}

func TestAdaptiveChunkStrategyIsAvailableAndBuildsTraceableChunks(t *testing.T) {
	strategy, info, err := ResolveChunkStrategy("adaptive")
	if err != nil {
		t.Fatalf("ResolveChunkStrategy adaptive returned error: %v", err)
	}
	if strategy.Name() != "adaptive" || !info.Available {
		t.Fatalf("strategy=%s info=%+v, want available adaptive", strategy.Name(), info)
	}
	section := domain.EmbeddingSection{
		SectionID:     "section-adaptive",
		DocumentID:    "doc-adaptive",
		SourceID:      "source-adaptive",
		SourceName:    "Docs",
		DocumentTitle: "Adaptive",
		Content:       strings.Repeat("alpha beta gamma delta epsilon zeta eta theta.\n\n", 90),
		ContentHash:   "hash-adaptive",
	}
	chunks, err := Chunker{
		Measurer:          ConservativeTextMeasurer{},
		Strategy:          "adaptive",
		ChunkTargetTokens: 120,
	}.BuildChunks(section, "test-model", "generator-v1")
	if err != nil {
		t.Fatalf("BuildChunks adaptive returned error: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("adaptive chunks = %d, want multiple chunks", len(chunks))
	}
	for i, chunk := range chunks {
		if chunk.ChunkStrategy != "adaptive" {
			t.Fatalf("chunk[%d].ChunkStrategy = %q, want adaptive", i, chunk.ChunkStrategy)
		}
		if chunk.ChunkID == "" || chunk.Tokenizer != "conservative" {
			t.Fatalf("chunk[%d] = %+v, want id and tokenizer", i, chunk)
		}
	}
}

func chunksContainText(chunks []EmbeddingChunk, text string) bool {
	for _, chunk := range chunks {
		if strings.Contains(chunk.Text, text) {
			return true
		}
	}
	return false
}

func chunksContaining(chunks []EmbeddingChunk, text string) []EmbeddingChunk {
	out := make([]EmbeddingChunk, 0)
	for _, chunk := range chunks {
		if strings.Contains(chunk.Text, text) {
			out = append(out, chunk)
		}
	}
	return out
}

func buildBlockAwareChunks(t *testing.T, content string, targetTokens int) []EmbeddingChunk {
	t.Helper()
	return buildChunksWithStrategy(t, "block_aware_pack", content, targetTokens)
}

func buildAutoChunks(t *testing.T, content string, targetTokens int) []EmbeddingChunk {
	t.Helper()
	return buildChunksWithStrategy(t, "auto", content, targetTokens)
}

func buildChunksWithStrategy(t *testing.T, strategy string, content string, targetTokens int) []EmbeddingChunk {
	t.Helper()
	chunks, err := Chunker{
		Measurer:           ConservativeTextMeasurer{},
		Strategy:           strategy,
		ChunkTargetTokens:  targetTokens,
		ChunkOverlapTokens: 0,
	}.BuildChunks(domain.EmbeddingSection{
		SectionID:     "section-oversized",
		DocumentID:    "doc-oversized",
		SourceID:      "source-oversized",
		SourceName:    "Docs",
		DocumentTitle: "Oversized",
		Title:         "Oversized block",
		Content:       content,
		ContentHash:   "hash-oversized",
	}, "test-model", "generator-v1")
	if err != nil {
		t.Fatalf("BuildChunks %s returned error: %v", strategy, err)
	}
	return chunks
}

func chunkBodyForTest(text string) string {
	if _, body, ok := strings.Cut(text, "\ncontent:\n"); ok {
		return strings.TrimSpace(body)
	}
	return strings.TrimSpace(text)
}

func numberedChunkerLines(prefix string, count int) string {
	lines := make([]string, 0, count)
	for i := 0; i < count; i++ {
		lines = append(lines, prefix+" "+strconv.Itoa(i))
	}
	return strings.Join(lines, "\n")
}
