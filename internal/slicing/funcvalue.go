package slicing

import (
	"go/ast"
	"go/token"
	"go/types"

	"github.com/specforge/specforge/internal/codegraph"
)

// 函数值与未导出字段的值流：让「经函数形参/局部闭包调用」「经结构体字段中转」的值继续回溯，
// 而不是在调用点/字段取值处笼统地判为不可反推。

// funcValueReturns 函数值调用 `f(x)` 的第 idx 个返回值来源：f 为本函数的函数类型形参时取调用方传入的实参，
// f 为局部变量时取其全部赋值（闭包字面量或具名函数）。来源不可确定时返回 false（由调用方标不可反推）。
func (s *Slicer) funcValueReturns(fnID string, call *ast.CallExpr, idx, depth int, c *flowCtx) bool {
	info, fd := s.g.TypeInfoOfFunc(fnID), s.g.FuncDeclOf(fnID)
	id, ok := ast.Unparen(call.Fun).(*ast.Ident)
	if !ok || info == nil || fd == nil || fd.Body == nil {
		return false
	}
	v, ok := info.Uses[id].(*types.Var)
	if !ok {
		return false
	}
	key := fnID + "#fv" + itoa(int(v.Pos())) + "#" + itoa(idx)
	if c.visiting[key] {
		return true
	}
	if pi, isParam := paramIndex(fd, info)[v]; isParam {
		args := s.paramArgs(fnID, pi, c)
		if args == nil {
			return false
		}
		c.visiting[key] = true
		// 实参位于调用方函数体内；调用方自身的入参上下文未知，不再向上借用。
		sub := *c
		sub.callPos, sub.callExpr, sub.callerFn = token.NoPos, nil, ""
		for _, a := range args {
			s.funcExprReturns(a.fn, a.value, idx, depth+1, &sub)
		}
		return true
	}
	srcs := localFuncSources(fd.Body, info, v)
	if len(srcs) == 0 {
		return false
	}
	c.visiting[key] = true
	for _, src := range srcs {
		s.funcExprReturns(fnID, src, idx, depth+1, c)
	}
	return true
}

// paramArgs 函数 fnID 第 pi 个形参的实参来源：已知调用上下文（从某调用点进入）时取该调用点实参；
// 否则取全部调用点实参的并集（如经共享缓存/singleflight 中转后到达，结果可能来自任一调用方）。
// 任一调用点无法取到实参（接口分发的近似边、实参不足）时返回 nil。
func (s *Slicer) paramArgs(fnID string, pi int, c *flowCtx) []fieldWrite {
	if outer, ok := c.callExpr.(*ast.CallExpr); ok && c.callerFn != "" {
		if pi >= len(outer.Args) {
			return nil
		}
		return []fieldWrite{{fn: c.callerFn, value: outer.Args[pi]}}
	}
	sites := s.g.CallersOf(fnID)
	if len(sites) == 0 {
		return nil
	}
	out := make([]fieldWrite, 0, len(sites))
	for _, site := range sites {
		if pi >= len(site.ArgExprs) || site.ArgExprs[pi] == nil {
			return nil
		}
		out = append(out, fieldWrite{fn: site.Caller, value: site.ArgExprs[pi]})
	}
	return out
}

