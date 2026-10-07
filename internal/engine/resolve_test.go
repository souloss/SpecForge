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

// Page 上游响应：JSON 名（itemCount）只出现在 tag 里，嵌套匿名结构体。
type Page struct {
	Result struct {
		ItemCount int ` + "`json:\"itemCount\"`" + `
	} ` + "`json:\"result\"`" + `
}

func Fetch() Page { return Page{} }

func Paged() any { return Fetch().Result }
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

// TestApplyShapesEmptyObjectKeepsEvidence 空对象形状无字段证据：以 handler 首行兜底，schema 事实不被证据闸门丢弃。
func TestApplyShapesEmptyObjectKeepsEvidence(t *testing.T) {
	lp := fixturePhase(t)
	lp.schemas = map[string]*schema.Schema{}
	it := shapeItem(lp, anyPayload(), "m/svc.Data")
	out := gapOutcome{cp: cloneContract(it.cp), minConf: 1}
	lp.applyShapes(&out, []infer.ResolvedShape{{Gap: "s0", Shape: obj()}}, it)
	if out.accepted != 1 || len(out.schemas) != 1 {
		t.Fatalf("outcome = %+v", out)
	}
	evs := out.schemas[0].Evidence
	if len(evs) != 1 || !filepath.IsAbs(evs[0].File) || !strings.HasPrefix(evs[0].Quote, "llm:shape ← ") {
		t.Fatalf("fallback evidence = %+v", evs)
	}
}

// TestApplyShapesAcceptsTaggedFields 字段名只出现在切片引用结构体的 json tag 里：放行，证据指向字段声明行。
func TestApplyShapesAcceptsTaggedFields(t *testing.T) {
	lp := fixturePhase(t)
	lp.schemas = map[string]*schema.Schema{}
	it := shapeItem(lp, anyPayload(), "m/svc.Paged")
	out := gapOutcome{cp: cloneContract(it.cp), minConf: 1}
	lp.applyShapes(&out, []infer.ResolvedShape{{Gap: "s0", Shape: obj(
		infer.ShapeProp{Name: "itemCount", Shape: scalar("integer")})}}, it)
	if out.accepted != 1 || len(out.schemas) != 1 {
		t.Fatalf("outcome = %+v", out)
	}
	ev := out.schemas[0].Evidence
	if len(ev) != 1 || !strings.Contains(ev[0].Quote, "llm:shape-field itemCount ← ItemCount int") {
		t.Fatalf("evidence = %+v", ev)
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

// handlerSite 夹具中 Handler 的错误构造点（svc.go 第 10 行）。
const handlerSite = "NewError(ErrA)@svc.go:10"

// sitePayload 未解析行带来源点的载荷。
func sitePayload(sites ...string) facts.ContractPayload {
	cp := unresolvedPayload()
	cp.Responses[0].ErrSource = facts.JoinSites(sites)
	cp.Responses[0].Sink = "svc.go:10"
	return cp
}

// TestApplyErrorSitesClosesUnresolved 全部来源点定性通过复核时写出占位码行并闭合缺口。
func TestApplyErrorSitesClosesUnresolved(t *testing.T) {
	lp := fixturePhase(t)
	out := gapOutcome{cp: sitePayload(handlerSite), minConf: 1}
	lp.applyErrorCodes(&out, &infer.GapResolution{ErrorSites: []infer.ResolvedSite{
		{Site: "whatever@svc.go:10", Kind: infer.SiteKindDynamic}, // 表达式部分可改写，按位置回指
	}}, lp.slice("m/svc.Handler", nil))
	if out.accepted != 1 || out.rejected != 0 {
		t.Fatalf("accepted=%d rejected=%d, want 1/0", out.accepted, out.rejected)
	}
	if len(out.cp.Responses) != 1 || len(out.cp.Gaps) != 0 {
		t.Fatalf("unresolved row and gap should be replaced, got %+v gaps=%v", out.cp.Responses, out.cp.Gaps)
	}
	row := out.cp.Responses[0]
	if row.Envelope.Code != facts.DynamicCode || row.Source != "llm" || row.ErrSource != "whatever@svc.go:10" || row.Sink != "svc.go:10" {
		t.Fatalf("dynamic row not written back correctly: %+v", row)
	}
	if out.minConf != facts.VerificationCap(facts.VerifySymbol) {
		t.Fatalf("site classification should cap at symbol verification, got %v", out.minConf)
	}
	if len(out.evidence) != 1 || !strings.Contains(out.evidence[0].Quote, "NewError(ErrA)") {
		t.Fatalf("evidence should quote the site line, got %+v", out.evidence)
	}
}

// TestApplyErrorSitesRejects 非本行来源点、未知定性与复核不过的 catalog 码均拒绝，未解析行保留。
func TestApplyErrorSitesRejects(t *testing.T) {
	lp := fixturePhase(t)
	for name, rs := range map[string]infer.ResolvedSite{
		"foreign site":   {Site: "x@svc.go:12", Kind: infer.SiteKindUncoded},
		"unknown kind":   {Site: handlerSite, Kind: "maybe"},
		"catalog absent": {Site: handlerSite, Kind: infer.SiteKindCatalog, CodeRefs: []string{"ErrB"}},
		"catalog empty":  {Site: handlerSite, Kind: infer.SiteKindCatalog},
	} {
		out := gapOutcome{cp: sitePayload(handlerSite), minConf: 1}
		lp.applyErrorCodes(&out, &infer.GapResolution{ErrorSites: []infer.ResolvedSite{rs}}, lp.slice("m/svc.Handler", nil))
		if out.rejected != 1 || !hasUnresolved(out.cp) {
			t.Errorf("%s: rejected=%d, unresolved kept=%v", name, out.rejected, hasUnresolved(out.cp))
		}
	}
}

// TestApplyErrorSitesCatalogAndPartial catalog 定性的码按目录复核写回；只定性部分来源点时不闭合。
func TestApplyErrorSitesCatalogAndPartial(t *testing.T) {
	lp := fixturePhase(t)
	sl := lp.slice("m/svc.Handler", nil)
	out := gapOutcome{cp: sitePayload(handlerSite), minConf: 1}
	lp.applyErrorCodes(&out, &infer.GapResolution{ErrorSites: []infer.ResolvedSite{
		{Site: handlerSite, Kind: infer.SiteKindCatalog, CodeRefs: []string{"ErrA"}},
	}}, sl)
	if out.accepted != 1 || hasUnresolved(out.cp) || out.cp.Responses[0].Envelope.Code != 1001 {
		t.Fatalf("catalog site should close with ErrA row, got %+v", out.cp.Responses)
	}

	out = gapOutcome{cp: sitePayload(handlerSite, "y@svc.go:12"), minConf: 1}
	lp.applyErrorCodes(&out, &infer.GapResolution{ErrorSites: []infer.ResolvedSite{
		{Site: handlerSite, Kind: infer.SiteKindUncoded},
	}}, sl)
	if out.accepted != 1 || !hasUnresolved(out.cp) {
		t.Fatalf("partial classification must keep the unresolved row, got %+v", out.cp.Responses)
	}
}

// TestSiteContextsInjected 来源点窗口带行号注入任务，切片外的来源点跳过。
func TestSiteContextsInjected(t *testing.T) {
	lp := fixturePhase(t)
	ctxs := lp.siteContexts(lp.slice("m/svc.Handler", nil), []string{handlerSite, "z@other.go:3"})
	if len(ctxs) != 1 || ctxs[0].Site != handlerSite || ctxs[0].File != "svc/svc.go" ||
		!strings.Contains(ctxs[0].Code, "10| func Handler() error") {
		t.Fatalf("unexpected site contexts: %+v", ctxs)
	}
}

// hasUnresolved 载荷是否仍有未解析错误行。
func hasUnresolved(cp facts.ContractPayload) bool {
	for _, r := range cp.Responses {
		if r.Envelope != nil && r.Envelope.Code == facts.UnresolvedCode {
			return true
		}
	}
	return false
}

// scriptedProvider 按调用次序返回预设答案的 Provider（self-correct 测试用），记录每次请求 prompt。
type scriptedProvider struct {
	answers []string // 第 i 次调用返回 answers[i]（越界回最后一个）
	reqs    []infer.Request
}

func (s *scriptedProvider) Name() string { return "scripted" }
func (s *scriptedProvider) Complete(_ context.Context, req infer.Request) (infer.Response, error) {
	s.reqs = append(s.reqs, req)
	i := len(s.reqs) - 1
	if i >= len(s.answers) {
		i = len(s.answers) - 1
	}
	return infer.Response{Text: []byte(s.answers[i])}, nil
}

// TestSelfCorrectRetriesRejectedCodes self-correct：首轮幻觉码被拒后，把拒绝回执喂回模型重答，
// 第二轮答对则被采纳；provider 收到两次调用，第二次 prompt 含拒绝回执。
func TestSelfCorrectRetriesRejectedCodes(t *testing.T) {
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
	p := &scriptedProvider{answers: []string{
		`{"errorCodes":[{"codeRef":"m/svc.ErrFake"}]}`, // 首轮：幻觉码 → 拒
		`{"errorCodes":[{"codeRef":"m/svc.ErrA"}]}`,    // 二轮：答对 → 采纳
	}}
	lp := newLLMPhase(golang.NewProgram(g), catalog, 0, p, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sl := lp.slice("m/svc.Handler", nil)
	it := gapItem{cp: unresolvedPayload(), handler: "m/svc.Handler", sl: sl,
		task: infer.GapTask{Method: "GET", Path: "/x", Gaps: []string{facts.GapErrorUnresolved}}}
	out := lp.resolveOne(context.Background(), it)
	if out.err != nil {
		t.Fatal(out.err)
	}
	if len(p.reqs) != 2 {
		t.Fatalf("provider calls = %d, want 2 (self-correct retried)", len(p.reqs))
	}
	if !strings.Contains(p.reqs[1].Prompt, "ErrFake") {
		t.Fatalf("second prompt should carry the rejection receipt, got:\n%s", p.reqs[1].Prompt)
	}
	if out.accepted != 1 || out.rejected != 0 {
		t.Fatalf("accepted=%d rejected=%d, want 1/0", out.accepted, out.rejected)
	}
	var got *facts.ResponseFact
	for i := range out.cp.Responses {
		if out.cp.Responses[i].Envelope.Code == 1001 {
			got = &out.cp.Responses[i]
		}
	}
	if got == nil || got.Source != "llm" {
		t.Fatalf("ErrA not written back after self-correct: %+v", out.cp.Responses)
	}
}

// criticProvider 返回预设 critic 输出的 Provider（critic 测试用）。
type criticProvider struct{ out string }

func (c *criticProvider) Name() string { return "critic" }
func (c *criticProvider) Complete(_ context.Context, _ infer.Request) (infer.Response, error) {
	return infer.Response{Text: []byte(c.out)}, nil
}

// TestCriticAddsMissingResponse critic 补上 executor 漏掉的 404 分支（shape 字段须在源码里出现）。
func TestCriticAddsMissingResponse(t *testing.T) {
	lp := fixturePhase(t)
	// Data() 的 map 字面量含 "total" 字符串键；以 Data 为 handler，critic 报的字段名 "total" 能在源码中找到。
	cp := facts.ContractPayload{OperationID: "data", Responses: []facts.ResponseFact{{Status: 200, Envelope: &facts.Envelope{Code: 0}}}}
	it := criticItem{key: "GET /x", cp: cp, handler: "m/svc.Data"}
	sl := lp.slice("m/svc.Data", nil)
	accepted := lp.applyCriticFindings(it, &infer.CriticResult{Findings: []infer.CriticFinding{
		{Kind: "missing_response", Status: 404, Error: true, Shape: &infer.ShapeNode{Type: "object", Properties: []infer.ShapeProp{{Name: "total", Shape: &infer.ShapeNode{Type: "integer"}}}}},
	}}, sl)
	if len(accepted) != 2 { // 一份 schema 事实 + 一份 contract 事实
		t.Fatalf("accepted facts = %d, want 2", len(accepted))
	}
	var contractFact *facts.Fact
	for _, f := range accepted {
		if f.Kind == facts.KindContract {
			contractFact = f
		}
	}
	if contractFact == nil {
		t.Fatal("no contract fact from critic")
	}
	got, _ := contractFact.Contract()
	found404 := false
	for _, r := range got.Responses {
		if r.Status == 404 && r.Failure && r.Source == "llm" {
			found404 = true
		}
	}
	if !found404 {
		t.Fatalf("404 critic response not merged: %+v", got.Responses)
	}
}

// TestCriticRejectsHallucinatedField critic 报的字段名不在源码里：整条 finding 被弃。
func TestCriticRejectsHallucinatedField(t *testing.T) {
	lp := fixturePhase(t)
	cp := facts.ContractPayload{OperationID: "data", Responses: []facts.ResponseFact{{Status: 200, Envelope: &facts.Envelope{Code: 0}}}}
	it := criticItem{key: "GET /x", cp: cp, handler: "m/svc.Data"}
	sl := lp.slice("m/svc.Data", nil)
	accepted := lp.applyCriticFindings(it, &infer.CriticResult{Findings: []infer.CriticFinding{
		{Kind: "missing_response", Status: 500, Error: true, Shape: &infer.ShapeNode{Type: "object", Properties: []infer.ShapeProp{{Name: "ghostField", Shape: &infer.ShapeNode{Type: "string"}}}}},
	}}, sl)
	if len(accepted) != 0 {
		t.Fatalf("hallucinated critic finding must be rejected, got %d facts", len(accepted))
	}
}
