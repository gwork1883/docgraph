# DocGraph

[English](README.md) | 中文

DocGraph 是一个本地优先的文档知识图谱工具，用于把企业内的本地文档、代码仓库文档、HTML、Confluence、OpenAPI、SFTP 和网页内容统一索引到本地 SQLite，并通过 Web UI、HTTP API 和 MCP 工具提供给本地 agent 使用。

项目目标是让文档消费、检索、上下文拼装和影响分析尽量在本机或内网环境完成，避免把内部知识库同步到外部服务。

## 界面预览

Dashboard 展示当前本地知识库的来源、文档、section、节点、边和同步任务数量。

![DocGraph dashboard](docs/assets/screenshots/docgraph-dashboard.png)

Connectors 页面用于添加、编辑、同步和查看文档来源，支持 8 种连接器类型。

![DocGraph connectors](docs/assets/screenshots/docgraph-connectors.png)

Search 页面提供混合本地检索，结合文档 section、检索 profile 和向量嵌入。

![DocGraph search](docs/assets/screenshots/docgraph-search.png)

同步任务页面展示任务历史和状态追踪，以及每个来源的 cron 定时同步调度。

![DocGraph sync tasks](docs/assets/screenshots/docgraph-sync-tasks.png)

凭证管理页面管理 Confluence cookie 凭证，用于会话式访问。

![DocGraph credentials](docs/assets/screenshots/docgraph-credentials.png)

治理页面审核过期文档、知识关系提案和反馈标注。

![DocGraph governance](docs/assets/screenshots/docgraph-governance.png)

节点页面浏览和搜索知识图谱——产品、模块、文档、section 和 API 节点及其关系。

![DocGraph nodes](docs/assets/screenshots/docgraph-nodes.png)

## 功能概览

- 单个 Go 可执行文件，内置 Web UI。
- 本地 SQLite + FTS5，不依赖外部数据库。可选 pgvector 实现混合向量搜索。
- 支持 local、git、static、html、sftp、confluence、openapi、webdocs 等来源。
- 支持 `doc_search`、`doc_get_node`、`doc_get_section`、`doc_get_asset_uri`、`doc_related`、`doc_impact` MCP 工具；旧版 `doc_context` 调用仍兼容，但不再主动暴露。
- 支持本地知识图谱节点/边、同步任务历史和反馈标注。
- 混合向量搜索 + RRF 融合排序：FTS5 文本检索与 pgvector 向量相似度检索融合，按查询意图（entity/conceptual/general）自动调节权重。
- 嵌入管线：可配置 OpenAI 兼容 embedding provider，支持 auto/sentence/fixed 分块策略，带版本追踪和过期检测。
- 基于 GSE 的搜索分词器，CJK 精确分词配合英文 token 分析。
- 技术术语提取：从标题、表格和代码类内容中提取 API 名称和配置键，提升精确检索召回。
- 显式交叉引用解析：作者写的文档引用（如"参见：[配置指南](../config.md)"）作为 `suggested_reads.explicit_references` 返回。
- 定时同步调度：每个源可配置 cron 表达式自动同步；Job 管理 API 支持查看和取消同步任务。
- Confluence cookie 凭证管理：CRUD API + Web UI 管理 Confluence 会话 cookie。
- 支持中文检索辅助 profile 生成。
- 支持 token 模式保护 Web/API/MCP 入口。
- MCP 支持 stdio 和 Streamable HTTP 两种传输方式；旧版 SSE 端点保留兼容。

## 搜索优势

DocGraph 的搜索面向文档消费和 agent 上下文获取，不只是普通全文匹配：

- 多阶段本地召回结合 FTS5 分词检索、trigram 检索、profile 检索和子串兜底，让中文短语、英文术语、API 名称和符号都能参与召回。
- 可选混合向量搜索增加 pgvector 余弦相似度车道，与文本车道通过 RRF（Reciprocal Rank Fusion）融合排序。意图路由自动识别 entity/conceptual/general 查询并调整文本/向量权重。
- 结果以 section 为核心，返回具体文档、标题路径、片段、来源 URL 和命中证据，而不是只给整篇文件。
- 检索 profile 会确定性生成标签、关键短语、别名、API 引用、技术术语和 section 分布信号，同时把人工维护的文档描述与同步生成的元数据分开。
- 作者写的显式交叉引用（如"参见：[配置指南](../config.md)"）会被解析并作为推荐阅读返回，和知识图谱关系并列展示。
- 排序会综合 canonical 状态、标题和 heading 命中、词项覆盖、profile 命中、精确匹配、向量相似度和已批准的知识关系。
- MCP 工具遵循渐进式 `doc_search → doc_get_section → doc_get_asset_uri`：搜索和 section 只返回紧凑的 connector 资产 ID 与 MIME type，只有 URI tool 返回鉴权下载路径，MCP 响应不内嵌二进制内容。

## 工作原理

DocGraph 会把已有文档转换成本地可查询的知识层：

1. 连接来源：添加本地目录、Git 仓库、静态 HTML、Confluence 页面、OpenAPI 文件、SFTP 目录或在线文档站点。
2. 统一文档模型：每个 connector 把来源内容转换成通用的 document 和 section。
3. 本地索引：文档、section、检索 profile、节点、边、别名和同步任务都存入 SQLite，并使用 FTS5 做全文检索。可选将 section chunk 嵌入 pgvector 实现混合向量检索。
4. 构建关系：从内容中生成 product、module、document、section、API 等节点，并用带证据的边连接起来。显式交叉引用和技术术语会被提取，丰富检索信号。
5. 带证据查询：用户和 agent 可以通过 Web UI、REST API 或 MCP（stdio 和 Streamable HTTP）检索 section、拼装任务上下文、查看相关节点和做影响分析。

