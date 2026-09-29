package engine

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/specforge/specforge/internal/codegraph"
	"github.com/specforge/specforge/internal/facts"
	"github.com/specforge/specforge/internal/frontend"
	"github.com/specforge/specforge/internal/frontend/golang"
	"github.com/specforge/specforge/internal/infer"
	"github.com/specforge/specforge/internal/loader"
	"github.com/specforge/specforge/internal/schema"
	"github.com/specforge/specforge/internal/verify"
)

// mockProvider 返回预设 JSON 的 Provider，记录调用次数（离线测试 LLM 阶段）。
type mockProvider struct {
	out   string
	calls int
}

func (m *mockProvider) Name() string { return "mock" }
func (m *mockProvider) Complete(_ context.Context, _ infer.Request) (infer.Response, error) {
	m.calls++
	return infer.Response{Text: []byte(m.out), Usage: infer.Usage{Calls: 1, InputTokens: 10, OutputTokens: 5}}, nil
}

// fixtureSrc 测试夹具模块：Handler 返回 ErrA（ErrB 从不出现），Data 返回带字面量键的 map。
const fixtureSrc = `package svc

import "errors"

const ErrA = 1001
const ErrB = 1002

func NewError(code int) error { return errors.New("x") }

func Handler() error { return NewError(ErrA) }

func Data() map[string]any { return map[string]any{"total": 1, "items": nil} }
`

