package slicing

import (
	"go/ast"
	"go/types"
	"strings"
	"testing"

	"github.com/specforge/specforge/internal/adapter"
	"github.com/specforge/specforge/internal/profile"
)

func TestSummarizeWrapperIgnoresWriterWithoutBodySlot(t *testing.T) {
	_, graph := buildTestSlicer(t, `package svc
func response(data interface{}) {}
func handler(value interface{}) { response(value) }
`)
	writer := adapter.Writer{Symbol: "m/svc.response", BodyArg: profile.NoSlot}
	_, _, ok := summarizeWrapper(graph, "m/svc.handler", map[string]adapter.Writer{writer.Symbol: writer}, map[string]profile.SinkPattern{})
	if ok {
		t.Fatal("writer without a body argument must not form a wrapper")
	}
}

// TestWriteErrorStatusSlotAndErrOnly typed-repo 形态：writeError 把 HTTP 状态码与错误信封都当形参，
// 体内经 writeJSON 中转写 errorEnvelope。断言摘要出 StatusSlot（状态码形参下标）与 ErrOnly（恒为错误信封），
// 且信封类型为 errorEnvelope、无成功 data 槽。
func TestWriteErrorStatusSlotAndErrOnly(t *testing.T) {
	_, graph := buildTestSlicer(t, `package svc

type ErrorCode string

type errorEnvelope struct {
	Code    ErrorCode ` + "`json:\"code\"`" + `
	Message string    ` + "`json:\"message\"`" + `
	Request string    ` + "`json:\"requestId\"`" + `
}

func writeJSON(w interface{}, status int, payload interface{}) {}
func writeError(w interface{}, r interface{}, status int, code ErrorCode) {
	writeJSON(w, status, errorEnvelope{Code: code, Message: "m", Request: "req-1"})
}
`)
	// 模拟原生写出器原语表：writeJSON 依赖的 Encode 裸写（data 槽 = 0）。
	writers := map[string]adapter.Writer{
		"m/svc.writeJSON": {Symbol: "m/svc.writeJSON", BodyArg: 2, StatusArg: 1, DefaultStatus: 200},
	}
	// 第一趟：writeJSON 本身无 body 写出（体内无原生写出器），不算包装器；把 writeJSON 手工登记为已知。
	known := map[string]profile.SinkPattern{
		"m/svc.writeJSON": {Symbol: "m/svc.writeJSON", DataSlot: 2, StatusSlot: 1, Raw: true},
	}
	p, _, ok := summarizeWrapper(graph, "m/svc.writeError", writers, known)
	if !ok {
		t.Fatal("writeError should be summarized as a wrapper")
	}
	if p.StatusSlot != 2 {
		t.Fatalf("StatusSlot = %d, want 2 (status param)", p.StatusSlot)
	}
	if !p.ErrOnly {
		t.Fatal("writeError must be ErrOnly (no success branch)")
	}
	if !strings.HasSuffix(p.ErrEnvelopeType, "errorEnvelope") {
		t.Fatalf("ErrEnvelopeType = %q, want ...errorEnvelope", p.ErrEnvelopeType)
	}
	if p.DataSlot != profile.NoSlot {
		t.Fatalf("DataSlot = %d, want NoSlot (no business data field)", p.DataSlot)
	}
}

// TestReflectStructToMapArg mapany-repo 形态：handler 经反射助手 structToMap(req) 把结构体按
// json 标签转 map[string]any 再裸写，值流追不到反射读出的字段名，但入参实参的静态类型即真实形状。
// 断言 reflectMapArg 取到入参结构体类型；非 struct→map 的单入参调用（如 json.Marshal）返回 nil。
func TestReflectStructToMapArg(t *testing.T) {
	_, graph := buildTestSlicer(t, `package svc

type Order struct {
	Symbol string ` + "`json:\"symbol\"`" + `
	Qty    int    ` + "`json:\"qty\"`" + `
}

func structToMap(v any) map[string]any { return nil }
func asJSON(v any) []byte             { return nil }

func create(o *Order) map[string]any {
	return structToMap(o)
}
func marshal(o *Order) []byte {
	return asJSON(o)
}
`)
	info := graph.TypeInfoOfFunc("m/svc.create")
	fd := graph.FuncDeclOf("m/svc.create")
	call := findReturnCall(fd, info)
	if call == nil {
		t.Fatal("no return call in create")
	}
	if got := reflectMapArg(call, info); got == nil || got.String() != "m/svc.Order" {
		t.Fatalf("reflectMapArg(structToMap(o)) = %v, want m/svc.Order", got)
	}
	// json.Marshal 类（返回 []byte）不该被识别为 struct→map 助手。
	info2 := graph.TypeInfoOfFunc("m/svc.marshal")
	fd2 := graph.FuncDeclOf("m/svc.marshal")
	if got := reflectMapArg(findReturnCall(fd2, info2), info2); got != nil {
		t.Fatalf("reflectMapArg(asJSON(o)) = %v, want nil", got)
	}
}

// findReturnCall 取函数体内第一个 return 语句里的调用表达式。
func findReturnCall(fd *ast.FuncDecl, info *types.Info) *ast.CallExpr {
	var out *ast.CallExpr
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if ret, ok := n.(*ast.ReturnStmt); ok && len(ret.Results) == 1 {
			if call, ok := ast.Unparen(ret.Results[0]).(*ast.CallExpr); ok {
				out = call
				return false
			}
		}
		return true
	})
	return out
}
