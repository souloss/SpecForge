package slicing

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"strings"

	"github.com/specforge/specforge/internal/adapter"
	"github.com/specforge/specforge/internal/codegraph"
	"github.com/specforge/specforge/internal/profile"
)

// 框架原生响应写出器 + 响应包装器自动发现（函数摘要）。
//
// 原生写出器是框架 API 的固定语义（无需 profile）；包装器是仓库自定义的
// 「把形参装进信封再交给原生写出器」的函数（如 code.WriteRes），
// 通过扫描其函数体一次性算出摘要：哪个形参进了信封哪个字段、按哪个形参是否为 nil 分流。

// writeFlow 包装器函数体内一次原生写出的形参流向。
type writeFlow struct {
	envType    string          // 写出的信封结构体类型 ID（直接透传形参或 map 信封时为空）
	fieldOf    map[int]string  // 形参下标 → 信封字段 JSON 名（值即形参本身）
	derivedOf  map[int]string  // 形参下标 → 信封字段 JSON 名（值由形参派生，如 err.Error()）
	direct     int             // 形参直接作为响应体时的下标；profile.NoSlot 表示否
	guardedBy  int             // 写出位于 `if 形参 != nil {}` 分支内时该形参下标；否则 profile.NoSlot
	mapFields  []EnvelopeField // map 字面量信封（gin.H{...}）的全部键；结构体信封为空
	code       *int            // 信封中「码」字段的整数常量（如 "code": 500）；无则 nil
	statusSlot int             // 状态码由该形参下标流入（经 WriteHeader/中转写出器），profile.NoSlot 表示否
}

// EnvelopeField 合成信封（map 字面量信封）的一个键。
type EnvelopeField struct {
	Key      string     // JSON 键
	Type     types.Type // 值的静态类型（承载 data 的键为形参类型，通常为 any）
	Role     string     // 语义：data / err / 空（普通字段）
	Required bool       // 是否在该包装器的每条写出路径上都出现
	JSONType string     // Type 未知时的 JSON 类型（LLM 包装器摘要给出，封闭集合）
}

// 合成信封字段语义。
const (
	RoleData = "data" // 承载业务数据的键
	RoleErr  = "err"  // 承载错误信息的键
)

// SyntheticEnvelope 包装器用 map 字面量构造的信封（无命名类型，由前端合成 schema）。
type SyntheticEnvelope struct {
	ID     string          // 合成类型 ID（wrapenv/<函数>.<函数名>Envelope）
	Fn     string          // 包装器函数符号 ID（证据）
	Fields []EnvelopeField // 键（按首次出现顺序）
}

// SyntheticEnvPrefix 合成信封类型 ID 前缀。
const SyntheticEnvPrefix = "wrapenv/"

// codeKeyNames 视为「业务码」的信封键（小写比较）：只采信整数常量值。
var codeKeyNames = map[string]bool{"code": true, "errcode": true, "err_code": true, "errno": true,
	"retcode": true, "ret_code": true, "statuscode": true, "status_code": true}

// errorMetaKey 信封键是否属于错误元数据（业务码/消息/请求 ID）而非业务数据。
// 恒为错误写出的信封（如 writeError 的 errorEnvelope{code,message,requestId}）只含此类键。
func errorMetaKey(name string) bool {
	n := strings.ToLower(name)
	if codeKeyNames[n] {
		return true
	}
	switch n {
	case "msg", "message", "errmsg", "error_msg", "requestid", "request_id":
		return true
	}
	return false
}

// hasDataField 信封写出是否含承载业务数据的直接形参字段（字段名非错误元数据键）。
// 用于区分成功信封（有 data 槽）与恒为错误信封（只有 code/message 等错误元字段）。
func hasDataField(f writeFlow) bool {
	for _, name := range f.fieldOf {
		if !errorMetaKey(name) {
			return true
		}
	}
	return false
}

