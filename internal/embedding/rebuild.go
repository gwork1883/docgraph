package embedding

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/docgraph/docgraph/internal/config"
	"github.com/docgraph/docgraph/internal/domain"
	"github.com/docgraph/docgraph/internal/storage"
)

type EnsureOptions struct {
	SourceID           string
	BatchSize          int
	ContextTokens      int
	ChunkTargetTokens  int
	ChunkOverlapTokens int
	MaxBatchTokens     int
	Concurrency        int
	Limits             ResolvedEmbeddingLimits
	Tokenizer          string
	ChunkStrategy      string
	GeneratorVersion   string
	OnProgress         func(EnsureProgress) error
}

type EnsureProgress struct {
	ScannedSections  int
	EmbeddedSections int
	SkippedSections  int
	DeferredSections int
	PendingSections  int
	ScannedChunks    int
	EmbeddedChunks   int
	SkippedChunks    int
	DeferredChunks   int
	PendingChunks    int
}

type EnsureResult struct {
	ScannedSections  int
	EmbeddedSections int
	SkippedSections  int
	DeferredSections int
	ScannedChunks    int
	EmbeddedChunks   int
	SkippedChunks    int
	DeferredChunks   int
}

func Ensure(ctx context.Context, store storage.Store, embedder Embedder, opts EnsureOptions) (EnsureResult, error) {
	if store == nil {
		return EnsureResult{}, fmt.Errorf("store is required")
	}
	if embedder == nil || strings.TrimSpace(embedder.Model()) == "" {
		return EnsureResult{}, fmt.Errorf("embedding provider is disabled")
	}
	limits := normalizeEnsureLimits(opts)
	batchSize := limits.BatchSize
	generatorVersion := strings.TrimSpace(opts.GeneratorVersion)
	if generatorVersion == "" {
		generatorVersion = DefaultGeneratorVersion
	}
	measurer := NewTextMeasurer(opts.Tokenizer, embedder.Model())
	chunker := Chunker{
		Measurer:           measurer,
		Strategy:           opts.ChunkStrategy,
		ContextTokens:      limits.ContextTokens,
		ChunkTargetTokens:  limits.ChunkTargetTokens,
		ChunkOverlapTokens: limits.ChunkOverlapTokens,
	}
	chunker = chunker.Normalized()
	maxBatchTokens := limits.MaxBatchTokens

	existing, err := existingChunkHashes(ctx, store, embedder.Model())
	if err != nil {
		return EnsureResult{}, err
	}
	result := EnsureResult{}
	offset := 0
	for {
		sections, err := store.ListSectionsForEmbedding(ctx, opts.SourceID, batchSize, offset)
		if err != nil {
			return result, err
		}
		if len(sections) == 0 {
			return result, nil
		}
		result.ScannedSections += len(sections)
		pending := make([]domain.EmbeddingChunkInput, 0)
		pendingSections := map[string]bool{}
		for _, section := range sections {
			chunks, err := chunker.BuildChunks(section, embedder.Model(), generatorVersion)
			if err != nil {
				return result, err
			}
			result.ScannedChunks += len(chunks)
			current := true
			for _, chunk := range chunks {
				hash := existing[chunk.ChunkID]
				if !embeddingChunkHashCurrent(hash, chunk, section, embedder.Model(), generatorVersion) &&
					!retrySplitChunksCurrent(existing, chunk, section, embedder.Model(), generatorVersion) {
					current = false
					break
				}
			}
			if current && len(chunks) > 0 {
				result.SkippedSections++
				result.SkippedChunks += len(chunks)
				continue
			}
			pendingSections[section.SectionID] = true
			deleteTokenizer, deleteStrategy := "", ""
			if len(chunks) > 0 {
				deleteTokenizer = chunks[0].Tokenizer
				deleteStrategy = chunks[0].ChunkStrategy
			}
			if err := store.DeleteEmbeddingChunksBySection(ctx, section.SectionID, embedder.Model(), generatorVersion, deleteTokenizer, deleteStrategy); err != nil {
				return result, err
			}
			for _, chunk := range chunks {
				pending = append(pending, domain.EmbeddingChunkInput{
					ChunkID:            chunk.ChunkID,
					SectionID:          chunk.SectionID,
					DocumentID:         chunk.DocumentID,
					SourceID:           chunk.SourceID,
					ChunkOrdinal:       chunk.ChunkOrdinal,
					ChunkStartToken:    chunk.ChunkStartToken,
					ChunkTokenCount:    chunk.ChunkTokenCount,
					ChunkText:          chunk.Text,
					Model:              embedder.Model(),
					SectionContentHash: chunk.SectionContentHash,
					ChunkTextHash:      chunk.TextHash,
					Tokenizer:          chunk.Tokenizer,
					ChunkStrategy:      chunk.ChunkStrategy,
					GeneratorVersion:   generatorVersion,
				})
			}
		}
		if len(pending) > 0 {
			if opts.OnProgress != nil {
				if err := opts.OnProgress(EnsureProgress{
					ScannedSections:  result.ScannedSections,
					EmbeddedSections: result.EmbeddedSections,
					SkippedSections:  result.SkippedSections,
					PendingSections:  len(pendingSections),
					ScannedChunks:    result.ScannedChunks,
					EmbeddedChunks:   result.EmbeddedChunks,
					SkippedChunks:    result.SkippedChunks,
					PendingChunks:    len(pending),
				}); err != nil {
					return result, err
				}
			}
			embedded, err := embedAndStoreChunks(ctx, store, embedder, pending, batchSize, maxBatchTokens, opts.Concurrency)
			if err != nil {
				return result, err
			}
			deferredSections := sectionIDsForChunks(embedded.Deferred)
			result.EmbeddedChunks += embedded.Embedded
			result.DeferredChunks += len(embedded.Deferred)
			result.EmbeddedSections += len(pendingSections) - len(deferredSections)
			result.DeferredSections += len(deferredSections)
		}
		offset += len(sections)
		if opts.OnProgress != nil {
			if err := opts.OnProgress(EnsureProgress{
				ScannedSections:  result.ScannedSections,
				EmbeddedSections: result.EmbeddedSections,
				SkippedSections:  result.SkippedSections,
				DeferredSections: result.DeferredSections,
				PendingSections:  0,
				ScannedChunks:    result.ScannedChunks,
				EmbeddedChunks:   result.EmbeddedChunks,
				SkippedChunks:    result.SkippedChunks,
				DeferredChunks:   result.DeferredChunks,
				PendingChunks:    0,
			}); err != nil {
				return result, err
			}
		}
	}
}

