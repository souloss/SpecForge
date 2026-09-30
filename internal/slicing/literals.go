package slicing

import (
	"go/ast"
	"go/types"

	"github.com/specforge/specforge/internal/codegraph"
)

// DataLiteral 流入成功行 data 槽的一个复合字面量（值级收窄的依据）。
type DataLiteral struct {
	Lit   *ast.CompositeLit // 字面量表达式
	Fn    string            // 所在函数符号 ID（字段赋值扫描的范围）
	Info  *types.Info       // 所在包的类型信息
	Extra []KeyValue        // 字面量之后对承载变量的常量键赋值（m["k"] = v），map 字面量才有
}

// DataLiterals 沿值流找出流入 h 的 data 槽的全部复合字面量（`fiber.Map{...}`、`T{...}`）。
// 任一路径不是字面量（不可反推、外部函数返回等）时 ok=false：只有「全部来源都是字面量」才能据此收窄，
// 否则会把一条路径的形状当成全部路径的形状。字面量 nil 路径忽略（无响应体分支）。
func (s *Slicer) DataLiterals(h SinkHit) ([]DataLiteral, bool) {
	pat, ok := s.prof.IsSink(h.Site.Callee)
	if !ok || !slotPresent(pat.DataSlot, h.Site.ArgSyms) || pat.DataSlot >= len(h.Site.ArgExprs) {
		return nil, false
	}
	var lits []DataLiteral
	leaf := func(e ast.Expr, t types.Type, info *types.Info, out map[string]bool) bool {
		if t != nil && isUntypedNil(t) {
			out[flowNil] = true
			return true
		}
		if e == nil {
			return false
		}
		if lit, isLit := stripAddr(e).(*ast.CompositeLit); isLit {
			fn := s.g.EnclosingFunc(lit.Pos())
			dl := DataLiteral{Lit: lit, Fn: fn, Info: info}
			if fd := s.g.FuncDeclOf(fn); fd != nil {
				dl.Extra = MapAssignments(fd.Body, holderOf(fd.Body, lit, info), info)
			}
			lits = append(lits, dl)
			out["lit"] = true
			return true
		}
		return false
	}
	c := &flowCtx{leaf: leaf, out: map[string]bool{}, visiting: map[string]bool{}}
	s.flowValues(h.Site.Caller, h.Site.ArgExprs[pat.DataSlot], 0, c)
	if c.out[flowOpaque] || len(lits) == 0 {
		return nil, false
	}
	return lits, true
}

// FieldAssignTypes 函数 fn 内对「类型为 typeID 的值」的字段 field 的全部赋值（`x.F = rhs`）右值静态类型；
// 嵌入结构体的 promoted 字段赋值（`result.List = lists`，List 属于嵌入的 PageResult）同样命中。
func (s *Slicer) FieldAssignTypes(fn, typeID, goField string) []types.Type {
	fd := s.g.FuncDeclOf(fn)
	info := s.g.TypeInfoOfFunc(fn)
	if fd == nil || fd.Body == nil || info == nil {
		return nil
	}
	var out []types.Type
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != len(as.Rhs) {
			return true
		}
		for i, lhs := range as.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != goField {
				continue
			}
			if baseTypeOf(sel.X, info) != typeID {
				continue
			}
			if rt := info.TypeOf(as.Rhs[i]); rt != nil {
				out = append(out, rt)
			}
		}
		return true
	})
	return out
}

// baseTypeOf 剥去指针取表达式类型 ID（与 FieldAssignTypes 的接收者口径一致）。
func baseTypeOf(x ast.Expr, info *types.Info) string {
	xt := info.TypeOf(x)
	if p, isPtr := xt.(*types.Pointer); isPtr {
		xt = p.Elem()
	}
	if xt == nil {
		return ""
	}
	return codegraph.TypeIDOf(xt)
}

// holderOf 以 lit 为初值的局部变量（`m := lit` / `var m = lit`）；没有则 nil。
func holderOf(body *ast.BlockStmt, lit *ast.CompositeLit, info *types.Info) types.Object {
	var obj types.Object
	ast.Inspect(body, func(n ast.Node) bool {
		switch st := n.(type) {
		case *ast.AssignStmt:
			for i, r := range st.Rhs {
				if stripAddr(r) == lit && i < len(st.Lhs) {
					if id, ok := st.Lhs[i].(*ast.Ident); ok {
						obj = info.Defs[id]
						if obj == nil {
							obj = info.Uses[id]
						}
					}
				}
			}
		case *ast.ValueSpec:
			for i, v := range st.Values {
				if stripAddr(v) == lit && i < len(st.Names) {
					obj = info.Defs[st.Names[i]]
				}
			}
		}
		return obj == nil
	})
	return obj
}
