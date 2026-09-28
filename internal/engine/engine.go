// Package engine 编排生成流水线（设计文档 §6.1 首次全量 12 步的 P0 实现）。
//
// 流程: 摄入 → 服务拓扑 → 框架识别 → 画像 → 路由抽取 →
// handler 分析（请求绑定/参数/中间件） → 响应追踪（切片+汇聚点） →
// Schema 合成 → 横切合成（security/错误码） → 语义增强（godoc） →
// 确定性编译 → 校验输出。
package engine

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/specforge/specforge/internal/adapter"
	"github.com/specforge/specforge/internal/codegraph"
	"github.com/specforge/specforge/internal/compiler"
	"github.com/specforge/specforge/internal/facts"
	"github.com/specforge/specforge/internal/infer"
	"github.com/specforge/specforge/internal/loader"
	"github.com/specforge/specforge/internal/memo"
	"github.com/specforge/specforge/internal/profile"
	"github.com/specforge/specforge/internal/slicing"
	"github.com/specforge/specforge/internal/typeschema"

	"golang.org/x/tools/go/packages"
)

// Version 引擎版本。分析行为（适配器/类型映射/编译）或依赖版本变化时须递增，
// 否则 memo 指纹会把旧引擎的缓存误判为新鲜（设计文档 §5.2 缓存键含 EngineVersion）。
const Version = "0.4.0"

// Config 生成配置。
type Config struct {
	RepoDir     string
	Service     string
	ProfilePath string
	OutDir      string
	// MemoDir 运行级 memo 缓存目录；空 = 禁用（每次全量重算）。
	MemoDir string
	// Provider 可选 LLM 供应方；非 nil 时对静态缺口做档位2 兜底采集。
	Provider infer.Provider
	// LLMBudget LLM 兜底采集的 operation 上限（0 = 不限，默认建议设值防全量卡死）。
	LLMBudget int
}

// Result 运行统计（CLI 与 --json 消费）。
type Result struct {
	Services         string
	Packages         int
	Symbols          int
	CallEdges        int
	Routes           int
	RoutesResolved   int
	RoutesUnresolved int
	Operations       int
	SchemaTypes      int
	SinkSites        int
	Facts            int
	Dropped          int
	LowConf          int
	Doc              *compiler.Document
	OutDir           string
	Repo             string
	FactsList        []*facts.Fact
	Cached           bool // 命中运行级 memo 缓存（本次未重新分析）
	// Gaps 静态缺口清单（档位判定输入）：每条 "METHOD /path: gap1; gap2"。
	Gaps []string
	// LLMCalls LLM 兜底采集次数（0 = 未启用或全部静态可定型）。
	LLMCalls int
}

