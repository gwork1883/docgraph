package config

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Server       ServerConfig
	Storage      StorageConfig
	Search       BackendConfig
	VectorSearch VectorSearchConfig
	Auth         AuthConfig
}

type ServerConfig struct {
	Host       string
	Port       int
	DataDir    string
	WebPrefix  string
	JobWorkers int
}

type StorageConfig struct {
	DSN string
}

type BackendConfig struct {
	DSN string
}

type VectorSearchConfig struct {
	Enabled                     bool
	SearchWeight                float64
	IntentRouteEntityText       float64
	IntentRouteEntityVector     float64
	IntentRouteConceptualText   float64
	IntentRouteConceptualVector float64
	IntentRouteGeneralText      float64
	IntentRouteGeneralVector    float64
	RRFK                        float64
	Embedding                   EmbeddingConfig
	VectorDB                    BackendConfig
}

type EmbeddingConfig struct {
	Provider           string
	Model              string
	APIURL             string
	APIKey             string
	Dimensions         int
	Tokenizer          string
	ChunkStrategy      string
	ContextTokens      int
	ChunkTargetTokens  int
	ChunkOverlapTokens int
	MaxBatchTokens     int
	BatchSize          int
	MaxInputTokens     int
	Concurrency        int
	Timeout            time.Duration
	GeneratorVersion   string
}

type AuthConfig struct {
	Mode  string
	Token string
}

const (
	DefaultPath               = "docgraph.yaml"
	DefaultVectorSearchWeight = 0.4
)

func Default() Config {
	dataDir := ".docgraph"
	dbPath := filepath.ToSlash(filepath.Join(dataDir, "docgraph.db"))
	return Config{
		Server: ServerConfig{
			Host:       "127.0.0.1",
			Port:       8787,
			DataDir:    dataDir,
			WebPrefix:  "",
			JobWorkers: 2,
		},
		Storage: StorageConfig{DSN: "sqlite://" + dbPath},
		Search:  BackendConfig{DSN: "sqlite://" + dbPath},
		VectorSearch: VectorSearchConfig{
			Enabled:                     false,
			SearchWeight:                DefaultVectorSearchWeight,
			IntentRouteEntityText:       0.8,
			IntentRouteEntityVector:     0.2,
			IntentRouteConceptualText:   0.3,
			IntentRouteConceptualVector: 0.7,
			IntentRouteGeneralText:      0.5,
			IntentRouteGeneralVector:    0.5,
			RRFK:                        60,
			VectorDB: BackendConfig{
				DSN: "none://",
			},
			Embedding: EmbeddingConfig{
				Provider:           "none",
				Model:              "",
				APIURL:             "https://api.openai.com/v1",
				Dimensions:         0,
				Tokenizer:          "auto",
				ChunkStrategy:      "auto",
				ContextTokens:      0,
				ChunkTargetTokens:  0,
				ChunkOverlapTokens: 0,
				MaxBatchTokens:     0,
				BatchSize:          64,
				MaxInputTokens:     0,
				Concurrency:        3,
				Timeout:            5 * time.Minute,
				GeneratorVersion:   "embedding-chunk-v1",
			},
		},
		Auth: AuthConfig{Mode: "none"},
	}
}

func DefaultWithTokenAuth() (Config, error) {
	cfg := Default()
	token, err := GenerateToken()
	if err != nil {
		return Config{}, err
	}
	cfg.Auth = AuthConfig{Mode: "token", Token: token}
	return cfg, nil
}

func GenerateToken() (string, error) {
	var data [32]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data[:]), nil
}

func redactConfigAPIKey(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") {
		return value
	}
	return "${DOCGRAPH_EMBEDDING_API_KEY}"
}

