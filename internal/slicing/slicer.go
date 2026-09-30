// Package slicing 实现程序切片与响应汇聚点追踪
// （设计文档 §6.1 步骤 5、§9.1 ShapeClosure、F5 响应契约推断）。
//
// 核心思想: 响应契约不在 handler 里，在调用图里。
// 正向可达集 ∩（到达响应汇聚点的反向可达集）→ 最小分析切片。
package slicing

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"sort"
	"strings"
	"sync"

	"github.com/specforge/specforge/internal/adapter"
	"github.com/specforge/specforge/internal/codegraph"
	"github.com/specforge/specforge/internal/profile"
)

// MaxDepth 切片调用深度上限（设计文档 §9.1 D=6）。
// 真实分层仓库（controller→interface→service→access 多层）需要更深穿透，
// 否则正向可达闭包到不了 service 层的响应汇聚点，导致「no response sink reached」。
const MaxDepth = 12

// SinkHit 一次响应写出点的解析结果。
type SinkHit struct {
	Site          codegraph.CallSite // WriteResponse 调用点
	Status        int                // HTTP 状态码（画像给出）
	Success       bool               // err 槽为 nil → 成功信封
	DataTypeID    string             // data 槽静态类型 ID（nil/unknown 时为空）
	DataUnknown   bool               // any/interface{}: 无法定型
	DataMapValue  string             // data 槽为 map[K]V 且 V 为基础类型时，V 的 JSON schema 类型（integer/string/...）；空 = 非 map 或值不可定型
	DataInline    types.Type         // data 槽经值流收窄到的匿名结构体类型（无类型 ID，由合成器直接出 schema）；nil = 无
	DataAlts      []string           // data 槽的闭合并集（全部路径可反推、候选均为命名类型）；analyzeHits 展开为逐变体行
	DataUnion     bool               // 本行是闭合并集中的一个变体（不再做值级收窄）
	HasBody       bool
	ErrConstID    string   // 错误码常量符号（可解析时）
	ErrConstIDs   []string // err 变量经值流反推到的全部错误码常量（analyzeHits 据此展开为逐码行）
	ErrCallCallee string   // 错误构造器调用（如 code.NewDefaultError）
	ErrUnresolved bool     // err 为变量: 信封码不可静态解析
	ErrUncoded    bool     // 错误行：错误值由仓库外构造（errors.New、三方库 error），不带业务码，包装器原样序列化
	ErrDynamic    bool     // 错误行：业务码取自运行时值（上游响应字段等），取值集合不可静态枚举
	uncodedAt     string   // err 变量中不带业务码路径的构造点（逗号分隔；展开为 ErrUncoded 行的来源证据）
	dynamicAt     string   // err 变量中动态码路径的取值点（逗号分隔；展开为 ErrDynamic 行的来源证据）
	ErrSource     string   // err 变量的来源证据（定义点的调用符号 + file:line）
	HandlerPath   []string // 从 handler 到 sink 的调用路径（证据链）
	EnvelopeType  string   // 真实信封结构体类型 ID（自动发现的包装器）；空 = profile 信封或无信封
	DataField     string   // 信封中承载 data 的字段 JSON 名
	ErrField      string   // 信封中承载错误对象的字段 JSON 名
	Raw           bool     // 原生写出：data 即响应体，无信封
	FixedCode     *int     // 信封中的固定业务码（包装器分支常量，如失败分支 "code": 500）；nil = 按常规推导
	ErrIsVar      bool     // err 槽是变量（同一调用点可能分流成功/失败）
	ErrorBranch   bool     // 原生写出位于 `if err != nil {}` 错误分支内（无 err 槽时据此判定为失败行）
	ParamFed      bool     // 原生写出的响应体直接取自所在函数的形参：该函数是未被识别的响应包装器
	Learned       bool     // 汇聚点模式来自 LLM 包装器摘要（行置信度按 symbol 级封顶）
}

// Slicer 响应追踪器。
type Slicer struct {
	g       *codegraph.Graph
	prof    *profile.Profile
	writers map[string]adapter.Writer // 框架原生写出器（原语表），按符号索引
	modPkgs map[string]bool           // 仓库内包路径（区分仓库内外的函数与类型）

	fieldMu    sync.Mutex                   // 保护 fieldCache（handler 可并发切片）
	fieldCache map[*types.Var]fieldWriteSet // 未导出字段 → 声明包内的写入点（惰性扫描）
}

// New 创建追踪器；writers 为框架原生写出器原语（状态码实参、值流中的写出结果语义）。
func New(g *codegraph.Graph, prof *profile.Profile, writers []adapter.Writer) *Slicer {
	return &Slicer{g: g, prof: prof, writers: writerIndex(writers), modPkgs: modulePkgs(g)}
}