除非你主动暴露服务或移动数据库，数据会保留在配置的本地数据目录里。

## 快速开始

```bash
go build -buildvcs=false -o bin/docgraph ./cmd/docgraph
./bin/docgraph serve
```

首次 `serve` 会创建 `docgraph.yaml`、生成本地管理秘钥、迁移 SQLite 数据库并启动服务。打开 `http://127.0.0.1:8787`，输入 `docgraph.yaml` 里的 token 后，可以在 Web UI 中添加和同步文档来源。`docgraph init` 仍然保留，用于只预创建或检查配置和数据库、不启动服务的运维场景。

启用混合向量搜索，在 `docgraph.yaml` 里添加 `vector_search` 配置：

```yaml
vector_search:
  enabled: true
  search_weight: 0.4
  embedding:
    provider: openai
    model: text-embedding-3-small
    api_url: https://api.openai.com/v1/embeddings
    api_key: sk-...
    dimensions: 1536
  vector_db:
    dsn: pgvector://user:pass@localhost:5432/docgraph?sslmode=disable
```

完整配置选项（意图路由权重、分块策略、嵌入管线调参等）参见 `docgraph.yaml` 中的配置参考。

如果需要把 DocGraph 部署在反向代理的路径前缀后面，可以在 `docgraph.yaml` 里设置 `server.web_prefix`。例如 `web_prefix: docgraph` 会把 Web UI、REST API 和 HTTP MCP 端点都放到 `/docgraph/` 下；留空则保持默认根路径。nginx 示例见 `scripts/nginx-docgraph.conf` 和 `scripts/nginx-docgraph-location.conf`，示例会保留前缀转发给 DocGraph。

也可以通过 CLI 添加本地文档：

```bash
./bin/docgraph source add --data ./.docgraph --name "Docs" --dsn /path/to/docs
./bin/docgraph source sync --data ./.docgraph --id src_xxx
./bin/docgraph search --data ./.docgraph "接口鉴权"
./bin/docgraph context --data ./.docgraph --max-sections 5 "排查接口鉴权失败"
```

## 常用开发命令

```bash
make test
make build
make build-release
make run
```

`make test` 会执行 `go test -buildvcs=false ./...`。Makefile 默认把 Go 构建缓存放在仓库内的 `.gocache/` 和 `.gomodcache/`，这些目录已被 `.gitignore` 忽略。

## 发布二进制

仓库包含 GitHub Actions workflow：`.github/workflows/release.yml`。

- 手动运行 `release` workflow：构建并上传 artifacts。
- 推送 `v*` tag：自动测试、构建并创建 GitHub Release。
- 默认产物覆盖 `linux/windows/darwin` 的 `amd64/arm64`。
- Release 中包含每个平台压缩包和 SHA-256 checksum 文件。
- 如果要手动重建已有 release，运行 `release` workflow 时填写 `release_tag`，例如 `v0.1.0`。

示例：

```bash
git tag v0.1.0
git push origin v0.1.0
```

## MCP 使用

DocGraph 支持两种 MCP 传输方式：

| 传输方式 | 端点 | 适用场景 |
|-----------|------|----------|
| **stdio** | `docgraph mcp` | 本地 agent、IDE 插件（Claude Code、VS Code、JetBrains） |
| **Streamable HTTP** | 运行中服务器的 `/mcp` | 远程 agent、HTTP 集成 |
| **旧版 SSE** | `/mcp/sse` | 旧客户端兼容；已弃用但保留可用 |

stdio 模式：

```bash
./bin/docgraph mcp --data ./.docgraph
```

Streamable HTTP 在 `docgraph serve` 运行时可用。启用 `auth.mode: token` 后，`/mcp` 与旧版 `/mcp/sse` 和 REST API 一样，需要 Bearer token 或 `X-DocGraph-Token`。完整配置参考 [docs/mcp-setup.md](docs/mcp-setup.md)。

## 运行时依赖

DocGraph 本体是单个 Go binary。部分连接器在运行时需要本机工具：

- Git 来源：需要本机可执行 `git`。
- SPA 网页抓取：需要 Chrome、Chromium、Edge，或设置 `ROD_BROWSER_BIN` 指向浏览器可执行文件。

## 文档

- [产品需求](docs/docgraph-prd.md)
- [架构说明](docs/docgraph-architecture.md)
- [MCP 配置](docs/mcp-setup.md)
- [验收清单](docs/docgraph-acceptance.md)

## 安全提示

- 不要把真实 token、cookie、私钥、内部域名或本地数据库提交到仓库。
- 生产或团队共享环境建议启用 `auth.mode: token`。
- `.docgraph/`、`.docgraph-test/`、`.gocache/`、`.gomodcache/`、`bin/` 已被忽略。

## 许可证

DocGraph 使用 [Server Side Public License v1.0](LICENSE)。

公司内部使用、内部部署和内部二次开发是允许的。若将 DocGraph 或其修改版本的功能作为服务提供给第三方，必须遵守 SSPL 第 13 条，公开对应的 Service Source Code，或另行取得商业授权。