func Write(path string, cfg Config) error {
	if path == "" {
		return fmt.Errorf("config path is required")
	}
	data := fmt.Sprintf(`server:
  host: %q
  port: %d
  data_dir: %q
  web_prefix: %q
  job_workers: %d
storage:
  dsn: %q
search:
  dsn: %q
vector_search:
  enabled: %t
  search_weight: %.2f
  intent_route_entity_w_text: %.2f
  intent_route_entity_w_vector: %.2f
  intent_route_conceptual_w_text: %.2f
  intent_route_conceptual_w_vector: %.2f
  intent_route_general_w_text: %.2f
  intent_route_general_w_vector: %.2f
  rrf_k: %.0f
  embedding:
    provider: %q
    model: %q
    api_url: %q
    api_key: %q
    dimensions: %d
    tokenizer: %q
    chunk_strategy: %q
    context_tokens: %d
    chunk_target_tokens: %d
    chunk_overlap_tokens: %d
    max_batch_tokens: %d
    batch_size: %d
    concurrency: %d
    timeout: %q
    generator_version: %q
  vector_db:
    dsn: %q
auth:
  mode: %q
  token: %q
`, cfg.Server.Host, cfg.Server.Port, cfg.Server.DataDir, cfg.Server.WebPrefix, cfg.Server.JobWorkers, cfg.Storage.DSN, cfg.Search.DSN, cfg.VectorSearch.Enabled, cfg.VectorSearch.SearchWeight, cfg.VectorSearch.IntentRouteEntityText, cfg.VectorSearch.IntentRouteEntityVector, cfg.VectorSearch.IntentRouteConceptualText, cfg.VectorSearch.IntentRouteConceptualVector, cfg.VectorSearch.IntentRouteGeneralText, cfg.VectorSearch.IntentRouteGeneralVector, cfg.VectorSearch.RRFK, cfg.VectorSearch.Embedding.Provider, cfg.VectorSearch.Embedding.Model, cfg.VectorSearch.Embedding.APIURL, redactConfigAPIKey(cfg.VectorSearch.Embedding.APIKey), cfg.VectorSearch.Embedding.Dimensions, cfg.VectorSearch.Embedding.Tokenizer, cfg.VectorSearch.Embedding.ChunkStrategy, cfg.VectorSearch.Embedding.ContextTokens, cfg.VectorSearch.Embedding.ChunkTargetTokens, cfg.VectorSearch.Embedding.ChunkOverlapTokens, cfg.VectorSearch.Embedding.MaxBatchTokens, cfg.VectorSearch.Embedding.BatchSize, cfg.VectorSearch.Embedding.Concurrency, cfg.VectorSearch.Embedding.Timeout.String(), cfg.VectorSearch.Embedding.GeneratorVersion, cfg.VectorSearch.VectorDB.DSN, cfg.Auth.Mode, cfg.Auth.Token)
	return os.WriteFile(path, []byte(data), 0o600)
}

func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	section := ""
	sectionIndent := 0
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		raw := scanner.Text()
		withoutComment := stripComment(raw)
		line := strings.TrimSpace(withoutComment)
		if line == "" {
			continue
		}
		indent := leadingIndent(withoutComment)

		if strings.HasSuffix(line, ":") && !strings.Contains(line, " ") {
			name := strings.TrimSuffix(line, ":")
			if indent > 0 && section == "vector_search" {
				section = "vector_search." + name
				sectionIndent = indent
			} else {
				section = name
				sectionIndent = indent
			}
			continue
		}

		// Pop back to parent section if indentation drops to or below sub-section level
		if indent <= sectionIndent && strings.HasPrefix(section, "vector_search.") {
			section = "vector_search"
			sectionIndent = 0
		}

		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return Config{}, fmt.Errorf("%s:%d: expected key: value", path, lineNo)
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if err := apply(&cfg, section, key, value); err != nil {
			return Config{}, fmt.Errorf("%s:%d: %w", path, lineNo, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return Config{}, err
	}

	if cfg.Storage.DSN == "" {
		return Config{}, fmt.Errorf("storage.dsn is required")
	}
	if cfg.Auth.Mode == "" {
		cfg.Auth.Mode = "none"
	}
	if cfg.Auth.Mode != "none" && cfg.Auth.Mode != "token" {
		return Config{}, fmt.Errorf("auth.mode must be none or token")
	}
	if cfg.Auth.Mode == "token" && cfg.Auth.Token == "" {
		return Config{}, fmt.Errorf("auth.token is required when auth.mode is token")
	}
	if cfg.Server.JobWorkers < 1 {
		cfg.Server.JobWorkers = 1
	}
	normalizeVectorSearchConfig(&cfg)
	prefix, err := NormalizeWebPrefix(cfg.Server.WebPrefix)
	if err != nil {
		return Config{}, err
	}
	cfg.Server.WebPrefix = prefix
	return cfg, nil
}

