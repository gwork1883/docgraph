package vectorstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/docgraph/docgraph/internal/domain"
)

type PGVectorStore struct {
	pool *pgxpool.Pool
}

func domainChunkID(sectionID string, ordinal int, model string, generatorVersion string, tokenizer string, chunkStrategy string, textHash string) string {
	value := fmt.Sprintf("%s\n%d\n%s\n%s\n%s\n%s\n%s", strings.TrimSpace(sectionID), ordinal, strings.TrimSpace(model), strings.TrimSpace(generatorVersion), strings.TrimSpace(tokenizer), strings.TrimSpace(chunkStrategy), strings.TrimSpace(textHash))
	sum := sha256.Sum256([]byte(value))
	return "embchk_" + hex.EncodeToString(sum[:16])
}

func OpenPGVector(ctx context.Context, dsn string) (*PGVectorStore, error) {
	pgdsn, err := postgresDSN(dsn)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.New(ctx, pgdsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect pgvector backend: %w", err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("acquire pgvector backend connection: %w", err)
	}
	conn.Release()
	return &PGVectorStore{pool: pool}, nil
}

func (s *PGVectorStore) Close() error {
	if s == nil || s.pool == nil {
		return nil
	}
	s.pool.Close()
	return nil
}

func (s *PGVectorStore) Migrate(ctx context.Context) error {
	return s.ensureSchema(ctx)
}

