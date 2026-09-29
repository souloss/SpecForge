package codegraph

import (
	"go/ast"
	"go/types"
	"strconv"
	"strings"
)

// valueIdentityOf 把实参表达式解析为稳定的「值身份」串:
//
//	标识符/选择器 → 符号 ID（常量/变量）
//	字面量        → lit:<value>
//	nil           → nil
//	函数调用      → call:<calleeID>
//	其它          → expr:<type>
func valueIdentityOf(e ast.Expr, info *types.Info) string {
	switch x := ast.Unparen(e).(type) {
	case *ast.Ident:
		if x.Name == "nil" {
			return "nil"
		}
		if obj := info.Uses[x]; obj != nil {
			return objID(obj)
		}
		if obj := info.Defs[x]; obj != nil {
			return "local:" + x.Name
		}
		return "ident:" + x.Name
	case *ast.SelectorExpr:
		if sel, ok := info.Selections[x]; ok && sel.Obj() != nil {
			return objID(sel.Obj())
		}
		if obj := info.Uses[x.Sel]; obj != nil {
			return objID(obj)
		}
	case *ast.BasicLit:
		return "lit:" + strconv.Quote(cleanLit(x.Value))
	case *ast.CallExpr:
		if callee, _ := resolveCallee(x, info); callee != "" {
			// 调用内的常量参数一并提取（错误码构造器模式）
			return "call:" + callee
		}
		return "call:?"
	}
	return "expr"
}

func cleanLit(v string) string { return strings.TrimSpace(v) }

func objID(obj types.Object) string {
	switch o := obj.(type) {
	case *types.Func:
		return methodIDOf(o)
	case *types.Var:
		if o.Pkg() != nil {
			return o.Pkg().Path() + "." + o.Name()
		}
		return "var:" + o.Name()
	case *types.Const:
		if o.Pkg() != nil {
			return o.Pkg().Path() + "." + o.Name()
		}
		return o.Name()
	}
	return obj.Name()
}

// ConstArgOfCall 解析 call 表达式中的常量实参符号 ID（错误码追踪用）。
// 返回形参位置 i 上的常量符号（若实参是 Ident/Selector 指向常量）。
func ConstArgOfCall(call ast.Expr, info *types.Info, argIdx int) (string, bool) {
	ce, ok := ast.Unparen(call).(*ast.CallExpr)
	if !ok || argIdx >= len(ce.Args) {
		return "", false
	}
	return ConstOf(ce.Args[argIdx], info)
}

// ConstOf 表达式若是指向常量的标识符（`ErrX` / `code.ErrX`），返回常量符号 ID。
func ConstOf(e ast.Expr, info *types.Info) (string, bool) {
	var id *ast.Ident
	switch x := ast.Unparen(e).(type) {
	case *ast.Ident:
		id = x
	case *ast.SelectorExpr:
		id = x.Sel
	default:
		return "", false
	}
	if c, ok := info.Uses[id].(*types.Const); ok {
		return constID(c), true
	}
	return "", false
}
