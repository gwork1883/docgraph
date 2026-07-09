package embedding

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/docgraph/docgraph/internal/config"
	"github.com/docgraph/docgraph/internal/domain"
)

func TestNeedsEnsureDetectsEmbeddingTextHashAndGeneratorChanges(t *testing.T) {
	section := domain.EmbeddingSection{
		SectionID:     "section-1",
		DocumentID:    "doc-1",
		SourceID:      "source-1",
		SourceName:    "Docs",
		ProductHint:   "DocGraph",
		ModuleHint:    "Search",
		DocumentTitle: "Hybrid Search",
		HeadingPath:   "Search > Vector",
		Title:         "Vector Trace",
		Content:       "Vector hits must be traceable.",
		ContentHash:   "hash-content",
	}
	textHash := HashText(BuildContent(section))
	current := domain.SectionEmbeddingHash{
		SectionID:         section.SectionID,
		DocumentID:        section.DocumentID,
		SourceID:          section.SourceID,
		ContentHash:       section.ContentHash,
		EmbeddingTextHash: textHash,
		GeneratorVersion:  "generator-v1",
	}
	if NeedsEnsure(section, current, "generator-v1") {
		t.Fatal("NeedsEnsure returned true for current embedding")
	}
	staleText := current
	staleText.EmbeddingTextHash = "old-text-hash"
	if !NeedsEnsure(section, staleText, "generator-v1") {
		t.Fatal("NeedsEnsure returned false for changed embedding text hash")
	}
	staleGenerator := current
	if !NeedsEnsure(section, staleGenerator, "generator-v2") {
		t.Fatal("NeedsEnsure returned false for changed generator version")
	}
}

func TestOpenAICompatibleEmbedderAllowsLocalServiceWithoutAPIKey(t *testing.T) {
	var gotAuthorization string
	embedder, err := NewOpenAICompatibleEmbedder(config.EmbeddingConfig{
		Model:  "local-embedding",
		APIURL: "http://embedding.local/v1",
	})
	if err != nil {
		t.Fatalf("NewOpenAICompatibleEmbedder returned error: %v", err)
	}
	embedder.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotAuthorization = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/embeddings" {
			t.Fatalf("path = %q, want /v1/embeddings", r.URL.Path)
		}
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Model != "local-embedding" || len(req.Input) != 1 || req.Input[0] != "hello" {
			t.Fatalf("request = %+v, want local model and input", req)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"data":[{"index":0,"embedding":[1,0]}]}`)),
			Header:     make(http.Header),
		}, nil
	})}
	vectors, err := embedder.Embed(context.Background(), []string{"hello"})
	if err != nil {
		t.Fatalf("Embed returned error: %v", err)
	}
	if gotAuthorization != "" {
		t.Fatalf("Authorization header = %q, want empty for local service without api key", gotAuthorization)
	}
	if len(vectors) != 1 || len(vectors[0]) != 2 || vectors[0][0] != 1 {
		t.Fatalf("vectors = %#v, want one embedding", vectors)
	}
}

func TestOpenAICompatibleEmbedderClassifiesContextExceeded(t *testing.T) {
	embedder, err := NewOpenAICompatibleEmbedder(config.EmbeddingConfig{
		Model:  "local-embedding",
		APIURL: "http://embedding.local/v1",
	})
	if err != nil {
		t.Fatalf("NewOpenAICompatibleEmbedder returned error: %v", err)
	}
	embedder.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"This model's maximum context length is 2048 tokens. However, you requested 4096 tokens."}}`)),
			Header:     make(http.Header),
		}, nil
	})}

	_, err = embedder.Embed(context.Background(), []string{strings.Repeat("x", 4096)})
	if err == nil {
		t.Fatal("Embed returned nil error, want context exceeded")
	}
	if !IsContextExceeded(err) {
		t.Fatalf("IsContextExceeded(%v) = false, want true", err)
	}
	if got := ContextLimitFromError(err); got != 2048 {
		t.Fatalf("ContextLimitFromError = %d, want 2048", got)
	}
}