// Trace 对一个 handler 做正向可达闭包内的汇聚点扫描。
//
// handler 可能是接口方法（真实仓库 handler 常经接口分发到 service 实现）。
// 此时 g.ForwardReach(handler) 只能走到接口方法自身（无函数体、无调用边），
// 需先把接口方法解析到全部具体实现，再对每个实现做可达闭包与 sink 扫描，
// 证据链标记为经接口分发。
func (s *Slicer) Trace(handlerID string) []SinkHit {
	roots := []string{handlerID}
	if s.g.FuncDeclOf(handlerID) == nil {
		impls := s.g.ConcreteImplsOf(handlerID)
		roots = append(roots, impls...)
	}
	var hits []SinkHit
	for _, root := range roots {
		hits = append(hits, s.traceFrom(root, handlerID)...)
	}
	return hits
}

// traceFrom 从单个根符号做正向可达闭包内的 sink 扫描。
func (s *Slicer) traceFrom(root, handlerID string) []SinkHit {
	reach := s.g.ForwardReach(root, MaxDepth)
	var hits []SinkHit
	// 确定性: 按 (函数, 行号) 排序遍历
	var fns []string
	for fn := range reach {
		fns = append(fns, fn)
	}
	sortStrings(fns)
	for _, fn := range fns {
		for _, callee := range sortedCallees(s.g, fn) {
			if _, ok := s.prof.IsSink(callee); !ok {
				continue
			}
			for _, site := range s.g.Callers[callee] {
				if site.Caller != fn {
					continue
				}
				// 包装器内部的写出由包装器调用点按摘要解析，不重复计入
				if _, inWrapper := s.prof.IsSink(site.Caller); inWrapper {
					continue
				}
				hits = append(hits, s.analyzeHits(site, reach, handlerID)...)
			}
		}
	}
	return hits
}

// analyzeHits 解析单个汇聚点调用；包装器按 err 分流且 err 为变量、data 非 nil 时，
// 同一调用点同时承载成功与失败两条路径（如 `return WriteRes(c, e, result)`），拆成两行。
// err 变量经值流反推出的每个错误码再展开为独立的失败行。
func (s *Slicer) analyzeHits(site codegraph.CallSite, reach map[string]int, handler string) []SinkHit {
	hit := s.analyzeHit(site, reach, handler)
	if !hit.ErrUnresolved && len(hit.ErrConstIDs) == 0 && hit.uncodedAt == "" && hit.dynamicAt == "" &&
		!(hit.ErrIsVar && hit.FixedCode != nil) {
		return expandDataAlts([]SinkHit{hit})
	}
	pat, _ := s.prof.IsSink(site.Callee)
	var out []SinkHit
	failure := hit
	if pat.BranchOnErr && slotPresent(pat.DataSlot, site.ArgSyms) && site.ArgSyms[pat.DataSlot] != "nil" {
		success := hit
		success.Success, success.ErrUnresolved, success.ErrSource, success.ErrConstIDs = true, false, "", nil
		success.FixedCode, success.EnvelopeType = pat.SuccessCode, pat.EnvelopeType
		out = append(out, success)
		failure.DataTypeID, failure.DataUnknown, failure.DataMapValue, failure.HasBody = "", false, "", false
		failure.DataAlts, failure.DataInline = nil, nil
	}
	return expandDataAlts(append(out, expandErrCodes(failure)...))
}

// expandDataAlts 闭合并集的 data 展开为逐变体的写出行（同状态码多变体由编译器渲染为 oneOf）。
func expandDataAlts(hits []SinkHit) []SinkHit {
	out := make([]SinkHit, 0, len(hits))
	for _, h := range hits {
		if len(h.DataAlts) == 0 {
			out = append(out, h)
			continue
		}
		for _, alt := range h.DataAlts {
			v := h
			v.DataAlts, v.DataUnion = nil, true
			v.DataTypeID, v.DataUnknown, v.HasBody = alt, false, true
			out = append(out, v)
		}
	}
	return out
}

// expandErrCodes 失败行按反推到的错误码逐个展开，不带业务码 / 动态码路径各展开为一行；
// 存在不可反推路径时额外保留一条未解析行。
func expandErrCodes(h SinkHit) []SinkHit {
	if len(h.ErrConstIDs) == 0 && h.uncodedAt == "" && h.dynamicAt == "" {
		return []SinkHit{h}
	}
	var out []SinkHit
	base := h
	base.ErrConstIDs, base.ErrUnresolved, base.uncodedAt, base.dynamicAt = nil, false, "", ""
	for _, id := range h.ErrConstIDs {
		c := base
		c.ErrConstID = id
		out = append(out, c)
	}
	if h.uncodedAt != "" {
		c := base
		c.ErrUncoded, c.ErrSource = true, h.uncodedAt
		out = append(out, c)
	}
	if h.dynamicAt != "" {
		c := base
		c.ErrDynamic, c.ErrSource = true, h.dynamicAt
		out = append(out, c)
	}
	if h.ErrUnresolved {
		h.ErrConstIDs = nil
		out = append(out, h)
	}
	return out
}

