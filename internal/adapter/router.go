package adapter

import (
	"go/ast"
	"go/token"
	"strconv"
	"strings"

	"github.com/specforge/specforge/internal/codegraph"
)

// RouterSpec 「链式路由 DSL」框架的路由形态描述（fiber、gin、echo、chi 等同构）：
//
//	g := app.Group("/prefix")            // 分组（可链式、可嵌套）
//	g.Post("/path", mw1, mw2, handler)   // 动词方法注册：末位 handler，前置中间件
//	for _, r := range table { app.Get(r.Path, r.Handler) }   // 表驱动注册
type RouterSpec struct {
	RouterTypes map[string]bool   // 路由对象类型 ID（去指针，如 github.com/gofiber/fiber/v2.App）
	GroupMethod string            // 分组方法名（如 Group）
	Verbs       map[string]string // 注册方法名 → HTTP 方法（如 Get → GET、Any → ALL）
	HandlerArg  int               // handler 实参下标（0 为路径）；<=0 表示末位（其前为中间件，fiber/gin）
}

// routerFramework 基于 RouterSpec 的通用框架实现：路由抽取算法共享，框架只提供声明。
type routerFramework struct {
	name   string     // 框架标识
	short  string     // 证据标签前缀
	module string     // 模块路径前缀
	spec   RouterSpec // 路由形态
	prims  Primitives // 原语表
}

// Name 实现 Framework。
func (f routerFramework) Name() string { return f.name }

// Short 实现 Framework。
func (f routerFramework) Short() string { return f.short }

// Module 实现 Framework。
func (f routerFramework) Module() string { return f.module }

// Primitives 实现 Framework。
func (f routerFramework) Primitives() Primitives { return f.prims }

// routeState 函数内路由注册上下文。
type routeState struct {
	groupPrefix map[string]string // 组变量名 → 前缀
}

// ExtractRoutes 实现 Framework：抽取一个包内的全部路由注册。
func (f routerFramework) ExtractRoutes(p *PkgCtx) []Route {
	var routes []Route
	for _, file := range p.Syntax {
		st := &routeState{groupPrefix: map[string]string{}}
		for _, d := range file.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				routes = append(routes, f.extractFunc(p, st, fd)...)
			}
		}
	}
	return routes
}

// extractFunc 抽取单个函数体内的分组与路由注册。
func (f routerFramework) extractFunc(p *PkgCtx, st *routeState, fd *ast.FuncDecl) []Route {
	var routes []Route
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			if len(node.Lhs) == 1 && len(node.Rhs) == 1 {
				if id, ok := node.Lhs[0].(*ast.Ident); ok {
					if prefix, ok2 := f.groupPrefixOf(node.Rhs[0], st); ok2 {
						st.groupPrefix[id.Name] = prefix
					}
				}
			}
		case *ast.CallExpr:
			routes = append(routes, f.extractRouteCall(p, st, node, fd)...)
		}
		return true
	})
	return routes
}

// groupPrefixOf 识别分组调用并返回拼接后的前缀。
func (f routerFramework) groupPrefixOf(e ast.Expr, st *routeState) (string, bool) {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) == 0 {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != f.spec.GroupMethod {
		return "", false
	}
	prefix, ok := stringArg(call.Args[0])
	if !ok {
		return "", false
	}
	if base, ok := receiverPrefix(sel.X, st); ok {
		prefix = base + prefix
	}
	return prefix, true
}

// receiverPrefix 组变量 → 前缀。
func receiverPrefix(e ast.Expr, st *routeState) (string, bool) {
	if id, ok := e.(*ast.Ident); ok {
		p, ok2 := st.groupPrefix[id.Name]
		return p, ok2
	}
	return "", false
}

// isRouter 接收者静态类型是否为该框架的路由对象（全限定类型比对，去指针与泛型实参）。
func (f routerFramework) isRouter(recvType string) bool {
	t := strings.TrimPrefix(recvType, "*")
	if i := strings.IndexByte(t, '['); i >= 0 {
		t = t[:i]
	}
	return f.spec.RouterTypes[t]
}