func normalizeEnsureLimits(opts EnsureOptions) ResolvedEmbeddingLimits {
	if opts.Limits.ChunkTargetTokens > 0 && opts.Limits.BatchSize > 0 && opts.Limits.MaxBatchTokens > 0 {
		return opts.Limits
	}
	return ResolveEmbeddingLimits(configFromEnsureOptions(opts), ProviderCapabilities{})
}

func configFromEnsureOptions(opts EnsureOptions) config.EmbeddingConfig {
	return config.EmbeddingConfig{
		ContextTokens:      opts.ContextTokens,
		ChunkTargetTokens:  opts.ChunkTargetTokens,
		ChunkOverlapTokens: opts.ChunkOverlapTokens,
		MaxBatchTokens:     opts.MaxBatchTokens,
		BatchSize:          opts.BatchSize,
	}
}

func existingChunkHashes(ctx context.Context, store storage.Store, model string) (map[string]domain.EmbeddingChunkHash, error) {
	result := map[string]domain.EmbeddingChunkHash{}
	offset := 0
	for {
		hashes, err := store.ListEmbeddingChunkHashes(ctx, model, 500, offset)
		if err != nil {
			return nil, err
		}
		if len(hashes) == 0 {
			return result, nil
		}
		for _, hash := range hashes {
			result[hash.ChunkID] = hash
		}
		offset += len(hashes)
	}
}

func sectionIDsForChunks(chunks []domain.EmbeddingChunkInput) map[string]bool {
	out := map[string]bool{}
	for _, chunk := range chunks {
		if strings.TrimSpace(chunk.SectionID) != "" {
			out[chunk.SectionID] = true
		}
	}
	return out
}

