package embedding

import (
	"context"
	"strings"
	"testing"

	"github.com/docgraph/docgraph/internal/config"
)

func TestResolveEmbeddingLimitsPrefersExplicitConfig(t *testing.T) {
	limits := ResolveEmbeddingLimits(config.EmbeddingConfig{
		Dimensions:         1024,
		ContextTokens:      4096,
		ChunkTargetTokens:  900,
		ChunkOverlapTokens: 90,
		MaxBatchTokens:     1800,
		BatchSize:          32,
	}, ProviderCapabilities{Dimensions: 768, ContextTokens: 2048, UsageAvailable: true})

	if limits.Dimensions != 1024 || limits.DimensionsSource != "config" {
		t.Fatalf("dimensions = %d/%s, want config dimensions", limits.Dimensions, limits.DimensionsSource)
	}
	if limits.ContextTokens != 4096 || limits.ContextSource != "config" {
		t.Fatalf("context = %d/%s, want config context", limits.ContextTokens, limits.ContextSource)
	}
	if limits.ChunkTargetTokens != 900 || limits.ChunkTargetSource != "config" {
		t.Fatalf("chunk target = %d/%s, want config target", limits.ChunkTargetTokens, limits.ChunkTargetSource)
	}
	if limits.ChunkOverlapTokens != 90 || limits.MaxBatchTokens != 1800 || limits.BatchSize != 32 {
		t.Fatalf("limits = %+v, want explicit overlap/max batch/batch size", limits)
	}
	if !limits.UsageAvailable {
		t.Fatalf("UsageAvailable = false, want provider usage propagated")
	}
}

func TestResolveEmbeddingLimitsUsesProviderProbeAndDefaults(t *testing.T) {
	limits := ResolveEmbeddingLimits(config.EmbeddingConfig{BatchSize: 0}, ProviderCapabilities{
		Dimensions:    768,
		ContextTokens: 2048,
	})

	if limits.Dimensions != 768 || limits.DimensionsSource != "provider_probe" {
		t.Fatalf("dimensions = %d/%s, want provider probe", limits.Dimensions, limits.DimensionsSource)
	}
	if limits.ContextTokens != 2048 || limits.ContextSource != "provider_probe" {
		t.Fatalf("context = %d/%s, want provider probe", limits.ContextTokens, limits.ContextSource)
	}
	if limits.ChunkTargetTokens != 1228 || limits.ChunkTargetSource != "provider_probe" {
		t.Fatalf("chunk target = %d/%s, want 3/5 provider context", limits.ChunkTargetTokens, limits.ChunkTargetSource)
	}
	if limits.MaxBatchTokens != 1536 || limits.MaxBatchSource != "provider_probe" {
		t.Fatalf("max batch = %d/%s, want provider-derived", limits.MaxBatchTokens, limits.MaxBatchSource)
	}
	if limits.BatchSize != 64 {
		t.Fatalf("BatchSize = %d, want default 64", limits.BatchSize)
	}
}

func TestResolveEmbeddingLimitsTightensFromContextExceeded(t *testing.T) {
	limits := ResolveEmbeddingLimits(config.EmbeddingConfig{
		ContextTokens:     4096,
		ChunkTargetTokens: 3000,
		MaxBatchTokens:    3000,
		BatchSize:         64,
	}, ProviderCapabilities{})

	err := parseContextExceededError("model", `{"error":{"message":"maximum context length is 2048 tokens, requested 4096 tokens"}}`)
	tightened := limits.TightenFromContextExceeded(err)

	if tightened.ContextTokens != 2048 || tightened.ContextSource != "context_exceeded" {
		t.Fatalf("context = %d/%s, want context exceeded", tightened.ContextTokens, tightened.ContextSource)
	}
	if tightened.ChunkTargetTokens > 2032 {
		t.Fatalf("chunk target = %d, want clamped below context", tightened.ChunkTargetTokens)
	}
	if tightened.MaxBatchTokens != 1536 || tightened.MaxBatchSource != "context_exceeded" {
		t.Fatalf("max batch = %d/%s, want tightened", tightened.MaxBatchTokens, tightened.MaxBatchSource)
	}
}

func TestResolveEmbeddingLimitsUsesLegacyMaxInputTokens(t *testing.T) {
	limits := ResolveEmbeddingLimits(config.EmbeddingConfig{
		MaxInputTokens: 700,
		BatchSize:      4,
	}, ProviderCapabilities{})

	if limits.ChunkTargetTokens != 700 || limits.ChunkTargetSource != "config_legacy_max_input_tokens" {
		t.Fatalf("chunk target = %d/%s, want legacy max input", limits.ChunkTargetTokens, limits.ChunkTargetSource)
	}
	if limits.MaxBatchTokens != 2800 {
		t.Fatalf("max batch = %d, want chunk target * batch size", limits.MaxBatchTokens)
	}
}

func TestResolveEmbeddingLimitsForEmbedderReadsCapabilities(t *testing.T) {
	embedder := fakeCapabilitiesEmbedder{
		model: "caps-model",
		caps:  ProviderCapabilities{Dimensions: 384, UsageAvailable: true},
	}
	limits := ResolveEmbeddingLimitsForEmbedder(config.EmbeddingConfig{BatchSize: 2}, embedder)

	if limits.Dimensions != 384 || !limits.UsageAvailable {
		t.Fatalf("limits = %+v, want capabilities dimensions and usage", limits)
	}
}

type fakeCapabilitiesEmbedder struct {
	model string
	caps  ProviderCapabilities
}

func (e fakeCapabilitiesEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return nil, nil
}

func (e fakeCapabilitiesEmbedder) Model() string {
	return e.model
}

func (e fakeCapabilitiesEmbedder) Dimensions() int {
	return 0
}

func (e fakeCapabilitiesEmbedder) Capabilities() ProviderCapabilities {
	caps := e.caps
	caps.Model = strings.TrimSpace(e.model)
	return caps
}
