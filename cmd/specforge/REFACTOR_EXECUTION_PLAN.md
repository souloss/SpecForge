# SpecForge 重构与优化执行计划

> 面向干净 Agent 的执行文档。目标是完成架构演进、逐步迁移、回归验证，并保留可审查的中间结果。除非某阶段明确要求，不要一次性重写整个流水线。

## 1. 背景与目标

SpecForge 需要合并多种 API 信息来源：源码静态分析、已有 OpenAPI 文档、运行时请求与响应、Overlay/Arazzo 工作流，以及受证据约束的 LLM 推断。最终既要生成可信的 OpenAPI，也要能解释某项 API 信息来自哪里、哪些地方存在冲突、哪些类型无法解析。

本次重构要达成：

- 建立唯一的 SpecForge 业务模型，承载跨来源的候选事实、证据、冲突和置信度。
- 用 libopenapi 处理 OpenAPI 文档边界能力，包括加载、引用解析、Overlay/Arazzo 和文档校验等。
- 用 libopenapi-validator 作为默认文档及 HTTP 请求/响应验证实现。
- 不把 kin-openapi 引入为第二个核心模型；仅在明确需要兼容或差异对照时，通过适配器可选使用。
- 让源码分析、文档导入、运行时采集、合并、报告和 OpenAPI 导出各处于清晰层次。
- 将所有未解析项和冲突暴露出来，禁止静默降级为不准确的 Schema。
- 生成可重复、可重新加载、经过规范验证的 OpenAPI 文件。

目标数据流：

```text
源码分析 ───────┐
OpenAPI 导入 ───┼──> Contract Graph ──> resolved contract ──> OpenAPI 文档
运行时采集 ────┘          │                                  │
                          ├── 来源和证据                    ├── 引用解析
                          ├── 候选值和冲突                  ├── 规范校验
                          ├── 置信度和缺口                  └── 确定性输出
                          └── Contract Graph Diff
```

## 2. 架构决策

### 2.1 Contract Graph 是唯一业务合并模型

Contract Graph 是 SpecForge 的规范事实模型，不是 OpenAPI 文档模型。它必须能表示多个来源同时针对同一个字段给出不同结论，并保留来源、证据、状态和置信度。

至少应承载：

- Operation、path、method、参数位置、请求体、响应状态码、响应头和响应体。
- Schema、引用、组合类型、联合类型和无法解析的类型节点。
- 源码事实、OpenAPI 声明、运行时观察和 LLM 推断。
- 每个事实对应的文件/行号、Go 符号、文档位置或运行时 Trace 信息。
- 同一字段的多个 Candidate、冲突、缺失、未知和解析诊断。
- 置信度、推断状态和证据强度。

不要把 libopenapi 或 kin-openapi 的类型当作 Contract Graph。那些库表达的是 OpenAPI 文档；Contract Graph 表达的是 SpecForge 收集和判断事实的过程及结果。把证据塞进 `x-specforge-*` 扩展不能替代多来源候选模型，且会让核心模型依赖一种文档序列化方式。

### 2.2 libopenapi 是 OpenAPI 文档边界

libopenapi 仅放在 OpenAPI 适配层，负责文档解析、低/高层模型、引用、Overlay/Arazzo 和 OpenAPI 文档操作。它的类型不得泄漏到源码分析和 Contract Graph。