// isAnyType 静态类型串是否为空接口（any/interface{}）。
func isAnyType(t string) bool {
	return t == "interface{}" || t == "any" || strings.HasSuffix(t, "any")
}

// slotPresent 槽位在实参范围内（NoSlot 或越界均为不存在）。
func slotPresent(slot int, args []string) bool {
	return slot >= 0 && slot < len(args)
}

// analyzeHit 解析单个汇聚点调用: data 槽类型 + err 槽语义。
func (s *Slicer) analyzeHit(site codegraph.CallSite, reach map[string]int, handler string) SinkHit {
	pat, _ := s.prof.IsSink(site.Callee)
	hit := SinkHit{Site: site, Status: pat.Status,
		EnvelopeType: pat.EnvelopeType, DataField: pat.DataField, ErrField: pat.ErrField, Raw: pat.Raw}
	hit.Learned = pat.Learned
	if st, ok := s.constStatus(site); ok {
		hit.Status = st
	}
	if pat.ErrSlot == profile.NoSlot {
		hit.Success = true // 无 err 槽：恒为成功写出
	}

	// ---- data 槽 ----
	if slotPresent(pat.DataSlot, site.ArgTypes) {
		t := site.ArgTypes[pat.DataSlot]
		var refined types.Type
		if isAnyType(t) {
			// any 槽：沿值流反推具体类型（反推失败保持 any → 不可定型）
			rt, rtT, alts := s.refineAny(site, pat.DataSlot)
			if rt != "" {
				t, refined = rt, rtT
			}
			hit.DataAlts = alts
		}
		switch {
		case t == untypedNil || t == "nil":
			hit.HasBody = false
		case isAnyType(t):
			// §7.2.3: any 静态不可定型
			if sym := site.ArgSyms[pat.DataSlot]; strings.HasPrefix(sym, "call:") {
				// data 是函数调用: 从返回类型再看一次（实参表达式的类型
				// 在 ArgTypes 里已是表达式静态类型，any 变量才是 any）
				hit.DataUnknown = true
			} else {
				hit.DataUnknown = true
			}
		default:
			id := normalizeTypeID(t)
			if id != "" {
				hit.DataTypeID = id
				hit.HasBody = true
			} else {
				// map[K]V 且 V 为基础类型时，静态可定型为 additionalProperties；
				// 否则才标 DataUnknown（真 any）。
				if mv, ok := mapValueType(t); ok {
					hit.DataMapValue = mv
					hit.HasBody = true
				} else if isAnonStruct(refined) {
					hit.DataInline = refined // 匿名结构体（如上游响应的 `Result struct{…}`）：结构静态确定
					hit.HasBody = true
				} else {
					hit.DataUnknown = true
				}
			}
		}
		// 切片数据响应: []pkg.T → 数组 schema（设计文档 §7.1）
		if strings.HasPrefix(t, "[]") {
			if elem := normalizeTypeID(strings.TrimPrefix(t, "[]")); elem != "" {
				hit.DataTypeID = "[]" + elem
				hit.HasBody = true
				hit.DataUnknown = false
			}
		}
	}

	if len(hit.DataAlts) > 0 {
		hit.DataUnknown, hit.HasBody = false, true // 闭合并集：结构静态确定（oneOf）
	}
	if hit.Success && pat.SuccessCode != nil {
		hit.FixedCode = pat.SuccessCode
	}
	if pat.Raw {
		hit.ErrorBranch = s.inErrorBranch(site)
		hit.ParamFed = s.paramFed(site, pat.DataSlot)
	}

	// ---- err 槽 ----
	if slotPresent(pat.ErrSlot, site.ArgSyms) {
		sym := site.ArgSyms[pat.ErrSlot]
		switch {
		case sym == "nil":
			hit.Success = true
		case strings.HasPrefix(sym, "call:"):
			hit.ErrCallCallee = strings.TrimPrefix(sym, "call:")
			// 从调用表达式中提取常量参数（错误码目录联动）；非常量构造（`g.Wait()`、`errors.New(..)`、
			// `NewError(res.Code, ..)`）与变量同样交给值流定性，不因缺常量而丢失失败行。
			if constID, ok := s.constFromErrCall(site.ArgExprs[pat.ErrSlot]); ok {
				hit.ErrConstID = constID
			} else {
				s.classifyErr(&hit, site, pat)
			}
		default:
			s.classifyErr(&hit, site, pat)
			hit.ErrIsVar = !hit.Success
		}
		if !hit.Success && pat.ErrEnvelopeType != "" {
			hit.EnvelopeType = pat.ErrEnvelopeType
		}
		if !hit.Success && pat.FailureCode != nil {
			// 包装器失败分支的信封码是常量（如 gin.H{"code": 500, ...}）：与 err 的具体值无关。
			hit.FixedCode = pat.FailureCode
			hit.ErrConstID, hit.ErrConstIDs, hit.ErrUnresolved, hit.ErrSource = "", nil, false, ""
			hit.uncodedAt, hit.dynamicAt = "", ""
		}
	}
	return hit
}

