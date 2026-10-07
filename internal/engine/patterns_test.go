package engine

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/specforge/specforge/internal/compiler"
	"github.com/specforge/specforge/internal/infer"
	"github.com/specforge/specforge/internal/schema"
)

// 本文件守护「真实项目 API 组织模式」的回归：每个 fixture 覆盖一类影响 OpenAPI 契约的写法，
// 断言关键契约字段（请求体 schema、响应信封形态、业务码、状态码、参数来源）。
// 模式目录见 testdata/PATTERNS.md（模式 ↔ 真实 file:line 证据 ↔ fixture ↔ 测试）。

// opsByKey 按 "METHOD /path" 索引编译后的 operation。
func opsByKey(res *Result) map[string]compiler.Operation {
	out := map[string]compiler.Operation{}
	for _, op := range res.Doc.Operations {
		out[op.Method+" "+op.Path] = op
	}
	return out
}

// TestJSONRPCEnvelope JSON-RPC 2.0 信封（sample-ipo 族）：
// Response 成功包 {jsonrpc,id,result}，WriteResponse 成功裸写 data；错误统一 ErrResponse{error:{code,message,data}}；
// 请求体带 params 外壳；header 鉴权 X-uin；通配版本组展开。
func TestJSONRPCEnvelope(t *testing.T) {
	repo := filepath.Join("..", "..", "testdata", "jsonrpc-repo")
	outDir := t.TempDir()
	res, err := Run(context.Background(), Config{
		RepoDir:     repo,
		ProfilePath: filepath.Join(repo, ".specforge", "profile.yaml"),
		OutDir:      outDir,
	})
	if err != nil {
		t.Fatalf("gen failed: %v", err)
	}
	if res.Operations < 6 {
		t.Fatalf("operations = %d, want >= 6", res.Operations)
	}
	ops := opsByKey(res)

	// Response 成功信封：result 承载业务体，且错误分支是 ErrResponse。
	check := ops["POST /ipo/v1/OrderCheck"]
	if check.Body == nil || check.Body.SchemaName != "OrderCheckReq" {
		t.Fatalf("OrderCheck body = %+v", check.Body)
	}
	if len(check.Responses) == 0 || len(check.Responses[0].Variants) != 2 ||
		!variantHas(check, "BaseResp") || !variantHas(check, "ErrResponse") {
		t.Fatalf("OrderCheck envelope variants = %+v", check.Responses[0].Variants)
	}
	// 业务码 enum：成功 0 + 校验失败 100012/100013。
	var codes []int
	for _, r := range check.Responses {
		codes = append(codes, r.Codes...)
	}
	if !containsInt(codes, 0) || !containsInt(codes, 100012) || !containsInt(codes, 100013) {
		t.Fatalf("OrderCheck codes = %v", codes)
	}
	// 鉴权 header 参数。
	if !hasParam(check, "header", "X-uin", true) {
		t.Fatalf("OrderCheck missing required X-uin header param: %+v", check.Params)
	}

	// WriteResponse 成功裸写：成功行无信封，业务体直接作为响应体（数组元素 schema）。
	list := ops["GET /ipo/v1/OrderList"]
	if len(list.Responses) == 0 {
		t.Fatalf("OrderList no responses")
	}
	raw := false
	for _, r := range list.Responses {
		raw = raw || r.Raw
	}
	if !raw || list.Responses[0].ArrayElem != "OrderListItem" {
		t.Fatalf("OrderList should be raw array of OrderListItem, got %+v", list.Responses[0])
	}
	// query 参数来自 QueryParser（无 params 外壳）。
	if !hasParam(list, "query", "uin", true) || !hasParam(list, "query", "page", true) {
		t.Fatalf("OrderList query params = %+v", list.Params)
	}

	// 错误信封字段名是 message（sample-ipo 族约定，非 msg）。
	if !schemaErrorField(res, "ErrResponse", "message") {
		t.Fatal("ErrResponse.error must carry field `message`")
	}
	if schemaErrorField(res, "ErrResponse", "msg") {
		t.Fatal("ErrResponse.error must NOT carry `msg` in the message variant")
	}
}