// funcExprReturns 函数值表达式 fe（位于函数 fnID 内）第 idx 个返回值的来源：闭包字面量取其 return 表达式，
// 具名函数/方法值取其实现的返回值，其余不可反推。
func (s *Slicer) funcExprReturns(fnID string, fe ast.Expr, idx, depth int, c *flowCtx) {
	info := s.g.TypeInfoOfFunc(fnID)
	lit, ok := ast.Unparen(fe).(*ast.FuncLit)
	if !ok {
		fn := funcObjOf(fe, info)
		switch {
		case fn == nil:
			c.markOpaque(s.g, fe.Pos(), exprLabel(fe)) // 函数变量：函数体不可静态确定
		case !s.inModule(fn.Pkg()):
			c.markExternal(s.g, fe.Pos(), exprLabel(fe))
		default:
			s.implReturns([]string{codegraph.FuncID(fn)}, idx, depth, c, fe, fnID)
		}
		return
	}
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		if inner, ok := n.(*ast.FuncLit); ok && inner != lit {
			return false // 嵌套闭包的 return 不是本闭包的返回值
		}
		ret, ok := n.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		switch {
		case len(ret.Results) > 1 && idx < len(ret.Results):
			s.flowValues(fnID, ret.Results[idx], depth, c)
		case len(ret.Results) == 1 && idx == 0:
			s.flowValues(fnID, ret.Results[0], depth, c)
		case len(ret.Results) == 1:
			// `return f()` 透传多返回值
			if inner, ok := ast.Unparen(ret.Results[0]).(*ast.CallExpr); ok {
				s.callResults(fnID, inner, idx, depth+1, c)
			} else {
				c.markOpaque(s.g, ret.Pos(), exprLabel(ret.Results[0]))
			}
		default:
			c.markOpaque(s.g, ret.Pos(), "bare return")
		}
		return true
	})
}

// localFuncSources 局部函数变量 v 在函数体内的全部赋值右值（`f := func…` / `f = g` / `var f = …`）。
// 存在多值赋值等无法对应右值的情况时返回 nil（整体放弃，不以部分来源代表全部）。
func localFuncSources(body *ast.BlockStmt, info *types.Info, v *types.Var) []ast.Expr {
	var out []ast.Expr
	bad := false
	isV := func(e ast.Expr) bool {
		id, ok := ast.Unparen(e).(*ast.Ident)
		return ok && (info.Defs[id] == v || info.Uses[id] == v)
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch st := n.(type) {
		case *ast.AssignStmt:
			for i, l := range st.Lhs {
				if !isV(l) {
					continue
				}
				if len(st.Lhs) != len(st.Rhs) {
					bad = true
					continue
				}
				out = append(out, st.Rhs[i])
			}
		case *ast.ValueSpec:
			for i, name := range st.Names {
				if info.Defs[name] != v {
					continue
				}
				if len(st.Values) != len(st.Names) {
					bad = true
					continue
				}
				out = append(out, st.Values[i])
			}
		case *ast.UnaryExpr:
			if st.Op == token.AND && isV(st.X) {
				bad = true // 取址后可能经指针改写
			}
		}
		return true
	})
	if bad {
		return nil
	}
	return out
}

// fieldWrite 一个值来源：值表达式及其所在函数（未导出字段的写入点、函数形参的调用点实参）。
type fieldWrite struct {
	fn    string   // 值表达式所在函数符号 ID
	value ast.Expr // 值表达式；nil = 字面量未给出该字段（零值）
}

// fieldWriteValues 未导出的仓库内字段 field 的取值：Go 可见性保证只有声明包能写它，回溯包内全部写入值
// （字面量零值路径记 nil）。字段已导出、在仓库外、被取址或存在无法定位的写入时返回 false。
func (s *Slicer) fieldWriteValues(field *types.Var, depth int, c *flowCtx) bool {
	if field.Exported() || !s.inModule(field.Pkg()) {
		return false
	}
	writes, ok := s.fieldWritesOf(field)
	if !ok || len(writes) == 0 {
		return false
	}
	key := "field#" + field.Pkg().Path() + "." + field.Name() + "@" + itoa(int(field.Pos()))
	if c.visiting[key] {
		return true
	}
	c.visiting[key] = true
	sub := *c
	sub.callPos, sub.callExpr, sub.callerFn = token.NoPos, nil, ""
	for _, w := range writes {
		if w.value == nil {
			sub.out[flowNil] = true // 字面量未给出：零值（接口/指针为 nil）
			continue
		}
		s.flowValues(w.fn, w.value, depth+1, &sub)
	}
	return true
}

