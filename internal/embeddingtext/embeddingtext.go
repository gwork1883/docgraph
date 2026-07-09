package embeddingtext

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf8"

	"github.com/docgraph/docgraph/internal/domain"
)

func BuildContent(section domain.EmbeddingSection) string {
	return BuildContentWithTokenBudget(section, 0)
}

func BuildContentWithTokenBudget(section domain.EmbeddingSection, maxInputTokens int) string {
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
	content := strings.TrimSpace(section.Content)
	if maxInputTokens > 0 {
		metadataTokens := estimateTokenCount(strings.Join(append(parts, "content:"), "\n"))
		contentBudget := maxInputTokens - metadataTokens - 32
		if contentBudget < 0 {
			contentBudget = 0
		}
		content = truncateRunes(content, contentBudget)
	}
	parts = append(parts, "content:\n"+content)
	return strings.Join(parts, "\n")
}

func HashText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func SectionTextHash(section domain.EmbeddingSection) string {
	return HashText(BuildContent(section))
}

func SectionTextHashWithTokenBudget(section domain.EmbeddingSection, maxInputTokens int) string {
	return HashText(BuildContentWithTokenBudget(section, maxInputTokens))
}

func NeedsEnsure(section domain.EmbeddingSection, existing domain.SectionEmbeddingHash, generatorVersion string) bool {
	return NeedsEnsureWithTokenBudget(section, existing, generatorVersion, 0)
}

func NeedsEnsureWithTokenBudget(section domain.EmbeddingSection, existing domain.SectionEmbeddingHash, generatorVersion string, maxInputTokens int) bool {
	return existing.SectionID == "" ||
		existing.DocumentID != section.DocumentID ||
		existing.SourceID != section.SourceID ||
		existing.ContentHash != section.ContentHash ||
		existing.EmbeddingTextHash != SectionTextHashWithTokenBudget(section, maxInputTokens) ||
		existing.GeneratorVersion != strings.TrimSpace(generatorVersion)
}

func estimateTokenCount(value string) int {
	return utf8.RuneCountInString(value)
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		if value == "" {
			return value
		}
		return "[truncated for embedding input]"
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "\n[truncated for embedding input]"
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