// Run 执行完整生成。
func Run(cfg Config) (*Result, error) {
	// 0. 运行级 memo 缓存：输入指纹未变直接复用上次产物，跳过整个分析管线。
	fp, err := memo.Fingerprint(cfg.RepoDir, cfg.Service, cfg.ProfilePath, Version)
	if err != nil {
		return nil, err
	}
	store := memo.New(cfg.MemoDir)
	if spec, report, summary, ok := store.Load(fp); ok {
		outDir := cfg.OutDir
		if outDir == "" {
			outDir = filepath.Join(cfg.RepoDir, ".specforge", "out")
		}
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(outDir, "openapi.yaml"), spec, 0o644); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(outDir, "report.md"), report, 0o644); err != nil {
			return nil, err
		}
		return &Result{
			Services:         summary.Service,
			Repo:             cfg.RepoDir,
			Operations:       summary.Operations,
			Routes:           summary.Routes,
			RoutesResolved:   summary.Routes - summary.RoutesUnresolved,
			RoutesUnresolved: summary.RoutesUnresolved,
			SchemaTypes:      summary.SchemaTypes,
			Facts:            summary.Facts,
			LowConf:          summary.LowConfidence,
			OutDir:           outDir,
			Cached:           true,
		}, nil
	}

	// 1. 仓库摄入
	l, err := loader.LoadRepo(cfg.RepoDir)
	if err != nil {
		return nil, err
	}
	svcName := cfg.Service
	if svcName == "" && len(l.Services) > 0 {
		svcName = l.Services[0].Name
	}
	// 未显式指定 --service 时，默认全仓分析（所有服务），而不是回落到第一个服务。
	filterName := cfg.Service

	// 1-2. 服务拓扑 + 框架识别
	framework := detectFrameworkOf(l)
	pkgs := l.ServiceFilter(filterName)

	// 3. 约定画像
	prof, err := loadProfile(cfg, l)
	if err != nil {
		return nil, err
	}

	// 代码图
	g, err := codegraph.Build(pkgs, l.Fset)
	if err != nil {
		return nil, err
	}

	res := &Result{
		Services: svcName, Repo: cfg.RepoDir,
		Packages: len(pkgs), Symbols: len(g.Syms),
	}
	for _, cs := range g.Callers {
		res.CallEdges += len(cs)
	}

	// 4. 路由提取
	routes := extractAllRoutes(g)
	res.Routes = len(routes)

	// 通配符展开（profile 规则，设计文档 §3.3）
	routes = expandWildcards(routes, prof)

	var resolved []adapter.Route
	for _, r := range routes {
		if r.Unresolved == "" {
			res.RoutesResolved++
			resolved = append(resolved, r)
		} else {
			res.RoutesUnresolved++
		}
	}

	// 5-6. handler 分析 + 响应追踪
	slicer := slicing.New(g, prof)
	synth := typeschema.New(g)
	var factList []*facts.Fact

	// 8. 横切: 错误码目录
	catalog := slicer.BuildErrorCatalog()

	for _, r := range resolved {
		f, err := buildOperationFacts(g, prof, synth, slicer, r, catalog)
		if err != nil {
			return nil, fmt.Errorf("op %s %s: %w", r.Method, r.Path, err)
		}
		factList = append(factList, f...)
		res.SinkSites += len(f)
	}

	// 聚合静态缺口清单（GapReport）：LLM 兜底之前的待补项，档位判定输入。
	res.Gaps = collectGaps(factList)

	// 9. LLM 兜底采集（档位2 模板化）：对静态缺口做单次调用补全事实。
	// 显式启用（cfg.Provider != nil）才执行；离线时缺口保持 unknown，绝不静默编造。
	// LLMBudget 限制本次兜底的 operation 数（防全量串行卡死）。
	if cfg.Provider != nil {
		var llmSchemas []*facts.Fact
		res.LLMCalls, llmSchemas = resolveGaps(factList, g, prof, catalog, cfg.Provider, cfg.LLMBudget)
		// LLM 兜底补出的 any 响应 schema 事实并入 factList，随既有管线编译。
		factList = append(factList, llmSchemas...)
	}

	// 收集全部 schema 事实（去重 by type ID）
	schemaFacts := map[string]*typeschema.Schema{}
	for _, f := range factList {
		if f.Kind == facts.KindSchema {
			if s, ok := f.Value.(*typeschema.Schema); ok {
				schemaFacts[strings.TrimPrefix(f.ID, "schema:")] = s
			}
		}
	}
	res.SchemaTypes = len(schemaFacts)

	// 统计
	res.FactsList = factList
	res.Facts = len(factList)
	for _, f := range factList {
		if f.Confidence < 0.8 {
			res.LowConf++
		}
	}

	// 10-11. 确定性编译 + 校验
	doc, err := compiler.Compile(compiler.Input{
		ServiceName: svcName,
		Framework:   framework,
		Facts:       factList,
		Schemas:     schemaFacts,
		Graph:       g,
	})
	if err != nil {
		return nil, err
	}
	res.Doc = doc
	res.Operations = len(doc.Operations)

	outDir := cfg.OutDir
	if outDir == "" {
		outDir = filepath.Join(cfg.RepoDir, ".specforge", "out")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	res.OutDir = outDir

	specYAML, err := compiler.RenderYAML(doc)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(outDir, "openapi.yaml"), specYAML, 0o644); err != nil {
		return nil, err
	}
	report := renderReport(doc, res)
	if err := os.WriteFile(filepath.Join(outDir, "report.md"), []byte(report), 0o644); err != nil {
		return nil, err
	}
	// 写 memo 缓存：下次输入未变时直接复用产物。
	_ = store.Save(fp, specYAML, []byte(report), memo.Summary{
		Service:          svcName,
		Operations:       res.Operations,
		Routes:           res.Routes,
		RoutesUnresolved: res.RoutesUnresolved,
		SchemaTypes:      res.SchemaTypes,
		Facts:            res.Facts,
		LowConfidence:    res.LowConf,
	})
	return res, nil
}

// detectFrameworkOf 依赖 import 识别框架（F2）。
func detectFrameworkOf(l *loader.Loaded) string {
	for _, p := range l.Pkgs {
		for path := range p.Imports {
			if f := adapter.DetectFramework([]string{path}); f != "" {
				return f
			}
		}
	}
	return "unknown"
}

func loadProfile(cfg Config, l *loader.Loaded) (*profile.Profile, error) {
	if cfg.ProfilePath != "" {
		return profile.Load(cfg.ProfilePath)
	}
	// 默认查找仓库内 .specforge/profile.yaml
	cand := filepath.Join(cfg.RepoDir, ".specforge", "profile.yaml")
	if _, err := os.Stat(cand); err == nil {
		return profile.Load(cand)
	}
	return profile.Default(), nil
}

