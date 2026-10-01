package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/specforge/specforge/internal/compiler"
	"github.com/specforge/specforge/internal/eval"
	"github.com/specforge/specforge/internal/facts"
	"github.com/specforge/specforge/internal/infer"
)

// TestEndToEndAccuracy 端到端精度回归守卫:
// 生成 → 评测 → 全指标必须维持满分基线（设计文档 §1.6 验收标准）。
// 任何使指标回退的改动都会在此测试失败——这是「文档进 CI gate」
// 的前提（设计文档 §8.5 确定性自检的对偶）。
func TestEndToEndAccuracy(t *testing.T) {
	repo := filepath.Join("..", "..", "testdata", "sample-repo")
	outDir := t.TempDir()

	result, err := Run(context.Background(), Config{
		RepoDir:     repo,
		ProfilePath: filepath.Join(repo, ".specforge", "profile.yaml"),
		OutDir:      outDir,
	})
	if err != nil {
		t.Fatalf("gen failed: %v", err)
	}
	if result.Operations != 10 {
		t.Errorf("operations = %d, want 10", result.Operations)
	}
	if result.Routes != 10 || result.RoutesResolved != 10 {
		t.Errorf("routes = %d (resolved %d), want 10/10", result.Routes, result.RoutesResolved)
	}
	// 确定性: 双编译逐字节一致
	if !compileTwice(result) {
		t.Error("determinism check failed")
	}

	// 评测: 全指标满分基线
	truth := filepath.Join("..", "..", "testdata", "ground-truth.yaml")
	spec := filepath.Join(outDir, "openapi.yaml")
	m, err := eval.Evaluate(truth, spec)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	checks := []struct {
		name string
		got  float64
		want float64
	}{
		{"RouteRecall", m.RouteRecall, 1.0},
		{"RoutePrecision", m.RoutePrecision, 1.0},
		{"ParamF1", m.ParamF1, 1.0},
		{"ReqFieldF1", m.ReqFieldF1, 1.0},
		{"RespFieldF1", m.RespFieldF1, 1.0},
		{"EnvelopeRecall", m.EnvelopeRecall, 1.0},
	}
	for _, c := range checks {
		if c.got < c.want {
			t.Errorf("%s = %.3f, want >= %.3f", c.name, c.got, c.want)
		}
	}
	if m.HallucRate > 0 {
		t.Errorf("Hallucination = %.3f, want 0", m.HallucRate)
	}

	// 输出文件存在性
	if _, err := os.Stat(spec); err != nil {
		t.Error("openapi.yaml missing")
	}
	if _, err := os.Stat(filepath.Join(outDir, "report.md")); err != nil {
		t.Error("report.md missing")
	}
}

// compileTwice 引擎侧确定性检查（设计文档 §8.5 双跑自检）。
func compileTwice(r *Result) bool {
	if r.Doc == nil {
		return false
	}
	return compiler.CompileTwiceCheck(r.Doc)
}

// sampleRepo 样本仓库路径。
var sampleRepo = filepath.Join("..", "..", "testdata", "sample-repo")