// classifyErr err 实参的值流定性：每条路径归为常量码 / 不带业务码 / 动态码 / 不可反推；
// 只有不可反推的路径保留「未解析」行（LLM 兜底目标），不因部分命中而静默丢失。
func (s *Slicer) classifyErr(hit *SinkHit, site codegraph.CallSite, pat profile.SinkPattern) {
	ef := s.errCodesOf(site, pat.ErrSlot)
	if ef.onlyNil {
		hit.Success = true // err 恒为 nil（如被调方只 `return x, nil`）：只有成功写出，不是未解析错误
		return
	}
	if !pat.ErrRaw && len(ef.uncodedAt) > 0 {
		// 包装器不是把 err 原样放进信封（而是派生码/消息）：不带业务码的错误会渲染成什么不可静态确定。
		ef.opaque, ef.opaqueAt, ef.uncodedAt = true, append(ef.opaqueAt, ef.uncodedAt...), nil
		sortStrings(ef.opaqueAt)
	}
	hit.ErrSource = s.traceErrSource(site, pat.ErrSlot)
	if len(ef.opaqueAt) > 0 {
		// 值流给出的不可反推点比局部溯源更精确（跨函数定位到动态错误的构造处）。
		hit.ErrSource = joinSites(ef.opaqueAt)
	}
	hit.ErrConstIDs = ef.consts
	hit.uncodedAt, hit.dynamicAt = joinSites(ef.uncodedAt), joinSites(ef.dynamicAt)
	hit.ErrUnresolved = ef.opaque || len(ef.consts)+len(ef.uncodedAt)+len(ef.dynamicAt) == 0
}

// inErrorBranch 写出点是否位于所在函数内 `if <error 值> != nil { ... }` 的 then 分支。
func (s *Slicer) inErrorBranch(site codegraph.CallSite) bool {
	fd, info := s.g.FuncDeclOf(site.Caller), s.g.TypeInfoOfFunc(site.Caller)
	if fd == nil || fd.Body == nil || info == nil || len(site.ArgExprs) == 0 || site.ArgExprs[0] == nil {
		return false
	}
	pos := site.ArgExprs[0].Pos()
	found := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || found {
			return !found
		}
		if pos < ifs.Body.Pos() || pos >= ifs.Body.End() {
			return true
		}
		if be, ok := ast.Unparen(ifs.Cond).(*ast.BinaryExpr); ok && be.Op == token.NEQ {
			for _, side := range []ast.Expr{be.X, be.Y} {
				if t := info.TypeOf(side); t != nil && types.IsInterface(t) && types.Identical(t.Underlying(), errorIface) {
					found = true
				}
			}
		}
		return true
	})
	return found
}

// paramFed 写出的响应体是否由所在函数的「值形参」构成（直接透传、放进字面量、经 m["k"]=形参 赋值、
// 或作为辅助函数实参）——命中说明该函数是没被识别出来的响应包装器，调用点实参类型会丢失。
// 框架上下文等仓库外命名类型的形参（*gin.Context、*fiber.Ctx）与方法接收者位置不计。
func (s *Slicer) paramFed(site codegraph.CallSite, slot int) bool {
	fd, info := s.g.FuncDeclOf(site.Caller), s.g.TypeInfoOfFunc(site.Caller)
	if fd == nil || fd.Body == nil || info == nil || fd.Type.Params == nil || slot < 0 || slot >= len(site.ArgExprs) {
		return false
	}
	params := paramIndex(fd, info)
	exprs := []ast.Expr{stripAddr(site.ArgExprs[slot])}
	if id, ok := exprs[0].(*ast.Ident); ok {
		obj := info.Uses[id]
		if init := localInitOf(fd.Body, obj, info); init != nil {
			exprs = append(exprs, init)
		}
		exprs = append(exprs, assignedParts(fd.Body, obj, info)...)
	}
	for _, e := range exprs {
		if s.mentionsValueParam(e, info, params) {
			return true
		}
	}
	return false
}

// assignedParts 对局部变量 obj 的成员赋值右值（`m["k"] = v`、`r.F = v`）。
func assignedParts(body *ast.BlockStmt, obj types.Object, info *types.Info) []ast.Expr {
	var out []ast.Expr
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != len(as.Rhs) {
			return true
		}
		for i, lhs := range as.Lhs {
			var base ast.Expr
			switch l := lhs.(type) {
			case *ast.IndexExpr:
				base = l.X
			case *ast.SelectorExpr:
				base = l.X
			}
			if id, ok := ast.Unparen(base).(*ast.Ident); ok && info.Uses[id] == obj {
				out = append(out, as.Rhs[i])
			}
		}
		return true
	})
	return out
}