// extractAllRoutes 全仓路由抽取（fiber 模式）。
func extractAllRoutes(g *codegraph.Graph) []adapter.Route {
	var routes []adapter.Route
	for _, p := range g.Pkgs() {
		ctx := adapter.NewPkgCtx(p.PkgPath, p.Syntax, g,
			exprResolver(p), argTypesOf(p), recvTypeOf(p))
		fa := adapter.FiberAdapter{}
		routes = append(routes, fa.ExtractRoutes(ctx)...)
	}
	return routes
}

func exprResolver(p *packages.Package) func(ast.Expr) string {
	return func(e ast.Expr) string {
		info := p.TypesInfo
		switch x := ast.Unparen(e).(type) {
		case *ast.CallExpr:
			// 中间件构造器调用: auth.GetUserRelation() → 被调符号
			switch fn := x.Fun.(type) {
			case *ast.SelectorExpr:
				if sel, ok := info.Selections[fn]; ok && sel.Obj() != nil {
					if f, ok := sel.Obj().(*types.Func); ok {
						return methodIDStr(f)
					}
				}
				if obj := info.Uses[fn.Sel]; obj != nil {
					if f, ok := obj.(*types.Func); ok {
						return f.FullName()
					}
				}
			case *ast.Ident:
				if obj := info.Uses[fn]; obj != nil {
					if f, ok := obj.(*types.Func); ok {
						return f.FullName()
					}
				}
			}
			return ""
		case *ast.Ident:
			if obj := info.Uses[x]; obj != nil {
				if fn, ok := obj.(*types.Func); ok {
					return funcIDOf(fn)
				}
			}
		case *ast.SelectorExpr:
			if sel, ok := info.Selections[x]; ok && sel.Obj() != nil {
				if fn, ok := sel.Obj().(*types.Func); ok {
					return methodIDStr(fn)
				}
			}
			if obj := info.Uses[x.Sel]; obj != nil {
				if fn, ok := obj.(*types.Func); ok {
					return fn.FullName()
				}
			}
		}
		return ""
	}
}

func argTypesOf(p *packages.Package) func(*ast.CallExpr) []string {
	return func(call *ast.CallExpr) []string {
		var out []string
		for _, a := range call.Args {
			if tv, ok := p.TypesInfo.Types[a]; ok && tv.Type != nil {
				out = append(out, tv.Type.String())
			}
		}
		return out
	}
}

func recvTypeOf(p *packages.Package) func(ast.Expr) string {
	return func(e ast.Expr) string {
		if tv, ok := p.TypesInfo.Types[e]; ok && tv.Type != nil {
			return tv.Type.String()
		}
		return ""
	}
}

func funcIDOf(fn *types.Func) string {
	if fn.Signature().Recv() != nil {
		return methodIDStr(fn)
	}
	return fn.FullName()
}

func methodIDStr(fn *types.Func) string {
	sig := fn.Signature()
	pkgPath := ""
	if fn.Pkg() != nil {
		pkgPath = fn.Pkg().Path()
	} else {
		full := fn.FullName()
		if i := strings.LastIndex(full, "."); i > 0 {
			pkgPath = full[:i]
		}
	}
	if sig.Recv() != nil {
		recv := fullTypeID(sig.Recv().Type())
		return fmt.Sprintf("%s.(*%s).%s", pkgPath, recv, fn.Name())
	}
	return fn.FullName()
}

// fullTypeID 与 codegraph.typeID 一致（符号 ID 一致性的关键）。
func fullTypeID(t types.Type) string {
	t2 := t
	if pt, ok := t2.(*types.Pointer); ok {
		t2 = pt.Elem()
	}
	if n, ok := t2.(*types.Named); ok {
		if n.Obj().Pkg() != nil {
			return n.Obj().Pkg().Path() + "." + n.Obj().Name()
		}
		return n.Obj().Name()
	}
	return t2.String()
}

// expandWildcards 展开通配符路径。
//
// 优先级（设计文档 §3.3 的画像优先，外加自动推断兜底）：
//  1. profile 显式规则（wildcard_expansion）——人类对「版本歧义」的最终裁决；
//  2. 自动推断——当且仅当能从已解析路由中唯一确定一个具体版本段时；
//  3. 以上皆无 → 保留未解析（宁可少不可错，不注入幻觉路由）。
func expandWildcards(routes []adapter.Route, prof *profile.Profile) []adapter.Route {
	// 收集已解析路由的原始路径，供版本推断（v* → 具体版本段）。
	resolvedRaw := map[string]bool{}
	for _, r := range routes {
		if r.Unresolved == "" {
			resolvedRaw[r.RawPath] = true
		}
	}
	var out []adapter.Route
	for _, r := range routes {
		if r.Unresolved != adapter.ReasonWildcard {
			out = append(out, r)
			continue
		}
		// 1. profile 显式规则
		if expansions, ok := lookupExpansion(prof, r.RawPath); ok {
			for _, base := range expansions {
				out = append(out, expandTo(r, base))
			}
			continue
		}
		// 2. 自动推断（仅单版本确定时）
		if bases, ok := inferWildcardBases(r.RawPath, resolvedRaw); ok {
			for _, base := range bases {
				out = append(out, expandTo(r, base))
			}
			continue
		}
		// 3. 保留未解析
		out = append(out, r)
	}
	return out
}