// DiscoverSinks 自动发现响应汇聚点：框架原生写出器（原语表）+ 仓库内的响应包装器。
// 包装器摘要让 handler 层调用点按「形参槽位」解析 data/err，与手写 profile 的 sink 同构。
// 同时返回 map 字面量信封的合成定义（按合成类型 ID 索引），供前端合成信封 schema。
//
// 两趟不动点：第一趟发现「形参 → 原生写出器」的直接包装器（如 writeJSON(w,status,payload)）；
// 第二趟发现「把形参传给已发现包装器」的中转包装器（如 writeError(w,r,status,code) 内调
// writeJSON(w,status,errorEnvelope{...})），此时内层包装器的 DataSlot/StatusSlot 映射回外层实参。
func DiscoverSinks(g *codegraph.Graph, writers []adapter.Writer) ([]profile.SinkPattern, map[string]SyntheticEnvelope) {
	idx := writerIndex(writers)
	var out []profile.SinkPattern
	envs := map[string]SyntheticEnvelope{}
	// 原生写出器进 out（handler 层直接汇聚点，供 IsSink 匹配）；但**不进 known**：
	// known 只收录「仓库内包装器」，中转包装器据此把形参映射回内层槽位。若原生写出器进 known，
	// 其 StatusSlot 零值（0，如 Encode 的 BodyArg=0）会被 flowOfKnownWrapper 误当「状态码形参下标 0」，
	// 把 data 形参错绑成 StatusSlot。
	for _, w := range writers {
		out = append(out, profile.SinkPattern{
			Symbol: w.Symbol, DataSlot: w.BodyArg, ErrSlot: profile.NoSlot,
			Status: w.DefaultStatus, Raw: true, Auto: true,
		})
	}
	known := map[string]profile.SinkPattern{}
	// 第一趟：直接包装器（形参 → 原生写出器）。
	for _, id := range g.FuncIDs() {
		if p, env, ok := summarizeWrapper(g, id, idx, known); ok {
			out = append(out, p)
			known[p.Symbol] = p
			if env != nil {
				envs[env.ID] = *env
			}
		}
	}
	// 第二趟：中转包装器（形参 → 已发现的直接包装器）。
	for _, id := range g.FuncIDs() {
		if _, already := known[id]; already {
			continue
		}
		if p, env, ok := summarizeWrapper(g, id, idx, known); ok {
			out = append(out, p)
			known[p.Symbol] = p
			if env != nil {
				envs[env.ID] = *env
			}
		}
	}
	return out, envs
}

// writerIndex 写出器原语按符号索引。
func writerIndex(writers []adapter.Writer) map[string]adapter.Writer {
	idx := make(map[string]adapter.Writer, len(writers))
	for _, w := range writers {
		idx[w.Symbol] = w
	}
	return idx
}