// TestJSONRPCMsgVariant JSON-RPC 信封变体（access-control 族）：
// error.msg 字段名 + service 层组装信封 + 中间件透明剥壳后请求体是扁平 DTO（无 params 外壳）。
func TestJSONRPCMsgVariant(t *testing.T) {
	repo := filepath.Join("..", "..", "testdata", "jsonrpcmsg-repo")
	outDir := t.TempDir()
	res, err := Run(context.Background(), Config{
		RepoDir:     repo,
		ProfilePath: filepath.Join(repo, ".specforge", "profile.yaml"),
		OutDir:      outDir,
	})
	if err != nil {
		t.Fatalf("gen failed: %v", err)
	}
	ops := opsByKey(res)

	list := ops["POST /permAdmin/v1/RoleList"]
	if list.Body == nil || list.Body.SchemaName != "RoleListReq" {
		t.Fatalf("RoleList body = %+v", list.Body)
	}
	// 扁平请求体：无 params 外壳（jsonrpc/id 已被中间件剥掉）。
	if strings.Contains(list.Body.SchemaName, "Params") || list.Body.EnvelopeName != "" {
		t.Fatalf("RoleList body should be flat DTO (no envelope), got %+v", list.Body)
	}
	// 错误信封字段名是 msg。
	if !schemaErrorField(res, "ErrResponse", "msg") {
		t.Fatal("ErrResponse.error must carry field `msg` (access-control variant)")
	}
	if schemaErrorField(res, "ErrResponse", "message") {
		t.Fatal("ErrResponse.error must NOT carry `message` in the msg variant")
	}
	// 错误业务码可溯源。
	assign := ops["POST /permAdmin/v1/RoleAssign"]
	var codes []int
	for _, r := range assign.Responses {
		codes = append(codes, r.Codes...)
	}
	if !containsInt(codes, 100201) {
		t.Fatalf("RoleAssign codes = %v, want 100201", codes)
	}
}

// TestTypedResponseRESTful 强类型 RESTful（meridian 族）：无 code/msg/data 信封，成功响应体即业务结构体；
// 错误信封 {code(字符串枚举),message,requestId}；请求体经 json.Decoder 绑定；204 由裸 WriteHeader 给出。
func TestTypedResponseRESTful(t *testing.T) {
	repo := filepath.Join("..", "..", "testdata", "typed-repo")
	outDir := t.TempDir()
	res, err := Run(context.Background(), Config{RepoDir: repo, OutDir: outDir})
	if err != nil {
		t.Fatalf("gen failed: %v", err)
	}
	ops := opsByKey(res)

	create := ops["POST /api/v1/users"]
	if create.Body == nil || create.Body.SchemaName != "CreateUserRequest" {
		t.Fatalf("CreateUser body = %+v", create.Body)
	}
	// 成功响应体是强类型 User（无信封）。
	if !responseHasSchema(create, "User") {
		t.Fatalf("CreateUser success response should be typed User, got %+v", create.Responses)
	}

	login := ops["POST /api/v1/auth/login"]
	if login.Body == nil || login.Body.SchemaName != "LoginRequest" {
		t.Fatalf("Login body = %+v", login.Body)
	}
	if !responseHasSchema(login, "LoginResult") {
		t.Fatalf("Login success response should be typed LoginResult, got %+v", login.Responses)
	}

	// 204 由裸 WriteHeader 写出（无 body）。
	del := ops["DELETE /api/v1/assets/{assetId}"]
	if !hasResponseStatus(del, "204") {
		t.Fatalf("DeleteAsset should carry 204 (bare WriteHeader), got %+v", del.Responses)
	}
	if !hasParam(del, "path", "assetId", true) {
		t.Fatalf("DeleteAsset path param = %+v", del.Params)
	}

	// 错误信封 code 是字符串枚举（非整数业务码）。
	enums := schemaPropEnum(res, "errorEnvelope", "code")
	if len(enums) == 0 || !containsStr(enums, "not_found") || !containsStr(enums, "validation_error") {
		t.Fatalf("errorEnvelope.code enum = %v", enums)
	}
}

