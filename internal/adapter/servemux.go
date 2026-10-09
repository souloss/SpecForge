package adapter

import (
	"go/ast"
	"strings"
)

// stdlibPkg net/http 包路径（ServeMux 所在）。
const stdlibPkg = "net/http"

// ServeMux net/http ServeMux 的专用适配器（Go 1.22+ 方法+路径模式路由）。
//
// ServeMux 的路由注册是 `mux.HandleFunc("GET /items/{id}", h)` / `mux.Handle("POST /items", h)`：
// 「HTTP 方法 + 路径」合并在一个字符串字面量里，与 routerFramework 的 `<Router>.<Verb>(path, handler)`
// 形态不同，因此单独实现 ExtractRoutes。net/http 被几乎所有 Go Web 仓库 import（chi/gorilla/fiber 的底层），
// 所以它不进 registry，仅当仓库没有命中任何第三方框架（纯标准库仓库）时由前端兜底启用。
type ServeMux struct {
	name   string
	short  string
	module string
	prims  Primitives
}

// NewServeMux 构造 net/http ServeMux 适配器（仅用于纯标准库仓库的兜底抽取）。
func NewServeMux() Framework {
	return &ServeMux{
		name:   "net/http",
		short:  "nethttp",
		module: stdlibPkg,
		prims: Primitives{
			BodyBinders: []BodyBinder{
				{Symbol: chiMethod(jsonPkg, "Decoder", "Decode"), Arg: 0, ContentType: jsonContentType},
			},
			ParamReaders: []ParamReader{
				{Symbol: chiMethod(httpPkg, "Request", "PathValue"), In: "path", NameArg: 0, Type: "string"},
				{Symbol: chiMethod(httpPkg, "Request", "FormValue"), In: "query", NameArg: 0, Type: "string"},
				{Symbol: chiMethod(httpPkg, "Request", "Cookie"), In: "cookie", NameArg: 0, Type: "string"},
				{Symbol: chiMethod(httpPkg, "Header", "Get"), In: "header", NameArg: 0, Type: "string"},
			},
			Writers: []Writer{
				{Symbol: chiMethod(jsonPkg, "Encoder", "Encode"), BodyArg: 0, StatusArg: NoArg, DefaultStatus: defaultOKStatus},
				{Symbol: chiFunc(httpPkg, "Error"), BodyArg: 1, StatusArg: 2, DefaultStatus: defaultOKStatus},
				{Symbol: chiInterfaceMethod(httpPkg, "ResponseWriter", "Write"), BodyArg: 0, StatusArg: NoArg, DefaultStatus: defaultOKStatus},
				{Symbol: chiInterfaceMethod(httpPkg, "ResponseWriter", "WriteHeader"), BodyArg: NoArg, StatusArg: 0, DefaultStatus: defaultOKStatus},
			},
		},
	}
}

// Name 实现 Framework。
func (s *ServeMux) Name() string { return s.name }

// Short 实现 Framework。
func (s *ServeMux) Short() string { return s.short }

// Module 实现 Framework。
func (s *ServeMux) Module() string { return s.module }

// Primitives 实现 Framework。
func (s *ServeMux) Primitives() Primitives { return s.prims }

// ExtractRoutes 实现 Framework：扫描 (*http.ServeMux).HandleFunc/Handle 的「方法+路径」字符串字面量注册。
func (s *ServeMux) ExtractRoutes(p *PkgCtx) []Route {
	var routes []Route
	for _, file := range p.Syntax {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle") {
				return true
			}
			if !isServeMux(p.Info.RecvTypeOf(sel.X)) {
				return true
			}
			if len(call.Args) < 2 {
				return true
			}
			pattern, ok := stringArg(call.Args[0])
			if !ok {
				return true
			}
			method, path, ok := splitServeMuxPattern(pattern)
			if !ok {
				return true
			}
			handler := p.Info.ResolveExpr(call.Args[1])
			if handler == "" {
				return true
			}
			routes = append(routes, Route{
				Method: method, Path: path, RawPath: pattern, Handler: handler,
				File: p.File(call), Line: p.Line(call),
			})
			return true
		})
	}
	return routes
}

// isServeMux 接收者静态类型是否为 net/http.ServeMux。
func isServeMux(recvType string) bool {
	return strings.TrimPrefix(recvType, "*") == stdlibPkg+".ServeMux"
}

// splitServeMuxPattern 拆分「METHOD /path」模式串：首段为 HTTP 方法（可省略，缺省 GET），其余为路径。
// 首段不是已知 HTTP 方法时返回 ok=false（避免把纯路径或非方法串误当路由）。
func splitServeMuxPattern(pattern string) (method, path string, ok bool) {
	i := strings.IndexByte(pattern, ' ')
	if i < 0 {
		// 无方法前缀的纯路径注册（如 mux.HandleFunc("/health", h)），缺省 GET。
		return "GET", pattern, true
	}
	head, rest := pattern[:i], strings.TrimSpace(pattern[i+1:])
	switch strings.ToUpper(head) {
	case "GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS":
		return strings.ToUpper(head), rest, true
	}
	return "", "", false
}