func (s *PGVectorStore) ensureSchema(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
create extension if not exists vector;
create table if not exists embedding_chunks (
  chunk_id text primary key,
  section_id text not null,
  document_id text not null,
  source_id text not null,
  chunk_ordinal integer not null,
  chunk_start_token integer not null default 0,
  chunk_token_count integer not null default 0,
  chunk_text text not null,
  chunk_text_hash text not null,
  section_content_hash text not null,
  model text not null,
  dimensions integer not null,
  embedding vector not null,
  tokenizer text not null default 'conservative',
  chunk_strategy text not null default 'structural',
  generator_version text not null default '',
  generated_at timestamptz not null default now(),
  constraint embedding_chunks_plan_unique unique(section_id, chunk_ordinal, model, tokenizer, chunk_strategy, generator_version)
);
create index if not exists idx_embedding_chunks_section on embedding_chunks(section_id);
create index if not exists idx_embedding_chunks_source on embedding_chunks(source_id);
create index if not exists idx_embedding_chunks_model on embedding_chunks(model);
create index if not exists idx_embedding_chunks_model_dimensions on embedding_chunks(model, dimensions);
create index if not exists idx_embedding_chunks_hash on embedding_chunks(section_content_hash);
do $$
declare
  old_plan_unique text;
begin
  alter table embedding_chunks
    add column if not exists chunk_strategy text not null default 'structural';
  update embedding_chunks
    set tokenizer = 'conservative'
    where trim(coalesce(tokenizer, '')) = '';
  update embedding_chunks
    set chunk_strategy = 'structural'
    where trim(coalesce(chunk_strategy, '')) = '';
  select conname into old_plan_unique
  from pg_constraint
  where conrelid = 'embedding_chunks'::regclass
    and contype = 'u'
    and conname = 'embedding_chunks_section_id_chunk_ordinal_model_generator_version_key';
  if old_plan_unique is not null then
    execute format('alter table embedding_chunks drop constraint %I', old_plan_unique);
  end if;
  if not exists (
    select 1 from pg_constraint
    where conrelid = 'embedding_chunks'::regclass
      and conname = 'embedding_chunks_plan_unique'
  ) then
    alter table embedding_chunks
      add constraint embedding_chunks_plan_unique unique(section_id, chunk_ordinal, model, tokenizer, chunk_strategy, generator_version);
  end if;
  if not exists (
    select 1 from pg_constraint
    where conrelid = 'embedding_chunks'::regclass
      and conname = 'embedding_chunks_dimensions_positive'
  ) then
    alter table embedding_chunks
      add constraint embedding_chunks_dimensions_positive check (dimensions > 0);
  end if;
  if not exists (
    select 1 from pg_constraint
    where conrelid = 'embedding_chunks'::regclass
      and conname = 'embedding_chunks_vector_dims_match'
  ) then
    alter table embedding_chunks
      add constraint embedding_chunks_vector_dims_match check (vector_dims(embedding) = dimensions);
  end if;
end $$;
create index if not exists idx_embedding_chunks_plan on embedding_chunks(model, tokenizer, chunk_strategy, generator_version);
create table if not exists section_embeddings (
  section_id text not null,
  document_id text not null,
  source_id text not null,
  model text not null,
  dimensions integer not null,
  embedding vector not null,
  content_hash text not null,
  embedding_text_hash text not null,
  generator_version text not null default '',
  generated_at timestamptz not null default now(),
  primary key (section_id, model)
);
do $$
declare
  pk_name text;
begin
  select conname into pk_name
  from pg_constraint
  where conrelid = 'section_embeddings'::regclass
    and contype = 'p'
    and array_length(conkey, 1) = 1;
  if pk_name is not null then
    execute format('alter table section_embeddings drop constraint %I', pk_name);
    alter table section_embeddings add primary key (section_id, model);
  end if;
end $$;
create index if not exists idx_section_embeddings_source on section_embeddings(source_id);
create index if not exists idx_section_embeddings_model on section_embeddings(model);
create index if not exists idx_section_embeddings_hash on section_embeddings(content_hash);
`)
	if err != nil {
		return fmt.Errorf("migrate pgvector embedding chunks: %w", err)
	}
	var extVersion string
	if err := s.pool.QueryRow(ctx, `select extversion from pg_extension where extname = 'vector'`).Scan(&extVersion); err != nil {
		return fmt.Errorf("check pgvector extension: %w", err)
	}
	return nil
}

func (s *PGVectorStore) UpsertSectionEmbedding(ctx context.Context, input domain.SectionEmbeddingInput) error {
	chunkTextHash := input.EmbeddingTextHash
	chunkID := domainChunkID(input.SectionID, 0, input.Model, input.GeneratorVersion, "conservative", "structural", chunkTextHash)
	return s.UpsertEmbeddingChunk(ctx, domain.EmbeddingChunkInput{
		ChunkID:            chunkID,
		SectionID:          input.SectionID,
		DocumentID:         input.DocumentID,
		SourceID:           input.SourceID,
		ChunkOrdinal:       0,
		ChunkStartToken:    0,
		ChunkTokenCount:    0,
		ChunkText:          chunkTextHash,
		Model:              input.Model,
		Dimensions:         input.Dimensions,
		Embedding:          input.Embedding,
		SectionContentHash: input.ContentHash,
		ChunkTextHash:      chunkTextHash,
		Tokenizer:          "conservative",
		ChunkStrategy:      "structural",
		GeneratorVersion:   input.GeneratorVersion,
	})
}

func (s *PGVectorStore) UpsertEmbeddingChunk(ctx context.Context, input domain.EmbeddingChunkInput) error {
	input.SectionID = strings.TrimSpace(input.SectionID)
	input.DocumentID = strings.TrimSpace(input.DocumentID)
	input.SourceID = strings.TrimSpace(input.SourceID)
	input.Model = strings.TrimSpace(input.Model)
	input.SectionContentHash = strings.TrimSpace(input.SectionContentHash)
	input.ChunkTextHash = strings.TrimSpace(input.ChunkTextHash)
	input.Tokenizer = strings.TrimSpace(input.Tokenizer)
	input.ChunkStrategy = strings.TrimSpace(input.ChunkStrategy)
	input.GeneratorVersion = strings.TrimSpace(input.GeneratorVersion)
	input.ChunkID = strings.TrimSpace(input.ChunkID)
	if input.ChunkStrategy == "" {
		input.ChunkStrategy = "auto"
	}
	if input.ChunkID == "" {
		input.ChunkID = domainChunkID(input.SectionID, input.ChunkOrdinal, input.Model, input.GeneratorVersion, input.Tokenizer, input.ChunkStrategy, input.ChunkTextHash)
	}
	if input.ChunkID == "" || input.SectionID == "" || input.DocumentID == "" || input.SourceID == "" || input.Model == "" {
		return fmt.Errorf("chunk_id, section_id, document_id, source_id, and model are required")
	}
	if input.Dimensions <= 0 {
		input.Dimensions = len(input.Embedding)
	}
	if input.Dimensions <= 0 || len(input.Embedding) != input.Dimensions {
		return fmt.Errorf("embedding dimensions mismatch: dimensions=%d values=%d", input.Dimensions, len(input.Embedding))
	}
	if input.SectionContentHash == "" || input.ChunkTextHash == "" {
		return fmt.Errorf("section_content_hash and chunk_text_hash are required")
	}
	if err := s.checkModelDimensions(ctx, input.Model, input.Dimensions); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `
insert into embedding_chunks (chunk_id, section_id, document_id, source_id, chunk_ordinal, chunk_start_token, chunk_token_count, chunk_text, chunk_text_hash, section_content_hash, model, dimensions, embedding, tokenizer, chunk_strategy, generator_version, generated_at)
values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::vector, $14, $15, $16, now())
on conflict(chunk_id) do update set
  section_id = excluded.section_id,
  document_id = excluded.document_id,
  source_id = excluded.source_id,
  chunk_ordinal = excluded.chunk_ordinal,
  chunk_start_token = excluded.chunk_start_token,
  chunk_token_count = excluded.chunk_token_count,
  chunk_text = excluded.chunk_text,
  chunk_text_hash = excluded.chunk_text_hash,
  section_content_hash = excluded.section_content_hash,
  model = excluded.model,
  dimensions = excluded.dimensions,
  embedding = excluded.embedding,
  tokenizer = excluded.tokenizer,
  chunk_strategy = excluded.chunk_strategy,
  generator_version = excluded.generator_version,
  generated_at = now()
