package slicing

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"github.com/specforge/specforge/internal/codegraph"
)

// 值流反推：流入汇聚点某槽位的值，沿「局部定义 → 调用返回值 → 具体实现的 return 表达式」
// 回溯到叶子（设计文档 v2 §3.C 的 AST 版本，P1 由 SSA 替换）。
// 叶子如何解读由 flowLeaf 决定：类型流取具体静态类型，错误码流取错误构造调用的常量实参。

// maxValueFlowDepth 值流反推跨函数的最大跳数（handler → service → access 典型为 2–3 跳；经 errgroup 任务、
// 上游客户端封装再到 HTTP 请求助手可达 7–8 跳，实测加深到 8 对全量分析耗时无可见影响）。
const maxValueFlowDepth = 8

// flowNil 值流候选集中的哨兵：某条路径上取到字面量 nil。
const flowNil = "\x00nil"

// flowOpaque 值流候选集中的哨兵：某条路径无法继续反推（外部函数、反射、非常规表达式）。
const flowOpaque = "\x00opaque"

// flowUncoded 错误流哨兵：某条路径上的错误值由仓库外代码构造（errors.New、fmt.Errorf、三方库返回的 error），
// 不携带仓库的业务码——这是可静态确定的结论，而非不可反推。
const flowUncoded = "\x00uncoded"

// flowDynamic 错误码流哨兵：某条路径上的业务码取自运行时值（上游响应字段、外部函数返回的整数），
// 码的取值集合不可静态枚举，但来源确定。
const flowDynamic = "\x00dynamic"

// sentinelPrefix 全部哨兵的共同前缀（结果集中区分哨兵与常量符号）。
const sentinelPrefix = "\x00"

// flowMode 值流回溯追踪的值的语义（决定仓库外来源如何定性）。
type flowMode int

const (
	modeValue flowMode = iota // 类型流：仓库外来源不可反推
	modeErr                   // 错误值流：仓库外构造的 error 定性为「不带业务码」
	modeCode                  // 业务码流：来自运行时字段/外部函数的码定性为「动态码」
)

// untypedNil 与 ArgTypes 中字面量 nil 的类型串同口径：上层据此判定「无响应体」。
const untypedNil = "untyped nil"

// flowLeaf 叶子解读：e 为值表达式（多返回值调用的非首分量时为 nil），t 为该值的静态类型。
// 能在此终结回溯时把结果写入 out 并返回 true；返回 false 则继续沿值流回溯。
type flowLeaf func(e ast.Expr, t types.Type, info *types.Info, out map[string]bool) bool

// typeLeaf 类型流叶子：静态类型已具体（非接口）即终结。
func typeLeaf(_ ast.Expr, t types.Type, _ *types.Info, out map[string]bool) bool {
	if t == nil || types.IsInterface(t) {
		return false
	}
	addConcrete(out, t)
	return true
}

// errCodeLeaf 错误码流叶子：字面量 nil，或首个实参为常量的调用（`NewError(ErrX, ...)`）即终结。
func errCodeLeaf(e ast.Expr, t types.Type, info *types.Info, out map[string]bool) bool {
	if t != nil && isUntypedNil(t) {
		out[flowNil] = true
		return true
	}
	if e == nil {
		return false
	}
	if constID, ok := codegraph.ConstArgOfCall(e, info, 0); ok {
		out[constID] = true
		return true
	}
	return false
}

// constLeaf 错误码实参流叶子：常量标识符即终结（`errCode = code.ErrDatabase`）；字面量 nil 记 flowNil。
func constLeaf(e ast.Expr, t types.Type, info *types.Info, out map[string]bool) bool {
	if t != nil && isUntypedNil(t) {
		out[flowNil] = true
		return true
	}
	if e == nil {
		return false
	}
	if constID, ok := codegraph.ConstOf(e, info); ok {
		out[constID] = true
		return true
	}
	return false
}

