package slicing

import (
	"go/ast"
	"go/types"
	"path"

	"github.com/specforge/specforge/internal/codegraph"
)

// 错误值流的库语义与字段定性：让「仓库外构造的错误」「运行时取值的业务码」「errgroup 汇聚的错误」
// 得到确定的静态结论，而不是笼统的不可反推。

// errgroupPkgName errgroup 包的末段名（golang.org/x/sync/errgroup 及其同构分叉）。
const errgroupPkgName = "errgroup"

// errgroupType errgroup 的汇聚类型名。
const errgroupType = "Group"

// errgroupWait 返回首个非 nil 任务错误的方法名。
const errgroupWait = "Wait"

// errgroupSpawners 提交任务（func() error）的方法名。
var errgroupSpawners = map[string]bool{"Go": true, "TryGo": true}

// modulePkgs 被分析仓库内的包路径集合（loader 不加载依赖，Graph 的包即仓库包）。
func modulePkgs(g *codegraph.Graph) map[string]bool {
	out := map[string]bool{}
	for _, p := range g.Pkgs() {
		out[p.PkgPath] = true
	}
	return out
}

// inModule 包是否属于被分析仓库（universe 作用域的对象不属于）。
func (s *Slicer) inModule(pkg *types.Package) bool {
	return pkg != nil && s.modPkgs[pkg.Path()]
}

// calleeInModule 调用的被调对象（函数、方法或接口方法）是否声明在仓库内。
func (s *Slicer) calleeInModule(call *ast.CallExpr, info *types.Info) bool {
	obj := funcObjOf(call.Fun, info)
	return obj != nil && s.inModule(obj.Pkg())
}

// funcObjOf 函数值表达式（标识符、限定名、方法值）指向的函数对象；非函数返回 nil。
func funcObjOf(e ast.Expr, info *types.Info) *types.Func {
	switch x := ast.Unparen(e).(type) {
	case *ast.Ident:
		fn, _ := info.Uses[x].(*types.Func)
		return fn
	case *ast.SelectorExpr:
		if sel, ok := info.Selections[x]; ok {
			fn, _ := sel.Obj().(*types.Func)
			return fn
		}
		fn, _ := info.Uses[x.Sel].(*types.Func)
		return fn
	case *ast.IndexExpr: // 泛型实例化 F[T]
		return funcObjOf(x.X, info)
	case *ast.IndexListExpr:
		return funcObjOf(x.X, info)
	}
	return nil
}

// fieldValue 字段选择表达式的定性：业务码流中的字段取值是运行时值（动态码，如上游响应的 res.Error.Code）；
// 错误值流中仓库外类型的 error 字段（如 gorm 的 db.First(x).Error）是仓库外构造的错误；其余不可反推。
func (s *Slicer) fieldValue(x *ast.SelectorExpr, info *types.Info, depth int, c *flowCtx) {
	sel, ok := info.Selections[x]
	if !ok || sel.Kind() != types.FieldVal {
		c.markOpaque(s.g, x.Pos(), exprLabel(x)) // 包级变量等：AST 版本不反推
		return
	}
	if c.mode != modeCode {
		if field, ok := sel.Obj().(*types.Var); ok && s.fieldWriteValues(field, depth, c) {
			return // 未导出字段：写入点只在声明包内，回溯全部写入值
		}
	}
	switch {
	case c.mode == modeCode:
		c.markClass(s.g, flowDynamic, c.dynamicAt, x.Pos(), exprLabel(x))
	case c.mode == modeErr && !s.inModule(sel.Obj().Pkg()):
		c.markExternal(s.g, x.Pos(), exprLabel(x))
	case c.mode == modeErr && types.Implements(sel.Obj().Type(), errorIface):
		// 仓库内结构体的 error 类型字段（`tzymAsset.RequestError`）：值是仓库外/上游的裸 error，
		// 不带业务码；外层 handler 原样透传时，定性为「不带业务码」而非不可反推。
		c.markClass(s.g, flowUncoded, c.uncodedAt, x.Pos(), exprLabel(x))
	default:
		c.markOpaque(s.g, x.Pos(), exprLabel(x))
	}
}

// errgroupWait `g.Wait()` 的库摘要：其值为本函数内经 g.Go / g.TryGo 提交的全部任务返回值的并集（外加 nil）。
// 不是 errgroup.Wait，或本函数内找不到任何任务提交（g 跨函数传递）时返回 false，交由常规回溯。
func (s *Slicer) errgroupWait(fnID string, call *ast.CallExpr, depth int, c *flowCtx) bool {
	info, fd := s.g.TypeInfoOfFunc(fnID), s.g.FuncDeclOf(fnID)
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if info == nil || fd == nil || fd.Body == nil || !ok || sel.Sel.Name != errgroupWait || !isErrgroup(info.TypeOf(sel.X)) {
		return false
	}
	group := rootObj(sel.X, info)
	if group == nil {
		return false
	}
	var tasks []ast.Expr
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		sc, ok := n.(*ast.CallExpr)
		if !ok || len(sc.Args) != 1 {
			return true
		}
		if ss, ok := ast.Unparen(sc.Fun).(*ast.SelectorExpr); ok && errgroupSpawners[ss.Sel.Name] && rootObj(ss.X, info) == group {
			tasks = append(tasks, sc.Args[0])
		}
		return true
	})
	if len(tasks) == 0 {
		return false
	}
	c.out[flowNil] = true // 全部任务成功时 Wait 返回 nil
	for _, t := range tasks {
		s.taskReturns(fnID, t, depth+1, c)
	}
	return true
}

// taskReturns 回溯一个 errgroup 任务的返回值：闭包取其 return 表达式，具名函数/方法值取其实现的返回值。
func (s *Slicer) taskReturns(fnID string, task ast.Expr, depth int, c *flowCtx) {
	s.funcExprReturns(fnID, task, 0, depth, c)
}

// isErrgroup 类型（去指针）是否为 errgroup.Group。
func isErrgroup(t types.Type) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return false
	}
	return named.Obj().Name() == errgroupType && path.Base(named.Obj().Pkg().Path()) == errgroupPkgName
}

// rootObj 接收者表达式对应的对象（局部变量或字段），用于判定 g.Go 与 g.Wait 是否作用于同一个 group。
func rootObj(e ast.Expr, info *types.Info) types.Object {
	switch x := ast.Unparen(e).(type) {
	case *ast.Ident:
		if obj := info.Uses[x]; obj != nil {
			return obj
		}
		return info.Defs[x]
	case *ast.SelectorExpr:
		return info.Uses[x.Sel]
	}
	return nil
}
