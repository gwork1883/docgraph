# DocGraph Section-Scoped Chunk Evaluation Design

## 背景

DocGraph 的文档同步链路已经先把文档拆到 `section`，实际语义边界基本对应 Markdown 二级标题。也就是说，`section` 是搜索命中、结果展示、`doc_get_section` 深读和人工校验的主边界。

因此下一阶段的 chunk 调优不应该重新发明文档分段，也不应该把 chunk 当成公开读取单位。chunk 的职责应收敛为：

1. 在 section 内生成更稳定的 embedding 输入。
2. 控制 embedding token、索引行数和搜索候选成本。
3. 保护 section 内有结构价值的 block，避免搜索证据和 snippet 被切坏。

这里的核心指标是 `BI`，即 Block Integrity。第一阶段重点保护：

- fenced code block。
- Markdown table。
- HTML table。
- image / figure / caption。
- list。
- blockquote。
- admonition。
- API path、配置 key/value、错误码等连续说明块。

当前 `structural` splitter 主要按空行、短行和 token budget 打包。这个机制能跑通工程链路，但对表格、代码、图片说明等块状内容的理解有限。后续优化必须先有可重复评估闭环，否则容易只凭直觉调整 splitter。

## 目标

建立一个 section-scoped chunk/search 评估闭环，用于回答：

1. H2 section 作为基础语义边界是否已经足够？
2. section 内 chunk 是否破坏了有价值的 block？
3. 保护 BI 后，搜索是否仍能命中 expected section，snippet 是否更可读？
4. 不同 plan 的 chunk 数量、估算 token、embedding 成本和 BI 收益如何取舍？
5. 哪些 section 类型需要 chunk，哪些 section 应保持整体输入？

评估命中仍以 `expected_sections` 为主。chunk 是 section 内证据载体，主要用于 embedding 和 trace，不替代 section。

## 非目标

- 不在第一阶段引入 LLM 自动判分。
- 不在第一阶段实现 query-aware plan selection。
- 不重新设计 section parser。
- 不把 chunk 作为替代 section 的公开读取单位。
- 不要求默认 CI 依赖真实 embedding provider 或 pgvector。
- 不追求任意 HTML DOM 完整保真；第一阶段只保障可验证的 BI 对象。
- 不用主观“看几个结果不错”替代可记录指标。

## 边界模型与 BI 风险

### Section 边界已确定

DocGraph 的外层语义边界是 section。eval 不应再把 chunk plan 当成文档分段算法比较，而应判断 section 内如何构造 embedding chunk 更合理。

### Section 内 BI 破坏风险

当前 chunker 可能把以下内容切散：

- 表格表头和数据行。
- HTML table 的 `<thead>`、`<tbody>`、`<tr>`、`<td>`。
- fenced code opening / body / closing。
- 图片、caption 和相邻解释段。
- list item 及其缩进 continuation。
- API method/path 和返回说明。
- 配置 key/value 和约束说明。

这些切断不一定马上让 `hit@K` 下降，但会影响 chunk embedding 语义、vector trace、snippet 可读性和后续 agent 使用证据的可靠性。

### Table 是一等 BI 对象

Markdown table 和 HTML table 都必须作为 table block 识别，不应被当作普通短行处理。

如果 table 太大，后续策略应按 row group 切，并重复 caption/header，而不是按 rune 粗切。第一阶段先评估 table 是否被切断；header repeat 是下一阶段 `block-aware-pack` 的实现要求。

### Oversized BI 对象处理

超过预算的 block 不能简单按 rune 切。后续策略应为不同 block 定义退化规则：

- table：按行组切，重复 caption 和 header。
- code：保持 fence，优先按函数、空行或注释边界切。
- image/caption：图片、caption、紧邻解释段 sticky。
- list：按完整 item 或 item group 切。

## 评估数据集

建议新增目录：

```text
testdata/search_eval/
  queries.jsonl
  chunk_plans.json
  documents/
```

### Query Fixture