// errorIface 内置 error 接口（判定调用是否返回错误值）。
var errorIface = types.Universe.Lookup("error").Type().Underlying().(*types.Interface)

// varCodeCtorArg 错误构造调用的「变量码」实参：`NewError(errCode, ...)` 返回 error 且首参为非常量整数时，
// 返回该实参（值流改为追踪码变量本身的取值）；否则返回 nil。
func varCodeCtorArg(call *ast.CallExpr, info *types.Info) ast.Expr {
	if len(call.Args) == 0 {
		return nil
	}
	rt := info.TypeOf(call)
	if rt == nil || !types.Implements(rt, errorIface) {
		return nil
	}
	arg := call.Args[0]
	if _, isConst := codegraph.ConstOf(arg, info); isConst {
		return nil
	}
	at := info.TypeOf(arg)
	if at == nil {
		return nil
	}
	b, ok := at.Underlying().(*types.Basic)
	if !ok || b.Info()&types.IsInteger == 0 {
		return nil
	}
	return arg
}

// flowCtx 一次值流回溯的共享状态。
type flowCtx struct {
	leaf      flowLeaf        // 叶子解读
	out       map[string]bool // 叶子结果集（含 flowNil/flowOpaque 哨兵）
	visiting  map[string]bool // 已访问的变量/返回值（防环）
	assertT   types.Type      // 经 `x.(T)` 断言进入时的目标类型；nil = 无断言约束
	opaqueAt  map[string]bool // 不可反推点「表达式@file:line」集合；nil = 不收集
	mode      flowMode        // 追踪的值语义；modeErr 遇到「变量码」错误构造调用时改以 modeCode 追踪码实参（见 varCodeCtorArg）
	uncodedAt map[string]bool // 不带业务码的错误构造点「表达式@file:line」集合（modeErr）
	dynamicAt map[string]bool // 动态业务码的取值点「表达式@file:line」集合（modeCode）
	callPos   token.Pos       // 当前所在函数被调入的调用点（顶层为 NoPos）
	callExpr  ast.Expr        // 当前所在函数被调入的调用表达式（归因用）
	callerFn  string          // callExpr 所在的函数符号 ID（函数类型形参取实参时回到调用方回溯）；空 = 未知
}

// refineAny 返回流入 any 槽实参的唯一具体类型串；所有路径都可证明为 nil 时返回 untypedNil；
// 多个候选（多态）或存在不可反推路径时返回空，由上层保持「不可定型」——宁缺毋滥，不臆测。
// 第二返回值为唯一类型的 go/types 表示（匿名结构体等无类型 ID 的类型据此合成 schema；未收窄为 nil）。
// 第三返回值：无唯一类型、但全部路径都可反推且候选均为命名类型时的闭合并集（有序；响应按 oneOf 渲染）。
func (s *Slicer) refineAny(site codegraph.CallSite, slot int) (string, types.Type, []string) {
	typesOf := map[string]types.Type{} // 类型串 → 类型（剥指针后同串取首个）
	leaf := func(e ast.Expr, t types.Type, info *types.Info, out map[string]bool) bool {
		if !typeLeaf(e, t, info, out) {
			return false
		}
		if t != nil && !isUntypedNil(t) {
			key := strings.TrimPrefix(t.String(), "*")
			if _, dup := typesOf[key]; !dup {
				typesOf[key] = derefType(t)
			}
		}
		return true
	}
	found, ok := s.traceSlot(site, slot, leaf)
	if !ok {
		return "", nil, nil
	}
	sawNil, opaque := found[flowNil], found[flowOpaque]
	delete(found, flowNil)
	delete(found, flowOpaque)
	// 混入零值或自增标量（`x++` 等非数据路径）的候选，在存在具体结构体/切片时剔除：
	// 这些标量不是成功的响应数据，是同一函数体内无关路径的返回值，不应阻碍收窄。
	if len(found) > 1 {
		for t := range found {
			if scalarInertType(t) {
				delete(found, t)
			}
		}
	}
	switch {
	case opaque:
		return "", nil, nil
	case len(found) == 1:
		for t := range found {
			return t, typesOf[strings.TrimPrefix(t, "*")], nil
		}
	}
	// 指针与值同型（`*T` 与 `T`）序列化一致：剥去指针后若塌缩为唯一类型即收窄，返回值的形态。
	if rt := uniqueDeref(found); rt != "" {
		return rt, typesOf[rt], nil
	}
	if len(found) == 0 && sawNil {
		return untypedNil, nil, nil
	}
	return "", nil, closedUnion(found)
}