// summarizeWrapper 计算单个函数的包装器摘要；函数体内无「形参 → 原生写出器/已知包装器」流向时返回 false。
// known 为已发现的包装器（原生写出器 + 先前摘要出的直接包装器），中转包装器据此把形参映射回内层槽位。
func summarizeWrapper(g *codegraph.Graph, fnID string, writers map[string]adapter.Writer, known map[string]profile.SinkPattern) (profile.SinkPattern, *SyntheticEnvelope, bool) {
	fd := g.FuncDeclOf(fnID)
	info := g.TypeInfoOfFunc(fnID)
	if fd == nil || fd.Body == nil || info == nil || fd.Type.Params == nil {
		return profile.SinkPattern{}, nil, false
	}
	params := paramIndex(fd, info)
	if len(params) == 0 {
		return profile.SinkPattern{}, nil, false
	}
	guards := nilGuards(fd.Body, info, params)
	var flows []writeFlow
	// statusParam 包装器把 HTTP 状态码当形参时的形参下标（由 WriteHeader/带 StatusArg 的原生写出器绑定）。
	// 独立于 data 流向扫描：状态码专用写出器（如 w.WriteHeader(status)）无 body 实参，
	// 不产生 data flow，但它的状态码形参要映射到包装器的 StatusSlot。
	statusParam := profile.NoSlot
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false // 闭包内的写出不属于本函数的同步摘要
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		callee, _ := codegraph.ResolveCallee(call, info)
		// 状态码槽：原生写出器带 StatusArg（WriteHeader/http.Error/gin c.JSON(status,...)）且实参为函数形参时，
		// 绑定为包装器的 StatusSlot（在 data 流向判定之前，覆盖无 body 实参的状态码写出）。
		if w, ok := writers[callee]; ok && w.StatusArg >= 0 && w.StatusArg < len(call.Args) {
			if idx := statusParamOf(call.Args[w.StatusArg], info, params); idx != profile.NoSlot {
				statusParam = idx
			}
		}
		// 中转：形参流入已发现的包装器（writeError → writeJSON）时，把内层槽位映射回外层实参。
		if inner, ok := known[callee]; ok && callee != fnID {
			if f, mapped := flowOfKnownWrapper(g, call, info, params, fd.Body, inner); mapped {
				f.guardedBy = guardOf(guards, call.Pos())
				flows = append(flows, f)
			}
			return true
		}
		w, ok := writers[callee]
		if !ok || w.BodyArg < 0 || w.BodyArg >= len(call.Args) {
			return true
		}
		if f, ok := flowOf(g, call.Args[w.BodyArg], info, params, fd.Body); ok {
			f.guardedBy = guardOf(guards, call.Pos())
			if w.StatusArg >= 0 {
				if idx := statusParamOf(call.Args[w.StatusArg], info, params); idx != profile.NoSlot {
					f.statusSlot = idx
				}
			}
			flows = append(flows, f)
		}
		return true
	})
	return patternOf(fnID, flows, statusParam, defaultStatusOf(writers))
}

