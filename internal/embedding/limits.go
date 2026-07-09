package embedding

import (
	"strings"

	"github.com/docgraph/docgraph/internal/config"
)

const (
	defaultResolvedChunkTargetTokens = 512
	defaultResolvedBatchSize         = 64
	maxResolvedBatchSize             = 200
	metadataTokenReserve             = 16
)

type ResolvedEmbeddingLimits struct {
	ContextTokens      int    `json:"context_tokens,omitempty"`
	ChunkTargetTokens  int    `json:"chunk_target_tokens"`
	ChunkOverlapTokens int    `json:"chunk_overlap_tokens"`
	MaxBatchTokens     int    `json:"max_batch_tokens"`
	BatchSize          int    `json:"batch_size"`
	Dimensions         int    `json:"dimensions,omitempty"`
	UsageAvailable     bool   `json:"usage_available,omitempty"`
	ContextSource      string `json:"context_source,omitempty"`
	ChunkTargetSource  string `json:"chunk_target_source,omitempty"`
	MaxBatchSource     string `json:"max_batch_source,omitempty"`
	DimensionsSource   string `json:"dimensions_source,omitempty"`
}

func ResolveEmbeddingLimits(cfg config.EmbeddingConfig, caps ProviderCapabilities) ResolvedEmbeddingLimits {
	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = defaultResolvedBatchSize
	}
	if batchSize > maxResolvedBatchSize {
		batchSize = maxResolvedBatchSize
	}

	limits := ResolvedEmbeddingLimits{
		BatchSize:      batchSize,
		UsageAvailable: caps.UsageAvailable,
	}
	if cfg.Dimensions > 0 {
		limits.Dimensions = cfg.Dimensions
		limits.DimensionsSource = "config"
	} else if caps.Dimensions > 0 {
		limits.Dimensions = caps.Dimensions
		limits.DimensionsSource = "provider_probe"
	}

	if cfg.ContextTokens > 0 {
		limits.ContextTokens = cfg.ContextTokens
		limits.ContextSource = "config"
	} else if caps.ContextTokens > 0 {
		limits.ContextTokens = caps.ContextTokens
		limits.ContextSource = "provider_probe"
	}

	switch {
	case cfg.ChunkTargetTokens > 0:
		limits.ChunkTargetTokens = cfg.ChunkTargetTokens
		limits.ChunkTargetSource = "config"
	case cfg.MaxInputTokens > 0:
		limits.ChunkTargetTokens = cfg.MaxInputTokens
		limits.ChunkTargetSource = "config_legacy_max_input_tokens"
	case limits.ContextTokens > 0:
		limits.ChunkTargetTokens = limits.ContextTokens * 3 / 5
		limits.ChunkTargetSource = limits.ContextSource
	default:
		limits.ChunkTargetTokens = defaultResolvedChunkTargetTokens
		limits.ChunkTargetSource = "default"
	}
	limits.ChunkTargetTokens = clampChunkTargetToContext(limits.ChunkTargetTokens, limits.ContextTokens)

	maxOverlap := limits.ChunkTargetTokens / 5
	if cfg.ChunkOverlapTokens > 0 {
		limits.ChunkOverlapTokens = cfg.ChunkOverlapTokens
		if limits.ChunkOverlapTokens > maxOverlap {
			limits.ChunkOverlapTokens = maxOverlap
		}
	} else {
		limits.ChunkOverlapTokens = maxOverlap / 2
	}
	if limits.ChunkOverlapTokens < 0 {
		limits.ChunkOverlapTokens = 0
	}

	if cfg.MaxBatchTokens > 0 {
		limits.MaxBatchTokens = cfg.MaxBatchTokens
		limits.MaxBatchSource = "config"
	} else if limits.ContextTokens > 0 {
		limits.MaxBatchTokens = limits.ContextTokens * 3 / 4
		limits.MaxBatchSource = limits.ContextSource
	} else {
		limits.MaxBatchTokens = limits.ChunkTargetTokens * minInt(limits.BatchSize, 8)
		limits.MaxBatchSource = "default"
	}
	if limits.MaxBatchTokens < 1 {
		limits.MaxBatchTokens = 1
	}
	return limits
}

func ResolveEmbeddingLimitsForEmbedder(cfg config.EmbeddingConfig, embedder Embedder) ResolvedEmbeddingLimits {
	caps := ProviderCapabilities{}
	if embedder != nil {
		caps.Model = embedder.Model()
		caps.Dimensions = embedder.Dimensions()
		if provider, ok := embedder.(interface{ Capabilities() ProviderCapabilities }); ok {
			caps = mergeProviderCapabilities(caps, provider.Capabilities())
		}
	}
	return ResolveEmbeddingLimits(cfg, caps)
}

func (l ResolvedEmbeddingLimits) TightenFromContextExceeded(err error) ResolvedEmbeddingLimits {
	contextTokens := ContextLimitFromError(err)
	if contextTokens <= 0 {
		return l
	}
	if l.ContextTokens == 0 || contextTokens < l.ContextTokens {
		l.ContextTokens = contextTokens
		l.ContextSource = "context_exceeded"
	}
	l.ChunkTargetTokens = clampChunkTargetToContext(l.ChunkTargetTokens, l.ContextTokens)
	if l.MaxBatchTokens <= 0 || l.MaxBatchTokens > l.ContextTokens*3/4 {
		l.MaxBatchTokens = l.ContextTokens * 3 / 4
		l.MaxBatchSource = "context_exceeded"
	}
	if l.MaxBatchTokens < 1 {
		l.MaxBatchTokens = 1
	}
	return l
}

func clampChunkTargetToContext(target int, contextTokens int) int {
	if target <= 0 {
		target = defaultResolvedChunkTargetTokens
	}
	if contextTokens > 0 {
		maxTarget := contextTokens - metadataTokenReserve
		if maxTarget < 64 {
			maxTarget = contextTokens
		}
		if maxTarget > 0 && target > maxTarget {
			target = maxTarget
		}
	}
	if target < 64 {
		target = 64
	}
	return target
}

func mergeProviderCapabilities(base ProviderCapabilities, override ProviderCapabilities) ProviderCapabilities {
	if strings.TrimSpace(override.Model) != "" {
		base.Model = override.Model
	}
	if override.Reachable {
		base.Reachable = true
	}
	if override.Dimensions > 0 {
		base.Dimensions = override.Dimensions
	}
	if override.UsageAvailable {
		base.UsageAvailable = true
	}
	if override.ContextTokens > 0 {
		base.ContextTokens = override.ContextTokens
	}
	return base
}
