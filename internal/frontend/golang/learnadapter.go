package golang

import (
	"context"
	"fmt"
	"go/types"
	"path/filepath"
	"sort"
	"strings"

	"github.com/specforge/specforge/internal/adapter"
	"github.com/specforge/specforge/internal/codegraph"
	"github.com/specforge/specforge/internal/frontend"
	"github.com/specforge/specforge/internal/infer"
	"github.com/specforge/specforge/internal/loader"
)

// L3 框架层：仓库用了未登记的 Web 框架（抽不出路由）时，把候选框架包的导出 API 摘要交给 LLM，
// 由它写出与 adapter/gin.go 同构的声明；引擎在类型信息上逐个复核符号与实参下标并试运行抽路由，
// 通过后落盘 .specforge/adapters/<name>.json——此后该框架的全部仓库都走纯静态管线，不再有 LLM 成本。

// adaptersDir 仓库内适配器声明目录（相对仓库根）。
var adaptersDir = filepath.Join(".specforge", "adapters")

// maxAdapterCandidates 交给 LLM 的候选框架包上限。
const maxAdapterCandidates = 3

// maxDigestFuncs 候选包摘要中包级函数签名的上限。
const maxDigestFuncs = 40

// httpVerbs 路由注册方法名（大小写不敏感）→ 是否为 HTTP 动词（候选框架识别用）。
var httpVerbs = map[string]bool{"get": true, "post": true, "put": true, "delete": true, "patch": true, "head": true, "options": true}

// validMethods 适配器声明允许的 HTTP 方法取值。
var validMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "DELETE": true, "PATCH": true, "HEAD": true, "OPTIONS": true, "TRACE": true, "ALL": true}

// validIn 参数位置取值。
var validIn = map[string]bool{"query": true, "path": true, "header": true, "cookie": true}

// learnAdapter L3：识别候选框架包 → LLM 提出声明 → 复核 → 试运行 → 落盘。成功返回声明。
func learnAdapter(ctx context.Context, req frontend.Request, l *loader.Loaded, g *codegraph.Graph) (*adapter.Spec, frontend.StepStats) {
	var st frontend.StepStats
	universe := importUniverse(l)
	cands := candidateFrameworks(universe, l.Module)
	if len(cands) == 0 {
		return nil, st
	}
	task := infer.AdapterTask{Module: modulePathOf(cands[0].Path())}
	for _, p := range cands {
		task.Packages = append(task.Packages, digestPackage(p))
	}
	st.Attempted++
	prop, err := infer.ProposeAdapter(ctx, req.Provider, task)
	if err != nil {
		st.Failed++
		req.Log.Warn("适配器生成失败", "err", err)
		return nil, st
	}
	spec, err := verifyAdapter(prop, universe, task.Module)
	if err == nil {
		routes := extractAllRoutes(g, []adapter.Framework{spec.Framework()})
		if countResolved(routes) == 0 {
			err = fmt.Errorf("dry run extracted no routes")
		}
	}
	if err != nil {
		st.Rejected++
		req.Log.Warn("适配器声明未通过复核，丢弃", "err", err)
		return nil, st
	}
	st.Accepted++
	if path, err := adapter.SaveSpec(filepath.Join(req.RepoDir, adaptersDir), *spec); err == nil {
		req.Log.Info("已生成适配器声明（请审阅后提交）", "framework", spec.Name, "file", path)
	}
	return spec, st
}

// countResolved 可生成 operation 的路由数。
func countResolved(routes []adapter.Route) int {
	n := 0
	for _, r := range routes {
		if r.Unresolved == "" {
			n++
		}
	}
	return n
}

// importUniverse 仓库包直接与间接导入的全部包（来自导出数据的类型信息），按路径索引。
func importUniverse(l *loader.Loaded) map[string]*types.Package {
	out := map[string]*types.Package{}
	var walk func(p *types.Package)
	walk = func(p *types.Package) {
		if p == nil || out[p.Path()] != nil {
			return
		}
		out[p.Path()] = p
		for _, imp := range p.Imports() {
			walk(imp)
		}
	}
	for _, p := range l.Pkgs {
		if p.Types != nil {
			walk(p.Types)
		}
	}
	return out
}