// expandTo 把通配符路由按具体 base 前缀展开（base 如 "/ipo/v1"）。
func expandTo(r adapter.Route, base string) adapter.Route {
	suffix := strings.TrimPrefix(r.RawPath, wildcardPrefixOf(r.RawPath))
	normSuffix, _ := adapter.NormalizePathSuffix(suffix)
	nr := r
	nr.Path = base + normSuffix
	nr.Unresolved = ""
	return nr
}

// inferWildcardBases 自动推断通配符前缀的具体版本段。
//
// rawPath 形如 "/ipo/v*/OrderCreate"：取 `*` 前的段前缀 base="/ipo/v"，
// 在已解析路由里找以 base 开头、下一段为具体版本的路由（如 "/ipo/v1/Ping"），
// 收集去重后的版本段（"1"）。仅当恰好唯一一个版本段时返回该展开 base，
// 多版本歧义（如同时有 v1/v3）返回 false，交给 profile 裁决——把
// 「v* 到底指哪个版本」留给人类约定，绝不静态臆测注入 spurious 路由。
func inferWildcardBases(rawPath string, resolvedRaw map[string]bool) ([]string, bool) {
	star := strings.Index(rawPath, "*")
	if star < 0 {
		return nil, false
	}
	base := rawPath[:star] // 不含 '*'
	tokens := map[string]bool{}
	for p := range resolvedRaw {
		if !strings.HasPrefix(p, base) {
			continue
		}
		rest := p[len(base):]
		token := rest
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			token = rest[:i]
		}
		if token == "" {
			continue
		}
		tokens[token] = true
	}
	if len(tokens) != 1 {
		return nil, false
	}
	var base2 string
	for t := range tokens {
		base2 = base + t
	}
	return []string{base2}, true
}

// lookupExpansion 通配符展开规则查找: 全路径 → 含 '*' 的前缀段。
func lookupExpansion(prof *profile.Profile, rawPath string) ([]string, bool) {
	if v, ok := prof.PathConventions.WildcardExpansion[rawPath]; ok {
		return v, true
	}
	if i := strings.Index(rawPath, "*"); i > 0 {
		if v, ok := prof.PathConventions.WildcardExpansion[rawPath[:i+1]]; ok {
			return v, true
		}
	}
	return nil, false
}

func wildcardPrefixOf(raw string) string {
	if i := strings.Index(raw, "*"); i >= 0 {
		return raw[:i+1] // 含 '*' 本身
	}
	return raw
}

// buildOperationFacts 单个 operation 的全部事实构造。
func buildOperationFacts(g *codegraph.Graph, prof *profile.Profile,
	synth *typeschema.Synthesizer, slicer *slicing.Slicer,
	r adapter.Route, catalog map[string]slicing.ErrorCodeEntry) ([]*facts.Fact, error) {

	var fl []*facts.Fact
	opKey := facts.OpKey(r.Method, r.Path)
	ev := facts.Evidence{File: r.File, StartLine: r.Line, EndLine: r.Line,
		BlobSHA: g.FileHashOf(r.File)}

	// route 事实
	fl = append(fl, facts.NewFact(facts.KindRoute, opKey,
		facts.RoutePayload{
			Method: r.Method, Path: r.Path, Handler: r.Handler,
			Middleware: r.Middleware,
		}, ev))

	// handler 函数体分析（请求绑定 / 参数 / 神级中间件头）
	cp := &ContractPayloadBuilder{
		g: g, prof: prof, synth: synth, route: r, slicer: slicer,
		catalog: catalog, opKey: opKey, evFile: r.File, blob: g.FileHashOf(r.File),
	}
	contract, schemaFacts, extra := cp.Build()
	fl = append(fl, contract)
	fl = append(fl, schemaFacts...)
	fl = append(fl, extra...)

	// 语义增强: godoc（零成本 enrichment，设计文档 §5.8）
	if sym := g.Sym(r.Handler); sym != nil && sym.Doc != "" {
		fl = append(fl, &facts.Fact{
			ID: opKey + ":enrich", Kind: facts.KindEnrichment,
			Value:  facts.EnrichmentPayload{Summary: firstLine(sym.Doc)},
			Source: facts.SourceStatic, Confidence: 0.9,
			Evidence: []facts.Evidence{{File: sym.File, StartLine: sym.Line, EndLine: sym.Line,
				BlobSHA: g.FileHashOf(sym.File), Quote: firstLine(sym.Doc)}},
			Status: "verified",
		})
	}
	return fl, nil
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i > 0 {
		return s[:i]
	}
	return s
}