// flowOfKnownWrapper 中转包装器：把「形参 → 已发现包装器」的调用点映射回外层形参槽位。
// inner 为被调包装器的摘要（DataSlot/StatusSlot/ErrSlot）。返回的 writeFlow 直接以本函数形参下标
// 表达 data/err/status 流向，等价于把内层包装器的槽位「透传」到本函数。
func flowOfKnownWrapper(g *codegraph.Graph, call *ast.CallExpr, info *types.Info,
	params map[types.Object]int, body *ast.BlockStmt, inner profile.SinkPattern) (writeFlow, bool) {
	f := writeFlow{direct: profile.NoSlot, guardedBy: profile.NoSlot,
		fieldOf: map[int]string{}, derivedOf: map[int]string{}, statusSlot: profile.NoSlot}
	argAt := func(slot int) ast.Expr {
		if slot < 0 || slot >= len(call.Args) {
			return nil
		}
		return call.Args[slot]
	}
	// 仅当内层包装器是「裸写」包装器（Raw，data 槽即原始响应体，如 writeJSON）时才可能是中转包装器：
	// 中转包装器把形参装进信封再交给裸写器。若内层是信封包装器（如 code.WriteResponse），
	// 调用它并传业务数据的函数是 handler/服务（业务体由内层信封承载），不是中转包装器——
	// 否则 handler 会被误判成包装器，其自身的 WriteResponse 调用点在 Trace 时被「caller 是包装器」跳过。
	if !inner.Raw {
		return f, false
	}
	// 状态码槽：内层包装器把 status 当形参，本函数实参若也是本函数形参则透传其下标。
	if inner.StatusSlot >= 0 {
		if idx, ok := paramOf(argAt(inner.StatusSlot), info, params); ok {
			f.statusSlot = idx
		}
	}
	// data 槽：内层包装器的 data 实参，若为本函数形参则透传；若为信封字面量则按结构体信封解析。
	dataArg := argAt(inner.DataSlot)
	if idx, ok := paramOf(dataArg, info, params); ok {
		f.direct = idx
		// 直接透传时记一个 data 字段（使 fieldOf 非空，patternOf 才能视为有效 data 流向）。
		// 字段名取内层包装器的 DataField 语义；否则用占位名 "data"（上层据 direct 槽走 Raw，不落字段名）。
		if inner.DataField != "" {
			f.fieldOf[idx] = inner.DataField
		} else {
			f.fieldOf[idx] = "data"
		}
		return f, true
	}
	if lit, ok := stripAddr(dataArg).(*ast.CompositeLit); ok {
		t := info.TypeOf(lit)
		if t == nil {
			return f, false
		}
		if isStringKeyMap(t) {
			return mapFlowOf(lit, MapAssignments(body, holderOf(body, lit, info), info), info, params)
		}
		f.envType = typeIDOf(t)
		ti := g.Type(f.envType)
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			name := jsonFieldName(ti, key.Name)
			// 信封字面量里承载业务数据的槽：值是形参（data 槽）；其余字段按普通字段记录
			// （仅当整个字面量里至少有一个形参承载的字段时才视为有效信封——错误信封 code/message
			// 都由形参派生/内部表达式构造，也符合「信封」定义）。
			if idx, direct := paramOf(kv.Value, info, params); direct {
				f.fieldOf[idx] = name
			} else if idx, derived := derivedParamOf(kv.Value, info, params); derived {
				f.derivedOf[idx] = name
			} else if codeKeyNames[strings.ToLower(name)] {
				f.code = intConst(kv.Value, info)
			}
		}
		// 信封（errorEnvelope）本身由形参派生（code 形参），即使没有字段是纯形参，只要
		// 字面量类型可登记且存在派生字段/码字段，即认定该调用点是信封写出。
		return f, f.envType != "" && (len(f.fieldOf)+len(f.derivedOf) > 0)
	}
	// data 实参既非形参也非信封字面量（如 `BuildBaseResponse(c, payload)` 服务层组装、或局部变量）：
	// 不是「形参 → 信封」的透传，不视为中转包装器的 data 流向。这类调用点由上层
	// NarrowEnvelopeResult 跨函数收窄，而非包装器摘要——否则 handler 会被误判成包装器，
	// 其自身的 WriteResponse 调用点在 Trace 时被「caller 是包装器」跳过，导致 0 hit。
	if f.direct == profile.NoSlot && len(f.fieldOf) == 0 && len(f.derivedOf) == 0 && f.statusSlot == profile.NoSlot {
		return f, false
	}
	return f, true
}

// statusParamOf 写出器的状态码实参是否为函数形参（返回其下标；否则 NoSlot）。
func statusParamOf(e ast.Expr, info *types.Info, params map[types.Object]int) int {
	if e == nil {
		return profile.NoSlot
	}
	id, ok := ast.Unparen(e).(*ast.Ident)
	if !ok {
		return profile.NoSlot
	}
	if idx, ok := params[info.Uses[id]]; ok {
		return idx
	}
	return profile.NoSlot
}

// defaultStatusOf 包装器写出的默认 HTTP 状态（取原语表中写出器的默认值；无写出器时为 200）。
func defaultStatusOf(writers map[string]adapter.Writer) int {
	for _, w := range writers {
		return w.DefaultStatus
	}
	return fallbackStatus
}

// fallbackStatus 原语表为空时的默认 HTTP 状态。
const fallbackStatus = 200

