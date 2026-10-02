# SpecForge

**Agent 原生的 OpenAPI 生成器**：从没有注解的代码中提取真实 HTTP 契约，输出可追溯、可复现的 OpenAPI 3.1 文档。

SpecForge 把静态分析、调用图、类型合成和确定性编译组合成一条流水线。LLM 只处理静态分析明确标记的缺口，所有被采纳的事实都必须能回到源码证据。

## 当前能力

### Go 静态前端

仓库根目录包含 `go.mod` 时自动使用 Go 前端。内置适配器覆盖：

- Fiber (`github.com/gofiber/fiber/v2`)
- Gin (`github.com/gin-gonic/gin`)
- chi v4 (`github.com/go-chi/chi`)
- chi v5 (`github.com/go-chi/chi/v5`)

它们支持路由分组和中间件链、表驱动注册、chi 的 `Group`/`Route` 回调、请求绑定、路径/query/header/cookie 参数、响应写出、业务错误码和嵌套 JSON Schema。

### 通用源码前端

非 Go 仓库可以使用 `--frontend generic --llm`。它会按文件发现路由，再逐 operation 抽取请求和响应契约，并用源码文本核对路径、handler、字段和状态码。该模式需要 LLM，置信度上限为 0.6；未通过核对的结论会被拒绝并保留为缺口。

未登记的 Go Web 框架也可以在 `--llm` 下由模型生成适配器声明。通过符号和签名复核后，声明会保存到 `.specforge/adapters/`，后续运行可复用。

## 输出

`specforge gen` 默认写入 `<repo>/.specforge/out/`：

| 文件 | 用途 |
|---|---|
| `openapi.yaml` | 确定性渲染的 OpenAPI 3.1 文档 |
| `report.md` | 总览、低置信 operation、缺口和证据位置 |
| `operations.json` | 稳定的 operation 级契约和证据视图，供 `ops`、`explain` 和 Agent 消费 |
| `contract.json` | 规范化 Contract Graph，包含候选、来源和诊断 |

运行级缓存默认写入 `<repo>/.specforge/cache/`：`memo/` 缓存完整运行产物，`llm/` 缓存单次模型调用并校验模型实际读取的源码。缓存不含认证信息。

## 快速开始

环境要求：Go 1.27 或更高版本。

```bash
# 构建
go build -o specforge ./cmd/specforge

# 检查工具链、仓库和画像
./specforge doctor --repo path/to/repo
# read-only/shared checkout: keep cache outside the repository
./specforge doctor --repo path/to/repo --cache-dir /tmp/specforge-cache

# 生成 OpenAPI、报告和 operation 证据
./specforge gen --repo path/to/repo

# 指定服务和输出目录
./specforge gen --repo . --service service-ipo --out docs/api

# 使用仓库约定画像（默认会自动读取 .specforge/profile.yaml）
./specforge gen --repo . --profile .specforge/profile.yaml

# 摄入已有 OpenAPI 与脱敏 runtime JSONL（参数可重复）
./specforge gen --repo . --openapi docs/api/openapi.yaml --runtime observations.jsonl

# 摄入结构化人工契约事实（YAML/JSON）
./specforge gen --repo . --documentation docs/api-facts.yaml

# CI quality gates
./specforge gen --repo . --fail-on-conflict --fail-on-unresolved --min-confidence 0.8

# validate an existing OpenAPI document (resolves local refs)
./specforge verify --spec .specforge/out/openapi.yaml

# 查看低置信 operation，并解释单个 operation 的证据
./specforge ops --repo . --low-confidence
./specforge explain POST /api/orders/{id} --repo .

# 对照 ground truth 做评测；--fail-under 可作为 CI 闸门
./specforge eval --truth truth.yaml --spec .specforge/out/openapi.yaml --fail-under 0.9
```

没有 LLM 凭据时，Go 静态路径仍可离线运行；静态缺口会在 `report.md` 中显式保留。启用 `--llm`、`--llm-enrich` 或 `--llm-learn-profile` 时，需要设置：

```bash
export ANTHROPIC_AUTH_TOKEN=...
# 或 ANTHROPIC_API_KEY=...
export ANTHROPIC_BASE_URL=https://your-anthropic-compatible-edge.example/v1  # 可选
export ANTHROPIC_MODEL=deepseek-v4-pro-0813                                      # 可选
```