// collectGaps 聚合静态缺口清单：把 contract 事实里的 Gaps 提取成
// "METHOD /path: gap1; gap2" 的可读行，供档位判定与 report 消费。
func collectGaps(factList []*facts.Fact) []string {
	var out []string
	for _, f := range factList {
		if f.Kind != facts.KindContract {
			continue
		}
		cp, ok := f.Value.(facts.ContractPayload)
		if !ok {
			if cpp, ok2 := f.Value.(*facts.ContractPayload); ok2 {
				cp = *cpp
			} else {
				continue
			}
		}
		if len(cp.Gaps) == 0 {
			continue
		}
		method, path := opMethodPath(f.ID)
		out = append(out, method+" "+path+": "+strings.Join(cp.Gaps, "; "))
	}
	sort.Strings(out)
	return out
}

// opMethodPath 从 contract 事实 ID（"contract:op:METHOD:/path"）提取 method 与 path。
func opMethodPath(factID string) (string, string) {
	s := factID
	s = strings.TrimPrefix(s, "contract:")
	s = strings.TrimPrefix(s, "op:")
	if i := strings.Index(s, ":"); i > 0 {
		return s[:i], s[i+1:]
	}
	return "", s
}

// resolveGaps 对带缺口的 operation 做档位2/3 兜底采集（LLM 调用）。
//
// 只处理两类静态可判定的缺口：
//  1. err 变量错误码未解析 → LLM 从错误码目录里选码（catalog 命中校验通过才写回）；
//  2. any 响应不可定型 → LLM 从切片证据里读字段（字段名必须命中代码图白名单才写回）。
//
// 错误码写回是「原地修补 contract 事实的 Responses」；any 响应 schema 写回则新增一条
// KindSchema 事实（Value 为 *typeschema.Schema），由编译器经既有 refOf 管线自动 emit
// 进 components——编译器无需感知 LLM 来源，改动面最小。
// 返回 (实际发生的 LLM 调用次数, 新增的 schema 事实)。单个 operation 失败不阻断整体
// （降级保持 unknown）。
func resolveGaps(factList []*facts.Fact, g *codegraph.Graph, prof *profile.Profile,
	catalog map[string]slicing.ErrorCodeEntry, p infer.Provider, budget int) (int, []*facts.Fact) {

	calls := 0
	var newFacts []*facts.Fact
	successCode := successCodeOf(prof)
	for _, f := range factList {
		if f.Kind != facts.KindContract {
			continue
		}
		cp, ok := f.Value.(facts.ContractPayload)
		if !ok {
			continue
		}
		if len(cp.Gaps) == 0 {
			continue
		}
		if budget > 0 && calls >= budget {
			break // 预算耗尽：其余缺口留待下次，不阻塞全量
		}
		method, path := opMethodPath(f.ID)
		task := buildGapTask(method, path, cp.Gaps, g, prof, catalog, cp.ErrCandidates, cp.DataCandidates)

		// 档位3 优先：provider 支持工具时走多轮工具循环；否则退回档位2 模板化。
		var resolution *infer.GapResolution
		var err error
		if hasAnyGap(cp.Gaps, "any/interface{}") {
			if _, ok := p.(infer.ToolUser); ok {
				resolution, err = infer.ResolveGapsWithTools(p, task, buildTools(g), 12)
			}
		}
		if resolution == nil {
			resolution, err = infer.ResolveGaps(p, task)
		}
		if err != nil {
			// 单点失败降级：保持 unknown，不阻断。
			continue
		}
		calls++
		if scFact := applyGapResolution(&cp, resolution, catalog, g, successCode); scFact != nil {
			newFacts = append(newFacts, scFact)
		}
		f.Value = cp
	}
	return calls, newFacts
}

// successCodeOf 取画像信封的成功码（无画像默认 0）。
func successCodeOf(prof *profile.Profile) int {
	if prof != nil && prof.ResponseEnvelope != nil {
		return prof.ResponseEnvelope.SuccessCode
	}
	return 0
}

// hasAnyGap 判断缺口清单里是否含指定子串。
func hasAnyGap(gaps []string, substr string) bool {
	for _, g := range gaps {
		if strings.Contains(g, substr) {
			return true
		}
	}
	return false
}

