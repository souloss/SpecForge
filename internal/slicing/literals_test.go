package slicing

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/specforge/specforge/internal/codegraph"
	"github.com/specforge/specforge/internal/loader"
)

// buildTestSlicer 在临时目录加载 src 并返回 Slicer 与代码图（页面结果类的值级收窄单测）。
func buildTestSlicer(t *testing.T, src string) (*Slicer, *codegraph.Graph) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module m\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "svc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "svc", "svc.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := loader.LoadRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	g, err := codegraph.Build(l.Pkgs, l.Fset)
	if err != nil {
		t.Fatal(err)
	}
	g.SetRoot(l.Root)
	return New(g, nil, nil), g
}

// pageListFixture 模拟「页面结果嵌 PageResult，List 声明为 interface{}，函数内 `result.List = lists` 赋值」。
const pageListFixture = `package svc

type Item struct {
	OrderID int ` + "`json:\"orderId\"`" + `
}

type PageResult struct {
	List interface{} ` + "`json:\"list\"`" + `
}

type ApplyResult struct {
	PageResult
	StockCode string ` + "`json:\"equityCode\"`" + `
}

func Build() ApplyResult {
	var result ApplyResult
	var lists []Item
	result.List = lists
	result.StockCode = "x"
	return result
}
`

// TestFieldAssignTypesPromoted 嵌入结构体的 promoted 字段赋值：按外层类型与内层类型均能回溯到右值类型。
func TestFieldAssignTypesPromoted(t *testing.T) {
	s, g := buildTestSlicer(t, pageListFixture)
	ids := g.FuncIDs()
	if len(ids) != 1 {
		t.Fatalf("funcs = %v", ids)
	}
	fn := ids[0]
	got := s.FieldAssignTypes(fn, "m/svc.ApplyResult", "List")
	if len(got) != 1 || got[0].String() != "[]m/svc.Item" {
		t.Fatalf("FieldAssignTypes(ApplyResult) = %v", got)
	}
}