`, input.ChunkID, input.SectionID, input.DocumentID, input.SourceID, input.ChunkOrdinal, input.ChunkStartToken, input.ChunkTokenCount, input.ChunkText, input.ChunkTextHash, input.SectionContentHash, input.Model, input.Dimensions, vectorLiteral(input.Embedding), input.Tokenizer, input.ChunkStrategy, input.GeneratorVersion)
	if err != nil {
		return fmt.Errorf("upsert pgvector embedding chunk %s: %w", input.ChunkID, err)
	}
	return nil
}

func (s *PGVectorStore) checkModelDimensions(ctx context.Context, model string, dimensions int) error {
	return s.ValidateModelDimensions(ctx, model, dimensions)
}

func (s *PGVectorStore) ValidateModelDimensions(ctx context.Context, model string, dimensions int) error {
	model = strings.TrimSpace(model)
	if model == "" || dimensions <= 0 {
		return nil
	}
	var existing int
	err := s.pool.QueryRow(ctx, `
select dimensions
from embedding_chunks
where model = $1
limit 1
`, model).Scan(&existing)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("check pgvector dimensions for model %q: %w", model, err)
	}
	if existing != dimensions {
		return fmt.Errorf("pgvector dimensions mismatch for model %q: existing=%d new=%d", model, existing, dimensions)
	}
	return nil
}

// MaxHNSWDimensions is the maximum number of dimensions supported by
// pgvector's HNSW index (as of pgvector 0.7+). Embedding models must
// produce vectors with dimensions ≤ this value for ANN indexing to work.
const MaxHNSWDimensions = 2000

func (s *PGVectorStore) EnsureANNIndex(ctx context.Context, model string, dimensions int) error {
	if dimensions <= 0 {
		return fmt.Errorf("dimensions must be positive")
	}
	if dimensions > MaxHNSWDimensions {
		return fmt.Errorf("vector dimensions %d exceed pgvector HNSW limit of %d; vector search works but without ANN acceleration — use an embedding model with ≤%d dimensions for optimal search performance", dimensions, MaxHNSWDimensions, MaxHNSWDimensions)
	}
	stmt := annIndexStatement(dimensions)
	if _, err := s.pool.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("ensure pgvector hnsw index dimensions=%d: %w", dimensions, err)
	}
	return nil
}

func (s *PGVectorStore) GetSectionEmbedding(ctx context.Context, sectionID string, model string) (domain.VectorSearchHit, []float32, error) {
	sectionID = strings.TrimSpace(sectionID)
	model = strings.TrimSpace(model)
	if sectionID == "" || model == "" {
		return domain.VectorSearchHit{}, nil, fmt.Errorf("section id and model are required")
	}
	var chunkID string
	err := s.pool.QueryRow(ctx, `