// candidateFrameworks 仓库外、含「路由对象」类型的包：某命名类型至少有两个 HTTP 动词方法，
// 且方法形如 (path string, handler ...)。结果按路径排序、截断到上限。
func candidateFrameworks(universe map[string]*types.Package, module string) []*types.Package {
	var out []*types.Package
	for path, p := range universe {
		if module != "" && strings.HasPrefix(path, module) {
			continue
		}
		if len(routerTypesIn(p)) > 0 {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path() < out[j].Path() })
	if len(out) > maxAdapterCandidates {
		out = out[:maxAdapterCandidates]
	}
	return out
}

// routerTypesIn 包内像路由对象的命名类型（有 ≥2 个 (string, handler, ...) 形的 HTTP 动词方法）。
func routerTypesIn(p *types.Package) []*types.Named {
	var out []*types.Named
	for _, name := range p.Scope().Names() {
		tn, ok := p.Scope().Lookup(name).(*types.TypeName)
		if !ok || !tn.Exported() {
			continue
		}
		named, ok := tn.Type().(*types.Named)
		if !ok {
			continue
		}
		verbs := 0
		ms := types.NewMethodSet(types.NewPointer(named))
		for i := 0; i < ms.Len(); i++ {
			fn, ok := ms.At(i).Obj().(*types.Func)
			if !ok || !httpVerbs[strings.ToLower(fn.Name())] {
				continue
			}
			sig := fn.Signature()
			if sig.Params().Len() >= 2 && isString(sig.Params().At(0).Type()) {
				verbs++
			}
		}
		if verbs >= 2 {
			out = append(out, named)
		}
	}
	return out
}

// isString 类型底层是否为 string。
func isString(t types.Type) bool {
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Info()&types.IsString != 0
}

// digestPackage 候选包 API 摘要：路由对象类型、其动词方法的 handler 形参引用到的同包类型（请求上下文）、包级函数。
func digestPackage(p *types.Package) infer.PackageAPI {
	api := infer.PackageAPI{Path: p.Path()}
	qual := func(q *types.Package) string { return q.Name() }
	include := map[*types.Named]bool{}
	for _, rt := range routerTypesIn(p) {
		include[rt] = true
		ms := types.NewMethodSet(types.NewPointer(rt))
		for i := 0; i < ms.Len(); i++ {
			if fn, ok := ms.At(i).Obj().(*types.Func); ok {
				collectPkgNamed(fn.Signature(), p, include)
			}
		}
	}
	// 二跳：handler 函数类型（如 echo.HandlerFunc）的形参引用的上下文类型（echo.Context）。
	for n := range include {
		if sig, ok := n.Underlying().(*types.Signature); ok {
			collectPkgNamed(sig, p, include)
		}
	}
	var named []*types.Named
	for n := range include {
		named = append(named, n)
	}
	sort.Slice(named, func(i, j int) bool { return named[i].Obj().Name() < named[j].Obj().Name() })
	for _, n := range named {
		t := infer.TypeAPI{Name: n.Obj().Name(), Kind: kindName(n)}
		var mset *types.MethodSet
		if types.IsInterface(n) {
			mset = types.NewMethodSet(n)
		} else {
			mset = types.NewMethodSet(types.NewPointer(n))
		}
		for i := 0; i < mset.Len(); i++ {
			if fn, ok := mset.At(i).Obj().(*types.Func); ok && fn.Exported() {
				t.Methods = append(t.Methods, fn.Name()+strings.TrimPrefix(types.TypeString(fn.Signature(), qual), "func"))
			}
		}
		if sig, ok := n.Underlying().(*types.Signature); ok {
			t.Methods = append(t.Methods, "type "+n.Obj().Name()+" "+types.TypeString(sig, qual))
		}
		api.Types = append(api.Types, t)
	}
	for _, name := range p.Scope().Names() {
		if fn, ok := p.Scope().Lookup(name).(*types.Func); ok && fn.Exported() && len(api.Funcs) < maxDigestFuncs {
			api.Funcs = append(api.Funcs, fn.Name()+strings.TrimPrefix(types.TypeString(fn.Signature(), qual), "func"))
		}
	}
	return api
}

// collectPkgNamed 收集签名形参/结果中引用到的 p 包内命名类型（去指针、切片元素）。
func collectPkgNamed(sig *types.Signature, p *types.Package, into map[*types.Named]bool) {
	var visit func(t types.Type)
	visit = func(t types.Type) {
		switch x := t.(type) {
		case *types.Pointer:
			visit(x.Elem())
		case *types.Slice:
			visit(x.Elem())
		case *types.Named:
			if x.Obj().Pkg() == p && x.Obj().Exported() {
				into[x] = true
			}
		}
	}
	for _, tup := range []*types.Tuple{sig.Params(), sig.Results()} {
		for i := 0; i < tup.Len(); i++ {
			visit(tup.At(i).Type())
		}
	}
}