// fixturePhase 在临时目录构造夹具模块并返回 LLM 阶段上下文。
func fixturePhase(t *testing.T) *llmPhase {
	t.Helper()
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module m\n\ngo 1.21\n"), 0o644))
	must(t, os.MkdirAll(filepath.Join(dir, "svc"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "svc", "svc.go"), []byte(fixtureSrc), 0o644))
	l, err := loader.LoadRepo(dir)
	must(t, err)
	g, err := codegraph.Build(l.Pkgs, l.Fset)
	must(t, err)
	g.SetRoot(l.Root)
	catalog := map[string]frontend.ErrorCode{
		"m/svc.ErrA": {Symbol: "m/svc.ErrA", Code: 1001, Msg: "a"},
		"m/svc.ErrB": {Symbol: "m/svc.ErrB", Code: 1002, Msg: "b"},
	}
	return newLLMPhase(golang.NewProgram(g), catalog, 0, &mockProvider{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// unresolvedPayload 只有一个未解析错误行的载荷。
func unresolvedPayload() facts.ContractPayload {
	return facts.ContractPayload{
		OperationID: "handler",
		Gaps:        []string{"error envelope: err variable not statically resolved"},
		Responses:   []facts.ResponseFact{{Status: 200, Envelope: &facts.Envelope{Code: facts.UnresolvedCode, CodeRef: "unresolved"}}},
	}
}

// TestApplyErrorCodesVerifiesAgainstSlice 目录外码与切片中不出现的码都被拒绝；出现的码带行级证据写回。
func TestApplyErrorCodesVerifiesAgainstSlice(t *testing.T) {
	lp := fixturePhase(t)
	sl := lp.slice("m/svc.Handler", nil)
	out := gapOutcome{cp: unresolvedPayload()}
	lp.applyErrorCodes(&out, &infer.GapResolution{ErrorCodes: []infer.ResolvedError{
		{CodeRef: "m/svc.ErrFake"}, // 目录外：幻觉
		{CodeRef: "ErrB"},          // 目录内但切片中未出现：无证据
		{CodeRef: "ErrA"},          // 通过
	}}, sl)
	if out.accepted != 1 || out.rejected != 2 {
		t.Fatalf("accepted=%d rejected=%d, want 1/2", out.accepted, out.rejected)
	}
	var got *facts.ResponseFact
	for i := range out.cp.Responses {
		if out.cp.Responses[i].Envelope.Code == 1001 {
			got = &out.cp.Responses[i]
		}
	}
	if got == nil || got.Source != "llm" || got.Envelope.CodeRef != "m/svc.ErrA" {
		t.Fatalf("ErrA row not written back correctly: %+v", out.cp.Responses)
	}
	if len(out.evidence) != 1 || !strings.Contains(out.evidence[0].Quote, "NewError(ErrA)") {
		t.Fatalf("evidence should quote the source line, got %+v", out.evidence)
	}
	// 有拒绝项：即使声明 exhaustive 也保留未解析行（本例未声明）。
	if out.cp.Responses[len(out.cp.Responses)-1].Envelope.Code != facts.UnresolvedCode {
		t.Fatal("unresolved row must be kept")
	}
}

// TestApplyErrorCodesExhaustiveRemovesUnresolved 全部码通过复核且声明 exhaustive 时移除未解析行与缺口。
func TestApplyErrorCodesExhaustiveRemovesUnresolved(t *testing.T) {
	lp := fixturePhase(t)
	out := gapOutcome{cp: unresolvedPayload()}
	lp.applyErrorCodes(&out, &infer.GapResolution{Exhaustive: true, ErrorCodes: []infer.ResolvedError{{CodeRef: "m/svc.ErrA"}}},
		lp.slice("m/svc.Handler", nil))
	for _, r := range out.cp.Responses {
		if r.Envelope.Code == facts.UnresolvedCode {
			t.Fatal("unresolved row should be removed")
		}
	}
	if len(out.cp.Gaps) != 0 {
		t.Fatalf("error gap should be closed, got %v", out.cp.Gaps)
	}
}

// anyPayload any 响应缺口载荷（成功行无 schema，带字段候选）。
func anyPayload() facts.ContractPayload {
	return facts.ContractPayload{
		OperationID:    "data",
		Gaps:           []string{"success data: any/interface{} cannot be typed"},
		Responses:      []facts.ResponseFact{{Status: 200, Envelope: &facts.Envelope{Code: 0}}},
		DataCandidates: []facts.DataCandidateFact{{Name: "items"}, {Name: "total"}},
	}
}

// shapeItem 组装形状缺口的 gapItem（切片取 handler 的可达源码）。
func shapeItem(lp *llmPhase, cp facts.ContractPayload, handler string) gapItem {
	it := gapItem{cp: cp, handler: handler, sl: lp.slice(handler, nil)}
	it.targets = lp.shapeTargets(cp)
	return it
}

// obj / scalar 形状树构造辅助。
func obj(props ...infer.ShapeProp) *infer.ShapeNode {
	return &infer.ShapeNode{Type: "object", Properties: props}
}
func scalar(t string) *infer.ShapeNode { return &infer.ShapeNode{Type: t} }

// TestApplyShapesVerified 根形状（成功 data 为 any）：字段名在源码中出现、类型封闭 → 写回专属 schema，核对强度 text。
func TestApplyShapesVerified(t *testing.T) {
	lp := fixturePhase(t)
	lp.schemas = map[string]*schema.Schema{}
	it := shapeItem(lp, anyPayload(), "m/svc.Data")
	if len(it.targets) != 1 {
		t.Fatalf("targets = %+v", it.targets)
	}
	out := gapOutcome{cp: cloneContract(it.cp), minConf: 1}
	lp.applyShapes(&out, []infer.ResolvedShape{{Gap: "s0", Shape: obj(
		infer.ShapeProp{Name: "total", Required: true, Shape: scalar("integer")},
		infer.ShapeProp{Name: "items", Shape: &infer.ShapeNode{Type: "array", Items: scalar("string")}},
	)}}, it)
	if out.accepted != 1 || len(out.schemas) != 1 || len(out.cp.Gaps) != 0 {
		t.Fatalf("outcome = %+v", out)
	}
	if !strings.HasPrefix(out.cp.Responses[0].SchemaType, llmShapePrefix) || out.minConf != facts.VerificationCap(facts.VerifyText) {
		t.Fatalf("row/conf not updated: %+v conf=%v", out.cp.Responses[0], out.minConf)
	}
	f := out.schemas[0]
	sc, _ := f.Schema()
	if f.Verification != facts.VerifyText || len(sc.Props) != 2 || len(f.Evidence) == 0 {
		t.Fatalf("fact = %+v schema = %+v", f, sc)
	}
}

// TestApplyShapesRejects 源码中不存在的字段名、集外类型、超深嵌套：整棵拒绝，缺口保留。
func TestApplyShapesRejects(t *testing.T) {
	lp := fixturePhase(t)
	lp.schemas = map[string]*schema.Schema{}
	deep := scalar("string")
	for i := 0; i <= verify.MaxShapeDepth+1; i++ {
		deep = obj(infer.ShapeProp{Name: "total", Shape: deep})
	}
	for _, shape := range []*infer.ShapeNode{
		obj(infer.ShapeProp{Name: "phantom", Shape: scalar("string")}),
		obj(infer.ShapeProp{Name: "total", Shape: scalar("decimal")}),
		deep,
	} {
		it := shapeItem(lp, anyPayload(), "m/svc.Data")
		out := gapOutcome{cp: cloneContract(it.cp), minConf: 1}
		lp.applyShapes(&out, []infer.ResolvedShape{{Gap: "s0", Shape: shape}}, it)
		if out.accepted != 0 || out.rejected != 1 || len(out.cp.Gaps) != 1 {
			t.Fatalf("shape %+v should be rejected, outcome=%+v", shape, out)
		}
	}
}

// TestApplyShapesNestedPath 嵌套路径（PageResult.list 为 any）：只替换该路径，其它字段保持，原 schema 不被修改。
func TestApplyShapesNestedPath(t *testing.T) {
	lp := fixturePhase(t)
	base := &schema.Schema{Type: "object", Props: []schema.Prop{
		{Name: "count", Schema: &schema.Schema{Type: "integer"}},
		{Name: "list", Schema: &schema.Schema{Unknown: true}},
	}}
	lp.schemas = map[string]*schema.Schema{"m/svc.Page": base}
	cp := facts.ContractPayload{
		OperationID: "data",
		Gaps:        []string{facts.GapSchemaAny("m/svc.Page", "list")},
		Responses:   []facts.ResponseFact{{Status: 200, Envelope: &facts.Envelope{Code: 0}, SchemaType: "m/svc.Page"}},
	}
	it := shapeItem(lp, cp, "m/svc.Data")
	out := gapOutcome{cp: cloneContract(it.cp), minConf: 1}
	lp.applyShapes(&out, []infer.ResolvedShape{{Gap: "s0", Shape: &infer.ShapeNode{Type: "array",
		Items: obj(infer.ShapeProp{Name: "total", Shape: scalar("integer")})}}}, it)
	if out.accepted != 1 || len(out.schemas) != 1 {
		t.Fatalf("outcome = %+v", out)
	}
	sc, _ := out.schemas[0].Schema()
	if sc.Props[0].Name != "count" || sc.Props[1].Schema.Type != "array" || sc.Props[1].Schema.Items.Props[0].Name != "total" {
		t.Fatalf("replaced schema = %+v", sc)
	}
	if !base.Props[1].Schema.Unknown {
		t.Fatal("base schema must not be mutated")
	}
}

// TestResolveGapsWritesBackInOrder 端到端：mock provider → 复核 → 写回 contract 事实（并发下结果不变）。
func TestResolveGapsWritesBackInOrder(t *testing.T) {
	lp := fixturePhase(t)
	lp.p = &mockProvider{out: "```json\n{\"errorCodes\":[{\"codeRef\":\"ErrA\"}],\"exhaustive\":true}\n```"}
	f := &facts.Fact{ID: "contract:op:GET:/h", Kind: facts.KindContract, Value: unresolvedPayload(), Confidence: 0.75}
	routes := map[string]string{"op:GET:/h": "m/svc.Handler"}
	_, st := lp.resolveGaps(context.Background(), []*facts.Fact{f}, routes, 0, 4)
	if st.Attempted != 1 || st.Accepted != 1 || st.Failed != 0 {
		t.Fatalf("stats = %+v", st)
	}
	cp := f.Value.(facts.ContractPayload)
	if len(cp.Gaps) != 0 || f.Confidence != facts.VerificationCap(facts.VerifyTyped) || len(f.Evidence) != 1 {
		t.Fatalf("fact not updated: conf=%v gaps=%v ev=%v", f.Confidence, cp.Gaps, f.Evidence)
	}
}

// TestResolveGapsBudget 预算只处理收益最高的缺口，其余计入 BudgetSkipped。
func TestResolveGapsBudget(t *testing.T) {
	lp := fixturePhase(t)
	lp.p = &mockProvider{out: `{}`}
	var fl []*facts.Fact
	routes := map[string]string{}
	for _, p := range []string{"/a", "/b", "/c"} {
		fl = append(fl, &facts.Fact{ID: "contract:op:GET:" + p, Kind: facts.KindContract, Value: unresolvedPayload()})
		routes["op:GET:"+p] = "m/svc.Handler"
	}
	_, st := lp.resolveGaps(context.Background(), fl, routes, 2, 1)
	if st.Attempted != 2 || st.BudgetSkipped != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

// TestBuildToolsDeterministicAndSandboxed 工具输出确定（缓存读集校验的前提），read_source 不越出仓库。
func TestBuildToolsDeterministicAndSandboxed(t *testing.T) {
	lp := fixturePhase(t)
	tools := map[string]infer.ToolSpec{}
	for _, tl := range lp.tools {
		tools[tl.Name] = tl
	}
	ctx := context.Background()
	a, _ := tools["read_symbol"].Call(ctx, map[string]any{"id": "m/svc.Handler"})
	b, _ := tools["read_symbol"].Call(ctx, map[string]any{"id": "m/svc.Handler"})
	if a != b || !strings.Contains(a, "NewError(ErrA)") {
		t.Fatalf("read_symbol not deterministic or missing source: %s", a)
	}
	out, _ := tools["read_source"].Call(ctx, map[string]any{"file": "../../etc/passwd.go", "from": 1.0, "to": 5.0})
	if !strings.Contains(out, "error") {
		t.Fatalf("read_source must refuse paths outside the repo, got %s", out)
	}
	out, _ = tools["read_source"].Call(ctx, map[string]any{"file": "svc/svc.go", "from": 1.0, "to": 1.0})
	if !strings.Contains(out, "package svc") {
		t.Fatalf("read_source = %s", out)
	}
	found, _ := tools["find_symbol"].Call(ctx, map[string]any{"query": "handler"})
	if !strings.Contains(found, "m/svc.Handler") {
		t.Fatalf("find_symbol = %s", found)
	}
}

// TestLLMKeyOf 离线/LLM、不同预算的键不同；并发数不影响产物，不改变键。
func TestLLMKeyOf(t *testing.T) {
	p := &mockProvider{}
	off := llmKeyOf(Config{})
	a := llmKeyOf(Config{Provider: p, LLMBudget: 6, LLMConcurrency: 1})
	b := llmKeyOf(Config{Provider: p, LLMBudget: 6, LLMConcurrency: 8})
	c := llmKeyOf(Config{Provider: p, LLMBudget: 2})
	if off != "offline" || a == off || a != b || a == c {
		t.Fatalf("keys: off=%q a=%q b=%q c=%q", off, a, b, c)
	}
}

// TestBudgetOnlyCountsPaidCalls 预算只约束未缓存的调用：第二次运行缓存项免费，再推进一个新缺口。
func TestBudgetOnlyCountsPaidCalls(t *testing.T) {
	lp := fixturePhase(t)
	inner := &mockProvider{out: `{}`}
	lp.p = infer.NewCachedProvider(inner, t.TempDir())
	mk := func() ([]*facts.Fact, map[string]string) {
		var fl []*facts.Fact
		routes := map[string]string{}
		for _, p := range []string{"/a", "/b", "/c"} {
			fl = append(fl, &facts.Fact{ID: "contract:op:GET:" + p, Kind: facts.KindContract, Value: unresolvedPayload()})
			routes["op:GET:"+p] = "m/svc.Handler"
		}
		return fl, routes
	}
	fl, routes := mk()
	if _, st := lp.resolveGaps(context.Background(), fl, routes, 1, 1); st.Attempted != 1 || st.BudgetSkipped != 2 {
		t.Fatalf("run1 stats = %+v", st)
	}
	fl, routes = mk()
	if _, st := lp.resolveGaps(context.Background(), fl, routes, 1, 1); st.Attempted != 2 || st.BudgetSkipped != 1 || inner.calls != 2 {
		t.Fatalf("run2 stats = %+v, model calls = %d", st, inner.calls)
	}
}

// TestEnrichOperationsEvidence 语义增强产出带 handler 源码证据的事实，且能通过证据闸门。
func TestEnrichOperationsEvidence(t *testing.T) {
	lp := fixturePhase(t)
	lp.p = &mockProvider{out: `{"summary":"Returns error A","description":"demo"}`}
	routes := map[string]string{"op:GET:/h": "m/svc.Handler"}
	got := lp.enrichOperations(context.Background(), nil, routes, 2)
	if len(got) != 1 {
		t.Fatalf("want 1 enrichment fact, got %d", len(got))
	}
	kept, dropped := verifyEvidence(lp.prog, got)
	if len(kept) != 1 || len(dropped) != 0 || kept[0].Evidence[0].File != "svc/svc.go" {
		t.Fatalf("evidence gate: kept=%v dropped=%v", kept, dropped)
	}
	if p := kept[0].Value.(facts.EnrichmentPayload); p.Summary != "Returns error A" {
		t.Fatalf("payload = %+v", p)
	}
}
