# SpecForge

**Agent 原生的 OpenAPI 生成系统** —— 零注解、可溯源、确定性输出。

> 实现依据：《SpecForge v2.0 设计文档》（P0+P1 范围：静态全流水线 + IR + 确定性编译 + 评测闭环）

## 它做什么

给定一个 Go 仓库（fiber/gin 框架），不要求任何注解/注释，产出：

- **openapi.yaml** —— OpenAPI 3.1 规范文档
- **report.md** —— 置信度报告（低置信项 + 证据链 + 未解析项清单）

核心能力（对应设计文档 F1–F12 的 P0 子集）：

| 能力 | 实现 |
|---|---|
| 路由提取（含通配符组、中间件链、表驱动循环注册） | `internal/adapter/fiber.go` |
| 请求契约（BodyParser 绑定 + Query/Params/Get/Cookies 扫描 + binding 约束） | `internal/engine/contract.go` |
| **响应契约追踪**（调用图 + 响应汇聚点 + 信封矩阵） | `internal/slicing/slicer.go` |
| Schema 合成（嵌套/枚举/可空/omitempty/时间/RawMessage/any 降级） | `internal/typeschema/synth.go` |
| 横切（中间件→security、错误码目录、通配符展开） | `internal/profile` + engine |
| 确定性编译（$ref 去重、固定 key 序、逐字节稳定） | `internal/compiler` |
| 评测（路由召回/参数 F1/字段 F1/信封召回/幻觉率） | `internal/eval` |

## 架构（设计文档 §3.1 的 P0 形态）

```
loader ── go/packages 类型加载（等价 LSP 桥接）
   ↓
codegraph ── 符号表 / 调用边（含实参静态类型）/ 类型图 / 指纹
   ↓
adapter(fiber) ── 路由模式抽取（Group/Post/Use + 表驱动解析）
   ↓
slicing ── 正向可达 ∩ 响应汇聚点 → 信封矩阵（调用图深处找响应）
   ↓
typeschema ── Go 类型 → JSON Schema（§7 映射总表 + 边界 case）
   ↓
facts ── API Fact Graph（IR: route/contract/schema/security/errorcatalog/enrichment）
   ↓
compiler ── 八阶段流水线 → 确定性 YAML（双跑逐字节自检）
   ↓
eval ── ground truth 对比（七项指标）
```

## 快速开始

```bash
# 构建（Go 1.27）
go build -o specforge ./cmd/specforge

# 生成（对任何 Go + fiber 仓库）
./specforge gen --repo path/to/repo

# 带约定画像（信封/中间件语义/通配符规则）
./specforge gen --repo . --profile .specforge/profile.yaml

# 评测（对照 ground truth）
./specforge eval --truth truth.yaml --spec out/openapi.yaml
```

## 验证结果（本仓库 testdata/sample-repo）

样本仓库复刻了设计文档中的全部难点模式：`app.Group("/ipo/v*")` 通配符、
三层中间件链、`code.WriteResponse` 业务码信封（HTTP 200 + code/msg/data）、
service 层深处写响应、表驱动循环注册、`map[string]any`、`json.RawMessage`、
可空指针字段、`time.Time`、枚举常量组、跨接口共享类型。

```
$ specforge eval --truth ground-truth.yaml --spec openapi.yaml

Route Recall      : 1.000    （路由召回 ≥98% 达标）
Route Precision   : 1.000
Param F1          : 1.000    （请求字段 F1 ≥95% 达标）
Request Field F1  : 1.000
Response Field F1 : 1.000    （响应字段 F1 ≥90% 达标）
Envelope Recall   : 1.000    （状态码/信封召回 ≥85% 达标）
Hallucination     : 0.000    （幻觉率 <1% 达标）
```

确定性验证：双编译自检 + **跨进程两次运行逐字节一致**。

## 设计原则的落地位置

| 设计原则 | 落地 |
|---|---|
| 静态能算准的绝不问模型 | 全管线 0 token；LLM 层（`internal/infer`）仅在静态缺口时介入，无配置时显式降级 |
| LLM 产出事实而非 YAML | `internal/facts` 强类型 IR；编译器消费 IR |
| 无证据不成事实 | 每个事实带 `file:line + blobSha` 证据；unknown 显式标记绝不编造 |
| 确定性 | 固定 key 序渲染器 + 排序不变式 + `CompileTwiceCheck` |
| 增量就绪 | 代码图全产物带指纹（FileHash/TypeHash）；readSet 依赖三分法已在调用图查询落地 |

## 目录

```
cmd/specforge/          CLI（gen / eval / version）
internal/loader/        仓库摄入与服务拓扑
internal/codegraph/     符号表、调用图、类型图、指纹、局部类型
internal/adapter/       框架适配器（fiber；gin 部分）
internal/slicing/       程序切片与响应汇聚点追踪
internal/typeschema/    类型 → JSON Schema 合成
internal/facts/         API Fact Graph IR
internal/profile/       约定画像（信封/中间件/通配符）
internal/engine/        生成流水线编排
internal/compiler/      确定性编译器 + YAML 渲染
internal/eval/          评测基准（七项指标）
internal/infer/         LLM provider（OpenAI 兼容 + 离线降级）
testdata/sample-repo/   样本仓库（复刻 trade/ipoServer 难点模式）
testdata/ground-truth.yaml  人工标注真值
```

## 测试

```bash
go test ./...        # 单元 + 端到端精度回归守卫（满分基线）
```

端到端测试 `internal/engine/e2e_test.go` 锁定七项指标满分基线——
任何使精度回退或破坏确定性的改动都会失败。这是设计文档 §8.5
「文档进 CI gate」前提的测试化落地。

## 后续路线（P2+，见设计文档）

- SQLite 事实库 + readSet 反向索引 + 失效传播（P2 增量引擎）
- 约定画像 Agent 自动学习（P3，替代人工 profile.yaml）
- 多语言适配器（P4）、MCP Server 形态（P5）、运行时探针（P6）