func embeddingChunkHashCurrent(hash domain.EmbeddingChunkHash, chunk EmbeddingChunk, section domain.EmbeddingSection, model string, generatorVersion string) bool {
	return hash.ChunkID != "" &&
		hash.DocumentID == section.DocumentID &&
		hash.SourceID == section.SourceID &&
		hash.Model == model &&
		hash.SectionContentHash == section.ContentHash &&
		hash.ChunkTextHash == chunk.TextHash &&
		hash.Tokenizer == chunk.Tokenizer &&
		hash.ChunkStrategy == chunk.ChunkStrategy &&
		hash.GeneratorVersion == generatorVersion
}

func retrySplitChunksCurrent(existing map[string]domain.EmbeddingChunkHash, chunk EmbeddingChunk, section domain.EmbeddingSection, model string, generatorVersion string) bool {
	pending := domain.EmbeddingChunkInput{
		ChunkID:            chunk.ChunkID,
		SectionID:          chunk.SectionID,
		DocumentID:         chunk.DocumentID,
		SourceID:           chunk.SourceID,
		ChunkOrdinal:       chunk.ChunkOrdinal,
		ChunkStartToken:    chunk.ChunkStartToken,
		ChunkTokenCount:    chunk.ChunkTokenCount,
		ChunkText:          chunk.Text,
		Model:              model,
		SectionContentHash: chunk.SectionContentHash,
		ChunkTextHash:      chunk.TextHash,
		Tokenizer:          chunk.Tokenizer,
		ChunkStrategy:      chunk.ChunkStrategy,
		GeneratorVersion:   generatorVersion,
	}
	split := splitPendingChunkForRetry(pending)
	if len(split) == 0 {
		return false
	}
	for _, part := range split {
		hash := existing[part.ChunkID]
		if hash.ChunkID == "" ||
			hash.DocumentID != section.DocumentID ||
			hash.SourceID != section.SourceID ||
			hash.Model != model ||
			hash.SectionContentHash != section.ContentHash ||
			hash.ChunkTextHash != part.ChunkTextHash ||
			hash.Tokenizer != part.Tokenizer ||
			hash.ChunkStrategy != part.ChunkStrategy ||
			hash.GeneratorVersion != generatorVersion {
			return false
		}
	}
	return true
}

type embedAndStoreResult struct {
	Embedded int
	Deferred []domain.EmbeddingChunkInput
}