// mentionsValueParam 表达式中是否以「值」身份出现值形参（选择器/方法接收者位置除外）。
func (s *Slicer) mentionsValueParam(e ast.Expr, info *types.Info, params map[types.Object]int) bool {
	receivers := map[*ast.Ident]bool{}
	ast.Inspect(e, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := ast.Unparen(sel.X).(*ast.Ident); ok {
				receivers[id] = true
			}
		}
		return true
	})
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || found || receivers[id] {
			return !found
		}
		obj := info.Uses[id]
		if _, isParam := params[obj]; isParam && s.isValueParamType(obj.Type()) {
			found = true
		}
		return true
	})
	return found
}

// isValueParamType 形参类型是否承载响应「值」：接口（any/error）、基础类型或仓库内类型；
// 仓库外的命名类型（框架上下文、http.ResponseWriter 等）不算。
func (s *Slicer) isValueParamType(t types.Type) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return true // 接口字面量、基础类型、universe 的 error
	}
	return isModuleType(s.g, named)
}

// maxErrSourceSites 单个未解析错误行最多列出的不可反推来源点数（控制报告与提示词体积）。
const maxErrSourceSites = 5

// joinSites 来源点列表（截断到 maxErrSourceSites）拼为证据串；空列表为空串。
func joinSites(sites []string) string {
	return strings.Join(capStrings(sites, maxErrSourceSites), ", ") // 与 facts.SplitSites 的分隔口径一致
}

// capStrings 截取前 n 个元素（不足 n 原样返回）。
func capStrings(ss []string, n int) []string {
	if len(ss) > n {
		return ss[:n]
	}
	return ss
}

// constStatus 原生写出器带状态码实参（如 gin c.JSON(http.StatusCreated, x)）且实参为整数常量时返回其值。
func (s *Slicer) constStatus(site codegraph.CallSite) (int, bool) {
	w, ok := s.writers[site.Callee]
	if !ok || w.StatusArg == adapter.NoArg || w.StatusArg >= len(site.ArgExprs) {
		return 0, false
	}
	info := s.g.TypeInfoOfFunc(site.Caller)
	if info == nil {
		return 0, false
	}
	tv, ok := info.Types[site.ArgExprs[w.StatusArg]]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.Int {
		return 0, false
	}
	v, exact := constant.Int64Val(tv.Value)
	return int(v), exact
}

// traceErrSource 对 err 变量做保守的局部数据流：定位其在 sink 之前最近一次赋值的来源。
//
// 只处理两种确定性来源，其余显式返回空（交由上层按未解析降级，绝不臆测）：
//  1. `if err = g.Wait(); err != nil { return code.Response(c, err, nil) }` ——
//     来源为 errgroup.Wait，错误码不可静态确定（返回 "errgroup.Wait" 标记）。
//  2. 直接 `err := call(...)` 且该 call 返回 error —— 错误码来自被调函数内部。
//
// 返回人类可读的来源证据串（如 "errgroup.Wait@ipoServer.go:233"），
// 供 report 展示；本函数只负责溯源，不负责解析出具体错误码。
func (s *Slicer) traceErrSource(site codegraph.CallSite, errSlot int) string {
	fd := s.funcDeclOf(site.Caller)
	if fd == nil || fd.Body == nil {
		return ""
	}
	sinkLine := site.Line
	var errName string
	if errSlot < len(site.ArgExprs) {
		if id, ok := ast.Unparen(site.ArgExprs[errSlot]).(*ast.Ident); ok {
			errName = id.Name
		}
	}
	if errName == "" {
		return ""
	}
	var source string
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if source != "" {
			return false
		}
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		if s.g.Fset.Position(as.Pos()).Line > sinkLine {
			return false
		}
		for i, lhs := range as.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok || id.Name != errName || i >= len(as.Rhs) {
				continue
			}
			rhs := as.Rhs[i]
			if call, ok := ast.Unparen(rhs).(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Wait" {
					source = "errgroup.Wait"
				} else {
					source = "call"
				}
			}
		}
		return true
	})
	if source == "" {
		return ""
	}
	return source + "@" + shortFileOf(site.File) + ":" + itoa(sinkLine)
}

// funcDeclOf 定位符号 ID 对应的函数声明。
func (s *Slicer) funcDeclOf(symbolID string) *ast.FuncDecl {
	return s.g.FuncDeclOf(symbolID)
}