截至本计划编写时，官方项目资料列出 libopenapi 对 OpenAPI 3.0/3.1/3.2 以及 Overlay、Arazzo 和工具能力的支持；项目版本仍应在实施时检查 Go 兼容性、发行记录和 API 变更后锁定。参考：[libopenapi 官方仓库](https://github.com/pb33f/libopenapi)、[libopenapi releases](https://github.com/pb33f/libopenapi/releases)。

### 2.3 libopenapi-validator 是默认验证器

使用独立的 libopenapi-validator 适配器验证 OpenAPI 文档以及 HTTP 参数、请求和响应。Validator 类型和诊断在适配器边界内转换为 SpecForge 自己的结果结构。

参考：[libopenapi-validator 官方仓库](https://github.com/pb33f/libopenapi-validator)、[验证文档](https://pb33f.io/libopenapi/validation/)。

### 2.4 kin-openapi 是可选适配器，不是第二个核心模型

默认不引入 kin-openapi。仅在以下需求明确出现时实现独立适配器：

- 需要兼容使用 `openapi3filter` 的既有集成。
- 需要 Swagger 2.0 兼容。
- 需要其 Go Router 集成或其他特定能力。
- CI 需要与另一实现进行解析/验证差异对照。

通过 SpecForge 接口抽象验证能力，例如：

```go
type RuntimeValidator interface {
    ValidateRequest(*http.Request) ValidationResult
    ValidateResponse(*http.Request, *http.Response) ValidationResult
}
```

默认实现使用 libopenapi-validator。若增加 kin-openapi，则只在 `internal/runtime` 或专用 OpenAPI adapter 中出现，不能进入 Contract Graph、源码分析器、Compiler 或报告模型。参考：[kin-openapi 官方仓库](https://github.com/getkin/kin-openapi)、[kin-openapi releases](https://github.com/getkin/kin-openapi/releases)。

### 2.5 区分两种 Diff

- OpenAPI 文档 Diff：对比两个文档版本的语义变化，由 OpenAPI 文档适配层处理。
- Contract Graph Diff：对比源码、文档、运行时证据的事实差异，例如“源码新增但文档缺失”。由 SpecForge 核心模型处理。

不能把文档 Diff 的结果当成跨来源事实比较。

## 3. 目标目录与依赖方向

建议逐步演进为以下结构；迁移时先适应当前仓库的目录习惯，不要为匹配树形结构做无关搬迁。

```text
internal/
  contract/
    model.go
    operation.go
    schema.go
    evidence.go
    candidate.go
    diagnostics.go
    confidence.go
    reconcile.go
    merge.go
    diff.go

  source/
    golang/
    generic/

  runtime/
    capture.go
    validator.go
    libopenapi.go
    kinopenapi.go       # 仅当有明确需求时实现

  openapi/
    document.go
    import.go
    export.go
    resolve.go
    validate.go
    diff.go
    overlay.go
    arazzo.go
    libopenapi/
      loader.go
      importer.go
      exporter.go
      validator.go

  compiler/
    output.go
    deterministic_yaml.go

  report/
    report.go
    gaps.go
    conflicts.go
```

依赖方向：

```text
source ─────┐
runtime ────┼──> contract
openapi ────┘

contract ──> report
contract ──> OpenAPI export adapter
OpenAPI adapter ──> libopenapi / libopenapi-validator
Runtime adapter ──> validator implementation(s)
compiler ──> contract + OpenAPI export adapter
```

禁止核心层反向依赖第三方 OpenAPI 模型。例如源码适配器不得创建 libopenapi 对象，Contract Graph 不得包含 kin-openapi 类型，Compiler 不得调用源码解析器。

## 4. 核心数据模型要求

### 4.1 Evidence

```go
type Evidence struct {
    Source   EvidenceSource
    Location Location
    Summary  string
    Strength EvidenceStrength
}
```

`EvidenceSource` 至少包括：`source`、`openapi`、`runtime`、`documentation`、`llm`。

Location 至少能表达：

- 源码文件、起止行列、包、符号和函数。
- 路由注册点、Handler 调用链和相关 middleware。
- OpenAPI 文件和节点位置，包含外部 `$ref` 的来源文件。
- Runtime 请求、响应、采集时间和 Trace ID。

敏感请求/响应数据不得原样写入报告或 Graph；采用脱敏或仅保存结构性证据。

### 4.2 Candidate 与状态

同一个字段在合并决策之前必须能有多个候选值：

```go
type Candidate[T any] struct {
    Value      T
    Evidence   []Evidence
    Confidence float64
    Status     CandidateStatus
}
```

状态至少区分：

```text
verified
declared
observed
inferred
conflict
unresolved
inconclusive
```

每个 Operation 还应能标记 `source_only`、`document_only`、`observed_only`、`matched`、`conflict`、`unresolved`、`verified` 等聚合状态。

### 4.3 Schema 表达能力

Contract Graph Schema 必须能表示 OpenAPI 3.0/3.1 中实际遇到的结构，包括：

- `$ref` 和引用来源。
- `allOf`、`oneOf`、`anyOf`、`not`。
- `items`、`additionalProperties`。
- primitive/union types、nullable 语义。
- enum、format、default、examples、discriminator。
- required、readOnly、writeOnly。
- 无法解析或不支持的节点及诊断。

不能把未知类型静默替换为 `{ type: object }`、空 Schema 或 `any` 而不留诊断。

## 5. 合并策略

所有来源合并规则必须显式编码，不能靠 map 覆盖顺序或“最后写入获胜”。合并前先保存候选及证据，之后生成单独的 resolved view。

### 5.1 Path 和 Method

源码路由注册是应用实际暴露行为的主要证据；文档是声明，运行时是已观察样本。运行时未观察到某操作不表示该操作不存在。

建议规则：

- 源码注册用于确认当前构建中可见的 Path/Method。
- 文档补充 summary、description、example 和声明的约束。
- 运行时可以发现源码扫描遗漏的实际入口或行为，但要标记观察范围。
- 不同来源路径不一致时保留冲突，报告规范化过程和双方证据。

### 5.2 请求参数

建议证据优先顺序：源码绑定/读取/校验、文档声明、运行时样本、LLM 推断。

必须区分 Path、Query、Header、Cookie、Body。单次运行时请求只证明观察到了某个值，不证明字段可选性、枚举全集或结构完整性。

### 5.3 响应与错误码

跟踪源码中的显式响应写入、框架返回类型、middleware、错误映射、recovery 和短路处理。支持一个 Operation 有多个成功与错误响应码；不能只从正常分支推导响应。

必须覆盖并测试：

- `WriteHeader`、`http.Error`、JSON 错误对象。
- middleware 统一错误、鉴权拒绝、限流、panic/recovery。
- 框架的错误到 HTTP 状态码映射。
- 同一个 Handler 多个状态码和多个响应体类型。

### 5.4 冲突处理

发生冲突时：

1. 保留所有 Candidate 和 Evidence。
2. 只在有规则或证据证明等价时自动归并。
3. 不确定时标记 `conflict`，在报告列出差异。
4. 由输出策略决定报错、保守生成或要求人工决策。
5. 不要为了消除冲突而无条件生成 `anyOf`/`oneOf`。

### 5.5 LLM 兜底边界

LLM 可以解释现有证据、补充说明、给出候选 Schema 或辅助识别字段语义。所有结果必须保留模型/版本、输入证据、输出、置信度，并保持 `inferred`。

LLM 不得在没有证据时创建 Path、状态码或字段，不得覆盖源码事实和运行时事实，也不得将推断升级为 `verified`。不可靠或证据不足时输出 `unresolved`。

## 6. OpenAPI 导入和导出

### 6.1 导入

```text
原始 YAML/JSON
  -> libopenapi low-level document
  -> high-level model / refs resolve
  -> Contract Graph candidates
```

要求：

- 保留原始文件和位置来源，外部 `$ref` 要能追溯到源文件。
- 内外部引用都要解析或生成明确诊断；循环引用有保护。
- 未知/未支持结构不得静默丢弃。
- 导入映射覆盖所支持的 OpenAPI Schema 结构。
- 3.0 与 3.1 的语义差异不能未经验证地折叠。

### 6.2 导出

```text
Contract Graph
  -> 检查缺口/冲突
  -> 选择 resolved candidates
  -> OpenAPI 投影
  -> libopenapi load / refs / validation
  -> deterministic YAML 输出
  -> 再次加载验证
```

输出前至少检查：文档结构、Schema、`$ref`、Path/Operation、每个 Operation 的 Response、参数与 RequestBody 冲突、安全定义和引用。生成结果必须可以重新加载。

确定性 YAML 可以保留现有 renderer，但它只能序列化已经解析和验证的 OpenAPI 投影，不再负责 Schema 语义合并。

## 7. Overlay、Arazzo 和运行时

### 7.1 Overlay

Overlay 作为文档变换处理，不直接修改 Contract Graph：

```text
加载 base -> 校验 base -> 加载并应用 Overlay -> 校验结果 -> 导入 Graph
```

保存 base、overlay、每个 action 的 target 和执行诊断。Overlay 产生的信息要标记为文档/Overlay 来源。

### 7.2 Arazzo

Arazzo 保持为独立 Workflow Graph，通过引用关联 Contract Graph Operation。至少表达 Workflow、Step、Operation reference、Inputs、Outputs、Success criteria、Failure actions 和运行结果。

区分“Operation 存在”“Workflow 引用 Operation”“Workflow 执行过”“Workflow 执行成功”，不要把 Workflow 执行成功误当成接口完整性证明。

### 7.3 Runtime 验证

```text
HTTP request
  -> 匹配 Contract Graph operation
  -> 验证 request
  -> 执行真实 handler
  -> 捕获 response
  -> 验证 response
  -> 写入 Runtime Evidence
```

结果至少区分 `operation_not_found`、`request_invalid`、`handler_error`、`response_invalid`、`schema_unresolved`、`valid`、`inconclusive`。运行时只证明已访问样本，不代替静态分析，也不能证明接口全集。

## 8. 仓库迁移重点

先基于当前仓库确认真实路径、调用关系、已有未提交修改和测试基线；不要假设下面路径在分支中没有变化。

- `internal/engine/engine.go`：保留流水线编排，逐步让阶段产出/消费 Contract Graph。
- `internal/eval/eval.go`：检查并替换简化版 OpenAPI parser，禁止继续扩大手写 `oasDoc`/`oasSchema` 的覆盖范围。
- `internal/compiler/yaml.go`：保留确定性序列化能力；移除其中的业务合并和 Schema 猜测；输出前接规范校验。
- `internal/schema`：确认它与 Contract Graph Schema 的职责。可先作为 Go 源码侧 IR，再由适配器转换；避免出现两个事实权威模型。
- 现有框架适配器和路由/Handler 分析：仅输出带来源的事实，不直接创建 OpenAPI library objects。

## 9. 分阶段执行步骤与验收

### 阶段 A：基线与工作区审计

1. 检查 `AGENTS.md`、CodeGraph 和仓库结构；存在 `.codegraph/` 时按仓库要求先用 CodeGraph 定位入口。
2. 检查工作区状态，识别并保留用户已有变更；不要覆盖或回滚无关改动。
3. 运行当前测试与 build，记录失败及原因。
4. 对 Meridian 固定基线生成 OpenAPI 和报告，记录 Route、resolved/unresolved、Schema、缺口、响应码、未知类型统计。
5. 固定输入版本、命令、输出和统计，形成可重复比较的 baseline。

验收：基线命令可复跑；既有失败与重构引入的失败可区分；用户已有改动未被覆盖。

### 阶段 B：OpenAPI adapter 和依赖验证

1. 检查当前 Go 版本及候选 libopenapi、libopenapi-validator 版本的兼容性和 breaking changes。
2. 锁定版本并添加依赖。
3. 建立 OpenAPI adapter：Load、Resolve、Validate，统一转换错误和源位置诊断。
4. 添加最小导入/加载样本：3.0、3.1、内部/外部 `$ref`、无效文档。
5. 若确定有 Swagger 2 或 `openapi3filter` 等需求，再提出 kin-openapi adapter；否则暂不加入。

验收：依赖可在仓库当前 Go 版本下构建；成功文档能加载；坏引用和坏文档给出文件/位置/诊断；第三方对象未泄漏到核心包。

### 阶段 C：Contract Graph 最小模型

1. 定义 Operation、Parameter、RequestBody、Response、Schema、Evidence、Candidate、Diagnostic。
2. 定义候选状态、来源、证据强度和置信度的序列化格式。
3. 设计未知 Schema 和冲突的显式表示。
4. 实现稳定的 Graph 序列化，供测试、报告和调试使用。
5. 编写模型不变量测试。

验收：同一个字段可保存来自源码、文档、运行时的多个候选；可往返序列化；未知类型和冲突不会丢失。

### 阶段 D：OpenAPI 双向转换

1. 实现 OpenAPI -> Contract Graph importer。
2. 实现 Contract Graph resolved view -> OpenAPI exporter。
3. 映射 Schema、Operation、参数和多状态码响应。
4. 在 adapter 层处理引用和版本差异。
5. 加入 round-trip 测试并记录有意无法往返的字段。

验收：内部/外部引用、组合 Schema、错误响应和主要文档字段可以导入/导出；不能映射的结构有诊断；没有静默丢字段。

### 阶段 E：迁移源码分析

1. 让路由提取输出 route facts 和源码证据。
2. 让 Handler 参数/响应分析输出候选事实，而不是直接拼 OpenAPI。
3. 让 Schema 推导输出来源明确的 Schema candidate。
4. 追踪 middleware、统一错误、鉴权、recovery 和多响应码。
5. 将所有阶段汇入 Contract Graph，由独立 reconciliation 阶段生成 resolved view。

验收：源码分析不依赖 libopenapi/kin-openapi；每个生成的 Path、参数、响应码和 Schema 可追溯到源码证据或其他明确来源。

### 阶段 F：合并、冲突、缺口和 LLM 状态

1. 编码字段级来源合并规则。
2. 实现冲突保留和明确的 unresolved/inconclusive 状态。
3. 实现可解释的置信度计算，并输出组成依据。
4. 让报告逐项列出路径、方法、字段、证据、原因和状态。
5. 保证 LLM 结果始终标为 inferred，不能覆盖更强证据。

验收：合并顺序变化不改变已验证事实；冲突不被覆盖；无法解析项可定位；LLM 推断不会伪装成验证事实。

### 阶段 G：迁移 Compiler 与现有 eval

1. 让 Compiler 只消费 resolved Contract Graph。
2. 通过 OpenAPI adapter 生成并验证 OpenAPI 投影。
3. 保留 deterministic YAML renderer 作为输出层。
4. 替换 `internal/eval` 的缩减 OpenAPI parser，统一使用 OpenAPI adapter。
5. 加入生成后 reload、ref resolve、validation 和 round-trip 检查。

验收：生成文件可以再次加载和校验；两次相同输入输出字节一致；Compiler 不做来源合并或未知类型猜测。

### 阶段 H：Runtime validator

1. 定义 SpecForge `RuntimeValidator` 和统一 `ValidationResult`。
2. 默认接入 libopenapi-validator。
3. 将 request/response 结果及脱敏后的运行证据写回 Graph。
4. 用真实 HTTP 测试覆盖 path/query/header/body 和错误响应。
5. 仅在明确的兼容或差异测试需求下接入 kin-openapi；若实现 `both` 模式，差异要报告而非自动隐藏。

验收：无效请求和无效响应可分别定位；接口未被访问不会误判为不存在；验证器结果映射为稳定的 SpecForge 诊断。

### 阶段 I：Overlay、Arazzo、Diff

1. 实现 base/Overlay 加载、应用前后校验和来源记录。
2. Arazzo 作为独立 workflow 模型，验证 Operation references。
3. 实现 OpenAPI 文档 Diff 与 Contract Graph Diff 两条独立路径。
4. 为 Overlay action、Arazzo step 和 Diff diagnostics 增加测试。

验收：Overlay 结果可重新加载/校验；坏引用可定位；文档 Diff 不冒充跨来源事实对比；Graph Diff 能标识源码新增但文档缺失等差异。

### 阶段 J：端到端回归与清理

1. 运行所有测试、race、vet、build。
2. 运行框架和类型样本集及 Meridian 基准。
3. 比较新旧输出，审查每项差异；不能只以操作数增加作为正确性依据。
4. 确认所有未知和冲突都在报告中暴露。
5. 删除已无使用者的简化 parser 或重复模型；通过引用搜索和测试确认后再删。
6. 更新设计文档、用户命令说明和基准结果。

验收：无回归；所有剩余差异可解释；完整执行记录包含命令、结果、未解决项及其原因。

## 10. 测试矩阵

### 单元和 fixture 测试

- OpenAPI 3.0/3.1，内部/外部 `$ref`，循环引用和坏引用。
- `allOf`、`oneOf`、`anyOf`、`not`、union、`additionalProperties`、nullable、discriminator。
- enum、format、default、examples、readOnly/writeOnly、required。
- Path/Query/Header/Cookie 参数、request body、多响应码和错误响应。
- Overlay、Arazzo 引用、冲突、置信度和 unresolved 报告。

### Go 源码样本

至少覆盖：标准 `net/http`、chi、gin、echo；路由组/middleware；统一错误处理；嵌套/别名/泛型类型；interface/union；不同 Handler 注册写法；多个响应状态码和 body 类型。

### Contract Graph 不变量

1. 导入 -> 导出 -> 再导入不丢失承诺支持的关键字段。
2. 相同输入多次生成结果字节稳定。
3. 调换来源输入顺序不会覆盖已验证事实。
4. Runtime sample 不会覆盖静态事实。
5. LLM inference 不会升级成 verified。
6. 未解析类型不静默变成空对象。
7. 未访问的运行时分支不被标记为不存在。
8. 每个最终 OpenAPI 字段能追溯证据或显式生成规则。

## 11. 验证命令与验收指标

每个阶段按实际模块运行快速测试；完成时在仓库根目录执行：

```bash
go mod tidy
go test ./...
go test -race ./...
go vet ./...
go build ./...
```

对固定样本生成和验证：

```bash
go run ./cmd/specforge gen --repo <sample-repo>
go run ./cmd/specforge verify --spec <generated-openapi.yaml>
```

若 CLI 当前没有 `verify` 命令，应补充等价自动化测试或验证子命令，并记录实际命令，不得把计划中的命令当作已存在功能。

确定性检查：

```bash
go run ./cmd/specforge gen --repo <sample-repo>
sha256sum <generated-openapi.yaml>
go run ./cmd/specforge gen --repo <sample-repo>
sha256sum <generated-openapi.yaml>
```

两次 Hash 必须一致。若输出中包含时间戳等动态字段，先将其设计为显式可控字段或从确定性产物中分离。

最终 Meridian 验证应至少记录：

```text
routes: total / resolved / unresolved
operations: total
schemas: total / unresolved / unknown
parameters: 按 path/query/header/cookie 分类
responses: 按状态码分类，含错误响应
conflicts: 数量及明细
diagnostics: unresolved / unsupported / invalid refs
generated document: reload / ref resolution / validation 结果
runtime validation: 请求及响应样本结果
```

目标是将有源码证据且处于扫描范围内的路由、参数和响应完整解析，而不是为了数字归零而猜测。任何无法归零项必须报告路径、方法、字段、原因、证据、状态、置信度，以及是否适合 LLM 兜底。

## 12. 执行约束

- 先检查当前工作区和基线；不要覆盖或回滚用户现有改动。
- 每次只迁移可验证的一层，先加 adapter 和回归测试再删旧实现。
- Contract Graph 是业务合并的唯一权威模型。
- 第三方 OpenAPI 类型必须限制在 adapter 内。
- 不通过 `map[string]any` 手工承担 OpenAPI 语义合并。
- 不静默丢弃 refs、Schema 组合、响应状态码或解析错误。
- 不把未知类型自动降为 object/any 来制造“完整”结果。
- 不允许 LLM 创建无证据事实或覆盖强证据。
- 保留并验证确定性输出能力。
- kin-openapi 只有在明确需求成立后才作为独立、可选实现加入。
- 每个阶段完成后报告变更、测试命令、结果和仍存在的风险。

## 13. 完成定义

重构只有在以下条件全部满足后才算完成：

- Contract Graph 是唯一跨来源业务合并模型。
- 源码、OpenAPI、Runtime、Overlay 和 LLM 信息都有明确来源及证据。
- 冲突、缺口、未知类型和不支持结构均可见，且不会静默覆盖。
- libopenapi 只在 OpenAPI adapter 层使用；默认文档/HTTP 验证由 libopenapi-validator 提供。
- kin-openapi（若加入）只在可选 adapter/对照验证中出现。
- OpenAPI 文档 Diff 与 Contract Graph Diff 职责分开。
- Overlay、Arazzo、运行时验证各有明确模型及验证流程。
- Meridian 生成结果可重新加载、解析引用并通过规范校验。
- 同输入生成稳定；关键 round-trip、不变量和框架样本测试通过。
- 所有剩余未解析项、差异和风险均有可定位报告。

给执行 Agent 的核心要求：

> 先建立 Contract Graph 和 libopenapi adapter，再分阶段迁移源码分析、文档导入、运行时验证和 Compiler。所有来源都必须保留候选事实、证据、冲突与置信度；第三方 OpenAPI 模型不能成为核心模型。每次迁移都要有回归测试，所有生成文档都必须重新加载、解析引用并通过规范验证。