// patternOf 由各写出点流向合成 sink 模式：无守卫的写出为成功路径（data 槽），
// `if p != nil` 守卫内的写出为失败路径（err 槽 = p）。信封里「码」字段的整数常量按分支记为
// 固定业务码；map 字面量信封额外合成一份信封定义。statusParam 为状态码形参下标（无则 NoSlot）。
func patternOf(fnID string, flows []writeFlow, statusParam int, status int) (profile.SinkPattern, *SyntheticEnvelope, bool) {
	p := profile.SinkPattern{
		Symbol: fnID, DataSlot: profile.NoSlot, ErrSlot: profile.NoSlot, StatusSlot: profile.NoSlot,
		Status: status, Auto: true,
	}
	if statusParam != profile.NoSlot {
		p.StatusSlot = statusParam
	}
	hasSuccess, hasFailure := false, false
	var envFlows []writeFlow // 参与合成信封的写出（map 字面量）
	for _, f := range flows {
		if f.statusSlot != profile.NoSlot {
			p.StatusSlot = f.statusSlot
		}
		if f.direct != profile.NoSlot {
			if f.guardedBy == profile.NoSlot {
				p.DataSlot, p.Raw, hasSuccess = f.direct, true, true
			}
			continue
		}
		if f.guardedBy != profile.NoSlot {
			name, raw := f.fieldOf[f.guardedBy]
			ok := raw
			if !ok {
				name, ok = f.derivedOf[f.guardedBy]
			}
			if ok {
				p.ErrSlot, p.ErrField, p.ErrRaw, hasFailure = f.guardedBy, name, raw, true
				p.ErrEnvelopeType, p.FailureCode = f.envType, f.code
				if f.mapFields != nil {
					envFlows = append(envFlows, f)
				}
			}
			continue
		}
		if hasDataField(f) {
			// 信封承载业务数据的字段（非 code/message 等码字段）：成功信封。
			// data 槽取下标最小的业务数据字段。
			for idx, name := range f.fieldOf {
				if errorMetaKey(name) {
					continue
				}
				if p.DataSlot == profile.NoSlot || idx < p.DataSlot {
					p.DataSlot, p.DataField = idx, name
				}
			}
			p.EnvelopeType, p.SuccessCode, hasSuccess = f.envType, f.code, true
		} else {
			// 信封字段全是码/派生/字面量（如 writeError 的 code/message/requestId）：
			// 这是恒为错误写出的信封（无业务数据槽），不设 DataSlot。
			p.ErrEnvelopeType, hasFailure = f.envType, true
		}
		if f.mapFields != nil {
			envFlows = append(envFlows, f)
		}
	}
	if !hasSuccess && !hasFailure {
		return profile.SinkPattern{}, nil, false
	}
	if !hasSuccess {
		p.EnvelopeType = p.ErrEnvelopeType // 只有失败写出的包装器：信封即失败信封
		// 无成功分支、只有错误信封的包装器 = 恒为错误写出（如 writeError）。
		p.ErrOnly = true
	}
	p.BranchOnErr = hasSuccess && hasFailure
	var env *SyntheticEnvelope
	if len(envFlows) > 0 {
		for i := range envFlows {
			markRoles(&envFlows[i], p.DataSlot, p.ErrSlot)
		}
		env = synthesizeEnvelope(fnID, envFlows)
		p.EnvelopeType, p.ErrEnvelopeType = env.ID, env.ID
	}
	return p, env, true
}

// synthesizeEnvelope 合并各写出路径的 map 字面量键：键序按首次出现，每条路径都有的键为必填。
func synthesizeEnvelope(fnID string, flows []writeFlow) *SyntheticEnvelope {
	name := fnID[strings.LastIndex(fnID, ".")+1:]
	env := &SyntheticEnvelope{ID: SyntheticEnvPrefix + fnID + "." + name + "Envelope", Fn: fnID}
	count := map[string]int{}
	pos := map[string]int{}
	for _, f := range flows {
		for _, fld := range f.mapFields {
			count[fld.Key]++
			if i, seen := pos[fld.Key]; seen {
				if env.Fields[i].Role == "" {
					env.Fields[i].Role = fld.Role
				}
				continue
			}
			pos[fld.Key] = len(env.Fields)
			env.Fields = append(env.Fields, fld)
		}
	}
	for i := range env.Fields {
		env.Fields[i].Required = count[env.Fields[i].Key] == len(flows)
	}
	return env
}

