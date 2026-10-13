package golang

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/specforge/specforge/internal/adapter"
	"github.com/specforge/specforge/internal/codegraph"
	"github.com/specforge/specforge/internal/facts"
	"github.com/specforge/specforge/internal/loader"
	"github.com/specforge/specforge/internal/profile"
	"github.com/specforge/specforge/internal/slicing"
)

// buildTestGraph 在临时模块里加载 fixture 源码并构造代码图（复用 loader + codegraph）。
func buildTestGraph(t *testing.T, src string) *codegraph.Graph {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module m\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "svc.go"), []byte(src), 0o644); err != nil {
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
	return g
}

// headerConstFixture 模拟真实仓库的 header 读取：参数名以常量引用给出（readHeader(HeaderUin)），
// 而非字符串字面量。readHeader 以注入的原语符号替代框架方法，避免单测依赖外部框架模块。
const headerConstFixture = `package svc

const HeaderUin = "X-uin"

func readHeader(name string) string { return name }

func list() string {
	return readHeader(HeaderUin)
}
`

// TestConstHeaderArgResolved header 参数名以常量引用给出时应解析为常量字面值（X-uin）。
// 原语符号注入为仓库内函数 readHeader，覆盖「常量实参解析 + 可达闭包内参数读取」两条路径。
func TestConstHeaderArgResolved(t *testing.T) {
	g := buildTestGraph(t, headerConstFixture)
	prof := profile.Default()
	slicer := slicing.New(g, prof, nil)
	// 用自建 primIndex 注入一个 header 参数读取原语，符号指向 fixture 内的 readHeader。
	prims := &primIndex{
		body: map[string]adapter.BodyBinder{}, structs: map[string]adapter.StructBinder{},
		params: map[string]adapter.ParamReader{"m.readHeader": {In: "header", NameArg: 0, Type: "string"}},
		origin: map[string]string{"m.readHeader": "stub:readHeader"},
	}

	b := &ContractPayloadBuilder{
		g: g, prof: prof, slicer: slicer,
		prims: prims, opKey: facts.OpKey("GET", "/List"),
		route: adapter.Route{Handler: "m.list"},
	}
	cp := facts.ContractPayload{}
	b.scanReachableParamReads(&cp)

	var headerNames []string
	for _, p := range cp.Params {
		if p.In == "header" {
			headerNames = append(headerNames, p.Name)
		}
	}
	if len(headerNames) != 1 || headerNames[0] != "X-uin" {
		t.Fatalf("header params = %v, want [X-uin]", headerNames)
	}
	if cp.Params[0].Type != "string" {
		t.Fatalf("header param type = %q, want string", cp.Params[0].Type)
	}
}

// TestTrimQuotesStr 常量值引号剥离的边界行为。
func TestTrimQuotesStr(t *testing.T) {
	if got := trimQuotesStr(`"X-uin"`); got != "X-uin" {
		t.Fatalf("trimQuotesStr quoted = %q, want X-uin", got)
	}
	if got := trimQuotesStr(`plain`); got != "plain" {
		t.Fatalf("trimQuotesStr plain = %q, want plain", got)
	}
}