// shortFileOf 取文件名（无目录）。
func shortFileOf(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// itoa 小整数转字符串（避免引入 strconv 全量）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// constFromErrCall 从错误构造器调用中提取常量实参。
func (s *Slicer) constFromErrCall(expr ast.Expr) (string, bool) {
	call, ok := ast.Unparen(expr).(*ast.CallExpr)
	if !ok || len(call.Args) == 0 {
		return "", false
	}
	info := s.typeInfoOf(call)
	if info == nil {
		return "", false
	}
	return codegraph.ConstArgOfCall(expr, info, 0)
}

// typeInfoOf 通过文件路径定位所属包的类型信息。
func (s *Slicer) typeInfoOf(n ast.Node) *types.Info {
	pos := s.g.Fset.Position(n.Pos())
	if !pos.IsValid() {
		return nil
	}
	for _, p := range s.g.Pkgs() {
		for _, f := range p.Syntax {
			if s.g.Fset.Position(f.Pos()).Filename == pos.Filename {
				return p.TypesInfo
			}
		}
	}
	return nil
}

// ---- 错误码目录 ---------------------------------------------------------
// BuildErrorCatalog 扫描 profile 指定的错误码包（F7 横切）。
func (s *Slicer) BuildErrorCatalog() map[string]ErrorCodeEntry {
	out := map[string]ErrorCodeEntry{}
	src := s.prof.ErrorCodesSource
	for id, sym := range s.g.Syms {
		if sym.Kind != codegraph.KindConst {
			continue
		}
		if src != "" && !strings.HasSuffix(id, "/"+src+".") && !strings.Contains(id, "/"+src+"/") {
			// 只收录错误码包的常量（末段包名匹配）
			if !strings.HasSuffix(strings.TrimSuffix(id, "."+lastSeg(id)), src) {
				continue
			}
		}
		v, ok := s.g.ConstValueOf(id)
		if !ok {
			continue
		}
		out[id] = ErrorCodeEntry{
			Symbol: id, Name: lastSeg(id),
			Code: parseCodeValue(v),
			Msg:  s.errMsgOf(id),
		}
	}
	return out
}

// ErrorCodeEntry 错误码条目。
type ErrorCodeEntry struct {
	Symbol string
	Name   string
	Code   int
	Msg    string
}

// errMsgOf 从错误码包的 map[Code]string 字面量提取 msg（静态证据）。
func (s *Slicer) errMsgOf(constID string) string {
	constName := lastSeg(constID)
	for _, p := range s.g.Pkgs() {
		for _, f := range p.Syntax {
			for _, d := range f.Decls {
				gd, ok := d.(*ast.GenDecl)
				if !ok {
					continue
				}
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok || len(vs.Values) == 0 {
						continue
					}
					cl, ok := vs.Values[0].(*ast.CompositeLit)
					if !ok {
						continue
					}
					for _, elt := range cl.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if id, ok := kv.Key.(*ast.Ident); ok && id.Name == constName {
							if lit, ok := kv.Value.(*ast.BasicLit); ok {
								return trimQuotes(lit.Value)
							}
						}
					}
				}
			}
		}
	}
	return ""
}

// ---- 辅助 --------------------------------------------------------------

// isAnonStruct 类型是否为匿名（非命名）结构体。
func isAnonStruct(t types.Type) bool {
	if t == nil {
		return false
	}
	_, ok := types.Unalias(t).(*types.Struct)
	return ok
}

// normalizeTypeID 类型串 → 可登记的命名类型 ID（去指针与泛型实参）；基础类型、切片、map、匿名类型返回空。
func normalizeTypeID(t string) string {
	t = strings.TrimSpace(t)
	t = strings.TrimPrefix(t, "*")
	// 泛型实例化 "pkg.T[pkg.U]" → "pkg.T"
	if i := strings.Index(t, "["); i > 0 && strings.HasSuffix(t, "]") {
		t = t[:i]
	}
	if strings.ContainsAny(t, " ()") || t == "" || t == "string" || t == "int" ||
		t == "bool" || t == "float64" || strings.HasPrefix(t, "[]") ||
		strings.HasPrefix(t, "map[") || t == "any" || t == "interface{}" {
		return ""
	}
	return t
}

func parseCodeValue(v string) int {
	v = strings.Trim(v, `"`)
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
		return n
	}
	return -1
}

