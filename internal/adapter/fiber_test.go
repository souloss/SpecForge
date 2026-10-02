package adapter

import (
	"go/parser"
	"testing"
)

func TestExpandAllMethodRoutes(t *testing.T) {
	got := expandAllMethodRoutes([]Route{{Method: "ALL", Path: "/proxy/{wildcard}", Handler: "proxy.Handler", Unresolved: ReasonWildcard}})
	if len(got) != len(openAPIMethods)+1 {
		t.Fatalf("expanded route count = %d, want %d: %+v", len(got), len(openAPIMethods)+1, got)
	}
	methods := make(map[string]Route, len(got))
	for _, route := range got {
		if route.Method == "ALL" {
			t.Fatalf("ALL must not reach OpenAPI compilation: %+v", route)
		}
		methods[route.Method] = route
	}
	for _, method := range openAPIMethods {
		route, ok := methods[method]
		if !ok || route.Unresolved != ReasonWildcard || route.Handler != "proxy.Handler" {
			t.Errorf("expanded %s route = %+v, found=%v", method, route, ok)
		}
	}
	if route := methods["CONNECT"]; route.Unresolved != ReasonUnsupportedMethod {
		t.Errorf("CONNECT route = %+v, want unresolved reason %q", route, ReasonUnsupportedMethod)
	}
}

func TestNormalizePath(t *testing.T) {
	cases := []struct {
		in       string
		want     string
		wildcard bool
	}{
		{"/users/:id", "/users/{id}", false},
		{"/users/:id/items/:itemId", "/users/{id}/items/{itemId}", false},
		{"/ipo/v*", "/ipo/v*", true},
		{"/ipo/v*/OrderCheck", "/ipo/v*/OrderCheck", true},
		{"/health", "/health", false},
		{"/a/:x/*", "/a/{x}/*", true},
	}
	for _, c := range cases {
		got, wc := NormalizePath(c.in)
		if got != c.want || wc != c.wildcard {
			t.Errorf("normalizePath(%q) = (%q, %v), want (%q, %v)", c.in, got, wc, c.want, c.wildcard)
		}
	}
}

func TestStringArg(t *testing.T) {
	// 非 BasicLit → false（动态路径判定依赖）
	if _, ok := stringArg(nil); ok {
		t.Error("nil should not be a string arg")
	}
}

func TestRoutePathArgOptionalBaseURL(t *testing.T) {
	expr, err := parser.ParseExpr(`options.BaseURL + "/api/v1/users"`)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := routePathArg(expr)
	if !ok || got != "/api/v1/users" {
		t.Fatalf("routePathArg = %q, %v", got, ok)
	}
	lit, err := parser.ParseExpr(`"/healthz"`)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := routePathArg(lit); !ok || got != "/healthz" {
		t.Fatalf("literal routePathArg = %q, %v", got, ok)
	}
}

func TestDetectFramework(t *testing.T) {
	if got := DetectFramework([]string{"github.com/gofiber/fiber/v2"}); got != "gofiber/v2" {
		t.Errorf("fiber detect = %q", got)
	}
	if got := DetectFramework([]string{"github.com/gin-gonic/gin"}); got != "gin" {
		t.Errorf("gin detect = %q", got)
	}
	if got := DetectFramework([]string{"github.com/other/lib"}); got != "" {
		t.Errorf("unknown should be empty, got %q", got)
	}
}

func TestDetectChiFrameworks(t *testing.T) {
	if got := DetectFramework([]string{"github.com/go-chi/chi/v5", "github.com/go-chi/render"}); got != "chi/v5" {
		t.Errorf("chi v5 detect = %q", got)
	}
	if got := DetectFramework([]string{"github.com/go-chi/chi/middleware"}); got != "chi/v4" {
		t.Errorf("chi v4 detect = %q", got)
	}
	if got := DetectFramework([]string{"github.com/go-chi/chi/v5/middleware"}); got != "chi/v5" {
		t.Errorf("chi v5 middleware detect = %q", got)
	}
}

func TestChiPrimitives(t *testing.T) {
	p := ChiV5.Primitives()
	writers := map[string]Writer{}
	for _, w := range p.Writers {
		writers[w.Symbol] = w
	}
	if w, ok := writers[chiFunc(renderPkg, "JSON")]; !ok || w.BodyArg != 2 || w.StatusArg != NoArg {
		t.Fatalf("render.JSON writer = %+v, found=%v", w, ok)
	}
	if w, ok := writers[chiMethod(jsonPkg, "Encoder", "Encode")]; !ok || w.BodyArg != 0 {
		t.Fatalf("json Encoder.Encode writer = %+v, found=%v", w, ok)
	}
	readers := map[string]ParamReader{}
	for _, r := range p.ParamReaders {
		readers[r.Symbol] = r
	}
	if r, ok := readers[chiFunc(chiV5Pkg, "URLParam")]; !ok || r.In != "path" || r.NameArg != 1 {
		t.Fatalf("chi.URLParam reader = %+v, found=%v", r, ok)
	}
	spec := chiRouterSpec(chiV5Pkg)
	if !spec.NestedMethods["Group"] || !spec.NestedMethods["Route"] || spec.Verbs["Get"] != "GET" {
		t.Fatalf("chi route spec = %+v", spec)
	}
}

func TestSpecPreservesRouteShape(t *testing.T) {
	fw := (Spec{
		Name: "custom", Module: "example.com/custom", RouterTypes: []string{"example.com/custom.Router"},
		Verbs: map[string]string{"Handle": "ALL"}, HandlerArg: 2, PathArg: 1,
		PathArgs: map[string]int{"Handle": 1}, MethodArgs: map[string]int{"Handle": 0},
		NestedMethods: map[string]bool{"Route": true},
	}).Framework()
	got := fw.(routerFramework).spec
	if got.HandlerArg != 2 || got.PathArg != 1 || got.PathArgs["Handle"] != 1 || got.MethodArgs["Handle"] != 0 || !got.NestedMethods["Route"] {
		t.Fatalf("route shape lost when restoring spec: %+v", got)
	}
}