// flowOf 解析写出实参：形参直接透传、信封复合字面量 `T{Field: 形参}`，
// 或先赋给局部变量再写出（`r := &T{Field: 形参}; c.JSON(r)`）。
func flowOf(g *codegraph.Graph, arg ast.Expr, info *types.Info, params map[types.Object]int, body *ast.BlockStmt) (writeFlow, bool) {
	f := writeFlow{direct: profile.NoSlot, guardedBy: profile.NoSlot, statusSlot: profile.NoSlot,
		fieldOf: map[int]string{}, derivedOf: map[int]string{}}
	arg = stripAddr(arg)
	var holder types.Object // 承载字面量的局部变量（`m := gin.H{}; m["k"] = v` 的 m）
	if id, ok := arg.(*ast.Ident); ok {
		if idx, ok := params[info.Uses[id]]; ok {
			f.direct = idx
			return f, true
		}
		holder = info.Uses[id]
		init := localInitOf(body, holder, info)
		if init == nil {
			return f, false
		}
		arg = stripAddr(init)
	}
	lit, ok := arg.(*ast.CompositeLit)
	if !ok {
		// 文件/二进制写出的中间层：`c.SendStream(bytes.NewReader(data), n)` 的 data 实参被
		// bytes.NewReader 包了一层，看穿它取首实参（形参透传 / 局部变量），使外层包装器的
		// data 形参映射到正确槽位（ResponseExcel(c, name, data) 的 data）。
		if call, ok2 := arg.(*ast.CallExpr); ok2 && len(call.Args) == 1 {
			if callee, _ := codegraph.ResolveCallee(call, info); isByteWrapCallee(callee) {
				return flowOf(g, call.Args[0], info, params, body)
			}
		}
		return f, false
	}
	t := info.TypeOf(lit)
	if t == nil {
		return f, false
	}
	if isStringKeyMap(t) {
		return mapFlowOf(lit, MapAssignments(body, holder, info), info, params)
	}
	f.envType = typeIDOf(t)
	ti := g.Type(f.envType)
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		name := jsonFieldName(ti, key.Name)
		if idx, direct := paramOf(kv.Value, info, params); direct {
			f.fieldOf[idx] = name
		} else if idx, derived := derivedParamOf(kv.Value, info, params); derived {
			f.derivedOf[idx] = name
		} else if codeKeyNames[strings.ToLower(name)] {
			f.code = intConst(kv.Value, info)
		}
	}
	return f, len(f.fieldOf)+len(f.derivedOf) > 0
}

// isByteWrapCallee 被调符号是否为「包一层字节切片」的辅助（bytes.NewReader/NewBuffer/Strings.NewReader 等）：
// 文件/二进制写出的 data 实参常被它包一层再交给 SendStream/SendFile。
func isByteWrapCallee(callee string) bool {
	switch lastSeg(callee) {
	case "NewReader", "NewBuffer", "NewBufferString", "NewStringReader":
		return true
	}
	return false
}

// mapFlowOf map 字面量信封（`gin.H{"code": 0, "data": data}`）：常量字符串键为信封字段，
// 值为形参 / 形参派生表达式 / 其它（记录静态类型）；「码」键的整数常量记为该分支的固定业务码。
// extra 为字面量之后对同一变量的常量键赋值（`m["data"] = data`），一并视为信封键。
func mapFlowOf(lit *ast.CompositeLit, extra []KeyValue, info *types.Info, params map[types.Object]int) (writeFlow, bool) {
	f := writeFlow{direct: profile.NoSlot, guardedBy: profile.NoSlot, statusSlot: profile.NoSlot,
		fieldOf: map[int]string{}, derivedOf: map[int]string{},
		mapFields: []EnvelopeField{}}
	pairs := make([]KeyValue, 0, len(lit.Elts)+len(extra))
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			return f, false
		}
		key, ok := constString(kv.Key, info)
		if !ok {
			return f, false // 非常量键：信封结构取决于运行时，交给形状缺口
		}
		pairs = append(pairs, KeyValue{Key: key, Value: kv.Value})
	}
	pairs = append(pairs, extra...)
	seen := map[string]bool{}
	for _, kv := range pairs {
		if idx, direct := paramOf(kv.Value, info, params); direct {
			f.fieldOf[idx] = kv.Key
		} else if idx, derived := derivedParamOf(kv.Value, info, params); derived {
			f.derivedOf[idx] = kv.Key
		} else if codeKeyNames[strings.ToLower(kv.Key)] {
			f.code = intConst(kv.Value, info)
		}
		if !seen[kv.Key] {
			seen[kv.Key] = true
			f.mapFields = append(f.mapFields, EnvelopeField{Key: kv.Key, Type: info.TypeOf(kv.Value)})
		}
	}
	return f, len(f.fieldOf)+len(f.derivedOf) > 0
}