func TestOpenAICompatibleEmbedderProbeSetsDimensionsWhenZero(t *testing.T) {
	embedder, err := NewOpenAICompatibleEmbedder(config.EmbeddingConfig{
		Model:      "local-embedding",
		APIURL:     "http://embedding.local/v1",
		Dimensions: 0,
	})
	if err != nil {
		t.Fatalf("NewOpenAICompatibleEmbedder returned error: %v", err)
	}
	embedder.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"data":[{"index":0,"embedding":[1,0,0]}]}`)),
			Header:     make(http.Header),
		}, nil
	})}

	caps, err := embedder.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe returned error: %v", err)
	}
	if caps.Dimensions != 3 || embedder.Dimensions() != 3 {
		t.Fatalf("probe dimensions = %d embedder=%d, want 3", caps.Dimensions, embedder.Dimensions())
	}
}

func TestOpenAICompatibleEmbedderProbeRecordsUsageAvailability(t *testing.T) {
	embedder, err := NewOpenAICompatibleEmbedder(config.EmbeddingConfig{
		Model:  "local-embedding",
		APIURL: "http://embedding.local/v1",
	})
	if err != nil {
		t.Fatalf("NewOpenAICompatibleEmbedder returned error: %v", err)
	}
	embedder.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"data":[{"index":0,"embedding":[1,0,0]}],"usage":{"prompt_tokens":7,"total_tokens":7}}`)),
			Header:     make(http.Header),
		}, nil
	})}

	caps, err := embedder.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe returned error: %v", err)
	}
	if !caps.UsageAvailable {
		t.Fatalf("caps = %+v, want usage available", caps)
	}
	if got := embedder.Capabilities(); !got.UsageAvailable || got.Dimensions != 3 {
		t.Fatalf("Capabilities = %+v, want usage and dimensions", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestNormalizeL2(t *testing.T) {
	// Zero vector should remain zero
	result := normalizeL2([]float32{0, 0, 0})
	for _, v := range result {
		if v != 0 {
			t.Fatalf("zero vector normalized to non-zero: %v", result)
		}
	}

	// Unit vector should stay unit
	result = normalizeL2([]float32{1, 0, 0})
	if result[0] != 1 || result[1] != 0 || result[2] != 0 {
		t.Fatalf("unit vector changed: %v", result)
	}

	// Non-unit vector should normalize to unit length
	result = normalizeL2([]float32{3, 4, 0})
	var sum float64
	for _, v := range result {
		sum += float64(v) * float64(v)
	}
	if math.Abs(sum-1.0) > 1e-6 {
		t.Fatalf("normalized vector not unit length: squared norm = %v", sum)
	}
	if math.Abs(float64(result[0])-0.6) > 1e-6 || math.Abs(float64(result[1])-0.8) > 1e-6 {
		t.Fatalf("normalized values wrong: %v", result)
	}
}

func TestParseRetryAfterHeader(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  time.Duration
	}{
		{"empty", "", 0},
		{"integer seconds", "30", 30 * time.Second},
		{"zero", "0", 0},
		{"large integer", "120", 120 * time.Second},
		{"unparseable", "not-a-number", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseRetryAfterHeader(tt.input)
			if got != tt.want {
				t.Errorf("parseRetryAfterHeader(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestOpenAICompatibleEmbedderReturnsRateLimitError(t *testing.T) {
	embedder, err := NewOpenAICompatibleEmbedder(config.EmbeddingConfig{
		Model:  "local-embedding",
		APIURL: "http://embedding.local/v1",
	})
	if err != nil {
		t.Fatalf("NewOpenAICompatibleEmbedder returned error: %v", err)
	}
	headers := make(http.Header)
	headers.Set("Retry-After", "5")
	embedder.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Body:       io.NopCloser(strings.NewReader(`{"detail":"请求太频繁"}`)),
			Header:     headers,
		}, nil
	})}

	_, err = embedder.Embed(context.Background(), []string{"hello"})
	if err == nil {
		t.Fatal("Embed returned nil error, want rate limit error")
	}
	if !IsRateLimitError(err) {
		t.Fatalf("IsRateLimitError(%v) = false, want true", err)
	}
	if !IsTransientEmbeddingError(err) {
		t.Fatalf("IsTransientEmbeddingError(%v) = false, want true (429 is transient)", err)
	}
	var rlErr *RateLimitError
	if !errors.As(err, &rlErr) {
		t.Fatalf("expected RateLimitError, got %T: %v", err, err)
	}
	if rlErr.StatusCode != 429 {
		t.Fatalf("RateLimitError.StatusCode = %d, want 429", rlErr.StatusCode)
	}
	if rlErr.RetryAfter != 5*time.Second {
		t.Fatalf("RateLimitError.RetryAfter = %v, want 5s", rlErr.RetryAfter)
	}
}

func TestOpenAICompatibleEmbedderRateLimitWithoutRetryAfter(t *testing.T) {
	embedder, err := NewOpenAICompatibleEmbedder(config.EmbeddingConfig{
		Model:  "local-embedding",
		APIURL: "http://embedding.local/v1",
	})
	if err != nil {
		t.Fatalf("NewOpenAICompatibleEmbedder returned error: %v", err)
	}
	embedder.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Body:       io.NopCloser(strings.NewReader(`{"error":"rate limited"}`)),
			Header:     make(http.Header), // no Retry-After header
		}, nil
	})}

	_, err = embedder.Embed(context.Background(), []string{"hello"})
	if !IsRateLimitError(err) {
		t.Fatalf("IsRateLimitError(%v) = false, want true", err)
	}
	var rlErr *RateLimitError
	if !errors.As(err, &rlErr) {
		t.Fatalf("expected RateLimitError, got %T", err)
	}
	if rlErr.RetryAfter != 0 {
		t.Fatalf("RateLimitError.RetryAfter = %v, want 0 (no header)", rlErr.RetryAfter)
	}
}