// kindName 命名类型的种类名。
func kindName(n *types.Named) string {
	switch n.Underlying().(type) {
	case *types.Struct:
		return "struct"
	case *types.Interface:
		return "interface"
	case *types.Signature:
		return "func"
	}
	return "other"
}

// modulePathOf 由包路径推断模块路径（去掉末尾的 /vN 之后的子包段不做推断，直接取包路径）。
func modulePathOf(pkgPath string) string { return pkgPath }

// verifyAdapter 在类型信息上逐条复核 LLM 的声明：路由类型存在且有所声明的动词方法；每个原语符号解析到真实
// 函数/方法（归一为代码图的符号 ID 口径），实参下标在签名范围内且类型相容（名字实参为 string、状态码为整数）。
// 不合规的条目剪除并记入 Dropped（供审阅）；核心部分（≥1 路由类型、≥1 动词、≥1 写出器）缺失时整体拒绝。
func verifyAdapter(prop *infer.AdapterProposal, universe map[string]*types.Package, module string) (*adapter.Spec, error) {
	spec := &adapter.Spec{Name: prop.Name, Short: prop.Name, Module: module, GroupMethod: prop.GroupMethod,
		Verbs: map[string]string{}, HandlerArg: prop.HandlerArg, Learned: true}
	if spec.Name == "" {
		return nil, fmt.Errorf("adapter has no name")
	}
	drop := func(format string, a ...any) { spec.Dropped = append(spec.Dropped, fmt.Sprintf(format, a...)) }
	var routers []*types.Named
	for _, rt := range prop.RouterTypes {
		named, ok := lookupNamed(universe, rt)
		if !ok {
			drop("router type %s: not found", rt)
			continue
		}
		routers = append(routers, named)
		spec.RouterTypes = append(spec.RouterTypes, named.Obj().Pkg().Path()+"."+named.Obj().Name())
	}
	verbs := make([]string, 0, len(prop.Verbs))
	for m := range prop.Verbs {
		verbs = append(verbs, m)
	}
	sort.Strings(verbs)
	for _, m := range verbs {
		if verb := prop.Verbs[m]; validMethods[verb] && anyHasMethod(routers, m) {
			spec.Verbs[m] = verb
		} else {
			drop("verb %s → %s: not a router method or not an OpenAPI method", m, verb)
		}
	}
	if spec.GroupMethod != "" && !anyHasMethod(routers, spec.GroupMethod) {
		drop("group method %s: not found", spec.GroupMethod)
		spec.GroupMethod = ""
	}
	for _, b := range prop.BodyBinders {
		id, sig, ok := resolveSymbol(universe, b.Symbol)
		if !ok || b.Arg < 0 || b.Arg >= sig.Params().Len() {
			drop("body binder %s: not found or bad arg", b.Symbol)
			continue
		}
		spec.BodyBinders = append(spec.BodyBinders, adapter.BodyBinder{Symbol: id, Arg: b.Arg, ContentType: jsonMediaType})
	}
	for _, b := range prop.StructBinders {
		id, sig, ok := resolveSymbol(universe, b.Symbol)
		if !ok || !validIn[b.In] || b.TagKey == "" || b.Arg < 0 || b.Arg >= sig.Params().Len() {
			drop("struct binder %s: not found or bad arg/in/tagKey", b.Symbol)
			continue
		}
		spec.StructBinders = append(spec.StructBinders, adapter.StructBinder{Symbol: id, Arg: b.Arg, In: b.In, TagKey: b.TagKey})
	}
	for _, r := range prop.ParamReaders {
		id, sig, ok := resolveSymbol(universe, r.Symbol)
		if !ok || !validIn[r.In] || r.NameArg < 0 || r.NameArg >= sig.Params().Len() || !isString(sig.Params().At(r.NameArg).Type()) {
			drop("param reader %s: not found or name arg is not a string", r.Symbol)
			continue
		}
		typ := r.Type
		if typ == "" {
			typ = "string"
		}
		spec.ParamReaders = append(spec.ParamReaders, adapter.ParamReader{Symbol: id, In: r.In, NameArg: r.NameArg, Type: typ})
	}
	for _, w := range prop.Writers {
		id, sig, ok := resolveSymbol(universe, w.Symbol)
		if !ok {
			drop("writer %s: not found", w.Symbol)
			continue
		}
		n := sig.Params().Len()
		if w.BodyArg < 0 || w.BodyArg >= n || w.StatusArg >= n || (w.StatusArg >= 0 && !isInteger(sig.Params().At(w.StatusArg).Type())) {
			drop("writer %s: bad body/status arg", w.Symbol)
			continue
		}
		status := w.StatusArg
		if status < 0 {
			status = adapter.NoArg
		}
		spec.Writers = append(spec.Writers, adapter.Writer{Symbol: id, BodyArg: w.BodyArg, StatusArg: status, DefaultStatus: fallbackWriterStatus})
	}
	if len(spec.RouterTypes) == 0 || len(spec.Verbs) == 0 || len(spec.Writers) == 0 {
		return nil, fmt.Errorf("adapter core incomplete after verification (router types %d, verbs %d, writers %d): %s",
			len(spec.RouterTypes), len(spec.Verbs), len(spec.Writers), strings.Join(spec.Dropped, "; "))
	}
	return spec, nil
}