每行一个 JSON object：

```json
{
  "id": "api-path-search-001",
  "query": "GET /api/sources/{id}/artifacts 返回什么",
  "query_type": "exact_api",
  "expected_sections": ["section-id-1"],
  "expected_documents": ["document-id-1"],
  "must_include_terms": ["/api/sources/{id}/artifacts"],
  "must_preserve_blocks": ["api-artifacts-response-table"],
  "notes": "API path exact query should preserve the response table as one evidence block."
}
```

推荐 query types：

- `exact_api`: API path、HTTP method、endpoint。
- `config`: 配置项、环境变量、命令参数。
- `symbol`: 函数名、字段名、错误码。
- `semantic`: 用户不知道准确术语的自然语言问题。
- `troubleshooting`: 故障现象、报错描述、排障类查询。
- `concept`: 概念解释、设计背景、架构查询。
- `mixed_zh_en`: 中英文混合的企业内部常见 query。

### Document Fixture

第一批 fixture 应覆盖：

- H2 section with Markdown table。
- H2 section with HTML table。
- fenced code + explanation。
- API endpoint + 参数表 / 返回表。
- long config list。
- image / figure / caption。
- mixed Chinese/English troubleshooting section。

## 评估 Plan

第一阶段只比较与 DocGraph 边界模型相关的 plan：

```json
[
  {
    "name": "auto",
    "tokenizer": "auto",
    "strategy": "auto",
    "chunk_target_tokens": 768,
    "chunk_overlap_tokens": 0
  },
  {
    "name": "section-whole",
    "tokenizer": "conservative",
    "strategy": "section_whole",
    "chunk_target_tokens": 1024,
    "chunk_overlap_tokens": 0
  },
  {
    "name": "current-structural",
    "tokenizer": "conservative",
    "strategy": "structural",
    "chunk_target_tokens": 512,
    "chunk_overlap_tokens": 51
  },
  {
    "name": "block-aware-pack",
    "tokenizer": "conservative",
    "strategy": "block_aware_pack",
    "chunk_target_tokens": 768,
    "chunk_overlap_tokens": 0
  }
]
```

含义：

- `auto`: DocGraph 默认生产策略。section 放得下时整体输入；超预算时使用 block-aware section 内打包。`auto` 是 active plan 名称，写入向量元数据时保持为 `auto`，避免生产索引混入内部策略名。
- `section-whole`: section 整体作为 chunk，除非超过硬上限。用于判断 H2 section 边界本身是否已经足够。
- `current-structural`: 当前 splitter baseline，用于回归对照。
- `block-aware-pack`: 先识别 BI block，再按 token budget 打包。默认不切 BI；必须切时使用 block-specific 退化策略。

`adaptive` 不作为第一阶段主候选。现有 `adaptive` 优化的是 token balance、overlap waste 等内部指标，不直接表达 DocGraph 当前最重要的 BI 目标。

## 指标

### 检索质量

- `hit@1`: top 1 是否命中 expected section/document。
- `hit@3`: top 3 是否命中。
- `hit@5`: top 5 是否命中。
- `mrr`: expected result 的 reciprocal rank。
- `miss_count`: expected result 完全未命中次数。

### BI 质量

- `bi_block_split_count`: 没有任何单个 chunk 完整包含该 block 的次数。
- `bi_block_preserve_rate`: 完整保留的 block 占比。
- `table_split_count`: Markdown/HTML table 被切断次数。
- `markdown_table_preserve_rate`: Markdown table 完整保留占比。
- `html_table_preserve_rate`: HTML table 完整保留占比。
- `code_fence_split_count`: fenced code 被切断次数。
- `api_method_path_same_chunk_rate`: method/path 与关键说明在同一 chunk 的占比。

### 证据质量

- `snippet_has_must_terms`: snippet 是否包含 must include terms。
- `chunk_contains_expected_bi_block`: 命中的 chunk 是否包含 expected BI block。
- `vector_hit_resolves_to_expected_section`: vector hit 是否回溯到 expected section。
- `full_section_fetch_available`: 命中后是否能用 section id 获取完整 section。