// KeyValue map 的一个常量键及其值表达式。
type KeyValue struct {
	Key   string   // 常量字符串键
	Value ast.Expr // 值表达式
}

// MapAssignments 函数体内对 map 变量 obj 的常量键赋值（`m["k"] = v`），按出现顺序；非常量键跳过。
func MapAssignments(body *ast.BlockStmt, obj types.Object, info *types.Info) []KeyValue {
	if body == nil || obj == nil {
		return nil
	}
	var out []KeyValue
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != len(as.Rhs) {
			return true
		}
		for i, lhs := range as.Lhs {
			ix, ok := lhs.(*ast.IndexExpr)
			if !ok {
				continue
			}
			id, ok := ast.Unparen(ix.X).(*ast.Ident)
			if !ok || info.Uses[id] != obj {
				continue
			}
			if key, ok := constString(ix.Index, info); ok {
				out = append(out, KeyValue{Key: key, Value: as.Rhs[i]})
			}
		}
		return true
	})
	return out
}

// constString 常量字符串表达式的值。
func constString(e ast.Expr, info *types.Info) (string, bool) {
	tv, ok := info.Types[e]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(tv.Value), true
}

// markRoles 标注合成信封字段的语义（data/err），依据包装器模式的槽位绑定。
func markRoles(f *writeFlow, dataSlot, errSlot int) {
	for i := range f.mapFields {
		for idx, key := range f.fieldOf {
			if key == f.mapFields[i].Key && idx == dataSlot {
				f.mapFields[i].Role = RoleData
			}
		}
		for idx, key := range f.derivedOf {
			if key == f.mapFields[i].Key && idx == errSlot {
				f.mapFields[i].Role = RoleErr
			}
		}
		for idx, key := range f.fieldOf {
			if key == f.mapFields[i].Key && idx == errSlot {
				f.mapFields[i].Role = RoleErr
			}
		}
	}
}

// isStringKeyMap 类型底层是否为 map[string]V（gin.H、fiber.Map、map[string]any）。
func isStringKeyMap(t types.Type) bool {
	m, ok := t.Underlying().(*types.Map)
	if !ok {
		return false
	}
	k, ok := m.Key().Underlying().(*types.Basic)
	return ok && k.Info()&types.IsString != 0
}

// paramOf 表达式是否就是某个形参（返回下标）。
func paramOf(e ast.Expr, info *types.Info, params map[types.Object]int) (int, bool) {
	id, ok := ast.Unparen(e).(*ast.Ident)
	if !ok {
		return 0, false
	}
	idx, ok := params[info.Uses[id]]
	return idx, ok
}

// derivedParamOf 表达式是否由唯一一个形参派生（如 err.Error()、fmt.Sprint(err)），返回其下标。
func derivedParamOf(e ast.Expr, info *types.Info, params map[types.Object]int) (int, bool) {
	found := -1
	multi := false
	ast.Inspect(e, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		if idx, isParam := params[info.Uses[id]]; isParam {
			if found >= 0 && found != idx {
				multi = true
			}
			found = idx
		}
		return true
	})
	return found, found >= 0 && !multi
}

// intConst 整数常量表达式的值；非常量返回 nil。
func intConst(e ast.Expr, info *types.Info) *int {
	tv, ok := info.Types[e]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.Int {
		return nil
	}
	v, exact := constant.Int64Val(tv.Value)
	if !exact {
		return nil
	}
	n := int(v)
	return &n
}