// maxDataAlts 闭合并集的最大变体数：超出视为过度多态（多半是泛化的分发器），不渲染 oneOf。
const maxDataAlts = 8

// closedUnion 候选类型（已剔除哨兵）剥指针去重后均为可登记的命名类型时返回其有序集合；否则 nil。
func closedUnion(found map[string]bool) []string {
	set := map[string]bool{}
	for t := range found {
		id := normalizeTypeID(t)
		if id == "" {
			return nil // 匿名类型/基础类型/容器：无法作为独立变体登记
		}
		set[id] = true
	}
	if len(set) < 2 || len(set) > maxDataAlts {
		return nil
	}
	return sortedKeys(set)
}

// derefType 剥去一层指针。
func derefType(t types.Type) types.Type {
	if p, ok := t.(*types.Pointer); ok {
		return p.Elem()
	}
	return t
}

// uniqueDeref 候选类型剥去一层指针后若唯一（`*T` 与 `T` 视为同型），返回非指针形态；否则空。
func uniqueDeref(found map[string]bool) string {
	var got string
	for t := range found {
		base := strings.TrimPrefix(t, "*")
		if got == "" {
			got = base
			continue
		}
		if got != base {
			return ""
		}
	}
	return got
}

// scalarInertType 类型是否为「不可作为响应数据的惰性标量」：基础数值/布尔、或这些标量的指针与切片。
// 当它与具体结构体/切片候选并存时（如 ExerciseHoldings 里 `return 0, …` 的 int 分支），视为噪声剔除。
func scalarInertType(t string) bool {
	t = strings.TrimPrefix(t, "*")
	t = strings.TrimPrefix(t, "[]")
	switch t {
	case "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64",
		"float32", "float64", "bool", "string":
		return true
	}
	return false
}

// errFlow 流入 err 槽的错误值的静态定性：每条路径归入常量码、不带业务码、动态码或不可反推之一。
type errFlow struct {
	consts    []string // 反推到的错误码常量符号（有序，已排除成功码）
	uncodedAt []string // 不带业务码的错误构造点「表达式@file:line」（有序）
	dynamicAt []string // 动态业务码的取值点「表达式@file:line」（有序）
	opaque    bool     // 存在无法静态定性的路径（LLM 兜底目标）
	opaqueAt  []string // 不可反推点「表达式@file:line」（有序），作为未解析错误行的来源证据
	onlyNil   bool     // 全部路径均为 nil：该写出点不会走失败分支
}

// errCodesOf 对 err 槽做错误值流回溯并定性各条路径（见 errFlow）。
func (s *Slicer) errCodesOf(site codegraph.CallSite, slot int) errFlow {
	if !slotPresent(slot, site.ArgSyms) || slot >= len(site.ArgExprs) || site.ArgExprs[slot] == nil {
		return errFlow{opaque: true}
	}
	c := &flowCtx{leaf: errCodeLeaf, out: map[string]bool{}, visiting: map[string]bool{}, mode: modeErr,
		opaqueAt: map[string]bool{}, uncodedAt: map[string]bool{}, dynamicAt: map[string]bool{}}
	s.flowValues(site.Caller, site.ArgExprs[slot], 0, c)
	ef := errFlow{opaque: c.out[flowOpaque], opaqueAt: sortedKeys(c.opaqueAt),
		uncodedAt: sortedKeys(c.uncodedAt), dynamicAt: sortedKeys(c.dynamicAt),
		onlyNil: c.out[flowNil] && len(c.out) == 1}
	success := 0
	if s.prof != nil && s.prof.ResponseEnvelope != nil {
		success = s.prof.ResponseEnvelope.SuccessCode
	}
	for k := range c.out {
		// 值为成功码的常量（`errCode = ErrSuccess`，由 `if errCode != ErrSuccess` 守卫排除）不是错误分支。
		if !strings.HasPrefix(k, sentinelPrefix) && s.codeOfConst(k) != success {
			ef.consts = append(ef.consts, k)
		}
	}
	sortStrings(ef.consts)
	return ef
}