func NormalizeWebPrefix(value string) (string, error) {
	prefix := strings.TrimSpace(value)
	if prefix == "" || prefix == "/" {
		return "", nil
	}
	if strings.ContainsAny(prefix, "?#") || strings.Contains(prefix, " ") {
		return "", fmt.Errorf("server.web_prefix must be a path prefix, for example docgraph or /docgraph")
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	prefix = strings.TrimRight(prefix, "/")
	if prefix == "" {
		return "", nil
	}
	if strings.Contains(prefix, "//") {
		return "", fmt.Errorf("server.web_prefix must not contain empty path segments")
	}
	return prefix, nil
}

func stripComment(line string) string {
	if i := strings.IndexByte(line, '#'); i >= 0 {
		return line[:i]
	}
	return line
}

func apply(cfg *Config, section, key, value string) error {
	switch section {
	case "server":
		switch key {
		case "host":
			cfg.Server.Host = value
		case "port":
			port, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("invalid server.port")
			}
			cfg.Server.Port = port
		case "data_dir":
			cfg.Server.DataDir = value
		case "prefix", "web_prefix":
			cfg.Server.WebPrefix = value
		case "job_workers":
			workers, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("invalid server.job_workers")
			}
			cfg.Server.JobWorkers = workers
		default:
			return fmt.Errorf("unknown server key %q", key)
		}
	case "storage":
		if key != "dsn" {
			return fmt.Errorf("unknown storage key %q", key)
		}
		cfg.Storage.DSN = value
	case "search":
		if key != "dsn" {
			return fmt.Errorf("unknown search key %q", key)
		}
		cfg.Search.DSN = value
	case "vector":
		if key != "dsn" {
			return fmt.Errorf("unknown vector key %q", key)
		}
		cfg.VectorSearch.VectorDB.DSN = value
	case "embedding":
		return applyEmbedding(&cfg.VectorSearch.Embedding, key, value)
	case "vector_search":
		switch key {
		case "enabled":
			enabled, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("invalid vector_search.enabled")
			}
			cfg.VectorSearch.Enabled = enabled
		case "search_weight":
			searchWeight, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return fmt.Errorf("invalid vector_search.search_weight")
			}
			cfg.VectorSearch.SearchWeight = searchWeight
		case "intent_route_entity_w_text":
			v, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return fmt.Errorf("invalid vector_search.intent_route_entity_w_text")
			}
			cfg.VectorSearch.IntentRouteEntityText = v
		case "intent_route_entity_w_vector":
			v, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return fmt.Errorf("invalid vector_search.intent_route_entity_w_vector")
			}
			cfg.VectorSearch.IntentRouteEntityVector = v
		case "intent_route_conceptual_w_text":
			v, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return fmt.Errorf("invalid vector_search.intent_route_conceptual_w_text")
			}
			cfg.VectorSearch.IntentRouteConceptualText = v
		case "intent_route_conceptual_w_vector":
			v, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return fmt.Errorf("invalid vector_search.intent_route_conceptual_w_vector")
			}
			cfg.VectorSearch.IntentRouteConceptualVector = v
		case "intent_route_general_w_text":
			v, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return fmt.Errorf("invalid vector_search.intent_route_general_w_text")
			}
			cfg.VectorSearch.IntentRouteGeneralText = v
		case "intent_route_general_w_vector":
			v, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return fmt.Errorf("invalid vector_search.intent_route_general_w_vector")
			}
			cfg.VectorSearch.IntentRouteGeneralVector = v
		case "rrf_k":
			v, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return fmt.Errorf("invalid vector_search.rrf_k")
			}
			cfg.VectorSearch.RRFK = v
		default:
			return fmt.Errorf("unknown vector_search key %q", key)
		}
	case "vector_search.embedding":
		return applyEmbedding(&cfg.VectorSearch.Embedding, key, value)
	case "vector_search.vector_db":
		if key != "dsn" {
			return fmt.Errorf("unknown vector_search.vector_db key %q", key)
		}
		cfg.VectorSearch.VectorDB.DSN = value
	case "vector_db":
		if key != "dsn" {
			return fmt.Errorf("unknown vector_db key %q", key)
		}
		cfg.VectorSearch.VectorDB.DSN = value
	case "auth":
		switch key {
		case "mode":
			cfg.Auth.Mode = value
		case "token":
			cfg.Auth.Token = value
		default:
			return fmt.Errorf("unknown auth key %q", key)
		}
	case "":
		return fmt.Errorf("top-level key %q is not supported", key)
	default:
		return fmt.Errorf("unknown section %q", section)
	}
	return nil
}

func applyEmbedding(cfg *EmbeddingConfig, key, value string) error {
	switch key {
	case "provider":
		cfg.Provider = value
	case "model":
		cfg.Model = value
	case "api_url":
		cfg.APIURL = value
	case "api_key":
		cfg.APIKey = expandEnvValue(value)
	case "dimensions":
		dimensions, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid embedding.dimensions")
		}
		cfg.Dimensions = dimensions
	case "tokenizer":
		cfg.Tokenizer = value
	case "chunk_strategy":
		cfg.ChunkStrategy = value
	case "context_tokens":
		contextTokens, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid embedding.context_tokens")
		}
		cfg.ContextTokens = contextTokens
	case "chunk_target_tokens":
		chunkTargetTokens, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid embedding.chunk_target_tokens")
		}
		cfg.ChunkTargetTokens = chunkTargetTokens
	case "chunk_overlap_tokens":
		chunkOverlapTokens, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid embedding.chunk_overlap_tokens")
		}
		cfg.ChunkOverlapTokens = chunkOverlapTokens
	case "max_batch_tokens":
		maxBatchTokens, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid embedding.max_batch_tokens")
		}
		cfg.MaxBatchTokens = maxBatchTokens
	case "batch_size":
		batchSize, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid embedding.batch_size")
		}
		cfg.BatchSize = batchSize
	case "max_input_tokens", "max_input_chars":
		maxInputTokens, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid embedding.max_input_tokens")
		}
		cfg.MaxInputTokens = maxInputTokens
		if cfg.ChunkTargetTokens <= 0 {
			cfg.ChunkTargetTokens = maxInputTokens
		}
	case "concurrency":
		concurrency, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid embedding.concurrency")
		}
		cfg.Concurrency = concurrency
	case "timeout":
		timeout, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("invalid embedding.timeout")
		}
		cfg.Timeout = timeout
	case "generator_version":
		cfg.GeneratorVersion = value
	default:
		return fmt.Errorf("unknown embedding key %q", key)
	}
	return nil
}

