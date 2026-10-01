package adapter

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestChiPathKeepsLiteralColonSuffix(t *testing.T) {
	fw := routerFramework{spec: chiRouterSpec(chiV5Pkg)}
	got, wildcard := fw.normalizePath("/items/{itemId}:rotate")
	if got != "/items/{itemId}:rotate" || wildcard {
		t.Fatalf("chi normalizePath = (%q, %v)", got, wildcard)
	}
}

func TestExtractRoutesDoesNotLeakGroupNamesAcrossFunctions(t *testing.T) {
	const source = `package routes
func first(app Router, h Handler) {
 g := app.Group("/prefix")
 g.Get("/first", h)
}
func second(g Router, h Handler) { g.Get("/second", h) }
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "routes.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg := &PkgCtx{Syntax: []*ast.File{file}, Info: &TypeInfoView{
		ResolveExpr: func(expr ast.Expr) string {
			if id, ok := expr.(*ast.Ident); ok {
				return "routes." + id.Name
			}
			return ""
		},
		RecvTypeOf: func(ast.Expr) string { return "routes.Router" },
	}, File: func(ast.Node) string { return "routes.go" }, Line: func(n ast.Node) int { return fset.Position(n.Pos()).Line }}
	fw := routerFramework{spec: RouterSpec{RouterTypes: map[string]bool{"routes.Router": true}, GroupMethod: "Group", Verbs: map[string]string{"Get": "GET"}}}
	routes := fw.ExtractRoutes(pkg)
	paths := map[string]bool{}
	for _, route := range routes {
		paths[route.Path] = true
	}
	if len(routes) != 2 || !paths["/prefix/first"] || !paths["/second"] {
		t.Fatalf("routes = %+v", routes)
	}
}