// sortedKeys 集合的有序键。
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

// traceSlot 对汇聚点第 slot 个实参做值流回溯，返回叶子结果集。
func (s *Slicer) traceSlot(site codegraph.CallSite, slot int, leaf flowLeaf) (map[string]bool, bool) {
	if !slotPresent(slot, site.ArgSyms) || slot >= len(site.ArgExprs) || site.ArgExprs[slot] == nil {
		return nil, false
	}
	c := &flowCtx{leaf: leaf, out: map[string]bool{}, visiting: map[string]bool{}}
	s.flowValues(site.Caller, site.ArgExprs[slot], 0, c)
	return c.out, true
}

// flowValues 回溯表达式 e（位于函数 fnID 内）可能取到的值，叶子交给 leaf 解读。
func (s *Slicer) flowValues(fnID string, e ast.Expr, depth int, c *flowCtx) {
	info := s.g.TypeInfoOfFunc(fnID)
	fd := s.g.FuncDeclOf(fnID)
	if depth > maxValueFlowDepth || info == nil || fd == nil || fd.Body == nil {
		c.markOpaque(s.g, e.Pos(), "depth/body")
		return
	}
	e = ast.Unparen(e)
	if call, ok := e.(*ast.CallExpr); ok {
		s.callResults(fnID, call, 0, depth+1, c) // 调用由 callResults 取对应返回分量后交给叶子
		return
	}
	if c.leaf(e, info.TypeOf(e), info, c.out) {
		return
	}
	switch x := e.(type) {
	case *ast.TypeAssertExpr:
		s.flowValues(fnID, x.X, depth, c.asserting(info.TypeOf(x))) // `err.(*code.Error)` 透传被断言值
	case *ast.Ident:
		obj := info.Uses[x]
		if obj == nil {
			obj = info.Defs[x]
		}
		v, ok := obj.(*types.Var)
		if !ok {
			c.markOpaque(s.g, x.Pos(), x.Name)
			return
		}
		key := fnID + "#" + v.Name() + "@" + itoa(int(v.Pos()))
		if c.visiting[key] {
			return
		}
		c.visiting[key] = true
		s.assignedValues(fnID, fd, info, v, depth, c)
	case *ast.CompositeLit, *ast.UnaryExpr:
		// 由入参拼出的值（如 `&Error{Code: code}`）：不可反推的原因在调用方传参，归因到调用点。
		c.markOpaqueBubble(s.g, e.Pos(), exprLabel(e))
	case *ast.SelectorExpr:
		s.fieldValue(x, info, depth, c)
	default:
		c.markOpaque(s.g, e.Pos(), exprLabel(e)) // 索引、字段选择等：AST 版本不反推
	}
}

// addConcrete 记录一个静态类型候选（字面量 nil 记为 flowNil 哨兵）。
func addConcrete(out map[string]bool, t types.Type) {
	if isUntypedNil(t) {
		out[flowNil] = true
		return
	}
	out[t.String()] = true
}