// buildTools 构造档位3 子 Agent 的工具集：对代码图的只读查询闭包。
// 每个工具返回文本结果（JSON 序列化），genkit 负责 schema 引导。
func buildTools(g *codegraph.Graph) []infer.ToolSpec {
	return []infer.ToolSpec{
		{
			Name:        "read_symbol",
			Description: "Read a symbol's source location and doc. Input: {\"id\": \"<full symbol id>\"}.",
			Call: func(input map[string]any) (string, error) {
				id, _ := input["id"].(string)
				if id == "" {
					return `{"error":"id required"}`, nil
				}
				sym := g.Sym(id)
				if sym == nil {
					return `{"error":"symbol not found"}`, nil
				}
				b, _ := json.Marshal(map[string]any{
					"id":   sym.ID,
					"kind": sym.Kind,
					"file": sym.File,
					"line": sym.Line,
					"doc":  sym.Doc,
				})
				return string(b), nil
			},
		},
		{
			Name:        "callees",
			Description: "List a function's direct callees. Input: {\"id\": \"<symbol id>\"}.",
			Call: func(input map[string]any) (string, error) {
				id, _ := input["id"].(string)
				if id == "" {
					return `{"error":"id required"}`, nil
				}
				callees := g.CalleesOf(id)
				b, _ := json.Marshal(map[string]any{"callees": callees})
				return string(b), nil
			},
		},
		{
			Name:        "read_type",
			Description: "Read a named type's fields. Input: {\"id\": \"<type id>\"}.",
			Call: func(input map[string]any) (string, error) {
				id, _ := input["id"].(string)
				if id == "" {
					return `{"error":"id required"}`, nil
				}
				ti := g.Type(id)
				if ti == nil {
					return `{"error":"type not found"}`, nil
				}
				var fields []map[string]any
				for _, fld := range ti.Fields {
					fields = append(fields, map[string]any{
						"name":     fld.Name,
						"json":     fld.JSONName,
						"type":     fld.TypeStr,
						"required": fld.Required,
					})
				}
				b, _ := json.Marshal(map[string]any{
					"id": ti.ID, "isStruct": ti.IsStruct, "fields": fields,
				})
				return string(b), nil
			},
		},
	}
}

// buildGapTask 组装一次兜底采集的输入（证据切片 + 错误码目录 + 切片候选）。
//
// errCandidates / dataCandidates 是证据注入的关键：err 变量兜底时切片内已出现的
// 具体错误码、any 响应兜底时切片内已出现的字段名，分别作为 LLM 候选目录，它据此
// 缩小选码/选字段范围（而非在全量目录里空猜、或从空证据里编字段）。
func buildGapTask(method, path string, gaps []string, g *codegraph.Graph,
	prof *profile.Profile, catalog map[string]slicing.ErrorCodeEntry,
	errCandidates []facts.ErrCandidateFact, dataCandidates []facts.DataCandidateFact) infer.GapTask {

	task := infer.GapTask{
		Method:   method,
		Path:     path,
		Gaps:     gaps,
		Evidence: evidenceSummary(g, prof, method, path),
	}
	for id, e := range catalog {
		task.ErrorCatalog = append(task.ErrorCatalog, infer.ErrorCodeItem{
			Symbol: id, Code: e.Code, Msg: e.Msg,
		})
	}
	for _, c := range errCandidates {
		task.ErrCandidates = append(task.ErrCandidates, infer.ErrCandidateItem{
			Symbol: c.Symbol, Name: c.Name, Code: c.Code,
		})
	}
	for _, c := range dataCandidates {
		task.DataCandidates = append(task.DataCandidates, infer.DataCandidateItem{Name: c.Name})
	}
	return task
}

// evidenceSummary 组装给 LLM 的证据摘要（handler 源码 + 错误码目录行）。
// 目前简化实现：错误码目录本身就是最强证据；handler 源码留待档位3 工具化补全。
func evidenceSummary(g *codegraph.Graph, prof *profile.Profile, method, path string) string {
	var b strings.Builder
	b.WriteString("route: " + method + " " + path + "\n")
	b.WriteString("error codes are enumerated in the errorCatalog field; ")
	b.WriteString("pick codeRef symbols only from there.\n")
	return b.String()
}