func trimQuotes(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

func lastSeg(s string) string {
	if i := strings.LastIndex(s, "."); i >= 0 {
		return s[i+1:]
	}
	return s
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func sortedCallees(g *codegraph.Graph, fn string) []string {
	out := append([]string{}, g.Callees[fn]...)
	sortStrings(out)
	return out
}

// typeIDOf 取类型的全限定 ID（命名类型取包路径 + 类型名；其余取类型串）。
func typeIDOf(t types.Type) string {
	if named, ok := t.(*types.Named); ok {
		if obj := named.Obj(); obj != nil && obj.Pkg() != nil {
			return obj.Pkg().Path() + "." + obj.Name()
		}
	}
	return types.TypeString(t, func(p *types.Package) string { return p.Path() })
}

// CollectErrCandidates 收集切片内已出现的具体错误码候选。
//
// 这是「证据注入」的关键：err 变量兜底时，切片内其它汇聚点的 err 槽若是直接
// 错误构造器调用（如 code.Response(c, code.NewDefaultError(code.ErrDatabase), nil)），
// 其常量实参已由 analyzeHit 提取到 ErrConstID；此外 errgroup 协程里
// `return code.NewDefaultError(code.X)` 的错误码也一并扫描——这些就是该 operation
// 最可能的码。注入给 LLM 后它能据此缩小到几个候选，而非在全量目录里空猜。
func (s *Slicer) CollectErrCandidates(handlerID string, hits []SinkHit) []ErrCandidate {
	seen := map[string]bool{}
	var out []ErrCandidate
	add := func(constID string) {
		if constID == "" || seen[constID] {
			return
		}
		seen[constID] = true
		out = append(out, ErrCandidate{
			Symbol: constID,
			Name:   lastSeg(constID),
			Code:   s.codeOfConst(constID),
		})
	}
	// 1. 汇聚点 err 槽直接构造器的常量（analyzeHit 已提取）。
	for _, h := range hits {
		add(h.ErrConstID)
	}
	// 2. 正向可达函数体内 `return NewXxxError(code.X)`（errgroup 协程返回的错误）。
	for _, c := range s.scanErrorReturns(handlerID) {
		add(c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// scanErrorReturns 扫描 handler 正向可达函数的函数体，收集 `return NewXxxError(code.X)`
// 模式里的错误码常量。覆盖 errgroup 协程内 `return code.NewDefaultError(code.ErrDatabase)`
// 这类不经过 sink err 槽、直接由 g.Wait() 汇总的错误路径。
func (s *Slicer) scanErrorReturns(handlerID string) []string {
	roots := []string{handlerID}
	if s.g.FuncDeclOf(handlerID) == nil {
		roots = append(roots, s.g.ConcreteImplsOf(handlerID)...)
	}
	scanned := map[string]bool{}
	var out []string
	for _, root := range roots {
		reach := s.g.ForwardReach(root, MaxDepth)
		for fn := range reach {
			if scanned[fn] {
				continue
			}
			scanned[fn] = true
			fd := s.g.FuncDeclOf(fn)
			if fd == nil || fd.Body == nil {
				continue
			}
			info := s.g.TypeInfoOfFunc(fn)
			if info == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				ret, ok := n.(*ast.ReturnStmt)
				if !ok {
					return true
				}
				for _, expr := range ret.Results {
					call, ok := ast.Unparen(expr).(*ast.CallExpr)
					if !ok || !isErrCtorCall(call) {
						continue
					}
					if constID, ok := codegraph.ConstArgOfCall(call, info, 0); ok {
						out = append(out, constID)
					}
				}
				return true
			})
		}
	}
	return out
}

// isErrCtorCall 判定调用是否为错误码构造器（NewXxxError 系）。
// 约定画像 error_codes_source 指定错误码包；构造器命名统一为 New*Error* 前缀，
// 首个实参是码常量（code int）。以此命名启发式识别，误收由 applyGapResolution
// 的 catalog 命中校验兜底（非码常量不会命中目录，不会写回）。
func isErrCtorCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	name := sel.Sel.Name
	return strings.HasPrefix(name, "New") && strings.Contains(name, "Error")
}

// constValueOfKey 解析 map 键表达式的常量字符串值（constant.Key / Key / "lit"）。
// 用于 any 响应字段候选：map[string]any{ constant.K: v } 的 K 是常量标识符或
// 选择器，其值就是 JSON 键名。返回 (值, 是否成功)。
func (s *Slicer) constValueOfKey(key ast.Expr, info *types.Info) (string, bool) {
	switch k := ast.Unparen(key).(type) {
	case *ast.BasicLit:
		if k.Kind == token.STRING {
			return trimQuotes(k.Value), true
		}
	case *ast.Ident:
		if obj := info.Uses[k]; obj != nil {
			if c, ok := obj.(*types.Const); ok {
				return trimQuotes(c.Val().ExactString()), true
			}
		}
	case *ast.SelectorExpr:
		if sel := info.Selections[k]; sel != nil && sel.Obj() != nil {
			if c, ok := sel.Obj().(*types.Const); ok {
				return trimQuotes(c.Val().ExactString()), true
			}
		}
		if obj := info.Uses[k.Sel]; obj != nil {
			switch o := obj.(type) {
			case *types.Const:
				return trimQuotes(o.Val().ExactString()), true
			case *types.Var:
				// 包级 var（如 constant.Key = "k"）非 const，go/types 归为 Var；
				// 取其静态字符串字面量初值（其余返回空）。
				return s.varStringValue(o), true
			}
		}
	}
	return "", false
}

// varStringValue 取包级 var 的字符串字面量初值（constant.Key = "k" 场景）。
//
// go/types 把非 const 的包级变量归为 *types.Var，其值需从声明语句的初值表达式
// 解析。遍历其所在包的语法文件，定位该 var 的 ValueSpec 字符串字面量初值。
// 仅支持字符串字面量（字段候选最常见的形态），其余返回空串。
func (s *Slicer) varStringValue(v *types.Var) string {
	pkg := v.Pkg()
	if pkg == nil {
		return ""
	}
	for _, p := range s.g.Pkgs() {
		if p.PkgPath != pkg.Path() {
			continue
		}
		for _, f := range p.Syntax {
			for _, d := range f.Decls {
				gd, ok := d.(*ast.GenDecl)
				if !ok {
					continue
				}
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range vs.Names {
						if name.Name != v.Name() || i >= len(vs.Values) {
							continue
						}
						if lit, ok := ast.Unparen(vs.Values[i]).(*ast.BasicLit); ok && lit.Kind == token.STRING {
							return trimQuotes(lit.Value)
						}
					}
				}
			}
		}
	}
	return ""
}

// CollectDataCandidates 收集切片内已出现的数据字段候选。
//
// 这是 any 响应兜底的证据注入：any/interface{} 数据槽不可静态定型时，切片内
// 其它可达函数的函数体字面量（map/struct 复合字面量的 JSON 键名）就是该 operation
// 最可能返回的字段。注入给 LLM 后它据此缩小到几个候选字段，而非从空证据里编造。
// 字段名必须在切片内实际出现过（确定性字面量），防幻觉由 applyResponseSchema 的
// 白名单校验兜底。
func (s *Slicer) CollectDataCandidates(handlerID string, hits []SinkHit) []DataCandidate {
	// 只扫描 handler 正向可达函数体内的 map/struct 复合字面量键名——
	// 这正是 any 数据槽实际赋值处（如 `data := map[string]interface{}{"k": v}`）
	// 所在的函数体，覆盖 IPOSysConfig / PlacementOrderCancel 等 map 字面量场景。
	keys := s.scanDataKeys(handlerID)
	_ = hits // hits 仅用于将来按 data 槽表达式精确切片；当前全可达扫描已覆盖
	seen := map[string]bool{}
	var uniq []string
	for _, k := range keys {
		if !seen[k] {
			seen[k] = true
			uniq = append(uniq, k)
		}
	}
	sort.Strings(uniq)
	cands := make([]DataCandidate, 0, len(uniq))
	for _, k := range uniq {
		cands = append(cands, DataCandidate{Name: k})
	}
	return cands
}

// DataCandidate any 响应兜底的字段候选。
type DataCandidate struct {
	Name string // 字段名（JSON 键）
}

// scanDataKeys 扫描 handler 正向可达函数体内的 map[string]any / struct 复合
// 字面量键名。优先字符串字面量键（map key），其次是结构体字段 json tag。
func (s *Slicer) scanDataKeys(handlerID string) []string {
	roots := []string{handlerID}
	if s.g.FuncDeclOf(handlerID) == nil {
		roots = append(roots, s.g.ConcreteImplsOf(handlerID)...)
	}
	scanned := map[string]bool{}
	var out []string
	for _, root := range roots {
		reach := s.g.ForwardReach(root, MaxDepth)
		for fn := range reach {
			if scanned[fn] {
				continue
			}
			scanned[fn] = true
			fd := s.g.FuncDeclOf(fn)
			if fd == nil || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				cl, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				for _, elt := range cl.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if lit, ok := kv.Key.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						out = append(out, trimQuotes(lit.Value))
						continue
					}
					// 常量标识符 / 选择器键（constant.Key 或 Key），解析常量值。
					info := s.g.TypeInfoOfFunc(fn)
					if info == nil {
						continue
					}
					if v, ok := s.constValueOfKey(kv.Key, info); ok && v != "" {
						out = append(out, v)
					}
				}
				return true
			})
		}
	}
	return out
}