// fieldWritesOf 字段 field 在其声明包内的全部写入点（带缓存，并发安全）。ok=false 表示存在无法回溯的写入
// （取址、多值赋值、包级初始化）。
func (s *Slicer) fieldWritesOf(field *types.Var) ([]fieldWrite, bool) {
	s.fieldMu.Lock()
	defer s.fieldMu.Unlock()
	if s.fieldCache == nil {
		s.fieldCache = map[*types.Var]fieldWriteSet{}
	}
	if ws, hit := s.fieldCache[field]; hit {
		return ws.writes, ws.ok
	}
	ws := s.scanFieldWrites(field)
	s.fieldCache[field] = ws
	return ws.writes, ws.ok
}

// fieldWriteSet 字段写入点扫描结果。
type fieldWriteSet struct {
	writes []fieldWrite // 全部写入点（按源码顺序）
	ok     bool         // 全部写入点均可回溯
}

// scanFieldWrites 扫描字段声明包的全部语法树，收集键值/位置字面量与赋值语句中对 field 的写入。
func (s *Slicer) scanFieldWrites(field *types.Var) fieldWriteSet {
	var out fieldWriteSet
	out.ok = true
	for _, p := range s.g.Pkgs() {
		if p.Types != field.Pkg() {
			continue
		}
		info := p.TypesInfo
		for _, f := range p.Syntax {
			ast.Inspect(f, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CompositeLit:
					if v, found, keyed := literalFieldValue(x, info, field); found || keyed {
						out.add(s.g.EnclosingFunc(x.Pos()), v)
					}
				case *ast.AssignStmt:
					for i, l := range x.Lhs {
						sel, ok := ast.Unparen(l).(*ast.SelectorExpr)
						if !ok || !selectsField(sel, info, field) {
							continue
						}
						if len(x.Lhs) != len(x.Rhs) {
							out.ok = false
							continue
						}
						out.add(s.g.EnclosingFunc(x.Pos()), x.Rhs[i])
					}
				case *ast.UnaryExpr:
					if sel, ok := ast.Unparen(x.X).(*ast.SelectorExpr); ok && x.Op == token.AND && selectsField(sel, info, field) {
						out.ok = false // 取址：可能经指针写入
					}
				}
				return true
			})
		}
	}
	return out
}

// add 记录一次写入；不在任何函数体内（包级初始化）的写入无法回溯，整体放弃。
func (ws *fieldWriteSet) add(fn string, value ast.Expr) {
	if fn == "" {
		ws.ok = false
		return
	}
	ws.writes = append(ws.writes, fieldWrite{fn: fn, value: value})
}

// literalFieldValue 结构体字面量 lit 中 field 的值：found=给出了该字段；keyed=字面量类型含该字段但未给出（零值）。
func literalFieldValue(lit *ast.CompositeLit, info *types.Info, field *types.Var) (value ast.Expr, found, keyed bool) {
	t := info.TypeOf(lit)
	if t == nil {
		return nil, false, false
	}
	st, ok := derefType(t).Underlying().(*types.Struct)
	if !ok {
		return nil, false, false
	}
	pos := -1
	for i := 0; i < st.NumFields(); i++ {
		if st.Field(i) == field {
			pos = i
		}
	}
	if pos < 0 {
		return nil, false, false
	}
	for i, el := range lit.Elts {
		kv, isKV := el.(*ast.KeyValueExpr)
		if !isKV {
			if i == pos {
				return el, true, false // 位置字面量
			}
			continue
		}
		if id, ok := kv.Key.(*ast.Ident); ok && info.Uses[id] == field {
			return kv.Value, true, false
		}
	}
	return nil, false, true
}

// selectsField 选择表达式是否取 field（含嵌入提升的字段）。
func selectsField(sel *ast.SelectorExpr, info *types.Info, field *types.Var) bool {
	s, ok := info.Selections[sel]
	return ok && s.Kind() == types.FieldVal && s.Obj() == field
}
