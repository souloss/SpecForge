package golang

import (
	"context"
	"fmt"
	"go/ast"
	"go/types"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/specforge/specforge/internal/adapter"
	"github.com/specforge/specforge/internal/codegraph"
	"github.com/specforge/specforge/internal/facts"
	"github.com/specforge/specforge/internal/frontend"
	"github.com/specforge/specforge/internal/loader"
	"github.com/specforge/specforge/internal/profile"
	"github.com/specforge/specforge/internal/slicing"
	"github.com/specforge/specforge/internal/typeschema"

	"golang.org/x/tools/go/packages"
)

// Frontend Go 语言前端：go/packages 类型加载 → 代码图 → 框架适配器抽路由 →
// 调用图切片 + 值流反推响应/错误码 → 类型合成 schema → 事实。
type Frontend struct{}

// New 构造 Go 前端。
func New() *Frontend { return &Frontend{} }

// Name 实现 frontend.Frontend。
func (*Frontend) Name() string { return "go" }

// goModFile Go 模块根的标志文件。
const goModFile = "go.mod"

// Detect 实现 frontend.Frontend：仓库根有 go.mod 即为 Go 项目。
func (*Frontend) Detect(repoDir string) bool {
	_, err := os.Stat(filepath.Join(repoDir, goModFile))
	return err == nil
}

// IsSource 实现 frontend.Frontend：.go 源文件与模块依赖清单。
func (*Frontend) IsSource(name string) bool {
	return strings.HasSuffix(name, ".go") || name == goModFile || name == "go.sum"
}

// Analyze 实现 frontend.Frontend。
func (*Frontend) Analyze(ctx context.Context, req frontend.Request) (*frontend.Analysis, error) {
	end := req.Stage("load")
	l, err := loader.LoadRepoContext(ctx, req.RepoDir)
	end()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %v", frontend.ErrConfig, err)
	}
	if !l.HasService(req.Service) {
		if len(l.Errors) > 0 {
			return nil, fmt.Errorf("%w: service %q was not found after package loading errors: %s", frontend.ErrConfig, req.Service, strings.Join(l.Errors, "; "))
		}
		return nil, fmt.Errorf("%w: service %q was not found", frontend.ErrConfig, req.Service)
	}
	an := &frontend.Analysis{Service: serviceTitle(req.Service, l), Handlers: map[string]string{}}
	learned, err := adapter.LoadSpecs(filepath.Join(req.RepoDir, adaptersDir))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", frontend.ErrConfig, err)
	}
	var extra []adapter.Framework
	for _, sp := range learned {
		extra = append(extra, sp.Framework())
	}
	fws := detectFrameworks(l, extra)
	pkgs := l.ServiceFilter(req.Service)
	prof, err := loadProfile(req.RepoDir, req.ProfilePath)
	if err != nil {
		return nil, fmt.Errorf("%w: profile: %v", frontend.ErrConfig, err)
	}

	end = req.Stage("codegraph")
	g, err := codegraph.Build(pkgs, l.Fset)
	end()
	if err != nil {
		return nil, err
	}
	g.SetRoot(l.Root)
	prog := NewProgram(g)
	an.Program = prog
	an.Stats.Packages, an.Stats.Symbols = len(pkgs), len(g.Syms)
	for _, cs := range g.Callers {
		an.Stats.CallEdges += len(cs)
	}

	// L3：路由抽不到（未登记框架）时让 LLM 写适配器声明，复核 + 试运行通过后加入并落盘。
	if req.Provider != nil && countResolved(extractAllRoutes(g, fws)) == 0 {
		end = req.Stage("llm-adapter")
		spec, st := learnAdapter(ctx, req, l, g)
		an.Adapter = st
		if spec != nil {
			fws = append(fws, spec.Framework())
		}
		end()
	}
	an.Framework = frameworkNames(fws)
	prims := mergedPrimitives(fws)

	end = req.Stage("routes")
	routes := extractAllRoutes(g, fws)
	an.Stats.Routes = len(routes)
	routes = expandWildcards(routes, prof)
	an.Stats.RoutesUnique, an.Stats.RoutesCollapsed = routeIdentityStats(routes)
	an.UnresolvedRoutes = unresolvedRouteIssues(routes, prog.RelPath)
	var resolved []adapter.Route
	for _, r := range routes {
		if r.Unresolved == "" {
			an.Stats.RoutesResolved++
			resolved = append(resolved, r)
		} else {
			an.Stats.RoutesUnresolved++
		}
	}
	end()

	if req.LearnProfile && req.Provider != nil {
		end = req.Stage("llm-learn-profile")
		prof = learnProfileInto(ctx, prog, prof, req.Provider, resolved, req.Log)
		end()
	}
	// 响应汇聚点自动发现（原生写出器 + 包装器函数摘要），手写 profile 同名 sink 优先。
	sinks, envs := slicing.DiscoverSinks(g, prims.Writers)
	prof = withDiscoveredSinks(prof, sinks)

	end = req.Stage("analyze")
	catalog := slicing.New(g, prof, prims.Writers).BuildErrorCatalog()
	run := opRun{g: g, prof: prof, prims: prims, fws: fws, envs: envs, catalog: catalog, resolved: resolved, log: req.Log}
	run.analyze(an)
	end()

	// L2：静态没识别出的响应包装器交给 LLM 摘要，复核通过后作为包装器模式重跑契约分析。
	if req.Provider != nil {
		if wrappers := unsummarizedWrappers(an.Facts); len(wrappers) > 0 {
			end = req.Stage("llm-wrappers")
			pats, learnedEnvs, st := summarizeWrappers(ctx, prog, req, wrappers, defaultWriterStatus(prims))
			an.Wrappers = st
			if len(pats) > 0 {
				run.prof = withDiscoveredSinks(run.prof, pats)
				for id, e := range learnedEnvs {
					run.envs[id] = e
				}
				run.analyze(an)
			}
			end()
		}
	}
	an.Facts = append(an.Facts, unresolvedRouteFacts(routes)...)
	an.Catalog = make(map[string]frontend.ErrorCode, len(catalog))
	for id, e := range catalog {
		an.Catalog[id] = frontend.ErrorCode{Symbol: id, Code: e.Code, Msg: e.Msg}
	}
	an.SuccessCode = successCodeOf(prof)
	return an, nil
}