select chunk_id from embedding_chunks
where section_id = $1 and model = $2
order by chunk_ordinal asc
limit 1
`, sectionID, model).Scan(&chunkID)
	if err == nil {
		return s.GetEmbeddingChunk(ctx, chunkID, model)
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return domain.VectorSearchHit{}, nil, err
	}
	var hit domain.VectorSearchHit
	var vectorText string
	err = s.pool.QueryRow(ctx, `
select section_id, document_id, source_id, model, embedding::text, content_hash, embedding_text_hash, generator_version, generated_at::text
from section_embeddings
where section_id = $1 and model = $2
`, sectionID, model).Scan(&hit.SectionID, &hit.DocumentID, &hit.SourceID, &hit.Model, &vectorText, &hit.ContentHash, &hit.EmbeddingTextHash, &hit.GeneratorVersion, &hit.GeneratedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.VectorSearchHit{}, nil, sql.ErrNoRows
		}
		return domain.VectorSearchHit{}, nil, err
	}
	vector, err := parseVectorLiteral(vectorText)
	if err != nil {
		return domain.VectorSearchHit{}, nil, err
	}
	return hit, vector, nil
}

func (s *PGVectorStore) DeleteSectionEmbeddings(ctx context.Context, sectionID string) error {
	sectionID = strings.TrimSpace(sectionID)
	if sectionID == "" {
		return fmt.Errorf("section id is required")
	}
	if err := s.DeleteEmbeddingChunksBySection(ctx, sectionID, "", "", "", ""); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `delete from section_embeddings where section_id = $1`, sectionID)
	return err
}

func (s *PGVectorStore) SearchSectionsByVector(ctx context.Context, embedding []float32, model string, limit int, minSimilarity float64, plan EmbeddingPlanFilter) ([]domain.VectorSearchHit, error) {
	return s.SearchChunksByVector(ctx, embedding, model, limit, minSimilarity, plan)
}

func (s *PGVectorStore) GetEmbeddingChunk(ctx context.Context, chunkID string, model string) (domain.VectorSearchHit, []float32, error) {
	chunkID = strings.TrimSpace(chunkID)
	model = strings.TrimSpace(model)
	if chunkID == "" || model == "" {
		return domain.VectorSearchHit{}, nil, fmt.Errorf("chunk id and model are required")
	}
	var hit domain.VectorSearchHit
	var vectorText string
	err := s.pool.QueryRow(ctx, `
select chunk_id, section_id, document_id, source_id, chunk_ordinal, chunk_text, model, embedding::text,
       section_content_hash, chunk_text_hash, tokenizer, chunk_strategy, generator_version, generated_at::text
