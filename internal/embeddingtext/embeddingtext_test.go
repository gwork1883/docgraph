package embeddingtext

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/docgraph/docgraph/internal/domain"
)

func TestBuildContentWithTokenBudgetTruncatesContentOnly(t *testing.T) {
	section := domain.EmbeddingSection{
		DocumentTitle: "Doc",
		SourceName:    "Source",
		ProductHint:   "Product",
		ModuleHint:    "Module",
		HeadingPath:   "A > B",
		Title:         "Section",
		Content:       strings.Repeat("长", 5000),
	}

	got := BuildContentWithTokenBudget(section, 1200)

	for _, want := range []string{
		"document: Doc",
		"source: Source",
		"hint: Product / Module",
		"heading_path: A > B",
		"section: Section",
		"[truncated for embedding input]",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("BuildContentWithTokenBudget missing %q in %q", want, got)
		}
	}
	if runes := utf8.RuneCountInString(got); runes > 1300 {
		t.Fatalf("BuildContentWithTokenBudget rune count = %d, want <= 1300", runes)
	}
}