### 成本与性能

- `chunk_count`: 每个 plan 生成的 chunk 数。
- `total_chunk_tokens`: 总估算 token。
- `embedding_request_count`: embedding 请求次数。
- `embedding_input_tokens`: 如果 provider usage 可用，记录实际输入 tokens。
- `index_rows`: 写入向量索引的行数。
- `search_latency_ms`: 搜索耗时。

## 执行方式

建议实现内部 eval 命令或测试工具：

```bash
docgraph eval search \
  --queries testdata/search_eval/queries.jsonl \
  --plans testdata/search_eval/chunk_plans.json \
  --out .docgraph-test/eval/search-eval-result.json
```

第一阶段也可以先做 Go test helper，不必暴露正式 CLI。

### Phase 1: 离线 Fixture Eval

1. 加载 fixture documents。
2. 按 plan 对每个 section 构造 chunks。
3. 输出 chunk/token 成本和 BI metrics。
4. 对 golden queries 执行搜索，收集 SearchResult、attempts、trace、score breakdown。
5. 输出每个 plan 的 hit@K/MRR 和逐 query diff。

### Phase 2: 实现 Block-Aware Pack

1. 识别 fenced code、Markdown table、HTML table、image/caption、list、blockquote、admonition。
2. 按 block 打包，不在 block 内随意切。
3. oversized table/code 使用明确退化策略。
4. 与 `current-structural` 比较 BI split、搜索质量和成本。

当前初版实现状态：

- `auto` 已作为默认 chunk strategy 暴露：小 section 走 section-whole 行为，超预算 section 走 block-aware pack 行为，并以 `chunk_strategy=auto` 作为稳定 active plan 元数据写入索引。
- `block_aware_pack` 已作为可选 strategy 暴露。
- `section_whole` 已作为可选 strategy 暴露，用于 section fit 时整体输入、超预算时回退 structural。
- `structural` 已降级为 legacy/baseline strategy；`recursive` 仍作为 structural 兼容别名。
- `/api/embedding/chunk-plans/compare` 已切到 section-scoped eval helper，返回 plan summary、size compliance、block integrity 和 violations。
- 生产 vector search 已按当前 active plan 的 tokenizer、chunk_strategy、generator_version 过滤，避免旧 plan chunk 和当前 plan chunk 混搜。
- embedding ensure 已接入 `embedding.concurrency` 做 batch 级并发；job progress 的 total section 统计改为轻量 count，避免启动前重复执行完整 embedding status/chunk plan。
- embedding ensure 遇到 HTTP timeout / deadline 这类临时 embedding 服务容量错误时，会把失败 batch 标记为 deferred，继续处理其他 batch，并创建延迟 retry job，不再让整轮补齐失败。
- ensure 已能识别当前确定性 retry-split chunks，避免 oversized chunk 下次补齐时删掉 split chunk 后重复失败/拆分。
- BI eval helper 已能识别 fenced code、Markdown table、HTML table、image/caption、list、blockquote、admonition，并按 section content 映射 chunk spans。
- Markdown table 超预算时按 row group 切，并在每个子 chunk 重复表头。
- Markdown table 单行 row 超预算时不再裸 rune 切；退化子 chunk 仍通过 table wrapper 重复表头上下文。
- HTML table 超预算时按 data row group 切，并在每个子 chunk 重复 `<table>`、caption、thead/header、tbody 和 closing tag。
- HTML table 已覆盖 caption、多行 thead/tbody、多行 `<tr>` data row 的 header-repeat 退化。
- HTML table 单个 `<tr>` 超预算时不再裸 rune 切；退化子 chunk 仍通过 table wrapper 重复 caption/header/tbody/table 上下文。
- fenced code 超预算时在保留 opening/closing fence 的前提下，优先按空行、函数/方法声明、注释边界分组；无法形成可预算 group 时回退到按代码行分组。
- image/caption、list、blockquote、admonition 超预算时已有保守退化：按行、item 或 group 切，并在必要时重复图片行、list marker、blockquote marker 或 admonition wrapper。
- eval 已能把 table/code/list/admonition 等合法退化识别为 BI preserved，而不是简单记为 split violation。