from embedding_chunks
where chunk_id = $1 and model = $2
`, chunkID, model).Scan(&hit.ChunkID, &hit.SectionID, &hit.DocumentID, &hit.SourceID, &hit.ChunkOrdinal, &hit.ChunkText, &hit.Model, &vectorText, &hit.SectionContentHash, &hit.ChunkTextHash, &hit.Tokenizer, &hit.ChunkStrategy, &hit.GeneratorVersion, &hit.GeneratedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.VectorSearchHit{}, nil, sql.ErrNoRows
		}
		return domain.VectorSearchHit{}, nil, err
	}
	hit.ContentHash = hit.SectionContentHash
	hit.EmbeddingTextHash = hit.ChunkTextHash
	vector, err := parseVectorLiteral(vectorText)
	if err != nil {
		return domain.VectorSearchHit{}, nil, err
	}
	return hit, vector, nil
}

func (s *PGVectorStore) DeleteEmbeddingChunksBySection(ctx context.Context, sectionID string, model string, generatorVersion string, tokenizer string, chunkStrategy string) error {
	sectionID = strings.TrimSpace(sectionID)
	model = strings.TrimSpace(model)
	generatorVersion = strings.TrimSpace(generatorVersion)
	tokenizer = strings.TrimSpace(tokenizer)
	chunkStrategy = strings.TrimSpace(chunkStrategy)
	if sectionID == "" {
		return fmt.Errorf("section id is required")
	}
	query := `delete from embedding_chunks where section_id = $1`
	args := []any{sectionID}
	if model != "" {
		query += ` and model = $2`
		args = append(args, model)
	}
	if generatorVersion != "" {
		query += fmt.Sprintf(` and generator_version = $%d`, len(args)+1)
		args = append(args, generatorVersion)
	}
	if tokenizer != "" {
		query += fmt.Sprintf(` and tokenizer = $%d`, len(args)+1)
		args = append(args, tokenizer)
	}
	if chunkStrategy != "" {
		query += fmt.Sprintf(` and chunk_strategy = $%d`, len(args)+1)
		args = append(args, chunkStrategy)
	}
	_, err := s.pool.Exec(ctx, query, args...)
	return err
}

func (s *PGVectorStore) SearchChunksByVector(ctx context.Context, embedding []float32, model string, limit int, minSimilarity float64, plan EmbeddingPlanFilter) ([]domain.VectorSearchHit, error) {
	model = strings.TrimSpace(model)
	if len(embedding) == 0 || model == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 60
	}
	if minSimilarity < -1 {
		minSimilarity = -1
	}
	dimensions := len(embedding)
	query := chunkVectorSearchQuery(dimensions)
	rows, err := s.pool.Query(ctx, query, vectorLiteral(embedding), model, dimensions, minSimilarity, limit, strings.TrimSpace(plan.Tokenizer), strings.TrimSpace(plan.ChunkStrategy), strings.TrimSpace(plan.GeneratorVersion))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	hits := make([]domain.VectorSearchHit, 0)
	for rows.Next() {
		var hit domain.VectorSearchHit
		if err := rows.Scan(&hit.ChunkID, &hit.SectionID, &hit.DocumentID, &hit.SourceID, &hit.ChunkOrdinal, &hit.ChunkText, &hit.Similarity, &hit.Model, &hit.SectionContentHash, &hit.ChunkTextHash, &hit.Tokenizer, &hit.ChunkStrategy, &hit.GeneratorVersion, &hit.GeneratedAt); err != nil {
			return nil, err
		}
		hit.ContentHash = hit.SectionContentHash
		hit.EmbeddingTextHash = hit.ChunkTextHash
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}

func (s *PGVectorStore) ListSectionEmbeddingHashes(ctx context.Context, model string, limit, offset int) ([]domain.SectionEmbeddingHash, error) {
	chunkHashes, err := s.ListEmbeddingChunkHashes(ctx, model, limit, offset)
	if err == nil && len(chunkHashes) > 0 {
		out := make([]domain.SectionEmbeddingHash, 0, len(chunkHashes))
		seen := map[string]bool{}
		for _, hash := range chunkHashes {
			if seen[hash.SectionID] {
				continue
			}
			seen[hash.SectionID] = true
			out = append(out, domain.SectionEmbeddingHash{
				SectionID:         hash.SectionID,
				DocumentID:        hash.DocumentID,
				SourceID:          hash.SourceID,
				Model:             hash.Model,
				ContentHash:       hash.SectionContentHash,
				EmbeddingTextHash: hash.ChunkTextHash,
				GeneratorVersion:  hash.GeneratorVersion,
				GeneratedAt:       hash.GeneratedAt,
			})
		}
		return out, nil
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, fmt.Errorf("embedding model is required")
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.pool.Query(ctx, `
select section_id, document_id, source_id, model, content_hash, embedding_text_hash, generator_version, generated_at::text
from section_embeddings
where model = $1
order by section_id asc
limit $2 offset $3
`, model, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hashes := make([]domain.SectionEmbeddingHash, 0)
	for rows.Next() {
		var item domain.SectionEmbeddingHash
		if err := rows.Scan(&item.SectionID, &item.DocumentID, &item.SourceID, &item.Model, &item.ContentHash, &item.EmbeddingTextHash, &item.GeneratorVersion, &item.GeneratedAt); err != nil {
			return nil, err
		}
		hashes = append(hashes, item)
	}
	return hashes, rows.Err()
}

func (s *PGVectorStore) ListEmbeddingChunkHashes(ctx context.Context, model string, limit, offset int) ([]domain.EmbeddingChunkHash, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, fmt.Errorf("embedding model is required")
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.pool.Query(ctx, `
select chunk_id, section_id, document_id, source_id, chunk_ordinal, model, section_content_hash, chunk_text_hash, tokenizer, chunk_strategy, generator_version, generated_at::text
from embedding_chunks
where model = $1
order by section_id asc, chunk_ordinal asc, chunk_id asc
limit $2 offset $3
`, model, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hashes := make([]domain.EmbeddingChunkHash, 0)
	for rows.Next() {
		var item domain.EmbeddingChunkHash
		if err := rows.Scan(&item.ChunkID, &item.SectionID, &item.DocumentID, &item.SourceID, &item.ChunkOrdinal, &item.Model, &item.SectionContentHash, &item.ChunkTextHash, &item.Tokenizer, &item.ChunkStrategy, &item.GeneratorVersion, &item.GeneratedAt); err != nil {
			return nil, err
		}
		hashes = append(hashes, item)
	}
	return hashes, rows.Err()
}

func (s *PGVectorStore) GetEmbeddingCoverage(ctx context.Context, sourceID string, model string, generatorVersion string, tokenizer string, chunkStrategy string) (domain.EmbeddingCoverage, error) {
	sourceID = strings.TrimSpace(sourceID)
	model = strings.TrimSpace(model)
	if sourceID == "" || model == "" {
		return domain.EmbeddingCoverage{}, nil
	}
	var coverage domain.EmbeddingCoverage
	err := s.pool.QueryRow(ctx, `