// TestRunDeterministicAcrossRuns 两次独立运行产物逐字节一致（spec、报告、证据视图）。
func TestRunDeterministicAcrossRuns(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	for _, out := range []string{a, b} {
		if _, err := Run(context.Background(), Config{RepoDir: sampleRepo, OutDir: out}); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{specFile, reportFile, operationsFile} {
		x, _ := os.ReadFile(filepath.Join(a, name))
		y, _ := os.ReadFile(filepath.Join(b, name))
		if !bytes.Equal(x, y) || len(x) == 0 {
			t.Errorf("%s differs between runs", name)
		}
	}
}

// TestRunMemoHit 第二次运行命中 memo：产物一致、摘要回填、不再分析。
func TestRunMemoHit(t *testing.T) {
	memoDir, out1, out2 := t.TempDir(), t.TempDir(), t.TempDir()
	r1, err := Run(context.Background(), Config{RepoDir: sampleRepo, OutDir: out1, MemoDir: memoDir})
	if err != nil || r1.Cached {
		t.Fatalf("first run: cached=%v err=%v", r1 != nil && r1.Cached, err)
	}
	r2, err := Run(context.Background(), Config{RepoDir: sampleRepo, OutDir: out2, MemoDir: memoDir})
	if err != nil || !r2.Cached {
		t.Fatalf("second run should hit memo: err=%v", err)
	}
	if r2.Operations != r1.Operations || r2.SchemaTypes != r1.SchemaTypes || r2.Fingerprint != r1.Fingerprint {
		t.Fatalf("summary not restored: %+v vs %+v", r2, r1)
	}
	x, _ := os.ReadFile(filepath.Join(out1, specFile))
	y, _ := os.ReadFile(filepath.Join(out2, specFile))
	if !bytes.Equal(x, y) {
		t.Fatal("cached spec differs")
	}
}

// TestRunLLMCallCache LLM 调用缓存：同一代码第二次运行不再调用模型（memo 关闭，只测调用级缓存）。
func TestRunLLMCallCache(t *testing.T) {
	cacheDir := t.TempDir()
	p := &mockProvider{out: "{}"}
	r1, err := Run(context.Background(), Config{RepoDir: sampleRepo, OutDir: t.TempDir(), LLMCacheDir: cacheDir, Provider: p})
	if err != nil {
		t.Fatal(err)
	}
	if r1.LLM.Attempted == 0 || p.calls == 0 {
		t.Skip("sample repo has no gaps to send to the LLM")
	}
	first := p.calls
	r2, err := Run(context.Background(), Config{RepoDir: sampleRepo, OutDir: t.TempDir(), LLMCacheDir: cacheDir, Provider: p})
	if err != nil {
		t.Fatal(err)
	}
	if p.calls != first || r2.LLMCache.Hits != r1.LLM.Attempted {
		t.Fatalf("second run should be served from cache: calls %d→%d, cache=%+v", first, p.calls, r2.LLMCache)
	}
}

// TestRunConfigError 非 Go 模块目录返回 ErrConfig（CLI 映射为退出码 30）。
func TestRunConfigError(t *testing.T) {
	_, err := Run(context.Background(), Config{RepoDir: t.TempDir()})
	if err == nil || !errorsIs(err, ErrConfig) {
		t.Fatalf("want ErrConfig, got %v", err)
	}
}

// errorsIs errors.Is 的本地别名（避免与测试名冲突的可读性包装）。
func errorsIs(err, target error) bool { return errors.Is(err, target) }

// TestGinAdapter gin 适配器端到端：分组路由、路径/查询/头参数、JSON/Query 绑定、状态码实参、响应包装器。
// 适配器只有 adapter/gin.go 一个声明文件——本测试守护「新增 Go 框架 = 加一个声明文件」。
func TestGinAdapter(t *testing.T) {
	repo := filepath.Join("..", "..", "testdata", "gin-repo")
	res, err := Run(context.Background(), Config{RepoDir: repo, OutDir: t.TempDir()})
	if err != nil {
		t.Skipf("gin sample not loadable (module cache/network?): %v", err)
	}
	if res.Operations < 3 {
		t.Skipf("gin module not available: %d operations", res.Operations)
	}
	ops := map[string]compiler.Operation{}
	for _, op := range res.Doc.Operations {
		ops[op.Method+" "+op.Path] = op
	}
	create, ok := ops["POST /api/v1/users"]
	if !ok || create.Body == nil || create.Body.SchemaName != "CreateUserReq" {
		t.Fatalf("POST body not bound: %+v", create)
	}
	statuses := map[string]bool{}
	for _, r := range create.Responses {
		statuses[r.Status] = true
	}
	if !statuses["201"] || !statuses["400"] {
		t.Fatalf("status codes from c.JSON(status, body) not resolved: %+v", create.Responses)
	}
	get := ops["GET /api/v1/users/{id}"]
	params := map[string]string{}
	for _, p := range get.Params {
		params[p.In+":"+p.Name] = p.Origin
	}
	if params["path:id"] != "gin:Param" || params["header:X-Trace-Id"] != "gin:GetHeader" {
		t.Fatalf("params = %v", params)
	}
	if len(get.Responses) != 1 || get.Responses[0].SchemaName != "User" {
		t.Fatalf("wrapper ok(c, User{}) not traced: %+v", get.Responses)
	}
	list := ops["GET /api/v1/users"]
	names := map[string]bool{}
	for _, p := range list.Params {
		names[p.Name] = true
	}
	if !names["keyword"] || !names["page"] || !names["sort"] {
		t.Fatalf("query params = %+v", list.Params)
	}
}

// TestChiAdapter chi v5 适配器端到端：回调式 Route/Group、路径/query 参数、标准库 JSON 绑定与写出。
func TestChiAdapter(t *testing.T) {
	repo := filepath.Join("..", "..", "testdata", "chi-repo")
	res, err := Run(context.Background(), Config{RepoDir: repo, OutDir: t.TempDir()})
	if err != nil {
		t.Fatalf("chi sample failed to load: %v", err)
	}
	if res.Operations < 3 {
		t.Fatalf("chi operations = %d, want at least 3", res.Operations)
	}
	ops := map[string]compiler.Operation{}
	for _, op := range res.Doc.Operations {
		ops[op.Method+" "+op.Path] = op
	}
	get, ok := ops["GET /api/users/{id}"]
	if !ok || len(get.Params) < 2 {
		t.Fatalf("chi route or params missing: %+v", ops)
	}
	params := map[string]string{}
	for _, p := range get.Params {
		params[p.In+":"+p.Name] = p.Origin
	}
	if params["path:id"] != "chi:URLParam" || params["query:expand"] != "chi:Get" {
		t.Fatalf("chi params = %v", params)
	}
	if len(get.Responses) == 0 || get.Responses[0].SchemaName != "User" {
		t.Fatalf("chi JSON response missing: %+v", get.Responses)
	}
	create, ok := ops["POST /api/users"]
	if !ok || create.Body == nil || create.Body.SchemaName != "CreateUserRequest" {
		t.Fatalf("chi request body missing: %+v", create)
	}
	statuses := map[string]bool{}
	for _, response := range create.Responses {
		statuses[response.Status] = true
	}
	if !statuses["200"] || !statuses["400"] {
		t.Fatalf("chi response statuses missing: %+v", create.Responses)
	}
	if _, ok := ops["GET /api/stats"]; !ok {
		t.Fatalf("chi Group route missing: %+v", ops)
	}
	if _, ok := ops["GET /api/method"]; !ok {
		t.Fatalf("chi Method route missing: %+v", ops)
	}
	if _, ok := ops["GET /base-url"]; !ok {
		t.Fatalf("chi optional BaseURL route missing: %+v", ops)
	}
}

// TestGinEnvelopeShapes gin 样本的信封与形状场景（离线）：
// map 字面量信封包装器（含失败分支固定码）、同状态码多形状 oneOf、m["k"]=v 信封、以及未识别包装器必须显式报缺口。
func TestGinEnvelopeShapes(t *testing.T) {
	repo := filepath.Join("..", "..", "testdata", "gin-repo")
	res, err := Run(context.Background(), Config{RepoDir: repo, OutDir: t.TempDir()})
	if err != nil || res.Operations < 7 {
		t.Skipf("gin sample not loadable: %v", err)
	}
	ops := map[string]compiler.Operation{}
	for _, op := range res.Doc.Operations {
		ops[op.Method+" "+op.Path] = op
	}
	detail := ops["GET /api/v1/users/{id}/detail"].Responses[0]
	if len(detail.Variants) != 2 || detail.Variants[0].SchemaName != "User" || detail.Variants[0].EnvelopeName != "RespEnvelope" {
		t.Fatalf("detail variants = %+v", detail.Variants)
	}
	if !strings.Contains(detail.Description, "code=500") {
		t.Fatalf("fixed failure code missing: %s", detail.Description)
	}
	stats := ops["GET /api/v1/stats"].Responses[0]
	if len(stats.Variants) != 2 || !strings.Contains(stats.Description, "error branch") {
		t.Fatalf("stats = %+v", stats)
	}
	if settings := ops["GET /api/v1/users/{id}/settings"]; settings.Confidence != 1 || len(settings.Unknowns) != 0 {
		t.Fatalf("settings should be fully static: %+v", settings)
	}
	profile := ops["GET /api/v1/users/{id}/profile"]
	if profile.Confidence >= lowConfThreshold || !strings.Contains(strings.Join(profile.Unknowns, ";"), "response wrapper not summarized") {
		t.Fatalf("unsummarized wrapper must be an explicit gap: %+v", profile)
	}
}

// routeProvider 按 system prompt 前缀分派预设输出的 Provider（L3 适配器生成），其它任务返回空对象。
type routeProvider struct {
	adapter string // 适配器生成任务的输出
	calls   int    // 适配器生成调用次数
}

func (r *routeProvider) Name() string { return "route" }
func (r *routeProvider) Complete(_ context.Context, req infer.Request) (infer.Response, error) {
	if strings.HasPrefix(req.System, "You write a declarative adapter") {
		r.calls++
		return infer.Response{Text: []byte(r.adapter)}, nil
	}
	return infer.Response{Text: []byte(`{}`)}, nil
}

// echoProposal echo 适配器提案（点号符号形式，Context 为接口）。
const echoProposal = `{"name":"echo","routerTypes":["github.com/labstack/echo/v4.Echo","github.com/labstack/echo/v4.Group"],
"groupMethod":"Group","verbs":{"GET":"GET","POST":"POST"},"handlerArg":1,
"bodyBinders":[{"symbol":"github.com/labstack/echo/v4.Context.Bind","arg":0}],
"paramReaders":[{"symbol":"github.com/labstack/echo/v4.Context.Param","in":"path","nameArg":0,"type":"string"},
 {"symbol":"github.com/labstack/echo/v4.Context.QueryParam","in":"query","nameArg":0,"type":"string"}],
"writers":[{"symbol":"github.com/labstack/echo/v4.Context.JSON","bodyArg":1,"statusArg":0}]}`

// copyRepo 把样本仓库复制到临时目录（L3 会在仓库内落盘适配器声明，不能写脏 testdata）。
func copyRepo(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	return dst
}

// TestLLMAdapterEcho L3：未登记的 echo 框架 → LLM 声明 → 类型复核 + 试运行 → 落盘；
// 之后纯静态管线抽出完整契约，且下次运行（无 LLM）直接加载声明。
func TestLLMAdapterEcho(t *testing.T) {
	repo := copyRepo(t, filepath.Join("..", "..", "testdata", "echo-repo"))
	p := &routeProvider{adapter: echoProposal}
	res, err := Run(context.Background(), Config{RepoDir: repo, OutDir: t.TempDir(), Provider: p})
	if err != nil {
		t.Skipf("echo sample not loadable: %v", err)
	}
	if res.LLM.Adapter.Accepted != 1 || res.Operations != 3 {
		t.Fatalf("adapter=%+v operations=%d", res.LLM.Adapter, res.Operations)
	}
	ops := map[string]compiler.Operation{}
	for _, op := range res.Doc.Operations {
		ops[op.Method+" "+op.Path] = op
	}
	create := ops["POST /api/v1/orders"]
	if create.Body == nil || create.Body.SchemaName != "CreateOrderReq" || len(create.Responses) != 2 ||
		create.Responses[0].Status != "201" || create.Responses[1].Status != "400" {
		t.Fatalf("create = %+v", create)
	}
	get := ops["GET /api/v1/orders/{id}"]
	if len(get.Params) != 1 || get.Params[0].Origin != "echo:Param" || get.Responses[0].SchemaName != "Order" {
		t.Fatalf("get = %+v", get)
	}
	if _, err := os.Stat(filepath.Join(repo, ".specforge", "adapters", "echo.json")); err != nil {
		t.Fatalf("adapter spec not saved: %v", err)
	}
	offline, err := Run(context.Background(), Config{RepoDir: repo, OutDir: t.TempDir()})
	if err != nil || offline.Operations != 3 || offline.LLM.Adapter.Attempted != 0 {
		t.Fatalf("saved adapter not reused offline: ops=%d err=%v", offline.Operations, err)
	}
}

// TestLLMAdapterRejectsBogusSymbol 提案中的写出器符号不存在：整份声明拒绝，不落盘。
func TestLLMAdapterRejectsBogusSymbol(t *testing.T) {
	repo := copyRepo(t, filepath.Join("..", "..", "testdata", "echo-repo"))
	bogus := strings.Replace(echoProposal, "Context.JSON", "Context.RenderJSONMagic", 1)
	res, err := Run(context.Background(), Config{RepoDir: repo, OutDir: t.TempDir(), Provider: &routeProvider{adapter: bogus}})
	if err != nil {
		t.Skipf("echo sample not loadable: %v", err)
	}
	if res.LLM.Adapter.Rejected != 1 || res.Operations != 0 {
		t.Fatalf("adapter=%+v operations=%d", res.LLM.Adapter, res.Operations)
	}
	if _, err := os.Stat(filepath.Join(repo, ".specforge", "adapters")); !os.IsNotExist(err) {
		t.Fatal("rejected adapter must not be saved")
	}
}

// genericProvider L4 脚本化 Provider：路由发现按文件返回预设（含幻觉路由），契约抽取按路径返回预设（含幻觉字段）。
type genericProvider struct {
	routes    map[string]string // 文件相对路径 → 路由发现输出
	contracts map[string]string // "METHOD /path" → 契约抽取输出
}

func (g *genericProvider) Name() string { return "generic" }
func (g *genericProvider) Complete(_ context.Context, req infer.Request) (infer.Response, error) {
	var task struct {
		File   string `json:"file"`
		Method string `json:"method"`
		Path   string `json:"path"`
	}
	if err := json.Unmarshal([]byte(req.Prompt), &task); err != nil {
		return infer.Response{Text: []byte(`{}`)}, nil
	}
	if task.File != "" {
		return infer.Response{Text: []byte(g.routes[task.File])}, nil
	}
	if out, ok := g.contracts[task.Method+" "+task.Path]; ok {
		return infer.Response{Text: []byte(out)}, nil
	}
	return infer.Response{Text: []byte(`{}`)}, nil
}

// expressProvider express-repo 的脚本：4 条真实路由 + 3 条幻觉路由（路径不在注册行 / 挂载前缀不存在 / handler 不在注册行）。
func expressProvider() *genericProvider {
	return &genericProvider{
		routes: map[string]string{"src/app.js": `{"routes":[
 {"method":"GET","path":"/api/users/:id","handler":"getUser","line":9},
 {"method":"post","path":"/api/users","handler":"createUser","line":10},
 {"method":"GET","path":"/api/orders","handler":"listOrders","line":11},
 {"method":"GET","path":"/health","handler":"inline","line":14},
 {"method":"DELETE","path":"/api/admin","handler":"dropAll","line":9},
 {"method":"GET","path":"/v2/users","handler":"createUser","line":10},
 {"method":"GET","path":"/api/users","handler":"dropAll","line":10}]}`},
		contracts: map[string]string{
			"GET /api/users/{id}": `{"params":[{"in":"path","name":"id","type":"string","required":true},
 {"in":"query","name":"verbose","type":"boolean"},{"in":"query","name":"phantom","type":"string"}],
 "requestBody":null,
 "responses":[{"status":200,"error":false,"body":{"type":"object","properties":[
   {"name":"id","required":true,"shape":{"type":"integer"}},{"name":"name","shape":{"type":"string"}},
   {"name":"verbose","shape":{"type":"boolean"}}]}},
  {"status":400,"error":true,"body":{"type":"object","properties":[{"name":"error","shape":{"type":"string"}}]}}]}`,
			"POST /api/users": `{"params":[],"requestBody":{"type":"object","properties":[
   {"name":"name","required":true,"shape":{"type":"string"}},{"name":"email","shape":{"type":"string"}}]},
 "responses":[{"status":201,"body":{"type":"object","properties":[{"name":"id","shape":{"type":"integer"}},
   {"name":"name","shape":{"type":"string"}},{"name":"email","shape":{"type":"string"}}]}},
  {"status":422,"error":true,"body":{"type":"object","properties":[{"name":"error","shape":{"type":"string"}}]}}]}`,
			"GET /api/orders": `{"params":[{"in":"query","name":"status","type":"string"},{"in":"query","name":"page","type":"integer"}],
 "responses":[{"status":200,"body":{"type":"object","properties":[{"name":"page","shape":{"type":"integer"}},
   {"name":"items","shape":{"type":"array","items":{"type":"object","properties":[
     {"name":"orderId","shape":{"type":"string"}},{"name":"amount","shape":{"type":"number"}},
     {"name":"ghostField","shape":{"type":"string"}}]}}}]}}]}`,
			"GET /health": `{"responses":[{"status":200,"body":{"type":"object","properties":[{"name":"status","shape":{"type":"string"}}]}}]}`,
		},
	}
}

// TestGenericFrontendExpress L4：非 Go 仓库自动走通用前端；幻觉路由被文本核对拒绝，幻觉参数/字段被剔除或整棵拒绝，
// 跨文件 handler 按文本解析，全部事实带有效证据，置信度按 text 级封顶。
func TestGenericFrontendExpress(t *testing.T) {
	repo := filepath.Join("..", "..", "testdata", "express-repo")
	res, err := Run(context.Background(), Config{RepoDir: repo, OutDir: t.TempDir(), Provider: expressProvider(), LLMConcurrency: 3})
	if err != nil {
		t.Fatal(err)
	}
	if rs := res.LLM.Routes; rs.Attempted != 7 || rs.Accepted != 4 || rs.Rejected != 3 || res.Operations != 4 || res.Dropped != 0 {
		t.Fatalf("routes=%+v operations=%d dropped=%v", rs, res.Operations, res.DroppedIDs)
	}
	ops := map[string]compiler.Operation{}
	for _, op := range res.Doc.Operations {
		ops[op.Method+" "+op.Path] = op
		if op.Confidence > facts.VerificationCap(facts.VerifyText) {
			t.Fatalf("%s %s confidence %v exceeds the text cap", op.Method, op.Path, op.Confidence)
		}
	}
	get := ops["GET /api/users/{id}"]
	if get.OperationID != "getUser" || len(get.Params) != 2 || len(get.Responses) != 2 || get.Responses[1].Status != "400" {
		t.Fatalf("get = %+v", get)
	}
	create := ops["POST /api/users"]
	if create.Body == nil || create.Body.SchemaName != "createUserRequest" || create.Responses[0].Status != "201" {
		t.Fatalf("create = %+v", create)
	}
	orders := ops["GET /api/orders"]
	if len(orders.Params) != 2 || orders.Responses[0].SchemaName != "" || !strings.Contains(strings.Join(orders.Unknowns, ";"), "rejected") {
		t.Fatalf("orders: ghost field must reject the whole body shape: %+v", orders)
	}
	if h := ops["GET /health"]; h.OperationID != "getHealth" || h.Responses[0].SchemaName == "" {
		t.Fatalf("health = %+v", h)
	}
	if c := res.LLM.Contracts; c.Attempted != 4 || c.Rejected != 2 || c.Accepted != 2 {
		t.Fatalf("contracts = %+v", c)
	}
}

// TestGenericFrontendOffline 通用前端离线不可用：返回配置错误并提示 --llm，绝不编造。
func TestGenericFrontendOffline(t *testing.T) {
	_, err := Run(context.Background(), Config{RepoDir: filepath.Join("..", "..", "testdata", "express-repo"), OutDir: t.TempDir()})
	if err == nil || !errorsIs(err, ErrConfig) || !strings.Contains(err.Error(), "--llm") {
		t.Fatalf("want ErrConfig mentioning --llm, got %v", err)
	}
}
