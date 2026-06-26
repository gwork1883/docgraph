# DocGraph

[English](README.md) | 中文

DocGraph 是一个本地优先的文档知识图谱工具，用于把企业内的本地文档、代码仓库文档、HTML、Confluence、OpenAPI、SFTP 和网页内容统一索引到本地 SQLite，并通过 Web UI、HTTP API 和 MCP 工具提供给本地 agent 使用。

项目目标是让文档消费、检索、上下文拼装和影响分析尽量在本机或内网环境完成，避免把内部知识库同步到外部服务。

## 界面预览

Dashboard 展示当前本地知识库的来源、文档、section、节点、边和同步任务数量。

![DocGraph dashboard](docs/assets/screenshots/docgraph-dashboard.png)

Connectors 页面用于添加、编辑、同步和查看文档来源。

![DocGraph connectors](docs/assets/screenshots/docgraph-connectors.png)

Search 页面提供面向文档 section 和 profile 的本地检索。

![DocGraph search](docs/assets/screenshots/docgraph-search.png)

## 功能概览

- 单个 Go 可执行文件，内置 Web UI。
- 本地 SQLite + FTS5，不依赖外部数据库。
- 支持 local、git、static、html、sftp、confluence、openapi、webdocs 等来源。
- 支持 `doc_search`、`doc_context`、`doc_get_node`、`doc_get_section`、`doc_related`、`doc_impact` MCP 工具。
- 支持中文检索辅助 profile、本地知识图谱节点/边、同步任务历史和反馈标注。
- 支持 token 模式保护 Web/API/MCP 入口。

## 搜索优势

DocGraph 的搜索面向文档消费和 agent 上下文获取，不只是普通全文匹配：

- 多阶段本地召回结合 FTS5 分词检索、trigram 检索、profile 检索和子串兜底，让中文短语、英文术语、API 名称和符号都能参与召回。
- 结果以 section 为核心，返回具体文档、标题路径、片段、来源 URL 和命中证据，而不是只给整篇文件。
- 检索 profile 会确定性生成标签、关键短语、别名、API 引用和 section 分布信号，同时把人工维护的文档描述与同步生成的元数据分开。
- 排序会综合 canonical 状态、标题和 heading 命中、词项覆盖、profile 命中、精确匹配以及已批准的知识关系。
- MCP 工具默认先返回有边界的搜索摘要，agent 需要时再按 section 拉取全文，避免一次性塞入过多本地上下文。

## 工作原理

DocGraph 会把已有文档转换成本地可查询的知识层：

1. 连接来源：添加本地目录、Git 仓库、静态 HTML、Confluence 页面、OpenAPI 文件、SFTP 目录或在线文档站点。
2. 统一文档模型：每个 connector 把来源内容转换成通用的 document 和 section。
3. 本地索引：文档、section、检索 profile、节点、边、别名和同步任务都存入 SQLite，并使用 FTS5 做全文检索。
4. 构建关系：从内容中生成 product、module、document、section、API 等节点，并用带证据的边连接起来。
5. 带证据查询：用户和 agent 可以通过 Web UI、REST API 或 MCP 检索 section、拼装任务上下文、查看相关节点和做影响分析。

除非你主动暴露服务或移动数据库，数据会保留在配置的本地数据目录里。

## 快速开始

```bash
go build -buildvcs=false -o bin/docgraph ./cmd/docgraph
./bin/docgraph serve
```

首次 `serve` 会创建 `docgraph.yaml`、生成本地管理秘钥、迁移 SQLite 数据库并启动服务。打开 `http://127.0.0.1:8787`，输入 `docgraph.yaml` 里的 token 后，可以在 Web UI 中添加和同步文档来源。`docgraph init` 仍然保留，用于只预创建或检查配置和数据库、不启动服务的运维场景。

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

DocGraph 支持 stdio MCP：

```bash
./bin/docgraph mcp --data ./.docgraph
```

运行 `docgraph serve` 时，也可以通过 `/mcp` 使用 Streamable HTTP MCP；
旧的 HTTP/SSE 兼容入口仍保留在 `/mcp/sse`。配置参考 [docs/mcp-setup.md](docs/mcp-setup.md)。

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