// assignedValues 遍历函数体内对变量 v 的全部赋值/声明，逐个回溯右值；
// 无任何赋值（形参、闭包捕获）即不可反推。
func (s *Slicer) assignedValues(fnID string, fd *ast.FuncDecl, info *types.Info, v *types.Var, depth int, c *flowCtx) {
	isV := func(lhs ast.Expr) bool {
		id, ok := ast.Unparen(lhs).(*ast.Ident)
		if !ok {
			return false
		}
		return info.Defs[id] == v || info.Uses[id] == v
	}
	assigned := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		var lhs, rhs []ast.Expr
		switch st := n.(type) {
		case *ast.AssignStmt:
			lhs, rhs = st.Lhs, st.Rhs
		case *ast.ValueSpec:
			for _, name := range st.Names {
				lhs = append(lhs, name)
			}
			rhs = st.Values
		default:
			return true
		}
		for i, l := range lhs {
			if !isV(l) {
				continue
			}
			assigned = true
			switch {
			case len(rhs) == 0:
				c.out[flowNil] = true // `var x T`：零值（指针/接口为 nil）
			case len(rhs) == len(lhs):
				s.flowValues(fnID, rhs[i], depth, c)
			case len(rhs) == 1:
				switch r := ast.Unparen(rhs[0]).(type) {
				case *ast.CallExpr:
					s.callResults(fnID, r, i, depth+1, c)
				case *ast.TypeAssertExpr:
					if i == 0 { // `x, ok := y.(T)`：x 取断言值，ok 不属于本值流
						s.flowValues(fnID, r.X, depth, c.asserting(info.TypeOf(r.Type)))
					}
				default:
					c.markOpaque(s.g, r.Pos(), exprLabel(r)) // map 索引 / 通道接收的多值形式
				}
			}
		}
		return true
	})
	if !assigned {
		// 形参 / 闭包捕获：本函数内无赋值，值由调用方决定——归因到调用点。
		c.markOpaqueBubble(s.g, v.Pos(), v.Name())
	}
}

// callResults 调用第 idx 个返回值的来源：叶子可直接解读则终结，
// 否则解析到被调函数（接口方法 → 全部具体实现），回溯其 return 表达式。
func (s *Slicer) callResults(fnID string, call *ast.CallExpr, idx, depth int, c *flowCtx) {
	info := s.g.TypeInfoOfFunc(fnID)
	if info == nil {
		c.markOpaque(s.g, call.Pos(), exprLabel(call))
		return
	}
	var valueExpr ast.Expr // 多返回值的非首分量没有独立表达式
	if idx == 0 {
		valueExpr = call
	}
	if c.leaf(valueExpr, resultType(info.TypeOf(call), idx), info, c.out) {
		return
	}
	if depth > maxValueFlowDepth {
		// 超出跳数：叶子与仓库外被调方的定性无需再下钻，仍可给出结论；仓库内被调方才不可反推。
		if funcObjOf(call.Fun, info) != nil && !s.calleeInModule(call, info) {
			c.markExternal(s.g, call.Pos(), exprLabel(call))
		} else {
			c.markOpaque(s.g, call.Pos(), exprLabel(call))
		}
		return
	}
	if c.mode == modeErr && idx == 0 {
		if arg := varCodeCtorArg(call, info); arg != nil {
			// `NewError(errCode, ...)`：错误码由 errCode 的取值决定，改追踪码变量（常量叶子）。
			sub := *c
			sub.leaf, sub.mode = constLeaf, modeCode
			s.flowValues(fnID, arg, depth, &sub)
			return
		}
		if s.errgroupWait(fnID, call, depth, c) {
			return
		}
	}
	callee, viaIface := codegraph.ResolveCallee(call, info)
	if callee == "" {
		// 函数值调用：形参取调用方实参、局部变量取其赋值；仍不可确定才标不可反推。
		if !s.funcValueReturns(fnID, call, idx, depth, c) {
			c.markOpaque(s.g, call.Pos(), exprLabel(call))
		}
		return
	}
	if _, isWriter := s.writers[callee]; isWriter {
		// 框架原生写出器（c.JSON 等）的返回值只在序列化失败时非 nil，不是业务错误来源：
		// `return code.Response(c, err, nil)` 把写出结果当 err 回传时按 nil 处理，避免伪「未解析」。
		c.out[flowNil] = true
		return
	}
	impls := []string{callee}
	if viaIface || s.g.FuncDeclOf(callee) == nil {
		impls = s.g.ConcreteImplsOf(callee)
	}
	if len(impls) == 0 {
		if s.calleeInModule(call, info) {
			c.markOpaque(s.g, call.Pos(), exprLabel(call)) // 仓库内接口没有可见实现
		} else {
			c.markExternal(s.g, call.Pos(), exprLabel(call))
		}
	}
	s.implReturns(impls, idx, depth, c, call, fnID)
}

