package embedding

import (
	"github.com/docgraph/docgraph/internal/domain"
	"github.com/docgraph/docgraph/internal/embeddingchunk"
)

const DefaultChunkGeneratorVersion = embeddingchunk.DefaultChunkGeneratorVersion

type TextMeasurer = embeddingchunk.TextMeasurer
type TokenizerInfo = embeddingchunk.TokenizerInfo
type ConservativeTextMeasurer = embeddingchunk.ConservativeTextMeasurer
type Chunker = embeddingchunk.Chunker
type EmbeddingChunk = embeddingchunk.EmbeddingChunk
type ChunkStrategyInfo = embeddingchunk.ChunkStrategyInfo
type ChunkPlan = embeddingchunk.ChunkPlan
type ChunkPlanSummary = embeddingchunk.ChunkPlanSummary

func NewTextMeasurer(name string, model string) TextMeasurer {
	return embeddingchunk.NewTextMeasurer(name, model)
}

func ResolveTextMeasurer(name string, model string) (TextMeasurer, TokenizerInfo) {
	return embeddingchunk.ResolveTextMeasurer(name, model)
}

func AvailableTokenizers() []TokenizerInfo {
	return embeddingchunk.AvailableTokenizers()
}

func AvailableChunkStrategies() []ChunkStrategyInfo {
	return embeddingchunk.AvailableChunkStrategies()
}

func CompareChunkPlans(section domain.EmbeddingSection, model string, generatorVersion string, plans []ChunkPlan) ([]ChunkPlanSummary, error) {
	return embeddingchunk.CompareChunkPlans(section, model, generatorVersion, plans)
}

func BuildChunkID(sectionID string, ordinal int, model string, generatorVersion string, textHash string) string {
	return embeddingchunk.BuildChunkID(sectionID, ordinal, model, generatorVersion, textHash)
}

func BuildChunkIDForPlan(sectionID string, ordinal int, model string, generatorVersion string, tokenizer string, chunkStrategy string, textHash string) string {
	return embeddingchunk.BuildChunkIDForPlan(sectionID, ordinal, model, generatorVersion, tokenizer, chunkStrategy, textHash)
}