// TestFastAPIGeneric FastAPI/Pydantic 动态语言（weave 族，走 LLM 通用前端）：
// 装饰器路由 + Pydantic 请求体 + RESTful 直返 + HTTPException（真实状态码，无信封）。
func TestFastAPIGeneric(t *testing.T) {
	repo := filepath.Join("..", "..", "testdata", "fastapi-repo")
	res, err := Run(context.Background(), Config{
		RepoDir:        repo,
		OutDir:         t.TempDir(),
		Provider:       &fastAPIProvider{},
		Service:        "webui",
		ServiceRoot:    "src/weave/webui",
		LLMConcurrency: 3,
	})
	if err != nil {
		t.Fatalf("gen failed: %v", err)
	}
	ops := opsByKey(res)
	for _, want := range []string{"GET /api/health", "POST /api/auth/login", "GET /api/auth/me", "POST /api/runs/submit", "GET /api/runs/{run_id}"} {
		if _, ok := ops[want]; !ok {
			t.Fatalf("missing operation %s; got %v", want, keysOf(ops))
		}
	}
	login := ops["POST /api/auth/login"]
	if login.Body == nil || login.Body.SchemaName == "" {
		t.Fatalf("login body missing: %+v", login.Body)
	}
	if !hasResponseStatus(login, "200") || !hasResponseStatus(login, "401") {
		t.Fatalf("login statuses = %+v", login.Responses)
	}
	// 通用前端置信度按 text 核对封顶（不高于 0.6）。
	if login.Confidence > 0.6 {
		t.Fatalf("login confidence %v exceeds text cap", login.Confidence)
	}
}

// fastAPIProvider 脚本化 Provider：路由发现按文件返回预设，契约抽取按 operation 返回预设。
type fastAPIProvider struct{}

func (p *fastAPIProvider) Name() string { return "fastapi-fixture" }

func (p *fastAPIProvider) Complete(_ context.Context, req infer.Request) (infer.Response, error) {
	var task struct {
		File   string `json:"file"`
		Method string `json:"method"`
		Path   string `json:"path"`
	}
	if err := json.Unmarshal([]byte(req.Prompt), &task); err != nil {
		return infer.Response{Text: []byte(`{}`)}, nil
	}
	routes := map[string]string{
		"main.py":        `{"routes":[{"method":"GET","path":"/api/health","handler":"health","line":12}]}`,
		"routes/auth.py": `{"routes":[{"method":"POST","path":"/api/auth/login","handler":"login","line":15},{"method":"GET","path":"/api/auth/me","handler":"me","line":23}]}`,
		"routes/runs.py": `{"routes":[{"method":"POST","path":"/api/runs/submit","handler":"submit_run","line":18},{"method":"GET","path":"/api/runs/{run_id}","handler":"get_run","line":23},{"method":"GET","path":"/api/runs/{run_id}/events","handler":"run_events","line":30}]}`,
	}
	if task.File != "" {
		if out, ok := routes[task.File]; ok {
			return infer.Response{Text: []byte(out)}, nil
		}
		return infer.Response{Text: []byte(`{}`)}, nil
	}
	contracts := map[string]string{
		"GET /api/health":               `{"params":[],"responses":[{"status":200,"error":false,"body":{"type":"object","properties":[{"name":"status","shape":{"type":"string"}}]}}]}`,
		"POST /api/auth/login":          `{"params":[],"requestBody":{"type":"object","properties":[{"name":"username","shape":{"type":"string"}},{"name":"password","shape":{"type":"string"}}]},"responses":[{"status":200,"error":false,"body":{"type":"object","properties":[{"name":"id","shape":{"type":"string"}},{"name":"username","shape":{"type":"string"}},{"name":"role","shape":{"type":"string"}}]}},{"status":401,"error":true,"body":null}]}`,
		"GET /api/auth/me":              `{"params":[{"in":"cookie","name":"weave_session","type":"string"},{"in":"header","name":"X-CSRF-Token","type":"string"}],"responses":[{"status":200,"error":false,"body":{"type":"object","properties":[{"name":"id","shape":{"type":"string"}},{"name":"username","shape":{"type":"string"}},{"name":"role","shape":{"type":"string"}}]}}]}`,
		"POST /api/runs/submit":         `{"params":[],"requestBody":{"type":"object","properties":[{"name":"suite_id","shape":{"type":"string"}},{"name":"case_ids","shape":{"type":"array","items":{"type":"string"}}},{"name":"run_id","shape":{"type":"string"}}]},"responses":[{"status":202,"error":false,"body":{"type":"object","properties":[{"name":"run_id","shape":{"type":"string"}},{"name":"status","shape":{"type":"string"}},{"name":"suite_id","shape":{"type":"string"}},{"name":"cases","shape":{"type":"string"}}]}}]}`,
		"GET /api/runs/{run_id}":        `{"params":[{"in":"path","name":"run_id","type":"string","required":true}],"responses":[{"status":200,"error":false,"body":{"type":"object","properties":[{"name":"run_id","shape":{"type":"string"}},{"name":"status","shape":{"type":"string"}}]}},{"status":404,"error":true,"body":null}]}`,
		"GET /api/runs/{run_id}/events": `{"params":[{"in":"path","name":"run_id","type":"string","required":true}],"responses":[{"status":200,"error":false,"body":null}]}`,
	}
	if out, ok := contracts[task.Method+" "+task.Path]; ok {
		return infer.Response{Text: []byte(out)}, nil
	}
	return infer.Response{Text: []byte(`{}`)}, nil
}

