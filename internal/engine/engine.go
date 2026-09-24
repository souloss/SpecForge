// Package engine 编排生成流水线（设计文档 §6.1 首次全量 12 步的 P0 实现）。
//
// 流程: 摄入 → 服务拓扑 → 框架识别 → 画像 → 路由抽取 →
// handler 分析（请求绑定/参数/中间件） → 响应追踪（切片+汇聚点） →
// Schema 合成 → 横切合成（security/错误码） → 语义增强（godoc） →
// 确定性编译 → 校验输出。
package engine

import (
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
	"github.com/specforge/specforge/internal/loader"
	"github.com/specforge/specforge/internal/profile"
	"github.com/specforge/specforge/internal/slicing"
	"github.com/specforge/specforge/internal/typeschema"

	"golang.org/x/tools/go/packages"
)

// Config 生成配置。
type Config struct {
	RepoDir     string
	Service     string
	ProfilePath string
	OutDir      string
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
}

// Run 执行完整生成。
func Run(cfg Config) (*Result, error) {
	// 0. 仓库摄入
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

// renderReport 置信度报告（F11）。
func renderReport(doc *compiler.Document, res *Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# SpecForge 置信度报告\n\n")
	fmt.Fprintf(&b, "- 生成时间: %s\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(&b, "- operations: %d, schema types: %d\n", len(doc.Operations), len(doc.Schemas))
	fmt.Fprintf(&b, "- 低置信项 (<0.8): %d\n\n", res.LowConf)

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
	fmt.Fprintf(&b, `"spec":%q`, r.OutDir+"/openapi.yaml")
	b.WriteString("}\n")
	_, err := w.WriteString(b.String())
	return err
}