func routeIdentityStats(routes []adapter.Route) (unique, collapsed int) {
	seen := make(map[string]bool, len(routes))
	for _, route := range routes {
		if route.Unresolved != "" && route.Unresolved != adapter.ReasonNoHandler {
			continue
		}
		if route.Method == "" || route.Path == "" {
			continue
		}
		key := facts.OpKey(route.Method, route.Path)
		if seen[key] {
			collapsed++
			continue
		}
		seen[key] = true
		unique++
	}
	return unique, collapsed
}

func unresolvedRouteIssues(routes []adapter.Route, relativePath func(string) string) []frontend.RouteIssue {
	var issues []frontend.RouteIssue
	for _, route := range routes {
		if route.Unresolved == "" {
			continue
		}
		file := route.File
		if relativePath != nil && file != "" {
			file = relativePath(file)
		}
		issues = append(issues, frontend.RouteIssue{
			Method: route.Method, Path: route.Path, RawPath: route.RawPath, Handler: route.Handler,
			File: file, Line: route.Line, Reason: route.Unresolved,
		})
	}
	return issues
}

func unresolvedRouteFacts(routes []adapter.Route) []*facts.Fact {
	var generated []*facts.Fact
	for _, route := range routes {
		if route.Unresolved != adapter.ReasonNoHandler || route.Method == "" || route.Path == "" {
			continue
		}
		generated = append(generated, &facts.Fact{
			ID:   "contract:" + facts.OpKey(route.Method, route.Path),
			Kind: facts.KindContract,
			Value: facts.ContractPayload{
				Responses: []facts.ResponseFact{{
					Status: 200, Description: "Response contract unresolved because the route handler is not statically resolvable.", Raw: true,
				}},
				Gaps: []string{"route handler is not statically resolvable: " + route.Unresolved},
			},
			Source: facts.SourceStatic, Confidence: 0.35, Status: "unresolved",
			Evidence: []facts.Evidence{{
				File: route.File, StartLine: route.Line, EndLine: route.Line,
				Quote: "route registration with unresolved handler: " + route.RawPath,
			}},
		})
	}
	return generated
}

// opRun 一轮逐 operation 契约分析的输入（L2 采纳新包装器后以同一输入重跑）。
type opRun struct {
	g        *codegraph.Graph                     // 代码图
	prof     *profile.Profile                     // 画像（含自动发现/LLM 摘要的汇聚点）
	prims    adapter.Primitives                   // 框架原语
	fws      []adapter.Framework                  // 识别出的框架
	envs     map[string]slicing.SyntheticEnvelope // 合成信封
	catalog  map[string]slicing.ErrorCodeEntry    // 错误码目录
	resolved []adapter.Route                      // 已解析路由
	log      *slog.Logger                         // 日志
}