// implReturns 回溯各实现函数第 idx 个返回值的 return 表达式；via 为进入这些函数的调用表达式
// （调用或函数值，位于函数 callerFn 内），函数体内入参依赖型的不可反推归因到它。
func (s *Slicer) implReturns(impls []string, idx, depth int, c *flowCtx, via ast.Expr, callerFn string) {
	sub := *c
	sub.callPos, sub.callExpr, sub.callerFn = via.Pos(), via, callerFn
	for _, impl := range impls {
		c := &sub // 进入被调函数体：其内的入参依赖型不可反推归因到本调用点
		key := impl + "#ret" + itoa(idx)
		if c.visiting[key] {
			continue
		}
		c.visiting[key] = true
		fd := s.g.FuncDeclOf(impl)
		if fd == nil || fd.Body == nil {
			// 实现均来自仓库内类型（仓库外被调方在 callResults 已定性）：无函数体（汇编、嵌入接口）不可反推。
			c.markOpaque(s.g, via.Pos(), exprLabel(via))
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if _, ok := n.(*ast.FuncLit); ok {
				return false // 闭包的 return 不是本函数的返回值
			}
			ret, ok := n.(*ast.ReturnStmt)
			if !ok {
				return true
			}
			switch {
			case len(ret.Results) == 0:
				// 命名返回值的裸 return：回溯该命名结果变量在函数体内的全部赋值（未赋值路径为零值）。
				if v := namedResult(fd, s.g.TypeInfoOfFunc(impl), idx); v != nil {
					key := impl + "#named" + itoa(idx)
					if !c.visiting[key] {
						c.visiting[key] = true
						c.out[flowNil] = true
						s.assignedValues(impl, fd, s.g.TypeInfoOfFunc(impl), v, depth, c)
					}
				} else {
					c.markOpaque(s.g, ret.Pos(), "bare return")
				}
			case idx < len(ret.Results) && len(ret.Results) > 1:
				s.flowValues(impl, ret.Results[idx], depth, c)
			case len(ret.Results) == 1:
				// `return f()` 透传多返回值，或单返回值函数
				if inner, ok := ast.Unparen(ret.Results[0]).(*ast.CallExpr); ok && idx > 0 {
					s.callResults(impl, inner, idx, depth+1, c)
				} else if idx == 0 {
					s.flowValues(impl, ret.Results[0], depth, c)
				}
			}
			return true
		})
	}
}

// namedResult 函数第 idx 个命名结果变量；无命名结果或越界返回 nil。
func namedResult(fd *ast.FuncDecl, info *types.Info, idx int) *types.Var {
	if info == nil || fd.Type.Results == nil {
		return nil
	}
	i := 0
	for _, field := range fd.Type.Results.List {
		if len(field.Names) == 0 {
			return nil
		}
		for _, name := range field.Names {
			if i == idx {
				v, _ := info.Defs[name].(*types.Var)
				return v
			}
			i++
		}
	}
	return nil
}

// asserting 进入 `x.(T)` 断言分支的子上下文（共享结果集与防环表）。
func (c *flowCtx) asserting(t types.Type) *flowCtx {
	sub := *c
	sub.assertT = t
	return &sub
}

