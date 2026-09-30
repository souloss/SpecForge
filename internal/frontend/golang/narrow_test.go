package golang

import (
	"go/ast"
	"os"
	"path/filepath"
	"testing"

	"github.com/specforge/specforge/internal/codegraph"
	"github.com/specforge/specforge/internal/loader"
	"github.com/specforge/specforge/internal/slicing"
	"github.com/specforge/specforge/internal/typeschema"
)

// nestedLitFixture 结构体字面量内嵌 map[string]interface{} 字面量（仿 PlacementApplyRemindCheckResult.Template.Params）。
const nestedLitFixture = `package svc

type Template struct {
	ID     int                    ` + "`json:\"templateId\"`" + `
	Params map[string]interface{} ` + "`json:\"params\"`" + ` // 模板参数
}

type Result struct {
	Count    int      ` + "`json:\"count\"`" + `
	Template Template ` + "`json:\"template\"`" + `
}

func Build(code string) Result {
	re := Result{
		Template: Template{
			ID:     1,
			Params: map[string]interface{}{"equityCode": code, "total": 3},
		},
	}
	return re
}
`

// narrowBuilder 在临时模块上构造只含收窄所需依赖的 builder，并返回 Build 内的结构体字面量。
func narrowBuilder(t *testing.T, src string) (*ContractPayloadBuilder, slicing.DataLiteral) {
	t.Helper()
	dir := t.TempDir()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module m\n\ngo 1.21\n"), 0o644))
	must(os.MkdirAll(filepath.Join(dir, "svc"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "svc", "svc.go"), []byte(src), 0o644))
	l, err := loader.LoadRepo(dir)
	must(err)
	g, err := codegraph.Build(l.Pkgs, l.Fset)
	must(err)
	g.SetRoot(l.Root)
	const fn = "m/svc.Build"
	var lit *ast.CompositeLit
	ast.Inspect(g.FuncDeclOf(fn).Body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CompositeLit); ok && lit == nil {
			lit = c
		}
		return lit == nil
	})
	b := &ContractPayloadBuilder{g: g, synth: typeschema.New(g), slicer: slicing.New(g, nil, nil)}
	return b, slicing.DataLiteral{Lit: lit, Fn: fn, Info: g.TypeInfoOfFunc(fn)}
}

// TestStructLiteralNarrowsNestedMap 嵌套 map 字面量按常量键收窄为 object：值取静态类型、全部键必填，
// 字段 doc 保留，map 合成说明丢弃。
func TestStructLiteralNarrowsNestedMap(t *testing.T) {
	b, l := narrowBuilder(t, nestedLitFixture)
	sc := b.structLiteralSchema([]slicing.DataLiteral{l}, l.Info.TypeOf(l.Lit))
	if sc == nil {
		t.Fatal("nested map literal not narrowed")
	}
	var params *typeschema.Schema
	for _, p := range sc.Props {
		if p.Name == "template" {
			for _, q := range p.Schema.Props {
				if q.Name == "params" {
					params = q.Schema
				}
			}
		}
	}
	if params == nil || params.Unknown || len(params.Props) != 2 || len(params.Required) != 2 {
		t.Fatalf("params = %+v", params)
	}
	if params.Props[0].Name != "equityCode" || params.Props[0].Schema.Type != "string" ||
		params.Props[1].Name != "total" || params.Props[1].Schema.Type != "integer" {
		t.Fatalf("params props = %+v", params.Props)
	}
	if params.Description != "模板参数" {
		t.Fatalf("description = %q, want field doc", params.Description)
	}
}
