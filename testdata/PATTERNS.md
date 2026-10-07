# SpecForge 回归测试数据：API 编写与组织模式目录

本目录（`testdata/`）是 SpecForge 的回归测试数据来源。每个 fixture 仓库对应一类「影响 OpenAPI
契约」的真实写法，均取自 `/home/whj/projects` 下的真实项目，并按「至少 3 个 API 示例」的要求铺满。

本文是模式目录：说明每种写法的**契约影响**、**真实出处**（file:line 证据）与**覆盖它的 fixture**。
新增/变更 fixture 时必须同步更新本目录，保证「模式 ↔ 证据 ↔ fixture ↔ 测试」四点一致。

---

## 0. 总览：项目 → 模式映射

调查对象共 30+ 个项目，按 API 组织方式可归为三大族 + 两个独立形态：

| 族 | 项目 | 框架 | 响应信封 | 业务码位置 |
|---|---|---|---|---|
| **JSON-RPC 族**（主导，约 15 个 Go 服务） | sample-ipo / sample-app / funds / equity / asset-service / identity / access-control / adapters / personnel / controls / edge / vendor-proxy / market-proxy / internal-data / shared-lib | Fiber v2 | `{jsonrpc,id,result,error}` | `X-retcode` 头 + `error.code`（HTTP 恒 200） |
| **code/msg/data 族** | SpecForge sample 建模的 trade 老版本 | Fiber v2 | `{code,msg,data}` | body `code`（HTTP 恒 200） |
| **RESTful 强类型族** | meridian | chi v5（oapi-codegen 生成） | 直接结构体，无信封 | HTTP 状态码 + `error.code` 字符串枚举 |
| **RESTful 动态语言族** | weave | FastAPI（Python） | 直接 dict/list，无信封 | HTTP 状态码（HTTPException） |
| 无 HTTP API | opman-web-new(纯前端) / mymcps / feishu-docu-mcp(MCP) / airflow-dags / robotframework | — | — | — |

`sample-ipo-rebase` 与 `sample-ipo` 同源（常量拆领域文件、`pkg/constant/ipoFini.go`/`amount.go`，码值一致）；
`equity-equityin-optimize` 是 `equity` 的 git worktree（差异仅在 StockOut 模块）。二者不新增契约模式，不单独建模。

---

## 1. JSON-RPC 2.0 响应信封（主导模式）

### 契约影响

- 请求体外壳：`{"jsonrpc":"2.0","id":"...","params":{...业务字段...}}`；`jsonrpc`/`id` 是必填字段（`validate:"required,eq=2.0"`）。
- 成功响应：`{"jsonrpc":"2.0","id":"...","result":{...业务结构体/列表...}}`，**没有顶层 `code` 字段**。
- 失败响应：`{"jsonrpc":"2.0","id":"...","error":{"code":100001,"message":"...","data":{...}}}`。
- HTTP 状态码**恒为 200**；业务码同时出现在**响应头 `X-retcode`**（`err.Error()` = 错误码字符串，成功为 `"0"`）与 **body `error.code`**。
- 错误对象字段名存在变体：绝大多数是 `error.message`，`access-control` 是 `error.msg`（见 §3）。

### 真实出处

- 信封/写出口：`sample-ipo/pkg/code/base.go:41-88`（`WriteResponse`/`Response`/`ResponseExcel`）、
  `funds/pkg/code/base.go`、`identity/pkg/code/base.go`、`access-control/pkg/code/base.go`、`controls/pkg/code/base.go`、`sample-app/internal/pkg/code/base.go`。
- 请求外壳：`sample-ipo/internal/common/mapping/base.go:13-16`（`BaseData`）、`personnel/internal/adminstaff/mapping/mapping.go:3-20`（`BaseData/BaseBody/BaseResp`）。
- 错误对象：`sample-ipo/pkg/code/error.go:14-21`（`Error{Code int; Message string; Data interface{}}`，`Error()` 返回 `fmt.Sprint(Code)`）。
- 头常量：`sample-ipo/pkg/constant/header.go:4-20`（`HeaderRetCode="X-retcode"`、`HeaderRequestId="X-request-id"` 小写版）、
  `shared-lib/header/hani.go:6-21`（canonical `X-Retcode`/`X-Request-Id`）。