// analyze 逐 operation 构造事实（单个 operation 失败隔离），结果写入 an（覆盖上一轮）。
func (r *opRun) analyze(an *frontend.Analysis) {
	slicer := slicing.New(r.g, r.prof, r.prims.Writers)
	synth := typeschema.New(r.g)
	reqSynth := typeschema.NewRequest(r.g)
	binders := slicing.DiscoverBinders(r.g, r.prims.BodyBinders)
	primIx := newPrimIndex(r.fws)
	an.Facts, an.OpFailures, an.Stats.SinkSites = nil, nil, 0
	an.Handlers = map[string]string{}
	for _, rt := range r.resolved {
		fl, ferr := safeBuildOperationFacts(r.g, r.prof, synth, reqSynth, slicer, rt, r.catalog, binders, primIx, r.envs)
		if ferr != nil {
			an.OpFailures = append(an.OpFailures, frontend.OpFailure{Op: rt.Method + " " + rt.Path, Error: ferr.Error()})
			r.log.Warn("operation 分析失败，已跳过", "op", rt.Method+" "+rt.Path, "err", ferr)
			continue
		}
		an.Handlers[facts.OpKey(rt.Method, rt.Path)] = rt.Handler
		an.Facts = append(an.Facts, fl...)
		an.Stats.SinkSites += len(fl)
	}
}