// markExternal 值来自仓库外函数（无函数体可回溯）：通常不可反推；但若经断言约束到
// 仓库内声明的具体类型，外部代码无从构造该类型（如 errors.Wrap 的结果断言为 *code.Error 必失败），
// 该路径在断言后不可达，直接剪除。
// 未经断言约束时按追踪语义定性：错误值流 → 不带业务码；业务码流 → 动态码；类型流 → 不可反推。
func (c *flowCtx) markExternal(g *codegraph.Graph, pos token.Pos, label string) {
	if c.assertT != nil && isModuleType(g, c.assertT) {
		return
	}
	switch c.mode {
	case modeErr:
		c.markClass(g, flowUncoded, c.uncodedAt, pos, label)
	case modeCode:
		c.markClass(g, flowDynamic, c.dynamicAt, pos, label)
	default:
		c.markOpaque(g, pos, label)
	}
}

// markClass 记录一条已定性路径：哨兵写入结果集，「表达式@file:line」写入对应来源集合（证据）。
func (c *flowCtx) markClass(g *codegraph.Graph, sentinel string, at map[string]bool, pos token.Pos, label string) {
	c.out[sentinel] = true
	if at != nil && pos.IsValid() {
		at[siteLabel(g, pos, label)] = true
	}
}

// maxOpaqueLabelLen 不可反推点标签（表达式源码摘要）的最大长度，防止报告/提示词被长表达式撑爆。
const maxOpaqueLabelLen = 60

// markOpaque 标记存在不可反推路径，并记录其源码位置与表达式摘要（证据：报告展示与 LLM 兜底定位）。
func (c *flowCtx) markOpaque(g *codegraph.Graph, pos token.Pos, label string) {
	c.out[flowOpaque] = true
	if c.opaqueAt == nil || !pos.IsValid() {
		return
	}
	c.opaqueAt[siteLabel(g, pos, label)] = true
}

// siteLabel 来源点标签「表达式摘要@file:line」（摘要超长截断）。
func siteLabel(g *codegraph.Graph, pos token.Pos, label string) string {
	p := g.Fset.Position(pos)
	if len(label) > maxOpaqueLabelLen {
		label = label[:maxOpaqueLabelLen] + "…"
	}
	return label + "@" + shortFileOf(p.Filename) + ":" + itoa(p.Line)
}

// markOpaqueBubble 入参依赖型不可反推：有调用点时记在调用点（`NewError(res.Code)@x.go:9`），
// 否则记在原位。
func (c *flowCtx) markOpaqueBubble(g *codegraph.Graph, pos token.Pos, label string) {
	if c.callExpr != nil {
		c.markOpaque(g, c.callPos, exprLabel(c.callExpr))
		return
	}
	c.markOpaque(g, pos, label)
}

// exprLabel 表达式的单行源码摘要（go/types 的表达式格式化，不读源文件）。
func exprLabel(e ast.Expr) string {
	return types.ExprString(e)
}

// isModuleType 类型（去指针）是否为被分析仓库内声明的命名类型。
func isModuleType(g *codegraph.Graph, t types.Type) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	if _, ok := t.(*types.Named); !ok {
		return false
	}
	return g.Type(codegraph.TypeIDOf(t)) != nil
}

// resultType 调用结果类型的第 idx 个分量（单返回值时 idx 只能为 0）。
func resultType(t types.Type, idx int) types.Type {
	if t == nil {
		return nil
	}
	if tup, ok := t.(*types.Tuple); ok {
		if idx < tup.Len() {
			return tup.At(idx).Type()
		}
		return nil
	}
	if idx == 0 {
		return t
	}
	return nil
}

// isUntypedNil 是否为字面量 nil 的类型（不贡献具体类型）。
func isUntypedNil(t types.Type) bool {
	b, ok := t.(*types.Basic)
	return ok && b.Kind() == types.UntypedNil
}