### 覆盖 fixture

`testdata/jsonrpc-repo`（≥3 API：`OrderCheck`/`OrderQuery`/`OrderCancel`/`Ping` 走 `Response`，`OrderList`/`StockCreate`/`Health` 走裸写 `WriteResponse`）。
回归测试：`internal/engine/patterns_test.go::TestJSONRPCEnvelope`。

---

## 2. 裸写 data 的 WriteResponse 变体（成功无信封）

### 契约影响

`WriteResponse` 与 `Response` 的**成功形态不同**：`Response` 成功包 `{jsonrpc,id,result}`，`WriteResponse` 成功**直接裸写 `c.JSON(data)`**（无信封、无 `jsonrpc`/`id`）；两者错误形态相同（`ErrResponse`）。同一仓库内两种写出口并存（identity/adapters/sample-ipo 最典型），提取器必须按汇聚点函数区分成功响应形状，而不能假定全仓统一信封。

### 真实出处

- `sample-ipo/pkg/code/base.go:41-64`（`WriteResponse` 成功裸写 vs `Response` 成功包信封，同一文件紧邻）。
- `identity/pkg/code/base.go`（`WriteResp`/`WriteRes` 恒信封、`WriteResponse` 裸写、`Response` 带 `result`，四套并存）。
- 使用量佐证（identity）：`WriteResponse` 1261 处、`WriteResp` 495、`WriteRes` 648、`Response` 41。

### 覆盖 fixture

`testdata/jsonrpc-repo`（`OrderList`/`StockCreate`/`Health` 三处裸写 ≥3）。
回归测试：`internal/engine/patterns_test.go::TestJSONRPCEnvelope`（断言裸写 operation 无信封字段）。

---

## 3. error.msg 字段名变体 + 中间件透明剥壳/补壳

### 契约影响

- 错误对象字段名不同：`access-control` 用 `json:"msg"`，其余服务用 `json:"message"`。提取响应错误 schema 时字段名不可硬编码。
- `access-control` 用 `jsonRpcMiddleware` 做**请求透明剥壳**：先 `BodyParser` 成 `map[string]interface{}`，存在 `params` 则 `json.Marshal(params)` 替换 `c.Request().SetBody(...)`，handler 再正常 `BodyParser` 到裸 DTO——即「请求时剥壳、响应时补壳」，handler 的请求体是**扁平 DTO**（无 `params` 外壳），与其他服务的 `params` 外壳不同。
- 成功信封在 **service 层**用 `BuildBaseResponse` 组装后再交给 `WriteResponse` 裸写（controller 只 `WriteResponse(ctx, nil, u.BuildBaseResponse(ctx, payload))`）。

### 真实出处

- `access-control/internal/access-control/router.go:132-147`（`jsonRpcMiddleware` 剥壳）。
- `access-control/pkg/code/error.go:9-16`（`Error.Msg` json tag `json:"msg"`）。
- `access-control/internal/access-control/service/account/account.go:40-48`（`BuildBaseResponse`）。

### 覆盖 fixture

`testdata/jsonrpcmsg-repo`（≥3 API，均走剥壳中间件 + `error.msg` + service 层组装信封）。
回归测试：`internal/engine/patterns_test.go::TestJSONRPCMsgVariant`。

---

## 4. RESTful 强类型响应对象（oapi-codegen / strict-server 形态）

### 契约影响

- **无信封**：成功响应体就是业务结构体本身；状态码藏在**命名响应类型**里（`Login200JSONResponse`/`CreateUser201JSONResponse`/`204NoContent`），body 藏在 `Body` 字段。
- 错误是稳定信封 `{code,message,details?,requestId}`，`code` 是**蛇形字符串枚举**（`not_found`/`validation_error`/`unauthenticated`…，非整数、非 HTTP 码），HTTP 状态是**真实 RESTful 状态码**（201/401/404/409/422…，非恒 200）。
- 请求解码是 `json.NewDecoder(r.Body).Decode` / `r.MultipartReader()`（无 binding tag），校验靠 kin-openapi 中间件 + handler 手写判空。
- 鉴权按 operation 用 strict middleware 包装，安全策略从内嵌 OpenAPI 的 `security` 反推（`Authorization: Bearer`/cookie）。

