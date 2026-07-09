package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docgraph/docgraph/internal/config"
	"github.com/docgraph/docgraph/internal/domain"
	"github.com/docgraph/docgraph/internal/embeddingtext"
)

const DefaultGeneratorVersion = DefaultChunkGeneratorVersion

type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Model() string
	Dimensions() int
}

type ProviderCapabilities struct {
	Reachable      bool
	Model          string
	Dimensions     int
	UsageAvailable bool
	ContextTokens  int
}

type ContextExceededError struct {
	Model         string
	ContextTokens int
	InputTokens   int
	Body          string
}

func (e *ContextExceededError) Error() string {
	parts := []string{"embedding input exceeds provider context"}
	if e.Model != "" {
		parts = append(parts, "model="+e.Model)
	}
	if e.InputTokens > 0 {
		parts = append(parts, fmt.Sprintf("input_tokens=%d", e.InputTokens))
	}
	if e.ContextTokens > 0 {
		parts = append(parts, fmt.Sprintf("context_tokens=%d", e.ContextTokens))
	}
	if e.Body != "" {
		parts = append(parts, "body="+truncateForError(e.Body, 240))
	}
	return strings.Join(parts, " ")
}

func IsContextExceeded(err error) bool {
	var target *ContextExceededError
	return errors.As(err, &target)
}

// RateLimitError represents an HTTP 429 Too Many Requests response from the
// embedding provider. It carries the Retry-After header value (in seconds)
// if the provider included one.
type RateLimitError struct {
	StatusCode int
	Body       string
	RetryAfter time.Duration // 0 if the header was absent or unparseable
}

func (e *RateLimitError) Error() string {
	parts := []string{"embedding rate limited"}
	if e.StatusCode > 0 {
		parts = append(parts, fmt.Sprintf("status=%d", e.StatusCode))
	}
	if e.RetryAfter > 0 {
		parts = append(parts, fmt.Sprintf("retry_after=%ds", int(e.RetryAfter.Seconds())))
	}
	if e.Body != "" {
		parts = append(parts, "body="+truncateForError(e.Body, 240))
	}
	return strings.Join(parts, " ")
}

func IsRateLimitError(err error) bool {
	var target *RateLimitError
	return errors.As(err, &target)
}

func RateLimitRetryAfter(err error) time.Duration {
	var target *RateLimitError
	if errors.As(err, &target) {
		return target.RetryAfter
	}
	return 0
}

func IsTransientEmbeddingError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if IsRateLimitError(err) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func ContextLimitFromError(err error) int {
	var target *ContextExceededError
	if errors.As(err, &target) {
		return target.ContextTokens
	}
	return 0
}

type NoOpEmbedder struct{}

func NewNoOpEmbedder() Embedder {
	return NoOpEmbedder{}
}

func (NoOpEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return nil, fmt.Errorf("embedding provider is disabled")
}

func (NoOpEmbedder) Model() string {
	return ""
}

func (NoOpEmbedder) Dimensions() int {
	return 0
}

func NewEmbedder(cfg config.EmbeddingConfig) (Embedder, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "", "none":
		return NewNoOpEmbedder(), nil
	case "openai", "openai-compatible":
		return NewOpenAICompatibleEmbedder(cfg)
	default:
		return nil, fmt.Errorf("unsupported embedding provider %q", cfg.Provider)
	}
}

type OpenAICompatibleEmbedder struct {
	apiURL           string
	apiKey           string
	model            string
	dimensions       int
	client           *http.Client
	mu               sync.Mutex
	usageAvailable   bool
	lastPromptTokens int
}