// ---- 断言辅助 ----

func hasParam(op compiler.Operation, in, name string, required bool) bool {
	for _, p := range op.Params {
		if p.In == in && p.Name == name && p.Required == required {
			return true
		}
	}
	return false
}

func hasResponseStatus(op compiler.Operation, status string) bool {
	for _, r := range op.Responses {
		if r.Status == status {
			return true
		}
	}
	return false
}

// responseHasSchema 任一响应行（含变体）是否引用给定业务体 schema。
func responseHasSchema(op compiler.Operation, name string) bool {
	for _, r := range op.Responses {
		if r.SchemaName == name || r.ArrayElem == name {
			return true
		}
		for _, v := range r.Variants {
			if v.SchemaName == name || v.ArrayElem == name {
				return true
			}
		}
	}
	return false
}

// variantHas 响应行的变体是否包含给定信封名。
func variantHas(op compiler.Operation, envelope string) bool {
	for _, r := range op.Responses {
		for _, v := range r.Variants {
			if v.EnvelopeName == envelope {
				return true
			}
		}
	}
	return false
}

// schemaErrorField 断言 components 里命名信封 schema 的 error 字段下存在指定子字段。
func schemaErrorField(res *Result, schemaName, field string) bool {
	for _, ns := range res.Doc.Schemas {
		if ns.Name != schemaName {
			continue
		}
		for _, p := range ns.Schema.Props {
			if p.Name == "error" {
				return propHas(p.Schema, field)
			}
		}
	}
	return false
}

// schemaPropEnum 断言命名 schema 的某个字段的枚举值。
func schemaPropEnum(res *Result, schemaName, propName string) []string {
	for _, ns := range res.Doc.Schemas {
		if ns.Name != schemaName {
			continue
		}
		for _, p := range ns.Schema.Props {
			if p.Name == propName {
				return p.Schema.Enum
			}
		}
	}
	return nil
}

// propHas 递归检查 object schema 是否含给定直接子字段。
func propHas(sc *schema.Schema, field string) bool {
	if sc == nil {
		return false
	}
	for _, p := range sc.Props {
		if p.Name == field {
			return true
		}
	}
	return false
}

func containsInt(hay []int, needle int) bool {
	for _, v := range hay {
		if v == needle {
			return true
		}
	}
	return false
}

func containsStr(hay []string, needle string) bool {
	for _, v := range hay {
		if v == needle {
			return true
		}
	}
	return false
}

func keysOf(m map[string]compiler.Operation) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
