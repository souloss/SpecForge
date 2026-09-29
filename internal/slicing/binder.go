package slicing

import (
	"go/ast"
	"go/token"
	"go/types"

	"github.com/specforge/specforge/internal/adapter"
	"github.com/specforge/specforge/internal/codegraph"
)

// 请求绑定包装器自动发现（函数摘要）：与响应包装器对偶。
//
// 真实仓库常把 c.BodyParser 包一层（如 code.ValidateBody(c, params)），甚至把业务形参
// 装进请求信封再绑定（`p := new(BaseRequestParams); p.Params = params; c.BodyParser(p)`）。
// handler 层只看得到包装器调用，不识别包装器就会丢掉整个 requestBody。

// binderFixpointMaxRounds 包装器套包装器的不动点迭代上限（防御病态递归）。
const binderFixpointMaxRounds = 8

// Binder 请求绑定器摘要：调用点第 Slot 个实参的静态类型即请求体业务类型。
type Binder struct {
	Symbol       string // 绑定器函数符号 ID
	Slot         int    // 承载业务请求体的实参下标
	EnvelopeType string // 请求信封结构体类型 ID；空 = 业务体即请求体
	DataField    string // 信封中承载业务体的字段 JSON 名（EnvelopeType 非空时有效）
}

// DiscoverBinders 发现仓库内全部请求绑定包装器（含包装器的包装器），按符号 ID 索引。
// primitives 为框架的请求体绑定原语（如 fiber Ctx.BodyParser、gin Context.ShouldBindJSON）。
func DiscoverBinders(g *codegraph.Graph, primitives []adapter.BodyBinder) map[string]Binder {
	prim := make(map[string]int, len(primitives)) // 原语符号 → 绑定目标实参下标
	for _, b := range primitives {
		prim[b.Symbol] = b.Arg
	}
	out := map[string]Binder{}
	ids := g.FuncIDs() // 已排序：摘要结果与遍历顺序无关，但保持日志/调试确定性
	for round := 0; round < binderFixpointMaxRounds; round++ {
		changed := false
		for _, id := range ids {
			if _, done := out[id]; done {
				continue
			}
			if b, ok := summarizeBinder(g, id, out, prim); ok {
				out[id] = b
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	return out
}

// summarizeBinder 计算单个函数的绑定器摘要：形参（直接或经信封字段）流入绑定原语或已知绑定器。
func summarizeBinder(g *codegraph.Graph, fnID string, known map[string]Binder, prim map[string]int) (Binder, bool) {
	fd := g.FuncDeclOf(fnID)
	info := g.TypeInfoOfFunc(fnID)
	if fd == nil || fd.Body == nil || info == nil || fd.Type.Params == nil {
		return Binder{}, false
	}
	params := paramIndex(fd, info)
	if len(params) == 0 {
		return Binder{}, false
	}
	var found Binder
	ok := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if ok {
			return false
		}
		if _, isLit := n.(*ast.FuncLit); isLit {
			return false
		}
		call, isCall := n.(*ast.CallExpr)
		if !isCall {
			return true
		}
		callee, _ := codegraph.ResolveCallee(call, info)
		slot, envType, dataField := -1, "", ""
		if arg, isPrim := prim[callee]; isPrim {
			slot = arg
		} else if inner, isBinder := known[callee]; isBinder {
			slot, envType, dataField = inner.Slot, inner.EnvelopeType, inner.DataField
		}
		if slot < 0 || slot >= len(call.Args) {
			return true
		}
		arg := stripAddr(call.Args[slot])
		id, isIdent := arg.(*ast.Ident)
		if !isIdent {
			return true
		}
		obj := info.Uses[id]
		if idx, isParam := params[obj]; isParam {
			found, ok = Binder{Symbol: fnID, Slot: idx, EnvelopeType: envType, DataField: dataField}, true
			return false
		}
		// 局部信封变量：`p := new(T); p.F = 形参` 或 `p := &T{F: 形参}`。
		if envType != "" {
			return true // 已是信封绑定器的内层实参，不再叠加信封
		}
		if b, envOK := envelopeBinding(g, fd.Body, obj, info, params); envOK {
			b.Symbol = fnID
			found, ok = b, true
			return false
		}
		return true
	})
	return found, ok
}

// envelopeBinding 局部信封变量 obj 的业务形参流向：字段赋值 `obj.F = 形参` 或复合字面量 `T{F: 形参}`。
func envelopeBinding(g *codegraph.Graph, body *ast.BlockStmt, obj types.Object, info *types.Info,
	params map[types.Object]int) (Binder, bool) {
	if obj == nil {
		return Binder{}, false
	}
	t := obj.Type()
	if ptr, isPtr := t.(*types.Pointer); isPtr {
		t = ptr.Elem()
	}
	envType := typeIDOf(t)
	ti := g.Type(envType)
	if ti == nil || !ti.IsStruct {
		return Binder{}, false
	}
	if init := localInitOf(body, obj, info); init != nil {
		if lit, isLit := stripAddr(init).(*ast.CompositeLit); isLit {
			for _, el := range lit.Elts {
				kv, isKV := el.(*ast.KeyValueExpr)
				if !isKV {
					continue
				}
				key, k1 := kv.Key.(*ast.Ident)
				val, k2 := ast.Unparen(kv.Value).(*ast.Ident)
				if !k1 || !k2 {
					continue
				}
				if idx, isParam := params[info.Uses[val]]; isParam {
					return Binder{Slot: idx, EnvelopeType: envType, DataField: jsonFieldName(ti, key.Name)}, true
				}
			}
		}
	}
	var out Binder
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		as, isAssign := n.(*ast.AssignStmt)
		if !isAssign || as.Tok != token.ASSIGN || len(as.Lhs) != len(as.Rhs) {
			return true
		}
		for i, lhs := range as.Lhs {
			sel, isSel := lhs.(*ast.SelectorExpr)
			if !isSel {
				continue
			}
			base, isIdent := ast.Unparen(sel.X).(*ast.Ident)
			val, isVal := ast.Unparen(as.Rhs[i]).(*ast.Ident)
			if !isIdent || !isVal || info.Uses[base] != obj {
				continue
			}
			if idx, isParam := params[info.Uses[val]]; isParam {
				out, found = Binder{Slot: idx, EnvelopeType: envType, DataField: jsonFieldName(ti, sel.Sel.Name)}, true
				return false
			}
		}
		return true
	})
	return out, found
}