select count(distinct section_id), count(*)
from embedding_chunks
where source_id = $1 and model = $2
  and ($3 = '' or generator_version = $3)
  and ($4 = '' or tokenizer = $4)
  and ($5 = '' or chunk_strategy = $5)
`, sourceID, model, generatorVersion, tokenizer, chunkStrategy).Scan(&coverage.EmbeddedSections, &coverage.EmbeddedChunks)
	if err != nil {
		return domain.EmbeddingCoverage{}, err
	}
	return coverage, nil
}

func postgresDSN(dsn string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "pgvector" {
		parsed.Scheme = "postgres"
	}
	return parsed.String(), nil
}

func RedactDSN(dsn string) string {
	parsed, err := url.Parse(strings.TrimSpace(dsn))
	if err != nil || parsed.User == nil {
		return dsn
	}
	if username := parsed.User.Username(); username != "" {
		if _, ok := parsed.User.Password(); ok {
			parsed.User = url.UserPassword(username, "xxxxx")
		} else {
			parsed.User = url.User(username)
		}
	} else {
		parsed.User = url.UserPassword("", "xxxxx")
	}
	return parsed.String()
}

func vectorLiteral(values []float32) string {
	parts := make([]string, len(values))
	for i, value := range values {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			value = 0
		}
		parts[i] = strconv.FormatFloat(float64(value), 'g', -1, 32)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func parseVectorLiteral(value string) ([]float32, error) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(strings.TrimSuffix(value, "]"), "[")
	if value == "" {
		return nil, nil
	}
	fields := strings.Split(value, ",")
	out := make([]float32, 0, len(fields))
	for _, field := range fields {
		n, err := strconv.ParseFloat(strings.TrimSpace(field), 32)
		if err != nil {
			return nil, err
		}
		out = append(out, float32(n))
	}
	return out, nil
}

func annIndexName(dimensions int) string {
	return fmt.Sprintf("idx_embedding_chunks_hnsw_cosine_d_%d", dimensions)
}

func annIndexStatement(dimensions int) string {
	return fmt.Sprintf(
		`create index if not exists %s on embedding_chunks using hnsw ((embedding::vector(%d)) vector_cosine_ops) where dimensions = %d`,
		pgx.Identifier{annIndexName(dimensions)}.Sanitize(),
		dimensions,
		dimensions,
	)
}

func chunkVectorSearchQuery(dimensions int) string {
	distanceExpr := fmt.Sprintf(`(embedding::vector(%d) <=> $1::vector(%d))`, dimensions, dimensions)
	return fmt.Sprintf(`
select chunk_id, section_id, document_id, source_id, chunk_ordinal, chunk_text,
       1 - %s as similarity,
       model, section_content_hash, chunk_text_hash, tokenizer, chunk_strategy, generator_version, generated_at::text
from embedding_chunks
where model = $2
  and dimensions = $3
  and 1 - %s >= $4
  and ($6 = '' or tokenizer = $6)
  and ($7 = '' or chunk_strategy = $7)
  and ($8 = '' or generator_version = $8)
order by %s, section_id asc, chunk_ordinal asc
limit $5
`, distanceExpr, distanceExpr, distanceExpr)
}