// unsummarizedWrappers 全部 operation 报告的未识别包装器（去重、有序）。
func unsummarizedWrappers(fl []*facts.Fact) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range fl {
		if cp, ok := f.Contract(); ok {
			for _, w := range cp.UnsummarizedWrappers {
				if !seen[w] {
					seen[w] = true
					out = append(out, w)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// defaultWriterStatus 原语表写出器的默认 HTTP 状态（包装器摘要的写出状态口径）。
func defaultWriterStatus(p adapter.Primitives) int {
	for _, w := range p.Writers {
		return w.DefaultStatus
	}
	return fallbackWriterStatus
}

// fallbackWriterStatus 原语表无写出器时的默认 HTTP 状态。
const fallbackWriterStatus = 200

// successCodeOf 画像信封的成功码（无信封约定时为 0）。
func successCodeOf(prof *profile.Profile) int {
	if prof != nil && prof.ResponseEnvelope != nil {
		return prof.ResponseEnvelope.SuccessCode
	}
	return 0
}

// staticEnrichConfidence godoc 摘要（静态语义增强）的置信度：注释可能过时，略低于结构事实。
const staticEnrichConfidence = 0.9

// serviceTitle 产物标题用的服务名：显式 --service 取其名；全仓模式取模块路径（而非首个服务，避免误导）。
func serviceTitle(service string, l *loader.Loaded) string {
	if service != "" {
		return service
	}
	if l.Module != "" {
		return l.Module
	}
	return filepath.Base(l.Root)
}

// safeBuildOperationFacts 隔离单个 operation 分析中的 panic（真实仓库的罕见语法形态不应拖垮全量）。
func safeBuildOperationFacts(g *codegraph.Graph, prof *profile.Profile,
	synth, reqSynth *typeschema.Synthesizer, slicer *slicing.Slicer,
	r adapter.Route, catalog map[string]slicing.ErrorCodeEntry, binders map[string]slicing.Binder, prims *primIndex,
	envs map[string]slicing.SyntheticEnvelope) (fl []*facts.Fact, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return buildOperationFacts(g, prof, synth, reqSynth, slicer, r, catalog, binders, prims, envs)
}

// detectFrameworks 依据仓库包的 import 识别使用的框架（F2）。
func detectFrameworks(l *loader.Loaded, extra []adapter.Framework) []adapter.Framework {
	seen := map[string]bool{}
	var imports []string
	for _, p := range l.Pkgs {
		for path := range p.Imports {
			if !seen[path] {
				seen[path] = true
				imports = append(imports, path)
			}
		}
	}
	sort.Strings(imports)
	return adapter.DetectWith(imports, extra)
}

// frameworkNames 框架名（逗号分隔）；未识别为 "unknown"。
func frameworkNames(fws []adapter.Framework) string {
	if len(fws) == 0 {
		return "unknown"
	}
	names := make([]string, len(fws))
	for i, fw := range fws {
		names[i] = fw.Name()
	}
	return strings.Join(names, ",")
}

// mergedPrimitives 多框架原语表合并。
func mergedPrimitives(fws []adapter.Framework) adapter.Primitives {
	var p adapter.Primitives
	for _, fw := range fws {
		p = p.Merge(fw.Primitives())
	}
	return p
}

// loadProfile 加载约定画像：显式路径 > 仓库内 .specforge/profile.yaml > 内置默认。
func loadProfile(repoDir, profilePath string) (*profile.Profile, error) {
	if profilePath != "" {
		return profile.Load(profilePath)
	}
	// 默认查找仓库内 .specforge/profile.yaml
	cand := filepath.Join(repoDir, ".specforge", "profile.yaml")
	if _, err := os.Stat(cand); err == nil {
		return profile.Load(cand)
	}
	return profile.Default(), nil
}

// extractAllRoutes 全仓路由抽取：每个包交给每个识别出的框架适配器。
func extractAllRoutes(g *codegraph.Graph, fws []adapter.Framework) []adapter.Route {
	var routes []adapter.Route
	for _, p := range g.Pkgs() {
		ctx := adapter.NewPkgCtx(p.PkgPath, p.Syntax, g, exprResolver(p), argTypesOf(p), recvTypeOf(p))
		for _, fw := range fws {
			routes = append(routes, fw.ExtractRoutes(ctx)...)
		}
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
	resolvedKeys := map[string]bool{} // "METHOD path"：展开冲突检测
	for _, r := range routes {
		if r.Unresolved == "" {
			resolvedRaw[r.RawPath] = true
			resolvedKeys[r.Method+" "+r.Path] = true
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
		// 2. 自动推断（仅单版本确定、且展开结果不与已有具体路由冲突时）
		if bases, ok := inferWildcardBases(r.RawPath, resolvedRaw); ok && !collides(r, bases, resolvedKeys) {
			for _, base := range bases {
				out = append(out, expandTo(r, base))
			}
			continue
		}
		// 3. 模板化：通配符段转为路径参数（/ipo/v* → /ipo/v{version}），忠实表达「任意版本」。
		if tr, ok := templateWildcard(r); ok {
			out = append(out, tr)
			continue
		}
		// 4. 保留未解析
		out = append(out, r)
	}
	return out
}

// collides 通配符按 bases 展开后是否与已有具体路由（同 method + path）重名。
// 重名说明通配符是「其它版本」的兜底组（如先注册 /ipo/v3 再注册 /ipo/v*），展开到该版本是错的。
func collides(r adapter.Route, bases []string, resolvedKeys map[string]bool) bool {
	for _, base := range bases {
		if resolvedKeys[r.Method+" "+expandTo(r, base).Path] {
			return true
		}
	}
	return false
}

// wildcardVersionParam 版本段通配符（`v*`）模板化后的路径参数名。
const wildcardVersionParam = "version"

// wildcardGenericParam 其它通配符模板化后的路径参数名。
const wildcardGenericParam = "wildcard"

// versionSegPrefix 版本段通配符的段前缀（`/ipo/v*` 中 `*` 前的 `v`）。
const versionSegPrefix = "v"

// templateWildcard 把首个 `*` 转为 OpenAPI 路径参数；`*` 前同段为 `v` 时命名 version，否则 wildcard。
// 已占用的参数名不重复使用（返回 false 保留未解析）。
func templateWildcard(r adapter.Route) (adapter.Route, bool) {
	star := strings.Index(r.RawPath, "*")
	if star < 0 {
		return r, false
	}
	segStart := strings.LastIndex(r.RawPath[:star], "/") + 1
	name := wildcardGenericParam
	if r.RawPath[segStart:star] == versionSegPrefix {
		name = wildcardVersionParam
	}
	if strings.Contains(r.RawPath, ":"+name) {
		return r, false
	}
	tmpl := r.RawPath[:star] + "{" + name + "}" + r.RawPath[star+1:]
	if strings.Contains(tmpl, "*") {
		return r, false // 多个通配符：不做多参数模板化，保守保留未解析
	}
	norm, _ := adapter.NormalizePath(tmpl)
	nr := r
	nr.Path = norm
	nr.Unresolved = ""
	nr.WildcardParam = name
	return nr, true
}

// expandTo 把通配符路由按具体 base 前缀展开（base 如 "/ipo/v1"）。
func expandTo(r adapter.Route, base string) adapter.Route {
	suffix := strings.TrimPrefix(r.RawPath, wildcardPrefixOf(r.RawPath))
	normSuffix, _ := adapter.NormalizePath(suffix)
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
	synth, reqSynth *typeschema.Synthesizer, slicer *slicing.Slicer,
	r adapter.Route, catalog map[string]slicing.ErrorCodeEntry, binders map[string]slicing.Binder, prims *primIndex,
	envs map[string]slicing.SyntheticEnvelope) ([]*facts.Fact, error) {

	var fl []*facts.Fact
	opKey := facts.OpKey(r.Method, r.Path)
	ev := facts.Evidence{File: r.File, StartLine: r.Line, EndLine: r.Line,
		BlobSHA: g.FileHashOf(r.File)}

	// route 事实
	fl = append(fl, facts.NewFact(opKey,
		facts.RoutePayload{
			Method: r.Method, Path: r.Path, Handler: r.Handler,
			Middleware: r.Middleware,
		}, ev))

	// handler 函数体分析（请求绑定 / 参数 / 神级中间件头）
	cp := &ContractPayloadBuilder{
		g: g, prof: prof, synth: synth, reqSynth: reqSynth, route: r, slicer: slicer,
		catalog: catalog, binders: binders, prims: prims, envs: envs, opKey: opKey, evFile: r.File, blob: g.FileHashOf(r.File),
	}
	contract, schemaFacts, extra := cp.Build()
	ensurePathParams(contract, r)
	fl = append(fl, contract)
	fl = append(fl, schemaFacts...)
	fl = append(fl, extra...)

	// 语义增强: godoc（零成本 enrichment，设计文档 §5.8）。
	// handler 常经接口分发到具体实现，接口方法无函数体也无 doc，
	// 需解析到具体实现再取其 doc 作为 summary 来源。
	var enrichSym *codegraph.Symbol
	if sym := g.Sym(r.Handler); sym != nil && sym.Doc != "" {
		enrichSym = sym
	} else {
		for _, impl := range g.ConcreteImplsOf(r.Handler) {
			if isym := g.Sym(impl); isym != nil && isym.Doc != "" {
				enrichSym = isym
				break
			}
		}
	}
	if enrichSym != nil {
		fl = append(fl, &facts.Fact{
			ID: opKey + ":enrich", Kind: facts.KindEnrichment,
			Value:  facts.EnrichmentPayload{Summary: firstLine(enrichSym.Doc)},
			Source: facts.SourceStatic, Confidence: staticEnrichConfidence,
			Evidence: []facts.Evidence{{File: enrichSym.File, StartLine: enrichSym.Line, EndLine: enrichSym.Line,
				BlobSHA: g.FileHashOf(enrichSym.File), Quote: firstLine(enrichSym.Doc)}},
			Status: "verified",
		})
	}
	return fl, nil
}

// ensurePathParams 路径模板里的每个 `{name}` 都必须有对应的 path 参数（OpenAPI 硬约束）：
// handler 未经 c.Params 读取的段（或通配符模板化产生的段）按路由模板补齐。
func ensurePathParams(contract *facts.Fact, r adapter.Route) {
	cp, ok := contract.Contract()
	if !ok {
		return
	}
	for _, name := range pathTemplateParams(r.Path) {
		if paramExists(cp.Params, "path", name) {
			continue
		}
		pf := facts.ParamFact{In: "path", Name: name, Required: true, Type: "string", Origin: "route-template"}
		if name == r.WildcardParam {
			pf.Origin = "route-wildcard"
			pf.Description = "路由通配符段，原始注册路径 " + r.RawPath
		}
		cp.Params = append(cp.Params, pf)
	}
	contract.Value = cp
}

// pathTemplateParams 路径模板中的参数名（按出现顺序）。
func pathTemplateParams(path string) []string {
	var out []string
	for {
		i := strings.IndexByte(path, '{')
		if i < 0 {
			return out
		}
		j := strings.IndexByte(path[i:], '}')
		if j < 0 {
			return out
		}
		out = append(out, path[i+1:i+j])
		path = path[i+j+1:]
	}
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i > 0 {
		return s[:i]
	}
	return s
}

// withDiscoveredSinks 把自动发现的 sink 并入画像（返回副本，不改调用方持有的画像）；
// 手写/学习得到的同符号 sink 优先，自动结果只补缺。
func withDiscoveredSinks(prof *profile.Profile, found []profile.SinkPattern) *profile.Profile {
	merged := *prof
	merged.ResponseSinks = append([]profile.SinkPattern(nil), prof.ResponseSinks...)
	for _, p := range found {
		if _, exists := prof.IsSink(p.Symbol); exists {
			continue
		}
		merged.ResponseSinks = append(merged.ResponseSinks, p)
	}
	return &merged
}