func normalizeIntentRouteWeight(value float64, defaultWeight float64) float64 {
	if value <= 0 {
		return defaultWeight
	}
	if value > 1 {
		return 1
	}
	return value
}

func normalizeVectorSearchConfig(cfg *Config) {
	if cfg.VectorSearch.SearchWeight <= 0 {
		cfg.VectorSearch.SearchWeight = DefaultVectorSearchWeight
	}
	if cfg.VectorSearch.SearchWeight > 1 {
		cfg.VectorSearch.SearchWeight = 1
	}
	// Normalize intent route weights: each pair must sum to ~1.0 and be in [0.1, 0.9]
	cfg.VectorSearch.IntentRouteEntityText = normalizeIntentRouteWeight(cfg.VectorSearch.IntentRouteEntityText, 0.8)
	cfg.VectorSearch.IntentRouteEntityVector = normalizeIntentRouteWeight(cfg.VectorSearch.IntentRouteEntityVector, 0.2)
	cfg.VectorSearch.IntentRouteConceptualText = normalizeIntentRouteWeight(cfg.VectorSearch.IntentRouteConceptualText, 0.3)
	cfg.VectorSearch.IntentRouteConceptualVector = normalizeIntentRouteWeight(cfg.VectorSearch.IntentRouteConceptualVector, 0.7)
	cfg.VectorSearch.IntentRouteGeneralText = normalizeIntentRouteWeight(cfg.VectorSearch.IntentRouteGeneralText, 0.5)
	cfg.VectorSearch.IntentRouteGeneralVector = normalizeIntentRouteWeight(cfg.VectorSearch.IntentRouteGeneralVector, 0.5)
	if cfg.VectorSearch.RRFK <= 0 {
		cfg.VectorSearch.RRFK = 60
	}
	embedding := &cfg.VectorSearch.Embedding
	embedding.Provider = strings.TrimSpace(strings.ToLower(embedding.Provider))
	if embedding.Provider == "" {
		embedding.Provider = "none"
	}
	if embedding.APIURL == "" {
		embedding.APIURL = "https://api.openai.com/v1"
	}
	if strings.TrimSpace(embedding.Tokenizer) == "" {
		embedding.Tokenizer = "auto"
	}
	if strings.TrimSpace(embedding.ChunkStrategy) == "" {
		embedding.ChunkStrategy = "auto"
	}
	if embedding.BatchSize <= 0 {
		embedding.BatchSize = 64
	}
	if embedding.ChunkTargetTokens <= 0 && embedding.MaxInputTokens > 0 {
		embedding.ChunkTargetTokens = embedding.MaxInputTokens
	}
	if embedding.ContextTokens < 0 {
		embedding.ContextTokens = 0
	}
	if embedding.ChunkTargetTokens < 0 {
		embedding.ChunkTargetTokens = 0
	}
	if embedding.ChunkOverlapTokens < 0 {
		embedding.ChunkOverlapTokens = 0
	}
	if embedding.MaxBatchTokens < 0 {
		embedding.MaxBatchTokens = 0
	}
	if embedding.Concurrency <= 0 {
		embedding.Concurrency = 3
	}
	if embedding.Timeout <= 0 {
		embedding.Timeout = 5 * time.Minute
	}
	if embedding.GeneratorVersion == "" {
		embedding.GeneratorVersion = "embedding-chunk-v1"
	}
	cfg.VectorSearch.VectorDB.DSN = strings.TrimSpace(cfg.VectorSearch.VectorDB.DSN)
	if cfg.VectorSearch.VectorDB.DSN == "" {
		cfg.VectorSearch.VectorDB.DSN = "none://"
	}
}

func leadingIndent(value string) int {
	return len(value) - len(strings.TrimLeft(value, " \t"))
}

func expandEnvValue(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") {
		return os.Getenv(strings.TrimSuffix(strings.TrimPrefix(value, "${"), "}"))
	}
	return os.ExpandEnv(value)
}
