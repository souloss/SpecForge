package slicing

import (
	"go/ast"
	"testing"

	"github.com/specforge/specforge/internal/codegraph"
)

// refineAnyFixture 接口方法返回 (interface{}, error)：实现里存在具体结构体、int 噪声分支、*T/T 两种形态。
const refineAnyFixture = `package svc

type Result struct {
	A int ` + "`json:\"a\"`" + `
}

type Svc interface {
	Get() (interface{}, error)
}

type svcImpl struct{}

func (svcImpl) Get() (interface{}, error) {
	var re Result
	if false {
		return 0, nil // 噪声：非数据路径的惰性标量
	}
	if false {
		return &re, nil // 指针与值同型，序列化一致
	}
	return re, nil
}

func Controller() error {
	var s Svc = svcImpl{}
	result, err := s.Get()
	return response(result, err)
}

func response(data interface{}, err error) error { return err }
`

// siteOfControllerCall 取 Controller 内 response(result, err) 调用，构造成 CallSite 供 refineAny 回溯。
func siteOfControllerCall(t *testing.T, g *codegraph.Graph) codegraph.CallSite {
	t.Helper()
	return siteOfResponseCall(t, g, "m/svc.Controller")
}

// siteOfResponseCall 取函数 fn 内首个调用（约定为 response(data, err)），构造成 CallSite。
func siteOfResponseCall(t *testing.T, g *codegraph.Graph, fn string) codegraph.CallSite {
	t.Helper()
	fd := g.FuncDeclOf(fn)
	info := g.TypeInfoOfFunc(fn)
	var call *ast.CallExpr
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			call = c
			return false
		}
		return true
	})
	return codegraph.CallSite{
		Caller: fn, Callee: "m/svc.response",
		ArgExprs: []ast.Expr{call.Args[0], call.Args[1]},
		ArgTypes: []string{info.TypeOf(call.Args[0]).String(), info.TypeOf(call.Args[1]).String()},
		ArgSyms:  []string{"result", "err"},
	}
}

// TestRefineAnyNarrowsInterfaceReturn 接口方法 any 返回：剔除 int 噪声与 *T/T 同型后收窄到唯一具体类型。
func TestRefineAnyNarrowsInterfaceReturn(t *testing.T) {
	s, g := buildTestSlicer(t, refineAnyFixture)
	site := siteOfControllerCall(t, g)
	if got, gt, _ := s.refineAny(site, 0); got != "m/svc.Result" || gt == nil || gt.String() != "m/svc.Result" {
		t.Fatalf("refineAny = %q (%v), want m/svc.Result", got, gt)
	}
}

// funcValueFixture 函数值与未导出字段中转：局部闭包、单调用方的函数形参、多调用方的函数形参、字段写入与取址。
const funcValueFixture = `package svc

type A struct {
	X int ` + "`json:\"x\"`" + `
}

type B struct {
	Y int ` + "`json:\"y\"`" + `
}

type box struct{ v interface{} }

type holder struct{ v interface{} }

type leaky struct{ v interface{} }

func response(data interface{}, err error) error { return err }

func Local() error {
	f := func() (interface{}, error) { return A{}, nil }
	r, err := f()
	return response(r, err)
}

func via(fetch func() (interface{}, error)) (interface{}, error) { return fetch() }

func Single() error {
	r, err := via(func() (interface{}, error) { return A{}, nil })
	return response(r, err)
}

func viaShared(fetch func() (interface{}, error)) interface{} {
	b := box{}
	v, _ := fetch()
	b.v = v
	return b.v
}

func SharedA() error { return response(viaShared(func() (interface{}, error) { return A{}, nil }), nil) }

func SharedB() error { return response(viaShared(func() (interface{}, error) { return B{}, nil }), nil) }

func Field() error {
	h := holder{v: A{}}
	return response(h.v, nil)
}

func Leaky() error {
	l := leaky{v: A{}}
	fill(&l.v)
	return response(l.v, nil)
}

func fill(p *interface{}) { *p = B{} }
`

// TestRefineAnyThroughFuncValues 局部闭包、单调用方形参、未导出字段写入均收窄到 A；
// 多调用方形参（经字段中转、无调用上下文）取并集 A|B 不收窄为单类型（改由闭合并集表达）；字段被取址不收窄。
func TestRefineAnyThroughFuncValues(t *testing.T) {
	s, g := buildTestSlicer(t, funcValueFixture)
	for fn, want := range map[string]string{
		"m/svc.Local":   "m/svc.A",
		"m/svc.Single":  "m/svc.A",
		"m/svc.Field":   "m/svc.A",
		"m/svc.SharedA": "",
		"m/svc.SharedB": "",
		"m/svc.Leaky":   "",
	} {
		if got, _, _ := s.refineAny(siteOfResponseCall(t, g, fn), 0); got != want {
			t.Errorf("%s: refineAny = %q, want %q", fn, got, want)
		}
	}
}

// TestRefineAnyClosedUnion 多调用方形参经字段中转：全部路径可反推、候选均为命名类型 → 闭合并集 [A B]；
// 存在不可反推路径（字段被取址）时不给并集。
func TestRefineAnyClosedUnion(t *testing.T) {
	s, g := buildTestSlicer(t, funcValueFixture)
	if _, _, alts := s.refineAny(siteOfResponseCall(t, g, "m/svc.SharedA"), 0); len(alts) != 2 ||
		alts[0] != "m/svc.A" || alts[1] != "m/svc.B" {
		t.Fatalf("SharedA alts = %v, want [m/svc.A m/svc.B]", alts)
	}
	if _, _, alts := s.refineAny(siteOfResponseCall(t, g, "m/svc.Leaky"), 0); alts != nil {
		t.Fatalf("Leaky alts = %v, want nil (opaque path)", alts)
	}
}