// jsonMediaType 请求体媒体类型。
const jsonMediaType = "application/json"

// isInteger 类型底层是否为整数。
func isInteger(t types.Type) bool {
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Info()&types.IsInteger != 0
}

// splitSymbol "包路径.X[.Y]" → (包, 剩余段)：取 universe 中最长匹配的包路径。
func splitSymbol(universe map[string]*types.Package, sym string) (*types.Package, []string, bool) {
	sym = strings.NewReplacer("(*", "", ")", "", " ", "").Replace(sym)
	var best *types.Package
	for path, p := range universe {
		if strings.HasPrefix(sym, path+".") && (best == nil || len(path) > len(best.Path())) {
			best = p
		}
	}
	if best == nil {
		return nil, nil, false
	}
	return best, strings.Split(sym[len(best.Path())+1:], "."), true
}

// lookupNamed 解析 "包路径.类型名"。
func lookupNamed(universe map[string]*types.Package, sym string) (*types.Named, bool) {
	p, rest, ok := splitSymbol(universe, sym)
	if !ok || len(rest) != 1 {
		return nil, false
	}
	tn, ok := p.Scope().Lookup(rest[0]).(*types.TypeName)
	if !ok {
		return nil, false
	}
	named, ok := tn.Type().(*types.Named)
	return named, ok
}

// anyHasMethod 任一路由类型（或其指针）方法集中有名为 name 的方法。
func anyHasMethod(routers []*types.Named, name string) bool {
	for _, n := range routers {
		obj, _, _ := types.LookupFieldOrMethod(types.NewPointer(n), true, n.Obj().Pkg(), name)
		if _, ok := obj.(*types.Func); ok {
			return true
		}
		obj, _, _ = types.LookupFieldOrMethod(n, true, n.Obj().Pkg(), name)
		if _, ok := obj.(*types.Func); ok {
			return true
		}
	}
	return false
}

// resolveSymbol 解析原语符号为代码图符号 ID 与签名：包级函数 → pkg.F；接口方法 → pkg.I.M（与调用边的
// 接口分发口径一致）；具体类型方法 → 声明方法的规范 ID（嵌入提升的方法取其声明类型）。
func resolveSymbol(universe map[string]*types.Package, sym string) (string, *types.Signature, bool) {
	p, rest, ok := splitSymbol(universe, sym)
	if !ok {
		return "", nil, false
	}
	switch len(rest) {
	case 1:
		fn, ok := p.Scope().Lookup(rest[0]).(*types.Func)
		if !ok {
			return "", nil, false
		}
		return fn.FullName(), fn.Signature(), true
	case 2:
		tn, ok := p.Scope().Lookup(rest[0]).(*types.TypeName)
		if !ok {
			return "", nil, false
		}
		named, ok := tn.Type().(*types.Named)
		if !ok {
			return "", nil, false
		}
		if types.IsInterface(named) {
			obj, _, _ := types.LookupFieldOrMethod(named, true, p, rest[1])
			fn, ok := obj.(*types.Func)
			if !ok {
				return "", nil, false
			}
			return p.Path() + "." + rest[0] + "." + rest[1], fn.Signature(), true
		}
		obj, _, _ := types.LookupFieldOrMethod(types.NewPointer(named), true, p, rest[1])
		fn, ok := obj.(*types.Func)
		if !ok {
			return "", nil, false
		}
		return codegraph.FuncID(fn), fn.Signature(), true
	}
	return "", nil, false
}