func embedAndStoreChunks(ctx context.Context, store storage.Store, embedder Embedder, chunks []domain.EmbeddingChunkInput, batchSize int, maxBatchTokens int, concurrency int) (embedAndStoreResult, error) {
	batches := planEmbeddingChunkBatches(chunks, batchSize, maxBatchTokens)
	if concurrency <= 1 || len(batches) <= 1 {
		result := embedAndStoreResult{}
		for _, batch := range batches {
			start, end := batch.Start, batch.End
			if err := embedAndStoreChunkBatch(ctx, store, embedder, chunks[start:end], maxBatchTokens); err != nil {
				if IsContextExceeded(err) && end-start > 1 {
					mid := start + ((end - start) / 2)
					nextMaxBatchTokens := tightenedRetryMaxBatchTokens(maxBatchTokens, err)
					leftEmbedded, err := embedAndStoreChunks(ctx, store, embedder, chunks[start:mid], batchSize, nextMaxBatchTokens, 1)
					if err != nil {
						return result, err
					}
					result.Embedded += leftEmbedded.Embedded
					result.Deferred = append(result.Deferred, leftEmbedded.Deferred...)
					rightEmbedded, err := embedAndStoreChunks(ctx, store, embedder, chunks[mid:end], batchSize, nextMaxBatchTokens, 1)
					if err != nil {
						return result, err
					}
					result.Embedded += rightEmbedded.Embedded
					result.Deferred = append(result.Deferred, rightEmbedded.Deferred...)
				} else if IsContextExceeded(err) && end-start == 1 {
					split := splitPendingChunkForRetry(chunks[start])
					if len(split) <= 1 {
						return result, err
					}
					nextMaxBatchTokens := tightenedRetryMaxBatchTokens(maxBatchTokens, err)
					if nextMaxBatchTokens <= 0 {
						nextMaxBatchTokens = maxInt(1, chunks[start].ChunkTokenCount/2)
					}
					splitEmbedded, err := embedAndStoreChunks(ctx, store, embedder, split, 1, nextMaxBatchTokens, 1)
					if err != nil {
						return result, err
					}
					result.Embedded += splitEmbedded.Embedded
					result.Deferred = append(result.Deferred, splitEmbedded.Deferred...)
				} else if ctx.Err() == nil && IsTransientEmbeddingError(err) {
					result.Deferred = append(result.Deferred, chunks[start:end]...)
				} else {
					return result, err
				}
			} else {
				result.Embedded += end - start
			}
			start = end
		}
		return result, nil
	}
	if concurrency > len(batches) {
		concurrency = len(batches)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type batchResult struct {
		embedded int
		deferred []domain.EmbeddingChunkInput
		err      error
	}
	jobs := make(chan embeddingChunkBatch)
	results := make(chan batchResult, len(batches))
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for batch := range jobs {
				start, end := batch.Start, batch.End
				embedded := 0
				if err := embedAndStoreChunkBatch(ctx, store, embedder, chunks[start:end], maxBatchTokens); err != nil {
					if IsContextExceeded(err) {
						count, retryErr := embedAndStoreChunks(ctx, store, embedder, chunks[start:end], 1, tightenedRetryMaxBatchTokens(maxBatchTokens, err), 1)
						if retryErr != nil {
							results <- batchResult{err: retryErr}
							cancel()
							continue
						}
						embedded += count.Embedded
						results <- batchResult{embedded: embedded, deferred: count.Deferred}
						continue
					} else if ctx.Err() == nil && IsTransientEmbeddingError(err) {
						results <- batchResult{deferred: append([]domain.EmbeddingChunkInput{}, chunks[start:end]...)}
						continue
					} else {
						results <- batchResult{err: err}
						cancel()
						continue
					}
				} else {
					embedded += end - start
				}
				results <- batchResult{embedded: embedded}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, batch := range batches {
			select {
			case <-ctx.Done():
				return
			case jobs <- batch:
			}
		}
	}()
	wg.Wait()
	close(results)
	result := embedAndStoreResult{}
	for item := range results {
		if item.err != nil {
			return embedAndStoreResult{}, item.err
		}
		result.Embedded += item.embedded
		result.Deferred = append(result.Deferred, item.deferred...)
	}
	if err := ctx.Err(); err != nil && err != context.Canceled {
		return result, err
	}
	return result, nil
}

func tightenedRetryMaxBatchTokens(maxBatchTokens int, err error) int {
	if contextTokens := ContextLimitFromError(err); contextTokens > 0 {
		limits := ResolvedEmbeddingLimits{MaxBatchTokens: maxBatchTokens}.TightenFromContextExceeded(err)
		if limits.MaxBatchTokens > 0 {
			return limits.MaxBatchTokens
		}
	}
	next := maxBatchTokens / 2
	if next <= 0 {
		next = 1
	}
	return next
}

func splitPendingChunkForRetry(chunk domain.EmbeddingChunkInput) []domain.EmbeddingChunkInput {
	text := strings.TrimSpace(chunk.ChunkText)
	if utf8.RuneCountInString(text) < 128 {
		return nil
	}
	runes := []rune(text)
	mid := len(runes) / 2
	leftText := strings.TrimSpace(string(runes[:mid]))
	rightText := strings.TrimSpace(string(runes[mid:]))
	if leftText == "" || rightText == "" {
		return nil
	}
	left := chunk
	right := chunk
	left.ChunkOrdinal = chunk.ChunkOrdinal*1000000 + 500000
	right.ChunkOrdinal = chunk.ChunkOrdinal*1000000 + 500001
	left.ChunkText = leftText
	right.ChunkText = rightText
	left.ChunkTextHash = HashText(leftText)
	right.ChunkTextHash = HashText(rightText)
	left.ChunkID = BuildChunkIDForPlan(chunk.SectionID, left.ChunkOrdinal, chunk.Model, chunk.GeneratorVersion, chunk.Tokenizer, chunk.ChunkStrategy, left.ChunkTextHash)
	right.ChunkID = BuildChunkIDForPlan(chunk.SectionID, right.ChunkOrdinal, chunk.Model, chunk.GeneratorVersion, chunk.Tokenizer, chunk.ChunkStrategy, right.ChunkTextHash)
	left.ChunkTokenCount = maxInt(1, chunk.ChunkTokenCount/2)
	right.ChunkStartToken = chunk.ChunkStartToken + left.ChunkTokenCount
	right.ChunkTokenCount = maxInt(1, chunk.ChunkTokenCount-left.ChunkTokenCount)
	left.Embedding = nil
	right.Embedding = nil
	left.Dimensions = 0
	right.Dimensions = 0
	return []domain.EmbeddingChunkInput{left, right}
}

type embeddingChunkBatch struct {
	Start  int
	End    int
	Tokens int
}

func planEmbeddingChunkBatches(chunks []domain.EmbeddingChunkInput, batchSize int, maxBatchTokens int) []embeddingChunkBatch {
	if batchSize <= 0 {
		batchSize = 1
	}
	batches := make([]embeddingChunkBatch, 0)
	for start := 0; start < len(chunks); {
		end := start
		tokens := 0
		for end < len(chunks) && end-start < batchSize {
			nextTokens := chunks[end].ChunkTokenCount
			if nextTokens <= 0 {
				nextTokens = ConservativeTextMeasurer{}.CountTokens(chunks[end].ChunkText)
			}
			if end > start && maxBatchTokens > 0 && tokens+nextTokens > maxBatchTokens {
				break
			}
			tokens += nextTokens
			end++
		}
		if end == start {
			nextTokens := chunks[start].ChunkTokenCount
			if nextTokens <= 0 {
				nextTokens = ConservativeTextMeasurer{}.CountTokens(chunks[start].ChunkText)
			}
			end = start + 1
			tokens = nextTokens
		}
		batches = append(batches, embeddingChunkBatch{Start: start, End: end, Tokens: tokens})
		start = end
	}
	return batches
}

func embedAndStoreChunkBatch(ctx context.Context, store storage.Store, embedder Embedder, chunks []domain.EmbeddingChunkInput, maxBatchTokens int) error {
	texts := make([]string, len(chunks))
	for i := range chunks {
		texts[i] = chunks[i].ChunkText
	}

	// Exponential backoff retry for rate-limit (429) errors.
	const maxRetries = 5
	const baseDelay = 2 * time.Second
	const maxDelay = 60 * time.Second

	var vectors [][]float32
	var err error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		vectors, err = embedder.Embed(ctx, texts)
		if err == nil {
			break
		}
		if !IsRateLimitError(err) {
			break // non-rate-limit error: don't retry
		}
		if attempt >= maxRetries {
			break // exhausted retries
		}
		// Compute backoff: use provider's Retry-After hint if available,
		// otherwise exponential delay capped at maxDelay.
		delay := RateLimitRetryAfter(err)
		if delay <= 0 {
			delay = baseDelay * time.Duration(1<<uint(attempt)) // 2s, 4s, 8s, 16s, 32s
			if delay > maxDelay {
				delay = maxDelay
			}
		} else if delay > maxDelay {
			delay = maxDelay
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}

	if err != nil {
		if IsContextExceeded(err) {
			chunk := chunks[0]
			return fmt.Errorf("embed chunks context exceeded chunk_id=%s tokens=%d model=%s context=%d max_batch_tokens=%d: %w", chunk.ChunkID, chunk.ChunkTokenCount, embedder.Model(), ContextLimitFromError(err), maxBatchTokens, err)
		}
		if IsRateLimitError(err) {
			return fmt.Errorf("embed chunks rate limited after %d retries batch_size=%d max_batch_tokens=%d: %w", maxRetries, len(texts), maxBatchTokens, err)
		}
		return fmt.Errorf("embed chunks batch_size=%d max_batch_tokens=%d: %w", len(texts), maxBatchTokens, err)
	}
	if len(vectors) != len(chunks) {
		return fmt.Errorf("embedding response count=%d, want %d", len(vectors), len(chunks))
	}
	for i := range chunks {
		chunk := chunks[i]
		chunk.Dimensions = len(vectors[i])
		chunk.Embedding = vectors[i]
		if err := store.UpsertEmbeddingChunk(ctx, chunk); err != nil {
			return err
		}
	}
	return nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
