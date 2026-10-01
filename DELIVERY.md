# SpecForge 当前交付说明

本文记录仓库当前代码的可运行能力。长期架构和产品规划见 [`DESIGN.md`](DESIGN.md)，面向使用者的安装和工作流见 [`README.md`](README.md)。

## 交付范围

### 已交付

- Go 1.27 CLI，命令包括 `doctor`、`gen`、`ops`、`explain`、`eval`、`verify`、`cache` 和 `version`。
- Go 静态前端：`go/packages` 类型加载、符号/调用图、值流和证据校验。
- 内置 Go 框架适配器：Fiber、Gin、chi v4、chi v5。
- 路由分组、中间件链、表驱动注册、通配符路径、chi `Group`/`Route` 回调和 `Method` 注册。
- 请求体绑定、query/path/header/cookie 参数、validator/binding 约束、响应矩阵和业务错误码追踪。
- Go 类型到 JSON Schema：嵌套结构、数组、枚举、指针可空、`omitempty`、时间、`json.RawMessage`、`map[string]any` 和值级字面量收窄。
- 确定性 OpenAPI 3.1 编译：固定排序、`$ref` 去重、证据闸门和编译双跑检查。
- libopenapi OpenAPI 边界：本地引用解析、文档规范校验、Overlay 应用后 reload 校验、OpenAPI 文档 Diff 和 Arazzo 工作流解析。
- libopenapi-validator HTTP 验证边界：请求、响应和文档诊断转换为稳定的 SpecForge 结果；运行时验证不覆盖未访问接口。
- Contract Graph 核心模型：多来源 Candidate、Evidence、冲突、未知 Schema 和显式 operation 状态。
- 通用源码前端：通过 LLM 发现非 Go 项目路由和契约，并对模型输出做源码核对。
- LLM 缺口补全、约定画像学习、语义增强、未登记 Go 框架适配器学习和内容寻址缓存。
- `--json` 机器契约、结构化退出码和 `eval --fail-under` CI 质量闸门。

### 生成物

每次 `gen` 在输出目录写入：

| 文件 | 说明 |
|---|---|
| `openapi.yaml` | OpenAPI 3.1 文档 |
| `report.md` | 置信度、缺口、失败 operation 和证据总览 |
| `operations.json` | schema version 1 的 operation 级证据接口 |

仓库内还会按需生成：

- `.specforge/profile.yaml`：输入画像，可手工维护；`--llm-learn-profile` 会在当前运行中补全缺失约定。
- `.specforge/adapters/*.json`：经源码签名复核的 LLM 学习适配器。
- `.specforge/cache/memo/`：完整运行缓存。
- `.specforge/cache/llm/`：按 prompt 和工具读集校验的 LLM 调用缓存。

## 使用与验证

```bash
go version
go test ./...

go run ./cmd/specforge doctor --repo .
go run ./cmd/specforge gen --repo testdata/sample-repo --no-cache
go run ./cmd/specforge verify --spec testdata/sample-repo/.specforge/out/openapi.yaml
go run ./cmd/specforge eval \
  --truth testdata/ground-truth.yaml \
  --spec testdata/sample-repo/.specforge/out/openapi.yaml \
  --fail-under 0.9
```

发布或部署时可先执行 `go build -o specforge ./cmd/specforge`，再用生成的二进制替换上面命令中的 `go run ./cmd/specforge`。

Go 静态前端没有 LLM 也能运行；不能静态确定的字段会保留为缺口。通用前端和显式的 `--llm`、`--llm-enrich`、`--llm-learn-profile` 需要 `ANTHROPIC_AUTH_TOKEN` 或 `ANTHROPIC_API_KEY`。完整环境变量、缓存和 CI 用法见 README。

当前测试覆盖：

- `internal/adapter`：Fiber、Gin、chi 路由形态和原语声明。
- `internal/engine`：样本仓库端到端生成、响应/错误流、确定性和证据输出。
- `internal/eval`：OpenAPI 评测、开放对象字段和响应信封展平。
- `internal/contract`：候选事实、证据、冲突解析和 JSON 往返不变量。
- `internal/openapi`：3.0/3.1 加载、引用、Overlay、Arazzo、文档 Diff 和 HTTP 验证适配器。
- `internal/frontend/generic`：LLM 路由/契约文本核对。

## 已知边界

- 默认 Go 前端只在仓库根目录存在 `go.mod` 时自动选择；可用 `--repo` 指向模块根。
- 通用前端依赖模型发现路由和契约，置信度上限为 0.6，且必须通过源码核对。
- 运行级缓存是 memo 和文件缓存，不是 SQLite 事实库；基于 readSet 的反向失效传播仍是后续路线。
- Java、Python、Node 尚无静态语言前端；Express 样本用于通用前端回归。
- 尚未提供进程级流量探针、PR 评论机器人或 MCP Server；HTTP 验证器可由调用方提供已捕获的请求/响应样本。
- 旧设计文档中的 Fiber-only、P0-only 和 `specforge/` 嵌套目录描述是历史背景；以本文件和 README 的当前实现说明为准。

## 版本与兼容性

CLI 和引擎版本来自 `internal/engine.Version`。`operations.json` 和 `--json` 信封当前为 schema version `1`。引擎版本会进入 memo 指纹，升级引擎后旧运行缓存会自动失效。