### 真实出处

- `meridian/internal/handler/identity.go:28-35`（返回 `auth.Login200JSONResponse{Body: ...}`）。
- `meridian/internal/handler/response.go:32-50`（`writeErrorDetails`：`{code,message,details?,requestId}` + `writeJSON`）。
- `meridian/internal/handler/server.go:171-237`（`responseErrorHandler` 哨兵错误 → 状态码映射 switch）。
- `meridian/internal/service/error_codes.go:8-43`（字符串错误码常量）。
- `meridian/internal/generated/api/auth/server.gen.go:762-776`（`json.NewDecoder(...).Decode`）。

### 覆盖 fixture

`testdata/typed-repo`（chi v5 手写 strict-handler 同构形态，≥3 API：`Login`/`CreateUser`/`GetAsset`/`DeleteAsset`）。
回归测试：`internal/engine/patterns_test.go::TestTypedResponseRESTful`。

---

## 5. FastAPI/Pydantic 动态语言 RESTful（weave）

### 契约影响

- 路由用**装饰器**声明（`@router.get("/{id}")`），非集中注册；路径参数在函数签名。
- 请求体是 **Pydantic `BaseModel` + `Field(...)`**（`pattern`/`min_length`/`max_length`），snake_case 字段。
- 响应**直接 `return {...}` / `return [...]`**，无信封；失败 `raise HTTPException(status_code, detail)`。
- 鉴权是 per-route **依赖注入**（`Depends` + `Annotated` 别名），非全局中间件；SSE/文件用 `StreamingResponse`。

### 真实出处

- `weave/src/weave/webui/main.py:73-118`（`create_app` 工厂 + `include_router`）。
- `weave/src/weave/webui/routes/runs.py:26-33`（`SubmitRunRequest(BaseModel)` + `Field`）。
- `weave/src/weave/webui/routes/auth.py:28-43`（`return {...}` / `HTTPException`）。
- `weave/src/weave/webui/auth.py:88-122`（`CurrentUser`/`ProtectedUser` Depends 别名）。

### 覆盖 fixture

`testdata/fastapi-repo`（≥3 路由，走 LLM 通用前端 + 脚本化 provider）。
回归测试：`internal/engine/patterns_test.go::TestFastAPIGeneric`。

---

## 6. 既有 fixture 覆盖的模式（不重复新建）

| 模式 | fixture | ≥3 API | 回归测试 |
|---|---|---|---|
| code/msg/data 信封 + `BizError` 业务码枚举（`code` 字段 enum） | `sample-repo` | 8 | `TestEndToEndAccuracy` |
| `gin.H` map 字面量信封 + 固定失败码 + 同状态码多形状 oneOf + 未识别包装器缺口 | `gin-repo` | 7 | `TestGinAdapter` / `TestGinEnvelopeShapes` |
| net/http 直接结构体 + `json.Encoder` + 真实状态码 + chi 回调路由 | `chi-repo` | 4 | `TestChiAdapter` |
| 错误值流定性：untyped/dynamic/uncoded/unresolved + 命名错误信封 `{error}`/`{result}` | `errflow-repo` | 5 | `TestErrorFlowClassification` |
| Express/JS 路由 + RESTful 状态码（LLM 通用前端，幻觉核对） | `express-repo` | 4 | `TestGenericFrontendExpress` |
| echo 未登记框架（LLM 生成适配器声明） | `echo-repo` | 3 | `TestLLMAdapterEcho` |

---

## 7. 维护约定

1. 每个新 fixture 是一个**自包含仓库**（含 `go.mod`/`pyproject.toml`），不引用外部私有模块，能在无网络/离线环境下被 `specforge gen` 分析。
2. 每个 fixture 对应一个 `internal/engine/patterns_test.go` 中的回归测试，断言**每条 API 的关键契约字段**（方法、路径、请求体 schema、响应信封形态、业务码、状态码、参数来源）。
3. 新增「不同写法」时：先在本目录补一节（含真实 file:line 证据），再新建 fixture（≥3 API），再写测试，三者同步提交。
4. 命名约定：`<pattern>-repo`；ground-truth（可选）放 fixture 根目录 `ground-truth.yaml`。
