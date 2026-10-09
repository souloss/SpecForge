package adapter

import (
	"go/ast"
	"strings"

	"github.com/specforge/specforge/internal/codegraph"
)

// gorillaMuxPkg gorilla/mux 的模块路径。
const gorillaMuxPkg = "github.com/gorilla/mux"

// gorillaMethod 构造 gorilla/mux 方法的符号 ID。
func gorillaMethod(recv, method string) string { return MethodSymbol(gorillaMuxPkg, recv, method) }

// gorillaFunc 构造 gorilla/mux 包级函数符号 ID。
func gorillaFunc(name string) string { return gorillaMuxPkg + "." + name }

// Gorilla gorilla/mux 适配器：路由注册是 `r.HandleFunc("/path", h).Methods("GET")` 的链式形态，
// 与 routerFramework 的 `<Router>.<Verb>(path, handler)` 不同，单独实现 ExtractRoutes。
// 路径参数 `{id}` 直接进入 OpenAPI 模板（gorilla 自身语法即 `{name}`），由 ensurePathParams 补齐。
var Gorilla Framework = &gorillaFramework{}

type gorillaFramework struct{}

// Name 实现 Framework。
func (gorillaFramework) Name() string { return "gorilla/mux" }

// Short 实现 Framework。
func (gorillaFramework) Short() string { return "gorilla" }

// Module 实现 Framework。
func (gorillaFramework) Module() string { return gorillaMuxPkg }

// Primitives 实现 Framework：net/http 原语（json Decoder/Encoder、http.Error、ResponseWriter）+ mux.Vars。
func (gorillaFramework) Primitives() Primitives {
	return Primitives{
		BodyBinders: []BodyBinder{
			{Symbol: chiMethod(jsonPkg, "Decoder", "Decode"), Arg: 0, ContentType: jsonContentType},
		},
		ParamReaders: []ParamReader{
			{Symbol: chiMethod(httpPkg, "Request", "PathValue"), In: "path", NameArg: 0, Type: "string"},
			{Symbol: chiMethod(httpPkg, "Header", "Get"), In: "header", NameArg: 0, Type: "string"},
			{Symbol: chiMethod(httpPkg, "Request", "Cookie"), In: "cookie", NameArg: 0, Type: "string"},
		},
		Writers: []Writer{
			{Symbol: chiMethod(jsonPkg, "Encoder", "Encode"), BodyArg: 0, StatusArg: NoArg, DefaultStatus: defaultOKStatus},
			{Symbol: chiFunc(httpPkg, "Error"), BodyArg: 1, StatusArg: 2, DefaultStatus: defaultOKStatus},
			{Symbol: chiInterfaceMethod(httpPkg, "ResponseWriter", "Write"), BodyArg: 0, StatusArg: NoArg, DefaultStatus: defaultOKStatus},
			{Symbol: chiInterfaceMethod(httpPkg, "ResponseWriter", "WriteHeader"), BodyArg: NoArg, StatusArg: 0, DefaultStatus: defaultOKStatus},
		},
	}
}

// ExtractRoutes 实现 Framework：扫描 `(*mux.Router).HandleFunc/Handle(...).Methods("VERB")` 链式注册。
// 两趟：先识别 Methods 链（带具体 HTTP 方法），再识别无 Methods 链的裸注册（缺省 ALL 展开）。
func (gorillaFramework) ExtractRoutes(p *PkgCtx) []Route {
	var routes []Route
	for _, file := range p.Syntax {
		chained := map[*ast.CallExpr]bool{} // 被 .Methods() 包裹的 HandleFunc/Handle 调用
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			methods := gorillaMethodsChain(call, p)
			if len(methods) == 0 {
				return true
			}
			sel, _ := call.Fun.(*ast.SelectorExpr)
			inner, ok := sel.X.(*ast.CallExpr)
			if !ok || !isGorillaRegister(inner, p) || len(inner.Args) < 2 {
				return true
			}
			path, ok := stringArg(inner.Args[0])
			if !ok {
				return true
			}
			handler := p.Info.ResolveExpr(inner.Args[1])
			if handler == "" {
				return true
			}
			chained[inner] = true
			for _, method := range methods {
				routes = append(routes, Route{Method: method, Path: path, RawPath: path, Handler: handler, File: p.File(call), Line: p.Line(call)})
			}
			return true
		})
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isGorillaRegister(call, p) || chained[call] || len(call.Args) < 2 {
				return true
			}
			path, ok := stringArg(call.Args[0])
			if !ok {
				return true
			}
			handler := p.Info.ResolveExpr(call.Args[1])
			if handler == "" {
				return true
			}
			routes = append(routes, Route{Method: "ALL", Path: path, RawPath: path, Handler: handler, File: p.File(call), Line: p.Line(call)})
			return true
		})
	}
	return routes
}

// gorillaMethodsChain 调用是否为 `(*mux.Route).Methods("GET",...)` 链，返回其常量 HTTP 方法。
func gorillaMethodsChain(call *ast.CallExpr, p *PkgCtx) []string {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Methods" {
		return nil
	}
	if recv := p.Info.RecvTypeOf(sel.X); strings.TrimPrefix(recv, "*") != gorillaMuxPkg+".Route" {
		return nil
	}
	var out []string
	for _, arg := range call.Args {
		if s, ok := stringArg(arg); ok {
			if m := strings.ToUpper(s); validGorillaMethod(m) {
				out = append(out, m)
			}
		}
	}
	return out
}

// isGorillaRegister 调用是否为 `(*mux.Router).HandleFunc(path, h)` 或 `(*mux.Router).Handle(path, h)`。
func isGorillaRegister(call *ast.CallExpr, p *PkgCtx) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || (sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle") {
		return false
	}
	recv := p.Info.RecvTypeOf(sel.X)
	return strings.TrimPrefix(recv, "*") == gorillaMuxPkg+".Router"
}

// validGorillaMethod 是否为 .Methods() 允许的 HTTP 方法。
func validGorillaMethod(m string) bool {
	switch m {
	case "GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS", "CONNECT", "TRACE":
		return true
	}
	return false
}

var _ = codegraph.MethodID // 保持 codegraph 依赖（符号 ID 口径一致）