// ErrCandidate err 兜底的候选错误码（切片内已出现的具体码）。
type ErrCandidate struct {
	Symbol string // 全限定常量符号
	Name   string // 末段常量名（跨包匹配键）
	Code   int    // 码值（-1 表示未解析）
}

// codeOfConst 从常量值解析码值（复用目录同款解析）。
func (s *Slicer) codeOfConst(constID string) int {
	if v, ok := s.g.ConstValueOf(constID); ok {
		return parseCodeValue(v)
	}
	return -1
}

// mapValueType 解析 map[K]V 的 V：V 是基础类型时返回其 JSON schema 类型名
// （integer/string/number/boolean/object），用于静态定型 additionalProperties map；
// V 是 any/interface{} 或非基础类型时返回空（不可静态定型，走 LLM 兜底）。
func mapValueType(typeStr string) (string, bool) {
	if !strings.HasPrefix(typeStr, "map[") {
		return "", false
	}
	end := strings.Index(typeStr, "]")
	if end < 0 || end+1 >= len(typeStr) {
		return "", false
	}
	val := strings.TrimSpace(typeStr[end+1:])
	// 剥离指针
	val = strings.TrimPrefix(val, "*")
	switch val {
	case "string":
		return "string", true
	case "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64", "uintptr":
		return "integer", true
	case "float32", "float64":
		return "number", true
	case "bool":
		return "boolean", true
	case "any", "interface{}":
		return "", false // 值本身 any：不可定型
	}
	// 命名类型：若能解析为命名类型 ID，视为 object（可引用组件）；否则不可定型。
	if id := normalizeTypeID(val); id != "" {
		return "object", true
	}
	return "", false
}