常用 LLM 控制项：`--llm-budget` 限制本次新增调用数，`--llm-concurrency` 控制并发，`--fail-on-degraded` 在调用失败或预算耗尽时返回非零退出码。默认单次调用超时可用 `SPECFORGE_LLM_TIMEOUT` 覆盖，设 `SPECFORGE_LLM_THINKING=off` 可关闭扩展思考。

## CLI 工作流

| 命令 | 作用 |
|---|---|
| `doctor` | 检查 Go 工具链、`go.mod`、框架依赖、画像、缓存和 LLM 配置 |
| `gen` | 分析仓库并生成三份产物 |
| `ops` | 从最近一次生成结果列出 operation、置信度和缺口 |
| `explain METHOD PATH` | 展示参数、请求体、响应矩阵和逐行证据 |
| `eval` | 计算 route、参数、请求/响应字段、信封召回和幻觉率 |
| `verify` | 使用 libopenapi 加载、解析引用并校验 OpenAPI 文档 |
| `cache stats` / `cache clean` | 查看或清理运行级和 LLM 缓存 |
| `version` | 打印 CLI 和引擎版本 |

分析和运维命令（`doctor`、`gen`、`ops`、`explain`、`eval`、`verify`、`cache`、`version`）支持 `--json` 机器模式。stdout 只输出一个版本化 JSON 信封，日志写到 stderr，适合 CI 和脚本消费。

## 架构

```
frontend
  ├─ Go: go/packages + codegraph + framework adapters
  └─ generic: LLM route/contract extraction with source verification
        ↓
facts      API Fact Graph（route / contract / schema / security / enrichment）
        ↓
slicing    调用图和值流追踪响应汇聚点与错误分支
        ↓
compiler   证据闸门、$ref 去重、稳定排序、OpenAPI 3.1 YAML
        ↓
eval       ground truth 对比与 CI 质量闸门
```

主要目录：

```
cmd/specforge/       CLI（doctor / gen / ops / explain / eval / verify / cache / version）
internal/frontend/   Go 静态前端与通用 LLM 前端
internal/adapter/    Fiber、Gin、chi 和可学习的框架适配器
internal/codegraph/  符号表、调用图、类型图和源码指纹
internal/slicing/    响应写出、值流和错误流追踪
internal/typeschema/ Go 类型到 JSON Schema 的合成
internal/facts/      API Fact Graph 中间表示
internal/compiler/   确定性编译器和 YAML 渲染器
internal/eval/       OpenAPI 评测指标和报告
internal/infer/      LLM provider、任务协议和调用缓存
internal/memo/       运行级缓存
testdata/            Fiber、Gin、chi、Echo、Express 和回归真值样本
```

## 验证

```bash
go test ./...
```

端到端回归覆盖 Fiber、Gin 和 chi，并检查路由、参数、请求/响应字段、信封、错误流、确定性和证据输出。样本真值评测可运行：

```bash
go run ./cmd/specforge gen --repo testdata/sample-repo --no-cache
go run ./cmd/specforge eval \
  --truth testdata/ground-truth.yaml \
  --spec testdata/sample-repo/.specforge/out/openapi.yaml \
  --fail-under 0.9
```

当前样本回归基线为 route recall、参数 F1、请求字段 F1、响应字段 F1 和 envelope recall 均为 1.000，幻觉率为 0.000；具体结果以本地测试和生成输入为准。

## 设计与路线图

- [`docs/architecture/design-v2.md`](docs/architecture/design-v2.md)：Fact 生命周期、证据协议、编译器、LLM 编排和增量引擎设计。
- [`docs/status/current-delivery.md`](docs/status/current-delivery.md)：当前仓库的实现状态、交付内容、验证命令和已知边界。
- [`docs/README.md`](docs/README.md)：文档目录和各文档的职责说明。

当前版本是全量分析 + 运行级 memo/LLM 缓存，并提供可选的 SQLite Contract Graph 快照缓存；按 readSet 的 operation 级反向失效传播仍属于后续增量引擎路线。Java、Python、Node 的静态前端以及进程级运行时探针尚未作为内置能力交付。