仍待细化：

- HTML table 仍不是完整 DOM parser；嵌套 table、属性异常换行和非法 HTML 需要后续 fixture 验证。
- table row 单行本身超预算目前只保证 header/context repeat，仍未做字段级或 cell-aware 退化。
- image/caption 的紧邻解释段 sticky 规则仍较保守，后续可以补充 figure 周边段落 fixture。
- API method/path、配置 key/value、错误码等连续说明块仍待作为独立 BI pattern 固化。
- ensure 仍会按 model 加载所有 chunk hashes；后续应增加 source/plan-scoped hash listing，减少大库补齐时的内存和 DB 扫描成本。
- pgvector 写入仍是 per-chunk upsert，且维度校验仍在每个 chunk 路径上；后续可以做 batch upsert 和进程内维度校验缓存。
- deferred retry 目前是固定延迟重跑；后续可以根据连续 timeout 自动 backoff，并动态降低 batch_size / max_batch_tokens / concurrency。
- eval command、fixture dataset、query replay 和 UI/artifact 可视化仍待实现。

### Phase 3: Query Log Replay 与可视化

1. 加入真实 query log replay。
2. 管理界面或 artifact 页面展示低质量 query、BI 破坏项和回归项。
3. 将人工 feedback 作为 golden set 扩充来源。

## 逐 Query Diff 输出

每条 query 应能看到不同 plan 的差异：

```json
{
  "query_id": "config-001",
  "query": "DOCGRAPH_DATA_DIR 如何配置",
  "plans": [
    {
      "plan": "current-structural",
      "rank": 3,
      "hit": true,
      "top_sections": ["s2", "s7", "expected-s1"],
      "bi_block_split_count": 2,
      "notes": ["expected section found at rank 3", "config table split"]
    },
    {
      "plan": "block-aware-pack",
      "rank": 1,
      "hit": true,
      "top_sections": ["expected-s1", "s2", "s7"],
      "bi_block_split_count": 0,
      "notes": ["config table preserved"]
    }
  ]
}
```

## 验收标准

第一阶段完成标准：

- 至少 30 条 golden queries，覆盖 API/config/symbol/semantic/troubleshooting/concept。
- Markdown table 与 HTML table fixture 各至少 5 个。
- 至少能比较 `section-whole`、`current-structural`、`block-aware-pack` 三类 plan。
- 输出 hit@1/hit@3/hit@5/MRR、BI preserve rate、table split count、chunk_count、token_count。
- 每条 query 能查看 top results、snippet 和 vector trace。
- 任意 chunk algorithm 改动前后能跑同一套 fixture 并生成 diff。

`block-aware-pack` 完成标准：

- Markdown table 与 HTML table 都作为 BI 一等对象识别。
- 默认不在 table/code/list block 内切 chunk。
- 超预算 table 有 row-group/header-repeat 退化策略。
- 相比 `current-structural` 明显降低 BI split，且检索质量不低于 baseline 的可接受阈值。

第二阶段完成标准：

- 加入真实 query log replay。
- 加入 provider usage token 偏差统计。
- 管理界面或 artifact 页面可查看低质量 query、BI 破坏项和回归项。
- 人工 feedback 可进入 golden query / must preserve block 标注流程。

## 后续阶段

- BI parser/reference model：稳定识别 section 内 block spans。
- `block-aware-pack`：基于 block 的 section 内 packer。
- eval command/report：输出可复跑 JSON report。
- UI/artifact 可视化：查看低质量 query、BI 破坏和成本差异。
- query log replay：用真实查询补充 golden set。
- provider tokenizer 校准：对本地 embedding model 建立可验证 tokenizer mapping。