func NewOpenAICompatibleEmbedder(cfg config.EmbeddingConfig) (*OpenAICompatibleEmbedder, error) {
	apiURL := strings.TrimRight(strings.TrimSpace(cfg.APIURL), "/")
	if apiURL == "" {
		apiURL = "https://api.openai.com/v1"
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		return nil, fmt.Errorf("embedding.model is required for openai-compatible provider")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &OpenAICompatibleEmbedder{
		apiURL:     apiURL,
		apiKey:     strings.TrimSpace(cfg.APIKey),
		model:      model,
		dimensions: cfg.Dimensions,
		client:     &http.Client{Timeout: timeout},
	}, nil
}

func (e *OpenAICompatibleEmbedder) Model() string {
	return e.model
}

func (e *OpenAICompatibleEmbedder) Dimensions() int {
	return e.dimensions
}

func (e *OpenAICompatibleEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	payload := map[string]any{
		"model": e.model,
		"input": texts,
	}
	if e.dimensions > 0 {
		payload["dimensions"] = e.dimensions
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.apiURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusTooManyRequests {
			retryAfter := parseRetryAfterHeader(resp.Header.Get("Retry-After"))
			return nil, &RateLimitError{
				StatusCode: resp.StatusCode,
				Body:       string(data),
				RetryAfter: retryAfter,
			}
		}
		if isContextExceededResponse(resp.StatusCode, data) {
			return nil, parseContextExceededError(e.model, string(data))
		}
		return nil, fmt.Errorf("embedding request failed: status=%d body=%s", resp.StatusCode, truncateForError(string(data), 500))
	}
	var decoded struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
		Usage *struct {
			PromptTokens int `json:"prompt_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage,omitempty"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, err
	}
	out := make([][]float32, len(texts))
	for _, item := range decoded.Data {
		if item.Index < 0 || item.Index >= len(out) {
			continue
		}
		out[item.Index] = item.Embedding
	}
	for i, vector := range out {
		if len(vector) == 0 {
			return nil, fmt.Errorf("embedding response missing vector at index %d", i)
		}
		if e.dimensions > 0 && len(vector) > e.dimensions {
			// Matryoshka/MRL truncation: take the first e.dimensions elements
			// and L2-normalize so cosine similarity remains valid.
			truncated := normalizeL2(vector[:e.dimensions])
			out[i] = truncated
		} else if e.dimensions > 0 && len(vector) != e.dimensions {
			return nil, fmt.Errorf("embedding response dimensions=%d, want %d", len(vector), e.dimensions)
		}
		if e.dimensions == 0 {
			e.dimensions = len(vector)
		}
	}
	if decoded.Usage != nil {
		e.mu.Lock()
		e.usageAvailable = true
		e.lastPromptTokens = decoded.Usage.PromptTokens
		e.mu.Unlock()
	}
	return out, nil
}

func (e *OpenAICompatibleEmbedder) Probe(ctx context.Context) (ProviderCapabilities, error) {
	vectors, err := e.Embed(ctx, []string{"DocGraph embedding capability probe."})
	if err != nil {
		return ProviderCapabilities{Model: e.model}, err
	}
	dimensions := e.dimensions
	if dimensions == 0 && len(vectors) > 0 {
		dimensions = len(vectors[0])
	}
	caps := e.Capabilities()
	caps.Reachable = true
	caps.Model = e.model
	caps.Dimensions = dimensions
	return caps, nil
}

func (e *OpenAICompatibleEmbedder) Capabilities() ProviderCapabilities {
	e.mu.Lock()
	defer e.mu.Unlock()
	return ProviderCapabilities{
		Model:          e.model,
		Dimensions:     e.dimensions,
		UsageAvailable: e.usageAvailable,
	}
}

func BuildContent(section domain.EmbeddingSection) string {
	return embeddingtext.BuildContent(section)
}

func BuildContentWithTokenBudget(section domain.EmbeddingSection, maxInputTokens int) string {
	return embeddingtext.BuildContentWithTokenBudget(section, maxInputTokens)
}

func HashText(text string) string {
	return embeddingtext.HashText(text)
}

func NeedsEnsure(section domain.EmbeddingSection, existing domain.SectionEmbeddingHash, generatorVersion string) bool {
	return embeddingtext.NeedsEnsure(section, existing, generatorVersion)
}

func NeedsEnsureWithTokenBudget(section domain.EmbeddingSection, existing domain.SectionEmbeddingHash, generatorVersion string, maxInputTokens int) bool {
	return embeddingtext.NeedsEnsureWithTokenBudget(section, existing, generatorVersion, maxInputTokens)
}

// normalizeL2 performs L2 normalization on a float32 vector.
// After Matryoshka/MRL truncation, the remaining dimensions are no longer
// unit-length, so re-normalization is required for cosine similarity correctness.
func normalizeL2(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	norm := float32(math.Sqrt(sum))
	if norm == 0 {
		return v
	}
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x / norm
	}
	return out
}

func truncateForError(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}

func isContextExceededResponse(status int, data []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusRequestEntityTooLarge && status != http.StatusUnprocessableEntity {
		return false
	}
	value := strings.ToLower(string(data))
	return strings.Contains(value, "context") && (strings.Contains(value, "exceed") || strings.Contains(value, "too long") || strings.Contains(value, "maximum"))
}

func parseContextExceededError(model string, body string) error {
	err := &ContextExceededError{Model: model, Body: body}
	lower := strings.ToLower(body)
	if n := firstIntAfter(lower, "maximum context length"); n > 0 {
		err.ContextTokens = n
	} else if n := firstIntAfter(lower, "context length"); n > 0 {
		err.ContextTokens = n
	} else if n := firstIntAfter(lower, "max context"); n > 0 {
		err.ContextTokens = n
	}
	if n := firstIntAfter(lower, "requested"); n > 0 {
		err.InputTokens = n
	} else if n := firstIntAfter(lower, "input"); n > 0 {
		err.InputTokens = n
	}
	return err
}

func firstIntAfter(value string, marker string) int {
	i := strings.Index(value, marker)
	if i < 0 {
		return 0
	}
	re := regexp.MustCompile(`\d+`)
	match := re.FindString(value[i:])
	if match == "" {
		return 0
	}
	n, _ := strconv.Atoi(match)
	return n
}

// parseRetryAfterHeader parses the HTTP Retry-After header value.
// It accepts both integer seconds and HTTP-date formats per RFC 7231 §7.1.3.
// Returns 0 if the value is absent or unparseable.
func parseRetryAfterHeader(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	// Integer seconds form
	if n, err := strconv.Atoi(value); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	// HTTP-date form (rarely used by API providers; parse as fallback)
	if t, err := http.ParseTime(value); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return 0
}