// applyGapResolution 把 LLM 产出写回 contract 的 Responses，并做防线校验。
//
// 防幻觉硬约束（设计文档 §8.3）：
//   - errorCodes 的 codeRef 必须命中错误码目录，否则丢弃该条；
//   - 补出的错误行只替换「未解析行」（Envelope.Code == -1），不碰静态已定的行；
//   - responseSchema 的每个字段名必须命中代码图里的命名类型字段（Type 白名单校验），
//     否则整条 responseSchema 丢弃（禁止编造结构）。
//
// 返回值为补出的 any 响应 schema 事实（*facts.Fact，KindSchema）；未补出时返回 nil。
func applyGapResolution(cp *facts.ContractPayload, r *infer.GapResolution,
	catalog map[string]slicing.ErrorCodeEntry, g *codegraph.Graph, successCode int) *facts.Fact {

	if r == nil {
		return nil
	}
	// 建立 catalog 的 codeRef → entry 快速反查（catalog 的 key 即全限定符号）。
	bySymbol := map[string]slicing.ErrorCodeEntry{}
	for sym, e := range catalog {
		bySymbol[sym] = e
		bySymbol[shortSym(sym)] = e
	}
	// 未解析行集合：Envelope.Code == -1。
	var unresolvedIdx []int
	for i, row := range cp.Responses {
		if row.Envelope != nil && row.Envelope.Code == -1 {
			unresolvedIdx = append(unresolvedIdx, i)
		}
	}
	if len(unresolvedIdx) > 0 && len(r.ErrorCodes) > 0 {
		// 用 LLM 补出的错误码替换未解析行（逐个匹配；多出/缺省保守保留原样）。
		used := map[int]bool{}
		for _, ec := range r.ErrorCodes {
			entry, ok := bySymbol[ec.CodeRef]
			if !ok {
				entry, ok = bySymbol[shortSym(ec.CodeRef)]
			}
			if !ok {
				continue // 幻觉 codeRef：丢弃
			}
			// 找到下一个未替换的未解析行。
			for _, idx := range unresolvedIdx {
				if used[idx] {
					continue
				}
				cp.Responses[idx].Envelope = &facts.Envelope{
					Code:    entry.Code,
					CodeRef: ec.CodeRef,
					Msg:     entry.Msg,
				}
				cp.Responses[idx].Source = "llm"
				used[idx] = true
				break
			}
		}
	}

	// any 响应 schema 写回：LLM 只读证据报字段名，命中代码图命名类型字段白名单
	// 才采纳；字段类型从代码图反查（而非信任 LLM 的 type 字符串，防类型漂移）。
	return applyResponseSchema(cp, r.ResponseSchema, g, successCode)
}

// shortSym 取符号 ID 的末段（pkg.ConstName → ConstName）。
func shortSym(s string) string {
	if i := strings.LastIndex(s, "."); i >= 0 {
		return s[i+1:]
	}
	return s
}

// applyResponseSchema 把 LLM 补出的 any 响应结构写回：新增一条 KindSchema 事实，
// 并让成功响应行指向该 schema，交由编译器既有 refOf 管线 emit 进 components。
//
// 防幻觉硬约束：
//   - 字段名必须命中 cp.DataCandidates（切片内已出现的字段候选，证据注入），
//     任一字段不在候选内则整条丢弃，保持 unknown——禁止编造结构；
//   - 字段类型优先从代码图命名类型字段白名单反查；反查不到（map[string]any 键值
//     本就不可静态定型）时才降级用 LLM 报的 type，但限定在封闭类型集内。
//
// 返回新 schema 事实，丢弃时返回 nil。
func applyResponseSchema(cp *facts.ContractPayload, rs *infer.ResolvedSchema,
	g *codegraph.Graph, successCode int) *facts.Fact {

	if rs == nil || len(rs.Properties) == 0 {
		return nil
	}
	// 定位 any 响应未定型的成功行（Envelope.Code == successCode 且 SchemaType 为空）。
	target := -1
	for i, row := range cp.Responses {
		if row.Envelope != nil && row.Envelope.Code == successCode && row.SchemaType == "" {
			target = i
			break
		}
	}
	if target < 0 {
		return nil
	}
	// 字段候选白名单（证据注入）：字段名必须来自切片内实际出现的键名。
	candSet := map[string]bool{}
	for _, c := range cp.DataCandidates {
		candSet[c.Name] = true
	}
	if len(candSet) == 0 {
		return nil // 无字段候选证据：拒绝编造
	}
	// 命名类型字段白名单：field JSON 名 → 代码图字段类型串（更强类型证据）。
	namedTypes := fieldWhitelist(g)
	var props []typeschema.Prop
	var required []string
	for _, p := range rs.Properties {
		if !candSet[p.Name] {
			return nil // 字段名不在切片候选内：幻觉，整条丢弃
		}
		typ := namedTypes[p.Name]
		if typ == "" {
			typ = sanitizeType(p.Type)
		}
		props = append(props, typeschema.Prop{
			Name: p.Name, Schema: &typeschema.Schema{Type: typ}, Required: p.Required,
		})
		if p.Required {
			required = append(required, p.Name)
		}
	}
	sort.Strings(required)
	// 合成 object schema：字段序按 LLM 报出顺序（确定性由 LLM 输入顺序保证）。
	synth := &typeschema.Schema{Type: "object", Props: props, Required: required}
	schemaID := "llm:any:" + cp.OperationID
	cp.Responses[target].SchemaType = schemaID
	cp.Responses[target].Source = "llm"
	return &facts.Fact{
		ID: "schema:" + schemaID, Kind: facts.KindSchema, Value: synth,
		Source: facts.SourceLLM, Confidence: 0.8,
		Evidence: []facts.Evidence{{File: "llm:response-schema", Quote: schemaID}},
		Status:   "verified",
	}
}

