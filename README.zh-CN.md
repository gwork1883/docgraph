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
- 支持 token 模式保护 Web/API/SSE 入口。

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
./bin/docgraph init --data ./.docgraph
./bin/docgraph serve --data ./.docgraph --host 127.0.0.1 --port 8787
```

打开 `http://127.0.0.1:8787` 后，可以在 Web UI 中添加和同步文档来源。

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

也可以通过 HTTP/SSE 方式使用，配置参考 [docs/mcp-setup.md](docs/mcp-setup.md)。

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