// stripAddr 去掉括号与取地址：`&T{}` / `(x)` → 内层表达式。
func stripAddr(e ast.Expr) ast.Expr {
	e = ast.Unparen(e)
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = ast.Unparen(u.X)
	}
	return e
}

// localInitOf 局部变量 obj 在函数体内的唯一定义式右值（`x := expr` / `var x = expr`）；
// 被多次定义或找不到时返回 nil（值流不唯一时不猜）。
func localInitOf(body *ast.BlockStmt, obj types.Object, info *types.Info) ast.Expr {
	if body == nil || obj == nil {
		return nil
	}
	var init ast.Expr
	defs := 0
	ast.Inspect(body, func(n ast.Node) bool {
		switch st := n.(type) {
		case *ast.AssignStmt:
			if len(st.Lhs) != len(st.Rhs) {
				return true
			}
			for i, lhs := range st.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok {
					continue
				}
				if info.Defs[id] == obj || info.Uses[id] == obj {
					init, defs = st.Rhs[i], defs+1
				}
			}
		case *ast.ValueSpec:
			for i, name := range st.Names {
				if info.Defs[name] == obj && i < len(st.Values) {
					init, defs = st.Values[i], defs+1
				}
			}
		}
		return true
	})
	if defs != 1 {
		return nil
	}
	return init
}

// jsonFieldName 结构体字段 Go 名 → JSON 名（类型信息缺失时退回 Go 名）。
func jsonFieldName(ti *codegraph.TypeInfo, goName string) string {
	if ti != nil {
		for _, fl := range ti.Fields {
			if fl.Name == goName {
				return fl.JSONName
			}
		}
	}
	return goName
}

// paramIndex 形参对象 → 下标（不含接收者；匿名/空白形参不参与）。
func paramIndex(fd *ast.FuncDecl, info *types.Info) map[types.Object]int {
	out := map[types.Object]int{}
	idx := 0
	for _, field := range fd.Type.Params.List {
		if len(field.Names) == 0 {
			idx++
			continue
		}
		for _, name := range field.Names {
			if obj := info.Defs[name]; obj != nil && name.Name != "_" {
				out[obj] = idx
			}
			idx++
		}
	}
	return out
}

// nilGuard `if 形参 != nil { ... }` 分支的源码范围。
type nilGuard struct {
	param    int       // 被判空的形参下标
	from, to token.Pos // then 分支范围
}

// nilGuards 收集函数体内以形参判非空为条件的 then 分支。
func nilGuards(body *ast.BlockStmt, info *types.Info, params map[types.Object]int) []nilGuard {
	var out []nilGuard
	ast.Inspect(body, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		be, ok := ast.Unparen(ifs.Cond).(*ast.BinaryExpr)
		if !ok || be.Op != token.NEQ {
			return true
		}
		for _, side := range [][2]ast.Expr{{be.X, be.Y}, {be.Y, be.X}} {
			id, ok := ast.Unparen(side[0]).(*ast.Ident)
			nilID, ok2 := ast.Unparen(side[1]).(*ast.Ident)
			if !ok || !ok2 || nilID.Name != "nil" {
				continue
			}
			if idx, ok := params[info.Uses[id]]; ok {
				out = append(out, nilGuard{param: idx, from: ifs.Body.Pos(), to: ifs.Body.End()})
			}
		}
		return true
	})
	return out
}

// guardOf 写出点所在的最内层判空分支形参；不在任何判空分支内返回 profile.NoSlot。
func guardOf(guards []nilGuard, pos token.Pos) int {
	best, span := profile.NoSlot, token.Pos(0)
	for _, gd := range guards {
		if pos >= gd.from && pos < gd.to && (best == profile.NoSlot || gd.to-gd.from < span) {
			best, span = gd.param, gd.to-gd.from
		}
	}
	return best
}