// sanitizeType 把 LLM 报的 type 字符串限定在封闭类型集内（防类型漂移）。
// 合法值映射为 OpenAPI 类型；非法值保守回退 "string"（字段名已过候选校验，
// 类型仅在此集合内取值，杜绝任意类型注入）。
func sanitizeType(t string) string {
	switch strings.TrimSpace(t) {
	case "integer", "number", "boolean", "object", "array", "string":
		return strings.TrimSpace(t)
	}
	return "string"
}

// fieldWhitelist 建命名类型字段白名单：字段 JSON 名 → 底层类型名。
//
// 跨全仓命名类型聚合：LLM 补出的响应字段名只有命中该白名单才被采纳，
// 类型从代码图反查（而非信任 LLM 的 type 字符串），防止结构幻觉与类型漂移。
func fieldWhitelist(g *codegraph.Graph) map[string]string {
	wl := map[string]string{}
	for _, ti := range g.Types {
		if !ti.IsStruct {
			continue
		}
		for _, f := range ti.Fields {
			if f.JSONName == "-" {
				continue
			}
			typ := basicKindStrOf(f.TypeStr)
			if typ == "" {
				typ = "string"
			}
			wl[f.JSONName] = typ
		}
	}
	return wl
}

// basicKindStrOf 提取类型串的底层 JSON schema 类型名（含切片/映射降级）。
func basicKindStrOf(typeStr string) string {
	s := strings.TrimPrefix(typeStr, "*")
	if strings.HasPrefix(s, "[]") {
		return "array"
	}
	if strings.HasPrefix(s, "map[") {
		return "object"
	}
	// 命名类型截取末段（如 mapping.T → T，仍映射到 string 兜底）。
	if i := strings.LastIndex(s, "."); i >= 0 {
		s = s[i+1:]
	}
	switch strings.TrimSpace(s) {
	case "string":
		return "string"
	case "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64", "uintptr":
		return "integer"
	case "float32", "float64":
		return "number"
	case "bool":
		return "boolean"
	case "byte", "time.Time":
		return "string"
	}
	return ""
}

// renderReport 置信度报告（F11）。
func renderReport(doc *compiler.Document, res *Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# SpecForge 置信度报告\n\n")
	fmt.Fprintf(&b, "- 生成时间: %s\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(&b, "- operations: %d, schema types: %d\n", len(doc.Operations), len(doc.Schemas))
	fmt.Fprintf(&b, "- 低置信项 (<0.8): %d\n", res.LowConf)
	if len(res.Gaps) > 0 {
		fmt.Fprintf(&b, "- 静态缺口 (待 LLM 兜底): %d\n", len(res.Gaps))
	}
	if res.LLMCalls > 0 {
		fmt.Fprintf(&b, "- LLM 兜底调用: %d 次\n", res.LLMCalls)
	}
	fmt.Fprintf(&b, "\n")

	var low []*compiler.Operation
	for i := range doc.Operations {
		if doc.Operations[i].Confidence < 0.8 {
			low = append(low, &doc.Operations[i])
		}
	}
	sort.Slice(low, func(i, j int) bool { return low[i].Confidence < low[j].Confidence })
	if len(low) == 0 {
		b.WriteString("全部 operation 置信度 ≥ 0.8。\n")
	}
	for _, op := range low {
		fmt.Fprintf(&b, "## %s %s (confidence %.2f)\n\n", op.Method, op.Path, op.Confidence)
		for _, u := range op.Unknowns {
			fmt.Fprintf(&b, "- 未解析: %s\n", u)
		}
		for _, e := range op.Evidence {
			fmt.Fprintf(&b, "- 证据: %s\n", e)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// WriteJSONSummary --json 输出。
func WriteJSONSummary(r *Result, w *os.File) error {
	// 简明 JSON 摘要（agent 友好，设计文档 §3.2 接入层）
	var b strings.Builder
	b.WriteString("{")
	fmt.Fprintf(&b, `"service":%q,`, r.Services)
	fmt.Fprintf(&b, `"operations":%d,`, r.Operations)
	fmt.Fprintf(&b, `"routes":%d,`, r.Routes)
	fmt.Fprintf(&b, `"routes_unresolved":%d,`, r.RoutesUnresolved)
	fmt.Fprintf(&b, `"schema_types":%d,`, r.SchemaTypes)
	fmt.Fprintf(&b, `"facts":%d,`, r.Facts)
	fmt.Fprintf(&b, `"low_confidence":%d,`, r.LowConf)
	fmt.Fprintf(&b, `"gaps":%d,`, len(r.Gaps))
	fmt.Fprintf(&b, `"llm_calls":%d,`, r.LLMCalls)
	fmt.Fprintf(&b, `"spec":%q`, r.OutDir+"/openapi.yaml")
	b.WriteString("}\n")
	_, err := w.WriteString(b.String())
	return err
}