// extractRouteCall 识别 <router>.<Verb>("/path", handlers...)。
//
// 误判防护：接收者必须能拿到静态类型且是该框架的路由对象；同名方法（HTTP 客户端的 Get/Post、
// 请求上下文的 Get、net/http.Header.Get 等）一律不是路由注册。拿不到类型时保守丢弃
// （宁可漏动态场景，也不把客户端调用误报成路由）。
func (f routerFramework) extractRouteCall(p *PkgCtx, st *routeState, call *ast.CallExpr, fd *ast.FuncDecl) []Route {
	if len(call.Args) < 2 {
		return nil
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	method, ok := f.spec.Verbs[sel.Sel.Name]
	if !ok {
		return nil
	}
	if recvType := p.Info.RecvTypeOf(sel.X); recvType == "" || !f.isRouter(recvType) {
		return nil
	}
	rawPath, pathLit := stringArg(call.Args[0])
	if !pathLit {
		if table := f.resolveTableRoute(p, st, call, fd); len(table) > 0 {
			return table
		}
		return []Route{{
			Method: method, RawPath: "<dynamic>", Path: "<dynamic>",
			File: p.File(call), Line: p.Line(call), Unresolved: ReasonDynamic,
		}}
	}
	prefix := ""
	if base, ok := receiverPrefix(sel.X, st); ok {
		prefix = base
	} else if g, ok := f.groupPrefixOf(sel.X, st); ok {
		prefix = g // 链式 app.Group("/x").Post(...)
	}
	full := prefix + rawPath
	normalized, wildcard := NormalizePath(full)
	r := Route{Method: method, Path: normalized, RawPath: full, File: p.File(call), Line: p.Line(call)}
	if wildcard {
		r.Unresolved = ReasonWildcard
	}
	// handlers: 默认末位为 handler、其前为中间件链；声明了 HandlerArg 时（如 echo 的 GET(path, h, m...)）以其为准
	handlerIdx := len(call.Args) - 1
	if f.spec.HandlerArg > 0 && f.spec.HandlerArg < len(call.Args) {
		handlerIdx = f.spec.HandlerArg
	}
	for i, arg := range call.Args[1:] {
		symID := p.Info.ResolveExpr(arg)
		if symID == "" {
			continue
		}
		if i+1 == handlerIdx {
			r.Handler = symID
		} else {
			r.Middleware = append(r.Middleware, symID)
		}
	}
	if r.Handler == "" {
		r.Unresolved = ReasonNoHandler
	}
	return []Route{r}
}

// resolveTableRoute 表驱动循环注册的静态解析（设计文档 F3: 循环注册）。
//
//	for _, r := range table { app.Get(r.Path, r.Handler) }
//
// table 为函数内复合字面量 → 逐元素展开为字面量路由。
func (f routerFramework) resolveTableRoute(p *PkgCtx, st *routeState, routeCall *ast.CallExpr, fd *ast.FuncDecl) []Route {
	sel, ok := routeCall.Args[0].(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Path" {
		return nil
	}
	base, ok := sel.X.(*ast.Ident)
	if !ok {
		return nil
	}
	comp := findLoopTable(fd.Body, base.Name)
	if comp == nil {
		return nil
	}
	routeSel, ok := routeCall.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	method, ok := f.spec.Verbs[routeSel.Sel.Name]
	if !ok {
		return nil
	}
	prefix := ""
	if base2, ok := receiverPrefix(routeSel.X, st); ok {
		prefix = base2
	}
	var routes []Route
	for _, elt := range comp.Elts {
		inner, ok := elt.(*ast.CompositeLit)
		if !ok {
			continue
		}
		var pathStr string
		var handlerExpr ast.Expr
		for _, kv := range inner.Elts {
			kve, ok := kv.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kve.Key.(*ast.Ident)
			if !ok {
				continue
			}
			switch key.Name {
			case "Path":
				if lit, ok := kve.Value.(*ast.BasicLit); ok {
					if s, err := strconv.Unquote(lit.Value); err == nil {
						pathStr = s
					}
				}
			case "Handler":
				handlerExpr = kve.Value
			}
		}
		if pathStr == "" || handlerExpr == nil {
			continue
		}
		full := prefix + pathStr
		normalized, wildcard := NormalizePath(full)
		nr := Route{
			Method: method, Path: normalized, RawPath: full,
			Handler: p.Info.ResolveExpr(handlerExpr),
			File:    p.File(inner),
			Line:    p.Line(inner),
		}
		if wildcard {
			nr.Unresolved = ReasonWildcard
		}
		if nr.Handler == "" {
			nr.Unresolved = ReasonNoHandler
		}
		routes = append(routes, nr)
	}
	return routes
}

// findLoopTable 定位 range 循环变量对应的复合字面量表。
func findLoopTable(body *ast.BlockStmt, loopVar string) *ast.CompositeLit {
	var tableName string
	ast.Inspect(body, func(n ast.Node) bool {
		if rs, ok := n.(*ast.RangeStmt); ok {
			if val, ok := rs.Value.(*ast.Ident); ok && val.Name == loopVar {
				if id, ok := rs.X.(*ast.Ident); ok {
					tableName = id.Name
				}
			}
		}
		return true
	})
	if tableName == "" {
		return nil
	}
	var comp *ast.CompositeLit
	ast.Inspect(body, func(n ast.Node) bool {
		if as, ok := n.(*ast.AssignStmt); ok && len(as.Lhs) == 1 && len(as.Rhs) == 1 {
			if id, ok := as.Lhs[0].(*ast.Ident); ok && id.Name == tableName {
				if cl, ok := as.Rhs[0].(*ast.CompositeLit); ok {
					comp = cl
				}
			}
		}
		return true
	})
	return comp
}

// NormalizePath 路由路径 → OpenAPI 路径模板：`:param` → `{param}`，`*name`（具名通配，如 gin）→ `{name}`；
// 匿名通配 `*`（含段内 `v*`）与可选参数 `?` 保留原样并报告 wildcard=true（由引擎展开或模板化）。
func NormalizePath(p string) (string, bool) {
	wildcard := strings.Contains(p, "?")
	var b strings.Builder
	for i := 0; i < len(p); {
		c := p[i]
		if c == ':' || (c == '*' && i+1 < len(p) && isIdentByte(p[i+1]) && (i == 0 || p[i-1] == '/')) {
			j := i + 1
			for j < len(p) && p[j] != '/' {
				j++
			}
			b.WriteString("{" + p[i+1:j] + "}")
			i = j
			continue
		}
		if c == '*' {
			wildcard = true
		}
		b.WriteByte(c)
		i++
	}
	return b.String(), wildcard
}

// isIdentByte 是否为标识符字符（ASCII）。
func isIdentByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// stringArg 字符串字面量实参的值。
func stringArg(e ast.Expr) (string, bool) {
	if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		if s, err := strconv.Unquote(lit.Value); err == nil {
			return s, true
		}
	}
	return "", false
}

// NewPkgCtx 构造包上下文。
func NewPkgCtx(path string, syntax []*ast.File, g *codegraph.Graph,
	resolve func(ast.Expr) string, argTypes func(*ast.CallExpr) []string,
	recvTypeOf func(ast.Expr) string) *PkgCtx {
	fset := g.Fset
	return &PkgCtx{
		Path:   path,
		Syntax: syntax,
		Graph:  g,
		Info: &TypeInfoView{
			ResolveExpr: resolve,
			ArgTypes:    argTypes,
			RecvTypeOf:  recvTypeOf,
		},
		File: func(n ast.Node) string {
			if pos := fset.Position(n.Pos()); pos.IsValid() {
				return pos.Filename
			}
			return ""
		},
		Line: func(n ast.Node) int {
			if pos := fset.Position(n.Pos()); pos.IsValid() {
				return pos.Line
			}
			return 0
		},
	}
}
