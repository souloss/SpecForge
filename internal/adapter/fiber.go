package adapter

import (
	"go/ast"
	"go/token"
	"strconv"
	"strings"

	"github.com/specforge/specforge/internal/codegraph"
)

// FiberAdapter gofiber/v2 路由模式适配器。
type FiberAdapter struct{}

func (FiberAdapter) Name() string { return "gofiber/v2" }

func (FiberAdapter) RouterMethods() []string {
	return []string{"Get", "Post", "Put", "Delete", "Patch", "Head", "Options"}
}

// fiberState 函数内路由注册上下文。
type fiberState struct {
	groupPrefix map[string]string // 组变量名 → 前缀
}

// ExtractRoutes 抽取一个包内的全部 fiber 路由注册。
//
// 识别模式:
//
//	g := app.Group("/prefix")            // 组注册（可链式）
//	g.Post("/path", mw1, mw2, handler)   // 末位 handler，前置中间件
//	app.Get("/path", handler)            // 直接注册
//	for _, r := range table { app.Get(r.Path, r.Handler) }   // 表驱动（F3 难点）
func (a FiberAdapter) ExtractRoutes(p *PkgCtx) []Route {
	var routes []Route
	for _, f := range p.Syntax {
		st := &fiberState{groupPrefix: map[string]string{}}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				routes = append(routes, a.extractFunc(p, st, fd)...)
			}
		}
	}
	return routes
}

func (a FiberAdapter) extractFunc(p *PkgCtx, st *fiberState, fd *ast.FuncDecl) []Route {
	var routes []Route
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			// g := app.Group("/prefix")
			if len(node.Lhs) == 1 && len(node.Rhs) == 1 {
				if id, ok := node.Lhs[0].(*ast.Ident); ok {
					if prefix, ok2 := groupPrefixOf(node.Rhs[0], st); ok2 {
						st.groupPrefix[id.Name] = prefix
					}
				}
			}
		case *ast.CallExpr:
			// 路由注册调用（含表驱动循环体内调用）
			routes = append(routes, a.extractRouteCall(p, st, node, fd)...)
		}
		return true
	})
	return routes
}

// groupPrefixOf 识别 Group 调用并返回拼接前缀。
func groupPrefixOf(e ast.Expr, st *fiberState) (string, bool) {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) == 0 {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Group" {
		return "", false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	prefix, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	if base, ok := receiverPrefix(sel.X, st); ok {
		prefix = base + prefix
	}
	return prefix, true
}

// receiverPrefix 组变量 → 前缀。
func receiverPrefix(e ast.Expr, st *fiberState) (string, bool) {
	if id, ok := e.(*ast.Ident); ok {
		p, ok2 := st.groupPrefix[id.Name]
		return p, ok2
	}
	return "", false
}

// extractRouteCall 识别 <recv>.<Method>("/path", handlers...)。
func (a FiberAdapter) extractRouteCall(p *PkgCtx, st *fiberState, call *ast.CallExpr, fd *ast.FuncDecl) []Route {
	if len(call.Args) < 1 {
		return nil
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	method := strings.ToUpper(sel.Sel.Name)
	if !isRouterMethod(method) {
		return nil
	}
	// 请求访问器误判防护: c.Get("X-Header") 接收者是 *fiber.Ctx，
	// 且路由注册至少需要 (path, handler) 两个实参。
	if recvType := p.Info.RecvTypeOf(sel.X); recvType != "" && strings.Contains(recvType, "fiber.Ctx") {
		return nil
	}
	if len(call.Args) < 2 {
		return nil
	}

	// 路径字面量
	rawPath, pathLit := stringArg(call.Args[0])
	if !pathLit {
		// 表驱动注册: path 实参形如 r.Path（F3 循环注册模式）
		if table := a.resolveTableRoute(p, st, call, fd); len(table) > 0 {
			return table
		}
		return []Route{{
			Method: method, RawPath: "<dynamic>", Path: "<dynamic>",
			Handler: "", File: p.File(call), Line: p.Line(call),
			Unresolved: ReasonDynamic,
		}}
	}
	// 组前缀拼接
	prefix := ""
	if base, ok := receiverPrefix(sel.X, st); ok {
		prefix = base
	} else if g, ok := groupPrefixOf(sel.X, st); ok {
		prefix = g // 链式 app.Group("/x").Post(...)
	}
	full := prefix + rawPath
	normalized, wildcard := normalizePath(full)

	r := Route{
		Method:  method,
		Path:    normalized,
		RawPath: full,
		File:    p.File(call),
		Line:    p.Line(call),
	}
	if wildcard {
		r.Unresolved = ReasonWildcard
	}
	// handlers: 末位为 handler，前置为中间件链
	handlerIdx := len(call.Args) - 1
	for i, arg := range call.Args[1:] {
		symID := p.Info.ResolveExpr(arg)
		if symID == "" {
			continue
		}
		if i == handlerIdx-1 {
			r.Handler = symID
		} else {
			r.Middleware = append(r.Middleware, symID)
		}
	}
	if r.Handler == "" && len(call.Args) > 1 {
		r.Unresolved = ReasonNoHandler
	}
	return []Route{r}
}

// resolveTableRoute 表驱动循环注册的静态解析（设计文档 F3: 循环注册）。
//
//	for _, r := range table { app.Get(r.Path, r.Handler) }
//
// table 为函数内复合字面量 → 逐元素展开为字面量路由。
func (a FiberAdapter) resolveTableRoute(p *PkgCtx, st *fiberState, routeCall *ast.CallExpr, fd *ast.FuncDecl) []Route {
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
	method := strings.ToUpper(routeSel.Sel.Name)
	if !isRouterMethod(method) {
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
		normalized, wildcard := normalizePath(full)
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

func isRouterMethod(upper string) bool {
	switch upper {
	case "GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS", "ALL":
		return true
	}
	return false
}

// normalizePath 归一化: fiber ":param" → OpenAPI "{param}"；检测通配符。
func normalizePath(p string) (string, bool) {
	wildcard := strings.Contains(p, "*") || strings.Contains(p, "?")
	var b strings.Builder
	i := 0
	for i < len(p) {
		c := p[i]
		if c == ':' {
			j := i + 1
			for j < len(p) && p[j] != '/' {
				j++
			}
			b.WriteString("{" + p[i+1:j] + "}")
			i = j
			continue
		}
		b.WriteByte(c)
		i++
	}
	return b.String(), wildcard
}

func stringArg(e ast.Expr) (string, bool) {
	if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		s, err := strconv.Unquote(lit.Value)
		if err == nil {
			return s, true
		}
	}
	return "", false
}

// DetectFramework 依据 import 识别框架（F2）。
func DetectFramework(imports []string) string {
	for _, imp := range imports {
		if strings.Contains(imp, "gofiber/fiber") {
			return "gofiber/v2"
		}
		if strings.Contains(imp, "gin-gonic/gin") {
			return "gin"
		}
	}
	return ""
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

// NormalizePathSuffix 导出路径归一化（通配符展开后重用）。
func NormalizePathSuffix(p string) (string, bool) {
	return normalizePath(p)
}