func TestOpenAICompatibleEmbedderTruncatesMRLVector(t *testing.T) {
	// Simulate a model that returns 2560-dim vectors but config says 1024
	embedder, err := NewOpenAICompatibleEmbedder(config.EmbeddingConfig{
		Model:      "qwen3-embedding-4b",
		APIURL:     "http://embedding.local/v1",
		Dimensions: 3, // request 3 dims
	})
	if err != nil {
		t.Fatalf("NewOpenAICompatibleEmbedder returned error: %v", err)
	}
	// Build a 5-dim vector that is L2-normalized at full length
	// v = [0.4, 0.4, 0.4, 0.4, 0.4] has norm sqrt(0.8) ≈ 0.894
	// After truncation to 3 dims: [0.4, 0.4, 0.4] norm = sqrt(0.48) ≈ 0.693
	// After re-normalize: [0.4/0.693, 0.4/0.693, 0.4/0.693] ≈ [0.577, 0.577, 0.577]
	fullVec := make([]float32, 5)
	for i := range fullVec {
		fullVec[i] = 0.4
	}
	// Build JSON embedding response with 5-dim vector
	embeddingJSON, _ := json.Marshal(struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}{
		Data: []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}{
			{Index: 0, Embedding: fullVec},
		},
	})
	respBody := string(embeddingJSON)
	embedder.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(respBody)),
			Header:     make(http.Header),
		}, nil
	})}

	vectors, err := embedder.Embed(context.Background(), []string{"test"})
	if err != nil {
		t.Fatalf("Embed returned error: %v", err)
	}
	if len(vectors) != 1 || len(vectors[0]) != 3 {
		t.Fatalf("vector length = %d, want 3", len(vectors[0]))
	}
	// Check L2 norm = 1
	var sum float64
	for _, v := range vectors[0] {
		sum += float64(v) * float64(v)
	}
	if math.Abs(sum-1.0) > 1e-6 {
		t.Fatalf("truncated+normalized vector not unit length: squared norm = %v", sum)
	}
}
